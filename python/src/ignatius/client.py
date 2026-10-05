"""The high-level entry points: in-process ``Ignatius`` and the ``GatewayClient``."""

from __future__ import annotations

import asyncio
from collections.abc import Callable
from typing import Any

import httpx

from .config import Config, Router, load_config
from .run import run as run_plan
from .types import Answer, Plan, Request, Routed


class Ignatius:
    """Runs plans in-process against the models in a config (no gateway needed)."""

    def __init__(self, config: Config):
        self.config = config
        self.router = Router(config)

    @classmethod
    def from_config(cls, path: str) -> Ignatius:
        return cls(load_config(path))

    async def run(
        self,
        request: Request,
        model: str | None = None,
        plan: Plan | None = None,
        escalate_if: Callable[[str, Answer], bool] | None = None,
    ) -> Routed:
        """Route ``request`` by ``model`` (route name, alias or inline route; empty
        means the default route) or an explicit ``plan``."""
        if plan is not None and model:
            raise ValueError("pass either model or plan, not both")
        request.validate()
        p = plan if plan is not None else self.router.resolve(model)
        return await run_plan(self.config.registry, request, p, escalate_if)

    def run_sync(self, *args: Any, **kwargs: Any) -> Routed:
        return asyncio.run(self.run(*args, **kwargs))


class GatewayError(Exception):
    """The gateway rejected the call (a caller mistake or auth failure)."""

    def __init__(self, status: int, body: Any):
        super().__init__(f"gateway returned HTTP {status}: {body}")
        self.status, self.body = status, body


class GatewayClient:
    """Typed client for a running Ignatius gateway's native ``/v1/route`` (L2)."""

    def __init__(self, base_url: str, api_key: str = "", timeout_s: float = 60.0):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout_s = timeout_s

    def _headers(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {self.api_key}"} if self.api_key else {}

    async def route(self, request: Request, plan: Plan | None = None, route: str | None = None) -> Routed:
        if (plan is None) == (route is None):
            raise ValueError('exactly one of "plan" and "route" is required')
        body: dict[str, Any] = {"request": request.to_dict()}
        if plan is not None:
            body["plan"] = plan.to_dict()
        else:
            body["route"] = route
        async with httpx.AsyncClient(timeout=self.timeout_s) as c:
            r = await c.post(f"{self.base_url}/v1/route", json=body, headers=self._headers())
        if r.status_code != 200:
            raise GatewayError(r.status_code, _json_or_text(r))
        return Routed.from_dict(r.json())

    async def feedback(
        self,
        request_id: str,
        question_id: str,
        verdict: str,
        correct: Any = None,
        source: str = "human",
    ) -> None:
        """Say whether one question's decision was good (SPEC 13.2).

        ``request_id`` is ``Routed.request_id`` or the ``ignatius.request_id`` of an L1
        response. ``verdict`` is ``"good"`` or ``"bad"``; ``correct`` is the right answer
        (a choice label, a score level, or a noul bool or probability) and is the most
        useful thing to send with a ``"bad"`` verdict. ``source`` is ``"human"`` or
        ``"automated"``. Raises GatewayError: 404 ``feedback_disabled`` if the gateway has
        no store, 404 ``unknown_request`` if there is no such request or question (or it has aged out).
        Any client may give feedback on any request.
        """
        body: dict[str, Any] = {"request_id": request_id, "question_id": question_id, "verdict": verdict, "source": source}
        if correct is not None:
            body["correct"] = correct
        async with httpx.AsyncClient(timeout=self.timeout_s) as c:
            r = await c.post(f"{self.base_url}/v1/ignatius/feedback", json=body, headers=self._headers())
        if r.status_code != 200:
            raise GatewayError(r.status_code, _json_or_text(r))

    async def systemone(
        self, state: Any, questions: dict[str, Any], model: str = "", images: list[str] | None = None
    ) -> dict[str, Any]:
        """Jev-compatible L1 call; returns the raw response (answers, usage, ignatius).

        ``images`` are base64 PNG, JPEG or WebP strings shared by every question (SPEC 3.1). A model
        without an image allowance in its ``[limits]`` refuses them with a 422 ``images_not_supported``.
        """
        body: dict[str, Any] = {"state": state, "model": model, "questions": questions}
        if images:
            body["images"] = images
        async with httpx.AsyncClient(timeout=self.timeout_s) as c:
            r = await c.post(f"{self.base_url}/v1/systemone", json=body, headers=self._headers())
        if r.status_code != 200:
            raise GatewayError(r.status_code, _json_or_text(r))
        return r.json()


def _json_or_text(r: httpx.Response) -> Any:
    try:
        return r.json()
    except ValueError:
        return r.text
