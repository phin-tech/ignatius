"""TOML config shared with the Go gateway, and route resolution (SPEC 3, 6.1)."""

from __future__ import annotations

import os
import re
import tomllib
from collections.abc import Mapping
from dataclasses import dataclass, field, replace

from .backend import CONFIDENCE_DERIVED, Registry, SystemOne, Unsupported, WithConfidence, WorkersAI, valid_confidence_policy
from .resilience import Breaker, BreakerConfig, Cache, Cached, CacheConfig, Limited, LimitsConfig, Priced
from .inline import INLINE_GRAMMAR, NotInline, parse_inline
from .types import MODE_SINGLE, Plan

DEFAULT_UPSTREAM_MODEL = "jev-latest"


class ResolveError(ValueError):
    """The model/route string names nothing (HTTP 422 territory)."""

    def __init__(self, message: str, routes: list[str], models: list[str], grammar: str,
                 profiles: list[str] | None = None):
        super().__init__(message)
        self.routes, self.models, self.grammar = routes, models, grammar
        self.profiles = profiles or []


MAX_PROFILES = 64
# A profile name is a safe, bounded token: no inline-route separators (: , > @ |).
PROFILE_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9_.-]{0,63}$")
# These are metric label values or protocol markers, so a profile may not take them.
RESERVED_PROFILE_NAMES = frozenset({"inline", "plan", "unknown", "none", "jev-latest"})


def validate_profiles(profiles: dict[str, str], registry: Registry, routes: dict[str, Plan]) -> None:
    """Raise ValueError unless every profile is a well-formed name pointing at a model alias."""
    if len(profiles) > MAX_PROFILES:
        raise ValueError(f"at most {MAX_PROFILES} profiles are allowed, got {len(profiles)}")
    for name, target in profiles.items():
        if not PROFILE_NAME_RE.match(name):
            raise ValueError(f"profile name {name!r} must be lowercase letters, digits, '_', '.' or '-' "
                             "(at most 64 characters, starting with a letter or digit)")
        if name in RESERVED_PROFILE_NAMES:
            raise ValueError(f"profile name {name!r} is reserved")
        if name in registry:
            raise ValueError(f"profile {name!r} clashes with a model alias of the same name")
        if name in routes:
            raise ValueError(f"profile {name!r} clashes with a route of the same name")
        if target not in registry:
            raise ValueError(f"profile {name!r} targets {target!r}, which is not a model alias "
                             "(profiles point at models, not at other profiles or routes)")


def substitute_profiles(plan: Plan, profiles: dict[str, str]) -> Plan:
    """A copy of ``plan`` with profile names rewritten to the real aliases they point
    at, so everything downstream (breaker, cache, cost, stats) sees real models."""
    m = lambda n: profiles.get(n, n)  # noqa: E731
    return replace(
        plan,
        model=m(plan.model),
        models=[m(x) for x in plan.models],
        tiers=[replace(t, model=m(t.model)) for t in plan.tiers],
    )


@dataclass
class Config:
    registry: Registry
    routes: dict[str, Plan] = field(default_factory=dict)
    profiles: dict[str, str] = field(default_factory=dict)
    default_route: str = ""
    allow_inline_routes: bool = True
    max_inline_models: int = 5
    request_timeout_ms: int = 30000


def load_config(path: str, environ: Mapping[str, str] | None = None) -> Config:
    """Load models and routes from the same TOML the Go gateway reads. API keys
    come from the env vars the file names, never from the file."""
    env = os.environ if environ is None else environ
    with open(path, "rb") as f:
        doc = tomllib.load(f)
    models = doc.get("models") or {}
    if not models:
        raise ValueError(f"{path} defines no models")
    cache_doc = doc.get("cache") or {}
    cache_cfg = CacheConfig(
        enabled=bool(cache_doc.get("enabled", False)),
        ttl_ms=int(cache_doc.get("ttl_ms", 0)),
        max_entries=int(cache_doc.get("max_entries", 0)),
    )
    reg = Registry()
    cache: Cache | None = None  # one LRU shared by every cached model; keys include the alias
    for alias, m in models.items():
        provider = m.get("provider")
        if provider not in ("systemone", "workers-ai"):
            reg[alias] = Unsupported(m.get("provider", ""))
            continue
        if provider == "systemone" and not m.get("base_url"):
            raise ValueError(f"model {alias!r}: base_url is required")
        if provider == "workers-ai":
            if not (m.get("model") and m.get("account_id_env") and m.get("api_key_env")):
                raise ValueError(f'model {alias!r}: workers-ai needs model (e.g. "@cf/cloudflare/clef-flash"), '
                                 "account_id_env and api_key_env")
            if not env.get(m["account_id_env"]):
                raise ValueError(f"model {alias!r}: environment variable {m['account_id_env']} is not set")
        upstream = m.get("model", DEFAULT_UPSTREAM_MODEL)
        policy = m.get("confidence", "")
        if not valid_confidence_policy(policy):
            raise ValueError(f'model {alias!r}: confidence must be "provider" or "derived", got {policy!r}')
        backend = SystemOne(
            base_url=m.get("base_url", ""),
            model=upstream,
            api_key=env.get(m.get("api_key_env", ""), "") if m.get("api_key_env") else "",
            auth_header=m.get("auth_header", ""),
            timeout_s=(m.get("timeout_ms") or 10000) / 1000,
        )
        if provider == "workers-ai":
            backend = WorkersAI(
                account_id=env[m["account_id_env"]],
                model=m["model"],
                api_key=env.get(m["api_key_env"], ""),
                base_url=m.get("base_url", ""),
                timeout_s=(m.get("timeout_ms") or 10000) / 1000,
            )
        if policy == CONFIDENCE_DERIVED:
            backend = WithConfidence(backend, policy)
        # Decorators, outermost first: cache, circuit breaker, pricing, confidence policy, the backend.
        priced = "price_input_per_mtok" in m or "price_output_per_mtok" in m
        if priced:
            backend = Priced(backend, float(m.get("price_input_per_mtok", 0)), float(m.get("price_output_per_mtok", 0)))
        b = m.get("breaker") or {}
        if b.get("enabled", True):
            backend = Breaker(backend, BreakerConfig(
                failure_threshold=int(b.get("failure_threshold", 0)), cooldown_ms=int(b.get("cooldown_ms", 0))))
        if m.get("cache", cache_cfg.enabled):
            cache = cache if cache is not None else Cache(cache_cfg)  # not `or`: an empty Cache is falsy
            backend = Cached(backend, cache, alias, upstream, priced)
        lim = m.get("limits") or {}
        limits = LimitsConfig(int(lim.get("max_questions", 0)), int(lim.get("max_images", 0)))
        if limits.max_questions < 0 or limits.max_images < 0:
            raise ValueError(f"model {alias!r}: limits cannot be negative")
        reg[alias] = Limited(backend, limits)  # outermost: see Limited
    routes = {name: Plan.from_dict(r) for name, r in (doc.get("routes") or {}).items()}
    cfg = Config(
        registry=reg,
        routes=routes,
        profiles={str(k): str(v) for k, v in (doc.get("profiles") or {}).items()},
        default_route=doc.get("default_route", ""),
        allow_inline_routes=doc.get("allow_inline_routes", True),
        max_inline_models=doc.get("max_inline_models") or 5,
        request_timeout_ms=doc.get("request_timeout_ms") or 30000,
    )
    Router(cfg)  # validate routes and default_route now, not at first request
    return cfg


class Router:
    """Resolves a model string to a Plan: named route, then alias, then inline."""

    def __init__(self, cfg: Config):
        from .run import validate_plan  # local import: run imports types only

        self.cfg = cfg
        validate_profiles(cfg.profiles, cfg.registry, cfg.routes)
        for name, plan in cfg.routes.items():  # a route may name a profile: validate the substituted plan
            try:
                validate_plan(substitute_profiles(plan, cfg.profiles), cfg.registry)
            except ValueError as e:
                raise ValueError(f"route {name!r}: {e}") from e
        d = cfg.default_route
        if d and d not in cfg.routes and d not in cfg.registry and d not in cfg.profiles:
            raise ValueError(f"default_route {d!r} is not a route, profile or model")

    def _unknown(self, msg: str) -> ResolveError:
        return ResolveError(
            msg,
            sorted(self.cfg.routes),
            self.cfg.registry.names(),
            INLINE_GRAMMAR if self.cfg.allow_inline_routes else "",
            sorted(self.cfg.profiles),
        )

    def resolve(self, model: str | None) -> Plan:
        if not model or model == DEFAULT_UPSTREAM_MODEL:
            if not self.cfg.default_route:
                raise self._unknown("no model given and no default_route configured")
            model = self.cfg.default_route
        p = self.cfg.profiles
        if model in self.cfg.routes:
            return substitute_profiles(self.cfg.routes[model], p)
        if model in p:
            return Plan(MODE_SINGLE, model=p[model])
        if model in self.cfg.registry:
            return Plan(MODE_SINGLE, model=model)
        if self.cfg.allow_inline_routes:
            try:
                return substitute_profiles(parse_inline(model, self.cfg.max_inline_models), p)
            except NotInline:
                pass
            except ValueError as e:
                raise self._unknown(str(e)) from e
        raise self._unknown(f"no such route, profile or model: {model}")
