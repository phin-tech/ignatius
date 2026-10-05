"""Fan-out reducers (SPEC 6.0). A question with no responder is absent."""

from __future__ import annotations

from .backend import conf_value, derive_confidence
from .types import Answer, Question, Result


def reduce(kind: str, questions: dict[str, Question], results: list[Result]) -> dict[str, Answer]:
    out: dict[str, Answer] = {}
    for qid, q in questions.items():
        answers = [r.answers[qid] for r in results if qid in r.answers and r.answers[qid].type == q.type]
        if not answers:
            continue
        if kind == "most_confident":
            best = answers[0]
            for a in answers[1:]:
                if conf_value(a) > conf_value(best):
                    best = a
            out[qid] = best
            continue
        if q.type == "choice":
            merged = _merge_choice(kind, answers)
        elif q.type == "noul":
            merged = Answer("noul", noul=_mean([a.noul for a in answers]))
        elif q.type == "score":
            merged = Answer(
                "score",
                score=_mean([a.score for a in answers]),
                probabilities=_mean_probs(answers),
                legend=answers[0].legend,
            )
        else:
            continue
        merged.model = "ensemble"
        merged.contributors = [a.model for a in answers]
        merged.confidence = derive_confidence(merged)
        out[qid] = merged
    return out


def _mean(xs: list[float | None]) -> float | None:
    vals = [x for x in xs if x is not None]
    return sum(vals) / len(vals) if vals else None


def _mean_probs(answers: list[Answer]) -> dict[str, float]:
    out: dict[str, float] = {}
    for a in answers:
        for k, p in a.probabilities.items():
            out[k] = out.get(k, 0.0) + p / len(answers)
    return out


def _argmax(p: dict[str, float]) -> str:
    best, bv = "", float("-inf")
    for k in sorted(p):  # deterministic ties: lexicographically smallest wins
        if p[k] > bv:
            best, bv = k, p[k]
    return best


def _merge_choice(kind: str, answers: list[Answer]) -> Answer:
    if kind == "mean":
        p = _mean_probs(answers)
        return Answer("choice", choice=_argmax(p), probabilities=p)
    # vote: plurality; ties go to the larger summed probability, then the name.
    counts: dict[str, int] = {}
    psum: dict[str, float] = {}
    shares: dict[str, float] = {}
    for a in answers:
        counts[a.choice] = counts.get(a.choice, 0) + 1
        psum[a.choice] = psum.get(a.choice, 0.0) + a.probabilities.get(a.choice, 0.0)
        for k in a.probabilities:
            shares.setdefault(k, 0.0)
    score: dict[str, float] = {}
    for c, n in counts.items():
        shares[c] = n / len(answers)
        score[c] = n * 1e6 + psum[c]  # count dominates; probability breaks ties
    return Answer("choice", choice=_argmax(score), probabilities=shares)
