// Package cli is the ignatius command. cmd/ignatius is a three-line main around Main, so a
// custom build can add sink plugins the way a Caddy build adds modules:
//
//	package main
//
//	import (
//		"github.com/phin-tech/ignatius/go/cli"
//		_ "github.com/phin-tech/ignatius/go/plugins/kinesis" // registers the "kinesis" event sink
//	)
//
//	func main() { cli.Main() }
//
// Commands:
//
//	ignatius serve --config ignatius.toml
//	ignatius run   --config ignatius.toml --model 'cascade:jeff>jev' request.json
//	ignatius run   --config ignatius.toml --plan plan.json -        (request on stdin)
//	ignatius hash-password                                          (password on stdin, bcrypt hash out)
//	ignatius purge     --config ignatius.toml --client NAME [--before DATE] [--content-only]
//	ignatius export    --config ignatius.toml --client NAME [--since DATE]   (JSONL on stdout)
//	ignatius calibrate --config ignatius.toml --client NAME [--since DATE] [--source human|automated|audit|all]
//	ignatius eval      --config ignatius.toml --data test.jsonl --route triage [--route 'fan-out:a,b|vote' ...] [--sweep] [--json out.json]
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/phin-tech/ignatius/go/eval"
	"github.com/phin-tech/ignatius/go/gateway"
	"github.com/phin-tech/ignatius/go/ignatius"
	"github.com/phin-tech/ignatius/go/store"
	"github.com/phin-tech/ignatius/go/telemetry"
	"golang.org/x/crypto/bcrypt"
)

// Version is stamped at build time with
// -ldflags "-X github.com/phin-tech/ignatius/go/cli.Version=...".
var Version = "dev"

// Main runs the command with os.Args and exits the process.
func Main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		serve(os.Args[2:])
	case "run":
		run(os.Args[2:])
	case "hash-password":
		hashPassword()
	case "purge":
		purge(os.Args[2:])
	case "export":
		export(os.Args[2:])
	case "calibrate":
		calibrate(os.Args[2:])
	case "eval":
		evalCmd(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: ignatius serve --config FILE\n       ignatius run --config FILE (--model STR | --plan FILE) REQUEST.json|-\n       ignatius hash-password   (reads the password on stdin)\n       ignatius purge|export|calibrate --config FILE --client NAME   (need a [store]; see SPEC 13)\n       ignatius eval --config FILE --data test.jsonl --route ROUTE [--route ROUTE ...]   (judge routes on a labeled set; SPEC 15)")
	os.Exit(2)
}

func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	path := fs.String("config", "ignatius.toml", "path to the TOML config")
	_ = fs.Parse(args)

	cfg, err := gateway.LoadConfig(*path)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	reg, err := ignatius.BuildRegistryWith(cfg.Models, os.Getenv, ignatius.Options{Cache: cfg.Cache})
	if err != nil {
		log.Fatalf("models: %v", err)
	}
	// One structured JSON line per request goes to stderr; trace ids are in each line.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	tel, err := telemetry.Setup(context.Background(), cfg.Telemetry, os.Getenv, Version)
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tel.Shutdown(ctx); err != nil {
			log.Printf("telemetry shutdown: %v", err)
		}
	}()
	srv, err := gateway.New(cfg, reg, os.Getenv)
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	defer srv.Close()
	srv.SetMetricsHandler(tel.MetricsHandler)
	if !srv.HasClientKeys() && !isLoopback(cfg.Listen) && os.Getenv("IGNATIUS_ALLOW_NO_AUTH") != "1" {
		log.Fatalf("refusing to listen on %s with no client key: set %s (or IGNATIUS_ALLOW_NO_AUTH=1)", cfg.Listen, cfg.APIKeyEnv)
	}
	if len(cfg.Users) > 0 && !isLoopback(cfg.Listen) && !cfg.Admin.TrustProxy {
		log.Printf("warning: [[users]] sign in with a password, so serve over TLS: terminate it in a proxy and set [admin] trust_proxy = true (otherwise every login throttles as one address)")
	}
	hs := &http.Server{Addr: cfg.Listen, Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv.StartRetention(ctx, time.Hour)
	// ListenAndServe returns the moment Shutdown is called, not when in-flight requests
	// finish. Wait for Shutdown before returning, or the deferred srv.Close would close the
	// store and the event queues under requests that are still running.
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = hs.Shutdown(sctx)
	}()
	log.Printf("ignatius listening on %s; models %v; routes %v", cfg.Listen, srv.Router().ModelNames(), srv.Router().RouteNames())
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-shutdownDone // then the deferred srv.Close drains the store and event queues
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := fs.String("config", "ignatius.toml", "path to the TOML config")
	model := fs.String("model", "", "route, alias or inline route, e.g. 'cascade:jeff>jev'")
	planFile := fs.String("plan", "", "path to a Plan JSON file (instead of --model)")
	_ = fs.Parse(args)
	if fs.NArg() != 1 || (*model == "") == (*planFile == "") {
		usage()
	}
	cfg, err := gateway.LoadConfig(*path)
	if err != nil {
		fatal(2, "config: %v", err)
	}
	reg, err := ignatius.BuildRegistryWith(cfg.Models, os.Getenv, ignatius.Options{Cache: cfg.Cache})
	if err != nil {
		fatal(2, "models: %v", err)
	}
	rt, err := gateway.NewRouter(cfg, reg)
	if err != nil {
		fatal(2, "routes: %v", err)
	}
	var plan ignatius.Plan
	if *planFile != "" {
		b, err := os.ReadFile(*planFile)
		if err == nil {
			err = json.Unmarshal(b, &plan)
		}
		if err != nil {
			fatal(2, "plan: %v", err)
		}
	} else if plan, err = rt.Resolve(*model); err != nil {
		fatal(2, "%v", err)
	}
	var in io.Reader = os.Stdin
	if fs.Arg(0) != "-" {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			fatal(2, "request: %v", err)
		}
		defer f.Close()
		in = f
	}
	var req ignatius.Request
	if err := json.NewDecoder(in).Decode(&req); err != nil {
		fatal(2, "request: %v", err)
	}
	if err := req.Validate(); err != nil {
		fatal(2, "request: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.RequestTimeoutMS)*time.Millisecond)
	defer cancel()
	routed, err := ignatius.Run(ctx, reg, req, plan)
	if err != nil {
		fatal(2, "%v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(routed)
	if !routed.OK {
		os.Exit(1)
	}
}

// openStoreFor loads the config and opens its store for an offline command.
func openStoreFor(path string) (gateway.Config, store.Store) {
	cfg, err := gateway.LoadConfig(path)
	if err != nil {
		fatal(2, "config: %v", err)
	}
	if cfg.Store == nil {
		fatal(2, "the config has no [store] table")
	}
	st, err := store.Open(cfg.Store.Driver, os.Getenv(cfg.Store.DSNEnv))
	if err != nil {
		fatal(2, "%v", err)
	}
	return cfg, st
}

// parseDate accepts YYYY-MM-DD or RFC 3339. Empty is the zero time.
func parseDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	fatal(2, "cannot parse date %q (want YYYY-MM-DD or RFC 3339)", s)
	return time.Time{}
}

func hasClient(cfg gateway.Config, name string) (storeContent bool, ok bool) {
	for _, c := range cfg.Clients {
		if c.Name == name {
			return c.StoreContent, true
		}
	}
	for _, u := range cfg.Users {
		if u.Name == name {
			return u.StoreContent, true
		}
	}
	return false, name == "default" || name == "anonymous"
}

func purge(args []string) {
	fs := flag.NewFlagSet("purge", flag.ExitOnError)
	path := fs.String("config", "ignatius.toml", "path to the TOML config")
	name := fs.String("client", "", "client whose records to delete (required)")
	before := fs.String("before", "", "delete records older than this date (default: now, so everything)")
	contentOnly := fs.Bool("content-only", false, "delete stored content only; keep request metadata and feedback")
	_ = fs.Parse(args)
	if *name == "" || fs.NArg() != 0 {
		usage()
	}
	_, st := openStoreFor(*path)
	defer st.Close()
	cut := parseDate(*before)
	if cut.IsZero() {
		cut = time.Now()
	}
	res, err := st.Purge(context.Background(), store.PurgeFilter{Client: *name, Before: cut, ContentOnly: *contentOnly})
	if err != nil {
		fatal(2, "purge: %v", err)
	}
	fmt.Printf("deleted %d requests, %d stored contents, %d feedback rows\n", res.Requests, res.Contents, res.Feedback)
}

func export(args []string) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	path := fs.String("config", "ignatius.toml", "path to the TOML config")
	name := fs.String("client", "", "client to export (required; must have store_content = true)")
	since := fs.String("since", "", "only requests at or after this date")
	_ = fs.Parse(args)
	if *name == "" || fs.NArg() != 0 {
		usage()
	}
	cfg, st := openStoreFor(*path)
	defer st.Close()
	if content, ok := hasClient(cfg, *name); !ok {
		fatal(2, "no client or user named %q in the config", *name)
	} else if !content {
		fatal(2, "client %q has not opted in (store_content = true), so no content was stored", *name)
	}
	n, err := gateway.ExportJSONL(context.Background(), st, *name, parseDate(*since), os.Stdout)
	if err != nil {
		fatal(2, "export: %v", err)
	}
	fmt.Fprintf(os.Stderr, "exported %d questions\n", n)
}

func calibrate(args []string) {
	fs := flag.NewFlagSet("calibrate", flag.ExitOnError)
	path := fs.String("config", "ignatius.toml", "path to the TOML config")
	name := fs.String("client", "", "client whose feedback to read (required)")
	since := fs.String("since", "", "only feedback at or after this date")
	source := fs.String("source", "human", `labels to count: "human", "automated", "audit" (shadow audits) or "all"`)
	target := fs.Float64("target", 0.95, "accuracy a kept answer should reach")
	minKept := fs.Int("min", 20, "answers that must clear a bar before it is recommended")
	asJSON := fs.Bool("json", false, "print JSON instead of tables")
	_ = fs.Parse(args)
	if *name == "" || fs.NArg() != 0 || (*source != "human" && *source != "automated" && *source != "audit" && *source != "all") {
		usage()
	}
	_, st := openStoreFor(*path)
	defer st.Close()
	fbs, err := st.Feedback(context.Background(), *name, parseDate(*since))
	if err != nil {
		fatal(2, "calibrate: %v", err)
	}
	if *source == "audit" || *source == "all" {
		as, err := st.Audits(context.Background(), *name, parseDate(*since))
		if err != nil {
			fatal(2, "calibrate: %v", err)
		}
		fbs = append(fbs, gateway.AuditFeedback(as)...)
	}
	if *source != "all" {
		kept := fbs[:0]
		for _, f := range fbs {
			if f.Source == *source {
				kept = append(kept, f)
			}
		}
		fbs = kept
	}
	rows := gateway.Calibrate(fbs, *target, *minKept)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rows)
		return
	}
	gateway.WriteCalibration(os.Stdout, rows, *target, *minKept)
}

// routeList is a repeatable --route flag.
type routeList []string

func (r *routeList) String() string     { return strings.Join(*r, ",") }
func (r *routeList) Set(v string) error { *r = append(*r, v); return nil }

// evalCmd runs a labeled test set through one or more routes and judges what comes back (SPEC 15). Every
// route is a route name, a profile, an alias or an inline route; the first is the baseline the others are
// compared with. It makes real calls to the models the routes name.
func evalCmd(args []string) {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	path := fs.String("config", "ignatius.toml", "path to the TOML config")
	data := fs.String("data", "", "the labeled test set: JSONL, one item per line, or - for stdin (required)")
	var routes routeList
	fs.Var(&routes, "route", "a route, profile, alias or inline route to judge (repeat; the first is the baseline)")
	sweep := fs.Bool("sweep", false, "also sweep the threshold of each cascade (one extra run of each of its models)")
	conc := fs.Int("concurrency", 4, "items in flight at once")
	misses := fs.Int("misses", 10, "wrong answers to list per route (-1 for all, 0 for none)")
	jsonOut := fs.String("json", "", "also write the full report here as JSON")
	maxItems := fs.Int("max-items", 0, "refuse a larger dataset (0 = no limit)")
	_ = fs.Parse(args)
	if *data == "" || len(routes) == 0 || fs.NArg() != 0 {
		usage()
	}
	cfg, err := gateway.LoadConfig(*path)
	if err != nil {
		fatal(2, "config: %v", err)
	}
	reg, err := ignatius.BuildRegistryWith(cfg.Models, os.Getenv, ignatius.Options{Cache: cfg.Cache})
	if err != nil {
		fatal(2, "models: %v", err)
	}
	rt, err := gateway.NewRouter(cfg, reg)
	if err != nil {
		fatal(2, "routes: %v", err)
	}
	var in io.Reader = os.Stdin
	if *data != "-" {
		f, err := os.Open(*data)
		if err != nil {
			fatal(2, "data: %v", err)
		}
		defer f.Close()
		in = f
	}
	items, err := eval.ParseJSONL(in, *maxItems)
	if err != nil {
		fatal(2, "data: %v", err)
	}
	var cands []eval.Candidate
	for _, name := range routes {
		plan, err := rt.Resolve(name)
		if err != nil {
			fatal(2, "route %q: %v", name, err)
		}
		cands = append(cands, eval.Candidate{Name: name, Plan: plan})
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	last := time.Now()
	rep, err := eval.Run(ctx, reg, items, cands, eval.Options{
		Concurrency: *conc, Sweep: *sweep, Misses: *misses,
		ItemTimeout: time.Duration(cfg.RequestTimeoutMS) * time.Millisecond,
		Progress: func(done, total int) {
			if done == total || time.Since(last) > 2*time.Second {
				last = time.Now()
				fmt.Fprintf(os.Stderr, "\r%d of %d done", done, total)
			}
		},
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fatal(2, "eval: %v", err)
	}
	fmt.Print(rep.Text())
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*jsonOut, append(b, '\n'), 0o644); err != nil {
			fatal(2, "json: %v", err)
		}
	}
}

func fatal(code int, format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ignatius: "+format+"\n", a...)
	os.Exit(code)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hashPassword reads one password from stdin (so it never lands in shell history or
// argv) and prints its bcrypt hash for a [[users]] entry.
func hashPassword() {
	pw, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
	if err != nil {
		log.Fatal(err)
	}
	pw = bytes.TrimRight(pw, "\r\n")
	switch {
	case len(pw) == 0:
		log.Fatal("hash-password: no password on stdin")
	case len(pw) > 72:
		log.Fatal("hash-password: bcrypt ignores everything past 72 bytes; use a shorter password")
	}
	h, err := bcrypt.GenerateFromPassword(pw, 12)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(h))
}
