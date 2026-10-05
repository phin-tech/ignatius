"""`ignatius eval` with the real binary: a labeled set through a baseline and a cascade, judged (SPEC 15).
Skipped when `go` is not installed."""

import json
import shutil
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

GO_DIR = Path(__file__).resolve().parents[2] / "go"
pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not installed")


def serve(perfect: bool):
    """Answers item i<N> right, except that a fallible server is wrong, and unsure, on every fourth item."""

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            self.send_response(200 if self.path == "/readyz" else 404)
            self.end_headers()

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            idx = int(str(body["state"]).split()[0][1:])
            gold = "a" if idx % 2 == 0 else "b"
            pick, other, conf = gold, ("b" if gold == "a" else "a"), 0.95
            if not perfect and idx % 4 == 0:
                pick, other, conf = other, gold, 0.6
            answers = {q: {"type": "choice", "choice": pick, "confidence": conf,
                           "probabilities": {pick: 0.5 + conf / 2, other: 0.5 - conf / 2}} for q in body["questions"]}
            out = json.dumps({"model": "m", "answers": answers, "usage": {"input_tokens": 100, "output_tokens": 1}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(out)

    srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


@pytest.fixture(scope="module")
def stack(tmp_path_factory):
    tmp = tmp_path_factory.mktemp("evalcli")
    binary = tmp / "ignatius"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/ignatius"], cwd=GO_DIR, check=True, capture_output=True)
    cheap, strong = serve(False), serve(True)
    (tmp / "gw.toml").write_text(f"""
[models.cheap]
provider = "systemone"
base_url = "http://127.0.0.1:{cheap.server_address[1]}"
model = "m"
price_input_per_mtok = 1
[models.strong]
provider = "systemone"
base_url = "http://127.0.0.1:{strong.server_address[1]}"
model = "m"
price_input_per_mtok = 10
""")
    q = {"c": {"type": "choice", "instructions": "?", "criteria": {"a": "x", "b": "y"}}}
    with open(tmp / "test.jsonl", "w") as f:
        for i in range(40):
            f.write(json.dumps({"id": f"t{i}", "state": f"i{i} SECRET-TEXT", "questions": q, "gold": {"c": "a" if i % 2 == 0 else "b"}}) + "\n")
    yield tmp, binary
    cheap.shutdown()
    strong.shutdown()


def run(stack, *args):
    tmp, binary = stack
    return subprocess.run([str(binary), "eval", "--config", str(tmp / "gw.toml"), *args], capture_output=True, text=True)


def test_a_cascade_is_judged_against_a_baseline(stack):
    tmp, _ = stack
    out = run(stack, "--data", str(tmp / "test.jsonl"), "--route", "strong", "--route", "cascade:cheap@0.8>strong", "--sweep",
              "--json", str(tmp / "report.json"))
    assert out.returncode == 0, out.stderr
    assert "40 items, 40 judged questions" in out.stdout
    assert "no detectable difference from strong" in out.stdout and "35% of its cost" in out.stdout
    assert "tier 0 cheap" in out.stdout and "escalated 10" in out.stdout
    assert "sweep of cascade:cheap@0.8>strong" in out.stdout
    assert "SECRET-TEXT" not in out.stdout, "the report holds no content"

    rep = json.loads((tmp / "report.json").read_text())
    assert rep["baseline"] == "strong" and [c["name"] for c in rep["candidates"]] == ["strong", "cascade:cheap@0.8>strong"]
    assert rep["candidates"][1]["accuracy"] == 1 and rep["candidates"][1]["vs_baseline"]["cost_ratio"] == pytest.approx(0.35)
    assert len(rep["sweeps"][0]["rows"]) == 6 and "SECRET-TEXT" not in json.dumps(rep)


def test_a_worse_route_is_called_worse_and_its_misses_are_listed(stack):
    tmp, _ = stack
    out = run(stack, "--data", str(tmp / "test.jsonl"), "--route", "strong", "--route", "cheap", "--misses", "3")
    assert out.returncode == 0, out.stderr
    assert "worse than strong by 25.0 points" in out.stdout
    assert out.stdout.count("  miss ") == 3 and "gold a, got b from cheap" in out.stdout


def test_a_bad_dataset_names_the_line(stack):
    tmp, _ = stack
    bad = tmp / "bad.jsonl"
    bad.write_text(Path(tmp / "test.jsonl").read_text().splitlines()[0] + "\n{not json}\n")
    out = run(stack, "--data", str(bad), "--route", "strong")
    assert out.returncode == 2 and "line 2" in out.stderr


def test_usage_errors_and_unknown_routes(stack):
    tmp, _ = stack
    assert run(stack, "--route", "strong").returncode == 2  # no --data
    assert run(stack, "--data", str(tmp / "test.jsonl")).returncode == 2  # no --route
    out = run(stack, "--data", str(tmp / "test.jsonl"), "--route", "ghost")
    assert out.returncode == 2 and "ghost" in out.stderr
