#!/usr/bin/env python3
"""Latency distribution for a streaming chat endpoint.

Reports p50/p90/p99 for TTFT and for decode throughput, over N paced requests.
Pacing matters: the zcode-proxy velocity pacer allows 10 req/min (burst 3),
so hammering it back-to-back measures the pacer, not the model.
"""

import argparse
import json
import statistics
import sys
import time

import requests


def pct(vals, p):
    if not vals:
        return None
    s = sorted(vals)
    if len(s) == 1:
        return s[0]
    k = (len(s) - 1) * (p / 100.0)
    lo, hi = int(k), min(int(k) + 1, len(s) - 1)
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


PROMPT = (
    "You are a helpful assistant.\n\n"
    "Write a focused explanation (about 250 words) of how a token-bucket rate "
    "limiter works, why the refill rate matters, and what happens under "
    "sustained overload. Use flowing prose, no bullet lists."
)


def run_once(base, key, model, prompt, max_tokens):
    payload = {"model": model, "messages": [{"role": "user", "content": prompt}],
               "max_tokens": max_tokens, "stream": True,
               "stream_options": {"include_usage": True}}
    t0 = time.perf_counter()
    t_reason = t_content = last = None
    usage = None
    with requests.post(f"{base}/chat/completions",
                       headers={"Authorization": f"Bearer {key}"},
                       json=payload, stream=True, timeout=900) as r:
        if r.status_code != 200:
            raise RuntimeError(f"HTTP {r.status_code}: {r.read()[:300]!r}")
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
                if dl.get("reasoning_content") or dl.get("reasoning"):
                    t_reason = t_reason or now
                    last = now
                if dl.get("content"):
                    t_content = t_content or now
                    last = now
    t_end = time.perf_counter()
    out = (usage or {}).get("completion_tokens", 0) or 0
    win = (last - t_content) if (t_content and last) else 0.0
    return {
        "ttft_reasoning": (t_reason - t0) if t_reason else None,
        "ttft_content": (t_content - t0) if t_content else None,
        "total": t_end - t0,
        "out_tok": out,
        "in_tok": (usage or {}).get("prompt_tokens", 0),
        "tok_s_decode": (out / win) if out and win > 1e-6 else None,
        "tok_s_e2e": (out / (t_end - t0)) if out else None,
    }


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:18082/v1")
    ap.add_argument("--key", default="localtest123")
    ap.add_argument("--model", default="glm-5.3")
    ap.add_argument("--n", type=int, default=15)
    ap.add_argument("--pace", type=float, default=7.0)
    ap.add_argument("--max-tokens", type=int, default=400)
    ap.add_argument("--json-out", default=None)
    a = ap.parse_args()

    print(f"model={a.model}  n={a.n}  pace={a.pace}s  max_tokens={a.max_tokens}")
    print(f"{'#':>3} {'in_tok':>7} {'ttftR':>7} {'ttftC':>7} {'tot_s':>7} "
          f"{'out_tok':>7} {'dec_t/s':>8} {'e2e_t/s':>8}")
    print("-" * 62)
    rows = []
    for i in range(1, a.n + 1):
        if a.pace > 0 and i > 1:
            time.sleep(a.pace)
        try:
            r = run_once(a.base, a.key, a.model, PROMPT, a.max_tokens)
        except Exception as e:
            print(f"{i:>3}  FAILED {type(e).__name__}: {e}")
            continue
        rows.append(r)
        tr = f"{r['ttft_reasoning']:.2f}" if r['ttft_reasoning'] else "  -"
        print(f"{i:>3} {r['in_tok']:>7} {tr:>7} {r['ttft_content']:>7.2f} "
              f"{r['total']:>7.2f} {r['out_tok']:>7} "
              f"{r['tok_s_decode']:>8.1f} {r['tok_s_e2e']:>8.1f}")

    def block(label, key, nd=2, scale=1.0):
        v = [r[key] * scale for r in rows if r.get(key) is not None]
        if not v:
            return f"{label:<12} n/a"
        return (f"{label:<12} p50={pct(v,50):.{nd}f}  p90={pct(v,90):.{nd}f}  "
                f"p99={pct(v,99):.{nd}f}  min={min(v):.{nd}f}  max={max(v):.{nd}f}")

    print("\n== distribution ==")
    print(block("TTFT (s)", "ttft_content"))
    print(block("TTFT reason(s)", "ttft_reasoning"))
    print(block("tok/s decode", "tok_s_decode", 1))
    print(block("tok/s e2e", "tok_s_e2e", 1))
    print(block("total (s)", "total"))
    print(f"{'median out':<12} {statistics.median([r['out_tok'] for r in rows]):.0f} tokens")

    if a.json_out:
        with open(a.json_out, "w") as f:
            json.dump({"model": a.model, "runs": rows}, f, indent=2)
        print(f"\nwrote {a.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
