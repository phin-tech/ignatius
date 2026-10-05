import assert from "node:assert/strict";
import { test } from "node:test";
import worker, { normalize, pickModel, toDataUri } from "../src/index.js";

const KEY = "k-secret";
const questions = {
  dept: { type: "choice", instructions: "?", criteria: { a: "x", b: "y" } },
  urgent: { type: "noul", instructions: "?" },
};

function env(overrides = {}) {
  const calls = [];
  const e = {
    WORKER_API_KEY: KEY,
    DEFAULT_MODEL: "clef-flash",
    calls,
    AI: {
      async run(id, input) {
        calls.push({ id, input });
        return {
          answers: {
            dept: { choice: "a", probabilities: { a: 0.9, b: 0.1 }, confidence: 0.8 },
            urgent: { noul: 0.7 },
          },
          usage: { input_tokens: 12, output_tokens: 0 },
        };
      },
    },
    ...overrides,
  };
  return e;
}

const call = (e, body, { auth = `Bearer ${KEY}`, method = "POST", path = "/v1/systemone" } = {}) =>
  worker.fetch(
    new Request(`https://w.test${path}`, {
      method,
      headers: auth ? { authorization: auth, "content-type": "application/json" } : {},
      body: method === "POST" ? (typeof body === "string" ? body : JSON.stringify(body)) : undefined,
    }),
    e,
  );

test("answers in the Jev shape, naming the model and filling in the types", async () => {
  const e = env();
  const res = await call(e, { state: "ticket", model: "clef", questions });
  assert.equal(res.status, 200);
  const out = await res.json();
  assert.equal(out.model, "clef");
  assert.equal(out.answers.dept.type, "choice");
  assert.equal(out.answers.dept.choice, "a");
  assert.equal(out.answers.urgent.type, "noul");
  assert.equal(out.usage.input_tokens, 12);
  assert.deepEqual(e.calls[0], { id: "@cf/cloudflare/clef", input: { model: "clef", state: "ticket", questions } });
});

test("images are forwarded only when there are some", async () => {
  const e = env();
  const png = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3, 4]).toString("base64");
  const jpg = Buffer.from([0xff, 0xd8, 0xff, 0xe0, 0, 0x10, 0x4a, 0x46]).toString("base64");
  await call(e, { state: "s", images: [png, jpg], questions });
  await call(e, { state: "s", questions });
  // Workers AI takes data URIs, so raw base64 is converted, labeled by what the bytes are.
  assert.deepEqual(e.calls[0].input.images, [`data:image/png;base64,${png}`, `data:image/jpeg;base64,${jpg}`]);
  assert.equal("images" in e.calls[1].input, false);
});

test("no model, and the SDK's jev-latest, use DEFAULT_MODEL; an unknown one is refused", async () => {
  const e = env({ DEFAULT_MODEL: "clef" });
  await call(e, { state: "s", questions });
  await call(e, { state: "s", model: "jev-latest", questions });
  assert.deepEqual(e.calls.map((c) => c.id), ["@cf/cloudflare/clef", "@cf/cloudflare/clef"]);
  const res = await call(e, { state: "s", model: "gpt-9", questions });
  assert.equal(res.status, 422);
  assert.equal((await res.json()).error.type, "unknown_model");
  assert.equal(e.calls.length, 2, "an unknown model must not reach Workers AI");
  assert.equal(pickModel("clef-flash", env()).id, "@cf/cloudflare/clef-flash");
});

test("auth: 403 without a credential, 401 with a wrong one, rotation through comma-separated keys", async () => {
  const e = env({ WORKER_API_KEY: "old, new" });
  assert.equal((await call(e, { state: "s", questions }, { auth: "" })).status, 403);
  assert.equal((await call(e, { state: "s", questions }, { auth: "Basic abc" })).status, 403);
  assert.equal((await call(e, { state: "s", questions }, { auth: "Bearer nope" })).status, 401);
  assert.equal((await call(e, { state: "s", questions }, { auth: "Bearer old" })).status, 200);
  assert.equal((await call(e, { state: "s", questions }, { auth: "Bearer new" })).status, 200);
  assert.equal(e.calls.length, 2, "only authorized requests reach Workers AI");
});

test("with no key configured the Worker refuses everything", async () => {
  for (const key of [undefined, "", "  , "]) {
    const e = env({ WORKER_API_KEY: key });
    const res = await call(e, { state: "s", questions });
    assert.equal(res.status, 503);
    assert.equal(e.calls.length, 0);
  }
});

test("validation: bad JSON, no questions, too many questions or images, empty image, bad type", async () => {
  const e = env();
  const status = async (b) => (await call(e, b)).status;
  assert.equal(await status("{not json"), 400);
  assert.equal(await status({ state: "s" }), 422);
  assert.equal(await status({ state: "s", questions: {} }), 422);
  assert.equal(await status({ state: "s", questions: { q: { type: "essay" } } }), 422);
  const many = Object.fromEntries(Array.from({ length: 65 }, (_, i) => [`q${i}`, { type: "noul", instructions: "?" }]));
  assert.equal(await status({ state: "s", questions: many }), 422);
  assert.equal(await status({ state: "s", images: Array(5).fill("aGk="), questions }), 422);
  assert.equal(await status({ state: "s", images: ["aGk=", ""], questions }), 422);
  assert.equal(await status({ state: "s", images: ["aGk="], questions }), 422, "not a PNG, JPEG or WebP");
  assert.equal(await status({ state: "s", images: "aGk=", questions }), 422);
  assert.equal(e.calls.length, 0, "an invalid request must not reach Workers AI");
});

test("an upstream failure is a 502 that echoes neither the request nor the error text", async () => {
  const e = env({
    AI: { async run() { throw Object.assign(new Error("SECRET-IN-ERROR echo of the state"), { name: "AiError" }); } },
  });
  const res = await call(e, { state: "SECRET-STATE", questions });
  const text = await res.text();
  assert.equal(res.status, 502);
  assert.ok(!text.includes("SECRET"), text);
  const empty = env({ AI: { async run() { return { answers: {} }; } } });
  assert.equal((await call(empty, { state: "s", questions })).status, 502);
});

test("normalize accepts answers under result or at the top level", () => {
  const q = { n: { type: "noul" } };
  assert.equal(normalize({ result: { answers: { n: { noul: 0.9 } } } }, q).answers.n.type, "noul");
  assert.equal(normalize({ answers: { n: { noul: 0.9, type: "noul" } } }, q).answers.n.noul, 0.9);
  assert.equal(normalize({}, q), null);
  assert.equal(normalize(null, q), null);
});

test("readyz needs no auth and the other paths are refused", async () => {
  const e = env();
  assert.equal((await call(e, undefined, { auth: "", method: "GET", path: "/readyz" })).status, 200);
  assert.equal((await call(e, undefined, { method: "GET", path: "/v1/systemone" })).status, 405);
  assert.equal((await call(e, undefined, { method: "GET", path: "/nope" })).status, 404);
  assert.equal(e.calls.length, 0);
});

test("toDataUri labels PNG, JPEG and WebP by their bytes, passes data URIs through, rejects the rest", () => {
  const b64 = (bytes) => Buffer.from(bytes).toString("base64");
  const webp = b64([...Buffer.from("RIFF"), 0, 0, 0, 0, ...Buffer.from("WEBP"), 1, 2]);
  assert.match(toDataUri(webp), /^data:image\/webp;base64,/);
  assert.equal(toDataUri("data:image/gif;base64,AAAA"), "data:image/gif;base64,AAAA", "already a data URI");
  assert.equal(toDataUri(b64([0x47, 0x49, 0x46, 0x38, 0x39, 0x61, 0, 0, 0, 0, 0, 0])), null, "GIF is not supported");
  assert.equal(toDataUri("***not base64***"), null);
  assert.equal(toDataUri(""), null);
});
