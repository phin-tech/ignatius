"""Plan execution: single, fan_out and cascade (SPEC 4 and 6.0)."""

from __future__ import annotations

import asyncio
import time
from collections.abc import Callable

from .backend import Registry, conf_value
from .reduce import reduce
from .types import (
    MODE_CASCADE,
    MODE_FAN_OUT,
    MODE_SINGLE,
    Answer,
    Failure,
    Hop,
    HopQuestion,
    Plan,
    PlanError,
    Request,
    Routed,
)


def validate_plan(plan: Plan, reg: Registry) -> None:
    """Raise PlanError listing every problem with the Plan."""
    probs: list[str] = []

    def has(alias: str) -> None:
        if alias not in reg:
            probs.append(f"unknown model {alias!r} (available: {reg.names()})")

    if plan.mode == MODE_SINGLE:
        if not plan.model:
            probs.append("single requires model")
        else:
            has(plan.model)
    elif plan.mode == MODE_FAN_OUT:
        if not plan.models:
            probs.append("fan_out requires models")
        seen: set[str] = set()
        for m in plan.models:
            has(m)
            if m in seen:
                probs.append(f"duplicate model {m!r}")
            seen.add(m)
        if plan.reduce not in ("", "none", "vote", "mean", "most_confident"):
            probs.append(f"unknown reduce {plan.reduce!r}")
    elif plan.mode == MODE_CASCADE:
        if not plan.tiers:
            probs.append("cascade requires tiers")
        for t in plan.tiers:
            has(t.model)
    else:
        probs.append(f"unknown mode {plan.mode!r}")
    if probs:
        raise PlanError(probs)


async def run(
    reg: Registry,
    request: Request,
    plan: Plan,
    escalate_if: Callable[[str, Answer], bool] | None = None,
) -> Routed:
    """Execute a Plan. Provider failures are data in the returned Routed; only an
    invalid Plan raises (PlanError)."""
    validate_plan(plan, reg)
    deadline = time.monotonic() + plan.timeout_ms / 1000 if plan.timeout_ms > 0 else None

    def remaining() -> float | None:
        return None if deadline is None else max(0.0, deadline - time.monotonic())

    out = Routed(mode=plan.mode)
    if plan.mode == MODE_SINGLE:
        r = await reg.call(plan.model, request, remaining)
        if isinstance(r, Failure):
            out.failures.append(r)
        else:
            out.results.append(r)
            out.answers = r.answers
            out.ok = True
    elif plan.mode == MODE_FAN_OUT:
        await _fan_out(reg, request, plan, remaining, out)
    else:
        await _cascade(reg, request, plan, remaining, escalate_if, out)
    priced = [res.cost_usd for res in out.results if res.cost_usd is not None]
    if priced:
        out.cost_usd = sum(priced)
    return out


async def _fan_out(reg, request, plan, remaining, out: Routed) -> None:
    # never fail fast: every call is bounded by its own timeout; results keep the
    # given order, not completion order
    slots = await asyncio.gather(*(reg.call(m, request, remaining) for m in plan.models))
    for s in slots:
        (out.failures if isinstance(s, Failure) else out.results).append(s)
    out.ok = len(out.results) >= (plan.min_success if plan.min_success > 0 else 1)
    if plan.reduce not in ("", "none"):
        out.answers = reduce(plan.reduce, request.questions, out.results)


async def _cascade(reg, request, plan, remaining, escalate_if, out: Routed) -> None:
    pending = sorted(request.questions)
    best: dict[str, Answer] = {}  # highest confidence so far; ties go to the later tier
    for i, tier in enumerate(plan.tiers):
        if not pending:
            break
        last = i == len(plan.tiers) - 1
        sub = Request(request.state, {qid: request.questions[qid] for qid in pending}, request.images)
        hop = Hop(tier=i, model=tier.model, questions=list(pending))
        res = await reg.call(tier.model, sub, remaining)
        if isinstance(res, Failure):
            out.failures.append(res)
            hop.latency_ms, hop.error = res.latency_ms, res.error
            hop.detail = [HopQuestion(qid, escalated=not last) for qid in pending]
            if not last:
                hop.escalated = list(pending)
            out.trace.append(hop)
            continue
        hop.latency_ms = res.latency_ms
        out.results.append(res)
        nxt: list[str] = []
        for qid in pending:
            a = res.answers.get(qid)
            if a is None:  # a successful tier that skipped a question escalates it
                nxt.append(qid)
                hop.detail.append(HopQuestion(qid, escalated=not last))
                continue
            prev = best.get(qid)
            if prev is None or conf_value(a) >= conf_value(prev):
                best[qid] = a
            if last:  # the last tier never settles; the best-seen pass below decides
                nxt.append(qid)
                hop.detail.append(HopQuestion(qid, confidence=a.confidence))
                continue
            bar = tier.bar(a.type, plan)
            settled = a.confidence is not None and a.confidence >= bar
            if escalate_if is not None:
                settled = not escalate_if(qid, a)
            hop.detail.append(HopQuestion(qid, a.confidence, bar, not settled))
            if settled:
                out.answers[qid] = a
            else:
                nxt.append(qid)
        if not last:
            hop.escalated = list(nxt)
        out.trace.append(hop)
        pending = nxt
    for qid in pending:  # still unsettled: best answer seen, if any
        if qid in best:
            out.answers[qid] = best[qid]
    out.ok = len(out.answers) == len(request.questions)
