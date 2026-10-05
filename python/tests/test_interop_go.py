"""Cross-language check: the real Go gateway and the Python SDK must agree.

Builds ../go/cmd/ignatius, runs it against fake Jev-compatible upstreams, and
compares its /v1/route results with the same plans run in-process in Python.
Skipped when `go` is not installed."""

import asyncio
import json
import shutil
import socket
import subprocess
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

from ignatius import GatewayClient, GatewayError, Plan, Priced, Question, Registry, Request, Routed, SystemOne, run

GO_DIR = Path(__file__).resolve().parents[2] / "go"
pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not installed")

# (noul P(yes), choice probabilities, choice confidence or None)
UPSTREAMS = {
    "cheap": (0.55, {"a": 0.6, "b": 0.4}, None),
    "smart": (0.97, {"a": 0.05, "b": 0.95}, 0.95),
}


# USD per million tokens (input, output); every fake reports 4 input and 1 output tokens
PRICES = {"cheap": (0.5, 0.1), "smart": (1.0, 0.25)}


def serve_fake(noul_p, probs, conf):
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            self.send_response(200 if self.path == "/readyz" else 404)
            self.end_headers()

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            answers = {}
            for qid, q in body["questions"].items():
                if q["type"] == "noul":
                    answers[qid] = {"type": "noul", "noul": noul_p}
                else:
                    a = {"type": "choice", "choice": max(probs, key=probs.get), "probabilities": probs}
                    if conf is not None:
                        a["confidence"] = conf
                    answers[qid] = a
            out = json.dumps({"model": "fake", "answers": answers, "usage": {"input_tokens": 4, "output_tokens": 1}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(out)

    srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Stack:
    """What the fixture yields. It still unpacks as ``base, urls``; ``config`` is the
    TOML the Go gateway was started with, so the Python SDK can load the very same file."""

    def __init__(self, base, urls, config):
        self.base, self.urls, self.config = base, urls, config

    def __iter__(self):
        return iter((self.base, self.urls))


@pytest.fixture(scope="module")
def stack(tmp_path_factory):
    tmp = tmp_path_factory.mktemp("interop")
    binary = tmp / "ignatius"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/ignatius"], cwd=GO_DIR, check=True, capture_output=True)
    servers = {name: serve_fake(*spec) for name, spec in UPSTREAMS.items()}
    urls = {n: f"http://127.0.0.1:{s.server_address[1]}" for n, s in servers.items()}
    port = free_port()
    toml = f'listen = "127.0.0.1:{port}"\napi_key_env = "IGN_TEST_KEY"\ndefault_route = "cheap"\n'
    toml += """
[profiles]
fast = "cheap"
best = "smart"

[routes.profiled]
mode = "cascade"
tiers = [ { model = "fast", threshold = 0.5 }, "best" ]
"""
    for n, u in urls.items():
        pin, pout = PRICES[n]
        toml += (f'\n[models.{n}]\nprovider = "systemone"\nbase_url = "{u}"\nmodel = "jev-latest"\n'
                 f'price_input_per_mtok = {pin}\nprice_output_per_mtok = {pout}\n')
    (tmp / "gw.toml").write_text(toml)
    proc = subprocess.Popen([str(binary), "serve", "--config", str(tmp / "gw.toml")],
                            env={"IGN_TEST_KEY": "k1", "PATH": "/usr/bin:/bin"}, stderr=subprocess.PIPE)
    base = f"http://127.0.0.1:{port}"
    for _ in range(50):
        try:
            urllib.request.urlopen(base + "/healthz", timeout=0.5)
            break
        except OSError:
            time.sleep(0.1)
    else:
        proc.kill()
        pytest.fail(f"gateway did not start: {proc.stderr.read().decode()}")
    yield Stack(base, urls, tmp / "gw.toml")
    proc.terminate()
    proc.wait(timeout=5)
    for s in servers.values():
        s.shutdown()


def request() -> Request:
    return Request("a ticket", {"urgent": Question("noul", "Urgent?"),
                                "dept": Question("choice", "Which team?", ["a", "b"])})


def strip_volatile(d):
    """Latencies differ run to run; everything else must match exactly."""
    if isinstance(d, dict):
        return {k: strip_volatile(v) for k, v in d.items() if k != "latency_ms"}
    if isinstance(d, list):
        return [strip_volatile(x) for x in d]
    return d


def local_registry(urls) -> Registry:
    return Registry({n: Priced(SystemOne(u, "jev-latest"), *PRICES[n]) for n, u in urls.items()})


PLANS = {
    "cascade with a per-tier threshold": {"mode": "cascade", "tiers": [{"model": "cheap", "threshold": 0.5}, "smart"]},
    "cascade, per-type thresholds": {"mode": "cascade", "tiers": [{"model": "cheap", "threshold": {"noul": 0.9, "choice": 0.5}}, "smart"]},
    "fan-out, vote": {"mode": "fan_out", "models": ["cheap", "smart"], "reduce": "vote"},
    "fan-out, mean": {"mode": "fan_out", "models": ["smart", "cheap"], "reduce": "mean"},
    "fan-out, most_confident": {"mode": "fan_out", "models": ["cheap", "smart"], "reduce": "most_confident"},
    "fan-out, raw": {"mode": "fan_out", "models": ["cheap", "smart"]},
    "single": {"mode": "single", "model": "smart"},
}


@pytest.mark.parametrize("name", PLANS)
def test_go_gateway_and_python_agree(stack, name):
    base, urls = stack
    plan = Plan.from_dict(PLANS[name])
    over_http = asyncio.run(GatewayClient(base, "k1").route(request(), plan=plan))
    in_process = asyncio.run(run(local_registry(urls), request(), plan))
    assert strip_volatile(over_http.to_dict()) == strip_volatile(in_process.to_dict())
    assert over_http.ok


def test_go_trace_detail_decodes_in_python(stack):
    base, _ = stack
    r = asyncio.run(GatewayClient(base, "k1").route(request(), plan=Plan.from_dict(PLANS["cascade with a per-tier threshold"])))
    hop0 = r.trace[0]
    by_id = {q.id: q for q in hop0.detail}
    assert by_id["urgent"].escalated and by_id["urgent"].threshold == 0.5 and by_id["urgent"].confidence == pytest.approx(0.1)
    assert not by_id["dept"].escalated  # 0.6 >= 0.5 settles at the cheap tier
    assert r.answers["urgent"].model == "smart" and r.answers["dept"].model == "cheap"


def test_l1_inline_route_and_default(stack):
    base, _ = stack
    c = GatewayClient(base, "k1")
    qs = {k: q.to_dict() for k, q in request().questions.items()}
    r = asyncio.run(c.systemone("a ticket", qs, "cascade:cheap@0.5>smart"))
    assert r["ignatius"]["mode"] == "cascade" and r["ignatius"]["sources"]["urgent"]["model"] == "smart"
    assert "confidence" not in r["answers"]["urgent"]  # noul carries no confidence on the Jev wire
    r = asyncio.run(c.systemone("a ticket", qs, "jev-latest"))  # default_route = cheap
    assert r["ignatius"]["sources"]["urgent"]["model"] == "cheap"


def test_auth_and_plan_errors(stack):
    base, _ = stack
    with pytest.raises(GatewayError) as e:
        asyncio.run(GatewayClient(base, "wrong").route(request(), route="cheap"))
    assert e.value.status == 401
    with pytest.raises(GatewayError) as e:
        asyncio.run(GatewayClient(base).route(request(), route="cheap"))
    assert e.value.status == 403
    with pytest.raises(GatewayError) as e:
        asyncio.run(GatewayClient(base, "k1").route(request(), plan=Plan("cascade")))
    assert e.value.status == 422 and "cascade requires tiers" in json.dumps(e.value.body)


def test_cost_matches_exactly_across_languages(stack):
    base, urls = stack
    plan = Plan.from_dict(PLANS["cascade with a per-tier threshold"])
    over_http = asyncio.run(GatewayClient(base, "k1").route(request(), plan=plan))
    in_process = asyncio.run(run(local_registry(urls), request(), plan))
    assert over_http.cost_usd is not None and over_http.cost_usd == in_process.cost_usd  # exact, not approx
    assert [r.cost_usd for r in over_http.results] == [r.cost_usd for r in in_process.results]
    expected = sum(4 * PRICES[r.model][0] / 1e6 + 1 * PRICES[r.model][1] / 1e6 for r in over_http.results)
    assert over_http.cost_usd == pytest.approx(expected)


@pytest.mark.parametrize("model", ["fast", "best", "profiled", "cascade:fast>best", "cascade:best>fast", "fan-out:fast,best|mean"])
def test_profiles_resolve_identically_in_go_and_python(stack, model):
    """The same TOML file, the real Go gateway and the Python SDK: same profiles, same result."""
    from ignatius import Ignatius

    over_http = asyncio.run(GatewayClient(stack.base, "k1").route(request(), route=model))
    in_process = asyncio.run(Ignatius.from_config(str(stack.config)).run(request(), model=model))
    assert over_http.ok and strip_volatile(over_http.to_dict()) == strip_volatile(in_process.to_dict())
    # attributed to real models only: a profile name never shows up as a model
    assert {r.model for r in over_http.results} <= {"cheap", "smart"}
    assert {a.model for a in over_http.answers.values()} <= {"cheap", "smart", "ensemble"}


def test_go_gateway_lists_the_profiles_the_python_sdk_loaded(stack):
    from ignatius import load_config

    req = urllib.request.Request(stack.base + "/v1/profiles", headers={"Authorization": "Bearer k1"})
    listed = {p["name"]: p["target"] for p in json.load(urllib.request.urlopen(req))["profiles"]}
    assert listed == load_config(str(stack.config)).profiles == {"fast": "cheap", "best": "smart"}
