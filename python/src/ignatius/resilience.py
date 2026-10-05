"""Backend decorators: pricing, circuit breaker and response cache (SPEC 11).

Composition, outermost first: cache, breaker, pricing, then the real backend."""

from __future__ import annotations

import asyncio
import copy
import hashlib
import json
import threading
import time
from collections import OrderedDict
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from .backend import Backend, CallError, WireResponse
from .types import (
    KIND_CIRCUIT_OPEN,
    KIND_DECODE,
    KIND_HTTP,
    KIND_TIMEOUT,
    KIND_TRANSPORT,
    KIND_UNSUPPORTED,
    Answer,
    Question,
    Request,
    Usage,
)


async def _probe_of(inner: Any) -> str:
    probe = getattr(inner, "probe", None)
    return await probe() if probe else "unknown"


class Priced:
    """Prices a backend's calls from the usage it reports (SPEC 11.3)."""

    def __init__(self, inner: Backend, in_per_mtok: float = 0.0, out_per_mtok: float = 0.0):
        self.inner, self.in_per_mtok, self.out_per_mtok = inner, in_per_mtok, out_per_mtok

    async def call(self, request: Request) -> WireResponse:
        w = await self.inner.call(request)
        # same operation order as the Go library, so costs match to the last bit
        w.cost_usd = w.usage.input_tokens * self.in_per_mtok / 1e6 + w.usage.output_tokens * self.out_per_mtok / 1e6
        return w

    async def probe(self) -> str:
        return await _probe_of(self.inner)


# ---- circuit breaker --------------------------------------------------------------

DEFAULT_BREAKER_THRESHOLD = 5
DEFAULT_BREAKER_COOLDOWN_S = 30.0


@dataclass
class BreakerConfig:
    enabled: bool = True
    failure_threshold: int = 0  # 0 = default (5)
    cooldown_ms: int = 0  # 0 = default (30000)


_SUCCESS, _FAILURE, _IGNORE = "success", "failure", "ignore"


def classify(exc: BaseException | None) -> str:
    """How the breaker sees a call's outcome (SPEC 11.1)."""
    if exc is None:
        return _SUCCESS
    if isinstance(exc, CallError):
        if exc.kind in (KIND_TIMEOUT, KIND_TRANSPORT, KIND_DECODE):
            return _FAILURE
        if exc.kind == KIND_HTTP:
            return _FAILURE if exc.status >= 500 or exc.status == 429 else _SUCCESS
        return _IGNORE  # unsupported, circuit_open
    if isinstance(exc, asyncio.CancelledError):
        return _IGNORE  # the caller gave up; not the model's fault
    return _FAILURE  # timeouts, connection errors, anything else


class Breaker:
    """Stops calling a failing backend. After ``failure_threshold`` consecutive
    countable failures it opens and rejects calls instantly (kind circuit_open);
    after the cooldown it lets exactly one probe call through.

    Known divergence from the Go library: a plan-level ``timeout_ms`` expiry
    cancels the in-flight call, which Python cannot tell apart from a caller
    cancelling, so it is ignored here. Go counts it as a timeout. Per-model
    timeouts (``timeout_ms`` on the model) count in both."""

    def __init__(self, inner: Backend, cfg: BreakerConfig | None = None, now: Callable[[], float] = time.monotonic):
        cfg = cfg or BreakerConfig()
        self.inner = inner
        self.threshold = cfg.failure_threshold if cfg.failure_threshold > 0 else DEFAULT_BREAKER_THRESHOLD
        self.cooldown = cfg.cooldown_ms / 1000 if cfg.cooldown_ms > 0 else DEFAULT_BREAKER_COOLDOWN_S
        self.now = now
        self._lock = threading.Lock()
        self._state = "closed"
        self._failures = 0
        self._opened_at = 0.0
        self._probing = False

    def _admit(self) -> None:
        with self._lock:
            if self._state == "closed":
                return
            if self._state == "open":
                wait = self.cooldown - (self.now() - self._opened_at)
                if wait > 0:
                    raise CallError(KIND_CIRCUIT_OPEN, f"breaker open; next probe in {round(wait)}s")
                self._state, self._probing = "half_open", True
                return
            if self._probing:  # half-open: one probe at a time
                raise CallError(KIND_CIRCUIT_OPEN, "breaker half-open; probe in flight")
            self._probing = True

    def _record(self, exc: BaseException | None) -> None:
        with self._lock:
            probe = self._state == "half_open"
            verdict = classify(exc)
            if verdict == _SUCCESS:
                self._failures, self._state, self._probing = 0, "closed", False
            elif verdict == _FAILURE:
                self._failures += 1
                if probe or self._failures >= self.threshold:
                    self._state, self._opened_at, self._probing = "open", self.now(), False
            elif probe:
                self._probing = False  # a cancelled probe gives no verdict; let another call probe

    async def call(self, request: Request) -> WireResponse:
        self._admit()
        try:
            w = await self.inner.call(request)
        except BaseException as e:
            self._record(e)
            raise
        self._record(None)
        return w

    @property
    def state(self) -> str:
        with self._lock:
            return self._state

    async def probe(self) -> str:
        return KIND_CIRCUIT_OPEN if self.state != "closed" else await _probe_of(self.inner)


# ---- response cache ---------------------------------------------------------------


@dataclass
class CacheConfig:
    enabled: bool = False
    ttl_ms: int = 0  # 0 = default (5 min)
    max_entries: int = 0  # 0 = default (10000)


class Cache:
    """Bounded in-memory LRU with TTL, keyed per question (SPEC 11.2)."""

    def __init__(self, cfg: CacheConfig | None = None, now: Callable[[], float] = time.monotonic):
        cfg = cfg or CacheConfig()
        self.ttl = cfg.ttl_ms / 1000 if cfg.ttl_ms > 0 else 300.0
        self.max = cfg.max_entries if cfg.max_entries > 0 else 10000
        self.now = now
        self._lock = threading.Lock()
        self._items: OrderedDict[str, tuple[Answer, float]] = OrderedDict()

    def __len__(self) -> int:
        with self._lock:
            return len(self._items)

    def get(self, key: str) -> Answer | None:
        with self._lock:
            hit = self._items.get(key)
            if hit is None:
                return None
            ans, expires = hit
            if self.now() >= expires:
                del self._items[key]
                return None
            self._items.move_to_end(key)
            return copy.deepcopy(ans)

    def put(self, key: str, ans: Answer) -> None:
        with self._lock:
            self._items[key] = (copy.deepcopy(ans), self.now() + self.ttl)
            self._items.move_to_end(key)
            while len(self._items) > self.max:
                self._items.popitem(last=False)


def hash_images(images: list[str]) -> str:
    """A digest of an ordered list of images, "" for none (so keys without images are unchanged)."""
    if not images:
        return ""
    h = hashlib.sha256()
    for img in images:
        h.update(hashlib.sha256(img.encode()).digest())
    return h.hexdigest()


def question_key(alias: str, upstream: str, state: Any, q: Question, images_hash: str = "") -> str | None:
    """Hash of everything that determines one question's answer from one model:
    the alias, its upstream model, the state, a digest of the images and the question content
    (not its id). The state itself is not retained, only the hash."""
    try:
        blob = json.dumps(
            [alias, upstream, state, images_hash, q.type, q.instructions, q.criteria], sort_keys=True, separators=(",", ":")
        )
    except (TypeError, ValueError):
        return None
    return hashlib.sha256(blob.encode()).hexdigest()


class Cached:
    """Serves repeated questions from a Cache. Cached questions are answered
    without a call; the rest go to ``inner`` as a smaller request and are merged."""

    def __init__(self, inner: Backend, cache: Cache, alias: str, upstream_model: str = "", priced: bool = False):
        self.inner, self.cache, self.alias, self.upstream_model, self.priced = inner, cache, alias, upstream_model, priced

    async def call(self, request: Request) -> WireResponse:
        hits: dict[str, Answer] = {}
        keys: dict[str, str] = {}
        miss = Request(request.state, {}, request.images)
        images_hash = hash_images(request.images)  # once per call, not per question: images are large
        for qid, q in request.questions.items():
            key = question_key(self.alias, self.upstream_model, request.state, q, images_hash)
            if key is not None:
                if (a := self.cache.get(key)) is not None:
                    hits[qid] = a
                    continue
                keys[qid] = key
            miss.questions[qid] = q
        if not miss.questions:
            return WireResponse(model="cache", answers=hits, cached=len(hits), cost_usd=0.0 if self.priced else None)
        w = await self.inner.call(miss)
        for qid in miss.questions:
            if qid in w.answers and qid in keys:
                self.cache.put(keys[qid], w.answers[qid])
        if hits:
            w.answers = {**w.answers, **hits}
            w.cached = len(hits)
        return w

    async def probe(self) -> str:
        return await _probe_of(self.inner)


# ---- per-model limits (SPEC 3.1) --------------------------------------------------


@dataclass
class LimitsConfig:
    """What one call to a model can take. ``max_questions`` splits a larger request into calls of at
    most that many (0 = no limit). ``max_images`` is how many images a call may carry (0 = none: a
    request with images fails with kind ``unsupported``, which a cascade escalates past)."""

    max_questions: int = 0
    max_images: int = 0


class Limited:
    """Enforces a model's limits. The outermost decorator, so a split request reaches the cache and the
    breaker as the ordinary smaller calls they already handle, and an unsupported request never touches
    the breaker."""

    def __init__(self, inner: Backend, limits: LimitsConfig | None = None):
        self.inner, self.limits = inner, limits or LimitsConfig()

    async def call(self, request: Request) -> WireResponse:
        n = len(request.images)
        if n > self.limits.max_images:
            if self.limits.max_images == 0:
                msg = f"this model takes no images, the request has {n}"
            else:
                msg = f"this model takes at most {self.limits.max_images} images, the request has {n}"
            raise CallError(KIND_UNSUPPORTED, msg)
        step = self.limits.max_questions
        if step <= 0 or len(request.questions) <= step:
            return await self.inner.call(request)
        # Too many questions for one call: split, call the pieces together, merge. All or nothing.
        ids = sorted(request.questions)  # the same split every time, so the cache sees the same pieces
        pieces = [
            Request(request.state, {qid: request.questions[qid] for qid in ids[i : i + step]}, request.images)
            for i in range(0, len(ids), step)
        ]
        results = await asyncio.gather(*(self.inner.call(p) for p in pieces), return_exceptions=True)
        for r in results:
            if isinstance(r, BaseException):
                raise r  # the first failing piece, in order
        out = WireResponse(model=results[0].model)
        for r in results:
            out.answers.update(r.answers)
            out.usage = Usage(out.usage.input_tokens + r.usage.input_tokens, out.usage.output_tokens + r.usage.output_tokens)
            out.cached += r.cached
            if r.cost_usd is not None:
                out.cost_usd = (out.cost_usd or 0.0) + r.cost_usd
        return out

    async def probe(self) -> str:
        return await _probe_of(self.inner)
