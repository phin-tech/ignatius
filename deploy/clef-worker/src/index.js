// Jev-compatible /v1/systemone in front of Workers AI Clef.
//
//   POST /v1/systemone   {state, model?, images?, questions}  ->  {model, answers, usage}
//   GET  /readyz         no auth; does not call the model
//
// Auth: `Authorization: Bearer <key>`, checked against the WORKER_API_KEY secret (comma-separated to
// rotate). Missing credential is 403 and a wrong one 401, as on Jev and Decis. With no key configured the
// Worker refuses every request: it never serves unauthenticated.
//
// Nothing from a request (state, questions, images) or from the model is logged.

const MODELS = {
  clef: "@cf/cloudflare/clef",
  "clef-flash": "@cf/cloudflare/clef-flash",
};
const MAX_QUESTIONS = 64;
const MAX_IMAGES = 4;
const MAX_BODY_BYTES = 8 * 1024 * 1024;
const QUESTION_TYPES = new Set(["noul", "choice", "score"]);

const json = (body, status = 200) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const fail = (status, type, message) => json({ error: { type, message } }, status);

// Constant-time comparison of two strings, by their SHA-256 digests (so lengths do not leak).
async function sameSecret(a, b) {
  const enc = new TextEncoder();
  const [da, db] = await Promise.all([
    crypto.subtle.digest("SHA-256", enc.encode(a)),
    crypto.subtle.digest("SHA-256", enc.encode(b)),
  ]);
  const x = new Uint8Array(da), y = new Uint8Array(db);
  let diff = 0;
  for (let i = 0; i < x.length; i++) diff |= x[i] ^ y[i];
  return diff === 0;
}

// authorize returns null when the caller may proceed, else the response to send.
export async function authorize(request, env) {
  const keys = String(env.WORKER_API_KEY || "").split(",").map((k) => k.trim()).filter(Boolean);
  if (keys.length === 0) {
    return fail(503, "not_configured", "WORKER_API_KEY is not set, so this Worker refuses every request");
  }
  const [scheme, token] = (request.headers.get("authorization") || "").split(" ", 2);
  if (!token || scheme.toLowerCase() !== "bearer") return fail(403, "missing_credentials", "a Bearer token is required");
  let ok = false;
  for (const k of keys) ok = (await sameSecret(token, k)) || ok; // no early exit: compare against every key
  return ok ? null : fail(401, "invalid_credentials", "invalid API key");
}

// toDataUri turns a raw base64 image (what Ollama and the Jev wire take) into the data URI Workers AI
// requires ("image must be an embedded base64 data URI"), labeling it by what the bytes are. A data
// URI passes through. It returns null for something that is not a PNG, JPEG or WebP.
export function toDataUri(image) {
  if (image.startsWith("data:")) return image;
  let head;
  try {
    head = atob(image.replace(/\s/g, "").slice(0, 24)); // the first 18 bytes are enough to tell
  } catch {
    return null;
  }
  const b = (i) => head.charCodeAt(i);
  let mime = null;
  if (head.length >= 8 && b(0) === 0x89 && head.slice(1, 4) === "PNG") mime = "image/png";
  else if (head.length >= 3 && b(0) === 0xff && b(1) === 0xd8 && b(2) === 0xff) mime = "image/jpeg";
  else if (head.length >= 12 && head.slice(0, 4) === "RIFF" && head.slice(8, 12) === "WEBP") mime = "image/webp";
  return mime ? `data:${mime};base64,${image.replace(/\s/g, "")}` : null;
}

function validate(body) {
  if (body === null || typeof body !== "object" || Array.isArray(body)) return "the body must be a JSON object";
  const qs = body.questions;
  if (qs === null || typeof qs !== "object" || Array.isArray(qs) || Object.keys(qs).length === 0) {
    return "questions must be an object with at least one question";
  }
  const ids = Object.keys(qs);
  if (ids.length > MAX_QUESTIONS) return `at most ${MAX_QUESTIONS} questions per request, got ${ids.length}`;
  for (const id of ids) {
    const q = qs[id];
    if (!q || !QUESTION_TYPES.has(q.type)) return `question ${JSON.stringify(id)}: type must be noul, choice or score`;
  }
  if (body.images !== undefined) {
    if (!Array.isArray(body.images)) return "images must be an array of base64 strings";
    if (body.images.length > MAX_IMAGES) return `at most ${MAX_IMAGES} images per request, got ${body.images.length}`;
    for (let i = 0; i < body.images.length; i++) {
      if (typeof body.images[i] !== "string" || body.images[i] === "") return `images[${i}] must be a non-empty base64 string`;
      if (toDataUri(body.images[i]) === null) return `images[${i}] is not a PNG, JPEG or WebP image`;
    }
  }
  return null;
}

// pickModel maps the request's model name to a Workers AI model id. No name, or the official SDK's
// default "jev-latest", is whatever DEFAULT_MODEL says.
export function pickModel(name, env) {
  const wanted = !name || name === "jev-latest" ? env.DEFAULT_MODEL || "clef-flash" : name;
  return MODELS[wanted] ? { alias: wanted, id: MODELS[wanted] } : null;
}

// normalize accepts the answers where Workers AI may put them (under "result", or at the top level),
// and fills in an answer's type from its question when the model leaves it out.
export function normalize(raw, questions) {
  const r = raw && typeof raw === "object" ? raw : {};
  const body = r.result && typeof r.result === "object" ? r.result : r;
  const answers = body.answers && typeof body.answers === "object" ? body.answers : null;
  if (!answers || Object.keys(answers).length === 0) return null;
  const out = {};
  for (const [id, a] of Object.entries(answers)) {
    out[id] = { ...a, type: a.type || (questions[id] && questions[id].type) };
  }
  return { answers: out, usage: body.usage || { input_tokens: 0, output_tokens: 0 } };
}

// debugText describes an error for the dev log: its message and any own fields (an AiError keeps its
// detail in fields, not in the message).
function debugText(e) {
  try {
    const own = {};
    for (const k of Object.getOwnPropertyNames(e || {})) own[k] = e[k];
    return `${e && e.message} ${JSON.stringify(own)} ${e && e.cause ? String(e.cause).slice(0, 300) : ""}`.slice(0, 800);
  } catch {
    return "(unprintable)";
  }
}

async function systemOne(request, env) {
  const denied = await authorize(request, env);
  if (denied) return denied;

  const text = await request.text();
  if (text.length > MAX_BODY_BYTES) return fail(413, "request_too_large", "request body exceeds the limit");
  let body;
  try {
    body = JSON.parse(text);
  } catch {
    return fail(400, "invalid_json", "request body must be a JSON object with state, model and questions");
  }
  const problem = validate(body);
  if (problem) return fail(422, "invalid_request", problem);
  const model = pickModel(body.model, env);
  if (!model) {
    return fail(422, "unknown_model", `model must be one of ${Object.keys(MODELS).join(", ")} (or jev-latest for the default)`);
  }

  // `model` is a required field of Clef's input (its schema: `cf ai get-model-schema`).
  const input = { model: model.alias, state: body.state, questions: body.questions };
  if (body.images && body.images.length > 0) input.images = body.images.map(toDataUri);
  let raw;
  try {
    raw = await env.AI.run(model.id, input);
  } catch (e) {
    // The name only: a message can echo the request. DEBUG_ERRORS=1 (a dev setting, never a deploy default)
    // also logs the message, to the Worker's own log and never into the response.
    console.error("workers ai call failed", e && e.name, env.DEBUG_ERRORS === "1" ? debugText(e) : "");
    return fail(502, "upstream_error", "the Workers AI call failed");
  }
  const result = normalize(raw, body.questions);
  if (!result) return fail(502, "upstream_error", "Workers AI returned no answers");
  return json({ model: model.alias, answers: result.answers, usage: result.usage });
}

export default {
  async fetch(request, env) {
    const { pathname } = new URL(request.url);
    if (pathname === "/readyz" && request.method === "GET") return json({ status: "ok" });
    if (pathname === "/v1/systemone") {
      if (request.method !== "POST") return fail(405, "method_not_allowed", "POST only");
      return systemOne(request, env);
    }
    return fail(404, "not_found", "no such endpoint");
  },
};
