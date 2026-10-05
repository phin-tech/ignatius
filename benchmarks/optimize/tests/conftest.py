import pytest

import prices


@pytest.fixture(autouse=True)
def no_price_lookups(monkeypatch):
    """No test reaches LiteLLM's file or OpenRouter. A test that wants a price file sets its own."""
    monkeypatch.setattr(prices, "load_litellm", lambda: {})
    monkeypatch.setattr(prices, "openrouter_price", lambda model: None)
