"""Ignatius: orchestrate System One decision models (single, fan-out, cascade)."""

from .backend import Backend, CallError, Registry, SystemOne, Unsupported, WireResponse, WithConfidence, WorkersAI, derived_confidence
from .client import GatewayClient, GatewayError, Ignatius
from .config import Config, ResolveError, Router, load_config, substitute_profiles, validate_profiles
from .inline import INLINE_GRAMMAR, NotInline, parse_inline
from .resilience import Breaker, BreakerConfig, Cache, CacheConfig, Cached, Limited, LimitsConfig, Priced
from .run import run, validate_plan
from .types import (
    Answer,
    Failure,
    Hop,
    HopQuestion,
    Plan,
    PlanError,
    Question,
    Request,
    Result,
    Routed,
    Threshold,
    Tier,
)

__all__ = [
    "Answer", "Backend", "Breaker", "BreakerConfig", "Cache", "CacheConfig", "Cached", "CallError", "Config", "Failure", "GatewayClient", "GatewayError", "Hop",
    "HopQuestion", "INLINE_GRAMMAR", "Ignatius", "NotInline", "Plan", "PlanError", "Question",
    "Limited", "LimitsConfig", "Priced", "Registry", "Request", "ResolveError", "Result", "Routed", "Router", "SystemOne", "Threshold",
    "Tier", "Unsupported", "WireResponse", "WithConfidence", "WorkersAI", "derived_confidence", "load_config", "parse_inline", "run", "substitute_profiles", "validate_plan", "validate_profiles",
]
