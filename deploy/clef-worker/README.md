# ignatius-clef: hosted Clef behind the Jev-compatible API

A Cloudflare Worker that serves `POST /v1/systemone` (and `GET /readyz`) in front of Workers AI
[Clef](https://developers.cloudflare.com/changelog/post/2026-10-01-clef-workers-ai/), so Ignatius, the
official typesafe SDK or any Jev client uses it as an ordinary `provider = "systemone"` model. See SPEC 3.1
for how it fits and what has been checked against the real service.

```
POST /v1/systemone   {state, model?, images?, questions}  ->  {model, answers, usage}
GET  /readyz         no auth, does not call the model
```

- `model`: `clef-flash` (9B) or `clef` (27B). None, or the SDK's `jev-latest`, uses `DEFAULT_MODEL`
  (`clef-flash` in `wrangler.jsonc`).
- `images`: up to 4 base64 PNG, JPEG or WebP strings, raw (as Ollama and Jev take them) or data URLs. The Worker
  converts raw ones to the data URLs Workers AI requires.
- Auth: `Authorization: Bearer <key>` against the `WORKER_API_KEY` secret (several keys, comma-separated, to
  rotate). **With no key set the Worker refuses every request.**

## Try it locally

`wrangler dev` runs the Worker on your machine but its `AI` binding calls the real Workers AI on your account
(a few cents at most for a handful of calls):

```
npm test                                       # the logic, with a fake AI binding
printf 'WORKER_API_KEY=dev-key\n' > .dev.vars  # gitignored
npx wrangler dev
curl -s localhost:8787/v1/systemone -H 'Authorization: Bearer dev-key' -d \
  '{"state":"Invoice charged twice, refund or we cancel","model":"clef-flash","questions":{"dept":{"type":"choice","instructions":"Which team?","criteria":{"billing":"invoices","technical":"bugs"}}}}'
```

## Deploy

Deploying puts a public `*.workers.dev` URL on your Cloudflare account, and every authorized request is
Workers AI usage billed to it. Set the secret first so the Worker is never up unauthenticated:

```
npx wrangler secret put WORKER_API_KEY         # paste a long random key (openssl rand -hex 32)
npx wrangler deploy
```

`workers_dev` is on in `wrangler.jsonc`; turn it off and add a `routes` entry to serve it from your own
domain. Then point a model at the URL (SPEC 3.1) with `api_key_env` naming the variable that holds the key.
