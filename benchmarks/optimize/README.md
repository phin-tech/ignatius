# Optimizing questions with GEPA

`optimize.py` rewrites the questions in a labeled test set so a route answers them better. [GEPA](https://github.com/gepa-ai/gepa)
is the optimizer and `ignatius eval` (SPEC 15) is the scorer: each candidate set of questions is put on the rows, judged by
the real binary against the route you name, and the misses are what GEPA's reflection model reads to propose a rewrite.

It is an experiment. Read [What we have seen](#what-we-have-seen) before relying on a result.

## Run it

You need Go 1.26, [uv](https://docs.astral.sh/uv/), a Jev key, and a key for a reflection model. From this directory:

```sh
(cd ../../go && go build -o ../benchmarks/optimize/ignatius ./cmd/ignatius)   # the scorer, built here (git-ignored)
cp ignatius.example.toml ignatius.toml                      # one model, priced

export TYPESAFE_API_KEY=...                                 # the model being optimized
export ANTHROPIC_API_KEY=...                                # the reflection model (see below for others)

uv run python optimize.py --binary ./ignatius --config ignatius.toml \
    --data ../../examples/eval/github-prs/prs.jsonl --route jev --budget 300
```

It prints the split, GEPA's progress, and a report, and writes `questions.optimized.json` (the new questions and the report).
`uv run pytest` runs the tests against the real binary and a fake model (needs `go`).

To try a harder task, build the BANKING77 set (77 intents, one `choice` question) and pass `--criteria`, which also
rewrites the option descriptions:

```sh
uv run python banking77.py --n 200 --out banking77.jsonl    # fetches from Hugging Face; cached under ~/.cache/ignatius-bench
uv run python optimize.py --binary ./ignatius --config ignatius.toml --data banking77.jsonl \
    --route jev --criteria --budget 800 --minibatch 8
```

## What changes

By default only each question's `instructions`. `--criteria` also rewrites each option's description. Each text is its own
component, and the option keys of a choice or noul are the labels gold is checked against, so they never change; neither
does the number of score levels. All rows must carry the same questions. With `--criteria`, the next component to rewrite is
chosen from the instruction and the options involved in the minibatch's wrong answers, weighted by how often (GEPA's default
walks the components in turn, which on a 77-option question mostly picks an option no wrong answer involved).

## Cost

Every evaluation makes real model calls, and so does the reflection model.

- `--budget` counts items evaluated, **not dollars**.
- `--max-reflection-cost 0.40` stops the search once the reflection model has cost that many USD, to within one call. The cost
  is counted from the token usage each response reports, at a price looked up in [LiteLLM's price file](https://github.com/BerriAI/litellm)
  (cached for a day). For OpenRouter the price is checked against OpenRouter's own model list and its numbers win if they
  disagree, because LiteLLM's file is community maintained and was wrong for one model we used. The resolved price is printed
  at the start; `--reflection-price-in` and `--reflection-price-out` (USD per million tokens) override it.
- Price the models in the gateway config (`price_input_per_mtok`), or no evaluation cost is reported and the cost gate below
  cannot work.

A cheaper reflection model: `--reflection-provider openrouter` or `fireworks` (keys `OPENROUTER_API_KEY`, `FIREWORKS_API_KEY`,
and that provider's own model id in `--reflection-model`), or `--reflection-provider openai` with `--reflection-base-url` and
`--reflection-key-env` for any chat-completions server. The default is Anthropic (`claude-sonnet-5-5`).

## How it is judged

The rows are split 40/30/30 into feedback, validation and held-out sets. GEPA maximizes accuracy on the first two, less a
length penalty (`--length-penalty`, default 0.05 for doubling the questions' length): the models bill by input tokens, so a
longer question costs more on every request. The held-out set then compares the starting questions with the best candidate, in
plain accuracy: the difference with a paired bootstrap interval, the cost per item, calibration, and how many items were fixed
or broken.

**The new questions are recommended only if the interval on the accuracy difference excludes zero and they cost no more than
`--max-cost-ratio` (default 1.25) times the old per item.** A gain inside the noise is not a gain. If no candidate beat the
starting questions on validation there is nothing to compare, and the report says so.

The reflection prompt is our own, not GEPA's. GEPA's asks the model to include "all niche and domain specific factual
information" from the examples, which on a labeled set made it copy the examples' titles and phrases into the instruction and
grow it. Ours asks for a general rule, forbids copying the examples, and asks for no more length.

## What we have seen

Jev on the 72 pull requests in `examples/eval/github-prs` (about 93% accurate before we start), a DeepSeek reflection model:

- **No gain in four runs.** One run's rewrite was a 4,600-character instruction that copied phrases from the feedback
  examples and cost 2.3 times as much per item for one extra correct question; the length penalty and our prompt came from that.
- **Run-to-run noise is about one question.** Evaluating identical questions twice differed by one question in 63, so a
  one-question gain is not a result. The bootstrap interval resamples items and does not capture this.
- **Small minibatches mislead.** Three candidates that beat the original on a 6-item minibatch lost on the 22-item validation set.
- **A tiny held-out set cannot confirm anything.** 21 items gives an interval several points wide.

No BANKING77 result is recorded here yet. Jev scores 82.5% on the 200-row sample before optimization, which leaves room to find a gain.

## Things to know

- A cheap tier's confidence can rise on wrong answers as well as right ones when a question is reworded, and a cascade's
  threshold was set for the old wording. Rerun `ignatius eval --sweep` on a result before adopting it in a cascade.
- The scorer derives per-item results from eval's miss list. If a model errors or rate limits, a question goes unanswered and
  is in no miss list, so the run stops with an error and not a wrong score.
- The reflection model's replies must hold the new text in a ``` block. One that does not is dropped and the search goes on.
