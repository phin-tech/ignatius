"""SPEC 3.1: images and per-model limits. The shared fixtures (images_cascade.json, limits_split.json)
cover the cascade and the split against the Go library; these cover the rest."""

import asyncio
import json

import httpx
import pytest

from ignatius import (Answer, CallError, Limited, LimitsConfig, Plan, Question, Registry, Request, SystemOne, WireResponse,
                      load_config, run)
from ignatius.resilience import Cache, CacheConfig, Cached, hash_images, question_key
from ignatius.types import KIND_UNSUPPORTED, Usage


class Recorder:
    def __init__(self, cost=0.0, fail=""):
        self.calls, self.cost, self.fail = [], cost, fail

    async def call(self, request):
        self.calls.append(request)
        out = WireResponse(model="rec", usage=Usage(10, 1), cost_usd=self.cost or None)
        for qid in request.questions:
            if qid == self.fail:
                raise CallError("http", "503: boom", 503)
            out.answers[qid] = Answer(type="noul", noul=0.9)
        return out


def qs(n):
    return {f"q{i:03d}": Question("noul", "?") for i in range(n)}


@pytest.mark.parametrize("limits,images,ok,msg", [
    (LimitsConfig(), [], True, ""),
    (LimitsConfig(), ["aGk="], False, "takes no images"),
    (LimitsConfig(max_images=4), ["aGk=", "aGk="], True, ""),
    (LimitsConfig(max_images=2), ["aGk=", "aGk="], True, ""),
    (LimitsConfig(max_images=1), ["aGk=", "aGk="], False, "at most 1 images"),
    (LimitsConfig(max_questions=5), ["aGk="], False, "takes no images"),
])
def test_limited_refuses_images_the_model_cannot_take(limits, images, ok, msg):
    inner = Recorder()
    call = Limited(inner, limits).call(Request("x", qs(1), images))
    if ok:
        asyncio.run(call)
        return
    with pytest.raises(CallError) as e:
        asyncio.run(call)
    assert e.value.kind == KIND_UNSUPPORTED and msg in e.value.message
    assert inner.calls == [], "an unsupported request must not reach the model"


def test_limited_splits_too_many_questions_and_merges():
    inner = Recorder(cost=0.5)
    w = asyncio.run(Limited(inner, LimitsConfig(max_questions=4, max_images=1)).call(Request("s", qs(10), ["aGk="])))
    assert sorted(len(c.questions) for c in inner.calls) == [2, 4, 4]
    assert all(c.state == "s" and c.images == ["aGk="] for c in inner.calls)
    assert len(w.answers) == 10 and w.usage.input_tokens == 30 and w.usage.output_tokens == 3
    assert w.cost_usd == 1.5


def test_limited_is_all_or_nothing_and_leaves_small_requests_alone():
    with pytest.raises(CallError) as e:
        asyncio.run(Limited(Recorder(fail="q005"), LimitsConfig(max_questions=4)).call(Request("s", qs(10))))
    assert e.value.kind == "http"
    inner = Recorder()
    asyncio.run(Limited(inner, LimitsConfig(max_questions=4)).call(Request("s", qs(4))))
    assert len(inner.calls) == 1


def test_images_reach_the_wire_only_when_there_are_some():
    bodies = []

    def handler(request: httpx.Request) -> httpx.Response:
        bodies.append(json.loads(request.content))
        return httpx.Response(200, json={"model": "clef", "answers": {"q000": {"type": "noul", "noul": 0.9}}, "usage": {}})

    client = httpx.AsyncClient(transport=httpx.MockTransport(handler))
    backend = Limited(SystemOne("http://x", "clef", client=client), LimitsConfig(max_images=4, max_questions=64))
    asyncio.run(backend.call(Request("x", qs(1), ["aGk=", "d29ybGQ="])))
    asyncio.run(backend.call(Request("x", qs(1))))
    assert bodies[0]["images"] == ["aGk=", "d29ybGQ="] and bodies[0]["model"] == "clef"
    assert "images" not in bodies[1]


def test_the_cache_key_includes_the_images():
    q = Question("noul", "is it a cat?")
    key = lambda imgs: question_key("m", "u", "x", q, hash_images(imgs))  # noqa: E731
    assert key(["AAA"]) == key(["AAA"])
    assert len({key([]), key(["AAA"]), key(["BBB"]), key(["AAA", "BBB"]), key(["BBB", "AAA"])}) == 5
    assert hash_images([]) == ""


def test_the_cache_hands_images_to_the_backend_on_a_miss_and_keys_on_them():
    inner = Recorder()
    c = Cached(inner, Cache(CacheConfig(enabled=True, ttl_ms=60000, max_entries=100)), "m", "u")
    req = Request("x", qs(1), ["AAA"])
    asyncio.run(c.call(req))
    assert len(inner.calls) == 1 and inner.calls[0].images == ["AAA"]
    asyncio.run(c.call(req))
    assert len(inner.calls) == 1, "the same question and image is a hit"
    asyncio.run(c.call(Request("x", qs(1), ["BBB"])))
    assert len(inner.calls) == 2, "another picture is a miss"


def test_a_cascade_skips_a_tier_without_images(tmp_path):
    text_only, vision = Recorder(), Recorder()
    reg = Registry({"jev": Limited(text_only), "clef": Limited(vision, LimitsConfig(max_images=4))})
    plan = Plan.from_dict({"mode": "cascade", "tiers": [{"model": "jev", "threshold": 0.5}, "clef"]})
    routed = asyncio.run(run(reg, Request("x", qs(2), ["aGk="]), plan))
    assert routed.ok and not text_only.calls and len(vision.calls) == 1 and vision.calls[0].images == ["aGk="]
    assert [f.error.kind for f in routed.failures] == [KIND_UNSUPPORTED]
    asyncio.run(run(reg, Request("x", qs(2)), plan))
    assert len(text_only.calls) == 1, "text-only requests are unchanged"


def test_validation_and_wire_round_trip():
    with pytest.raises(ValueError, match=r"images\[1\]"):
        Request("x", qs(1), ["aGk=", ""]).validate()
    Request("x", qs(1), ["aGk="]).validate()
    r = Request("x", qs(1), ["aGk="])
    assert Request.from_dict(r.to_dict()).images == ["aGk="] and "images" not in Request("x", qs(1)).to_dict()


def test_config_reads_limits_and_rejects_negative(tmp_path):
    ok = tmp_path / "o.toml"
    ok.write_text('[models.clef]\nprovider = "systemone"\nbase_url = "http://x"\n[models.clef.limits]\nmax_questions = 64\nmax_images = 4\n')
    backend = load_config(str(ok)).registry["clef"]
    assert isinstance(backend, Limited) and backend.limits == LimitsConfig(64, 4)
    bad = tmp_path / "b.toml"
    bad.write_text('[models.clef]\nprovider = "systemone"\nbase_url = "http://x"\n[models.clef.limits]\nmax_images = -1\n')
    with pytest.raises(ValueError, match="limits cannot be negative"):
        load_config(str(bad))


def test_gateway_client_systemone_sends_images_only_when_given():
    from ignatius import GatewayClient

    sent = []

    class Transport(httpx.AsyncBaseTransport):
        async def handle_async_request(self, request):
            sent.append(json.loads(request.content))
            return httpx.Response(200, json={"answers": {}})

    real = httpx.AsyncClient

    class Patched(real):
        def __init__(self, *a, **kw):
            super().__init__(*a, transport=Transport(), **kw)

    import ignatius.client as client_mod

    old = client_mod.httpx.AsyncClient
    client_mod.httpx.AsyncClient = Patched
    try:
        c = GatewayClient("http://gw")
        asyncio.run(c.systemone("s", {"q": {"type": "noul", "instructions": "?"}}, "clef", images=["aGk="]))
        asyncio.run(c.systemone("s", {"q": {"type": "noul", "instructions": "?"}}, "jev"))
    finally:
        client_mod.httpx.AsyncClient = old
    assert sent[0]["images"] == ["aGk="] and sent[0]["model"] == "clef"
    assert "images" not in sent[1]
