"""Runs the shared spec/fixtures/*.json, the same suite the Go library passes."""

import asyncio
import json
import math

import pytest

from ignatius import Plan, Registry, Request, run
from conftest import FIXTURES, MockBackend


def subset(path: str, want, got, errors: list[str]) -> None:
    """want must be contained in got: objects by key, arrays by length and element,
    numbers to 1e-9."""
    if isinstance(want, dict):
        if not isinstance(got, dict):
            errors.append(f"{path}: want object, got {got!r}")
            return
        for k, wv in want.items():
            if k not in got:
                errors.append(f"{path}.{k}: missing")
            else:
                subset(f"{path}.{k}", wv, got[k], errors)
    elif isinstance(want, list):
        if not isinstance(got, list) or len(got) != len(want):
            errors.append(f"{path}: want array len {len(want)}, got {got!r}")
            return
        for i, (w, g) in enumerate(zip(want, got)):
            subset(f"{path}[{i}]", w, g, errors)
    elif isinstance(want, (int, float)) and not isinstance(want, bool):
        if not isinstance(got, (int, float)) or isinstance(got, bool) or math.fabs(got - want) > 1e-9:
            errors.append(f"{path}: want {want}, got {got!r}")
    elif want != got:
        errors.append(f"{path}: want {want!r}, got {got!r}")


@pytest.mark.parametrize("path", FIXTURES, ids=lambda p: p.name)
def test_fixture(path):
    fx = json.loads(path.read_text())
    reg = Registry({alias: MockBackend(spec).build() for alias, spec in fx["mock"].items()})
    plan = Plan.from_dict({"mode": fx["mode"], **fx["args"]})
    routed = asyncio.run(run(reg, Request.from_dict(fx["request"]), plan))
    errors: list[str] = []
    subset("$", fx["expect"], json.loads(json.dumps(routed.to_dict())), errors)
    assert not errors, f"{fx['name']}:\n" + "\n".join(errors)


def test_there_are_fixtures():
    assert len(FIXTURES) >= 7
