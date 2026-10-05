package events

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// A sink type is the unit of extension, the way a Caddy module is: a package registers its
// type from init(), and a build that blank-imports the package has it. The core binary has
// "webhook" and "file". Anything that needs a heavy dependency (a broker client, a cloud
// SDK) lives in its own Go module and is compiled in only by a build that asks for it
// (SPEC 14.5), so the core's go.mod and binary never carry it.

// SinkType describes one kind of sink.
type SinkType struct {
	// Validate checks a sink's config without touching the environment. May be nil.
	Validate func(SinkConfig) error
	// New builds the sink. getenv resolves the *_env settings; a missing one is an error
	// here, so a typo is a startup failure rather than silently dropped events. An error
	// must not echo a URL, a credential or any value read from the environment.
	New func(SinkConfig, func(string) string) (Sink, error)
}

var (
	regMu    sync.RWMutex
	registry = map[string]SinkType{}
)

// RegisterSinkType makes a sink type available under name. Call it from init(). It panics on
// an empty name, a nil New, or a name that is already taken, so two plugins cannot silently
// shadow each other.
func RegisterSinkType(name string, t SinkType) {
	regMu.Lock()
	defer regMu.Unlock()
	switch {
	case name == "":
		panic("events: RegisterSinkType with an empty name")
	case t.New == nil:
		panic(fmt.Sprintf("events: sink type %q has no New", name))
	}
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("events: sink type %q is registered twice", name))
	}
	registry[name] = t
}

func registered(name string) bool {
	regMu.RLock()
	defer regMu.RUnlock()
	_, ok := registry[name]
	return ok
}

func lookup(name string) SinkType {
	regMu.RLock()
	defer regMu.RUnlock()
	return registry[name]
}

// SinkTypes lists the registered types, sorted.
func SinkTypes() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Option helpers for plugins reading SinkConfig.Options (TOML decodes integers as int64).

// Timeout is the sink's timeout_ms, or 5 seconds when it is not set.
func (s SinkConfig) Timeout() time.Duration {
	return time.Duration(firstPositive(s.TimeoutMS, 5000)) * time.Millisecond
}

// OptString returns Options[key] if it is a string.
func (s SinkConfig) OptString(key string) string {
	v, _ := s.Options[key].(string)
	return v
}

// OptInt returns Options[key] if it is an integer, else 0.
func (s SinkConfig) OptInt(key string) int {
	switch v := s.Options[key].(type) {
	case int64:
		return int(v)
	case int:
		return v
	case float64:
		return int(v)
	}
	return 0
}

// OptKeys returns the option names, sorted, so a plugin can reject unknown ones.
func (s SinkConfig) OptKeys() []string {
	out := make([]string, 0, len(s.Options))
	for k := range s.Options {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// OptBool returns Options[key] if it is a boolean.
func (s SinkConfig) OptBool(key string) bool {
	v, _ := s.Options[key].(bool)
	return v
}

// OptStrings returns Options[key] if it is a list of strings (an array of anything else, or
// a non-array, gives nil and false).
func (s SinkConfig) OptStrings(key string) ([]string, bool) {
	raw, ok := s.Options[key].([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		str, ok := v.(string)
		if !ok {
			return nil, false
		}
		out = append(out, str)
	}
	return out, true
}
