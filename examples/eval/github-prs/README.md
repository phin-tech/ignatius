# Sample test set: GitHub pull requests

`prs.jsonl` is 72 merged pull requests from [vuejs/core](https://github.com/vuejs/core) (25) and
[vitejs/vite](https://github.com/vitejs/vite) (47), as a labeled test set for `ignatius eval` (SPEC 15). It is real text
written by people, which makes it a harder and more realistic set than the synthetic one in the parent directory. A
snapshot of PRs merged between February and October 2026 is committed; `build.py` makes a fresh one.

The model is shown a PR's **title** (with the `feat:` or `fix:` prefix removed), its **description** and the **files it
changed** with the line counts, and asked three questions. The gold answers come from facts it is never shown:

| question | type | gold, and where it comes from |
|---|---|---|
| `type` | choice | `feature`, `bugfix`, `performance`, `refactor`, `docs`, `tests` or `chore`: the maintainers' own conventional-commit prefix on the PR title (12 / 14 / 8 / 10 / 10 / 8 / 10 PRs) |
| `touches_tests` | noul | whether any listed file is a test file, by its path (43 yes, 29 no); it tests reading a list of paths |
| `size` | score | the lines added plus deleted in four bands: under 10, 10 to 49, 50 to 299, 300 or more (25 / 11 / 19 / 17 PRs); the counts are shown, so it tests reading numbers |

## Run it

```sh
ignatius eval --config ignatius.toml --data examples/eval/github-prs/prs.jsonl --route jev --route 'cascade:cheap@0.8>jev' --sweep
```

The items are large (descriptions run to 1,500 characters and file lists to 25 paths, 166 KiB in all), so a run costs more
than the support-ticket set: about 62,000 input tokens per route (Jev, measured).

For reference, Jev (`jev-latest`, 2026-10-04) scored 93.1% overall (201 of 216 judged questions): 100% on `touches_tests`
and on `size`, and **79.2% on `type`**. `type` is the question that separates models here. Your numbers will differ.

## Read this before you trust a number

- **`type` is the maintainers' convention, not a ground truth.** Two readers can disagree about whether a change is a
  refactor or a bug fix, and Jev's confident disagreements are mostly defensible: a Vue change that restructures hydration
  code and fixes a hydration problem, a Vite PR whose only changed file is a spec but whose description talks about fixing
  a leak. The labels are good for comparing models and routes with one another; the absolute accuracy of `type` is not a
  measure of how right a model is.
- **Several things are left out on purpose**, because they would put label noise or an unfair signal into the set:
  - *Dependency bumps* (a `deps` scope, or a subject like "update rolldown to 1.1.1" or "use ESLint v10"): Vite ships some of
    these as `feat` and some as `fix`, which no reader could predict from the change.
  - *Bot PRs* (Renovate and Dependabot), whose descriptions paste other projects' changelogs.
  - *CodeRabbit's AI summaries.* Most Vue PRs carry a "Summary by CodeRabbit" the author did not write, and it often calls a
    refactor a bug fix. The block is removed, and a PR with nothing else to read is skipped. An earlier version of this set
    left it in, and many of Jev's confident misses were Vue refactors it read as bug fixes because of that text.
  - *PRs with no real description* (under 80 characters), *PRs with more than 25 changed files* unless a listed test file
    settles `touches_tests`, and *PRs whose paths mention "test" without being clearly a test file* (`vitestSetup.ts`).
- **There is no "is it a breaking change" question**, though it is an obvious one. A real one (a `!` in the prefix or a
  `BREAKING CHANGE:` footer) is rare: about three of more than 200 candidates here, too few positives to score.
- **72 items is small.** With intervals this wide, only a large gap between two routes will show up.
- **Both projects are JavaScript front-end tooling**, so this says little about how a model does on other kinds of PRs.

## Make your own

```sh
python3 build.py                                       # the two repositories above: writes prs.jsonl
python3 build.py --repo owner/name --repo owner/other --n 60 --seed 3 --out mine.jsonl
```

It needs the `gh` CLI, logged in, and it works best on a repository whose PR titles use conventional-commit prefixes.
Titles and descriptions are other people's text: keep the license of what you pull (`NOTICE` has the two this file uses),
and read what you publish. `@mentions` and email addresses are removed.
