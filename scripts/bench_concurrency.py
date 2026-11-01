#!/usr/bin/env python3
"""Concurrency probe for the zcode-proxy streaming endpoint.

The proxy's velocity pacer has capacity=burst=3 with a 15s queue timeout, so
concurrency above 3 mostly measures the pacer queue, not the model. This script
reports TTFT/decode per worker so the degradation curve is visible.
"""

import argparse
import statistics
import sys
import time
from concurrent.futures import ThreadPoolExecutor

import requests

PROMPT = (
    "You are a helpful assistant.\n\nWrite a focused explanation (about 200 "
    "words) of how a token-bucket rate limiter works and what happens under "
    "sustained overload. Flowing prose, no bullet lists."
)


def one(base, key, model, idx):
    payload = {"model": model, "messages": [{"role": "user", "content": PROMPT}],
               "max_tokens": 300, "stream": True,
               "stream_options": {"include_usage": True}}
    t0 = time.perf_counter()
    t_content = last = None
    usage = None
    status = 0
    try:
        with requests.post(f"{base}/chat/completions",
                           headers={"Authorization": f"Bearer {key}"},
                           json=payload, stream=True, timeout=900) as r:
            status = r.status_code
            if status != 200:
                # NB: read the body BEFORE leaving the with-block; use .text
                # (r.read() is unreliable on a streamed response object).
                body = (r.text or "")[:200].replace("\n", " ")
                return {"worker": idx, "status": status, "error": body}
            for line in r.iter_lines():
                if not line:
                    continue
                line = line.decode("utf-8", "replace")
                if not line.startswith("data:"):
                    continue
                d = line[5:].strip()
                if d == "[DONE]":
                    break
                try:
                    o = json.loads(d)
                except json.JSONDecodeError:
                    continue
                if o.get("usage"):
                    usage = o["usage"]
                now = time.perf_counter()
                for ch in o.get("choices", []):
                    dl = ch.get("delta") or {}
                    if dl.get("content") or dl.get("reasoning_content"):
                        t_content = t_content or now
                        last = now
    except Exception as e:
        return {"worker": idx, "status": status, "error": f"{type(e).__name__}: {e}"}
    t_end = time.perf_counter()
    out = (usage or {}).get("completion_tokens", 0) or 0
    win = (last - t_content) if (t_content and last) else 0.0
    return {
        "worker": idx, "status": status,
        "ttft": (t_content - t0) if t_content else None,
        "total": t_end - t0, "out_tok": out,
        "tok_s": (out / win) if out and win > 1e-6 else None,
    }


import json  # noqa: E402  (used inside worker)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:18082/v1")
    ap.add_argument("--key", default="localtest123")
    ap.add_argument("--model", default="glm-5.3")
    ap.add_argument("--concurrency", default="1,2,3,4,6")
    a = ap.parse_args()

    print(f"model={a.model}")
    print(f"{'conc':>5} {'ok':>4} {'ttft_p50':>9} {'ttft_max':>9} "
          f"{'dec_t/s':>8} {'total_p50':>10}")
    print("-" * 50)
    for c in [int(x) for x in a.concurrency.split(",")]:
        with ThreadPoolExecutor(max_workers=c) as ex:
            t0 = time.perf_counter()
            rows = list(ex.map(lambda i: one(a.base, a.key, a.model, i), range(c)))
            wall = time.perf_counter() - t0
        ok = [r for r in rows if r.get("ttft") is not None]
        bad = [r for r in rows if r.get("ttft") is None]
        if not ok:
            print(f"{c:>5} {0:>4}  all failed: {bad[0].get('error','?')[:60]}")
            continue
        ttfts = [r["ttft"] for r in ok]
        tps = [r["tok_s"] for r in ok if r["tok_s"]]
        tots = [r["total"] for r in ok]
        print(f"{c:>5} {len(ok):>4} {statistics.median(ttfts):>9.2f} "
              f"{max(ttfts):>9.2f} "
              f"{(statistics.median(tps) if tps else 0):>8.1f} "
              f"{statistics.median(tots):>10.2f}")
        for r in bad:
            print(f"      worker {r['worker']} failed: {r.get('error','?')[:90]}")
        if c != max(int(x) for x in a.concurrency.split(",")):
            time.sleep(25)  # let the velocity bucket refill before the next level
    return 0


if __name__ == "__main__":
    sys.exit(main())
