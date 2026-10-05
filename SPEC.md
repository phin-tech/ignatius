# Ignatius: Decision Model Orchestrator, Spec v0

Ignatius orchestrates calls to one or more System One decision models
(Jev, Jeff, Laya, Kev, Clef, ...), normalizes the answers, and offers composition
modes (single, fan_out, cascade). The Go (`go/`) and Python (`python/`) SDKs
implement this spec identically and both must pass `spec/fixtures/`.

## 1. Normalized types

JSON field names are the canonical names; SDKs may use idiomatic casing in code
but serialize exactly these.

```
Request   { state: any, questions: { <id>: Question } }
Question  { type: "noul"|"choice"|"score", instructions: string|object, criteria?: any }
Answer    {
  type:          "noul"|"choice"|"score"
  noul?:         float          # P(yes), noul only
  choice?:       string         # choice only
  score?:        float          # score only
  probabilities?: {string|number: float}   # choice/score distribution
  confidence:    float|null     # NORMALIZED, see 1.1
  model:         string         # alias that produced this answer, or "ensemble" (6.0)
  contributors?: [string]       # aliases behind a reduced (ensemble) answer
}
Result    {
  model:      string            # alias
  answers:    { <id>: Answer }
  usage:      { input_tokens: int, output_tokens: int }
  latency_ms: int
  cost_usd?:  float             # from the model's configured prices (11.3); cache hits cost 0
  cached?:    int               # how many of the questions came from the cache (11.2)
}
Failure   { model: string, error: { kind: string, message: string }, latency_ms: int }
```

`error.kind` is one of: `timeout`, `transport`, `http`, `decode`, `unsupported`,
`circuit_open` (the model's breaker is open, 11.1).

### 1.1 Normalized confidence

- choice, score: the provider's `confidence` if present, else `max(probabilities)`.
- noul: `abs(noul - 0.5) * 2` (0 = coin flip, 1 = certain). Providers send no
  separate confidence for noul.
- `null` only if nothing can be derived.

Confidence summarizes distribution concentration, not correctness. Thresholds
are policy the caller sets.

### 1.2 Confidence policy

The numbers 1.1 normalizes are not on one scale. Jev's formula is undisclosed; Decis documents one
formula for every engine but keeps each engine's own number in `decis.native_confidence`; and a
provider that sends no confidence gets `max(probabilities)`, which is not chance-corrected (0.6
over two options is 0.2 above a coin flip, not 0.6). A model's `confidence` setting chooses what
its thresholds are measured against:

```toml
[models.laya]
confidence = "derived"             # "provider" (the default) | "derived"
```

- **`provider`** is 1.1 exactly as above, and the default. It is right for Jev and Decis, whose
  published formulas are the ones below, so the two policies agree and nothing changes. Checked for
  Jev against the live API (2026-10-04, 10 answers: choice and score, from near-certain to
  ambiguous): its `confidence` matched the formula applied to its own `probabilities` to within
  0.01, the gap being the wire rounding probabilities to two decimals while Jev computes its number
  from the unrounded ones. So `derived` on Jev is the same scale, quantized to 0.01 or so.
- **`derived`** computes the confidence from the answer's own `probabilities`, whichever provider
  answered, with TypeSafe's published formulas (the ones Decis uses):

  ```
  choice, K options:  (p_max - 1/K) / (1 - 1/K)
  score,  n levels:   max(0, 1 - sum_i p_i * |i - m| / MAD_uniform)      m = the most likely level
                      MAD_uniform = (1/n) * sum_i |i - (n-1)/2|
  ```

  0 is no better than a uniform guess and 1 is certain. The provider's own number, if it sent one,
  is kept in `native_confidence` on the answer (on `/v1/route`; the Jev-compatible `/v1/systemone`
  wire has no such field). Where there is nothing to derive from (no probabilities, or score levels
  that are not integer indices) the provider's number stands. A single option or level is 1.
  Noul is unaffected: its confidence is always `abs(p - 0.5) * 2`.
- **Use `derived`** for a model whose number you cannot vouch for or that sends none: an open-weight
  engine behind a server that does not normalize, a future provider. It puts every tier's threshold
  on one scale, so "0.8" means the same shape of distribution at every tier.
- **Changing a model's policy mixes scales in what is already stored.** Outcomes, feedback and audits
  keep the confidence the policy produced at the time, and a row does not say which policy that was.
  After switching a model between `provider` and `derived`, calibrate with `--since <the date of the
  change>` (13.4), or the sweep puts two scales together.
- **Fan-out reducers other than `most_confident`** (`vote`, `mean`) merge the answers and then derive
  the ensemble's confidence from the merged probabilities by the 1.1 fallback (`max(probabilities)`),
  whatever the models' policies, so an ensemble answer is not on the derived scale. Cascades, `single`
  and `most_confident` keep the per-model number and are unaffected.
- **What it does not do.** The same scale is not the same reliability: a model can be confidently
  wrong, and two models at 0.8 are right at different rates. Thresholds stay per tier and are tuned
  on data (13.4, 13.7); this only stops the scale from changing under them. It also leaves the 1.1
  fallback alone for a `provider` model that sends no number, so mixing such a model with
  chance-corrected ones is the case to set `derived` on.
- **Where it applies.** Directly on the provider, below pricing, the breaker and the cache, so
  everything above stores and compares the policy's number. Changing it needs a restart (the cache
  is in memory). Go and Python implement it identically, checked by the shared fixture
  `spec/fixtures/confidence_derived.json`, which fails if the policy is ignored (the provider's 0.99
  would settle a question that the derived 0.55 escalates).

## 2. Providers

A provider turns a `Request` into a `Result` or `Failure` for one alias.

- `systemone`: HTTP `POST {base_url}/v1/systemone` (default
  `https://api.typesafe.ai`), `Authorization: Bearer <key>` if a key is set, body
  `{state, model, questions}`, response `{model, answers, usage}`. Covers Jev,
  Jeff, Laya, Kev, Decis, and any Jev-compatible server. Use for local servers
  with `base_url: http://localhost:PORT` and no key.
- `openai` (later, stub in v0): adapts to a text LLM via structured output.
  Returns `Failure{kind: unsupported}` in v0.

Per-provider-call timeout comes from the alias (`timeout_ms`, default 10000).

## 3. Config

TOML, for both the Go server and the Python SDK (Python 3.11+ reads it with the
standard library `tomllib`). API keys are referenced by env var name, never
inlined, so the file is safe to commit and to mount from a ConfigMap.

```toml
listen = ":8081"
api_key_env = "IGNATIUS_API_KEY"   # client keys for the gateway, comma-separated
default_route = "fast_then_smart"

[models.jev]
provider = "systemone"             # any Jev-compatible server; "openai" is a stub
base_url = "https://api.typesafe.ai"
model = "jev-latest"
api_key_env = "TYPESAFE_API_KEY"   # sent as Authorization: Bearer ...
# auth_header = "X-Api-Key"        # or send the raw key in this header instead
timeout_ms = 5000
# confidence = "derived"           # judge answers by their probabilities, not the provider's number (1.2)

[models.jeff]
provider = "systemone"
base_url = "http://localhost:8081"
model = "jev-latest"
```

Per-model resilience and cost keys (all optional, section 11):

```toml
[models.jev]
price_input_per_mtok = 0.042       # USD per million input tokens
price_output_per_mtok = 0.0
cache = true                       # override the global [cache] for this model
[models.jev.breaker]
failure_threshold = 5              # consecutive failures that open the breaker
cooldown_ms = 30000                # how long it stays open before one probe call

[cache]                            # global; off unless enabled
enabled = false
ttl_ms = 300000
max_entries = 10000

[[clients]]                        # gateway only (11.4)
name = "billing-app"
key_env = "BILLING_APP_KEY"
rate_limit_per_minute = 600        # 0 = unlimited
burst = 60                         # default: max(1, rate/10)
admin = false                      # may read /v1/stats and the status page data
routes = ["fast_then_smart"]       # optional allowlist; "inline" permits inline routes
```

Server-level keys: `listen`, `api_key_env`, `max_request_bytes`,
`request_timeout_ms`, `default_route`, `allow_inline_routes`,
`max_inline_models`. Top-level keys must precede the first `[table]`.

### 3.1 Images, limits and Clef

**Images.** A request may carry `images`: an array of base64 PNG, JPEG or WebP strings, shared by
every question, on `/v1/systemone` and in the `request` of `/v1/route` (and in the Python SDK's
`Request`). Each must be a non-empty string (422 `invalid_request` names the one that is not); the
gateway does not decode or inspect them. They are forwarded to a model as an `images` field next to
`state`, and omitted when there are none, so a model that has never heard of the field sees
exactly what it always did. Images are large: raise `max_request_bytes` (the 2 MiB default is a few
photographs).

**Limits.** What one call to a model can take is its `[models.NAME.limits]`:

```toml
[models.clef.limits]
max_questions = 64     # more questions than this are split into calls of at most this many and merged
max_images = 4         # images one call may carry
```

- **No `[limits]` means no images** (and no question limit). A request with images is never sent to
  a model that would silently ignore them: the call fails with kind `unsupported` ("this model takes
  no images, the request has 1"), without reaching the model or its circuit breaker. In a cascade that
  is an ordinary tier failure, so the tier is skipped and the next one, say Clef after Jev, answers; the
  response's `failures` and `trace` record the skip. In a fan-out it is one member's failure.
  On `/v1/systemone`, if the request has images and **every** failure is `unsupported` (no model on the
  route takes them), the gateway answers **422 `images_not_supported`**, with the failures and trace in
  the body, instead of 502 `routing_failed`: nothing failed upstream, the caller's request cannot be
  served by this route, and a client that retries 5xx must not retry it. A model that takes images but
  is genuinely failing stays a 502. (`/v1/route` reports failures as data in a 200, as always.)
- **A change in behavior.** Before this existed, an `images` field sent to `/v1/systemone` was silently
  ignored and the request was answered from the text alone. Now a model without `[limits]` refuses it,
  deliberately: an answer that quietly ignored the picture would be wrong without saying so. A gateway
  used as another gateway's upstream needs `max_images` set on that model before it will forward them.
- **Splitting** is by sorted question id, so the same request splits the same way every time and the
  cache sees the same pieces. The pieces run together; the result is all or nothing (the first failing
  piece's error), with answers merged, token usage and cost summed. A piece carries the state and the
  images.
- The wrapper that enforces limits is the **outermost** decorator on a model (limits, then cache,
  breaker, pricing, confidence policy, the provider), so a split request reaches the cache and the
  breaker as the smaller calls they already handle.
- `context_tokens` is **not implemented**: enforcing a context window needs the model's tokenizer.

**Where images go, and where they do not.** To the model or models the plan calls, and, in a shadow
audit (13.7), to the next tier. Never into the store, even for a client with `store_content` (its
stored state, questions and answers do not include them), into an event, a log line, `/v1/stats` or a
span or metric (tests check each, including the library's spans through a cascade, a fan-out and
a shadow audit). The response cache keys on a digest of the images, so the same text
with a different picture is a different question, and no image bytes are retained by it. What is kept
is **how many** there were: `requests.image_count` (a numbered migration), `images` in each
`request.completed` event, and `images` on every export row (13.5). The export holds no pictures, so a
row whose question was about "the attached image" has an answer and no input a trainer can see; the
count is how a trainer knows to skip it.

**Clef** is Cloudflare's open-weight decision model (Apache-2.0; `clef` 27B and `clef-flash` 9B,
released 2026-10-01), a System One model like Jev that also reads images, up to 4 per request and
up to 64 questions. Two ways to reach it:

- **Self-hosted through Ollama** (0.35.1 or later). Ollama serves `POST /v1/systemone` itself, with
  `model = "clef"` or `"clef-flash"`, so it is an ordinary `provider = "systemone"` entry with the
  limits above. No other server is needed.
- **Hosted on Cloudflare Workers AI**, `provider = "workers-ai"`:

  ```toml
  [models.clef]
  provider = "workers-ai"
  model = "@cf/cloudflare/clef"            # or "@cf/cloudflare/clef-flash"
  account_id_env = "CLOUDFLARE_ACCOUNT_ID" # the account id is in the URL, so it is kept out of the file
  api_key_env = "CLOUDFLARE_API_TOKEN"     # an account API token with Workers AI permission
  # base_url = "https://api.cloudflare.com/client/v4"   (the default)
  ```

  It is not the `/v1/systemone` wire: the request goes to
  `{base_url}/accounts/{account}/ai/run/{model}` with `Authorization: Bearer {token}`, and the answers
  come in Cloudflare's REST envelope. What was **checked against Cloudflare** (2026-10-04): Clef's input
  and output schemas (`cf ai get-model-schema --model @cf/cloudflare/clef-flash`), which are the System One
  shapes; that `model` is a **required input field** (`"clef"` or `"clef-flash"`, sent as well as being in
  the URL); that **images must be data URLs** (`data:image/png;base64,...`) and a raw base64 string is
  refused with "image must be an embedded base64 data URI" (Ollama and the Jev wire take raw base64, so the
  provider converts, labeling each image by its bytes and refusing anything that is not a PNG, JPEG or
  WebP with kind `unsupported`); Clef's image limits (4 images, 4 MiB and 16 megapixels each, 8 MiB total
  decoded, 13 MiB for the request body; remote URLs are not accepted); and real calls through a Worker (text
  and images). **Not checked:** the REST envelope around the output. `cf ai run` prints the output without
  it, so decoding is tolerant (answers under `result` or at the top level; a missing type is taken from
  the question; a response with no answers is a `decode` failure). A 200 with `"success": false` is an
  `http` failure naming only Cloudflare's numeric error codes, and the account id is kept out of error text.
  `go test ./ignatius -run TestRealClefWorkersAI -v` with `IGNATIUS_REAL=1`, `CLOUDFLARE_ACCOUNT_ID` and
  `CLOUDFLARE_API_TOKEN` checks the envelope against the real REST API (`IGNATIUS_REAL_IMAGE=path.png` also
  sends an image).
- **Through a Worker** (`deploy/clef-worker`): a small Cloudflare Worker that serves the Jev-compatible
  `POST /v1/systemone` (and `GET /readyz`) in front of the Workers AI binding, so Ignatius, the official
  typesafe SDK or anything else uses hosted Clef as an ordinary `provider = "systemone"` model, with no
  special provider and no account API token in Ignatius, only the Worker's own key:

  ```toml
  [models.clef-flash]
  provider = "systemone"
  base_url = "https://ignatius-clef.<your-subdomain>.workers.dev"
  model = "clef-flash"                  # "clef" for the 27B; "jev-latest" or none gets the Worker's DEFAULT_MODEL
  api_key_env = "CLEF_WORKER_KEY"       # the Worker's WORKER_API_KEY secret
  confidence = "derived"
  [models.clef-flash.limits]
  max_questions = 64
  max_images = 4
  ```

  The Worker checks `Authorization: Bearer` against the `WORKER_API_KEY` secret (comma-separated to rotate;
  403 without a credential, 401 with a wrong one; **with no key set it refuses every request**), validates
  the request (at most 64 questions and 4 images, images must be PNG, JPEG or WebP), converts images to data
  URLs, calls `env.AI.run`, and returns `{model, answers, usage}`. An upstream failure is a 502 that echoes
  neither the request nor the error text; nothing from a request is logged (`DEBUG_ERRORS=1`, a dev-only
  variable, also logs the error message to the Worker's own log, never to a response). Its tests run
  with a fake AI binding (`npm test`, in CI); it has also been run end to end with `wrangler dev`
  (whose AI binding calls the real service) behind a real Ignatius with images, a cascade from
  `clef-flash` to `clef`, the derived confidence policy and shadow audits. **It has not been deployed**:
  see `deploy/clef-worker/README.md`.
- **What a real Clef returned** (2026-10-04, `clef-flash` and `clef`): answers in exactly the System One
  shape, probabilities to four decimals, a `confidence` on choice and score, `usage.output_tokens` always 0;
  a solid red image read as `red` (0.986) and a blue one as `blue` (0.978). Clef's `confidence` is **not**
  the documented Jev formula applied to its own probabilities: in two choice samples it was 0.944 against
  0.972 and 0.913 against 0.956, and in one score sample 0.343 against 0.606 (a small sample, and Cloudflare
  says only that it is "derived from the probabilities"). So for Clef `provider` and `derived` are different
  scales, and a threshold tuned on one is wrong on the other; the example sets `derived`.

`deploy/clef/ignatius.toml` is a working example: Jev for text, then `clef-flash` through Ollama,
then hosted Clef, in one cascade, so an image request skips Jev. A test loads every example under
`deploy/` in both SDKs. Clef's context window is 64K on Workers AI and 256K in the Ollama build, so
it is a per-deployment fact.

## 4. Modes

All modes take `Request` and return a `Routed` envelope:

```
Routed {
  mode:     "single"|"fan_out"|"cascade"
  answers:  { <id>: Answer }          # final merged answer per question (single, cascade)
  results:  [Result]                  # every successful provider call, in call order
  failures: [Failure]
  trace:    [Hop]                     # cascade only
  cost_usd?: float                    # sum of the results' cost_usd
}
Hop {
  tier: int, model: string,
  questions: [id], escalated: [id]      # id lists sorted ascending
  latency_ms: int
  error?: { kind, message }             # set when the tier failed
  detail?: [ { id, confidence: float|null, threshold?: float, escalated: bool } ]
}                                       # detail explains each decision: confidence vs the bar
                                        # applied; threshold is absent on the last tier
```

### 4.1 single
`route(single, model)`: one call. On failure, `answers` is empty and `failures`
holds the error.

### 4.2 fan_out
Call N aliases concurrently with the same request. Never fail fast: wait for
all (each bounded by its own timeout), return every `Result` in `results` (order
= the order aliases were given, not completion order) and every `Failure` in
`failures`. `answers` is empty (the caller compares). Options:
`min_success` (default 1): if fewer succeed, the call returns the envelope with
an overall error flag `ok=false`.

### 4.3 cascade
Ordered `tiers` (cheap to expensive). Each tier is `{model, threshold?}`; a bare
string `"jeff"` is shorthand for `{model: "jeff"}`. Confidence is calibrated per
model and per question type, so it is not comparable across tiers: a tier's
`threshold` is its own bar for "good enough to stop here".

`threshold` is a number (applies to every question type) or a map
`{noul?, choice?, score?}` (missing keys fall back to the cascade-level
`threshold`, which defaults to 0.8). The last tier's threshold is unused.

1. Ask tier 0 all questions.
2. A question is *settled at tier i* if its normalized confidence is >= tier i's
   threshold for that question's type.
3. Ask tier i+1 ONLY the unsettled questions (questions are independent, so
   subsets are valid), then repeat.
4. A tier that fails (any Failure) escalates all of its pending questions to
   the next tier; if it is the last tier, those questions take the best answer
   seen so far (highest confidence), or are absent if none.
5. After the last tier, any still-unsettled question takes the answer with the
   highest confidence across tiers. Ties go to the later tier. (Raw confidences
   are compared here; this is a heuristic, not a calibrated comparison.)
6. `answers[id].model` records which alias supplied the final answer.
7. `trace` has one `Hop` per tier actually called.

Optional `escalate_if` predicate (SDK-native callback) overrides step 2 for a
question when provided.

Thresholds should be tuned on labeled examples from your own traffic, not
guessed. v1 may add a `calibrate` command that sweeps thresholds against a
labeled set and reports accuracy/escalation-rate tradeoffs.

## 5. Conformance fixtures

`spec/fixtures/*.json`. Each fixture (its `mode` and `args` together are a `Plan`, section 6.0):

```
{ "name", "mode", "args": {...}, "request": Request,
  "mock": { "<alias>": { "latency_ms"?, "error"?: {...}, "answers": {...} } },
  "expect": { ... subset match against Routed ... } }
```

A mock may also carry `usage` (the tokens it reports) and `price`
(`{input_per_mtok, output_per_mtok}`); a priced mock goes through the real pricing
decorator, so `cost_usd` is covered (`cost_accounting.json`). Breaker and cache
behavior depend on time and call history, so they are covered by unit tests in each
SDK instead, written to the same cases.

The harness replaces provider calls with the `mock` table, runs the mode, and
checks that `expect` is a subset of the produced `Routed` (floats to 1e-9).
Mock `answers` use the raw wire shape (pre-normalization), so fixtures also
cover normalization (1.1).

## 6. Layers and the native API

| Layer | Surface | Notes |
|---|---|---|
| L0 dunce-union | `/v1/systemone`, dispatch by `model` | self-hosted models behind one door |
| L1 compat | `/v1/systemone` on Ignatius, mode in the `model` string | for the official SDK; lossy (6.1) |
| **L2 native** | `POST /v1/route` with an explicit `Plan` | the real API; the SDKs wrap it |

There is no server-side task layer. **Callers own the questions, state shaping
and policy** (what "triage a ticket" means); Ignatius only executes a `Plan`
over a `Request`. Every L1 route compiles to a `Plan`, so there is one internal
representation and one set of fixtures.

### 6.0 Plan

```
Plan {
  mode:        "single"|"fan_out"|"cascade"
  model?:      alias                    # single
  models?:     [alias]                  # fan_out
  tiers?:      [Tier|alias]             # cascade; Tier = { model, threshold? } (4.3)
  threshold?:  number|{noul?,choice?,score?}   # cascade default, 0.8
  reduce?:     "none"|"vote"|"mean"|"most_confident"   # fan_out, default "none"
  min_success?: int                     # fan_out, default 1
  timeout_ms?: int                      # overall budget across the whole plan
}
```

Reducers (fan_out, applied per question over the successful results):
- `none`: `answers` stays empty; the caller compares `results`.
- `vote`: choice = plurality choice (ties: larger sum of that choice's
  probability), with `probabilities` = vote shares; noul and score = mean.
- `mean`: choice = argmax of mean probabilities; noul and score = mean.
- `most_confident`: take the answer with the highest normalized confidence.
A reduced `Answer` has `model: "ensemble"` (`most_confident` keeps the winner's
alias) and `contributors: [alias]`; its `probabilities` and `confidence` are
recomputed (1.1) from the reduced distribution. A question with no successful
contributors is absent from `answers`.

### 6.0.1 Endpoint

`POST /v1/route`: body `{ "request": Request, "plan": Plan }` or
`{ "request": Request, "route": "<named route>" }` (exactly one of `plan` and
`route`). Response `200` with the `Routed` envelope (section 4) plus `ok` and
`request_id`; provider failures are data inside it, not HTTP errors. HTTP errors
are only for the caller's mistakes: `400` malformed JSON, `403`/`401` auth,
`413` too large, `422` invalid plan (unknown alias, bad reducer, cascade with no
tiers; the body lists what was wrong). `GET /v1/routes` lists named routes,
`GET /v1/models` lists aliases with readiness, `GET /healthz`, `GET /readyz`.
The Go and Python SDKs are typed clients of this endpoint, and can also run a
`Plan` in-process with no gateway.

### 6.1 The official `typesafe-sdk` as the client interface

Because the gateway speaks `/v1/systemone`, a client points `base_url` at
Ignatius and keeps using `TypeSafeClient.system_one(...)`. The SDK has no knob
for modes, so **the mode is chosen by the `model` name**, resolved against named
*routes* in the gateway config:

```toml
default_route = "fast_then_smart"  # used for model="jev-latest" or no model

[routes.fast_then_smart]           # model="fast_then_smart"
mode = "cascade"
tiers = [ { model = "jeff", threshold = { noul = 0.5, choice = 0.9 } }, "clef", "jev" ]

[routes.consensus]                 # model="consensus"
mode = "fan_out"
models = ["jev", "clef", "jeff"]
reduce = "vote"                    # vote | mean | most_confident
```

```python
client = TypeSafeClient(api_key="local", base_url="http://ignatius:8081")
client.system_one(state=..., questions=..., model="fast_then_smart")
```

**Inline routes.** A `model` string may also carry an ad hoc route, with no
config needed. Separators `: , > @ |` never appear in aliases, so parsing is
unambiguous (hyphenated aliases like `jeff-gemma` are why `fan-out-jeff-jev`
is not the syntax):

```
jeff                            single (plain alias)
fan-out:jeff,jev,clef           fan_out, reducer defaults to vote
fan-out:jeff,jev|mean           explicit reducer: vote | mean | most_confident
cascade:jeff>clef>jev           cascade with the route default threshold
cascade:jeff@0.5>clef@0.7>jev   per-tier threshold (one number, all question types)
```

`fan-out` and `fan_out` are both accepted. Per-type thresholds and anything else
richer need a named route. Bad grammar or an unknown alias is HTTP 422 listing
the grammar and the available models. Fan-out width and cascade depth are
capped (`max_inline_models`, default 5). `allow_inline_routes` (default true)
turns the feature off. Resolution order for a `model` string: named route, then
profile (12), then alias, then inline grammar.

What fits through the SDK and what does not:

- **single** and **cascade** return one answer per question, so they fit the
  standard response unchanged. Provenance (which model answered, the trace, any
  failures) goes in an extra `ignatius` object in the response, like Decis's
  `decis`. The SDK ignores unknown fields, so it is visible only to raw HTTP
  clients or the Ignatius SDK, never lost.
- **fan_out** returns N answers per question, which the SDK's response types
  cannot hold. Through the SDK it must use a `reduce` (vote for choice, mean for
  noul/score, or most_confident) so it returns one answer. Raw fan-out (every
  model's result) is `POST /v1/route` or the Ignatius SDK.
- **Per-request knobs** (thresholds, tier lists) cannot travel through the SDK
  because it sends only `state`, `questions` and `model`. Put them in the named
  route; the Ignatius SDK and `/v1/route` accept them per call.
- Anything else the SDK drops (images, a second key) is out of reach through it
  by the same rule.
- Verified (typesafe-sdk, unchanged, against a local Ignatius with real Jev
  behind it): `system_one(model=...)` accepts route names, the default
  (`jev-latest`) resolves to `default_route`, and the extra `ignatius` response
  object is ignored without error.

## 7. Server (`ignatius serve`)

One Go binary serves both layers: Jev-compatible `POST /v1/systemone` (L1) and
native `POST /v1/route` (L2), plus `GET /v1/models` (alias + readiness status:
`ready`, `not_ready`, `unknown`, `unsupported`), `GET /v1/routes`, `GET /v1/profiles` (and, for admins when enabled, `PUT`/`DELETE`
on `/v1/profiles/{name}` and `/v1/routes/{name}`, section 12), `GET /v1/stats` (per-model counters, p50/p95 latency and the
last 50 requests as metadata only: never state, questions or answers),
`GET /healthz` and `GET /readyz` (200 unless every model is known `not_ready`; counts only, no names).

- **Status page:** `GET /` serves one static, dependency-free HTML page (strict
  CSP, nothing external). It holds no data; it asks for a client key, keeps it in
  `sessionStorage`, and polls `/v1/stats` and `/v1/routes`. Counters are in
  memory and reset on restart. Each recent request expands to its calls: for a
  cascade, per tier the latency and, per question, confidence against the
  threshold applied and whether it escalated; for a fan-out, each model's
  latency and confidences plus how far the models agreed (distinct choices, or
  spread for noul/score). Answer values are never stored, only structure,
  timings, confidences and errors.
- **Auth:** a client Bearer key from `api_key_env` (comma-separated to rotate).
  Missing credential is 403, wrong token 401, like Decis and Jev; auth runs before
  body validation. With no key configured it refuses to listen on a non-loopback
  address unless `IGNATIUS_ALLOW_NO_AUTH=1`.
- **Providers need only a URL and auth.** A model entry is
  `{provider, base_url, model, api_key_env?, auth_header?, timeout_ms?}`. Bearer
  is the default; `auth_header` sends the raw key in a custom header instead.
  Anything that speaks `/v1/systemone` works: hosted Jev or Clef, a self-hosted
  Decis, another Ignatius.
- **Model resolution** (`model` in L1, `route` in L2): named route, then profile (12),
  then alias (single), then inline grammar (6.1). `model` empty or `jev-latest` means
  `default_route`.
- **L1 contract details.** The response is the standard `{model, answers, usage}`
  plus an `ignatius` object (`request_id`, `mode`, `sources`, `trace`,
  `failures`). Noul answers carry no `confidence`; score answers keep `legend`.
  Because the Jev contract needs an answer for every question, a routing result
  that is not `ok` is HTTP 502 `routing_failed` with the failures and trace.
  Fan-out needs a reducer here (422 `reducer_required` otherwise).
- **`ok`** is defined for every mode: single = the call succeeded; cascade =
  every question has an answer; fan_out = at least `min_success` succeeded. A
  tier that succeeds but omits a question escalates that question.
- Startup fails on a named route or `default_route` that references an unknown
  model.

## 8. dunce-union: the optional self-hosting kit

Self-hosting is a feature alongside the core, not part of it. Ignatius needs
only a URL and auth per model, so anything you already run works. dunce-union is
the kit for running the open-weight models we support (Jeff, Laya, Kev) yourself:
unmodified [Decis](https://github.com/chaitin/Decis) images, one service per
model, behind an Ignatius gateway. It is deployment config, not a binary.

- `deploy/dunce-union/docker-compose.yml`: a gateway service plus one service per
  model, selected by compose profile. Only the gateway publishes a port. Two keys:
  `IGNATIUS_API_KEY` (clients to gateway) and `DUNCE_API_KEY` (gateway to models).
- `deploy/dunce-union/ignatius.toml`: the matching gateway config.
- Helm chart (planned, `deploy/helm/ignatius`): the gateway Deployment, and an
  optional Deployment and Service per self-hosted model (image, resources, GPU
  toggle), each auto-registered as a gateway model. Backends stay swappable per
  model (Decis images, Ray Serve, KServe), because only the URL matters.
- Decis facts the orchestrator relies on:
  - Bearer auth is mandatory (missing credential = 403, bad token = 401).
  - `model: jev-latest` is answered by whichever engine the container runs, so
    aliases use it instead of a version string.
  - `state` is text only (string, object or array); no images.
  - `confidence` on the wire is a *statistic Decis computes*, not a model output.
    Decis defines one formula for all its engines (choice:
    `(p_max - 1/K)/(1 - 1/K)`; score: `1 - H(p)/ln L`; noul has none) and keeps
    the engine's own value in `decis.native_confidence`. Jev's formula is
    undisclosed and Laya and Kev each use a different one. Same formula does not
    mean same reliability: Decis's docs say to recalibrate on your own labeled
    set and not to reuse thresholds across engines. So cascade thresholds stay
    per tier.
  - Images are large (4-18 GB compressed) and hold weights, so pull deliberately.
  - It serves your own weights: `decis serve --engine kev-0.8b --model-path kev-0.8b=/srv/finetunes/acme`
    (or `DECIS_MODEL_DIR/<engine-id>/`) loads a fine-tune of a registered engine behind the same API.
    So the loop in 13 closes without writing a server: export (13.5), train outside Ignatius, serve
    the result with Decis, register its URL as another alias. The checkpoint must be a complete one
    for that engine (Laya and Jeff carry their option limits inside; a Jeff re-export must bring the
    tokenizer vocabulary it was trained with).
  - Engines today (its docs, 2026-10): `laya-multilingual` (322M, 647 MiB, CPU is fine: about 100 to
    300 ms per question on 24 vCPUs), `laya` (421M), `kev-0.8b` (Qwen3.5-0.8B with LoRA, wants a GPU),
    `jeff-qwen3.5-0.8b` and `jeff-gemma4-e2b` (4.6B, 8.7 GiB). Apache-2.0; the project is days old (created
    2026-09-22), with no GPU or Apple-silicon benchmarks of its own, so treat it as young.
- Writing our own inference server is not planned. If it ever is, the first step
  is a spike: run Jeff through llama.cpp or ONNX and compare label-token
  probabilities against the upstream implementation's reference outputs.

## 9. Repository layout

```
SPEC.md  spec/fixtures/          # contract + conformance
go/
  ignatius/                      # library: types, Plan execution, reducers, backends
  gateway/                       # HTTP server: L1 + L2, routes, auth, feedback, retention (13)
  store/                         # SQLite and Postgres behind one Store interface (13.1)
  eval/                          # run a labeled set through routes and judge it (15)
  events/                        # event emitter and the sink registry; the file sink (14)
    sinks/webhook/               #   the webhook sink, a plugin in this module
  standard/                      # blank-imports the in-module plugins a stock build wants (14.5)
  cli/                           # the ignatius command as a library: serve, run, purge, export, calibrate
  cmd/ignatius/                  # the stock build: cli.Main plus standard
  plugins/                       # sink plugins, each its own Go module so the core never links their SDKs
    kinesis/                     #   Amazon Kinesis (AWS SDK); cmd/ignatius builds the command with it
    kafka/                       #   Kafka, Redpanda, MSK (franz-go); test-msk-floci.sh runs the MSK test
python/                          # router SDK (package: ignatius): in-process Ignatius + GatewayClient;
                                 # passes the same fixtures as Go, plus an interop test against the Go gateway
deploy/dunce-union/              # optional self-hosting kit (compose now, Helm later)
```

## 10. Roadmap

Status as of this revision. Items under *Next* are in rough priority order.

**Done:** spec and 11 shared fixtures; Go orchestrator and gateway (L1 + L2, named
and inline routes); TOML config; status page with per-request drill-down; performance
tests (direct vs through Ignatius); Python SDK with a Go-gateway interop test; the
dunce-union compose kit (**run end to end** with a real Laya container); the
circuit breaker, opt-in response cache, cost accounting, and per-client keys with
rate limits, route allowlists and admin-only stats (section 11); OpenTelemetry
traces, metrics and logs with a Prometheus endpoint (10.1); profiles (fast, best...) with an
opt-in editor on the status page (section 12); the store, feedback, calibrate, export and
retention (13), event sinks and compile-time plugins (14), shadow auditing (13.7), the confidence
policy (1.2), CI, images with per-model limits and Clef (3.1), and evaluation of routes on a labeled set (15).

**Next**, in rough priority order:

1. **Publish images:** CI builds them (nothing is pushed); publishing needs a registry decision.
2. **Real-data validation:** run the loop (cascade, shadow audit, feedback, `calibrate`,
   export) against real models, not fakes and emulators. `TestRealJevFeedbackLoop` does it
   against Jev (`IGNATIUS_REAL=1 TYPESAFE_API_KEY=...`); it has been run only against a
   Jev-shaped local server so far. The real test for the confidence policy is a Laya-versus-Jev
   run on the churn-risk ticket (Laya: 0.01, Jev: 0.98, both "confident").
3. **Helm chart** (`deploy/helm/ignatius`, see section 8).
4. **Retries and a `race` mode** (first confident answer wins) for tail latency.
5. **More providers:** an `openai`-style adapter (structured output; fan-out only,
   since it has no calibrated confidence). The Workers AI provider for hosted Clef exists (3.1);
   only the REST envelope around Clef's output is unchecked against Cloudflare.
6. **Per-client budgets** (spend caps per day) on top of 11.3 and 11.4.
7. **Wider performance coverage:** end-to-end cascade and fan-out latency, concurrency
    and throughput.
8. **Observability follow-ups:** alerting rules and a Grafana dashboard for the metrics
    below; OTLP *log* export (logs are structured stdout today); gRPC OTLP; an optional
    pprof endpoint behind an env toggle.

**Open decisions:** the confidence policy above; whether to store answer values
in stats behind an opt-in; whether Pkl is worth adding as an authoring layer
(rendering to the same TOML, no runtime dependency).

### 10.1 Observability (OpenTelemetry) — implemented

Goal: see what Ignatius did with each request, and why a cascade escalated, in tooling
you already run. The status page stays as the zero-setup local view.

**Where it lives.**
- `go/ignatius` and `go/gateway` emit through the OpenTelemetry **API** only, via the
  global providers. With no provider installed they are no-ops; an embedding app
  controls export by installing its own.
- `go/telemetry` is the only package that imports the **SDK**. `ignatius serve` calls it
  when `[telemetry]` is configured. Nothing is installed otherwise.
- **Cost when off is small, not zero (measured).** `Run` with an instant backend, no
  provider installed, vs before instrumentation: single +0.4 us, cascade +1.2 us,
  fan-out +2.7 us (about 17-39 extra allocations). With a real SDK recording every span:
  roughly 2.6 / 9.5 / 13.4 us in total per run. Against model calls of ~150 ms this is
  noise, but it is not free. `go test ./ignatius -bench Run -benchmem`, and
  `IGNATIUS_BENCH_SDK=1` for the SDK case.
- **Binary and image grew:** the stripped binary is ~15.6 MB and the container image
  went from 16 MB to 29 MB. The Go toolchain requirement is now 1.26 (the current
  OpenTelemetry requires it).

**Traces.** A server span `ignatius.request` per request (L1 or L2) that continues an
incoming W3C `traceparent`; under it `ignatius.run`, then for a cascade one
`ignatius.cascade.tier` per tier called, then `ignatius.model.call` (client span) per
backend call. Fan-out calls are concurrent siblings under `ignatius.run`. A cascade tier
span carries one `ignatius.decision` event per question: `ignatius.question.id`,
`ignatius.confidence`, `ignatius.threshold` (absent on the last tier) and
`ignatius.escalated`. Other attributes use the `ignatius.*` namespace
(`ignatius.mode`, `.route`, `.client`, `.ok`, `.request_id`, `.usage.input_tokens`,
`.usage.output_tokens`, `.cached`, `.cost_usd`, `.error.kind`). The OpenTelemetry GenAI
semantic conventions are **not** adopted: they have moved to a separate repository and I
could not verify current names, and these models do not generate text. Revisit if they
stabilize. Question ids are the caller's schema names (e.g. `churn_risk`), not content,
and appear only in span events, never in metrics.

**Metrics** (OTel names; the Prometheus form adds the unit and `_total`):

| Metric | Kind | Labels |
|---|---|---|
| `ignatius.requests` (`ignatius_requests_total`) | counter | layer, route, mode, ok, client, status_class |
| `ignatius.request.duration` (`..._seconds`) | histogram | layer, route, mode |
| `ignatius.model.call.duration` (`..._seconds`) | histogram | model, ok |
| `ignatius.model.failures` | counter | model, kind |
| `ignatius.answer.confidence` | histogram | model, question_type |
| `ignatius.cascade.escalations` | counter | route, tier, model |
| `ignatius.fanout.disagreement` | counter | route, question_type |
| `ignatius.cache.questions` | counter | model |
| `ignatius.cost` (`ignatius_cost_usd_total`) | counter | model |
| `ignatius.rate_limited` | counter | client |

Every label is **bounded**: model aliases, client names and route names come from config,
and `route` is a configured route or alias name, or the literals `inline`, `plan` or
`unknown`. A client-supplied model string is never a label. `GET /metrics` serves
Prometheus text, requires an **admin** client, and is absent unless `prometheus = true`.
Per-series `otel_scope_*` labels are turned off.

**Logs.** One structured JSON line per request on stderr (`log/slog`): `request_id`,
`trace_id` (so a line leads to its trace, present even with export off when the caller
sent a `traceparent`), `layer`, `client`, `route`, `mode`, `ok`, `status`, `duration_ms`,
`models`, `failure_kinds`. Level INFO, or WARN for a 5xx or a request that did not
succeed. OTLP log export is not implemented.

**Privacy rule, enforced by tests.** Spans, metrics and logs carry structure, timings,
config-defined names, counts, confidences, thresholds and error **kinds**. Never `state`,
question text, criteria, answer values, or error **messages**: an upstream error body can
echo the request, so a failed call's span records its kind (`http`, `timeout`, ...) and
nothing else, and the status page stores `HTTP 503`, not the body. A test sends a sentinel
string through state, instructions, criteria, answer labels and an upstream error body,
then asserts it appears in no span, metric, log line, OTLP payload or stats response.
Each leak path was checked by re-introducing it and confirming a test fails.

**Propagation.** An incoming `traceparent` is continued **only from an authenticated
caller**. An unauthenticated or rejected request gets a fresh root trace. Otherwise anyone
could send `traceparent: ...-01` to force every request to be sampled whatever
`traces_sample_ratio` says, or plant a trace id of their choosing in the gateway's log
lines. An authenticated caller can still force a trace to be sampled; that is accepted,
since the ratio is cost control against the public, not against your own clients. (With no
keys configured every caller is the anonymous client, so the trace is continued.) Outgoing
is **opt-in per model**
(`propagate_trace = true`), because forwarding internal trace ids to a third-party hosted
API is a leak; turn it on for models you run yourself. Only trace context is forwarded,
never baggage.

**Config** (`[telemetry]`, TOML; secrets by env var name like everything else):

```toml
[telemetry]
service_name = "ignatius"                      # default "ignatius"
otlp_endpoint = "http://otel-collector:4318"   # base URL; /v1/traces and /v1/metrics are appended
otlp_protocol = "http/protobuf"                # the only supported value; gRPC is not implemented
otlp_headers_env = "OTLP_HEADERS"              # env var holding "k=v,k2=v2" (e.g. an auth token)
traces_sample_ratio = 1.0                      # new traces kept; a sampled parent is always kept
metrics_interval_ms = 10000                    # OTLP push interval
prometheus = true                              # serve GET /metrics
```

Off unless `otlp_endpoint` is set or `prometheus = true`. **Environment variables
(tested):** the endpoint and protocol come only from this table, so
`OTEL_EXPORTER_OTLP_ENDPOINT` is ignored. Exporter settings the table leaves unset still
come from the standard variables (for example request headers from
`OTEL_EXPORTER_OTLP_HEADERS`, which `otlp_headers_env` overrides), and
`OTEL_RESOURCE_ATTRIBUTES` adds resource attributes. Sampling is parent-based. gRPC is
not implemented; that is a scope choice, not a size one (the gRPC packages are already
linked in through the OTLP protobuf dependencies).

**Python.** The SDK contains no OpenTelemetry code, by design. The gateway does the
work; the client is thin. To connect an app's trace to the gateway's, instrument `httpx`
(`opentelemetry-instrumentation-httpx`), which `GatewayClient` uses, and the gateway
continues the trace. Verified against the real gateway: the app's trace id appears in the
gateway's log line. An in-process Python SDK user who wants spans around calls can wrap
them with the OpenTelemetry API directly.

## 11. Resilience, cost and access control

Breaker, cache and pricing are **backend decorators** in the library, so every
SDK and the gateway get them, and `Run` and the fixtures are unaffected. Order,
outermost first: cache, breaker, pricing, then the real backend. Per-client keys,
rate limits and route allowlists are **gateway-only**.

### 11.1 Circuit breaker (per model)

On by default (`failure_threshold = 5`, `cooldown_ms = 30000`). Without it, a
cascade whose cheap tier is down pays that tier's full timeout on every request.

- **States:** closed, open, half_open. `failure_threshold` consecutive
  *countable* failures open it. While open, calls fail immediately with kind
  `circuit_open` (no network). After `cooldown_ms` it admits exactly one probe
  call (half_open); other calls are still rejected while the probe is in flight.
  A successful probe closes it; a failed probe re-opens it and restarts the
  cooldown.
- **Countable failures:** `timeout`, `transport`, `decode`, and `http` with
  status 5xx or 429. Other 4xx responses mean the server is alive, so they reset
  the failure count instead of adding to it. A call cancelled by its caller is
  ignored. Any success resets the count.
- **Known divergence:** a plan-level `timeout_ms` expiry counts as a timeout for the
  breaker in Go but is ignored in Python, because Python cancels the in-flight call
  and cannot tell that from a caller cancelling. Per-model `timeout_ms` counts in
  both.
- **Visibility:** `GET /v1/models` reports `circuit_open`; the status page shows
  it; the cascade trace shows the skipped tier with kind `circuit_open` and
  ~0 ms latency.

### 11.2 Response cache (opt-in)

Off by default. When enabled, answers are cached **per question**, not per
request: System One questions are independent, so a cascade's escalated subset
and a repeated request both hit.

- **Key:** sha256 of the model alias, its upstream model, the `state`, and the
  question's `type`, `instructions` and `criteria` (not its id). Canonical JSON.
- **Behavior:** cached questions are served without a call; the rest go to the
  backend as a smaller request and are merged. A request fully served from cache
  makes no call (`latency_ms` ~0, `cost_usd` 0, `cached` = number of questions).
  Only successful answers are cached. Entries expire by TTL and are evicted
  LRU at `max_entries`. In memory, per process.
- **Privacy:** keys are hashes, so `state` is not retained, but the cached
  answers are about the caller's content and live in memory until they expire.
  That is why it is opt-in. It is not shared across processes.

### 11.3 Cost accounting

`price_input_per_mtok` and `price_output_per_mtok` (USD per million tokens, from
the model's `usage`) give each Result a `cost_usd`; `Routed.cost_usd` is the sum.
No price means no `cost_usd`. The status page shows spend per model and a
cascade estimate: for each cascade request, the actual cost against what the
first tier's token counts would have cost at the final tier's prices. It is an
**estimate** (different models tokenize differently), it can be negative (when a
cascade escalated everything it cost more than going straight to the strong
model), and it is labelled as such.

### 11.4 Clients, rate limits and route allowlists (gateway)

- `api_key_env` keys remain the implicit client `default` (admin, unlimited).
  `[[clients]]` entries add named clients; each has its own key from `key_env`.
  A key used by two clients is a startup error. Keys never appear in any
  response, log or stat.
- **Rate limit:** a token bucket per client (`rate_limit_per_minute`, `burst`) on
  `/v1/systemone` and `/v1/route`. Over the limit is HTTP 429 `rate_limited` with
  `Retry-After` (whole seconds). Dashboard polling is not counted.
- **Allowlist:** if `routes` is set, the client may use only what it lists.
  An entry is a **named route** (usable as that route only), a **profile** or an
  **alias** (each usable alone, and as a member of an inline route). Entries are matched
  by the name the request used: allowing the profile `fast` does not allow the model
  behind it, though the client can see which model that is. A profile
  entry follows the profile, so retargeting it changes what that client can reach, which is
  the point of a profile. `"inline"` permits inline
  routes, but **every alias inside an inline route must itself be on the list**, so
  `"inline"` can never reach a model the client was not given. An explicit L2
  `plan` is refused (403 `route_not_allowed`), since it could name anything.
  Entries must name a real route, alias or `"inline"`, or startup fails.
- **No enumeration:** a restricted client gets the same 403 `route_not_allowed`
  for a forbidden name and a nonexistent one, and error bodies list no names.
  `GET /v1/models` and `GET /v1/routes` show a restricted client only the
  aliases and routes it may use (and the default only if allowed).
  `GET /readyz` is unauthenticated, so it reports counts only
  (`{"ready": n, "total": m}`), never model names.
- **Admin:** `/v1/stats` (and so the status page's data) needs an admin client,
  because it shows other clients' traffic metadata. `/v1/models` and
  `/v1/routes` are open to any valid client. A non-admin key gets 403
  `admin_required` on stats.
- Recent requests and counters record the client **name**, never the key.

### 11.5 Self-service API keys (opt-in)

Any holder of a valid key can mint more keys for themselves or others, so access is shared
by handing someone a key, with no config edit and no restart. Reaching the gateway is not
enough: the first key is always the operator's (`api_key_env` or a `[[clients]]` entry),
which is the root of trust.

```toml
[admin]
self_service_keys = true
state_file = "/var/lib/ignatius/state.json"   # required with self_service_keys
max_keys = 1000                                # total runtime keys; default 1000
max_keys_per_principal = 20                    # keys one principal may have created; default 20
```

- **Principals.** The tree is made of **principals**, not of key values: a configured client
  (or the implicit `default`), a `[[users]]` entry (11.6), or a runtime key. A runtime key's
  parent is the principal that minted it, recorded **by name**. Rotating a configured key
  therefore never orphans its keys, and a login session (11.6) is only a credential of its
  user, so a key a user mints belongs to the user and does not die when the session does.
- **Startup:** `self_service_keys` needs at least one configured key or user and a `state_file`
  (keys must survive a restart, and a gateway with no keys is the anonymous development
  mode). Either missing is a startup error.
- **Create:** `POST /v1/keys` with `{"name": "...", "rate_limit_per_minute": N, "burst": N,
  "routes": [...], "admin": false, "expires_in_s": N}`; every field but `name` is optional.
  The response is `{"id": "...", "name": "...", "key": "ig_..."}` and is the **only** time the
  key is shown. Names follow the profile-name rule and must not equal a configured client or
  user name (and a client or user name must not equal a runtime key's full name).
- **A key can never exceed its creator.** The child's `routes` must be a subset of the
  creator's (omitted means the creator's own list, so a restricted key cannot mint an
  unrestricted one); `admin` is allowed only if the creator is admin; a child's
  `rate_limit_per_minute` and `burst` cannot be higher than the creator's, and an omitted
  value inherits it; `expires_in_s` cannot outlive the creator's principal (a configured
  client or user never expires; a runtime key's own expiry applies). Violations are 422
  `exceeds_creator` naming the field.
- **Limits are shared up the tree, not multiplied.** A request is charged to the key's own
  bucket **and** to every ancestor principal's, so minting a hundred keys never raises a
  holder's total rate. The charge is **all or nothing**: every bucket in the chain is checked
  before any is spent (locked in a fixed order, root first), so a 429 at one level never
  burns tokens at another. Allowlists apply independently at each level.
- **Revoke and list:** `DELETE /v1/keys/{id}` revokes a key and **everything it created**, so
  one call shuts off a leaked key and anything it spawned. `GET /v1/keys` lists the caller's
  own descendants (id, name, creator, created, expires, last used, limits), never a key. An
  admin sees and revokes all runtime keys. A key cannot revoke its ancestors. Keys from the
  config are not runtime keys: they have no id, and `POST /v1/logout` with one is 409
  `not_removable`. A key that is not the caller's to revoke and one that does not exist both
  answer 404. `POST /v1/logout` ends the calling session, or revokes the calling runtime
  key and its descendants (the only way a key revokes itself).
- **Storage:** only the SHA-256 of a key is stored, in `<state_file>.keys` beside the state file
  (kept apart because the edit path rewrites the state file wholesale; mode 0600, written by
  atomic rename before the key is returned, as in 12.1). Keys are 256 random bits, so an
  unsalted hash is sufficient. Lookup hashes the presented token and compares it against
  every stored hash in constant time, as 11.4 does for static keys.
- **Restart and a changed config.** Keys are replayed on top of the config as in 12.1. A key
  whose creating principal no longer exists, or that would now exceed it (tighter routes or
  limits, no longer admin), is **skipped and reported as stale**, never fatal, and its
  descendants with it. Expired keys are dropped on load and on every key operation (create, list), and never authenticate.
- **Names and metrics:** a runtime key is recorded as `<principal>/<name>` in the audit log,
  but metric labels, the per-client counters and the status page's recent requests use only
  the **root** configured client or user, so arbitrary names cannot blow up label cardinality.
- **Audit:** `key_created` and `key_revoked` log lines carry `actor` (the creating principal),
  `id`, `name`, and the limits granted; never the key. Minting is counted against the
  creator's rate limit like any other request, so it cannot be used to spin.
- **Several replicas:** like edits (12.1), each replica keeps its own key store. A key made
  on one is unknown to the others. Use one replica for key minting, or configure keys.

### 11.6 Users and login (opt-in)

Password sign-in for the status page, for people who should not paste a key. It is the
simple-auth tier: users live in the config.

```toml
[admin]
login_ttl_s = 28800            # session lifetime; default 8 hours
max_sessions_per_user = 10     # the oldest is dropped past this; default 10
trust_proxy = false            # honour X-Forwarded-For/-Proto from a proxy you run

[[users]]
name = "sam"
password_hash = "$2a$12$..."   # bcrypt; a plaintext password is a startup error
admin = true
routes = ["fast", "best"]      # optional, as for a client
rate_limit_per_minute = 600    # optional, as for a client
```

- **Discovery:** `GET /v1/login` is 200 `{"password_login": true}` when users are configured
  and 404 otherwise, so the status page knows whether to show a form. It reveals no names.
- **Login:** `POST /v1/login` with `{"username": "...", "password": "..."}` returns a
  short-lived **session key** for that user: `{"key": "ig_...", "expires_in_s": N,
  "user": "sam", "admin": true}`. The status page stores it in `sessionStorage` and sends it
  as `Authorization: Bearer`, so the API keeps its no-cookie, no-CSRF property. Basic auth
  through the browser's own prompt is deliberately not supported: the browser would cache the
  credentials and attach them to cross-site requests.
- **A session is a credential, not a tree node.** It carries the user's limits and allowlist
  and is held **in memory only**, so a restart signs everyone out and nothing is written to
  the state file. It does not count toward `max_keys` or `max_keys_per_principal`; past
  `max_sessions_per_user` the oldest is dropped. Keys a signed-in user mints under 11.5
  belong to the user and outlive the session. `POST /v1/logout` ends a session.
- **Guessing:** the throttle runs **before** any password hashing, because bcrypt is CPU an
  attacker could otherwise spend for us. Failures are throttled per source address and per
  username with a short backoff. The response is identical for an unknown user and a wrong
  password (401 `invalid_credentials`), and an unknown user is compared against a dummy hash
  so timing does not tell them apart; the dummy is made at the highest cost any user's hash
  has. Passwords over 72 bytes are rejected (bcrypt ignores the rest). A name that is not a
  valid user name is treated as an unknown user: it is never a throttle key or logged. A
  success clears that user's counter only, never the address's, so one good account cannot
  be used to reset guessing at others. Successes and failures are logged (`login_ok`, `login_failed`, user and
  address, never the password).
- **Source address:** the socket address, unless `trust_proxy = true`, when the right-most
  `X-Forwarded-For` entry is used. Behind a proxy without that setting every login shares
  the proxy's address, which the startup log warns about.
- **Requirements:** sessions need neither `self_service_keys` nor a `state_file`; only the keys
  a user mints do. Users alone (no clients) are enough to require authentication: the gateway
  never falls back to anonymous when any user is configured. Passwords belong over TLS (terminate it in front of the
  gateway); the gateway warns at startup if users are configured on a non-loopback address
  without `trust_proxy = true`. A user name must not equal a client name or `default`.
- **Hashes:** `ignatius hash-password` reads a password from stdin and prints a bcrypt hash.
- **Not here:** groups, password reset, MFA and SSO. A user is a config entry, and changing
  one means editing the config.

## 12. Profiles

A **profile** names an intent (`fast`, `best`, `cheap`) and points at one model alias, so
callers and routes name the intent and an operator decides which model serves it. Swapping
what `best` means is one line, with no change to any caller or route.

```toml
[profiles]
fast = "jeff"
best = "jev"
```

**Naming convention.** An alias names the *concrete model* (`jeff`, `jev`, `laya`); a
profile names the *intent* (`fast`, `best`, `cheap`). Avoid naming an alias after a quality
such as `cheap` or `smart`: that is a profile's job, and it makes the status page ambiguous.
The status page's Models table lists aliases (stats, the breaker, the cache and cost are per
backend, and two profiles can share one alias), and shows for each the upstream model it
asks for, the host it calls (`host:port` only, never the path or credentials) and the
profiles currently pointing at it.

**Where a profile can appear.** Anywhere a model name can: the `model` field (L1) or `route`
(L2), `default_route`, a named route's `model`, `models` and `tiers`, and inline routes
(`cascade:fast>best`). Resolution order for a model string is named route, profile, alias,
inline grammar. A profile may only point at a **model alias**, never at another profile or a
route, so there are no chains or cycles.

**Resolved before a plan runs.** Profile names are rewritten to the real alias, in a copy of
the plan, before `Run`. So the circuit breaker, cache, cost accounting, spans, metrics and
stats all see the real model (`jeff`), never a duplicate under the profile's name. The
request keeps the name the caller used: the recent-requests row shows `fast`, with
`cheap` under "models called".

**Rules** (checked at startup, and again on every edit):
- A name matches `^[a-z0-9][a-z0-9_.-]{0,63}$`, so it never contains an inline-route
  separator (`: , > @ |`).
- Reserved: `inline`, `plan`, `unknown`, `none`, `jev-latest` (metric labels and protocol
  markers).
- It must not equal a model alias or a route name.
- At most **64** profiles, so the route label stays bounded. A profile name is a metric
  label value because it is config-defined, never client-supplied.
- A named route that uses a profile is validated *after* substitution, so a route can
  never be left invalid by a profile (for example a fan-out whose members collapse onto
  one model).

**Discovery.** `GET /v1/profiles` returns the profiles the caller may use, each with the
model it points at and that model's status. The model name behind a profile is deliberately
**not** treated as a secret. A restricted client (one with a route allowlist) sees only the
profiles on its list, and nothing about routes, profiles or models it was not given. An
admin also gets the source of each profile (`config`, `override` or `added`), the config
value an override replaced, the version, the aliases to choose from, the audit trail and any
stale overrides. Plan errors keep their detail for everyone (`duplicate model "jeff"`); a
restricted client reaching for something outside its list still gets the same bare 403 as
for a nonexistent name.

### 12.1 Editing at runtime (opt-in)

**Profiles and routes** can be edited while the gateway runs. **Models and clients cannot**:
they hold URLs, keys and access control, and belong in the config. Editing is off by
default: the config file is the source of truth, and in a container or GitOps setup it is
usually read-only.

```toml
[admin]
edit_profiles = true                                  # each kind is switched on separately
edit_routes = true
state_file = "/var/lib/ignatius/state.json"           # optional, see below
```

- **Who:** an admin client only (403 `admin_required`), and only for a kind that is switched
  on (403 `editing_disabled`).
- **Profiles:** `PUT /v1/profiles/{name}` with `{"target": "<alias>", "expected_version": N}`
  creates or retargets one; `DELETE /v1/profiles/{name}` removes a runtime-added profile or
  reverts an overridden one to its config value.
- **Routes:** `PUT /v1/routes/{name}` with `{"plan": {...}, "expected_version": N}` creates a
  route or replaces its plan (the same `Plan` shape as the config: `mode`, `model`, `models`,
  `tiers`, `threshold`, `reduce`, `min_success`, `timeout_ms`); `DELETE /v1/routes/{name}`
  removes a runtime-added route or reverts an overridden one. The body is strict: a
  misspelt field is a 400, not a silent no-op.
- **What cannot be deleted:** a profile or route defined in the config and not overridden
  (409 `not_removable`): change it, or edit the config. A runtime-added one cannot be deleted
  while a client's route allowlist names it, because allowlists are validated at startup and
  the next boot would fail; remove it from the client in the config first.
- **One version for everything:** profiles and routes share a single counter.
  `expected_version` is optional on the API. On a PUT it is a body field; on a DELETE it is
  the query parameter `?expected_version=N` (a body is also accepted, but proxies and CDNs
  commonly strip a DELETE's body, which would silently disable the check). The status page
  always sends the version of the data it was *showing*: the moment you start working in a
  table (pick a value, open the editor, arm a button) it is drawn from a frozen copy, and a
  redraw never adopts a newer version. A stale value is 409 `version_conflict` with the `current_version`,
  so two admins cannot silently overwrite each other, whichever kind they edit.
- **Atomic and consistent:** an edit is validated as a **whole snapshot** (every profile, every
  route, the default route), persisted, and only then swapped in as one immutable value. A
  request resolves against one snapshot from start to finish, so it sees the old or the new
  configuration, never half an edit. An edit that would make anything invalid is refused with
  422 `invalid_edit` and the reason: a retarget that collapses a fan-out onto one model, a
  profile delete that a route still uses, a route that clashes with a profile or alias.
- **Route limits**, on top of the plan's own validation and **for routes created or changed
  at runtime only**: thresholds in [0, 1] with known keys; at most 10 models per route;
  `min_success` between 0 and the number of models; `timeout_ms` at most 10 minutes; new
  route names follow the profile-name rule; at most 64 runtime-added routes. A route defined
  in the config is held to the plan rules alone, as it always was, so a config that loads
  today still loads (a threshold above 1, a deliberate way to force escalation, is fine
  there). Re-saving a config route unchanged is a no-op; changing it makes it a runtime
  route that must meet the limits; reverting it restores the config plan, exempt again. The
  `default_route` is config-only and cannot be broken or removed by an edit.
- **Persistence:** with `state_file`, each edit is written to a JSON file
  (`{"version": N, "profiles": {...}, "routes": {...}}`, only what differs from the config)
  by an atomic temp-file rename at mode 0600, **before** it is applied, so a failed save
  (500 `save_failed`) leaves memory and disk agreeing. At startup it is replayed on top of
  the config in a fixed order (profiles, then routes), each override judged against the ones
  before it. An override that no longer validates is **skipped and reported as stale**
  (`profile:name` or `route:name`), never fatal, so a stale edit cannot lock you out. The
  config itself is validated strictly; a corrupt state file is a startup error. Without
  `state_file`, edits live in memory and are lost on restart, and the status page says so.
- **Audit:** every real change logs one `config_changed` line (`actor` is the client name,
  `kind`, `op`, `name`, `from`, `to`, `version`; routes are described in words, e.g.
  `cascade fast@0.50 > best`) and appears in the status page's recent changes. A no-op edit
  changes nothing and is not logged.
- **Several replicas:** each keeps its own edits and state. Edit one replica, or leave
  editing off and use the config. A shared store is not implemented.

**Status page.** The Profiles table has a model dropdown per profile. The Routes table shows
each route **in words** (`fast (a question stays if confidence >= 0.50) -> best (final:
answers what is left)`) and, when editing is on, an editor: pick the mode; for a cascade,
order the tiers with arrows, pick each tier's model or profile and its threshold; for a
fan-out, pick the members, how to combine them and how many must answer; or a single model.
A per-type threshold map is shown read-only and preserved as it is. A change is two clicks
(the first arms the button and names the change, the second applies it), because it reaches
every caller. The server's error is shown as returned. A table with an edit in progress is
not redrawn by the 5-second refresh, so an open dropdown or a number being typed is never
disturbed.

**Not implemented:** editing models or clients, the `default_route`, or the per-type
threshold maps in the UI; a shared store for several replicas; profile chains; per-client
profile targets.

**Python.** The SDK resolves profiles from the same `[profiles]` table with the same rules
and passes a cross-language test against the Go gateway, but is static: there is no runtime
editing in an SDK.

## 13. Feedback and the store (implemented, except where noted)

Stats and drill-downs stay in memory and config edits in a file; neither moved onto the
store. This section adds a database, a feedback loop, and an explicit opt-in to keep
content, so the gateway can measure how good each model's decisions are on your own
traffic and produce training data. **Not implemented:** per-model scorecards on the status
page (13.4), moving stats and runtime config onto
the store, a Helm value for the store, and a Go client method for feedback (the Python `GatewayClient.feedback()` exists). The Postgres backend is tested against a real
Postgres only when `IGNATIUS_TEST_POSTGRES_DSN` is set.

### 13.1 The store

A small repository interface (`Store`) with two backends, chosen by config:

```toml
[store]
driver = "sqlite"                  # "sqlite" (default) | "postgres"
dsn_env = "IGNATIUS_STORE_DSN"     # sqlite: a file path (default ignatius.db); postgres: a connection string
# retention_days = 90              # optional; unset = never delete automatically
```

`[store]` present means the store is on; its absence means nothing is stored and the
feedback endpoint answers 404 `feedback_disabled`. A client that sets `store_content` or
`retention_days` without `[store]`, or a negative retention, is a startup error. The
gateway writes one `requests` row (client, layer, route, mode, time) and one `outcomes`
row per answered question (the model that supplied the answer, its confidence, the
cascade bar that applied) for **every** request, so feedback can be checked against the
request it names. A store write that fails is logged by kind and never fails the request.

**Writes are asynchronous.** A finished request is queued and a writer goroutine inserts it,
so the database is off the request path: with SQLite on a laptop a request costs about
55 µs with no store, with metadata only and with content alike
(`go test ./gateway -run xxx -bench StoreOverhead`, which also reports `dropped/op`). The queue
is bounded in memory (`[store] queue_size`, default 1000). When it is full the request is
**not recorded** (counted in `ignatius.store.dropped`, logged on the first drop and every
1000th) and the request itself is unaffected; a crash loses what is queued; a clean shutdown
drains it (10 seconds). `ignatius serve` waits for in-flight requests to finish on SIGTERM or
Ctrl-C (up to 15 seconds) before it drains, so a request answered during shutdown is still
recorded; a Python end-to-end test sends SIGTERM mid-request to check this. A record
enqueued after the writer closed is counted as dropped. Feedback that arrives before its request has been written waits for
it (up to 2 seconds) instead of getting a 404. Feedback itself is written synchronously: it is
low volume and the caller wants to know it was recorded.

**Containers.** The image runs as a non-root user, and the default SQLite file
`ignatius.db` resolves to the working directory, which that user cannot write. Set
`IGNATIUS_STORE_DSN` to a file on a mounted volume (or use Postgres).

- **SQLite** is the default: a pure-Go driver (no cgo), so the single static binary and
  the simple container stay. One gateway instance owns the file.
- **Postgres** is for several replicas sharing one store. It is also the shared store 12.1
  lists as not implemented, so runtime config edits and stats can move onto it later.
- `database/sql` with plain SQL and numbered migrations run at startup. No ORM, no
  dialect-specific features, so both backends pass the same test suite.
- Without `[store]`, nothing in this section is active and behavior is unchanged.

### 13.2 Feedback

A caller says whether a decision was good. Feedback attaches to a **question**, not a
request, because one request holds several independent judgments.

`POST /v1/ignatius/feedback`, same client keys and auth as `/v1/systemone`:

```json
{ "request_id": "...", "question_id": "urgency",
  "verdict": "good",                      // "good" | "bad"
  "correct": "high",                      // optional: the right answer
  "source": "human" }                     // "human" | "automated" (default "human")
```

- `request_id` is the one returned in the `ignatius` object of every response. Any
  client may give feedback on any request (see below).
- `correct` uses the same value shape as an answer of that question's type (a choice
  label, a score level, a noul probability or boolean). Its shape is always checked; it is
  checked against the question's own options or levels only when content is stored (13.3),
  because otherwise the criteria are not known.
- A later feedback for the same `(request_id, question_id)` from the same client replaces
  the earlier one; the history is kept as a row with a `superseded` timestamp.
- `source` is recorded and never merged: automated signals (a ticket was reopened, the
  user overrode the answer) are weaker than a person's correction and are weighted
  separately by `calibrate` and by exports.
- A request answered from the cache is a request like any other: it gets its own
  `request_id` and its own row. The feedback records the model that supplied the final
  answer (`answers[id].model`), its confidence, and for a cascade the bar that settled it.
- Feedback counts toward the client's rate limit and request counters, like the routing
  endpoints do (11.4 names only those two).
- **Without content storage** a feedback row holds only metadata: ids, the verdict, the
  model that answered, its confidence, the threshold, the client, the time. That is
  enough for `calibrate` and for per-model scorecards.
- **Late feedback:** there is no default expiry. If `[store] retention_days` is set,
  everything older is deleted (13.3) and feedback for a deleted request is 404
  `unknown_request`; otherwise rows are kept until an operator purges them.
- **Feedback crosses clients:** any authenticated client may give feedback on any request
  (a downstream system or a reviewer is often not the caller that made it). The row records
  who gave it (`client`) and who made the request (`owner`); calibration, scorecards and
  retention follow the **owner**, and each giver keeps one live verdict per question (a later
  one from the same giver replaces theirs). A request or question that does not exist is 404
  `unknown_request`, as is one past retention. The owner's questions are not shown to another
  client, so a non-owner's `correct` is checked by shape only (below). Request ids are random
  and not secret, so this is open to any client holding a valid key.
- Unknown JSON fields are 400. `correct` is checked by question type: a choice label (a
  string; when content is stored, one of the question's options), a score level (a number;
  with content, inside the levels), or for noul `true`, `false` or a probability in 0..1.

**Python:** `GatewayClient.feedback(request_id, question_id, verdict, correct=None,
source="human")`, and `Routed.request_id` on a result from the gateway. There is no Go
gateway client; Go callers post the JSON above. The Python SDK's in-process `Ignatius` has no
store and so no feedback.

### 13.3 Storing content (explicit opt-in)

The rule in 10.1 changes from "never store `state`, question text or answer values" to
"never, unless the client opted in". Off by default.

```toml
[[clients]]
name = "support-triage"
key_env = "TRIAGE_KEY"
store_content = true               # default false
retention_days = 30                # unset (the default) = keep until an operator deletes it
```

- Opt-in is **per client or user** (it fits the existing key config; a per-route switch is a
  possible later addition). A client without it is unaffected.
- For an opted-in client the store keeps, per request: `state`, each question's
  `instructions` and `criteria`, every model's answer and probabilities (including tiers
  that did not supply the final answer, which is what makes shadow auditing and
  distillation possible), and the plan that ran.
- **No retention unless configured.** Without any `retention_days` nothing is deleted.
  There are two knobs, both off by default. A client's `retention_days` deletes **that
  client's stored content** after that many days and keeps the request metadata and
  feedback. `[store] retention_days` deletes **everything** (requests, outcomes, content,
  feedback) older than that, for every client. A background job runs hourly and on startup;
  both are hard deletes (tested). Because content is kept indefinitely by default,
  `ignatius purge --client NAME [--before DATE] [--content-only]` deletes on demand.
- At-rest encryption is left to the volume or the database (state this in the deployment
  docs); Ignatius does not manage keys.
- **Privacy test.** The sentinel test of 10.1 stays for every client that has not opted
  in, and is extended: for an opted-in client the sentinel is present in the store and in
  no span, metric, log line, OTLP payload or stats response. Content never leaves the
  store except through the export (13.5), which is a command run on the host, not an HTTP
  endpoint. `TestStoredContentNeverReachesTelemetry` checks spans, metrics, logs and the
  admin endpoints, and fails when a leak is injected.

### 13.4 Using the feedback

- **`ignatius calibrate --client NAME [--since DATE] [--source human|automated|audit|all]
  [--target 0.95] [--min 20] [--json]`** (implemented): for each model and question type,
  sweeps the bars 0.5 to 0.95 over the feedback and prints, per bar, how many answers it
  keeps, their accuracy (a "good" verdict counts as right, "bad" as wrong) and the share
  that would escalate, then the lowest bar that reaches `--target` with at least `--min`
  kept answers. Default source is human. A noul answer's confidence is the derived `abs(p - 0.5) * 2` (1.1), so it is
  included; only a row with no confidence at all is left out. Stored confidences are on whatever scale the
  model's policy (1.2) produced at the time, so after changing a model's `confidence` setting, pass
  `--since` the date of the change. Where several clients rated the same decision it counts
  it once: a human's verdict over an automated one, then the request owner's, then the
  newest. It reads feedback only, so it needs no stored content.
- **Scorecards:** per alias and question type, agreement with outcomes: accuracy where a
  `correct` exists, and the share of "good" verdicts otherwise, against stated confidence.
  Shown on the status page for admin clients. This is the measured calibration the
  upstream docs do not publish.
- **Shadow auditing** (13.7) records, without anyone labeling anything, whether a cheap tier agreed
  with the next one; `calibrate --source audit` (or `all`) reads it as labels.

### 13.5 Export for fine-tuning

`ignatius export --client NAME [--since DATE]` writes JSONL to stdout, one line per question:
`state`, `images` (how many images the request carried; the images are not stored, so skip rows where
this is above zero unless the question stands on its own), the question, the final answer, every
model's probabilities, and the feedback
(`verdict`, `correct`, `source`, `client`). `feedback` is the verdict that stands when several
clients rated the question (a human's over an automated one, then the owner's, then the
newest); `all_feedback` lists every client's, for a trainer that wants to weigh disagreement
itself. Only for opted-in clients. Use `correct` as the label
where present, then human "good" verdicts, then a teacher model's answer; soft labels
(the probabilities) are kept so a student can be trained on distributions, not only the
top choice.

Training is outside Ignatius. The export feeds an external trainer (decisionsmith, Laya's
trainer, Kev's loop); the result is registered as another alias, so a fine-tuned model is
one more cascade tier. Closed models (Jev) can serve as the teacher but cannot be
fine-tuned.

### 13.6 Open decisions

- Per-route opt-in in addition to per-client.
- (Decided: writes are asynchronous, 13.1.) Whether a dropped request record should be
  retried or spilled to disk instead of dropped.
- Whether to store the full plan trace or only the final decisions (today: the plan and
  every model's result, but not the cascade trace).
- Whether `automated` feedback can ever count toward `calibrate` thresholds by default.
- Moving stats, drill-downs and runtime config edits onto the store (and so supporting
  several replicas) in the same change or a later one.

### 13.7 Shadow auditing

A cascade lets a cheap tier answer on its own when its confidence clears the bar. Whether that
confidence was earned is what `calibrate` needs labels for; shadow auditing produces them without
anyone labeling anything. For a sampled fraction of cascade requests, the questions a **non-final tier
settled by itself** are also put to the **next tier**, in the background after the response has gone,
and the comparison is recorded. This is roadmap item 4.

```toml
[audit]
sample_rate = 0.05      # fraction of cascade requests audited, 0 to 1; default 0 (off)
max_inflight = 4        # audits running at once; default 4
```

- **Needs a `[store]`** (a `sample_rate` above 0 without one is a startup error). `[audit]` with a
  rate of 0, or no `[audit]`, means no auditor and no extra calls.
- **What is audited.** One coin flip per cascade request. Then, per question: only one a tier settled
  and that has a next tier. A question the tier escalated was already asked of the next tier, and one
  settled at the last tier has none, so neither is audited. Questions settled at the same tier go to
  the next tier in one call. Only models already in the caller's plan are called, so the auditor
  reaches nothing the client could not have, and sends the state to nothing it would not have
  escalated to. Single and fan-out routes are never audited, and neither is a request that failed.
- **It never touches a response, but it is a real call.** The audit runs after the response is
  written, in its own goroutine, and the cascade trace in the response shows only the tiers that
  served the request. It goes through the same decorated registry as any call, so the next tier's
  rate limit and circuit breaker see it: a burst of audit timeouts or 429s counts against that
  breaker like any other failures, and could open it for real traffic. Two guards limit that: an
  audit is **never sent to a tier whose breaker is not closed** (counted as `reason=unhealthy`, so it
  can neither spend a half-open probe nor poke a tier known to be failing), and audits are bounded
  and sampled. A low `sample_rate` keeps the extra load well inside what the tier already carries. It is
  bounded: at most `max_inflight` at once, and a sampled request that finds them busy is skipped
  and counted (`ignatius.audit.skipped{reason=busy}`), never queued. A failed audit call is counted
  (`reason=failed`) and writes nothing. `Server.Close` waits for running audits (up to the shutdown
  budget) before it drains the store, so an audit started before shutdown is written.
- **Agreement** is per type: a choice agrees on the same option; a noul on the same side of one half
  (0.5 counts as yes); a score on the same nearest level. Answers of different types, or with no
  value, are not comparable and are not recorded.
- **What is stored.** An `audits` table (one row per audited question: request id, question id,
  the owner client, route, type, the tier and its confidence and the bar it cleared, the auditing
  tier and its confidence, and `agreed`), added by a numbered migration so an existing database
  upgrades in place. **No content**: no state, no question text, no answer value, whatever the
  client's `store_content`, and none in a span, metric or log line (a test checks). The audit's
  telemetry uses the route label `audit`. Retention (13.3) deletes a client's audits with its
  requests when `[store] retention_days` is set; a content-only purge leaves them.
- **As labels.** `ignatius calibrate --source audit` (or `all`) treats agreement as a "good" verdict
  for the tier that settled the question and disagreement as "bad", and sweeps thresholds as it does
  for human feedback. It is a **weaker label than a person's**: the next tier can be the wrong one, so
  a cheap tier that agrees with an expensive wrong one looks better than it is, and one that disagrees
  with an expensive wrong one looks worse. Where a question has both, a human's verdict beats an
  automated signal beats an audit (`store.Resolve`).
- **Cost.** Every audited question costs one call to the next tier, so the expected extra spend is
  about `sample_rate` times the share of questions settled early times that tier's price. The audit's
  cost is not in `/v1/stats` (it is not a request) or yet in a metric; `ignatius.audits{model,type,agreed}`
  counts the comparisons.
- **Not implemented:** an event for each audit, a per-client or per-route opt-out (a rate applies to
  every cascade request), sampling by confidence (auditing the questions nearest the bar teaches
  more), and showing agreement rates on the status page.

## 14. Events

Implemented. The gateway can emit what it does as events, so a pipeline outside Ignatius
(analytics, a labeling queue, a training job) can react without polling. It is independent
of the store: events work with no `[store]`, and the store works with no events.

### 14.1 Sinks

```toml
[events]
queue_size = 1000                 # per sink; default 1000

[[events.sinks]]
type = "webhook"                  # "webhook" | "file", or a plugin type (14.5)
name = "bridge"                   # a label for metrics and logs; default "<type>-<n>"
only = ["request.completed"]      # optional filter; default every type
timeout_ms = 5000                 # per attempt (every sink type)
workers = 1                       # concurrent senders; 1 keeps order
queue_size = 1000                 # overrides [events] queue_size for this sink
[events.sinks.options]            # webhook options
url_env = "EVENTS_URL"            # required: the URL is read from the environment, never the config
secret_env = "EVENTS_SECRET"      # optional: sign the body with HMAC-SHA256
auth_env = "EVENTS_AUTH"          # optional: sent whole as the Authorization header
retries = 2                       # after the first attempt

[[events.sinks]]
type = "file"
path = "/var/log/ignatius/events.jsonl"   # or "-" for stdout; opened 0600, append-only
```

The file sink is in the core. The webhook is a plugin that lives in this module (14.5): the stock
build has it, a minimal build need not.

**Kafka, Kinesis and the rest.** The core does not link a broker client (both have a compile-time
plugin, 14.5), and neither broker takes a plain webhook: Kinesis only accepts SigV4-signed `PutRecord` calls, and Kafka speaks
its own protocol. Put a small receiver behind the webhook, or point a log shipper at the file.
None of these paths is tested here.

- **Either broker, least code:** Vector (`http_server` source to a `kafka` or `aws_kinesis_streams`
  sink) or Redpanda Connect (`http_server` input to a `kafka` or `aws_kinesis` output).
- **Kinesis on AWS:** an API Gateway endpoint or a Lambda function URL that verifies
  `X-Ignatius-Signature` and calls `PutRecord`. API Gateway's direct Kinesis integration works
  too, but needs a mapping template and cannot check the HMAC.
- **Kafka REST proxies** expect their own body shape (for Confluent,
  `application/vnd.kafka.json.v2+json` with a `records` array), so they need an adapter.
- **File or stdout to either:** Vector, Fluent Bit or Filebeat reading the JSONL.

Not bridges, despite the names: Firehose's HTTP endpoint and EventBridge API destinations are
outbound (they call your server), and EventBridge Pipes reads from a source rather than
accepting a POST.

A broker-native sink can be compiled in as a plugin (14.5); the envelope and delivery rules
below do not change.

### 14.2 Envelope and delivery

Each event is a CloudEvents 1.0 structured-mode JSON object (`specversion`, `id`, `source`
= `ignatius`, `type`, `time`, `datacontenttype`, `data`), sent as
`application/cloudevents+json`, or one JSON line in a file. Webhook headers:

| Header | Value |
| --- | --- |
| `X-Ignatius-Event` | the type |
| `X-Ignatius-Delivery` | the event `id`, identical on every retry: an idempotency key |
| `X-Ignatius-Timestamp` | unix seconds of this attempt |
| `X-Ignatius-Signature` | `sha256=` and the hex HMAC-SHA256 of `timestamp + "." + body`, when `secret_env` is set; reject old timestamps to stop replays |

**Best effort.** Each sink has its own bounded in-memory queue and sender, so a slow sink
slows neither a request nor another sink. A full queue drops the event
(`ignatius.events.dropped`, logged on the first drop and every 1000th); an event is lost if the
process dies; a clean shutdown delivers what is queued (10 seconds). A webhook retries a
transport error, a timeout, 429 and 5xx with backoff (200 ms, 800 ms, ...), keeps the same
delivery id, and treats any other response as final (`ignatius.events.failed`). Redirects are
not followed, so a credential is never forwarded to another host. Delivery is therefore **at
least once with possible loss**: consumers dedupe on `id`. A transactional outbox (events written
with the request, then relayed) is the way to get stronger guarantees; it is not built.

### 14.3 Event types and privacy

The same rule as the store (13.3): metadata for every client, content only for a client with
`store_content = true`.

- **`ignatius.request.completed`**, one per `/v1/systemone` and `/v1/route` request, whether or
  not it succeeded: `request_id`, `client`, `layer`, `route`, `mode`, `ok`, `duration_ms`,
  `images` (a count, never an image), `cost_usd`, `failure_kinds`, and per answered question `questions[]` of `{question_id, type,
  model, confidence, threshold}`. For an opted-in client also `content`: `state`, `questions`,
  `answers` and `results` (every model's answer, including tiers that did not supply the final
  one).
- **`ignatius.feedback.received`**, one per accepted feedback: `request_id`, `question_id`,
  `client` (who gave it), `owner` (who made the request), `verdict`, `source`, `type`, `model`,
  `confidence`, `threshold`, `has_correct`. The corrected answer itself (`correct`) is included
  only when the **owner** opted in, because it is content about the owner's data.

Webhook and file errors, and the log lines about them, name the sink and a failure kind
(`timeout`, `transport`, `http`, `rejected`), never a URL (it may hold a token), a header, the
secret or a body. A webhook URL that is not http or https, or an unset `*_env`, is a startup
error that does not echo the value. Tests assert that a non-opted-in client's events contain no
content, that failures log only the kind, and that a redirect target never sees the
Authorization header.

### 14.4 Not implemented

Sink plugins beyond Kinesis and Kafka (NATS and the like); an outbox for durable delivery; batching several events per POST; an
admin endpoint to list sinks or replay events; events for config changes and key creation (the
audit log lines are the record of those today).

### 14.5 Sink plugins, chosen at compile time

A sink type is registered by a package, from `init()`, with
`events.RegisterSinkType(name, SinkType{Validate, New})`. The core has the `file` sink; the
`webhook` sink is a plugin that lives in this module (`go/events/sinks/webhook`, standard library
only). A heavier sink lives in **its own Go module** and is compiled in by a build that
imports it for its side effect, the way a Caddy build adds a module. Nothing is loaded at run
time (no `.so`, no Go `plugin` package): a binary has exactly the sinks it was built with,
adding one means a rebuild, and the core's `go.mod`, `go.sum` and binary never see a plugin's
dependencies.

- **The command is a library.** The commands are in package `go/cli`; `go/cmd/ignatius/main.go` is
  `cli.Main()` plus a blank import of `go/standard`, the way Caddy's `modules/standard` bundles the
  default modules (today: the webhook sink). A custom build is one file:

  ```go
  package main

  import (
  	"github.com/phin-tech/ignatius/go/cli"
  	_ "github.com/phin-tech/ignatius/go/standard"          // the webhook sink; leave it out to omit it
  	_ "github.com/phin-tech/ignatius/go/plugins/kinesis"
  )

  func main() { cli.Main() }
  ```

  The version is stamped with `-X github.com/phin-tech/ignatius/go/cli.Version=...`.
- **Config.** A plugin's settings are the sink's `[events.sinks.options]` table, read with
  `SinkConfig.OptString`, `OptInt` and `OptKeys` (so it can reject unknown keys). A config that
  names a type the binary does not have fails at startup with `not compiled into this binary`
  and the list of types it does have.
- **Registration is checked.** Registering an empty name, a type with no `New`, or a name already
  taken panics at startup, so two plugins cannot shadow each other.
- **What a plugin must do.** Return `*events.SendError` with a `Kind`, never an error carrying an
  endpoint, credential, message or body (the log line and metric name only the kind); read
  secrets from the environment, never the config; and fail startup for a setting that can never
  work.

**`go/plugins/kinesis`** (implemented, its own module that builds against the checkout beside it
through a `replace`):

```toml
[[events.sinks]]
type = "kinesis"
name = "stream"
only = ["request.completed"]            # as for any sink
[events.sinks.options]
stream = "ignatius-events"              # required
region = "us-east-1"                    # optional; else AWS_REGION or the usual chain
partition_by = "request_id"             # request_id (default) | client | type | id
endpoint_env = "KINESIS_ENDPOINT"       # optional env var holding a custom endpoint (LocalStack)
```

Credentials come from the AWS default chain (environment, shared config, web identity, ECS or
instance role), never the config. Each record is the CloudEvents JSON the webhook sends, written
with `PutRecord`. `request_id` is the default partition key, so one request's events and the
feedback about it stay in order on one shard (an event without one falls back to its id). A record
over Kinesis's 1 MiB limit is `rejected` without a network call, which an opted-in client's
content can reach. Kinds are `rejected` (no such stream, no permission, too large), `throttled`,
`timeout` and `transport`; the SDK retries throttling and transient errors first. Startup fails
if `DescribeStreamSummary` says the stream does not exist; any other error there (for example no
permission to describe) is left to the first send.

Build and run it:

```
cd go/plugins/kinesis && go build -o ignatius ./cmd/ignatius
docker build -f plugins/kinesis/Dockerfile -t ignatius-kinesis .       # from go/
```

Tested against a fake Kinesis speaking the JSON protocol, and by building both binaries and
running one config through each (the core build refuses it, the custom build delivers the event).
Opt-in integration tests run against a local AWS emulator, [Floci](https://floci.io), which keeps the
records so the test reads them back through the AWS SDK: the plugin's write, a missing stream
failing startup, and the custom binary serving a request whose event a consumer then reads from the
stream (`docker run -d --rm -p 127.0.0.1:4566:4566 floci/floci:latest`, then
`IGNATIUS_TEST_KINESIS_ENDPOINT=http://127.0.0.1:4566 go test ./plugins/kinesis -run Emulator`).
**Not tested against real AWS.** The core binary is 9.8 MB as an image and the Kinesis build 11.0 MB.

**`go/plugins/kafka`** (implemented, its own module like the Kinesis one; the client is franz-go, pure
Go with no cgo, so the image stays static). It works with Apache Kafka, Redpanda, MSK and anything that
speaks the Kafka protocol.

```toml
[[events.sinks]]
type = "kafka"
name = "bus"
only = ["request.completed"]            # as for any sink
timeout_ms = 5000                       # how long one event may take to be acknowledged; at least 1000
[events.sinks.options]
brokers = ["kafka-1:9092", "kafka-2:9092"]   # or brokers_env = "KAFKA_BROKERS" (comma separated); exactly one
topic = "ignatius-events"                    # required; must already exist
key_by = "request_id"                        # request_id (default) | client | type | id
acks = "all"                                 # all (default; idempotent producer) | leader
compression = "snappy"                       # none | gzip | snappy | lz4 | zstd
client_id = "ignatius"
tls = true                                   # system roots; tls_ca_file adds a PEM CA for a private one
tls_ca_file = "/etc/ssl/kafka-ca.pem"
sasl = "scram-sha-512"                       # plain | scram-sha-256 | scram-sha-512
username_env = "KAFKA_USER"                  # credentials come from the environment, never the config
password_env = "KAFKA_PASSWORD"
```

The record value is the CloudEvents JSON the webhook sends; the key is the request id (so one
request's events, and the feedback about it, stay in order on one partition; an event without one
falls back to its id); headers carry `content-type`, `ignatius-event` and `ignatius-delivery` for
consumers that filter or dedupe without parsing the value. With `acks = "all"` the producer is
idempotent, so the client's retries within `timeout_ms` do not duplicate a record. Kinds are
`rejected` (record too large, unknown topic, no permission, failed SASL), `timeout` and
`transport`; the client's message, which can name brokers and topics, is never carried.

Startup fails for a config that can never work (an unset `*_env`, an unreadable CA file, a
`timeout_ms` under 1000, a bad option) and when the cluster answers that the topic does not exist,
because the sink never creates topics. A cluster that cannot be reached, or rejects the credentials,
does not fail startup (a broker may be restarting; the check gets 3 seconds and moves on), and the
first send fails by kind instead. Client TLS certificates (mutual TLS), Kerberos and OAuth SASL are
not supported.

```
cd go/plugins/kafka && go build -o ignatius ./cmd/ignatius
docker build -f plugins/kafka/Dockerfile -t ignatius-kafka .       # from go/
```

Tested against franz-go's in-process fake cluster (`kfake`): a round trip with key and headers, each
`key_by`, brokers from the environment with `acks = "leader"`, a missing topic, an unreachable
cluster, SASL PLAIN with a right and a wrong password, TLS with a private CA, and a build test that
runs one config through the core binary (refuses it) and the custom one (delivers the event, read
back from the topic). An opt-in test (`IGNATIUS_TEST_KAFKA_BROKERS`, `IGNATIUS_TEST_KAFKA_TOPIC`)
runs a round trip against a real broker. **Against MSK as emulated by Floci** (a real Redpanda broker
behind the MSK API) `plugins/kafka/test-msk-floci.sh` creates a cluster through the MSK REST API,
waits for it to be `ACTIVE`, takes the bootstrap brokers the way an application would, creates the
topic, and sends events through the sink: five arrive with the right keys and headers, and a missing
topic fails startup. Floci does not publish the broker's port to the host and the broker advertises
its container name, so the script runs the test in a Go container on the same Docker network (the
test maps the advertised name itself). **Not tested against Apache Kafka, real MSK, or SCRAM,
TLS and SASL on a real broker**; those ran only against `kfake`.

**The webhook sink is optional too.** A build that imports package `cli` alone has only the `file`
sink, so its event sinks cannot make an outbound HTTP call at all; a config that asks for `webhook`
fails at startup with `not compiled into this binary` and the list of types it does have. That is
the point of leaving it out: a deployment can guarantee, by construction, that a config edit
cannot point event delivery at a URL. (The gateway's own HTTP server and its upstream model calls
are unaffected.) A test builds the stock command and a minimal one from a throwaway module and runs
one config through each. The webhook's settings are in its `[events.sinks.options]` table, like every
plugin's; `timeout_ms`, `queue_size` and `workers` stay on the sink. A key under `[events]` that this build does not
know is a startup error that names it (the rest of the file is read leniently, as before), so a
setting left in the wrong place, such as a webhook's `secret_env` at the top of the sink instead of
under its options, fails loudly instead of sending events unsigned.

## 15. Evaluation

Implemented. A way to run a labeled test set through a cascade, a fan-out or any other route, and judge the
result: how accurate it is, what each tier or member contributed, what it cost, whether its confidence can be
trusted, and whether it is actually better than the alternative. It runs the real plans through the real
library (`go/eval`), so it measures what production would do. There are three ways in, one engine behind
them: `ignatius eval` (15.2), an admin API (15.3) and an Evaluate panel on the status page (15.3).

### 15.1 The test set

JSONL, one item per line (a JSON array of items is accepted too):

```json
{"id": "t17", "state": "I was charged twice", "images": [], "questions": {
   "billing": {"type": "noul",   "instructions": "Is this about billing?"},
   "tone":    {"type": "choice", "instructions": "Tone?", "criteria": {"calm": null, "frustrated": null}},
   "urgency": {"type": "score",  "instructions": "How urgent?", "criteria": ["no rush", "this week", "today"]}},
 "gold": {"billing": true, "tone": "frustrated", "urgency": 2}}
```

- `state`, `images` and `questions` are an ordinary request. `gold` gives, per question id, the answer it should get;
  a question with no gold is asked but not judged. `id` is optional and only names the item in a report.
- **Gold by type.** A *choice*: one of its options (checked against `criteria`). A *noul*: `true` or `false`
  (`"yes"`, `"no"`, `1` and `0` also work). A *score*: a whole level index, inside the levels.
- Two sample sets ship in `examples/eval`: 57 hand-written support tickets (a choice, two noul questions and a score,
  with a README on how its labels were checked), and `github-prs`, 72 merged pull requests from two MIT-licensed projects
  with gold derived from facts the model is not shown, and a builder script for more. A test keeps each valid.
- **Validation is up front and names the problem:** an error says `line N:` and what is wrong (bad JSON, an unknown
  field, gold for a question the item does not ask, a gold answer that is not an option, no gold anywhere). Nothing
  runs until the whole set is valid.

### 15.2 `ignatius eval`

```
ignatius eval --config ignatius.toml --data test.jsonl \
  --route strong --route 'cascade:cheap@0.8>strong' --route 'fan-out:a,b,c|vote' \
  [--sweep] [--concurrency 4] [--misses 10] [--json report.json] [--max-items N]
```

Each `--route` is anything a request's `model` can be: a named route, a profile, a model alias or an inline route.
**The first is the baseline** and every other is compared with it. It makes real calls to the models the routes
name. A fan-out must have a reducer (`vote`, `mean` or `most_confident`): without one it returns several answers per
question and cannot be judged.

### 15.3 What is judged

- **Correctness per question.** A choice is right if it is the gold option. A noul is right if its probability is
  on the gold side of one half. A score is right if it rounds to the gold level (its mean distance from the gold
  level is reported too). A question the route did not answer counts as wrong, and is counted separately.
- **Accuracy** over the judged questions, with a 95% Wilson interval, and by question type.
- **Who answered, and how right they were:** for each model that gave final answers, its share and its accuracy on
  those. For a cascade this is the number that matters: is the cheap tier right when it keeps an answer? A cascade also
  reports each tier's questions asked, settled and escalated; a fan-out reports each member's own accuracy next to the
  combined one.
- **Cost, tokens and latency:** summed from the routed results (cost needs the model priced, 11.3), per 1,000 items;
  latency is wall time per item, p50 and p95.
- **Calibration:** the expected calibration error of the top probability (10 bins, lower is better), and the AUROC of
  the confidence for telling right answers from wrong ones (0.5 means it says nothing, 1 is perfect).
- **A verdict against the baseline.** The accuracy difference with a **paired bootstrap** interval (2,000 resamples of
  the items, so the pairing and the clustering of several questions in one item are respected, with a fixed seed so a
  rerun gives the same interval), the cost ratio, and one sentence: "better than X by 4.5 points (95% CI 1.0 to 8.0), at
  41% of its cost", "worse than", or "no detectable difference from". It says "better" or "worse" only when the interval
  excludes zero, so a small test set honestly says it cannot tell.
- **Misses:** the first N wrong answers, as item id, question, gold, what it said, who said it and how confident. No
  content: not the state, not the question text.
- **A threshold sweep** (`--sweep`), for cascades. It runs each tier's model once on its own over every item, then
  replays the cascade offline at bars of 0.5 to 0.95, applied to every tier but the last: the accuracy at each, the share
  of questions that reach the later tiers, and an estimated cost. This answers "where should the threshold be?" without
  guessing, for the price of one run per model. The cost estimate splits each tier's solo cost by the questions it would
  have been asked, so it is an estimate.

### 15.4 The API and the status page

Off by default, because a run spends model calls: `[eval] enabled = true`, with `max_items` (500), `max_concurrency` (8),
`max_body_bytes` (8 MiB) and `keep` (10 finished runs held in memory). All of it is **admin only**, and a route the
admin's own allowlist does not permit cannot be evaluated either.

```
POST   /v1/evals       {routes: [..up to 5..], data: "<jsonl>" | items: [..], sweep?, concurrency?, misses?}  ->  202 {id, status: "running", ...}
GET    /v1/evals/{id}  {id, status: running|done|failed|canceled, done, total, routes, items, report?, error?}
GET    /v1/evals       the recent runs, newest first, without reports
DELETE /v1/evals/{id}  cancels a running one (202), forgets a finished one (204)
```

**One at a time** (409 `eval_busy`), so a click cannot multiply the spend, and shutting the gateway down cancels a
run in progress. A bad dataset is 422 `invalid_dataset` with the line; an unknown route is 422 `unknown_model`; a
disabled feature is 404 `eval_disabled`. The status page shows an **Evaluate** panel (only when evaluations are on):
routes one per line, a test set pasted or loaded from a file, a sweep checkbox; it polls a running evaluation, can
cancel it, picks up one already running after a reload, and renders the comparison table, verdicts, tier and member
breakdowns, misses and the sweep. It builds all of it from text nodes, so nothing in a dataset or a report can become markup.

**Privacy.** The dataset is content. It lives in the gateway's memory for the run only (and in the browser's
textarea); it is not stored, not exported, not logged and not in a span or metric. A report holds numbers and names. The
log has `eval_started` and `eval_finished` with an id, the client and counts. The model calls themselves go through the
ordinary registry with the route label `eval`, so the telemetry rules of 10.1 hold; they are not recorded as requests in
`/v1/stats`, are not audited (13.7) and are not stored (13).

### 15.5 Not implemented

A pass/fail gate (`--min-accuracy`, a non-zero exit) for CI; per-class precision and recall (the report has accuracy by
question type, not by label); a persisted history of runs (finished runs are in memory, the last `keep`, and lost on
restart); a sweep that varies each tier's bar separately or other policies (only a single threshold on all but the last
tier); scheduled evaluations; and comparing runs from different days.
