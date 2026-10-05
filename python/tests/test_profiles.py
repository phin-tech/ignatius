"""Profiles (SPEC 12): intent names that resolve to real model aliases before a plan runs."""

import asyncio

import pytest

from ignatius import (
    Answer, Config, Ignatius, Plan, Question, Registry, Request, ResolveError, Router, Tier,
    WireResponse, load_config, substitute_profiles, validate_profiles,
)


class Echo:
    """A backend that answers a fixed noul and counts its calls."""

    def __init__(self, p):
        self.p, self.calls = p, 0

    async def call(self, request):
        self.calls += 1
        return WireResponse(answers={q: Answer("noul", noul=self.p) for q in request.questions})


def req():
    return Request("x", {"q": Question("noul", "?")})


def cfg(**kw):
    jeff, jev = Echo(0.55), Echo(0.99)
    base = dict(
        registry=Registry(jeff=jeff, jev=jev),
        profiles={"fast": "jeff", "best": "jev"},
        routes={"tiered": Plan("cascade", tiers=[Tier("fast"), Tier("best")])},
    )
    base.update(kw)
    return Config(**base), jeff, jev


def test_a_profile_resolves_to_the_real_model_everywhere_a_name_can_appear():
    c, jeff, jev = cfg(default_route="best")
    ig = Ignatius(c)
    r = ig.run_sync(req(), model="fast")
    assert (jeff.calls, jev.calls) == (1, 0) and r.results[0].model == "jeff", "attributed to the real model"
    assert r.answers["q"].model == "jeff"
    for model, hops in {"tiered": ["jeff", "jev"], "cascade:fast>best": ["jeff", "jev"],
                        "cascade:fast@0.5>jev": ["jeff", "jev"], "jev-latest": [], "": []}.items():
        routed = ig.run_sync(req(), model=model)
        assert routed.ok and [h.model for h in routed.trace] == hops, model
    assert ig.run_sync(req(), model="fan-out:fast,best|mean").mode == "fan_out"
    assert ig.run_sync(req(), model="jev-latest").results[0].model == "jev", "default_route can be a profile"


def test_resolving_does_not_mutate_the_stored_route():
    c, *_ = cfg()
    before = [t.model for t in c.routes["tiered"].tiers]
    plan = Router(c).resolve("tiered")
    assert [t.model for t in plan.tiers] == ["jeff", "jev"] and [t.model for t in c.routes["tiered"].tiers] == before == ["fast", "best"]
    assert substitute_profiles(Plan("single", model="fast"), {"fast": "jeff"}).model == "jeff"


@pytest.mark.parametrize("profiles,needle", [
    ({"fast": "nope"}, "not a model alias"),
    ({"fast": "jeff", "quick": "fast"}, "not a model alias"),  # no chains
    ({"jeff": "jeff"}, "clashes with a model alias"),
    ({"r": "jeff"}, "clashes with a route"),
    ({"Fast": "jeff"}, "must be lowercase"),
    ({"a:b": "jeff"}, "must be lowercase"),
    ({"a,b": "jeff"}, "must be lowercase"),
    ({"a>b": "jeff"}, "must be lowercase"),
    ({"": "jeff"}, "must be lowercase"),
    ({"x" * 65: "jeff"}, "must be lowercase"),
    ({"inline": "jeff"}, "reserved"),
    ({"unknown": "jeff"}, "reserved"),
    ({"jev-latest": "jeff"}, "reserved"),
])
def test_profile_validation(profiles, needle):
    reg = Registry(jeff=Echo(0.5))
    with pytest.raises(ValueError, match=needle):
        validate_profiles(profiles, reg, {"r": Plan("single", model="jeff")})


def test_at_most_64_profiles_and_startup_checks():
    reg = Registry(jeff=Echo(0.5))
    with pytest.raises(ValueError, match="at most"):
        validate_profiles({f"p{i}": "jeff" for i in range(65)}, reg, {})
    with pytest.raises(ValueError, match="ghost"):  # a route naming a profile that does not exist
        Router(Config(registry=reg, routes={"r": Plan("cascade", tiers=[Tier("ghost"), Tier("jeff")])}))
    with pytest.raises(ValueError, match="default_route"):
        Router(Config(registry=reg, default_route="ghost"))


def test_unknown_names_list_the_profiles_too():
    c, *_ = cfg()
    with pytest.raises(ResolveError) as e:
        Router(c).resolve("nope")
    assert e.value.profiles == ["best", "fast"] and "profile" in str(e.value)


def test_profiles_load_from_the_same_toml_the_gateway_reads(tmp_path):
    p = tmp_path / "c.toml"
    p.write_text('''
default_route = "best"
[profiles]
fast = "jeff"
best = "jev"
[models.jeff]
provider = "systemone"
base_url = "http://x"
[models.jev]
provider = "systemone"
base_url = "http://y"
[routes.tiered]
mode = "cascade"
tiers = [ { model = "fast", threshold = 0.5 }, "best" ]
''')
    config = load_config(str(p))
    assert config.profiles == {"fast": "jeff", "best": "jev"}
    r = Router(config)
    assert [t.model for t in r.resolve("tiered").tiers] == ["jeff", "jev"]
    assert r.resolve("").model == "jev"  # default_route is a profile
    p.write_text(p.read_text().replace('best = "jev"', 'best = "missing"'))
    with pytest.raises(ValueError, match="not a model alias"):
        load_config(str(p))
