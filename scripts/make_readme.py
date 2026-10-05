#!/usr/bin/env python3
"""Rewrite the drafted README prose with Hemmingway, keep code blocks byte-for-byte, and overwrite README.md.

Usage:  HEM_KEY=<key> python3 scripts/make_readme.py DRAFT.md            # rewrite + write README.md (backs up to README.md.bak)
        HEM_KEY=<key> python3 scripts/make_readme.py DRAFT.md --dry-run  # write rewritten_README.md next to the draft only
Needs only the standard library. The key is read from the environment and never written to a file.
"""
import json, os, re, shutil, sys, urllib.request

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
README = os.path.join(REPO, "README.md")

SYS = ("Rewrite the user's README prose in a plain, direct, casual first-person voice, short sentences, no marketing "
       "fluff. Keep ALL markdown structure exactly: headings, bullets, bold, inline `code`, links, and any SPEC "
       "references. Do not add or remove facts. 'I' is only the author talking about their own choices; never use 'I' for the software, say 'Ignatius', 'it' or 'you'. Output only the rewritten markdown.")


def rewrite(text):
    body = json.dumps({"model": "hemmingway-27b",
                       "messages": [{"role": "system", "content": SYS}, {"role": "user", "content": text}]}).encode()
    req = urllib.request.Request("https://hemmingway.io/v1/chat/completions", body,
                                 {"Content-Type": "application/json", "Authorization": "Bearer " + os.environ["HEM_KEY"]})
    with urllib.request.urlopen(req, timeout=180) as r:
        return json.load(r)["choices"][0]["message"]["content"].strip()


def rewrite_prose(path):
    """Rewrite every non-code chunk of a file; fenced blocks pass through untouched."""
    out = []
    for part in re.split(r"(```.*?```)", open(path).read(), flags=re.S):
        if part.startswith("```") or len(part.strip()) < 40:
            out.append(part)
        else:  # keep the chunk's own leading/trailing blank lines so a fence never runs into the prose beside it
            lead, trail = part[:len(part) - len(part.lstrip())], part[len(part.rstrip()):]
            out.append(lead + rewrite(part).strip() + trail)
    return "".join(out)


def fences(s):
    return re.findall(r"```.*?```", s, flags=re.S)


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("--")]
    if not args:
        sys.exit("usage: make_readme.py DRAFT.md [--dry-run]")
    dry = "--dry-run" in sys.argv
    draft = os.path.abspath(args[0])
    HERE = os.path.dirname(draft)
    lines = open(draft).read()  # also used to find the verbatim sections
    # Verbatim sections come from the draft too: Quickstart (up to "## Concepts") and "Judging routes" (up to "## Status page").
    q0, c0 = lines.index("## Quickstart"), lines.index("## Concepts")
    j0, s0 = lines.index("## Judging routes"), lines.index("## Status page")
    pieces = [("head", lines[:q0], True), ("quickstart", lines[q0:c0], False),
              ("tail", lines[c0:j0], True), ("judging", lines[j0:s0], False), ("tail2", lines[s0:], True)]
    final = []
    for name, text, rw in pieces:
        if rw:
            tmp = os.path.join(HERE, f"_{name}.md")
            open(tmp, "w").write(text)
            new = rewrite_prose(tmp)
            print(f"rewrote {name}: {len(text)} -> {len(new)} chars")
            final.append(new)
        else:
            final.append(text)
    result = "".join(final)

    out = os.path.join(HERE, "rewritten_README.md")
    open(out, "w").write(result)
    if fences(result) != fences(lines):
        sys.exit(f"code blocks changed during rewrite; refusing to write README.md (see {out})")
    if dry:
        print("dry run; wrote", out)
        return
    shutil.copy(README, README + ".bak")
    open(README, "w").write(result)
    print("wrote", README, "(backup at README.md.bak)")


if __name__ == "__main__":
    main()
