#!/usr/bin/env python3
"""Mini capability eval (terminal-bench style, low request count).

Usage: MODEL=fledge-alpha-free BASE=http://127.0.0.1:8787/v1 python3 bench_eval.py
One request per task, temperature 0, automatic verification.
"""
import json, os, re, sys, time
import requests

BASE = os.environ.get("BASE", "http://127.0.0.1:8787/v1")
MODEL = os.environ.get("MODEL", "fledge-alpha-free")
TIMEOUT = int(os.environ.get("TIMEOUT", "120"))

def chat(messages, tools=None, max_tokens=2048):
    body = {"model": MODEL, "messages": messages, "temperature": 0,
            "max_tokens": max_tokens, "stream": False}
    if tools:
        body["tools"] = tools
    t0 = time.time()
    r = requests.post(f"{BASE}/chat/completions", json=body, timeout=TIMEOUT)
    dt = time.time() - t0
    r.raise_for_status()
    return r.json(), dt

def text_of(resp):
    return resp["choices"][0]["message"].get("content") or ""

# ---------------- tasks ----------------
def t_math():
    resp, dt = chat([{"role": "user", "content":
        "Compute 17*23 + 5^4. Answer with only the final integer."}])
    return "1016" in text_of(resp).replace(",", ""), text_of(resp).strip()[:80], dt

def t_logic():
    resp, dt = chat([{"role": "user", "content":
        "All bloops are razzies. Some razzies are lazzies. "
        "Mary is a bloop. Is Mary definitely a lazzie? Answer yes or no, then one sentence why."}])
    t = text_of(resp).lower()
    return t.strip().startswith("no"), text_of(resp).strip()[:80], dt

def t_code():
    resp, dt = chat([{"role": "user", "content":
        "Write a Python function `is_pal(s)` that returns True if string s is a palindrome "
        "ignoring case and non-alphanumerics. Output only the code block."}])
    t = text_of(resp)
    ok = "def is_pal" in t and ("isalnum" in t or "isalpha" in t or "[^a-z0-9]" in t)
    return ok, "def present+filter" if ok else t[:80], dt

def t_instruction():
    resp, dt = chat([{"role": "user", "content":
        "List exactly 3 colors, one per line, no numbering, no extra words."}])
    lines = [l.strip() for l in text_of(resp).strip().splitlines() if l.strip()]
    ok = len(lines) == 3 and all(len(l.split()) <= 3 for l in lines)
    return ok, str(lines)[:80], dt

def t_json():
    resp, dt = chat([{"role": "user", "content":
        "Output a JSON object with keys 'name' (string), 'age' (number), 'tags' (array of 2 strings). No prose."}])
    try:
        m = re.search(r"\{.*\}", text_of(resp), re.S)
        o = json.loads(m.group(0))
        ok = isinstance(o["name"], str) and isinstance(o["age"], (int, float)) and len(o["tags"]) == 2
    except Exception:
        ok = False
    return ok, text_of(resp).strip()[:80], dt

def t_needle():
    hay = ("The quick brown fox jumps over the lazy dog. " * 40)
    resp, dt = chat([{"role": "user", "content":
        hay + "\n\nThe secret code is ZEPHYR-42. What is the secret code? Answer with just the code."}])
    return "ZEPHYR-42" in text_of(resp), text_of(resp).strip()[:80], dt

def t_tool():
    tools = [{"type": "function", "function": {
        "name": "get_weather", "description": "Get weather for a city",
        "parameters": {"type": "object", "properties": {"city": {"type": "string"}},
                       "required": ["city"]}}}]
    resp, dt = chat([{"role": "user", "content": "What's the weather in Paris?"}], tools=tools)
    msg = resp["choices"][0]["message"]
    tcs = msg.get("tool_calls") or []
    ok = bool(tcs) and tcs[0]["function"]["name"] == "get_weather" and "paris" in tcs[0]["function"]["arguments"].lower()
    return ok, json.dumps(tcs)[:80] if tcs else (msg.get("content") or "")[:80], dt

def t_reasoning():
    resp, dt = chat([{"role": "user", "content":
        "A bat and ball cost $1.10 total. The bat costs $1.00 more than the ball. "
        "How much does the ball cost in cents? Final line: 'ANSWER: <cents>'."}])
    return "ANSWER: 5" in text_of(resp), text_of(resp).strip()[-60:], dt

def t_summarize():
    txt = ("Photosynthesis converts sunlight, water, and CO2 into glucose and oxygen. "
           "It occurs in chloroplasts, mainly in leaves. Chlorophyll absorbs light. " * 6)
    resp, dt = chat([{"role": "user", "content": f"Summarize in one sentence (max 25 words):\n{txt}"}])
    words = len(text_of(resp).split())
    return 3 <= words <= 35, f"{words} words", dt

def t_format():
    resp, dt = chat([{"role": "user", "content":
        "Output a markdown table with columns Name and Score and exactly 2 data rows."}])
    t = text_of(resp)
    ok = t.count("|") >= 8 and "---" in t
    return ok, "table" if ok else t[:60], dt

TASKS = [
    ("math", t_math), ("logic", t_logic), ("code", t_code),
    ("instruction-following", t_instruction), ("json-output", t_json),
    ("needle-in-haystack", t_needle), ("tool-call", t_tool),
    ("reasoning", t_reasoning), ("summarize", t_summarize), ("markdown-table", t_format),
]

if __name__ == "__main__":
    rows = []
    for name, fn in TASKS:
        try:
            ok, detail, dt = fn()
        except Exception as e:
            ok, detail, dt = False, f"ERROR: {e}"[:80], 0
        rows.append((name, ok, dt, detail))
        print(f"{'PASS' if ok else 'FAIL'}  {name:22s} {dt:6.1f}s  {detail}")
    passed = sum(1 for _, ok, _, _ in rows)
    avg = sum(d for _, _, d, _ in rows) / max(len(rows), 1)
    print(f"\n{MODEL}: {passed}/{len(rows)} passed, avg {avg:.1f}s/req, total {sum(d for _,_,d,_ in rows):.0f}s")
