"""The Workers AI provider (hosted Clef). Unverified against Cloudflare itself; see WorkersAI."""

import asyncio
import json

import httpx
import pytest

from ignatius import CallError, Limited, Question, Request, WorkersAI, load_config
from ignatius.types import KIND_DECODE, KIND_HTTP

PNG = "iVBORw0KGgoAAAAA"  # a PNG signature plus a few bytes: enough for the format to be recognized
REQ = Request("ticket", {"c": Question("choice", "?", {"a": "x", "b": "y"}), "n": Question("noul", "?")}, [PNG])


def backend(status, body, seen):
    def handler(request: httpx.Request) -> httpx.Response:
        seen.update(path=request.url.path, auth=request.headers.get("authorization"), body=json.loads(request.content))
        return httpx.Response(status, content=body if isinstance(body, bytes) else json.dumps(body).encode())

    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    return WorkersAI("ACC123", "@cf/cloudflare/clef-flash", "tok", "https://cf.test/client/v4", client=client)


def test_addresses_the_model_in_the_url_and_sends_what_clef_takes():
    seen = {}
    b = backend(200, {"result": {"answers": {
        "c": {"type": "choice", "choice": "a", "probabilities": {"a": 0.9, "b": 0.1}, "confidence": 0.8},
        "n": {"type": "noul", "noul": 0.7}}, "usage": {"input_tokens": 12, "output_tokens": 0}},
        "success": True, "errors": [], "messages": []}, seen)
    w = asyncio.run(b.call(REQ))
    assert seen["path"] == "/client/v4/accounts/ACC123/ai/run/@cf/cloudflare/clef-flash" and seen["auth"] == "Bearer tok"
    # Clef's input schema requires `model` as well as the URL, and images as data URLs.
    assert seen["body"]["model"] == "clef-flash" and seen["body"]["state"] == "ticket"
    assert seen["body"]["images"] == [f"data:image/png;base64,{PNG}"]
    assert w.answers["c"].choice == "a" and w.answers["n"].noul == 0.7 and w.usage.input_tokens == 12
    asyncio.run(b.call(Request("t", REQ.questions)))
    assert "images" not in seen["body"]


@pytest.mark.parametrize("status,body,kind", [
    (200, {"answers": {"n": {"noul": 0.9}}}, None),
    (200, {"result": {"answers": {"n": {"noul": 0.9}}}, "success": True}, None),
    (200, {"result": None, "success": False, "errors": [{"code": 7003, "message": "SECRET-DETAIL"}]}, KIND_HTTP),
    (200, {"result": {}, "success": True}, KIND_DECODE),
    (200, b"<html>", KIND_DECODE),
    (401, {"success": False, "errors": [{"code": 10000}]}, KIND_HTTP),
    (429, {}, KIND_HTTP),
])
def test_decodes_tolerantly_and_fails_by_kind(status, body, kind):
    b = backend(status, body, {})
    if kind is None:
        w = asyncio.run(b.call(REQ))
        assert w.answers["n"].type == "noul" and w.answers["n"].noul == 0.9  # the type came from the question
        return
    with pytest.raises(CallError) as e:
        asyncio.run(b.call(REQ))
    assert e.value.kind == kind and "SECRET-DETAIL" not in e.value.message
    if status == 401:
        assert e.value.status == 401


def test_config_builds_workers_ai_with_limits(tmp_path):
    cfg = tmp_path / "g.toml"
    cfg.write_text('''[models.clef]
provider = "workers-ai"
model = "@cf/cloudflare/clef-flash"
account_id_env = "CF_ACCOUNT"
api_key_env = "CF_TOKEN"
[models.clef.limits]
max_images = 4
max_questions = 64
''')
    reg = load_config(str(cfg), {"CF_ACCOUNT": "ACC9", "CF_TOKEN": "tok9"}).registry
    inner = reg["clef"].inner  # Limited, then the circuit breaker, then the backend
    while not isinstance(inner, WorkersAI):
        inner = inner.inner
    assert isinstance(reg["clef"], Limited) and inner.account_id == "ACC9" and inner.api_key == "tok9"
    for text, env, msg in [
        ('[models.m]\nprovider = "workers-ai"\naccount_id_env = "A"\napi_key_env = "T"\n', {"A": "x", "T": "y"}, "needs model"),
        ('[models.m]\nprovider = "workers-ai"\nmodel = "@cf/x"\napi_key_env = "T"\n', {"T": "y"}, "account_id_env"),
        ('[models.m]\nprovider = "workers-ai"\nmodel = "@cf/x"\naccount_id_env = "A"\napi_key_env = "T"\n', {"T": "y"}, "A is not set"),
    ]:
        bad = tmp_path / "b.toml"
        bad.write_text(text)
        with pytest.raises(ValueError, match=msg):
            load_config(str(bad), env)


def test_the_example_configs_in_deploy_load():
    """The Clef example is documentation people copy: it must keep loading in the SDK too."""
    from pathlib import Path

    path = Path(__file__).resolve().parents[2] / "deploy" / "clef" / "ignatius.toml"
    env = {"CLOUDFLARE_ACCOUNT_ID": "acc", "CLOUDFLARE_API_TOKEN": "tok", "TYPESAFE_API_KEY": "k"}
    cfg = load_config(str(path), env)
    assert sorted(cfg.registry) == ["clef", "clef-flash", "jev"] and list(cfg.routes) == ["triage"]
    assert cfg.registry["jev"].limits.max_images == 0, "no [limits] means no images"
    assert cfg.registry["clef-flash"].limits.max_images == 4


def test_the_confidence_policy_applies_to_workers_ai_too(tmp_path):
    from ignatius import Plan, Registry, run

    seen = {}
    b = backend(200, {"result": {"answers": {"c": {"type": "choice", "choice": "a",
                      "probabilities": {"a": 0.6, "b": 0.4}, "confidence": 0.99}}}, "success": True}, seen)
    from ignatius import WithConfidence

    reg = Registry({"clef": WithConfidence(b, "derived")})
    req = Request("x", {"c": Question("choice", "?", ["a", "b"])})
    routed = asyncio.run(run(reg, req, Plan.from_dict({"mode": "single", "model": "clef"})))
    a = routed.answers["c"]
    assert abs(a.confidence - 0.2) < 1e-9 and a.native_confidence == 0.99

    cfg = tmp_path / "g.toml"
    cfg.write_text('[models.clef]\nprovider = "workers-ai"\nmodel = "@cf/x"\naccount_id_env = "A"\napi_key_env = "T"\nconfidence = "derived"\n')
    backend_ = load_config(str(cfg), {"A": "a", "T": "t"}).registry["clef"]
    while not isinstance(backend_, WithConfidence):
        backend_ = backend_.inner
    assert isinstance(backend_.inner, WorkersAI)


def test_workers_image_labels_by_the_bytes_and_refuses_what_clef_cannot_read():
    import base64 as b64

    from ignatius.backend import workers_image

    enc = lambda raw: b64.b64encode(raw).decode()  # noqa: E731
    png, jpg = enc(b"\x89PNG\r\n\x1a\n\x00\x00\x00\x00"), enc(b"\xff\xd8\xff\xe0\x00\x10JFIF")
    webp, gif = enc(b"RIFF\x00\x00\x00\x00WEBPVP8 "), enc(b"GIF89a\x00\x00\x00\x00\x00\x00")
    assert workers_image(png) == f"data:image/png;base64,{png}"
    assert workers_image(jpg) == f"data:image/jpeg;base64,{jpg}"
    assert workers_image(webp) == f"data:image/webp;base64,{webp}"
    assert workers_image("DATA:image/gif;base64,AAAA") == "DATA:image/gif;base64,AAAA"
    assert workers_image(png[:8] + "\n" + png[8:]) == f"data:image/png;base64,{png}"
    for bad in (gif, "***not base64***", "aGk=", ""):
        assert workers_image(bad) is None

    seen = {}
    b = backend(200, {"answers": {"n": {"noul": 0.9}}}, seen)
    with pytest.raises(CallError) as e:
        asyncio.run(b.call(Request("x", REQ.questions, [png, gif])))
    assert e.value.kind == "unsupported" and "images[1]" in e.value.message
    assert not seen, "nothing is sent when an image is refused"
