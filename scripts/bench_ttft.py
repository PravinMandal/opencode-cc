#!/usr/bin/env python3
"""TTFT + output throughput benchmark for an OpenAI-compatible streaming endpoint.

Measures:
  - TTFT   : request send -> first SSE chunk carrying real output content.
  - tok/s  : completion_tokens / (last content chunk - first content chunk),
             i.e. steady-state decode speed (what a streaming user feels).
  - e2e    : completion_tokens / total wall time (includes prefill + overhead).
  - burst  : whether the stream is genuinely incremental. If nearly all chunks
             land inside one flush window, the endpoint is buffering, and tok/s
             is an artefact rather than real decode throughput.

Usage:
  python3 scripts/bench_ttft.py --base http://127.0.0.1:18082/v1 --key localtest123 \
      --model glm-5.3 --runs 3 --prompt-tokens 200 --max-tokens 600
"""

import argparse
import json
import statistics
import sys
import time

import requests

TASK = (
    "\n\nTask: explain in detail how a fixed-window rate limiter with a token "
    "bucket works, including why the refill rate matters, how it interacts "
    "with burst capacity, and what happens under sustained overload. Use "
    "clear, flowing prose. Do not use bullet lists."
)


def build_prompt(words: int) -> str:
    filler = (
        "The quick brown fox jumps over the lazy dog while the streaming proxy "
        "measures latency distributions across many concurrent turns and "
        "aggregates token counts for each response. "
    )
    w = filler.split()
    ctx = " ".join((w * (words // len(w) + 2))[:words])
    return (
        "You are a helpful assistant. Answer precisely and concisely.\n\n"
        "Background context (may be partly irrelevant):\n" + ctx + TASK
    )


def run_once(base, key, model, prompt, max_tokens):
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens,
        "stream": True,
        "stream_options": {"include_usage": True},
    }
    headers = {"Authorization": f"Bearer {key}"}
    url = f"{base}/chat/completions"

    t0 = time.perf_counter()
    first_content = None
    last_content = None
    usage = None
    n_chunks = 0
    n_content = 0
    text = []

    with requests.post(url, headers=headers, json=payload, stream=True, timeout=600) as r:
        if r.status_code != 200:
            raise RuntimeError(f"HTTP {r.status_code}: {r.read()[:400]!r}")
        t_headers = time.perf_counter()
        for line in r.iter_lines():
            if not line:
                continue
            line = line.decode("utf-8", "replace") if isinstance(line, bytes) else line
            if not line.startswith("data:"):
                continue
            data = line[5:].strip()
            if data == "[DONE]":
                break
            try:
                obj = json.loads(data)
            except json.JSONDecodeError:
                continue
            n_chunks += 1
            if obj.get("usage"):
                usage = obj["usage"]
            now = time.perf_counter()
            for ch in obj.get("choices", []):
                d = ch.get("delta") or {}
                piece = d.get("content") or d.get("reasoning_content") or d.get("reasoning")
                if piece:
                    n_content += 1
                    if first_content is None:
                        first_content = now
                    last_content = now
                    if d.get("content"):
                        text.append(d["content"])

    t_end = time.perf_counter()
    out_tok = (usage or {}).get("completion_tokens", 0) or 0
    in_tok = (usage or {}).get("prompt_tokens", 0) or 0
    win = (last_content - first_content) if (first_content and last_content) else 0.0

    return {
        "ttft": (first_content - t0) if first_content else None,
        "t_headers": t_headers - t0,
        "total": t_end - t0,
        "completion_tokens": out_tok,
        "prompt_tokens": in_tok,
        "decode_window": win,
        "tok_s": (out_tok / win) if out_tok and win > 1e-6 else None,
        "tok_s_e2e": (out_tok / (t_end - t0)) if out_tok else None,
        "chunks": n_chunks,
        "content_chunks": n_content,
        "ms_per_chunk": (win * 1000.0 / n_content) if n_content > 1 and win > 0 else None,
        "text": "".join(text),
    }


def f(v, nd=2):
    return "  n/a" if v is None else f"{v:.{nd}f}"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base", default="http://127.0.0.1:18082/v1")
    ap.add_argument("--key", default="localtest123")
    ap.add_argument("--model", default="glm-5.3")
    ap.add_argument("--runs", type=int, default=3)
    ap.add_argument("--prompt-words", type=int, default=200)
    ap.add_argument("--max-tokens", type=int, default=600)
    ap.add_argument("--json-out", default=None)
    ap.add_argument("--show-text", action="store_true")
    args = ap.parse_args()

    prompt = build_prompt(args.prompt_words)
    print(f"endpoint  : {args.base}/chat/completions")
    print(f"model     : {args.model}")
    print(f"prompt    : ~{args.prompt_words} words of context | max_tokens={args.max_tokens}")
    print(f"runs      : {args.runs}\n")

    rows = []
    for i in range(1, args.runs + 1):
        try:
            r = run_once(args.base, args.key, args.model, prompt, args.max_tokens)
        except Exception as e:
            print(f"run {i}/{args.runs}: FAILED {type(e).__name__}: {e}")
            continue
        rows.append(r)
        print(f"run {i}/{args.runs}: ttft={f(r['ttft'])}s  hdr={f(r['t_headers'])}s  "
              f"tok/s={f(r['tok_s'],1)}  e2e={f(r['tok_s_e2e'],1)}  total={f(r['total'])}s  "
              f"out={r['completion_tokens']}tok in={r['prompt_tokens']}tok  "
              f"chunks={r['content_chunks']} ms/chunk={f(r['ms_per_chunk'],1)}")
        if args.show_text:
            print("--- text ---\n" + r["text"][:900] + "\n--------------\n")

    if not rows:
        print("\nno successful runs")
        return 1

    def stat(key, nd=2):
        vals = [r[key] for r in rows if r.get(key) is not None]
        if not vals:
            return "n/a"
        return f"min={min(vals):.{nd}f} med={statistics.median(vals):.{nd}f} max={max(vals):.{nd}f}"

    print("\n== summary ==")
    print(f"TTFT      : {stat('ttft')} s")
    print(f"hdr time  : {stat('t_headers')} s")
    print(f"tok/s     : {stat('tok_s',1)}  (decode-only)")
    print(f"tok/s e2e : {stat('tok_s_e2e',1)}  (whole request)")
    print(f"total     : {stat('total')} s")
    print(f"out tokens: {stat('completion_tokens',0)}")
    print(f"ms/chunk  : {stat('ms_per_chunk',1)}  -> large value == buffered stream")

    if args.json_out:
        with open(args.json_out, "w") as fh:
            json.dump({"runs": rows, "model": args.model, "base": args.base}, fh, indent=2)
        print(f"\nwrote {args.json_out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
