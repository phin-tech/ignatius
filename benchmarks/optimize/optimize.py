"""Rewrite the questions of a labeled test set with GEPA so a route answers them better.

    uv run python optimize.py --binary ./ignatius --config ignatius.toml \\
        --data ../../examples/eval/github-prs/prs.jsonl --route 'cascade:cheap@0.8>strong' \\
        --budget 300 --out questions.optimized.json

The data is split into a feedback set (what GEPA reflects on), a validation set (what it compares candidates on) and a
held-out set it never sees. GEPA maximizes accuracy. The held-out set then judges the result against the starting
questions: accuracy with an interval, cost, and which items got fixed or broken. Every evaluation makes real model
calls, and so does the reflection model (Anthropic, OpenRouter, Fireworks or any chat-completions server).
"""

from __future__ import annotations

import argparse
import json
import os
import random
import re
import sys

import anthropic
import gepa
import httpx

import prices
from adapter import Evaluator, FailureSelector, IgnatiusAdapter, join_questions, split_questions


def load_rows(path: str) -> tuple[list[dict], dict]:
    with open(path) as f:
        rows = [json.loads(line) for line in f if line.strip()]
    if not rows:
        sys.exit("no rows in the test set")
    base = rows[0]["questions"]
    for i, r in enumerate(rows):
        if r["questions"] != base:
            sys.exit(f"row {i + 1} asks different questions from row 1: the optimizer needs one question set")
        if not r.get("id"):
            r["id"] = f"row{i + 1}"
    ids = [r["id"] for r in rows]
    if len(set(ids)) != len(ids):
        sys.exit("row ids must be unique: per-item scores are matched to eval's misses by id")
    return rows, base


def split(rows: list[dict], seed: int, fractions=(0.4, 0.3)) -> tuple[list[dict], list[dict], list[dict]]:
    rows = rows[:]
    random.Random(seed).shuffle(rows)
    a = round(len(rows) * fractions[0])
    b = a + round(len(rows) * fractions[1])
    return rows[:a], rows[a:b], rows[b:]


# Providers that speak the OpenAI chat completions API: base URL and the variable holding the key.
OPENAI_COMPATIBLE = {
    "openrouter": ("https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"),
    "fireworks": ("https://api.fireworks.ai/inference/v1", "FIREWORKS_API_KEY"),
}


def as_text(prompt) -> str:
    return prompt if isinstance(prompt, str) else "\n\n".join(m["content"] for m in prompt)


class ReflectionLM:
    """The reflection model, with its spend counted from the token usage each response reports. GEPA wraps a plain
    callable in a tracker that always reports a cost of 0, so `max_reflection_cost` would never fire; an object with
    a `total_cost` of its own is used as it is."""

    def __init__(self, call, price_in_per_mtok: float = 0.0, price_out_per_mtok: float = 0.0):
        self.call, self.price_in, self.price_out = call, price_in_per_mtok, price_out_per_mtok
        self.total_tokens_in = self.total_tokens_out = 0

    @property
    def total_cost(self) -> float:
        return (self.total_tokens_in * self.price_in + self.total_tokens_out * self.price_out) / 1e6

    def __call__(self, prompt) -> str:
        text, tokens_in, tokens_out = self.call(as_text(prompt))
        self.total_tokens_in += tokens_in
        self.total_tokens_out += tokens_out
        # GEPA takes the text of the first ``` block as the new instruction and scores it, even if it is empty. A
        # model that answers with nothing (or no block) must not get an empty question evaluated, so fail the proposal.
        block = re.search(r"```[^\n`]*\n?(.*?)```", text, re.S)
        if not block or not block.group(1).strip():
            raise ValueError("the reflection model returned no instruction in a ``` block")
        return text


def reflection_lm(model: str, provider: str = "anthropic", base_url: str | None = None, key_env: str | None = None,
                  price_in: float = 0.0, price_out: float = 0.0) -> ReflectionLM:
    """The reflection model. `provider` is anthropic, openrouter, fireworks, or openai (any server that speaks chat
    completions: give `base_url` and `key_env`). Prices are USD per million tokens."""
    if provider == "anthropic":
        client = anthropic.Anthropic()

        def call(prompt: str):
            msg = client.messages.create(model=model, max_tokens=4096, messages=[{"role": "user", "content": prompt}])
            return ("".join(b.text for b in msg.content if b.type == "text"),
                    msg.usage.input_tokens, msg.usage.output_tokens)

        return ReflectionLM(call, price_in, price_out)

    default_url, default_key = OPENAI_COMPATIBLE.get(provider, (None, None))
    base_url, key_env = base_url or default_url, key_env or default_key
    if not base_url or not key_env:
        sys.exit(f"provider {provider!r} needs --reflection-base-url and --reflection-key-env")
    if key_env not in os.environ:
        sys.exit(f"{key_env} is not set")
    # A request that stalls must not stall the search: bound every phase, and retry once before the proposal fails.
    http = httpx.Client(timeout=httpx.Timeout(60, connect=10), headers={"Authorization": f"Bearer {os.environ[key_env]}"})

    def call(prompt: str):
        for attempt in (1, 2):
            try:
                r = http.post(base_url.rstrip("/") + "/chat/completions", json={
                    "model": model, "max_tokens": 4096, "messages": [{"role": "user", "content": prompt}]})
                r.raise_for_status()
                break
            except httpx.TransportError:
                if attempt == 2:
                    raise
        body = r.json()
        usage = body.get("usage") or {}
        return (body["choices"][0]["message"]["content"] or "",
                usage.get("prompt_tokens", 0), usage.get("completion_tokens", 0))

    return ReflectionLM(call, price_in, price_out)


def bootstrap_ci(deltas: list[float], resamples: int = 2000, seed: int = 0) -> tuple[float, float]:
    """95% interval of the mean of per-item score differences, resampling items (so it is paired)."""
    rng = random.Random(seed)
    n = len(deltas)
    means = sorted(sum(deltas[rng.randrange(n)] for _ in range(n)) / n for _ in range(resamples))
    return means[int(0.025 * resamples)], means[int(0.975 * resamples) - 1]


def judge(adapter: IgnatiusAdapter, rows: list[dict], seed_q: dict, best_q: dict, max_cost_ratio: float = 1.25) -> dict:
    """Held-out comparison, paired by item, and the decision on it.

    The new questions are recommended only if they are detectably more accurate (the paired interval on the accuracy
    difference excludes zero) and do not cost more than `max_cost_ratio` times the old ones per item. A gain inside the
    noise is not a gain, and with a small held-out set that will often be the case: "keep the originals" is the honest
    answer then. Scores here are plain accuracy, without the search's length penalty.
    """
    adapter.evaluate(rows, split_questions(seed_q))
    before, before_scores = adapter.last_report, adapter.last_accuracy
    adapter.evaluate(rows, split_questions(best_q))
    after, after_scores = adapter.last_report, adapter.last_accuracy
    deltas = [b - a for a, b in zip(before_scores, after_scores)]
    fixed, broken = sum(1 for d in deltas if d > 0), sum(1 for d in deltas if d < 0)
    delta = sum(deltas) / len(deltas)
    lo, hi = bootstrap_ci(deltas)
    cost_b, cost_a = before.get("cost_usd_per_1k_items"), after.get("cost_usd_per_1k_items")
    ratio = cost_a / cost_b if cost_a is not None and cost_b else None

    better = lo > 0
    affordable = ratio is None or ratio <= max_cost_ratio
    verdict = (f"accuracy {delta * 100:+.1f} points (95% CI {lo * 100:+.1f} to {hi * 100:+.1f})"
               + (f", cost {ratio:.2f}x per item" if ratio is not None else ", cost not measured (price the models)"))
    if better and affordable:
        recommendation = "use the optimized questions"
    elif not better:
        recommendation, verdict = "keep the originals", verdict + ": no detectable gain"
    else:
        recommendation, verdict = "keep the originals", verdict + f": costs more than the {max_cost_ratio}x allowed"
    return {"items": len(rows), "before": summary(before), "after": summary(after), "items_fixed": fixed,
            "items_broken": broken, "accuracy_delta": delta, "delta_ci95": [lo, hi], "cost_ratio": ratio,
            "verdict": verdict, "recommendation": recommendation}


# GEPA's own prompt asks the reflection model to "identify all niche and domain specific factual information ... and
# include it". On a labeled test set that makes it copy the feedback examples into the instruction (their titles, their
# phrases) and grow it, which overfits and costs more per request. This one asks for the opposite.
REFLECTION_PROMPT = """I gave a classifier the following instruction for one question it must answer about an input:
```
<curr_param>
```

Here are examples of inputs, what it answered, and feedback on each answer:
```
<side_info>
```

Write a better instruction for that question.

- Find the pattern behind the wrong answers and fix it with a general rule about what the question means. Say what \
distinguishes the options or the cases where it went wrong, in terms that would apply to inputs you have not seen.
- The examples are only a sample. Do not copy their titles, names, identifiers or phrases into the instruction, and \
do not list one rule per example. If a rule would only help one of the examples, leave it out.
- Keep it about as long as the current instruction, and shorter if you can. Every word is paid for on every request, \
and a longer instruction has to earn its length. Keep what already works in the current instruction.
- Confidently wrong answers matter most: they are not sent on for a second opinion.

Provide the new instruction within ``` blocks."""


def summary(rep: dict) -> dict:
    lo, hi = rep["ci95"]
    return {"accuracy": rep["accuracy"], "ci95": [lo, hi], "cost_usd_per_1k_items": rep.get("cost_usd_per_1k_items"),
            "ece": rep.get("ece_top_probability"), "auroc": rep.get("auroc_confidence"),
            "tiers": rep.get("tiers")}


def main(argv: list[str] | None = None) -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--binary", required=True, help="the ignatius binary")
    ap.add_argument("--config", required=True)
    ap.add_argument("--data", required=True)
    ap.add_argument("--route", required=True, help="the route to optimize for, e.g. 'cascade:cheap@0.8>strong'")
    ap.add_argument("--budget", type=int, default=300, help="max GEPA metric calls: one per item evaluated")
    ap.add_argument("--criteria", action="store_true",
                    help="also rewrite each option's description (many more components, so raise --budget)")
    ap.add_argument("--reflection-provider", default="anthropic",
                    choices=["anthropic", "openrouter", "fireworks", "openai"])
    ap.add_argument("--reflection-model", default="claude-sonnet-5-5",
                    help="the provider's model id (OpenRouter and Fireworks name their own, e.g. 'vendor/model')")
    ap.add_argument("--reflection-base-url", help="for --reflection-provider openai: the server's /v1 URL")
    ap.add_argument("--reflection-key-env", help="the variable holding the key (default: the provider's usual one)")
    ap.add_argument("--reflection-price-in", type=float, default=0.0,
                    help="USD per million input tokens (default: looked up in LiteLLM's price file)")
    ap.add_argument("--reflection-price-out", type=float, default=0.0,
                    help="USD per million output tokens (default: looked up in LiteLLM's price file)")
    ap.add_argument("--max-reflection-cost", type=float,
                    help="stop the search once the reflection model has cost this many USD (needs a price; checked "
                         "between iterations, so it can overshoot by one call)")
    ap.add_argument("--length-penalty", type=float, default=0.05,
                    help="score lost per item when the questions double in length, in proportion (default 0.05): "
                         "models bill by input tokens, so longer questions cost more on every request")
    ap.add_argument("--max-cost-ratio", type=float, default=1.25,
                    help="the most the new questions may cost per item, against the old, to be recommended")
    ap.add_argument("--minibatch", type=int, default=8)
    ap.add_argument("--concurrency", type=int, default=4)
    ap.add_argument("--seed", type=int, default=0)
    ap.add_argument("--out", default="questions.optimized.json")
    args = ap.parse_args(argv)
    price_in, price_out, price_from = prices.resolve(args.reflection_provider, args.reflection_model,
                                                     args.reflection_price_in, args.reflection_price_out)
    priced = price_from != "unknown"
    if priced:
        print(f"reflection model price: ${price_in:g} in / ${price_out:g} out per million tokens, from {price_from}",
              flush=True)
    elif args.max_reflection_cost is not None:
        ap.error(f"no price found for {args.reflection_model!r}: pass --reflection-price-in and "
                 "--reflection-price-out. The cost is counted from token usage at those prices, and without them "
                 "the cap could never be reached")

    rows, base = load_rows(args.data)
    train, val, test = split(rows, args.seed)
    if len(test) < 30:
        print(f"warning: the held-out set has {len(test)} items; its interval will be too wide to confirm a gain",
              file=sys.stderr)
    adapter = IgnatiusAdapter(Evaluator(args.binary, args.config, args.route, args.concurrency), base,
                              args.length_penalty)
    seed = split_questions(base, criteria=args.criteria)
    print(f"{len(rows)} rows -> {len(train)} feedback, {len(val)} validation, {len(test)} held out; "
          f"{len(seed)} components", flush=True)

    lm = reflection_lm(args.reflection_model, args.reflection_provider, args.reflection_base_url,
                       args.reflection_key_env, price_in, price_out)
    result = gepa.optimize(
        seed_candidate=seed, trainset=train, valset=val, adapter=adapter, reflection_lm=lm,
        reflection_minibatch_size=args.minibatch, max_metric_calls=args.budget,
        reflection_prompt_template=REFLECTION_PROMPT,
        module_selector=FailureSelector(args.seed) if args.criteria else "round_robin",
        max_reflection_cost=args.max_reflection_cost, seed=args.seed, display_progress_bar=True)

    best_q = join_questions(base, result.best_candidate)
    changed = sorted(k for k in seed if result.best_candidate.get(k) != seed[k])
    search_spend = adapter.spent_usd  # before the held-out runs, which are not part of the search
    if changed:
        report = judge(adapter, test, base, best_q, args.max_cost_ratio)
    else:
        # Nothing beat the starting questions on validation. Comparing them with themselves on the held-out items
        # would only report the model's run-to-run noise as a gain.
        report = {"items": len(test), "recommendation": "keep the originals",
                  "verdict": "no candidate beat the starting questions on the validation set; nothing to compare"}
    report["components_changed"] = changed
    report["reflection"] = {"tokens_in": getattr(lm, "total_tokens_in", None),
                            "tokens_out": getattr(lm, "total_tokens_out", None),
                            "usd": lm.total_cost if priced else None, "price_from": price_from}
    if search_spend:
        report["model_spend_usd_during_search"] = search_spend
    else:
        print("warning: no cost reported; price the models in the config (price_input_per_mtok) to see spend and the "
              "cost ratio", file=sys.stderr)
    with open(args.out, "w") as f:
        json.dump({"questions": best_q, "held_out": report}, f, indent=2)
    print(json.dumps(report, indent=2))
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
