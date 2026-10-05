import asyncio
import json

import httpx
import pytest

from ignatius import (
    Answer, CallError, Config, Ignatius, NotInline, Plan, PlanError, Question, Registry, Request,
    ResolveError, Router, SystemOne, Tier, Unsupported, WireResponse, load_config, parse_inline, run,
)
from ignatius.reduce import reduce
from ignatius.types import Result


def noul_req() -> Request:
    return Request("x", {"q": Question("noul", "?")})


def mock_client(handler) -> httpx.AsyncClient:
    return httpx.AsyncClient(transport=httpx.MockTransport(handler))


class Fixed:
    """A backend returning fixed answers, counting calls."""

    def __init__(self, **answers: Answer):
        self.answers, self.calls = answers, 0

    async def call(self, request):
        self.calls += 1
        return WireResponse(answers={k: Answer.from_dict(v.to_dict()) for k, v in self.answers.items()})


def noul(p: float) -> Answer:
    return Answer("noul", noul=p)


def test_systemone_request_shape_and_normalization():
    seen = {}

    def handler(req: httpx.Request) -> httpx.Response:
        seen["url"], seen["auth"], seen["body"] = str(req.url), req.headers.get("authorization"), json.loads(req.content)
        return httpx.Response(200, json={
            "model": "decis/x", "answers": {"q": {"type": "noul", "noul": 0.9}},
            "usage": {"input_tokens": 7, "output_tokens": 3}, "decis": {"ignored": True}})

    reg = Registry(jeff=SystemOne("http://up/", "jev-latest", "k", client=mock_client(handler)))
    r = asyncio.run(run(reg, noul_req(), Plan("single", model="jeff")))
    assert r.ok and seen["url"] == "http://up/v1/systemone" and seen["auth"] == "Bearer k"
    assert seen["body"]["model"] == "jev-latest" and seen["body"]["state"] == "x"
    a = r.answers["q"]
    assert a.model == "jeff" and a.confidence == pytest.approx(0.8)
    assert r.results[0].usage.input_tokens == 7


def test_custom_auth_header():
    seen = {}

    def handler(req):
        seen.update(req.headers)
        return httpx.Response(200, json={"answers": {}})

    reg = Registry(a=SystemOne("http://up", api_key="secret", auth_header="X-Api-Key", client=mock_client(handler)))
    asyncio.run(run(reg, noul_req(), Plan("single", model="a")))
    assert seen["x-api-key"] == "secret" and "authorization" not in seen


def test_failure_kinds():
    def make(handler):
        return SystemOne("http://up", client=mock_client(handler))

    def boom(exc):
        def h(req):
            raise exc
        return h

    reg = Registry(
        http=make(lambda r: httpx.Response(503, text="boom")),
        decode=make(lambda r: httpx.Response(200, text="not json")),
        timeout=make(boom(httpx.ReadTimeout("slow"))),
        transport=make(boom(httpx.ConnectError("refused"))),
        openai=Unsupported("openai"),
    )
    want = {"http": "http", "decode": "decode", "timeout": "timeout", "transport": "transport", "openai": "unsupported"}
    for alias, kind in want.items():
        r = asyncio.run(run(reg, noul_req(), Plan("single", model=alias)))
        assert not r.ok and r.failures[0].error.kind == kind, (alias, r.failures)


def test_plan_validation():
    reg = Registry(a=Unsupported(""), b=Unsupported(""))
    bad = [
        Plan("nope"), Plan("single"), Plan("single", model="zzz"), Plan("fan_out"),
        Plan("fan_out", models=["a", "a"]), Plan("fan_out", models=["a"], reduce="median"),
        Plan("cascade"), Plan("cascade", tiers=[Tier("a"), Tier("zzz")]),
    ]
    for p in bad:
        with pytest.raises(PlanError):
            asyncio.run(run(reg, noul_req(), p))


def test_plan_json_shapes():
    p = Plan.from_dict({"mode": "cascade", "threshold": 0.7,
                        "tiers": ["a", {"model": "b", "threshold": {"noul": 0.5}}, {"model": "c", "threshold": 0.9}]})
    assert p.tiers[0].model == "a" and p.tiers[1].threshold.by_type == {"noul": 0.5} and p.tiers[2].threshold.default == 0.9
    assert p.tiers[1].bar("choice", p) == 0.7  # missing type falls back to the cascade-level threshold
    assert Tier("x").bar("noul", Plan("cascade")) == 0.8
    assert Plan.from_dict(p.to_dict()).to_dict() == p.to_dict()


def test_escalate_if_overrides_confidence():
    reg = Registry(a=Fixed(q=noul(0.99)), b=Fixed(q=noul(0.4)))
    plan = Plan("cascade", tiers=[Tier("a"), Tier("b")])
    r = asyncio.run(run(reg, noul_req(), plan, escalate_if=lambda qid, a: True))
    assert len(r.trace) == 2 and r.answers["q"].model == "a"  # a is still more confident: best-seen keeps it


def test_cascade_stops_when_everything_settled():
    second = Fixed(q=noul(0.5))
    reg = Registry(a=Fixed(q=noul(0.99)), b=second)
    r = asyncio.run(run(reg, noul_req(), Plan("cascade", tiers=[Tier("a"), Tier("b")])))
    assert len(r.trace) == 1 and second.calls == 0 and r.ok


def test_vote_tie_and_mean():
    def res(choice, pa, pb):
        return Result("m", {"c": Answer("choice", choice=choice, model="m", probabilities={"a": pa, "b": pb})})

    qs = {"c": Question("choice")}
    assert reduce("vote", qs, [res("a", 0.55, 0.45), res("b", 0.1, 0.9)])["c"].choice == "b"
    assert reduce("mean", qs, [res("a", 0.9, 0.1), res("b", 0.4, 0.6), res("b", 0.4, 0.6)])["c"].choice == "a"


def test_plan_timeout_bounds_the_whole_run():
    class Slow:
        async def call(self, request):
            await asyncio.sleep(0.5)
            return WireResponse()

    r = asyncio.run(run(Registry(a=Slow()), noul_req(), Plan("single", model="a", timeout_ms=40)))
    assert not r.ok and r.failures[0].error.kind == "timeout"


def test_fan_out_runs_concurrently_and_keeps_given_order():
    class Delayed:
        def __init__(self, ms, p):
            self.ms, self.p = ms, p

        async def call(self, request):
            await asyncio.sleep(self.ms / 1000)
            return WireResponse(answers={"q": noul(self.p)})

    reg = Registry(slow=Delayed(150, 0.9), fast=Delayed(5, 0.2))
    import time
    t0 = time.perf_counter()
    r = asyncio.run(run(reg, noul_req(), Plan("fan_out", models=["slow", "fast"])))
    assert time.perf_counter() - t0 < 0.28  # concurrent, not 155ms + serial overhead
    assert [x.model for x in r.results] == ["slow", "fast"] and r.ok and r.answers == {}


def test_parse_inline():
    p = parse_inline("fan-out:jeff,jev|mean", 5)
    assert (p.mode, p.models, p.reduce) == ("fan_out", ["jeff", "jev"], "mean")
    assert parse_inline("fan_out:a,b").reduce == "vote"
    p = parse_inline("cascade:jeff@0.5>clef-flash>jev", 5)
    assert [t.model for t in p.tiers] == ["jeff", "clef-flash", "jev"] and p.tiers[0].threshold.default == 0.5
    assert p.tiers[1].threshold is None
    with pytest.raises(NotInline):
        parse_inline("jeff-gemma")
    for bad in ["fan-out:", "fan-out:a,,b", "cascade:a@x>b", "cascade:a@2>b", "vote:a,b", "fan-out:a,b,c,d,e,f"]:
        with pytest.raises(ValueError) as e:
            parse_inline(bad, 5)
        assert not isinstance(e.value, NotInline), bad


def test_request_validate():
    with pytest.raises(ValueError):
        Request("x", {}).validate()
    with pytest.raises(ValueError):
        Request("x", {"q": Question("rating")}).validate()
    noul_req().validate()


CONFIG = """
default_route = "smart"
allow_inline_routes = false

[models.cheap]
provider = "systemone"
base_url = "http://cheap:8000"
model = "jev-latest"
api_key_env = "CHEAP_KEY"
auth_header = "X-Api-Key"
timeout_ms = 3000

[models.big]
provider = "systemone"
base_url = "http://big:8000"

[models.gpt]
provider = "openai"

[routes.smart]
mode = "cascade"
threshold = 1
tiers = [ { model = "cheap", threshold = { noul = 0.5, choice = 0.9 } }, "big" ]

[routes.vote]
mode = "fan_out"
models = ["cheap", "big"]
reduce = "vote"
"""


def test_load_config_toml_and_router(tmp_path):
    p = tmp_path / "ignatius.toml"
    p.write_text(CONFIG)
    cfg = load_config(str(p), {"CHEAP_KEY": "secret"})
    cheap = cfg.registry["cheap"].inner.inner  # Limited, then the circuit breaker (on by default), then the backend
    assert isinstance(cheap, SystemOne) and cheap.api_key == "secret" and cheap.auth_header == "X-Api-Key"
    assert cheap.timeout_s == 3.0 and isinstance(cfg.registry["gpt"], Unsupported)
    smart = cfg.routes["smart"]  # mixed-type tiers and thresholds, same TOML the Go gateway reads
    assert smart.tiers[1].model == "big" and smart.tiers[0].threshold.by_type["choice"] == 0.9 and smart.threshold.default == 1

    rt = Router(cfg)
    assert rt.resolve("").mode == "cascade" and rt.resolve("jev-latest").tiers[0].model == "cheap"  # default route
    assert rt.resolve("vote").mode == "fan_out" and rt.resolve("big").mode == "single"
    with pytest.raises(ResolveError) as e:  # inline is off in this config
        rt.resolve("fan-out:cheap,big")
    assert e.value.models == ["big", "cheap", "gpt"] and e.value.routes == ["smart", "vote"] and e.value.grammar == ""


def test_config_validation(tmp_path):
    p = tmp_path / "bad.toml"
    p.write_text('[models.a]\nprovider="systemone"\nbase_url="http://x"\n[routes.r]\nmode="cascade"\ntiers=["a","ghost"]\n')
    with pytest.raises(ValueError, match="ghost"):
        load_config(str(p))
    p.write_text('default_route="ghost"\n[models.a]\nprovider="systemone"\nbase_url="http://x"\n')
    with pytest.raises(ValueError, match="default_route"):
        load_config(str(p))


def test_ignatius_facade_routes_by_model_string():
    cfg = Config(Registry(cheap=Fixed(q=noul(0.55)), smart=Fixed(q=noul(0.99))), default_route="cheap")
    ig = Ignatius(cfg)
    r = ig.run_sync(noul_req(), model="cascade:cheap>smart")
    assert [h.model for h in r.trace] == ["cheap", "smart"] and r.answers["q"].model == "smart"
    assert ig.run_sync(noul_req()).trace == []  # default route = single alias
    with pytest.raises(ValueError):
        ig.run_sync(noul_req(), model="cheap", plan=Plan("single", model="cheap"))
