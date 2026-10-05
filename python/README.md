# ignatius (Python SDK)

Orchestrate System One decision models (Jev, Clef, Jeff, Laya, ...): single,
fan-out and cascade, with normalized answers. Same behavior as the Go library;
both pass the shared fixtures in `../spec/fixtures/`. Requires Python 3.11+.

```python
import asyncio
from ignatius import Ignatius, Question, Request

ig = Ignatius.from_config("ignatius.toml")      # the same TOML the Go gateway reads
req = Request(
    {"subject": "Duplicate charge", "body": "Refund it today or we cancel."},
    {"churn": Question("noul", "Does the user threaten to leave?"),
     "team": Question("choice", "Who handles this?", {"billing": "invoices", "sales": "pricing"})},
)

routed = asyncio.run(ig.run(req, model="cascade:jeff@0.8>jev"))   # or a route name, alias, "fan-out:a,b|vote"
print(routed.answers["team"].choice, routed.answers["team"].model)
for hop in routed.trace:                          # why each tier escalated (or didn't)
    print(hop.model, [(q.id, q.confidence, q.threshold, q.escalated) for q in hop.detail])
```

Talk to a running gateway instead of calling models directly:

```python
from ignatius import GatewayClient, Plan
routed = asyncio.run(GatewayClient("http://localhost:8081", api_key="...").route(req, route="fast_then_smart"))
```

Provider failures are data (`routed.failures`, `routed.ok`), not exceptions; only
an invalid plan raises (`PlanError`, `ResolveError`).

```
uv sync && uv run pytest        # the interop tests need `go` on PATH and skip without it
```

## Tracing

The SDK has no OpenTelemetry code. To make an app's trace continue into the gateway,
instrument `httpx` the standard way (`opentelemetry-instrumentation-httpx`); the gateway
picks up the `traceparent` and its log lines and spans carry your trace id.

```python
from opentelemetry.instrumentation.httpx import HTTPXClientInstrumentor
HTTPXClientInstrumentor().instrument()   # before using GatewayClient
```

## Profiles

`[profiles]` in the config names an intent and points it at a model alias, and works
wherever a model name does (`model="fast"`, route tiers, `cascade:fast>best`). The SDK
loads the same table as the gateway with the same rules; it is static here, since there is
no runtime editing in an SDK.

```toml
[profiles]
fast = "jeff"
best = "jev"
```
