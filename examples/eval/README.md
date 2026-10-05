# Sample test set: support-ticket triage

`support-triage.jsonl` is a small labeled set for trying `ignatius eval` (SPEC 15): 57 short customer-support tickets,
each with four questions and the answer each should get. The tickets are original and synthetic, written for this
repository, and carry no real customer data.

| question | type | gold |
|---|---|---|
| `department` | choice | `billing`, `technical`, `sales` or `account` (17 / 17 / 14 / 9 tickets) |
| `refund` | noul | does the customer ask for money back or a reversal (6 yes, 51 no) |
| `churn` | noul | does the customer say they will cancel, leave or switch (6 yes, 51 no) |
| `urgency` | score | 0 no timing mentioned, 1 a deadline later this week, 2 needs help today or tomorrow or is blocked, 3 already past a deadline (35 / 6 / 13 / 3) |

Most tickets are clear. Some are there to catch a model that reads keywords instead of meaning: a ticket that says it is
*not* a billing question, one that thanks the team for a refund it already got, one that says "I'm not going to cancel",
one that is sarcastic, and one that raises a bug and a sales question together. The urgency scale is written into the
question, and the labels follow it strictly: no timing stated means level 0.

## Run it

Against one model, then a cascade against that model, with the threshold sweep:

```sh
ignatius eval --config ignatius.toml --data examples/eval/support-triage.jsonl --route jev
ignatius eval --config ignatius.toml --data examples/eval/support-triage.jsonl \
  --route jev --route 'cascade:cheap@0.8>jev' --sweep
```

The first route is the baseline; the report gives each route's accuracy with a 95% interval, what each tier of the cascade
answered and how right it was, cost and latency, and a verdict such as "no detectable difference from jev, at 40% of its
cost". The status page's Evaluate panel takes the same file (`[eval] enabled = true`). With 57 items the intervals are wide,
so expect "no detectable difference" unless two routes are far apart.

For reference, Jev (`jev-latest`, 2026-10-04) scored 97.8% (223 of 228 judged questions): every choice and yes/no question
right and 91.2% of the urgency scores, whose five misses are all borderline calls it was unsure about (confidence
0.2 to 0.6). Your numbers will differ.

## How the labels were checked

The labels were written by hand, then run through Jev. Every disagreement was read to decide whether the model or the label
was wrong. That caught real label problems: urgency level 1 had been given to tickets that never mention timing, the
`account` and `technical` categories both claimed "logins", and one churn ticket was really a privacy request. Those were
fixed in the labels and in the question wording, and three tickets that were defensible either way were removed. A label
this process cannot settle should not be in a gold set, so if you find one that is wrong, please say so.

## A harder set, from real pull requests

`github-prs/` has 72 merged pull requests from two open source projects, labeled by facts the model is not shown (the
maintainers' own commit prefix, the changed files, the line counts), with a script that builds a fresh set from any
repository. It is real text by real authors, so it is harder, and it comes with its own caveats about how far the labels can
be trusted: read `github-prs/README.md`.

## Make your own

Each line is one JSON object: `state`, `questions` and `gold` (and optionally `id` and `images`). The format and what is
judged are in SPEC 15.1. A good set has the clear cases your system must never get wrong, and a few hard ones, and it is
labeled by someone who can say why each answer is right.
