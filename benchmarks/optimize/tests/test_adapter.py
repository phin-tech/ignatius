"""The adapter against the real `ignatius eval` binary and a fake model that answers well only when the question says
to be careful. Skipped when `go` is not installed."""

import json
import shutil
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import gepa
import pytest

from adapter import Evaluator, IgnatiusAdapter, join_questions, split_questions

GO_DIR = Path(__file__).resolve().parents[3] / "go"
pytestmark = pytest.mark.skipif(shutil.which("go") is None, reason="go toolchain not installed")

QUESTIONS = {"c": {"type": "choice", "instructions": "Which?", "criteria": {"a": "the first", "b": "the second"}}}


def serve():
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_GET(self):
            self.send_response(200 if self.path == "/readyz" else 404)
            self.end_headers()

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            if "BOOM" in str(body["state"]):
                self.send_response(500)
                self.end_headers()
                return
            idx = int(str(body["state"]).split()[0][1:])
            gold = "a" if idx % 2 == 0 else "b"
            careful = all("carefully" in q["instructions"] for q in body["questions"].values())
            pick = gold if careful else "a"
            other = "b" if pick == "a" else "a"
            answers = {q: {"type": "choice", "choice": pick, "confidence": 0.9,
                           "probabilities": {pick: 0.95, other: 0.05}} for q in body["questions"]}
            out = json.dumps({"model": "m", "answers": answers, "usage": {"input_tokens": 100, "output_tokens": 1}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(out)

    srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


@pytest.fixture(scope="module")
def adapter(tmp_path_factory):
    tmp = tmp_path_factory.mktemp("opt")
    binary = tmp / "ignatius"
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/ignatius"], cwd=GO_DIR, check=True, capture_output=True)
    srv = serve()
    (tmp / "gw.toml").write_text(f"""
[models.m]
provider = "systemone"
base_url = "http://127.0.0.1:{srv.server_address[1]}"
model = "m"
price_input_per_mtok = 1
""")
    yield IgnatiusAdapter(Evaluator(str(binary), str(tmp / "gw.toml"), "m"), QUESTIONS)
    srv.shutdown()


def rows(n):
    return [{"id": f"t{i}", "state": f"i{i} text", "questions": QUESTIONS, "gold": {"c": "a" if i % 2 == 0 else "b"}}
            for i in range(n)]


def test_components_cover_the_texts_and_never_the_keys():
    parts = split_questions(QUESTIONS)
    assert set(parts) == {"c.instructions", "c.criteria.a", "c.criteria.b"}
    parts["c.instructions"] = "Which, carefully?"
    q = join_questions(QUESTIONS, parts)["c"]
    assert q["instructions"] == "Which, carefully?" and list(q["criteria"]) == ["a", "b"]


def test_scores_come_from_the_misses(adapter):
    seed = split_questions(QUESTIONS)
    out = adapter.evaluate(rows(10), seed, capture_traces=True)
    assert out.scores == [1.0, 0.0] * 5  # always says a: right on the even items only
    assert out.trajectories[1].judged["c"]["got"] == "a"
    better = {**seed, "c.instructions": "Which, carefully?"}
    assert adapter.evaluate(rows(10), better).scores == [1.0] * 10
    assert adapter.spent_usd > 0


def test_the_reflective_dataset_names_the_failure_and_the_state(adapter):
    seed = split_questions(QUESTIONS)
    out = adapter.evaluate(rows(4), seed, capture_traces=True)
    ds = adapter.make_reflective_dataset(seed, out, ["c.instructions"])["c.instructions"]
    wrong = [r for r in ds if r["Feedback"].startswith("Wrong")]
    assert len(wrong) == 2 and "confidently wrong" in wrong[0]["Feedback"]
    assert wrong[0]["Inputs"]["state"] == "i1 text"


def test_gepa_finds_the_better_wording_end_to_end(adapter):
    def lm(prompt):
        return "```\nWhich, carefully?\n```"

    seed = split_questions(QUESTIONS)
    result = gepa.optimize(seed_candidate=seed, trainset=rows(8), valset=rows(8), adapter=adapter, reflection_lm=lm,
                           reflection_minibatch_size=4, max_metric_calls=60, module_selector="all")
    assert "carefully" in result.best_candidate["c.instructions"]


def test_an_unanswered_question_is_an_error_not_a_right_answer(adapter):
    batch = rows(4)
    batch[1]["state"] = "i1 BOOM"
    with pytest.raises(RuntimeError, match="disagree"):
        adapter.evaluate(batch, split_questions(QUESTIONS))


def test_an_option_description_only_sees_items_about_that_option(adapter):
    seed = split_questions(QUESTIONS)
    out = adapter.evaluate(rows(4), seed, capture_traces=True)  # always says a; items 1 and 3 are gold b
    ds = adapter.make_reflective_dataset(seed, out, ["c.criteria.b"])["c.criteria.b"]
    assert len(ds) == 2 and all("'b'" in r["Feedback"] and "option 'b' only" in r["Feedback"] for r in ds)


def test_the_cli_writes_the_questions_and_a_held_out_verdict(adapter, tmp_path, monkeypatch):
    import optimize

    monkeypatch.setattr(optimize, "reflection_lm", lambda *a: (lambda prompt: "```\nWhich, carefully?\n```"))
    data = tmp_path / "test.jsonl"
    data.write_text("".join(json.dumps(r) + "\n" for r in rows(40)))
    out = tmp_path / "q.json"
    ev = adapter.evaluator
    optimize.main(["--binary", ev.binary, "--config", ev.config, "--data", str(data), "--route", ev.route,
                   "--budget", "80", "--minibatch", "4", "--out", str(out)])
    res = json.loads(out.read_text())
    assert "carefully" in res["questions"]["c"]["instructions"]
    held = res["held_out"]
    # the held-out items are a shuffled 12 of 40: the seed wording is right on the even ones and the new one on all
    odd = round(12 * (1 - held["before"]["accuracy"]))
    assert held["items"] == 12 and held["after"]["accuracy"] == 1 and 0 < odd < 12
    assert held["items_fixed"] == odd and held["items_broken"] == 0
    assert held["recommendation"] == "use the optimized questions"
    assert held["components_changed"] == ["c.instructions"]


def test_an_openai_compatible_reflection_model(monkeypatch):
    import optimize

    seen = {}

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            seen["path"], seen["auth"] = self.path, self.headers["Authorization"]
            seen["body"] = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            out = json.dumps({"choices": [{"message": {"content": "```\nnew text\n```"}}],
                              "usage": {"prompt_tokens": 2000, "completion_tokens": 500}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(out)

    srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    monkeypatch.setenv("FIREWORKS_API_KEY", "k123")
    lm = optimize.reflection_lm("some/model", "fireworks", base_url=f"http://127.0.0.1:{srv.server_address[1]}/v1",
                                price_in=1.0, price_out=4.0)
    assert lm("rewrite this") == "```\nnew text\n```"
    assert (lm.total_tokens_in, lm.total_tokens_out) == (2000, 500) and lm.total_cost == pytest.approx(0.004)
    assert seen["path"] == "/v1/chat/completions" and seen["auth"] == "Bearer k123"
    assert seen["body"]["model"] == "some/model" and seen["body"]["messages"][0]["content"] == "rewrite this"
    srv.shutdown()

    monkeypatch.delenv("FIREWORKS_API_KEY")
    with pytest.raises(SystemExit, match="FIREWORKS_API_KEY is not set"):
        optimize.reflection_lm("m", "fireworks")


def test_the_reflection_cost_cap_stops_the_search(adapter, tmp_path, monkeypatch):
    import optimize

    calls = []

    def call(prompt):
        calls.append(prompt)
        return "```\nWhich?\n```", 1_000_000, 0  # a $1 call at $1 per million, proposing nothing new

    monkeypatch.setattr(optimize, "reflection_lm", lambda *a: optimize.ReflectionLM(call, a[4], a[5]))
    data = tmp_path / "test.jsonl"
    data.write_text("".join(json.dumps(r) + "\n" for r in rows(40)))
    ev = adapter.evaluator
    optimize.main(["--binary", ev.binary, "--config", ev.config, "--data", str(data), "--route", ev.route,
                   "--budget", "100000", "--minibatch", "4", "--out", str(tmp_path / "q.json"),
                   "--reflection-price-in", "1", "--reflection-price-out", "1", "--max-reflection-cost", "3"])
    assert 3 <= len(calls) <= 5, "stops about when the spend reaches $3, to within a call"
    assert json.loads((tmp_path / "q.json").read_text())["held_out"]["reflection"]["usd"] == pytest.approx(len(calls))


def test_a_cost_cap_without_prices_is_refused(adapter, tmp_path):
    import optimize

    ev = adapter.evaluator
    with pytest.raises(SystemExit) as e:
        optimize.main(["--binary", ev.binary, "--config", ev.config, "--data", "x", "--route", ev.route,
                       "--max-reflection-cost", "3"])
    assert e.value.code == 2


class FakeAdapter:
    """Stands in for the adapter in the gate: held-out accuracy and cost for the seed questions, then the new ones."""

    def __init__(self, before, after, cost_before=1.0, cost_after=1.0):
        self.runs = [(before, cost_before), (after, cost_after)]
        self.last_report, self.last_accuracy = None, []

    def evaluate(self, rows, candidate):
        scores, cost = self.runs.pop(0)
        self.last_accuracy = scores
        self.last_report = {"accuracy": sum(scores) / len(scores), "ci95": [0, 1], "cost_usd_per_1k_items": cost}


def gate(before, after, **kw):
    import optimize

    return optimize.judge(FakeAdapter(before, after, **kw), [{}] * len(before), {"q": {"instructions": "x"}},
                          {"q": {"instructions": "y"}})


def test_one_fixed_item_is_not_a_detectable_gain():
    res = gate([1.0] * 19 + [0.0], [1.0] * 20)
    assert res["items_fixed"] == 1 and res["items_broken"] == 0
    assert res["recommendation"] == "keep the originals" and "no detectable gain" in res["verdict"]


def test_a_clear_gain_at_the_same_cost_is_recommended():
    res = gate([0.0] * 10 + [1.0] * 10, [1.0] * 20)
    assert res["recommendation"] == "use the optimized questions" and res["delta_ci95"][0] > 0


def test_a_clear_gain_that_costs_too_much_is_not():
    res = gate([0.0] * 10 + [1.0] * 10, [1.0] * 20, cost_before=1.0, cost_after=2.3)
    assert res["recommendation"] == "keep the originals" and "costs more than" in res["verdict"]
    assert res["cost_ratio"] == pytest.approx(2.3)


def test_an_unmeasured_cost_is_said_so_and_does_not_block():
    res = gate([0.0] * 10 + [1.0] * 10, [1.0] * 20, cost_before=None, cost_after=None)
    assert res["recommendation"] == "use the optimized questions" and "cost not measured" in res["verdict"]


def test_longer_questions_are_penalized_in_the_search_but_not_in_the_accuracy(adapter):
    penalized = IgnatiusAdapter(adapter.evaluator, QUESTIONS, length_penalty=0.5)
    seed = split_questions(QUESTIONS)
    assert penalized.growth(seed) == 0
    longer = {**seed, "c.criteria.a": seed["c.criteria.a"] + "x" * 25}  # the texts total 25 characters: this doubles them
    assert penalized.growth(longer) == pytest.approx(1.0)
    assert penalized.growth({**seed, "c.instructions": "?"}) == 0, "shorter is not rewarded"
    out = penalized.evaluate(rows(4), longer)
    assert out.scores == [s - 0.5 for s in penalized.last_accuracy] and penalized.last_accuracy == [1.0, 0.0, 1.0, 0.0]


def test_the_reflection_prompt_keeps_gepas_placeholders_and_forbids_copying(adapter, tmp_path, monkeypatch):
    import optimize
    from gepa.strategies.instruction_proposal import InstructionProposalSignature

    InstructionProposalSignature.validate_prompt_template(optimize.REFLECTION_PROMPT)  # raises if a placeholder is gone
    seen = []

    def call(prompt):
        seen.append(prompt)
        return "```\nWhich?\n```", 10, 10

    monkeypatch.setattr(optimize, "reflection_lm", lambda *a: optimize.ReflectionLM(call))
    data = tmp_path / "test.jsonl"
    data.write_text("".join(json.dumps(r) + "\n" for r in rows(40)))
    ev = adapter.evaluator
    optimize.main(["--binary", ev.binary, "--config", ev.config, "--data", str(data), "--route", ev.route,
                   "--budget", "40", "--minibatch", "4", "--out", str(tmp_path / "q.json")])
    assert seen and "Do not copy their titles" in seen[0]
    assert " text" in seen[0], "the examples' states are filled in"
    assert "<curr_param>" not in seen[0] and "<side_info>" not in seen[0]


def test_a_reflection_with_no_instruction_in_a_block_fails_the_proposal():
    import optimize

    for reply in ("", "no block here", "```\n\n```", "```text\n   \n```"):
        with pytest.raises(ValueError, match="no instruction"):
            optimize.ReflectionLM(lambda p, r=reply: (r, 1, 1))("x")
    assert optimize.ReflectionLM(lambda p: ("Sure:\n```\nnew text\n```", 1, 1))("x").endswith("```")


def test_the_selector_picks_only_what_the_wrong_answers_point_at():
    from adapter import FailureSelector, Trace

    cand = {"q.instructions": "?", "q.criteria.card_arrival": "a", "q.criteria.lost_card": "b", "q.criteria.other": "c"}
    wrong = Trace({}, {"q": {"gold": "card_arrival", "got": "lost_card", "right": False}})
    right = Trace({}, {"q": {"gold": "other", "got": "other", "right": True}})
    sel = FailureSelector(0)
    picks = {sel(None, [wrong, right], [], 0, cand)[0] for _ in range(60)}
    assert picks == {"q.instructions", "q.criteria.card_arrival", "q.criteria.lost_card"}, "never 'other': it was right"
    assert sel(None, [right], [], 0, cand) == ["q.instructions"], "no failures: fall back to the first component"


def test_when_nothing_beats_the_originals_there_is_no_held_out_comparison(adapter, tmp_path, monkeypatch):
    import optimize

    # a model that only ever proposes the starting text: no candidate can beat it
    monkeypatch.setattr(optimize, "reflection_lm", lambda *a: optimize.ReflectionLM(lambda p: ("```\nWhich?\n```", 1, 1)))
    data = tmp_path / "test.jsonl"
    data.write_text("".join(json.dumps(r) + "\n" for r in rows(40)))
    ev = adapter.evaluator
    optimize.main(["--binary", ev.binary, "--config", ev.config, "--data", str(data), "--route", ev.route,
                   "--budget", "60", "--minibatch", "4", "--out", str(tmp_path / "q.json")])
    held = json.loads((tmp_path / "q.json").read_text())["held_out"]
    assert held["components_changed"] == [] and held["recommendation"] == "keep the originals"
    assert "nothing to compare" in held["verdict"]
    assert "items_fixed" not in held and "before" not in held, "no self-comparison is reported as a result"


def test_the_default_anthropic_path_returns_the_text_and_counts_the_tokens(monkeypatch):
    import types

    import optimize

    created = {}

    class Messages:
        def create(self, **kw):
            created.update(kw)
            return types.SimpleNamespace(content=[types.SimpleNamespace(type="text", text="```\nnew\n```")],
                                         usage=types.SimpleNamespace(input_tokens=1000, output_tokens=200))

    monkeypatch.setattr(optimize.anthropic, "Anthropic", lambda: types.SimpleNamespace(messages=Messages()))
    lm = optimize.reflection_lm("claude-x", "anthropic", price_in=2.0, price_out=10.0)
    assert lm("rewrite") == "```\nnew\n```"
    assert created["model"] == "claude-x" and created["messages"] == [{"role": "user", "content": "rewrite"}]
    assert (lm.total_tokens_in, lm.total_tokens_out) == (1000, 200) and lm.total_cost == pytest.approx(0.004)
