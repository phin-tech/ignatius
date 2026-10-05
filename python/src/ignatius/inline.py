"""Inline routes carried in a model name (SPEC 6.1)."""

from __future__ import annotations

from .types import MODE_CASCADE, MODE_FAN_OUT, Plan, Threshold, Tier

INLINE_GRAMMAR = "fan-out:a,b[|vote|mean|most_confident]  |  cascade:a[@0.5]>b[@0.7]>c"


class NotInline(Exception):
    """The string has no inline-route prefix (it may be a plain alias)."""


def _split(s: str, sep: str) -> list[str]:
    parts = [p.strip() for p in s.split(sep)]
    if any(not p for p in parts):
        raise ValueError(f"empty model name in {s!r}")
    return parts


def parse_inline(s: str, max_models: int = 0) -> Plan:
    head, sep, body = s.partition(":")
    if not sep:
        raise NotInline(s)
    mode = head.replace("-", "_")
    if mode == MODE_FAN_OUT:
        names, _, reducer = body.partition("|")
        plan = Plan(MODE_FAN_OUT, models=_split(names, ","), reduce=reducer or "vote")
    elif mode == MODE_CASCADE:
        plan = Plan(MODE_CASCADE)
        for part in _split(body, ">"):
            alias, has, thr = part.partition("@")
            tier = Tier(alias)
            if has:
                try:
                    v = float(thr)
                except ValueError:
                    v = -1.0
                if not 0.0 <= v <= 1.0:
                    raise ValueError(f"tier {part!r}: threshold must be a number in [0,1]")
                tier.threshold = Threshold(default=v)
            plan.tiers.append(tier)
    else:
        raise ValueError(f"unknown inline mode {head!r} (grammar: {INLINE_GRAMMAR})")
    n = len(plan.models) + len(plan.tiers)
    if max_models > 0 and n > max_models:
        raise ValueError(f"inline route names {n} models, the limit is {max_models}")
    return plan
