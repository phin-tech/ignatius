# A quick labeled benchmark: Jev, Clef-flash and Clef

`bench.py` runs three public classification tasks through the Ignatius SDK, each as a typed System One question, on
Jev, `clef-flash` and `clef`. It measures accuracy, whether confidence tracks correctness, latency and cost, and it
simulates two-tier cascades from the same answers (no further calls). `results.json` holds the aggregates of the run
below; the per-item answers contain dataset text and are not committed.

```sh
uv run --project ../../python python bench.py --n 200          # from this directory
```

It needs `TYPESAFE_API_KEY` (Jev) and `CLEF_WORKER_URL` and `CLEF_WORKER_KEY` (the Worker in
`deploy/clef-worker`, which serves both Clef models). A model whose variables are missing is skipped. The datasets
come from the Hugging Face dataset server (rate limited; sampled rows are cached under `~/.cache/ignatius-bench`).
For judging your own routes on your own labeled data, use `ignatius eval` (SPEC 15) instead.

## Results

200 random items per task (seed 7), one question per request, 2026-10-04. `acc` has a 95% Wilson interval.

| task | model | acc | 95% CI | ECE | AUROC | p50 ms | $ / 1k requests |
|---|---|---|---|---|---|---|---|
| BANKING77 (77-way, 74 classes in the sample) | jev | 81.5% | 76 to 86 | 8.3% | 0.86 | 195 | 0.071 |
| | clef-flash | **96.5%** | 93 to 98 | 3.4% | 0.91 | 316 | 0.165 |
| | clef | 95.0% | 91 to 97 | 4.7% | 0.93 | 773 | 0.440 |
| AG News (4-way) | jev | 88.5% | 83 to 92 | 8.5% | 0.79 | 182 | 0.017 |
| | clef-flash | 92.0% | 87 to 95 | 2.4% | 0.74 | 308 | 0.020 |
| | clef | 90.4% | 85 to 94 | 2.2% | 0.86 | 587 | 0.053 |
| SST-2 (sentiment, yes/no) | jev | 94.0% | 90 to 97 | 7.4% | 0.91 | 194 | 0.013 |
| | clef-flash | 94.0% | 90 to 97 | 4.2% | 0.77 | 276 | 0.015 |
| | clef | 95.5% | 92 to 98 | 2.5% | 0.84 | 514 | 0.040 |

ECE is the expected calibration error of the top probability (lower is better); AUROC is how well the confidence
separates right answers from wrong ones (0.5 is nothing, 1 is perfect).

**What it shows**
- **BANKING77: Clef is far better**, by about 15 points, well outside the intervals. Cloudflare's own numbers point
  the same way (macro-F1 94.2 for Clef against 79.7 for Jev).
- **AG News and SST-2: no detectable difference.** The intervals overlap by a wide margin; with 200 items each, a gap
  of a few points would not show.
- **Calibration: Clef is better calibrated.** Jev is overconfident (ECE 7 to 9%); both Clef models sit at 2 to 5%.
- **Cascades.** On BANKING77 `clef-flash` alone (96.5%) beats any cascade into Jev: `clef-flash > jev` holds about 95%
  while sending 6% of questions to Jev at a 0.8 bar, and `jev > clef` needs a 0.95 bar and 40% escalation to reach 93%.
  A cascade only helps when the expensive tier is the better one; here it is the cheap one. On SST-2,
  `clef-flash > jev` at 0.8 gives 96.0% with 17% escalated, about a point over either alone, which is within noise.
  The full threshold tables are in `results.json`.

## Read this before you quote it

- **BANKING77 is probably in Clef's training distribution.** Cloudflare lists it among its own evaluation sets, so this
  is not a measure of zero-shot ability, and the same goes for how far ahead it looks. AG News and SST-2 are the
  fairer comparison, and there the two are level.
- **Small samples.** 200 items per task. Treat a difference as real only when the intervals separate.
- **Latency is not comparable.** It is wall time from one laptop: Jev directly, Clef through a local `wrangler dev`
  Worker that calls Workers AI, so Clef pays an extra hop. A first run, before the sampler was fixed, saw about 500 ms
  for every model, so the network dominates and these numbers say little about the models themselves. Cloudflare
  reports 39 ms (`clef-flash`) and 209 ms (`clef`) against 524 ms for Jev, measured on its side.
- **Cost is an estimate:** each model's input tokens from its own usage report, times the published price per million
  input tokens (Jev $0.042, `clef-flash` $0.09, `clef` $0.24; output tokens are free). Jev is the cheapest per token and
  its prompt for BANKING77 is slightly shorter.
- **Errors.** Four calls to `clef` (1 on BANKING77, 3 on AG News) failed with an HTTP error from Workers AI at
  concurrency 6 and are left out of that model's accuracy (`n` in `results.json` is 200 for all rows; `errors` counts them).
- **The first run was wrong, and why.** BANKING77's test split is sorted by label. The first sampler read windows of
  consecutive rows and so covered 7 of 77 classes, which made Jev look like 50% against Clef's 89%. The sampler now
  draws individual random rows, and the report prints how many classes the sample covers (the `cls` column) so a
  sample like that is visible at once.
- **Prompting.** Every choice option has a plain description built from its label (`card_arrival` becomes "card arrival").
  Richer descriptions could move any model's numbers, and not equally.
