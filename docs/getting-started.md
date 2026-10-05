---
title: Getting started
nav_order: 2
---

# Getting started
{: .no_toc }

You need Go 1.26. You also need a Jev API key, or any server that speaks `/v1/systemone`.

## Run the gateway

1. Clone the repo. Build the binary from the `go/` directory.

```sh
git clone https://github.com/phin-tech/ignatius && cd ignatius/go
go build -o ignatius ./cmd/ignatius      # or: docker build -t ignatius .
```

2. Write an `ignatius.toml` with one model. Secrets come from environment variables, and the file names those variables. It never contains secret values.

```toml
listen = "127.0.0.1:8081"
default_route = "jev"

[models.jev]
provider = "systemone"                 # anything that speaks /v1/systemone
base_url = "https://api.typesafe.ai"
model = "jev-latest"
api_key_env = "TYPESAFE_API_KEY"
```

3. Export the key, then start the gateway.

```sh
export TYPESAFE_API_KEY=...
./ignatius serve --config ignatius.toml
```

4. Check that it is up. `GET /healthz` and `GET /readyz` answer without a key. `/` is the status page.

No client key is needed on loopback. To listen anywhere else, set `api_key_env`. See [Auth](auth.html).

## Make a call

The gateway speaks Jev's `/v1/systemone`. Anything that can talk to Jev can talk to it.

1. Send two questions: a yes/no and a choice.

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

2. Read the answer. The first four fields are what Jev returns. The `ignatius` object is extra. Clients that don't know about it ignore it.

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

## Use the Python SDK

Ignatius is not on PyPI yet.

1. Install it from the repository.

```sh
pip install "git+https://github.com/phin-tech/ignatius.git#subdirectory=python"
```

2. Point it at the same config the gateway reads, then run a request in-process. You do not need the gateway for this.

```python
from ignatius import Ignatius, Question, Request

ign = Ignatius.from_config("ignatius.toml")
routed = ign.run_sync(Request(
    {"document": "I was charged twice. Please fix this ASAP."},
    {"billing": Question("noul", "Is this ticket about billing?")},
), model="jev")
a = routed.answers["billing"]
print(a.noul, a.model, a.confidence)
```

3. Use `GatewayClient` if you want to use a running gateway instead.

## Use the Go SDK

1. Import the `github.com/phin-tech/ignatius/go/ignatius` and `github.com/phin-tech/ignatius/go/gateway` packages.
2. Load the config. Build the registry and the router. Then run a plan.

```go
cfg, _ := gateway.LoadConfig("ignatius.toml")
reg, _ := ignatius.BuildRegistry(cfg.Models, os.Getenv)
router, _ := gateway.NewRouter(cfg, reg)
plan, _ := router.Resolve("jev")
routed, err := ignatius.Run(ctx, reg, ignatius.Request{
    State:     map[string]any{"document": "I was charged twice. Please fix this ASAP."},
    Questions: map[string]ignatius.Question{"billing": {Type: "noul", Instructions: "Is this ticket about billing?"}},
}, plan)
```

## Add a cascade

1. Add two things to `ignatius.toml`: a cheap model and a cascade route.

```toml
[models.cheap]                     # Decis, Ollama's Clef, or the Worker in deploy/clef-worker
provider = "systemone"
base_url = "http://localhost:9000"
model = "clef-flash"
confidence = "derived"             # judge it by its probabilities, so thresholds mean the same at every tier

[routes.triage]
mode = "cascade"
tiers = [ { model = "cheap", threshold = 0.9 }, "jev" ]    # keep a cheap answer at 0.9 confidence or more
```

2. Call it with `"model": "triage"`.
3. Read the trace. `sources` tells you which model produced each final answer. `trace` has one entry per tier that was called. Each entry shows the question's confidence, the threshold, and whether it escalated. `failures` lists any tier that errored. A failed tier escalates everything it was asked.
