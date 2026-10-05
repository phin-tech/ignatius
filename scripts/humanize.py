#!/usr/bin/env python3
"""Rewrite the prose of Markdown files with a chat model so it reads like a person wrote it. Code blocks pass through untouched.

Usage: HEM_KEY=<key> python3 scripts/humanize.py SRC_DIR OUT_DIR
Needs only the standard library. The key is read from the environment and never written to a file.
Only prose is sent; fenced code blocks are not. Checks that every code block survives byte for byte.
"""
import json, os, re, sys, urllib.request

SYS = ("Rewrite this technical documentation so it reads like a careful human engineer wrote it: plain words, short "
       "sentences, no hype, no filler, no 'seamlessly' or 'leverage' or 'robust'. Keep ALL markdown structure exactly: "
       "headings, numbered and bulleted lists (same numbers, same order), bold, `inline code`, links and SPEC references. "
       "Do not add or remove facts, steps or options. Write in the second person or neutrally; never use 'I' or 'we'. "
       "Output only the rewritten markdown.")


def rewrite(text):
    body = json.dumps({"model": "hemmingway-27b",
                       "messages": [{"role": "system", "content": SYS}, {"role": "user", "content": text}]}).encode()
    req = urllib.request.Request("https://hemmingway.io/v1/chat/completions", body,
                                 {"Content-Type": "application/json", "Authorization": "Bearer " + os.environ["HEM_KEY"]})
    with urllib.request.urlopen(req, timeout=180) as r:
        return json.load(r)["choices"][0]["message"]["content"].strip()


def humanize(src):
    out = []
    for part in re.split(r"(```.*?```)", src, flags=re.S):
        if part.startswith("```") or len(part.strip()) < 40:
            out.append(part)
        else:  # keep the chunk's own surrounding blank lines so a fence never runs into the prose beside it
            lead, trail = part[:len(part) - len(part.lstrip())], part[len(part.rstrip()):]
            out.append(lead + rewrite(part) + trail)
    return "".join(out)


def main():
    src_dir, out_dir = sys.argv[1], sys.argv[2]
    os.makedirs(out_dir, exist_ok=True)
    for name in sorted(os.listdir(src_dir)):
        if not name.endswith(".md"):
            continue
        src = open(os.path.join(src_dir, name)).read()
        new = humanize(src)
        if re.findall(r"```.*?```", src, flags=re.S) != re.findall(r"```.*?```", new, flags=re.S):
            sys.exit(f"{name}: code blocks changed; refusing to write")
        open(os.path.join(out_dir, name), "w").write(new)
        print("rewrote", name)


if __name__ == "__main__":
    main()
