"""Normalized types. JSON shapes match SPEC.md and the Go library exactly."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

MODE_SINGLE = "single"
MODE_FAN_OUT = "fan_out"
MODE_CASCADE = "cascade"

KIND_TIMEOUT = "timeout"
KIND_TRANSPORT = "transport"
KIND_HTTP = "http"
KIND_DECODE = "decode"
KIND_UNSUPPORTED = "unsupported"
KIND_CIRCUIT_OPEN = "circuit_open"

DEFAULT_THRESHOLD = 0.8
QUESTION_TYPES = ("noul", "choice", "score")


@dataclass
class Question:
    type: str
    instructions: Any = None
    criteria: Any = None

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"type": self.type, "instructions": self.instructions}
        if self.criteria is not None:
            d["criteria"] = self.criteria
        return d

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Question:
        return cls(d.get("type", ""), d.get("instructions"), d.get("criteria"))


@dataclass
class Request:
    """One state and typed questions. ``images`` are optional base64 PNG, JPEG or WebP images shared
    by every question (SPEC 3.1); only a model whose limits allow images is sent them."""

    state: Any
    questions: dict[str, Question]
    images: list[str] = field(default_factory=list)

    def validate(self) -> None:
        if not self.questions:
            raise ValueError("at least one question is required")
        for i, img in enumerate(self.images):
            if not img:
                raise ValueError(f"images[{i}] is empty: images are base64 strings")
        for qid, q in self.questions.items():
            if q.type not in QUESTION_TYPES:
                raise ValueError(f"question {qid!r}: type must be noul, choice or score, got {q.type!r}")

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"state": self.state, "questions": {k: q.to_dict() for k, q in self.questions.items()}}
        if self.images:
            d["images"] = self.images
        return d

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Request:
        return cls(
            d.get("state"),
            {k: Question.from_dict(v) for k, v in (d.get("questions") or {}).items()},
            list(d.get("images") or []),
        )


@dataclass
class Answer:
    """A normalized answer. ``confidence`` is already normalized (SPEC 1.1)."""

    type: str
    noul: float | None = None
    choice: str = ""
    score: float | None = None
    probabilities: dict[str, float] = field(default_factory=dict)
    legend: dict[str, str] = field(default_factory=dict)
    confidence: float | None = None
    native_confidence: float | None = None  # the provider's own number, when a derived policy replaced it
    model: str = ""
    contributors: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"type": self.type}
        if self.noul is not None:
            d["noul"] = self.noul
        if self.choice:
            d["choice"] = self.choice
        if self.score is not None:
            d["score"] = self.score
        if self.probabilities:
            d["probabilities"] = self.probabilities
        if self.legend:
            d["legend"] = self.legend
        d["confidence"] = self.confidence
        if self.native_confidence is not None:
            d["native_confidence"] = self.native_confidence
        if self.model:
            d["model"] = self.model
        if self.contributors:
            d["contributors"] = self.contributors
        return d

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Answer:
        return cls(
            type=d.get("type", ""),
            noul=d.get("noul"),
            choice=d.get("choice") or "",
            score=d.get("score"),
            probabilities={str(k): float(v) for k, v in (d.get("probabilities") or {}).items()},
            legend={str(k): v for k, v in (d.get("legend") or {}).items()},
            confidence=d.get("confidence"),
            native_confidence=d.get("native_confidence"),
            model=d.get("model") or "",
            contributors=list(d.get("contributors") or []),
        )


@dataclass
class Usage:
    input_tokens: int = 0
    output_tokens: int = 0

    def to_dict(self) -> dict[str, int]:
        return {"input_tokens": self.input_tokens, "output_tokens": self.output_tokens}


@dataclass
class Result:
    model: str
    answers: dict[str, Answer]
    usage: Usage = field(default_factory=Usage)
    latency_ms: int = 0
    cost_usd: float | None = None  # None = no price configured
    cached: int = 0  # questions served from the cache

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {
            "model": self.model,
            "answers": {k: a.to_dict() for k, a in self.answers.items()},
            "usage": self.usage.to_dict(),
            "latency_ms": self.latency_ms,
        }
        if self.cost_usd is not None:
            d["cost_usd"] = self.cost_usd
        if self.cached:
            d["cached"] = self.cached
        return d


@dataclass
class ErrorBody:
    kind: str
    message: str

    def to_dict(self) -> dict[str, str]:
        return {"kind": self.kind, "message": self.message}


@dataclass
class Failure:
    model: str
    error: ErrorBody
    latency_ms: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {"model": self.model, "error": self.error.to_dict(), "latency_ms": self.latency_ms}


@dataclass
class HopQuestion:
    id: str
    confidence: float | None = None
    threshold: float | None = None  # None on the last tier
    escalated: bool = False

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"id": self.id, "confidence": self.confidence}
        if self.threshold is not None:
            d["threshold"] = self.threshold
        d["escalated"] = self.escalated
        return d


@dataclass
class Hop:
    tier: int
    model: str
    questions: list[str]
    escalated: list[str] = field(default_factory=list)
    latency_ms: int = 0
    error: ErrorBody | None = None
    detail: list[HopQuestion] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {
            "tier": self.tier,
            "model": self.model,
            "questions": self.questions,
            "escalated": self.escalated,
            "latency_ms": self.latency_ms,
        }
        if self.error is not None:
            d["error"] = self.error.to_dict()
        if self.detail:
            d["detail"] = [q.to_dict() for q in self.detail]
        return d


@dataclass
class Routed:
    mode: str
    ok: bool = False
    answers: dict[str, Answer] = field(default_factory=dict)
    results: list[Result] = field(default_factory=list)
    failures: list[Failure] = field(default_factory=list)
    trace: list[Hop] = field(default_factory=list)
    cost_usd: float | None = None  # sum over priced results
    request_id: str = ""  # set when the result came from a gateway; the id feedback refers to

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {
            "mode": self.mode,
            "ok": self.ok,
            "answers": {k: a.to_dict() for k, a in self.answers.items()},
            "results": [r.to_dict() for r in self.results],
            "failures": [f.to_dict() for f in self.failures],
        }
        if self.trace:
            d["trace"] = [h.to_dict() for h in self.trace]
        if self.cost_usd is not None:
            d["cost_usd"] = self.cost_usd
        return d

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Routed:
        """Decode a gateway ``/v1/route`` response."""

        def err(e: dict[str, Any] | None) -> ErrorBody | None:
            return ErrorBody(e["kind"], e.get("message", "")) if e else None

        return cls(
            mode=d.get("mode", ""),
            ok=bool(d.get("ok")),
            cost_usd=d.get("cost_usd"),
            request_id=d.get("request_id") or "",
            answers={k: Answer.from_dict(a) for k, a in (d.get("answers") or {}).items()},
            results=[
                Result(
                    r["model"],
                    {k: Answer.from_dict(a) for k, a in (r.get("answers") or {}).items()},
                    Usage(**{k: int(v) for k, v in (r.get("usage") or {}).items()}),
                    int(r.get("latency_ms", 0)),
                    r.get("cost_usd"),
                    int(r.get("cached", 0)),
                )
                for r in d.get("results") or []
            ],
            failures=[
                Failure(f["model"], err(f["error"]) or ErrorBody("", ""), int(f.get("latency_ms", 0)))
                for f in d.get("failures") or []
            ],
            trace=[
                Hop(
                    h["tier"],
                    h["model"],
                    list(h.get("questions") or []),
                    list(h.get("escalated") or []),
                    int(h.get("latency_ms", 0)),
                    err(h.get("error")),
                    [
                        HopQuestion(q["id"], q.get("confidence"), q.get("threshold"), bool(q.get("escalated")))
                        for q in h.get("detail") or []
                    ],
                )
                for h in d.get("trace") or []
            ],
        )


@dataclass
class Threshold:
    """A number (all question types) or a per-type map. Missing types fall back
    to ``default``, then to the enclosing threshold."""

    default: float | None = None
    by_type: dict[str, float] = field(default_factory=dict)

    @classmethod
    def parse(cls, v: Any) -> Threshold:
        if isinstance(v, Threshold):
            return v
        if isinstance(v, bool):
            raise ValueError("threshold must be a number or a {noul,choice,score} map")
        if isinstance(v, (int, float)):
            return cls(default=float(v))
        if isinstance(v, dict):
            return cls(by_type={k: float(x) for k, x in v.items()})
        raise ValueError("threshold must be a number or a {noul,choice,score} map")

    def lookup(self, qtype: str) -> float | None:
        if qtype in self.by_type:
            return self.by_type[qtype]
        return self.default

    def to_json(self) -> Any:
        return dict(self.by_type) if self.by_type else self.default


@dataclass
class Tier:
    model: str
    threshold: Threshold | None = None

    @classmethod
    def parse(cls, v: Any) -> Tier:
        if isinstance(v, Tier):
            return v
        if isinstance(v, str):
            return cls(v)
        t = v.get("threshold")
        return cls(v["model"], Threshold.parse(t) if t is not None else None)

    def bar(self, qtype: str, plan: Plan) -> float:
        for th in (self.threshold, plan.threshold):
            if th is not None and (v := th.lookup(qtype)) is not None:
                return v
        return DEFAULT_THRESHOLD

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"model": self.model}
        if self.threshold is not None:
            d["threshold"] = self.threshold.to_json()
        return d


@dataclass
class Plan:
    """How to route a Request (SPEC 6.0)."""

    mode: str
    model: str = ""
    models: list[str] = field(default_factory=list)
    tiers: list[Tier] = field(default_factory=list)
    threshold: Threshold | None = None
    reduce: str = ""
    min_success: int = 0
    timeout_ms: int = 0

    @classmethod
    def from_dict(cls, d: dict[str, Any]) -> Plan:
        th = d.get("threshold")
        return cls(
            mode=d.get("mode", ""),
            model=d.get("model", ""),
            models=list(d.get("models") or []),
            tiers=[Tier.parse(t) for t in d.get("tiers") or []],
            threshold=Threshold.parse(th) if th is not None else None,
            reduce=d.get("reduce", ""),
            min_success=int(d.get("min_success", 0)),
            timeout_ms=int(d.get("timeout_ms", 0)),
        )

    def to_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {"mode": self.mode}
        if self.model:
            d["model"] = self.model
        if self.models:
            d["models"] = self.models
        if self.tiers:
            d["tiers"] = [t.to_dict() for t in self.tiers]
        if self.threshold is not None:
            d["threshold"] = self.threshold.to_json()
        if self.reduce:
            d["reduce"] = self.reduce
        if self.min_success:
            d["min_success"] = self.min_success
        if self.timeout_ms:
            d["timeout_ms"] = self.timeout_ms
        return d


class PlanError(ValueError):
    """The caller's Plan is invalid (HTTP 422 territory). Lists every problem."""

    def __init__(self, problems: list[str]):
        super().__init__("invalid plan: " + "; ".join(problems))
        self.problems = problems
