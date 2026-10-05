import pytest

import prices

TABLE = {
    "claude-sonnet-5-5": {"input_cost_per_token": 2e-06, "output_cost_per_token": 1e-05},
    "openrouter/some/model": {"input_cost_per_token": 3e-09, "output_cost_per_token": 2.4e-06},
    "fireworks_ai/accounts/fireworks/models/m": {"input_cost_per_token": 1e-07, "output_cost_per_token": 2e-07},
}


def test_the_key_is_found_under_the_providers_prefix(monkeypatch):
    monkeypatch.setattr(prices, "load_litellm", lambda: TABLE)
    assert prices.resolve("anthropic", "claude-sonnet-5-5") == (2.0, 10.0, "litellm's price file")
    assert prices.resolve("fireworks", "m")[:2] == pytest.approx((0.1, 0.2))
    assert prices.resolve("anthropic", "no-such-model") == (0.0, 0.0, "unknown")


def test_flags_win_over_any_lookup(monkeypatch):
    monkeypatch.setattr(prices, "load_litellm", lambda: TABLE)
    assert prices.resolve("anthropic", "claude-sonnet-5-5", 5.0, 6.0) == (5.0, 6.0, "flags")


def test_openrouters_own_list_beats_a_litellm_entry_that_disagrees(monkeypatch):
    monkeypatch.setattr(prices, "load_litellm", lambda: TABLE)
    monkeypatch.setattr(prices, "openrouter_price", lambda model: (0.30, 1.20))
    pin, pout, src = prices.resolve("openrouter", "some/model")
    assert (pin, pout) == (0.30, 1.20) and "disagrees" in src


def test_litellm_is_used_when_openrouter_agrees_or_is_unreachable(monkeypatch):
    monkeypatch.setattr(prices, "load_litellm", lambda: TABLE)
    monkeypatch.setattr(prices, "openrouter_price", lambda model: (0.003, 2.4))
    assert prices.resolve("openrouter", "some/model")[2] == "litellm's price file"
    monkeypatch.setattr(prices, "openrouter_price", lambda model: None)
    assert prices.resolve("openrouter", "some/model")[2] == "litellm's price file"


def test_the_cache_is_written_and_reused(tmp_path, monkeypatch):
    import httpx

    monkeypatch.undo()  # the autouse stub off, this test is about the real loader
    monkeypatch.setattr(prices, "CACHE", tmp_path / "p.json")
    calls = []

    class R:
        def raise_for_status(self): pass
        def json(self): return TABLE

    monkeypatch.setattr(httpx, "get", lambda *a, **k: calls.append(a) or R())
    assert prices.load_litellm() == TABLE and prices.load_litellm() == TABLE
    assert len(calls) == 1, "the second read comes from the cache"
    monkeypatch.setattr(httpx, "get", lambda *a, **k: (_ for _ in ()).throw(httpx.ConnectError("down")))
    (tmp_path / "p.json").touch()
    import os
    os.utime(tmp_path / "p.json", (0, 0))  # stale
    assert prices.load_litellm() == TABLE, "a stale cache still serves when the fetch fails"
