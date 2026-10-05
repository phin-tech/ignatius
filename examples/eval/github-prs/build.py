#!/usr/bin/env python3
"""Build an `ignatius eval` test set from merged GitHub pull requests.

The model is shown a PR's title (with its `feat:` or `fix:` prefix removed), its description and the files it
changed, and asked three questions. The gold answers come from signals it is never shown:

  type          the maintainers' own conventional-commit prefix on the PR title (feat, fix, perf, refactor, docs, test,
                chore/build/ci)
  touches_tests whether any listed file is a test file (the files are listed, so this tests reading paths)
  size          the lines added plus deleted, in four bands (the counts are shown, so this tests reading numbers)

    python3 build.py                                  # vuejs/core and vitejs/vite, 72 PRs, writes prs.jsonl
    python3 build.py --repo owner/name --repo ... --n 60 --seed 3 --out mine.jsonl

Needs the `gh` CLI, logged in. Titles and descriptions are other people's text: keep the licenses of the repositories
you pull from (NOTICE lists the two used for the committed file), and review what you publish. @mentions and email
addresses are removed here, and a description is cut at 1,500 characters.
"""

from __future__ import annotations

import argparse
import json
import random
import re
import subprocess
import sys
from collections import Counter, defaultdict

TITLE = re.compile(r"^(\w+)(\([^)]*\))?(!)?:\s*(.+)$")
TYPE_OF = {"feat": "feature", "fix": "bugfix", "perf": "performance", "refactor": "refactor", "docs": "docs",
           "test": "tests", "chore": "chore", "build": "chore", "ci": "chore"}
# "update rolldown to 1.1.1", "bump x", "use ESLint v10": a dependency bump, whatever its prefix or scope.
DEP_BUMP = re.compile(r"^(update|bump|upgrade)\b|\b(update|bump|upgrade)\b.*\b(to|from)\s+v?\d|^use\s+\S+.*\bv\d", re.I)
# __tests__, tests, e2e, and Vite's __tests_dts__ (type-level tests); *.spec.ts and *.test.ts
TEST_FILE = re.compile(r"(^|/)(__tests\w*__|tests?|e2e)/|\.(spec|test)\.[cm]?[jt]sx?$")
# CodeRabbit appends an AI-written "Summary by CodeRabbit" between these markers. It is not the author's text and it
# often calls a refactor a bug fix, so it is removed; a PR with nothing else to read is skipped.
CODERABBIT = re.compile(r"<!--\s*This is an auto-generated comment: release notes by coderabbit\.ai\s*-->.*?"
                        r"<!--\s*end of auto-generated comment: release notes by coderabbit\.ai\s*-->", re.S | re.I)
# Bot PRs (Renovate, Dependabot) paste the changelogs of the packages they update and are dull to judge: left out.
BOT = re.compile(r"This PR contains the following updates|renovatebot|dependabot", re.I)
MAX_FILES = 25  # files listed; a PR with more is kept only when a listed one settles touches_tests
MAX_BODY = 1500

# What this builder takes from each repository: its (type, PR count) quotas. Vue has the fixes and refactors; Vite has the rest.
DEFAULT_QUOTAS = {
    "vuejs/core": {"bugfix": 8, "performance": 5, "refactor": 10, "chore": 2},
    "vitejs/vite": {"feature": 12, "docs": 10, "tests": 8, "chore": 8, "bugfix": 6, "performance": 3},
}
# There is no "is this a breaking change" question on purpose: a real one (a `!` in the prefix, or a BREAKING CHANGE:
# footer) is rare, about three PRs in two hundred candidates here, too few positives to score.

QUESTIONS = {
    "type": {"type": "choice", "instructions": "What kind of change is this pull request?", "criteria": {
        "feature": "adds a capability that users or developers of the project can use",
        "bugfix": "fixes behavior that was incorrect",
        "performance": "makes something faster or lighter without changing what it does",
        "refactor": "restructures code without changing what it does",
        "docs": "changes documentation only",
        "tests": "adds or changes tests only",
        "chore": "maintenance: dependencies, tooling, CI, build or release configuration"}},
    "touches_tests": {"type": "noul", "instructions": "Does this pull request change any test files?",
                      "criteria": {"true": "at least one of the changed files is a test file", "false": "none of the changed files is a test file"}},
    "size": {"type": "score", "instructions": "How large is this change, by the lines added plus the lines deleted?",
             "criteria": ["fewer than 10 lines", "10 to 49 lines", "50 to 299 lines", "300 lines or more"]},
}


def size_level(lines: int) -> int:
    return 0 if lines < 10 else 1 if lines < 50 else 2 if lines < 300 else 3


def clean_body(body: str) -> str:
    """The description as the model sees it: no bot summary, no template comments, no email addresses or @mentions, a bounded length."""
    body = CODERABBIT.sub("", body or "")
    body = re.sub(r"<!--.*?-->", "", body, flags=re.S)
    body = re.sub(r"[\w.+-]+@[\w-]+\.[\w.-]+", "<email>", body)
    body = re.sub(r"(?<![\w/])@[A-Za-z0-9][A-Za-z0-9-]*(?:/[\w.-]+)?", "@user", body)
    body = re.sub(r"[ \t]+\n", "\n", body)
    body = re.sub(r"\n{3,}", "\n\n", body).strip()
    if len(body) > MAX_BODY:
        body = body[:MAX_BODY].rsplit(None, 1)[0] + " […]"
    return body


def convert(repo: str, pr: dict):
    """One PR as an eval item, or None if it does not give a clear label."""
    m = TITLE.match(pr["title"])
    if not m:
        return None
    prefix, scope, _bang, subject = m.groups()
    kind = TYPE_OF.get(prefix)
    # Dependency bumps are skipped: how a project labels them is its own convention (Vite ships rolldown updates as
    # `feat` or `fix`), not something a reader could tell from the change.
    if kind is None or (scope or "").strip("()").startswith("deps") or DEP_BUMP.search(subject.strip()):
        return None
    if BOT.search(pr.get("body") or ""):
        return None
    body = clean_body(pr.get("body") or "")
    if len(body) < 80:  # nothing to read
        return None
    files = [f["path"] for f in pr.get("files") or []]
    shown = files[:MAX_FILES]
    tests = any(TEST_FILE.search(p) for p in shown)
    if not tests and any("test" in p.lower() for p in shown):
        return None  # a path like vitestSetup.ts or test-utils.ts is test infrastructure by some readings and not by others
    if pr["changedFiles"] > MAX_FILES and not tests:
        return None  # a test file may be among the unlisted ones: the label would not be knowable
    lines = pr["additions"] + pr["deletions"]
    state = {"title": subject.strip(), "description": body, "changed_files": shown, "files_changed": pr["changedFiles"],
             "lines_added": pr["additions"], "lines_deleted": pr["deletions"]}
    return {"id": f"{repo.replace('/', '-')}-{pr['number']}", "state": state, "questions": QUESTIONS,
            "gold": {"type": kind, "touches_tests": tests, "size": size_level(lines)}}


def fetch(repo: str, limit: int) -> list[dict]:
    out = subprocess.run(["gh", "pr", "list", "-R", repo, "--state", "merged", "--limit", str(limit), "--json",
                          "number,title,body,files,additions,deletions,changedFiles"], capture_output=True, text=True)
    if out.returncode != 0:
        sys.exit(f"gh failed for {repo}: {out.stderr.strip()}")
    return json.loads(out.stdout)


def select(pools: dict[str, list[dict]], quotas: dict[str, dict[str, int]], seed: int) -> list[dict]:
    rng = random.Random(seed)
    cand: dict[str, list[dict]] = {}
    for repo, prs in pools.items():
        items = [it for it in (convert(repo, p) for p in prs) if it]
        rng.shuffle(items)
        cand[repo] = items
    chosen: dict[str, dict] = {}
    taken: dict[tuple[str, str], int] = Counter()
    for repo, q in quotas.items():
        for it in cand.get(repo, []):
            key = (repo.replace("/", "-"), it["gold"]["type"])
            if it["id"] not in chosen and taken[key] < q.get(it["gold"]["type"], 0):
                chosen[it["id"]] = it
                taken[key] += 1
    items = sorted(chosen.values(), key=lambda it: it["id"])
    rng.shuffle(items)
    return items


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--repo", action="append", help="owner/name; repeat. Default: vuejs/core and vitejs/vite")
    ap.add_argument("--n", type=int, default=0, help="with --repo: how many PRs to take, spread evenly over the types found")
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--limit", type=int, default=700, help="how many recent merged PRs to look through, per repository")
    ap.add_argument("--out", default="prs.jsonl")
    args = ap.parse_args()

    repos = args.repo or list(DEFAULT_QUOTAS)
    pools = {r: fetch(r, args.limit) for r in repos}
    if args.repo:
        per = max(1, (args.n or 60) // (len(repos) * len(set(TYPE_OF.values()))))
        quotas = {r: {t: per for t in set(TYPE_OF.values())} for r in repos}
    else:
        quotas = DEFAULT_QUOTAS
    items = select(pools, quotas, args.seed)
    with open(args.out, "w") as f:
        for it in items:
            f.write(json.dumps(it, ensure_ascii=False) + "\n")
    g = [it["gold"] for it in items]
    print(f"{len(items)} PRs -> {args.out}")
    print("  type:", dict(Counter(x["type"] for x in g)))
    print("  touches_tests yes:", sum(x["touches_tests"] for x in g),
          " size 0..3:", [sum(1 for x in g if x["size"] == s) for s in range(4)])
    return 0


if __name__ == "__main__":
    sys.exit(main())
