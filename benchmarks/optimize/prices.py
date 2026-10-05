"""Per-token prices for the reflection model, from LiteLLM's price file.

LiteLLM publishes `model_prices_and_context_window.json`, keyed by provider and model. It is community maintained and
can be wrong or stale, so for OpenRouter the price is also checked against OpenRouter's own model list, which is
authoritative for what it will charge. Prices are USD per million tokens.
"""

from __future__ import annotations

import json
import time
from pathlib import Path

import httpx

LITELLM_URL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
OPENROUTER_MODELS = "https://openrouter.ai/api/v1/models"
CACHE = Path.home() / ".cache" / "ignatius-optimize" / "litellm_prices.json"
MAX_AGE = 24 * 3600

# The key prefixes to try in LiteLLM's file, per provider of ours.
PREFIXES = {
    "anthropic": ["", "anthropic/"],
    "openrouter": ["openrouter/"],
    "fireworks": ["fireworks_ai/", "fireworks_ai/accounts/fireworks/models/"],
}


def load_litellm() -> dict:
    """LiteLLM's price file, cached for a day. A stale cache is used if the fetch fails; {} if there is nothing."""
    try:
        if CACHE.exists() and time.time() - CACHE.stat().st_mtime < MAX_AGE:
            return json.loads(CACHE.read_text())
    except (OSError, ValueError):
        pass
    try:
        r = httpx.get(LITELLM_URL, timeout=30, follow_redirects=True)
        r.raise_for_status()
        data = r.json()
        CACHE.parent.mkdir(parents=True, exist_ok=True)
        CACHE.write_text(json.dumps(data))
        return data
    except (httpx.HTTPError, ValueError, OSError):
        try:
            return json.loads(CACHE.read_text())
        except (OSError, ValueError):
            return {}


def litellm_price(provider: str, model: str, table: dict | None = None) -> tuple[float, float] | None:
    table = load_litellm() if table is None else table
    for prefix in PREFIXES.get(provider, []):
        e = table.get(prefix + model)
        if e and e.get("input_cost_per_token") is not None and e.get("output_cost_per_token") is not None:
            return e["input_cost_per_token"] * 1e6, e["output_cost_per_token"] * 1e6
    return None


def openrouter_price(model: str) -> tuple[float, float] | None:
    try:
        r = httpx.get(OPENROUTER_MODELS, timeout=30)
        r.raise_for_status()
        for m in r.json()["data"]:
            if m["id"] == model:
                return float(m["pricing"]["prompt"]) * 1e6, float(m["pricing"]["completion"]) * 1e6
    except (httpx.HTTPError, ValueError, KeyError):
        pass
    return None


def resolve(provider: str, model: str, price_in: float = 0.0, price_out: float = 0.0) -> tuple[float, float, str]:
    """(input, output, where it came from). Flags win. Otherwise LiteLLM's file, and for OpenRouter its own list when
    the two disagree. (0, 0, 'unknown') when nothing has the model."""
    if price_in or price_out:
        return price_in, price_out, "flags"
    found = litellm_price(provider, model)
    if provider == "openrouter":
        live = openrouter_price(model)
        if live and (found is None or any(abs(a - b) > 0.02 * max(a, b) for a, b in zip(found, live))):
            return live[0], live[1], "openrouter's own model list" + (" (litellm's file disagrees)" if found else "")
    if found:
        return found[0], found[1], "litellm's price file"
    return 0.0, 0.0, "unknown"
