"""A quick labeled benchmark of System One models through the Ignatius SDK.

Three public tasks, each a typed System One question:

  banking77  77-way intent (a `choice` with 77 options)         PolyAI BANKING77, test split
  ag_news    4-way topic (a `choice`)                            AG News, test split
  sst2       sentiment as a yes/no question (a `noul`)           SST-2, validation split

For every model it measures accuracy, whether the confidence tracks correctness (ECE of the top
probability, and how well the confidence separates right from wrong), latency and cost, and it simulates
a two-tier cascade from the same answers (accuracy and escalation rate at each threshold) without any
further calls. See README.md.

    uv run --project ../../python python bench.py --n 100          # from this directory

Environment: TYPESAFE_API_KEY (Jev), CLEF_WORKER_URL and CLEF_WORKER_KEY (the deploy/clef-worker Worker,
which serves both clef-flash and clef). A model whose variables are missing is skipped.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import math
import os
import random
import statistics
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field

from ignatius import Limited, LimitsConfig, Plan, Question, Registry, Request, SystemOne, WithConfidence, run

HF = "https://datasets-server.huggingface.co"
# USD per million input tokens, from Cloudflare's and TypeSafe's published prices (output tokens are free).
PRICE = {"jev": 0.042, "clef-flash": 0.09, "clef": 0.24}


def hf_json(path: str, **params) -> dict:
    """GET from the Hugging Face dataset server. It rate limits, so a 429 waits as long as it asks (or backs off) and
    retries."""
    url = f"{HF}/{path}?{urllib.parse.urlencode(params)}"
    for attempt in range(10):
        try:
            with urllib.request.urlopen(url, timeout=60) as r:
                return json.load(r)
        except urllib.error.HTTPError as e:
            if e.code != 429 or attempt == 9:
                raise
            wait = float(e.headers.get("Retry-After") or min(30, 2 ** attempt))
            print(f"  (dataset server asked us to wait {wait:.0f}s)", flush=True)
            time.sleep(wait)
    raise RuntimeError("unreachable")


def label_names(dataset: str) -> list[str]:
    info = hf_json("info", dataset=dataset)["dataset_info"]
    feats = info["features"] if "features" in info else next(iter(info.values()))["features"]
    for f in feats.values() if isinstance(feats, dict) else feats:
        if isinstance(f, dict) and f.get("_type") == "ClassLabel":
            return f["names"]
    raise RuntimeError(f"no ClassLabel in {dataset}")


def hf_row(dataset: str, config: str, split: str, offset: int) -> dict:
    return hf_json("rows", dataset=dataset, config=config, split=split, offset=offset, length=1)["rows"][0]["row"]


def sample_rows(dataset: str, config: str, split: str, n: int, seed: int) -> list[dict]:
    """n distinct rows at random positions in the split. One row per request, on purpose: BANKING77's test split
    is sorted by label, so reading windows of consecutive rows covers only a handful of classes. Sampled rows are
    cached on disk (IGNATIUS_BENCH_CACHE, default ~/.cache/ignatius-bench), so a re-run does not refetch."""
    cache_dir = os.environ.get("IGNATIUS_BENCH_CACHE") or os.path.expanduser("~/.cache/ignatius-bench")
    path = os.path.join(cache_dir, f"{dataset.replace('/', '__')}-{config}-{split}-n{n}-seed{seed}.json")
    if os.path.exists(path):
        with open(path) as f:
            return json.load(f)
    total = hf_json("rows", dataset=dataset, config=config, split=split, offset=0, length=1)["num_rows_total"]
    offsets = random.Random(seed).sample(range(total), min(n, total))
    with ThreadPoolExecutor(3) as pool:
        rows = list(pool.map(lambda o: hf_row(dataset, config, split, o), offsets))
    os.makedirs(cache_dir, exist_ok=True)
    with open(path, "w") as f:
        json.dump(rows, f)
    return rows


@dataclass
class Task:
    name: str
    items: list[tuple[str, Question, str]] = field(default_factory=list)  # (state, question, gold)


def build_tasks(n: int, seed: int) -> list[Task]:
    tasks = []

    names = label_names("legacy-datasets/banking77")
    crit = {nm: nm.replace("_", " ") for nm in names}
    t = Task("banking77")
    for r in sample_rows("legacy-datasets/banking77", "default", "test", n, seed):
        t.items.append((r["text"], Question("choice", "Which banking intent is the customer asking about?", crit), names[r["label"]]))
    tasks.append(t)

    names = label_names("fancyzhx/ag_news")
    crit = {"World": "world news and politics", "Sports": "sports", "Business": "business and economics", "Sci/Tech": "science and technology"}
    t = Task("ag_news")
    for r in sample_rows("fancyzhx/ag_news", "default", "test", n, seed + 1):
        t.items.append((r["text"], Question("choice", "Which topic is this news article about?", crit), names[r["label"]]))
    tasks.append(t)

    t = Task("sst2")
    for r in sample_rows("stanfordnlp/sst2", "default", "validation", n, seed + 2):
        t.items.append((r["sentence"].strip(), Question("noul", "Is the sentiment of this movie review positive?"), "yes" if r["label"] == 1 else "no"))
    tasks.append(t)
    return tasks


def build_models() -> dict[str, Limited]:
    limits = LimitsConfig(max_questions=64, max_images=4)
    models: dict[str, Limited] = {}
    if os.environ.get("TYPESAFE_API_KEY"):
        models["jev"] = Limited(SystemOne("https://api.typesafe.ai", "jev-latest", os.environ["TYPESAFE_API_KEY"], timeout_s=60), limits)
    url, key = os.environ.get("CLEF_WORKER_URL"), os.environ.get("CLEF_WORKER_KEY")
    if url and key:
        for name in ("clef-flash", "clef"):  # the policy: judge Clef by its probabilities (SPEC 1.2)
            models[name] = Limited(WithConfidence(SystemOne(url, name, key, timeout_s=120), "derived"), limits)
    return models


@dataclass
class Row:
    gold: str
    pred: str = ""
    correct: bool = False
    confidence: float | None = None
    native: float | None = None
    p_top: float | None = None
    latency_ms: float = 0.0
    tokens: int = 0
    error: str = ""


def decide(q: Question, a) -> tuple[str, float | None]:
    if q.type == "choice":
        return a.choice, max(a.probabilities.values()) if a.probabilities else None
    yes = a.noul is not None and a.noul >= 0.5
    return ("yes" if yes else "no"), (None if a.noul is None else max(a.noul, 1 - a.noul))


async def ask(reg: Registry, model: str, state: str, q: Question, gold: str) -> Row:
    row = Row(gold=gold)
    t0 = time.perf_counter()
    routed = await run(reg, Request(state, {"q": q}), Plan.from_dict({"mode": "single", "model": model}))
    row.latency_ms = (time.perf_counter() - t0) * 1000
    if not routed.ok or "q" not in routed.answers:
        row.error = routed.failures[0].error.kind if routed.failures else "no answer"
        return row
    a = routed.answers["q"]
    row.pred, row.p_top = decide(q, a)
    row.correct = row.pred == gold
    row.confidence, row.native = a.confidence, a.native_confidence
    row.tokens = routed.results[0].usage.input_tokens
    return row


async def run_model(reg: Registry, model: str, task: Task, concurrency: int) -> list[Row]:
    sem = asyncio.Semaphore(concurrency)

    async def one(item):
        async with sem:
            try:
                return await ask(reg, model, *item)
            except Exception as e:  # noqa: BLE001 - a benchmark records a failure, it does not stop
                return Row(gold=item[2], error=type(e).__name__)

    return await asyncio.gather(*(one(i) for i in task.items))


# ---- metrics ------------------------------------------------------------------------------------


def wilson(k: int, n: int, z: float = 1.96) -> tuple[float, float]:
    if n == 0:
        return (0.0, 0.0)
    p = k / n
    d = 1 + z * z / n
    c = p + z * z / (2 * n)
    m = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n))
    return ((c - m) / d, (c + m) / d)


def ece(rows: list[Row], bins: int = 10) -> float | None:
    """Expected calibration error of the top probability: the gap between how sure a model says it is and how
    often it is right. Lower is better."""
    rs = [r for r in rows if r.p_top is not None]
    if not rs:
        return None
    total = 0.0
    for b in range(bins):
        lo, hi = b / bins, (b + 1) / bins
        inb = [r for r in rs if lo <= r.p_top < hi or (b == bins - 1 and r.p_top == 1.0)]
        if inb:
            total += len(inb) / len(rs) * abs(sum(r.correct for r in inb) / len(inb) - sum(r.p_top for r in inb) / len(inb))
    return total


def auroc(rows: list[Row], key: str) -> float | None:
    """The chance that a right answer has a higher score than a wrong one (0.5 = no signal, 1 = perfect)."""
    pos = [getattr(r, key) for r in rows if r.correct and getattr(r, key) is not None]
    neg = [getattr(r, key) for r in rows if not r.correct and not r.error and getattr(r, key) is not None]
    if not pos or not neg:
        return None
    wins = sum((p > q) + 0.5 * (p == q) for p in pos for q in neg)
    return wins / (len(pos) * len(neg))


def pct(x: float | None) -> str:
    return "  n/a" if x is None else f"{100 * x:5.1f}"


def summarize(task: str, model: str, rows: list[Row]) -> dict:
    ok = [r for r in rows if not r.error]
    k = sum(r.correct for r in ok)
    lo, hi = wilson(k, len(ok))
    lat = sorted(r.latency_ms for r in ok)
    tokens = statistics.mean(r.tokens for r in ok) if ok else 0
    return {
        "task": task, "model": model, "n": len(rows), "errors": len(rows) - len(ok), "classes": len({r.gold for r in rows}),
        "accuracy": k / len(ok) if ok else None, "ci95": [lo, hi],
        "latency_ms_p50": statistics.median(lat) if lat else None, "latency_ms_p95": lat[int(0.95 * (len(lat) - 1))] if lat else None,
        "input_tokens_mean": tokens, "usd_per_1k": tokens * PRICE.get(model, 0) / 1e6 * 1000,
        "ece_top_prob": ece(ok), "auroc_confidence": auroc(ok, "confidence"), "auroc_top_prob": auroc(ok, "p_top"),
        "auroc_native": auroc(ok, "native") if any(r.native is not None for r in ok) else None,
    }


THRESHOLDS = [0.5, 0.6, 0.7, 0.8, 0.9, 0.95]


def cascade(cheap: list[Row], dear: list[Row], t: float) -> tuple[float, float]:
    """A cheap tier that keeps an answer when its confidence is at least t, else the dear tier answers. The same
    rule as a cascade route; no new calls, just the answers already collected."""
    n = hits = esc = 0
    for c, d in zip(cheap, dear):
        if c.error and d.error:
            continue
        n += 1
        if not c.error and c.confidence is not None and c.confidence >= t:
            hits += c.correct
        else:
            esc += 1
            hits += (not d.error) and d.correct
    return (hits / n if n else 0.0, esc / n if n else 0.0)


def report(results: dict[str, dict[str, list[Row]]]) -> dict:
    out = {"summaries": [], "cascades": []}
    print(f"\n{'task':<10}{'model':<12}{'n':>4}{'cls':>4}{'err':>4}{'acc%':>7}{'  95% CI':>14}{'p50 ms':>8}{'p95 ms':>8}{'tok':>6}{'$/1k':>8}{'ECE%':>7}{'AUROC':>7}")
    for task, by_model in results.items():
        for model, rows in by_model.items():
            s = summarize(task, model, rows)
            out["summaries"].append(s)
            ci = f"{100 * s['ci95'][0]:.0f}-{100 * s['ci95'][1]:.0f}"
            au = "  n/a" if s["auroc_confidence"] is None else f"{s['auroc_confidence']:.2f}"
            print(f"{task:<10}{model:<12}{s['n']:>4}{s['classes']:>4}{s['errors']:>4}{pct(s['accuracy']):>7}{ci:>14}{s['latency_ms_p50'] or 0:>8.0f}"
                  f"{s['latency_ms_p95'] or 0:>8.0f}{s['input_tokens_mean']:>6.0f}{s['usd_per_1k']:>8.4f}{pct(s['ece_top_prob']):>7}{au:>7}")
    pairs = [("clef-flash", "jev"), ("clef-flash", "clef"), ("clef", "jev"), ("jev", "clef")]
    for task, by_model in results.items():
        for cheap, dear in pairs:
            if cheap in by_model and dear in by_model:
                print(f"\ncascade {cheap} -> {dear} on {task}  (accuracy of the pair, share sent to {dear})")
                print("  " + "  ".join(f"@{t:.2f}: {100 * a:4.1f}% / {100 * e:3.0f}%" for t in THRESHOLDS for a, e in [cascade(by_model[cheap], by_model[dear], t)]))
                for t in THRESHOLDS:
                    a, e = cascade(by_model[cheap], by_model[dear], t)
                    out["cascades"].append({"task": task, "cheap": cheap, "dear": dear, "threshold": t, "accuracy": a, "escalated": e})
    return out


async def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--n", type=int, default=100, help="items per task")
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--concurrency", type=int, default=6)
    ap.add_argument("--tasks", default="banking77,ag_news,sst2")
    ap.add_argument("--raw", default="", help="write per-item results here (contains dataset text; do not commit)")
    ap.add_argument("--out", default="", help="write the aggregates here as JSON")
    args = ap.parse_args()

    models = build_models()
    if not models:
        print("no models: set TYPESAFE_API_KEY and/or CLEF_WORKER_URL and CLEF_WORKER_KEY", file=sys.stderr)
        return 2
    reg = Registry(models)
    wanted = set(args.tasks.split(","))
    tasks = [t for t in build_tasks(args.n, args.seed) if t.name in wanted]
    print(f"models: {', '.join(models)}   tasks: {', '.join(t.name for t in tasks)}   n={args.n} seed={args.seed}", flush=True)

    results: dict[str, dict[str, list[Row]]] = {}
    for task in tasks:
        results[task.name] = {}
        for name in models:
            t0 = time.time()
            results[task.name][name] = await run_model(reg, name, task, args.concurrency)
            rows = results[task.name][name]
            print(f"  {task.name:<10}{name:<12}done in {time.time() - t0:5.1f}s  ({sum(1 for r in rows if r.error)} errors)", flush=True)
    summary = report(results)
    if args.out:
        with open(args.out, "w") as f:
            json.dump({"n": args.n, "seed": args.seed, **summary}, f, indent=1)
    if args.raw:
        with open(args.raw, "w") as f:
            json.dump({t: {m: [r.__dict__ for r in rows] for m, rows in by.items()} for t, by in results.items()}, f)
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
