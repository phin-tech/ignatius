"""Backends, normalization and the registry that turns raw calls into Results."""

from __future__ import annotations

import asyncio
import base64
import binascii
import dataclasses
import time
from dataclasses import dataclass, field
from typing import Any, Protocol

import httpx

from .types import (
    KIND_DECODE,
    KIND_HTTP,
    KIND_TIMEOUT,
    KIND_TRANSPORT,
    KIND_UNSUPPORTED,
    Answer,
    ErrorBody,
    Failure,
    Request,
    Result,
    Usage,
)


class CallError(Exception):
    """A classified provider failure. ``status`` is the HTTP status for kind
    ``http`` (0 otherwise); the circuit breaker uses it to tell a sick server
    (5xx, 429) from a caller mistake (other 4xx)."""

    def __init__(self, kind: str, message: str, status: int = 0):
        super().__init__(f"{kind}: {message}")
        self.kind = kind
        self.message = message
        self.status = status


@dataclass
class WireResponse:
    """A provider's raw answer, before normalization."""

    model: str = ""
    answers: dict[str, Answer] = field(default_factory=dict)
    usage: Usage = field(default_factory=Usage)
    cost_usd: float | None = None  # set by the Priced decorator
    cached: int = 0  # set by the Cached decorator


class Backend(Protocol):
    async def call(self, request: Request) -> WireResponse: ...


def derive_confidence(a: Answer) -> float | None:
    """SPEC 1.1: noul is abs(p-0.5)*2; choice/score use the provider's confidence
    if present, else max(probabilities)."""
    if a.type == "noul":
        return None if a.noul is None else abs(a.noul - 0.5) * 2
    if a.confidence is not None:
        return a.confidence
    if a.probabilities:
        return max(a.probabilities.values())
    return None


CONFIDENCE_PROVIDER = "provider"
CONFIDENCE_DERIVED = "derived"


def valid_confidence_policy(policy: str) -> bool:
    return policy in ("", CONFIDENCE_PROVIDER, CONFIDENCE_DERIVED)


def derived_confidence(a: Answer) -> float | None:
    """SPEC 1.2: the chance-corrected concentration of an answer's own probabilities (TypeSafe's
    published formulas, which Decis uses too).

    choice, K options:  (p_max - 1/K) / (1 - 1/K)
    score,  n levels:   max(0, 1 - sum_i p_i*|i - m| / MAD_uniform), m the most likely level,
                        MAD_uniform = (1/n) * sum_i |i - (n-1)/2|

    ``None`` when there is nothing to derive from, so the provider's number stands.
    """
    if not a.probabilities:
        return None
    if a.type == "choice":
        k = len(a.probabilities)
        if k < 2:
            return 1.0
        pmax = max(a.probabilities.values())
        return min(1.0, max(0.0, (pmax - 1 / k) / (1 - 1 / k)))
    if a.type == "score":
        try:
            levels = sorted((int(key), p) for key, p in a.probabilities.items())
        except ValueError:  # levels are indices; anything else cannot be placed on the scale
            return None
        n = len(levels)
        m = levels[0]
        for lv in levels:
            if lv[1] > m[1]:  # the first of equals wins
                m = lv
        spread = sum(p * abs(i - m[0]) for i, p in levels)
        mad = sum(abs(idx - (n - 1) / 2) for idx in range(n)) / n
        if mad == 0:
            return 1.0
        return min(1.0, max(0.0, 1 - spread / mad))
    return None


class WithConfidence:
    """Applies a model's confidence policy to its backend's raw answers (SPEC 1.2). It sits
    directly on the provider, below pricing, the breaker and the cache."""

    def __init__(self, inner: Backend, policy: str):
        self.inner, self.policy = inner, policy

    async def call(self, request: Request) -> WireResponse:
        w = await self.inner.call(request)
        if self.policy != CONFIDENCE_DERIVED:
            return w
        answers: dict[str, Answer] = {}
        for qid, a in w.answers.items():
            a = dataclasses.replace(a)  # the backend may reuse its answers
            d = derived_confidence(a)
            if d is not None:
                if a.confidence is not None:
                    a.native_confidence = a.confidence
                a.confidence = d
            answers[qid] = a
        w.answers = answers
        return w

    async def probe(self) -> str:
        probe = getattr(self.inner, "probe", None)
        return await probe() if probe else "unknown"


def normalize(model: str, wire: dict[str, Answer]) -> dict[str, Answer]:
    out: dict[str, Answer] = {}
    for qid, a in wire.items():
        a.model = model
        a.confidence = derive_confidence(a)
        out[qid] = a
    return out


def conf_value(a: Answer) -> float:
    return -1.0 if a.confidence is None else a.confidence


class SystemOne:
    """Backend for any Jev-compatible server: ``POST {base_url}/v1/systemone``."""

    def __init__(
        self,
        base_url: str,
        model: str = "jev-latest",
        api_key: str = "",
        auth_header: str = "",
        timeout_s: float | None = None,
        client: httpx.AsyncClient | None = None,
    ):
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.api_key = api_key
        self.auth_header = auth_header
        self.timeout_s = timeout_s
        self._client = client

    async def call(self, request: Request) -> WireResponse:
        headers = {"Content-Type": "application/json"}
        if self.api_key:
            if self.auth_header:
                headers[self.auth_header] = self.api_key
            else:
                headers["Authorization"] = f"Bearer {self.api_key}"
        body = {"state": request.state, "model": self.model, "questions": request.to_dict()["questions"]}
        if request.images:
            body["images"] = request.images
        owned = self._client is None
        client = self._client or httpx.AsyncClient()
        try:
            resp = await client.post(
                f"{self.base_url}/v1/systemone", json=body, headers=headers, timeout=self.timeout_s
            )
        finally:
            if owned:
                await client.aclose()
        if resp.status_code // 100 != 2:
            raise CallError(KIND_HTTP, f"{resp.status_code}: {resp.text[:200]}", resp.status_code)
        try:
            d = resp.json()
            return WireResponse(
                model=d.get("model", ""),
                answers={k: Answer.from_dict(v) for k, v in (d.get("answers") or {}).items()},
                usage=Usage(**{k: int(v) for k, v in (d.get("usage") or {}).items()}),
            )
        except (ValueError, TypeError, AttributeError) as e:
            raise CallError(KIND_DECODE, str(e)) from e

    async def probe(self) -> str:
        """``ready``, ``not_ready`` or ``unknown`` (no /readyz, as on hosted APIs)."""
        try:
            async with httpx.AsyncClient(timeout=2.0) as c:
                r = await c.get(f"{self.base_url}/readyz")
        except httpx.HTTPError:
            return "not_ready"
        if r.status_code == 200:
            return "ready"
        return "unknown" if r.status_code in (404, 405) else "not_ready"


DEFAULT_WORKERS_AI_BASE = "https://api.cloudflare.com/client/v4"


def workers_image(img: str) -> str | None:
    """A raw base64 image (what the Jev wire and Ollama take) as the data URL Workers AI requires, labeled
    by what the bytes are. A data URL passes through. ``None`` for anything that is not a PNG, JPEG or WebP,
    which is all Clef reads."""
    if img[:5].lower() == "data:":
        return img
    img = "".join(img.split())
    try:
        head = base64.b64decode(img[:24], validate=True)  # 18 bytes: enough to tell
    except (ValueError, binascii.Error):
        return None
    if head[:8] == b"\x89PNG\r\n\x1a\n":
        mime = "image/png"
    elif head[:3] == b"\xff\xd8\xff":
        mime = "image/jpeg"
    elif head[:4] == b"RIFF" and head[8:12] == b"WEBP":
        mime = "image/webp"
    else:
        return None
    return f"data:{mime};base64,{img}"


class WorkersAI:
    """A System One model hosted on Cloudflare Workers AI (Clef: ``@cf/cloudflare/clef`` and
    ``@cf/cloudflare/clef-flash``). Not the ``/v1/systemone`` wire: the model is in the URL, the answers
    come in Cloudflare's ``{"result", "success", "errors"}`` envelope, and the credential is an account
    API token.

        POST {base_url}/accounts/{account_id}/ai/run/{model}     Authorization: Bearer {api_key}

    Checked against Cloudflare: Clef's input and output schemas (``cf ai get-model-schema``), its
    requirement that images be data URLs (a raw base64 string is refused), and a real call through a
    Worker. NOT yet checked: the REST envelope around the output, so decoding is tolerant (answers under
    ``result`` or at the top level, and a missing answer type is filled from the question).
    """

    def __init__(
        self,
        account_id: str,
        model: str,
        api_key: str = "",
        base_url: str = "",
        timeout_s: float | None = None,
        client: httpx.AsyncClient | None = None,
    ):
        self.account_id, self.model, self.api_key = account_id, model, api_key
        self.base_url = (base_url or DEFAULT_WORKERS_AI_BASE).rstrip("/")
        self.timeout_s = timeout_s
        self._client = client

    async def call(self, request: Request) -> WireResponse:
        headers = {"Content-Type": "application/json"}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"
        # `model` is a required field of Clef's input as well as being in the URL (its schema:
        # `cf ai get-model-schema --model @cf/cloudflare/clef-flash`).
        body: dict[str, Any] = {
            "model": self.model.rsplit("/", 1)[-1],
            "state": request.state,
            "questions": request.to_dict()["questions"],
        }
        if request.images:
            uris = []
            for i, img in enumerate(request.images):
                uri = workers_image(img)
                if uri is None:
                    raise CallError(KIND_UNSUPPORTED, f"images[{i}] is not a PNG, JPEG or WebP image")
                uris.append(uri)
            body["images"] = uris
        url = f"{self.base_url}/accounts/{self.account_id}/ai/run/{self.model}"
        owned = self._client is None
        client = self._client or httpx.AsyncClient()
        try:
            resp = await client.post(url, json=body, headers=headers, timeout=self.timeout_s)
        finally:
            if owned:
                await client.aclose()
        if resp.status_code // 100 != 2:
            raise CallError(KIND_HTTP, f"{resp.status_code}: {resp.text[:200]}", resp.status_code)
        try:
            d = resp.json()
            if d.get("success") is False:  # a 200 that says it failed; the codes are Cloudflare's numeric ones
                codes = ",".join(str(e.get("code")) for e in d.get("errors") or [])
                raise CallError(KIND_HTTP, f"workers-ai reported failure (codes {codes})")
            payload = d.get("result") if isinstance(d.get("result"), dict) else d
            raw = payload.get("answers") or {}
            if not raw:
                raise CallError(KIND_DECODE, "no answers in the Workers AI response")
            answers = {}
            for k, v in raw.items():
                a = Answer.from_dict(v)
                if not a.type and k in request.questions:  # take the type from the question when it is left out
                    a.type = request.questions[k].type
                answers[k] = a
            return WireResponse(
                model=payload.get("model", ""),
                answers=answers,
                usage=Usage(**{k: int(v) for k, v in (payload.get("usage") or {}).items()}),
            )
        except CallError:
            raise
        except (ValueError, TypeError, AttributeError) as e:
            raise CallError(KIND_DECODE, str(e)) from e


class Unsupported:
    """Stands in for providers not implemented yet (e.g. ``openai``)."""

    def __init__(self, provider: str):
        self.provider = provider

    async def call(self, request: Request) -> WireResponse:
        raise CallError(KIND_UNSUPPORTED, f"provider {self.provider} is not implemented")


class Registry(dict):
    """alias -> Backend."""

    def names(self) -> list[str]:
        return sorted(self)

    async def call(self, alias: str, request: Request, remaining=lambda: None) -> Result | Failure:
        """One backend call: a normalized Result or a classified Failure. ``remaining``
        returns the seconds left in the plan's overall budget (None = unbounded)."""
        start = time.perf_counter()

        def ms() -> int:
            return int((time.perf_counter() - start) * 1000)

        backend = self.get(alias)
        if backend is None:
            return Failure(alias, ErrorBody(KIND_UNSUPPORTED, f"unknown model {alias}"))
        try:
            wire = await asyncio.wait_for(backend.call(request), timeout=remaining())
        except CallError as e:
            return Failure(alias, ErrorBody(e.kind, e.message), ms())
        except (asyncio.TimeoutError, httpx.TimeoutException) as e:
            return Failure(alias, ErrorBody(KIND_TIMEOUT, str(e) or "deadline exceeded"), ms())
        except Exception as e:  # noqa: BLE001 - any other failure is a transport problem
            return Failure(alias, ErrorBody(KIND_TRANSPORT, str(e) or type(e).__name__), ms())
        return Result(alias, normalize(alias, wire.answers), wire.usage, ms(), wire.cost_usd, wire.cached)

    async def status(self, alias: str) -> str:
        b: Any = self.get(alias)
        if hasattr(b, "probe"):
            return await b.probe()
        return "unsupported" if isinstance(b, Unsupported) else "unknown"
