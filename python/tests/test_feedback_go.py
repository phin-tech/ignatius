"""End to end: the Python SDK sends feedback to a real Go gateway with a SQLite store, and
the gateway's own `export` and `calibrate` commands read it back (SPEC 13).
Skipped when `go` is not installed."""

import asyncio
import json
import shutil
import socket
import sqlite3
import subprocess
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

from ignatius import GatewayClient, GatewayError, Question, Request

GO_DIR = Path(__file__).resolve().parents[2] / "go"
pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not installed")

STATE = "SDK-SENTINEL-ticket"


def serve_fake(delay_s=0.0):
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            self.send_response(200 if self.path == "/readyz" else 404)
            self.end_headers()

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            time.sleep(delay_s)
            answers = {q: {"type": "choice", "choice": "a", "probabilities": {"a": 0.9, "b": 0.1}, "confidence": 0.8}
                       for q in body["questions"]}
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
    def __init__(self, base, binary, config, env, db):
        self.base, self.binary, self.config, self.env, self.db = base, binary, config, env, db

    def cli(self, *args) -> subprocess.CompletedProcess:
        return subprocess.run([str(self.binary), *args, "--config", str(self.config)], env=self.env,
                              capture_output=True, text=True)


@pytest.fixture(scope="module")
def stack(tmp_path_factory):
    tmp = tmp_path_factory.mktemp("feedback")
    binary = tmp / "ignatius"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/ignatius"], cwd=GO_DIR, check=True, capture_output=True)
    fake = serve_fake()
    port = free_port()
    db = tmp / "store.db"
    (tmp / "gw.toml").write_text(f"""
listen = "127.0.0.1:{port}"
api_key_env = "IGN_TEST_KEY"

[store]
driver = "sqlite"

[[clients]]
name = "kept"
key_env = "KEPT_KEY"
store_content = true

[[clients]]
name = "plain"
key_env = "PLAIN_KEY"

[models.m]
provider = "systemone"
base_url = "http://127.0.0.1:{fake.server_address[1]}"
model = "jev-latest"
""")
    env = {"IGN_TEST_KEY": "admin", "KEPT_KEY": "kk", "PLAIN_KEY": "pk", "IGNATIUS_STORE_DSN": str(db), "PATH": "/usr/bin:/bin"}
    proc = subprocess.Popen([str(binary), "serve", "--config", str(tmp / "gw.toml")], env=env, stderr=subprocess.PIPE)
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
    yield Stack(base, binary, tmp / "gw.toml", env, db)
    proc.terminate()
    proc.wait(timeout=5)
    fake.shutdown()


def request() -> Request:
    return Request(STATE, {"dept": Question("choice", "Which team?", ["a", "b"])})


def test_routed_carries_the_request_id_and_feedback_is_accepted(stack):
    c = GatewayClient(stack.base, "kk")
    routed = asyncio.run(c.route(request(), route="m"))
    assert routed.request_id
    asyncio.run(c.feedback(routed.request_id, "dept", "bad", correct="b"))
    asyncio.run(c.feedback(routed.request_id, "dept", "good", source="automated"))  # replaces the first


def test_feedback_errors(stack):
    kept, plain = GatewayClient(stack.base, "kk"), GatewayClient(stack.base, "pk")
    rid = asyncio.run(kept.route(request(), route="m")).request_id
    asyncio.run(plain.feedback(rid, "dept", "good"))  # feedback crosses clients
    with pytest.raises(GatewayError) as e:
        asyncio.run(plain.feedback("no-such-request", "dept", "good"))
    assert e.value.status == 404
    with pytest.raises(GatewayError) as e:
        asyncio.run(kept.feedback(rid, "dept", "bad", correct="not-an-option"))
    assert e.value.status == 422
    with pytest.raises(GatewayError) as e:
        asyncio.run(kept.feedback(rid, "dept", "great"))
    assert e.value.status == 422


def test_content_is_stored_only_for_the_opted_in_client(stack):
    asyncio.run(GatewayClient(stack.base, "kk").route(request(), route="m"))
    asyncio.run(GatewayClient(stack.base, "pk").route(request(), route="m"))
    db = sqlite3.connect(stack.db)
    query = ("SELECT r.client, COUNT(c.request_id) FROM requests r "
             "LEFT JOIN contents c USING (request_id) GROUP BY r.client")
    for _ in range(50):  # the gateway writes the store off the request path
        rows = dict(db.execute(query).fetchall())
        if "kept" in rows and "plain" in rows:
            break
        time.sleep(0.1)
    assert rows["kept"] > 0 and rows["plain"] == 0
    assert db.execute("SELECT COUNT(*) FROM contents WHERE state LIKE ?", (f"%{STATE}%",)).fetchone()[0] >= 1


def test_export_calibrate_and_purge_cli(stack):
    kept = GatewayClient(stack.base, "kk")
    for i in range(3):
        rid = asyncio.run(kept.route(request(), route="m")).request_id
        asyncio.run(kept.feedback(rid, "dept", "good" if i else "bad", correct=None if i else "b"))

    out = stack.cli("export", "--client", "kept")
    assert out.returncode == 0, out.stderr
    lines = [json.loads(line) for line in out.stdout.splitlines()]
    assert lines and all(line["state"] == STATE and line["question_id"] == "dept" for line in lines)
    assert any(line["feedback"] and line["feedback"]["verdict"] == "good" for line in lines)

    refused = stack.cli("export", "--client", "plain")  # never opted in
    assert refused.returncode != 0 and "opted in" in refused.stderr

    cal = stack.cli("calibrate", "--client", "kept", "--min", "1", "--target", "0.5")
    assert cal.returncode == 0 and "m / choice" in cal.stdout, cal.stdout + cal.stderr

    purged = stack.cli("purge", "--client", "kept", "--content-only")
    assert purged.returncode == 0 and "stored contents" in purged.stdout, purged.stderr
    assert stack.cli("export", "--client", "kept").stdout == ""


def test_sigterm_finishes_in_flight_requests_and_writes_their_records(tmp_path):
    """The store is written off the request path and closed on shutdown: a request that is
    still running when SIGTERM arrives must be answered, and its record must reach the
    database before the process exits."""
    binary = tmp_path / "ignatius"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/ignatius"], cwd=GO_DIR, check=True, capture_output=True)
    fake = serve_fake(delay_s=0.8)
    port, db, events = free_port(), tmp_path / "s.db", tmp_path / "events.jsonl"
    (tmp_path / "gw.toml").write_text(f"""
listen = "127.0.0.1:{port}"
api_key_env = "IGN_TEST_KEY"
[store]
[events]
[[events.sinks]]
type = "file"
path = "{events}"
[models.m]
provider = "systemone"
base_url = "http://127.0.0.1:{fake.server_address[1]}"
model = "jev-latest"
""")
    env = {"IGN_TEST_KEY": "k", "IGNATIUS_STORE_DSN": str(db), "PATH": "/usr/bin:/bin"}
    proc = subprocess.Popen([str(binary), "serve", "--config", str(tmp_path / "gw.toml")], env=env, stderr=subprocess.PIPE)
    base = f"http://127.0.0.1:{port}"
    for _ in range(50):
        try:
            urllib.request.urlopen(base + "/healthz", timeout=0.5)
            break
        except OSError:
            time.sleep(0.1)
    else:
        proc.kill()
        pytest.fail("gateway did not start")
    result = {}

    def call():
        try:
            result["routed"] = asyncio.run(GatewayClient(base, "k").route(request(), route="m"))
        except Exception as e:  # noqa: BLE001
            result["error"] = e

    t = threading.Thread(target=call)
    t.start()
    time.sleep(0.3)  # the request is inside the slow upstream call
    proc.terminate()
    t.join(timeout=10)
    assert proc.wait(timeout=15) == 0, proc.stderr.read().decode()
    fake.shutdown()
    assert "error" not in result, result.get("error")
    rid = result["routed"].request_id
    assert rid
    assert sqlite3.connect(db).execute("SELECT COUNT(*) FROM requests WHERE request_id = ?", (rid,)).fetchone()[0] == 1
    assert rid in events.read_text()


def test_shadow_audits_become_calibration_labels(tmp_path):
    """The real binary: a cascade with [audit] on records, in the background, whether the cheap tier
    agreed with the next one, and `calibrate --source audit` reads those rows as labels."""
    binary = tmp_path / "ignatius"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/ignatius"], cwd=GO_DIR, check=True, capture_output=True)
    cheap, smart = serve_fake(), serve_fake()  # both answer choice "a" at confidence 0.8: they agree
    port, db = free_port(), tmp_path / "s.db"
    (tmp_path / "gw.toml").write_text(f"""
listen = "127.0.0.1:{port}"
api_key_env = "IGN_TEST_KEY"
[store]
[audit]
sample_rate = 1
[routes.casc]
mode = "cascade"
tiers = [ {{ model = "cheap", threshold = 0.5 }}, "smart" ]
[models.cheap]
provider = "systemone"
base_url = "http://127.0.0.1:{cheap.server_address[1]}"
model = "jev-latest"
[models.smart]
provider = "systemone"
base_url = "http://127.0.0.1:{smart.server_address[1]}"
model = "jev-latest"
""")
    env = {"IGN_TEST_KEY": "k", "IGNATIUS_STORE_DSN": str(db), "PATH": "/usr/bin:/bin"}
    cfg = str(tmp_path / "gw.toml")
    proc = subprocess.Popen([str(binary), "serve", "--config", cfg], env=env, stderr=subprocess.PIPE)
    base = f"http://127.0.0.1:{port}"
    try:
        for _ in range(50):
            try:
                urllib.request.urlopen(base + "/healthz", timeout=0.5)
                break
            except OSError:
                time.sleep(0.1)
        else:
            pytest.fail("gateway did not start")
        c = GatewayClient(base, "k")
        for _ in range(3):
            r = asyncio.run(c.route(request(), route="casc"))
            assert r.ok and r.answers["dept"].model == "cheap"  # settled by the cheap tier, so audited
        conn = sqlite3.connect(db)
        for _ in range(60):  # audits run after the response, in the background
            n = conn.execute("SELECT COUNT(*) FROM audits WHERE agreed = 1").fetchone()[0]
            if n >= 3:
                break
            time.sleep(0.1)
        assert n >= 3, "three audits were expected"
        cols = [r[1] for r in conn.execute("PRAGMA table_info(audits)")]
        assert not {"state", "questions", "answers", "content"} & set(cols), cols  # an audit holds no content
    finally:
        proc.terminate()
        proc.wait(timeout=15)
        cheap.shutdown()
        smart.shutdown()
    out = subprocess.run([str(binary), "calibrate", "--config", cfg, "--client", "default", "--source", "audit", "--min", "1", "--target", "0.5"],
                         env=env, capture_output=True, text=True)
    assert out.returncode == 0, out.stderr
    assert "cheap / choice: 3 labeled" in out.stdout and "100% good overall" in out.stdout, out.stdout
    human = subprocess.run([str(binary), "calibrate", "--config", cfg, "--client", "default", "--source", "human"],
                           env=env, capture_output=True, text=True)
    assert "no feedback with a confidence yet" in human.stdout  # audits are not counted as human labels
