#!/usr/bin/env python3
"""吞吐压测：固定并发跑 N 个请求，算实际 RPM。

用法: python bench_rpm.py <port> <并发> <总请求数>
"""
import json
import statistics
import sys
import threading
import time
import urllib.request

PORT = sys.argv[1] if len(sys.argv) > 1 else "8790"
CONC = int(sys.argv[2]) if len(sys.argv) > 2 else 3
TOTAL = int(sys.argv[3]) if len(sys.argv) > 3 else 12
HOST = sys.argv[4] if len(sys.argv) > 4 else "172.30.38.1"
URL = f"http://{HOST}:{PORT}/v1/chat/completions"

results = []
lock = threading.Lock()
sem = threading.Semaphore(CONC)


def one(i):
    with sem:
        body = json.dumps({"model": "qwen3.7-plus",
                           "messages": [{"role": "user", "content": f"第{i}个：只回复两个字：收到"}]}).encode()
        req = urllib.request.Request(URL, data=body, headers={"Content-Type": "application/json"})
        t0 = time.time()
        ok, txt, err = False, "", ""
        try:
            with urllib.request.urlopen(req, timeout=180) as r:
                d = json.loads(r.read() or b"{}")
            c = ((d.get("choices") or [{}])[0].get("message") or {}).get("content", "")
            ok = bool(c.strip())
            txt = c[:20]
            if not ok:
                err = "empty"
        except Exception as e:
            err = str(e)[:60]
        with lock:
            results.append({"i": i, "ms": int((time.time() - t0) * 1000), "ok": ok, "txt": txt, "err": err})


t_start = time.time()
ths = [threading.Thread(target=one, args=(i,)) for i in range(TOTAL)]
for t in ths:
    t.start()
for t in ths:
    t.join()
wall = time.time() - t_start

ok = [r for r in results if r["ok"]]
ms = [r["ms"] for r in ok]
ms_all = [r["ms"] for r in results]
print(f"concurrency={CONC} total={TOTAL} wall={wall:.1f}s")
print(f"成功 {len(ok)}/{TOTAL}  RPM = {TOTAL / (wall / 60):.1f}  (成功计 {len(ok) / (wall / 60):.1f})")
if ms:
    ms.sort()
    print(f"耗时 P50={statistics.median(ms)/1000:.1f}s  P95={ms[int(len(ms)*0.95)-1]/1000:.1f}s  max={max(ms)/1000:.1f}s")
for r in results:
    if not r["ok"]:
        print(f"  FAIL #{r['i']} {r['ms']}ms {r['err']}")
