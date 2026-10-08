#!/usr/bin/env python3
"""Sweep prompt size for TTFT / throughput, and separate reasoning-TTFT from
visible-content-TTFT. Uses scripts/bench_ttft.run_once logic but records both."""

import argparse
import json
import statistics
import sys
import time

import requests

TASK = (
    "\n\nTask: explain in detail how a fixed-window rate limiter with a token "
    "bucket works, including why the refill rate matters and how it interacts "
    "with burst capacity. Use clear, flowing prose. Do not use bullet lists."
)
FILLER = (
    "The quick brown fox jumps over the lazy dog while the streaming proxy "
    "measures latency distributions across many concurrent turns and aggregates "
    "token counts for every response it forwards. "
)


def build_prompt(words):
    w = FILLER.split()
    ctx = " ".join((w * (words // len(w) + 2))[:words])
    return "You are a helpful assistant.\n\nBackground context:\n" + ctx + TASK


def run_once(base, key, model, prompt, max_tokens):
    payload = {
        "model": model, "messages": [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens, "stream": True,
        "stream_options": {"include_usage": True},
    }
    t0 = time.perf_counter()
    t_reason = t_content = last = None
    usage = None
    n_reason = n_content = 0
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
                    n_reason += 1
                    t_reason = t_reason or now
                    last = now
                if dl.get("content"):
                    n_content += 1
                    t_content = t_content or now
                    last = last or now
                    last = now
    t_end = time.perf_counter()
    out = (usage or {}).get("completion_tokens", 0) or 0
    u = usage or {}
    reason_tok = u.get("completion_tokens_details", {}).get("reasoning_tokens")
    finish = None
    return {
        "ttft_reasoning": (t_reason - t0) if t_reason else None,
        "ttft_content": (t_content - t0) if t_content else None,
        "total": t_end - t0,
        "out_tok": out,
        "in_tok": u.get("prompt_tokens", 0),
        "reasoning_tokens": reason_tok,
        "tok_s_e2e": (out / (t_end - t0)) if out else None,
        "n_reason_chunks": n_reason,
        "n_content_chunks": n_content,
        "truncated": finish_reason(u),
    }


def finish_reason(u):
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:18082/v1")
    ap.add_argument("--key", default="localtest123")
    ap.add_argument("--model", default="glm-5.3")
    ap.add_argument("--runs", type=int, default=2)
    ap.add_argument("--max-tokens", type=int, default=400)
    ap.add_argument("--words", default="0,100,500,2000,6000,15000")
    ap.add_argument("--pace", type=float, default=7.0,
                    help="seconds to wait before each request; the proxy's "
                         "velocity pacer allows 10/min (burst 3), so <6s "
                         "spacing makes the pacer, not the model, the bottleneck")
    ap.add_argument("--json-out", default=None)
    a = ap.parse_args()

    sizes = [int(x) for x in a.words.split(",")]
    print(f"model={a.model} runs={a.runs} max_tokens={a.max_tokens}")
    print(f"{'ctx_words':>9} {'in_tok':>7} {'ttftR':>7} {'ttftC':>7} {'tot_s':>7} "
          f"{'out_tok':>7} {'e2e_t/s':>8}")
    print("-" * 60)
    allrows = []
    for n in sizes:
        p = build_prompt(n)
        for i in range(a.runs):
            if a.pace > 0:
                time.sleep(a.pace)
            try:
                r = run_once(a.base, a.key, a.model, p, a.max_tokens)
            except Exception as e:
                print(f"{n:>9} run{i+1}: FAILED {type(e).__name__}: {e}")
                continue
            r["ctx_words"] = n
            allrows.append(r)
            tr = f"{r['ttft_reasoning']:.2f}" if r['ttft_reasoning'] else "n/a"
            tc = f"{r['ttft_content']:.2f}" if r['ttft_content'] else "n/a"
            ts = f"{r['tok_s_e2e']:.1f}" if r['tok_s_e2e'] else "n/a"
            print(f"{n:>9} {r['in_tok']:>7} {tr:>7} {tc:>7} {r['total']:>7.2f} "
                  f"{r['out_tok']:>7} {ts:>8}")
    if a.json_out:
        with open(a.json_out, "w") as f:
            json.dump(allrows, f, indent=2)
        print(f"\nwrote {a.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
