# Ignatius

Ignatius is a self-hosted gateway that sits in front of decision models like Jev, Clef and Decis. You ask one set of questions and get back normalized answers from whichever model (or models) you pick.

## Why

Decision models come in two flavors: fast and cheap, or slow and smart. You usually want both. Cheap for the easy stuff, smart for the hard stuff.

Every provider also answers a little differently. So you end up writing glue to normalize it all.

You could do that in a script. I did, kind of. But then you're also writing the retries, the thresholds, the logging, and the "why did it pick that one" part. That's the part that actually matters. Ignatius does the orchestration and hands you a trace of every decision it made.

## What it does

Three modes:

- **Single**: send the questions to one model. It's just a pass through that normalizes the answer.
- **Fan-out**: send the same questions to n models at once. You get all the answers back, or you combine them (vote, mean, most confident).
- **Cascade**: ask the cheap model first. Check how sure it is. Only send the questions it wasn't sure about to the next tier up.

```
questions ──> cheap model ──> confident? ──yes──> answer
                                  │
                                  no
                                  v
                             strong model ──> answer
```

The saving comes from the cascade. The cheap tier is confident on most questions, so the expensive model only ever sees the hard ones.

## Quickstart

### Run the gateway

Build the single binary (Go 1.26), or the distroless image, from the `go/` directory:

```sh
git clone https://github.com/phin-tech/ignatius && cd ignatius/go
go build -o ignatius ./cmd/ignatius      # or: docker build -t ignatius .
```

A minimal `ignatius.toml` with one model. Secrets are environment variables the file names, never values in it:

```toml
listen = "127.0.0.1:8081"
default_route = "jev"

[models.jev]
provider = "systemone"                 # anything that speaks /v1/systemone
base_url = "https://api.typesafe.ai"
model = "jev-latest"
api_key_env = "TYPESAFE_API_KEY"
```

```sh
export TYPESAFE_API_KEY=...
./ignatius serve --config ignatius.toml
# {"msg":"ignatius listening on 127.0.0.1:8081; models [jev]; routes []"}
```

`GET /healthz` and `GET /readyz` answer without a key, and `/` is the status page. On a loopback address no client key
is needed; to listen anywhere else set `api_key_env` (the gateway refuses to start on a non-loopback address with no client key, unless you set `IGNATIUS_ALLOW_NO_AUTH=1`).

### Make a call

The gateway speaks Jev's `/v1/systemone`, so anything that talks to Jev can talk to it:

```sh
curl -s localhost:8081/v1/systemone -d '{
  "state": {"document": "I was charged twice. Please fix this ASAP."},
  "model": "jev",
  "questions": {
    "billing": {"type": "noul",   "instructions": "Is this ticket about billing?"},
    "tone":    {"type": "choice", "instructions": "What is the customer'"'"'s tone?",
                "criteria": {"calm": null, "frustrated": null}}
  }
}'
```

```json
{
  "model": "jev",
  "answers": {
    "billing": {"type": "noul", "noul": 0.98},
    "tone": {"type": "choice", "choice": "frustrated", "probabilities": {"calm": 0, "frustrated": 1}, "confidence": 1}
  },
  "usage": {"input_tokens": 321, "output_tokens": 52},
  "ignatius": {"mode": "single", "request_id": "7aec6282a1af5595", "sources": {"billing": {"model": "jev"}, "tone": {"model": "jev"}}, "trace": null, "failures": []}
}
```

The first four fields are exactly what Jev returns. `ignatius` is the extra object only Ignatius adds, and clients that
do not know it ignore it.

**The official SDK**, unchanged: point `base_url` at the gateway. The SDK has no knob for routing, so the `model` name
selects a route, alias or inline route (below), and the SDK's default `jev-latest` selects `default_route`.

```python
from typesafe_sdk import Choice, Noul, TypeSafeClient     # pip install typesafe-sdk

with TypeSafeClient(api_key="local", base_url="http://127.0.0.1:8081") as client:
    response = client.system_one(
        state={"document": "I was charged twice. Please fix this ASAP."},
        questions={
            "billing": Noul(instructions="Is this ticket about billing?"),
            "tone": Choice(instructions="What is the customer's tone?", criteria={"calm": None, "frustrated": None}),
        },
    )
    print(response.nouls["billing"].noul, response.choices["tone"].choice)      # 0.98 frustrated
```

**The Ignatius SDKs** run the same config in process, with no gateway, or call a running one, and return the whole
result with its trace. Python (not on PyPI yet, so install from the repo: `pip install "git+https://github.com/phin-tech/ignatius.git#subdirectory=python"`):

```python
from ignatius import Ignatius, Question, Request

ign = Ignatius.from_config("ignatius.toml")                # the same TOML the gateway reads
routed = ign.run_sync(Request(
    {"document": "I was charged twice. Please fix this ASAP."},
    {"billing": Question("noul", "Is this ticket about billing?")},
), model="triage")
a = routed.answers["billing"]
print(a.noul, a.model, a.confidence)                        # which model answered, and how sure it was

# or against a running gateway (the native /v1/route):
#   from ignatius import GatewayClient; asyncio.run(GatewayClient("http://127.0.0.1:8081").route(request, route="triage"))
```

Go (`import "github.com/phin-tech/ignatius/go/ignatius"` and `.../go/gateway`):

```go
cfg, _ := gateway.LoadConfig("ignatius.toml")
reg, _ := ignatius.BuildRegistry(cfg.Models, os.Getenv)
router, _ := gateway.NewRouter(cfg, reg)
plan, _ := router.Resolve("triage")                         // a route, a profile, an alias or an inline route
routed, err := ignatius.Run(ctx, reg, ignatius.Request{
    State:     map[string]any{"document": "I was charged twice. Please fix this ASAP."},
    Questions: map[string]ignatius.Question{"billing": {Type: "noul", Instructions: "Is this ticket about billing?"}},
}, plan)
```

**Reading the trace.** Add a second model and a cascade route, so a cheap tier answers what it is sure of and the rest goes on:

```toml
[models.cheap]                     # any Jev-compatible server: Decis, Ollama's Clef, the Worker in deploy/clef-worker
provider = "systemone"
base_url = "http://localhost:9000"
model = "clef-flash"
confidence = "derived"             # judge it by its probabilities, so thresholds mean the same at every tier (SPEC 1.2)

[routes.triage]
mode = "cascade"
tiers = [ { model = "cheap", threshold = 0.9 }, "jev" ]    # keep a cheap answer at 0.9 confidence or more
```

A request with three questions (a yes/no, a choice and a 4-level score) came back with this `ignatius` object (numbers
rounded):

```json
{
  "mode": "cascade",
  "sources": {"billing": {"model": "cheap"}, "tone": {"model": "cheap"}, "urgency": {"model": "jev"}},
  "failures": [],
  "trace": [
    {"tier": 0, "model": "cheap", "questions": ["billing", "tone", "urgency"], "escalated": ["urgency"], "latency_ms": 533,
     "detail": [
       {"id": "billing", "confidence": 0.91, "threshold": 0.9, "escalated": false},
       {"id": "tone",    "confidence": 0.90, "threshold": 0.9, "escalated": false},
       {"id": "urgency", "confidence": 0.72, "threshold": 0.9, "escalated": true}
     ]},
    {"tier": 1, "model": "jev", "questions": ["urgency"], "escalated": [], "latency_ms": 478,
     "detail": [{"id": "urgency", "confidence": 0.92, "escalated": false}]}
  ]
}
```

- **`sources`** says which model gave each final answer: the cheap model for `billing` and `tone`, Jev for `urgency`.
- **`trace`** has one entry (a hop) per tier that was actually called, in order. Tier 1 was asked only `urgency`, because
  the cheap tier was confident enough about the other two: that is where the saving comes from.
- Each hop's **`detail`** is the decision per question: the tier's `confidence`, the `threshold` it had to meet, and whether
  the question `escalated`. `urgency` scored 0.72 against a bar of 0.9, so it went to Jev. The last tier has no threshold: it answers.
- **`failures`** lists any tier that errored (a timeout, an open circuit breaker, an image it cannot take). A failed tier
  escalates everything it was asked, so the request still gets answered; the failure is data, not an error.
- **`request_id`** is the same as the `X-Request-Id` header; it is what you give back when you say whether a decision was
  good (feedback, SPEC 13).

Other modes use the same shape: `fan_out` asks several models at once and combines them (`reduce = "vote"`), and a
`model` string like `cascade:cheap@0.9>jev` makes a route on the fly with no config.

## Concepts

### Models and providers

A `systemone` model is just a URL and some auth. Anything that speaks `/v1/systemone` works: Jev, Clef (through Ollama), Decis, even another Ignatius. Hosted Clef on Cloudflare Workers AI is `provider = "workers-ai"`. There's also `deploy/clef-worker`. It's a Cloudflare Worker that serves the Jev-compatible API in front of Workers AI Clef, so hosted Clef looks like any other `systemone` model. Images and per-model `limits` are in SPEC 3.1, and `deploy/clef/ignatius.toml` is an example.

### Routes

A route is a named plan: single, fan-out or cascade. You can also use an alias, or write one inline in the model name, like `cascade:a>b` or `fan-out:a,b|vote`. If you don't name one, you get `default_route`. An empty model name or `jev-latest` means the same.

### Cascade and thresholds

Each tier has a confidence threshold. If the tier's answer is at or above it, the answer stays. If it's below, that question goes to the next tier. I let you set thresholds per tier and per question type. If a tier fails (timeout, open circuit breaker), everything it was asked escalates to the next tier. If the last tier fails, those questions keep the best answer seen so far, or have none, and the failure shows up in `failures` and the trace.

### Fan-out and reducers

Raw fan-out returns every model's answer. I added reducers (`vote`, `mean`, `most_confident`) to collapse them into one. `min_success` sets how many models have to answer for the result to count.

### Confidence

Confidence is a normalized number saying how sure a model was. It doesn't mean the same thing across providers. That's why I made thresholds per tier. Set `confidence = "derived"` on a model to judge it by its probabilities so tiers sit on one scale (SPEC 1.2). I wouldn't trust it until you calibrated it on your own data.

## API

### Jev-compatible (L1): `/v1/systemone`

Anything that talks to Jev can talk to this, including the official SDK. I put an extra `ignatius` object in the response. Clients that don't know it ignore it.

### Native (L2): `/v1/route`

This one takes a plan and returns the full result envelope: answers, which model gave each one, the trace, and any failures. I made failures data here, not errors.

## Configuration

I put everything in `ignatius.toml`: server keys, `[models.*]`, `[routes.*]`, auth. Secrets are never values in the file. You name an environment variable (`api_key_env`) and it reads it.

## Profiles

A profile names an intent (`fast`, `best`) so you can swap the model behind it in one line. Aliases are concrete models (`jeff`, `jev`) and profiles are intents. Don't name an alias after a quality. I made them resolve to the real model, so stats and traces stay honest. You can edit profiles and routes on the status page, but I left it off by default and admin only. If you set `[admin] state_file`, it saves edits there. Otherwise they're lost on restart. Models and clients aren't editable there on purpose, and it isn't built for several replicas yet.

## Reliability, caching and cost

- **Circuit breaker**: opens when a model keeps failing, so the cascade skips it instead of waiting on timeouts.
- **Response cache**: I made it opt-in. Keys are per question, with a TTL. The tradeoff is privacy, since answers get held in memory.
- **Cost accounting**: put prices in the config and the status page shows spend and an estimated saving. That estimate can be negative if your cheap tier escalates too much.

## Clients and access control

I give named clients their own keys (from env vars), rate limits, route allowlists, and admin-only stats access.

## Feedback and the store

I made the `[store]` optional: SQLite by default, Postgres if you run replicas. Send a good/bad verdict and an optional correct answer per question with `POST /v1/ignatius/feedback` or the Python `GatewayClient.feedback()`. Content is only stored for clients with `store_content = true`, and nothing is deleted automatically unless you set `retention_days` (per client, or on `[store]`). `ignatius purge` deletes on demand. In a container, point `IGNATIUS_STORE_DSN` at a mounted volume. Request records are written asynchronously through a bounded queue, so a full queue or a crash can lose records. Feedback is written synchronously.

`ignatius calibrate`, `export` (JSONL for fine-tuning) and `purge` work off that data. I added shadow auditing (`[audit] sample_rate`). It asks the next tier the questions a cheap tier settled, in the background, and records whether they agreed. Those are labels with no human in the loop, at the price of one next-tier call per audited question. Use them with `calibrate --source audit`. SPEC 13.

## Events

`[[events.sinks]]` sends events to a webhook (CloudEvents, optionally HMAC-signed with `secret_env`, with retries and an idempotency id), a JSONL file, or stdout. I made the webhook sink optional at build time, so a minimal build can't make outbound HTTP calls from a sink. Kinesis and Kafka are compile-time plugins in `go/plugins/kinesis` and `go/plugins/kafka`. Each is its own module. You add them with a Caddy-style blank import. I kept delivery best effort and in memory. Everyone gets metadata, only opted-in clients get content. SPEC 14.

## Judging routes on your own data

Is the cascade as good as the expensive model alone, and what does it save? Give `ignatius eval` a labeled test set and the routes
to compare; it runs the real plans and reports accuracy with a confidence interval, what each tier contributed, cost, latency,
how well the confidence tracks correctness, and a plain-language verdict against the first route. `--sweep` also shows where a
cascade's threshold should be. The same engine is behind an admin API and an Evaluate panel on the status page
(`[eval] enabled = true`; every run makes real model calls). SPEC 15 has the test-set format and exactly what is judged;
`examples/eval` has two sample test sets to try it on (hand-written support tickets, and 72 real GitHub pull requests), and `benchmarks/classify` is a worked example on public data.

```sh
ignatius eval --config ignatius.toml --data test.jsonl --route strong --route 'cascade:cheap@0.8>strong' --sweep
```

```
# a cascade whose cheap tier handled three quarters of the questions, on a 40-item set where both were right every time:
verdict: no detectable difference from strong (+0.0 points, 95% CI +0.0 to +0.0), at 35% of its cost
```

## Status page

`/` is the status page. It shows the live state: models, routes, spend, and a drill-down per request. When you set a key, it asks for one.

## Self-hosting models (dunce-union)

This is the optional kit for running the models yourself. I included it for when you don't want to call a hosted API. It's a compose setup. You can also skip it and just point Ignatius at your own URL.

Be honest with yourself about model quality. The models differ, and so does how well their confidence is calibrated.

## SDKs

Python (not on PyPI yet; install from the repo with `pip install "git+https://github.com/phin-tech/ignatius.git#subdirectory=python"`) and Go (`github.com/phin-tech/ignatius/go/ignatius`). Both can run the same config in process, no gateway needed, or talk to a running one. The Quickstart has an example of each.

## Observability

Traces, metrics, logs, plus a Prometheus endpoint. A cascade shows up as a span tree with a decision event per question. That's how you read what it did.

Telemetry never carries error messages, only error kinds, and content never goes into it. A test checks that. The stats store keeps only sanitized error messages. Ignatius continues an authenticated caller's trace. Forwarding upstream is opt-in.

I left OpenTelemetry out of the Python SDK on purpose. Instrument httpx instead.

## Security and privacy

Secrets come from env vars. Auth is per client key. Loopback needs none, and the gateway refuses to start on a non-loopback address without one, unless you set `IGNATIUS_ALLOW_NO_AUTH=1`.

What gets logged and stored is metadata unless a client opts in to content. Trace headers only go upstream if you turn that on.

## Development

The Go gateway is in `go/`, the Python SDK in `python/`, and the shared fixtures live in `spec/`. `SPEC.md` is the contract.

Run the Go and Python suites, plus the interop test. I made a few integration tests opt-in; they skip without their environment. They cover the store on Postgres (`IGNATIUS_TEST_POSTGRES_DSN`), the Kinesis sink on the Floci emulator (`IGNATIUS_TEST_KINESIS_ENDPOINT`), the Kafka sink on Floci's MSK (`go/plugins/kafka/test-msk-floci.sh`), a real Kafka broker (`IGNATIUS_TEST_KAFKA_BROKERS`, `IGNATIUS_TEST_KAFKA_TOPIC`), and the whole feedback loop against real Jev (`IGNATIUS_REAL=1 TYPESAFE_API_KEY=... go test ./gateway -run TestRealJev -v`). CI runs the first three.

## Roadmap

Short list in `SPEC.md` section 10.

## License

Copyright 2026 Sam Phinizy. Licensed under the Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
