#!/usr/bin/env python3
"""流式验收：记录首字时间、chunk 数、总时长。"""
import json, sys, time, urllib.request

PORT = sys.argv[1] if len(sys.argv) > 1 else "8790"
URL = f"http://127.0.0.1:{PORT}/v1/chat/completions"
MODEL = sys.argv[2] if len(sys.argv) > 2 else "qwen3.7-plus"
PROMPT = sys.argv[3] if len(sys.argv) > 3 else "用三句话介绍杭州"

body = json.dumps({"model": MODEL, "messages": [{"role": "user", "content": PROMPT}], "stream": True}).encode()
req = urllib.request.Request(URL, data=body, headers={"Content-Type": "application/json"})
t0 = time.time()
first = None
chunks = 0
buf = []
with urllib.request.urlopen(req, timeout=180) as r:
    for raw in r:
        line = raw.decode().strip()
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if payload == "[DONE]":
            break
        try:
            obj = json.loads(payload)
        except Exception:
            continue
        d = (obj.get("choices") or [{}])[0].get("delta", {}).get("content")
        if d:
            chunks += 1
            if first is None:
                first = time.time() - t0
            buf.append(d)
total = time.time() - t0
print(f"model={MODEL} first_chunk={first:.2f}s chunks={chunks} total={total:.2f}s")
print("content:", "".join(buf)[:160].replace("\n", " | "))
