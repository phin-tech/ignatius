"""Turn BANKING77 into an `ignatius eval` test set: one `choice` question with 77 options, as benchmarks/classify asks it.

    uv run python banking77.py --n 200 --out banking77.jsonl

It reads the same cached sample as `benchmarks/classify/bench.py` (IGNATIUS_BENCH_CACHE, default
~/.cache/ignatius-bench) and fetches one row at a time from the Hugging Face dataset server when there is none. The
options are described by their labels with the underscores removed, so that is the wording the optimizer starts from.
"""

from __future__ import annotations

import argparse
import json
import os
import random
import time
from concurrent.futures import ThreadPoolExecutor

import httpx

HF = "https://datasets-server.huggingface.co"
DATASET = "legacy-datasets/banking77"
INSTRUCTIONS = "Which banking intent is the customer asking about?"


def hf(path: str, **params) -> dict:
    for attempt in range(10):
        r = httpx.get(f"{HF}/{path}", params=params, timeout=60)
        if r.status_code != 429:
            r.raise_for_status()
            return r.json()
        time.sleep(float(r.headers.get("Retry-After") or min(30, 2**attempt)))
    raise RuntimeError("the dataset server kept rate limiting")


def label_names() -> list[str]:
    info = hf("info", dataset=DATASET)["dataset_info"]
    feats = info["features"] if "features" in info else next(iter(info.values()))["features"]
    for f in feats.values() if isinstance(feats, dict) else feats:
        if isinstance(f, dict) and f.get("_type") == "ClassLabel":
            return f["names"]
    raise RuntimeError("no ClassLabel in the dataset")


def sample(n: int, seed: int) -> list[dict]:
    cache = os.environ.get("IGNATIUS_BENCH_CACHE") or os.path.expanduser("~/.cache/ignatius-bench")
    path = os.path.join(cache, f"{DATASET.replace('/', '__')}-default-test-n{n}-seed{seed}.json")
    if os.path.exists(path):
        with open(path) as f:
            return json.load(f)
    total = hf("rows", dataset=DATASET, config="default", split="test", offset=0, length=1)["num_rows_total"]
    offsets = random.Random(seed).sample(range(total), min(n, total))
    with ThreadPoolExecutor(3) as pool:
        rows = list(pool.map(lambda o: hf("rows", dataset=DATASET, config="default", split="test", offset=o,
                                          length=1)["rows"][0]["row"], offsets))
    os.makedirs(cache, exist_ok=True)
    with open(path, "w") as f:
        json.dump(rows, f)
    return rows


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--n", type=int, default=200)
    ap.add_argument("--seed", type=int, default=7, help="the sample seed (7 is the one bench.py uses)")
    ap.add_argument("--out", default="banking77.jsonl")
    args = ap.parse_args()
    names = label_names()
    questions = {"intent": {"type": "choice", "instructions": INSTRUCTIONS,
                            "criteria": {nm: nm.replace("_", " ") for nm in names}}}
    rows = sample(args.n, args.seed)
    with open(args.out, "w") as f:
        for i, r in enumerate(rows):
            f.write(json.dumps({"id": f"b{i}", "state": r["text"], "questions": questions,
                                "gold": {"intent": names[r["label"]]}}) + "\n")
    print(f"wrote {len(rows)} rows to {args.out}")


if __name__ == "__main__":
    main()
