"""SPEC 1.2: a model's confidence policy. The shared fixture (spec/fixtures/confidence_derived.json)
covers the cascade behavior against the Go library; these cover the formulas and the config."""

import asyncio
import math

import pytest

from ignatius import Answer, Plan, Question, Registry, Request, WithConfidence, derived_confidence, run
from conftest import MockBackend


def a(type_, **kw):
    return Answer(type=type_, **kw)


CASES = [
    ("two options, a coin flip", a("choice", probabilities={"a": 0.5, "b": 0.5}), 0.0),
    ("two options, certain", a("choice", probabilities={"a": 1.0, "b": 0.0}), 1.0),
    ("two options, 0.6", a("choice", probabilities={"a": 0.6, "b": 0.4}), 0.2),
    ("three options", a("choice", probabilities={"a": 0.7, "b": 0.2, "c": 0.1}), 0.55),
    ("more options, same 0.7", a("choice", probabilities={"a": 0.7, "b": 0.1, "c": 0.1, "d": 0.1}), (0.7 - 0.25) / 0.75),
    ("below a uniform guess clamps", a("choice", probabilities={"a": 0.2, "b": 0.2, "c": 0.2}), 0.0),
    ("a single option", a("choice", probabilities={"a": 1.0}), 1.0),
    ("score, spread", a("score", probabilities={"0": 0.1, "1": 0.2, "2": 0.6, "3": 0.1}), 0.5),
    ("score, certain", a("score", probabilities={"0": 0.0, "1": 0.0, "2": 1.0, "3": 0.0}), 1.0),
    ("score, uniform", a("score", probabilities={"0": 0.25, "1": 0.25, "2": 0.25, "3": 0.25}), 0.0),
    ("score, a single level", a("score", probabilities={"0": 1.0}), 1.0),
    ("score levels that are not indices", a("score", probabilities={"low": 0.5, "high": 0.5}), None),
    ("no probabilities", a("choice", confidence=0.9), None),
    ("noul is derived elsewhere", a("noul", noul=0.9), None),
]


@pytest.mark.parametrize("name,answer,want", CASES, ids=[c[0] for c in CASES])
def test_formulas(name, answer, want):
    got = derived_confidence(answer)
    if want is None:
        assert got is None
    else:
        assert got is not None and math.isclose(got, want, abs_tol=1e-12)


def test_policy_keeps_the_providers_number_as_native():
    backend = MockBackend({"answers": {
        "c": {"type": "choice", "choice": "a", "probabilities": {"a": 0.6, "b": 0.4}, "confidence": 0.99},
        "n": {"type": "noul", "noul": 0.9},
        "x": {"type": "choice", "choice": "a", "confidence": 0.7},
    }})
    w = asyncio.run(WithConfidence(backend, "derived").call(Request("s", {})))
    assert math.isclose(w.answers["c"].confidence, 0.2, abs_tol=1e-12) and w.answers["c"].native_confidence == 0.99
    assert w.answers["n"].native_confidence is None and w.answers["n"].confidence is None
    assert w.answers["x"].confidence == 0.7 and w.answers["x"].native_confidence is None
    w = asyncio.run(WithConfidence(backend, "provider").call(Request("s", {})))
    assert w.answers["c"].confidence == 0.99 and w.answers["c"].native_confidence is None


def test_the_two_policies_judge_one_answer_differently():
    spec = {"answers": {"q": {"type": "choice", "choice": "a", "probabilities": {"a": 0.6, "b": 0.4}, "confidence": 0.99}}}
    req = Request("s", {"q": Question("choice", "?", ["a", "b"])})
    reg = Registry({"trust": MockBackend(spec).build(), "derive": MockBackend({**spec, "confidence": "derived"}).build()})
    trust = asyncio.run(run(reg, req, Plan.from_dict({"mode": "single", "model": "trust"})))
    derive = asyncio.run(run(reg, req, Plan.from_dict({"mode": "single", "model": "derive"})))
    assert trust.answers["q"].confidence == 0.99 and "native_confidence" not in trust.answers["q"].to_dict()
    assert math.isclose(derive.answers["q"].confidence, 0.2, abs_tol=1e-12)
    assert derive.answers["q"].to_dict()["native_confidence"] == 0.99


def test_config_accepts_the_policy_and_rejects_an_unknown_one(tmp_path):
    from ignatius import load_config

    good = tmp_path / "g.toml"
    good.write_text('[models.m]\nprovider = "systemone"\nbase_url = "http://x"\nconfidence = "derived"\n')
    backend = load_config(str(good)).registry["m"]  # the breaker, which wraps the policy, which wraps the provider
    while not isinstance(backend, WithConfidence):
        backend = backend.inner
    assert backend.policy == "derived"
    bad = tmp_path / "b.toml"
    bad.write_text('[models.m]\nprovider = "systemone"\nbase_url = "http://x"\nconfidence = "vibes"\n')
    with pytest.raises(ValueError, match="confidence must be"):
        load_config(str(bad))
