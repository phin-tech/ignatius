"""Circuit breaker, response cache and pricing (SPEC 11). Mirrors the Go tests."""

import asyncio

import pytest

from ignatius import (
    Answer, Breaker, BreakerConfig, Cache, CacheConfig, Cached, CallError, Plan, Priced, Question,
    Registry, Request, Tier, WireResponse, load_config, run,
    Limited,
)
from ignatius.backend import SystemOne, Unsupported
from ignatius.types import KIND_CIRCUIT_OPEN, Usage


class Clock:
    def __init__(self):
        self.t = 1000.0

    def __call__(self):
        return self.t

    def advance(self, s):
        self.t += s


class Scripted:
    """A backend whose next result is controlled by the test."""

    def __init__(self, err=None, answers=None, usage=None):
        self.err, self.answers, self.usage = err, answers or {}, usage or Usage()
        self.calls, self.got, self.block = 0, [], None

    async def call(self, request):
        self.calls += 1
        self.got.append(request)
        if self.block is not None:
            await self.block.wait()
        if self.err is not None:
            raise self.err
        out = {q: self.answers.get(q) or Answer("noul", noul=0.9) for q in request.questions}
        return WireResponse(answers=out, usage=self.usage)


def http_err(status):
    return CallError("http", "x", status)


def noul_req():
    return Request("x", {"q": Question("noul", "?")})


async def call_once(b):
    try:
        await b.call(noul_req())
        return None
    except BaseException as e:  # noqa: BLE001
        return e


def is_open(err):
    return isinstance(err, CallError) and err.kind == KIND_CIRCUIT_OPEN


def test_breaker_opens_after_consecutive_failures_and_skips_the_network():
    clk, inner = Clock(), Scripted(http_err(503))
    b = Breaker(inner, BreakerConfig(failure_threshold=3, cooldown_ms=10000), clk)

    async def go():
        for _ in range(3):
            err = await call_once(b)
            assert err is not None and not is_open(err)
        assert b.state == "open"
        before = inner.calls
        assert is_open(await call_once(b)) and inner.calls == before

    asyncio.run(go())


def test_breaker_counts_only_sick_server_signals():
    clk, inner = Clock(), Scripted()
    b = Breaker(inner, BreakerConfig(failure_threshold=3), clk)

    async def go():
        async def run_n(err, n):
            inner.err = err
            for _ in range(n):
                await call_once(b)

        for err in (http_err(400), http_err(404), CallError("unsupported", "x"), asyncio.CancelledError()):
            await run_n(err, 10)
        assert b.state == "closed", "non-countable errors opened the breaker"
        await run_n(http_err(503), 2)
        await run_n(None, 1)  # a success resets the run
        await run_n(http_err(500), 2)
        assert b.state == "closed"
        await run_n(http_err(400), 1)  # a 4xx proves liveness and resets the count
        await run_n(http_err(500), 2)
        assert b.state == "closed"
        for name, e in {"429": http_err(429), "timeout": CallError("timeout", "x"), "transport": CallError("transport", "x"),
                        "decode": CallError("decode", "x"), "other": RuntimeError("boom")}.items():
            b2 = Breaker(Scripted(e), BreakerConfig(failure_threshold=2), clk)
            await call_once(b2)
            await call_once(b2)
            assert b2.state == "open", f"{name} should count as a failure"

    asyncio.run(go())


def test_half_open_admits_exactly_one_probe():
    async def go():
        clk = Clock()
        inner = Scripted(http_err(500))
        b = Breaker(inner, BreakerConfig(failure_threshold=2, cooldown_ms=5000), clk)
        await call_once(b)
        await call_once(b)
        clk.advance(4)
        assert is_open(await call_once(b)), "still cooling down"
        clk.advance(2)
        inner.err, inner.block = None, asyncio.Event()
        probe = asyncio.create_task(call_once(b))
        await asyncio.sleep(0.01)  # the probe is now in flight
        try:  # a broken breaker admits this caller, where it blocks on the held probe
            second = await asyncio.wait_for(call_once(b), timeout=0.5)
        except asyncio.TimeoutError:
            inner.block.set()
            pytest.fail("second caller was admitted while the probe was in flight")
        assert is_open(second), "only one probe at a time"
        inner.block.set()
        assert await probe is None
        assert b.state == "closed"
        assert await call_once(b) is None

    asyncio.run(go())


def test_failed_probe_reopens_and_restarts_cooldown():
    async def go():
        clk, inner = Clock(), Scripted(http_err(503))
        b = Breaker(inner, BreakerConfig(failure_threshold=1, cooldown_ms=5000), clk)
        await call_once(b)
        clk.advance(6)
        err = await call_once(b)  # the probe reaches the backend and fails
        assert err is not None and not is_open(err)
        assert b.state == "open"
        clk.advance(4)
        assert is_open(await call_once(b)), "cooldown must restart after a failed probe"

    asyncio.run(go())


def test_cancelled_probe_does_not_wedge_the_breaker():
    async def go():
        clk, inner = Clock(), Scripted(http_err(500))
        b = Breaker(inner, BreakerConfig(failure_threshold=1, cooldown_ms=1000), clk)
        await call_once(b)
        clk.advance(2)
        inner.err = asyncio.CancelledError()
        await call_once(b)  # the probe is cancelled by its caller: no verdict
        inner.err = None
        assert await call_once(b) is None, "another call should be allowed to probe"
        assert b.state == "closed"

    asyncio.run(go())


def test_open_breaker_makes_a_cascade_skip_the_tier_instantly():
    async def go():
        clk, sick = Clock(), Scripted(http_err(503))
        reg = Registry(cheap=Breaker(sick, BreakerConfig(failure_threshold=2), clk),
                       smart=Scripted(answers={"q": Answer("noul", noul=0.99)}))
        plan = Plan("cascade", tiers=[Tier("cheap"), Tier("smart")])
        for _ in range(2):
            await run(reg, noul_req(), plan)
        before = sick.calls
        r = await run(reg, noul_req(), plan)
        assert sick.calls == before and r.ok and r.answers["q"].model == "smart"
        assert r.trace[0].error.kind == KIND_CIRCUIT_OPEN and r.trace[0].latency_ms <= 5

    asyncio.run(go())


# ---- cache ----------------------------------------------------------------------


def two_q():
    return Request("ticket", {"a": Question("noul", "A?"), "b": Question("noul", "B?")})


def test_cache_serves_per_question_and_merges_misses():
    async def go():
        inner = Scripted(usage=Usage(100, 10))
        c = Cached(inner, Cache(), "m", "jev-latest", priced=True)
        w = await c.call(two_q())
        assert inner.calls == 1 and w.cached == 0 and len(w.answers) == 2
        w = await c.call(two_q())
        assert inner.calls == 1 and w.cached == 2 and len(w.answers) == 2
        assert w.cost_usd == 0.0, "a fully cached call on a priced model costs 0"
        partial = Request("ticket", {"b": Question("noul", "B?"), "c": Question("noul", "C?")})
        w = await c.call(partial)
        assert inner.calls == 2 and list(inner.got[-1].questions) == ["c"]
        assert w.cached == 1 and len(w.answers) == 2

    asyncio.run(go())


def test_cache_key_is_content_not_question_id_or_state():
    async def go():
        inner = Scripted()
        shared = Cache()
        c = Cached(inner, shared, "m", "u")

        async def ask(qid, q, state):
            return await c.call(Request(state, {qid: q}))

        q = Question("choice", "Which?", {"x": "1", "y": "2"})
        await ask("first", q, {"k": "v"})
        assert (await ask("renamed", q, {"k": "v"})).cached == 1, "same content, different id: hit"
        assert (await ask("first", q, {"k": "other"})).cached == 0, "different state: miss"
        assert (await ask("first", Question("choice", "Which one?", q.criteria), {"k": "v"})).cached == 0
        other = Cached(inner, shared, "other-model", "u")
        assert (await other.call(Request({"k": "v"}, {"first": q}))).cached == 0, "different alias: miss"

    asyncio.run(go())


def test_cache_ttl_and_lru():
    async def go():
        clk, inner = Clock(), Scripted()
        c = Cached(inner, Cache(CacheConfig(ttl_ms=1000, max_entries=2), clk), "m")

        def q(s):
            return Request(s, {"q": Question("noul", "?")})

        await c.call(q("one"))
        clk.advance(1.5)
        assert (await c.call(q("one"))).cached == 0, "expired after its TTL"
        await c.call(q("a"))
        await c.call(q("b"))
        await c.call(q("a"))  # touch a: b is now least recently used
        await c.call(q("c"))  # evicts b
        assert len(c.cache) == 2
        assert (await c.call(q("a"))).cached == 1
        assert (await c.call(q("b"))).cached == 0

    asyncio.run(go())


def test_cache_never_stores_failures_or_shares_mutable_answers():
    async def go():
        inner = Scripted(http_err(500))
        c = Cached(inner, Cache(), "m")
        req = Request("s", {"q": Question("choice", "?")})
        with pytest.raises(CallError):
            await c.call(req)
        inner.err = None
        inner.answers = {"q": Answer("choice", choice="a", probabilities={"a": 0.9, "b": 0.1})}
        assert (await c.call(req)).cached == 0 and inner.calls == 2
        hit = await c.call(req)
        hit.answers["q"].probabilities["a"] = 0  # a caller mutating its copy...
        again = await c.call(req)
        assert again.answers["q"].probabilities["a"] == 0.9, "...must not corrupt the cached entry"

    asyncio.run(go())


# ---- pricing and wiring -----------------------------------------------------------


def test_pricing_and_run_totals():
    use = Usage(1_000_000, 500_000)
    reg = Registry(
        jev=Priced(Scripted(usage=use), 0.042, 0),
        clef=Priced(Scripted(usage=Usage(2_000_000, 0)), 0.5),
        free=Scripted(usage=use),  # unpriced
        broke=Priced(Scripted(http_err(500)), 9),
    )
    r = asyncio.run(run(reg, noul_req(), Plan("fan_out", models=["jev", "clef", "free", "broke"])))
    by = {x.model: x.cost_usd for x in r.results}
    assert by["jev"] == pytest.approx(0.042) and by["clef"] == pytest.approx(1.0) and by["free"] is None
    assert r.cost_usd == pytest.approx(1.042), "failures and unpriced results add nothing"
    r = asyncio.run(run(reg, noul_req(), Plan("single", model="free")))
    assert r.cost_usd is None


def test_load_config_builds_the_decorator_stack(tmp_path):
    p = tmp_path / "c.toml"
    p.write_text('''
[cache]
enabled = true

[models.full]
provider = "systemone"
base_url = "http://x"
price_input_per_mtok = 0.042

[models.nobreak]
provider = "systemone"
base_url = "http://x"
[models.nobreak.breaker]
enabled = false

[models.optout]
provider = "systemone"
base_url = "http://x"
cache = false

[models.tuned]
provider = "systemone"
base_url = "http://x"
[models.tuned.breaker]
failure_threshold = 9
cooldown_ms = 1234

[models.gpt]
provider = "openai"
''')
    reg = load_config(str(p)).registry
    assert isinstance(reg["full"], Limited), "every systemone model is wrapped in Limited"
    full = reg["full"].inner  # then: cache, breaker, pricing, the backend
    assert isinstance(full, Cached) and full.priced and isinstance(full.inner, Breaker)
    assert isinstance(full.inner.inner, Priced) and isinstance(full.inner.inner.inner, SystemOne)
    assert isinstance(reg["nobreak"].inner, Cached) and isinstance(reg["nobreak"].inner.inner, SystemOne)
    assert isinstance(reg["optout"].inner, Breaker), "cache=false overrides the global setting"
    assert isinstance(reg["gpt"], Unsupported)
    t = reg["tuned"].inner.inner
    assert t.threshold == 9 and t.cooldown == pytest.approx(1.234)
    assert reg["full"].inner.cache is reg["tuned"].inner.cache, "cached models share one LRU"
