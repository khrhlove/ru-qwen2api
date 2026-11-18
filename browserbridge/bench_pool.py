#!/usr/bin/env python3
"""账号池压测：走网关直连（不经 bridge）。

用法: python bench_pool.py <key> <并发> <总数> [端口]
"""
import json
import statistics
import sys
import threading
import time
import urllib.request

KEY = sys.argv[1]
CONC = int(sys.argv[2])
TOTAL = int(sys.argv[3])
PORT = sys.argv[4] if len(sys.argv) > 4 else "39201"
URL = f"http://127.0.0.1:{PORT}/v1/chat/completions"

results = []
lock = threading.Lock()
sem = threading.Semaphore(CONC)


def one(i):
    with sem:
        body = json.dumps({"model": "qwen3.7-plus",
                           "messages": [{"role": "user", "content": f"第{i}个：只回复两个字：收到"}],
                           "stream": False}).encode()
        req = urllib.request.Request(URL, data=body,
                                     headers={"Content-Type": "application/json",
                                              "Authorization": f"Bearer {KEY}"})
        t0 = time.time()
        ok, err, txt = False, "", ""
        try:
            with urllib.request.urlopen(req, timeout=120) as r:
                d = json.loads(r.read() or b"{}")
            c = ((d.get("choices") or [{}])[0].get("message") or {}).get("content", "")
            txt = c[:16]
            ok = bool(c.strip())
            if not ok:
                err = (d.get("error") or {}).get("message", "empty")[:50]
        except Exception as e:
            err = str(e)[:50]
        with lock:
            results.append({"i": i, "ms": int((time.time() - t0) * 1000), "ok": ok,
                            "txt": txt, "err": err})


t0 = time.time()
ths = [threading.Thread(target=one, args=(i,)) for i in range(TOTAL)]
for t in ths:
    t.start()
for t in ths:
    t.join()
wall = time.time() - t0

ok = [r for r in results if r["ok"]]
ms = sorted(r["ms"] for r in ok)
print(f"concurrency={CONC} total={TOTAL} wall={wall:.1f}s")
print(f"成功 {len(ok)}/{TOTAL}  RPM = {TOTAL/(wall/60):.1f}（成功计 {len(ok)/(wall/60):.1f}）")
if ms:
    print(f"耗时 P50={statistics.median(ms)/1000:.1f}s  P95={ms[int(len(ms)*0.95)-1]/1000:.1f}s  max={max(ms)/1000:.1f}s")
for r in results:
    if not r["ok"]:
        print(f"  FAIL #{r['i']} {r['ms']}ms {r['err']}")
