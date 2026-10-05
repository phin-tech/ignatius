"""A GEPA adapter whose program is a set of Ignatius questions and whose scorer is `ignatius eval`.

The test set (SPEC 15.1) carries its questions on every row. Here one question set is canonical: it is split into
named components, GEPA rewrites them, and each evaluation puts the candidate's questions back on the rows before
`ignatius eval` judges them.

Components, per question id `q`:
  q.instructions      the question text
  q.criteria.<option> the description of one option of a choice or noul (the keys are the labels gold is checked
                      against, so they are never components and can never be renamed)
  q.level.<i>         the description of level i of a score

`ignatius eval --json` reports aggregates and a miss list, not a result per item, so a per-item score is derived from
the misses: ask for all of them, and a judged question that is not a miss was right.
"""

from __future__ import annotations

import copy
import json
import random
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path

from gepa.core.adapter import EvaluationBatch

ALL_MISSES = 1_000_000


def split_questions(questions: dict, criteria: bool = True) -> dict[str, str]:
    """The seed candidate: every text in a question set that may be rewritten. With `criteria` False, only the
    instructions are components and the option descriptions stay as they are."""
    parts: dict[str, str] = {}
    for qid, q in questions.items():
        parts[f"{qid}.instructions"] = q["instructions"]
        crit = q.get("criteria") if criteria else None
        if isinstance(crit, dict):
            for opt, desc in crit.items():
                if desc:
                    parts[f"{qid}.criteria.{opt}"] = desc
        elif isinstance(crit, list):
            for i, desc in enumerate(crit):
                parts[f"{qid}.level.{i}"] = desc
    return parts


def join_questions(base: dict, candidate: dict[str, str]) -> dict:
    """The question set with the candidate's texts put back. Keys, types and level counts come from `base`."""
    out = copy.deepcopy(base)
    for qid, q in out.items():
        q["instructions"] = candidate.get(f"{qid}.instructions", q["instructions"])
        crit = q.get("criteria")
        if isinstance(crit, dict):
            for opt, desc in crit.items():
                if desc:
                    crit[opt] = candidate.get(f"{qid}.criteria.{opt}", desc)
        elif isinstance(crit, list):
            q["criteria"] = [candidate.get(f"{qid}.level.{i}", d) for i, d in enumerate(crit)]
    return out


def component_question(component: str) -> str:
    return component.split(".", 1)[0]


def component_option(component: str) -> str | None:
    """The option (or level index) a criteria component describes, or None for an instructions component."""
    parts = component.split(".", 2)
    return parts[2] if len(parts) == 3 and parts[1] in ("criteria", "level") else None


def norm(v) -> str:
    return str(v).strip().lower()


@dataclass
class Trace:
    item: dict
    judged: dict[str, dict]  # question id -> {"gold", "right", "got", "model", "confidence"}


@dataclass
class Evaluator:
    """Runs `ignatius eval` for one route over rows with a given question set."""

    binary: str
    config: str
    route: str
    concurrency: int = 4

    def run(self, rows: list[dict], questions: dict) -> dict:
        with tempfile.TemporaryDirectory() as tmp:
            data, report = Path(tmp) / "test.jsonl", Path(tmp) / "report.json"
            with open(data, "w") as f:
                for r in rows:
                    f.write(json.dumps({**r, "questions": questions}) + "\n")
            proc = subprocess.run(
                [self.binary, "eval", "--config", self.config, "--data", str(data), "--route", self.route,
                 "--json", str(report), "--misses", str(ALL_MISSES), "--concurrency", str(self.concurrency)],
                capture_output=True, text=True)
            if proc.returncode != 0:
                raise RuntimeError(f"ignatius eval failed ({proc.returncode}): {proc.stderr.strip()}")
            return json.loads(report.read_text())["candidates"][0]


class IgnatiusAdapter:
    propose_new_texts = None  # use GEPA's own proposer

    def __init__(self, evaluator: Evaluator, base_questions: dict, length_penalty: float = 0.0):
        self.evaluator = evaluator
        self.base = base_questions
        self.length_penalty = length_penalty
        self.seed_texts = split_questions(base_questions)
        self.spent_usd = 0.0
        self.last_report: dict | None = None
        self.last_accuracy: list[float] = []  # the per-item scores before any length penalty

    def growth(self, candidate: dict[str, str]) -> float:
        """How much longer the candidate's texts are than the starting ones, as a fraction (0 if not longer). The
        models bill by input tokens, so a longer question costs more on every request."""
        start = sum(len(self.seed_texts[k]) for k in candidate if k in self.seed_texts)
        now = sum(len(t) for t in candidate.values())
        return max(0.0, now / start - 1) if start else 0.0

    def evaluate(self, batch: list[dict], candidate: dict[str, str], capture_traces: bool = False):
        rep = self.evaluator.run(batch, join_questions(self.base, candidate))
        self.last_report = rep
        self.spent_usd += rep.get("cost_usd") or 0.0
        misses = {(m["item"], m["question"]): m for m in rep.get("misses", [])}
        scores, outputs, traces = [], [], []
        for row in batch:
            judged = {q: g for q, g in row["gold"].items() if q in self.base}
            per_q = {}
            for q, gold in judged.items():
                m = misses.get((row["id"], q))
                per_q[q] = {"gold": gold, "right": m is None, "got": m["got"] if m else gold,
                            "model": m.get("model") if m else None, "confidence": m.get("confidence") if m else None}
            right = sum(1 for v in per_q.values() if v["right"])
            scores.append(right / len(per_q) if per_q else 0.0)
            outputs.append({q: v["got"] for q, v in per_q.items()})
            traces.append(Trace(row, per_q))
        # A question the route did not answer is wrong in eval's tally but is not in its miss list, so it would score
        # here as right. The totals catch that, and anything else that makes the two disagree.
        judged = sum(len(t.judged) for t in traces)
        right_total = sum(1 for t in traces for v in t.judged.values() if v["right"])
        if judged != rep["judged"] or right_total != rep["correct"]:
            raise RuntimeError(
                f"eval and the miss list disagree: judged {rep['judged']} vs {judged}, correct {rep['correct']} vs "
                f"{right_total} ({rep.get('unanswered', 0)} unanswered, {rep.get('failed_items', 0)} failed items, "
                f"failures {rep.get('failures')}). A model likely errored or rate limited; scores would be wrong")
        self.last_accuracy = scores
        penalty = self.length_penalty * self.growth(candidate)
        return EvaluationBatch(outputs=outputs, scores=[s - penalty for s in scores],
                               trajectories=traces if capture_traces else None)

    def make_reflective_dataset(self, candidate, eval_batch, components_to_update):
        out: dict[str, list[dict]] = {}
        for comp in components_to_update:
            qid = component_question(comp)
            opt = component_option(comp)
            records = []
            for tr in eval_batch.trajectories or []:
                j = tr.judged.get(qid)
                if j is None:
                    continue
                # An option's description is only evidence on items where that option was the right answer or the
                # one given, and the reflection model is told which option the text describes.
                if opt is not None and opt not in (norm(j["gold"]), norm(j["got"])):
                    continue
                q = join_questions(self.base, candidate)[qid]
                about = f" You are rewriting the description of the option {opt!r} only. " if opt is not None else ""
                if j["right"]:
                    feedback = f"Correct: it answered {j['gold']!r}."
                else:
                    sure = j["confidence"] is not None and j["confidence"] >= 0.8
                    feedback = (f"Wrong: the right answer is {j['gold']!r} but it answered {j['got']!r}"
                                + (f" with confidence {j['confidence']:.2f}" if j["confidence"] is not None else "")
                                + (". It was confidently wrong, which is the worst case: a cascade keeps a confident "
                                   "answer without asking the stronger model." if sure else "."))
                feedback += about
                records.append({
                    "Inputs": {"state": tr.item["state"], "question": q["instructions"],
                               "options": q.get("criteria")},
                    "Generated Outputs": j["got"],
                    "Feedback": feedback,
                })
            out[comp] = records
        return out


class FailureSelector:
    """Which component GEPA rewrites next: one the minibatch's wrong answers point at.

    GEPA's default walks the components in turn. With a question that has dozens of options (BANKING77 has 77) that
    mostly picks an option description no wrong answer involved, and the reflection model is asked to improve text it
    has no evidence about. Here the pool is the instruction of each question that got something wrong, plus the
    description of every option that was the right answer or the given one in a miss, weighted by how often.
    """

    def __init__(self, seed: int = 0):
        self.rng = random.Random(seed)

    def __call__(self, state, trajectories, subsample_scores, candidate_idx, candidate) -> list[str]:
        by_lower = {k.lower(): k for k in candidate}
        weight: dict[str, int] = {}
        for tr in trajectories or []:
            for qid, j in tr.judged.items():
                if j["right"]:
                    continue
                for name in [f"{qid}.instructions"] + [f"{qid}.{kind}.{norm(o)}" for o in (j["gold"], j["got"])
                                                         for kind in ("criteria", "level")]:
                    if name.lower() in by_lower:
                        weight[by_lower[name.lower()]] = weight.get(by_lower[name.lower()], 0) + 1
        if not weight:
            return [next(iter(candidate))]
        names = sorted(weight)
        return [self.rng.choices(names, weights=[weight[n] for n in names])[0]]
