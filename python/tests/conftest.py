import asyncio
from pathlib import Path

import pytest

from ignatius import Answer, CallError, Limited, LimitsConfig, Priced, WireResponse, WithConfidence
from ignatius.types import Usage

FIXTURES = sorted((Path(__file__).resolve().parents[2] / "spec" / "fixtures").glob("*.json"))


class MockBackend:
    """Replays a fixture's raw wire answers so normalization is covered too."""

    def __init__(self, spec: dict):
        self.spec = spec

    async def call(self, request):
        await asyncio.sleep(self.spec.get("latency_ms", 0) / 1000)
        if err := self.spec.get("error"):
            raise CallError(err["kind"], err["message"])
        return WireResponse(
            model="mock",
            answers={k: Answer.from_dict(v) for k, v in self.spec["answers"].items()},
            usage=Usage(**self.spec.get("usage", {})),
        )

    def build(self):
        """A mock with a confidence policy, and a priced one, go through the real decorators."""
        b = self
        if policy := self.spec.get("confidence"):
            b = WithConfidence(b, policy)
        if p := self.spec.get("price"):
            b = Priced(b, p.get("input_per_mtok", 0.0), p.get("output_per_mtok", 0.0))
        lim = self.spec.get("limits") or {}
        return Limited(b, LimitsConfig(lim.get("max_questions", 0), lim.get("max_images", 0)))  # as in load_config


@pytest.fixture
def mock_backend():
    return MockBackend
