#!/usr/bin/env python3
"""并发验收：同时打 N 个请求，比较墙钟时间与单个耗时。"""
import json, sys, threading, time, urllib.request

PORT = sys.argv[1] if len(sys.argv) > 1 else "8790"
N = int(sys.argv[2]) if len(sys.argv) > 2 else 3
URL = f"http://127.0.0.1:{PORT}/v1/chat/completions"
res = []

def call(i):
    body = json.dumps({"model": "qwen3.7-plus",
                       "messages": [{"role": "user", "content": f"第 {i} 个请求：只回复两个字：收到"}]}).encode()
    req = urllib.request.Request(URL, data=body, headers={"Content-Type": "application/json"})
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            d = json.loads(r.read() or b"{}")
        c = ((d.get("choices") or [{}])[0].get("message") or {}).get("content", "")
        res.append((i, round(time.time() - t0, 2), "OK", c[:20], d.get("_bridge_ms")))
    except Exception as e:
        res.append((i, round(time.time() - t0, 2), "ERR", str(e)[:60], None))

t0 = time.time()
ths = [threading.Thread(target=call, args=(i,)) for i in range(N)]
for t in ths: t.start()
for t in ths: t.join()
wall = time.time() - t0
for r in sorted(res):
    print("req", r)
print(f"wall={wall:.2f}s  (N={N})")
