#!/usr/bin/env python3
"""browser-bridge 充分测试：覆盖 t2t / agent / 连续请求 / 短答与长答。

用法：.venv/bin/python test_bridge.py [port]
"""
import json
import sys
import time
import urllib.error
import urllib.request

PORT = sys.argv[1] if len(sys.argv) > 1 else "8790"
URL = f"http://127.0.0.1:{PORT}/v1/chat/completions"

CASES = [
    ("t2t-short", "qwen3.7-plus", "只回复两个字：收到"),
    ("agent-qwen4", "qwen4-max-agent", "只回复两个字：收到"),
    ("t2t-math", "qwen3.8-max", "1+1 等于几？只回答数字"),
    ("agent-38max", "qwen3.8-max-agent", "用一句话说明你现在在做什么"),
    ("t2t-repeat", "qwen3.7-plus", "再说一次：收到"),
]


def call(model: str, prompt: str, timeout: int = 300):
    body = json.dumps(
        {"model": model, "messages": [{"role": "user", "content": prompt}], "stream": False}
    ).encode()
    req = urllib.request.Request(URL, data=body, headers={"Content-Type": "application/json"})
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            data = json.loads(r.read() or b"{}")
        ms = int((time.time() - t0) * 1000)
        content = ((data.get("choices") or [{}])[0].get("message") or {}).get("content") or ""
        return {"ok": True, "ms": ms, "len": len(content), "content": content, "err": ""}
    except urllib.error.HTTPError as e:
        return {"ok": False, "ms": int((time.time() - t0) * 1000), "len": 0, "content": "",
                "err": f"HTTP {e.code}: {e.read()[:160]!r}"}
    except Exception as e:  # noqa: BLE001
        return {"ok": False, "ms": int((time.time() - t0) * 1000), "len": 0, "content": "",
                "err": f"{type(e).__name__}: {e}"[:200]}


def main():
    print(f"target {URL}")
    ok = 0
    total = 0
    rows = []
    for name, model, prompt in CASES:
        res = call(model, prompt)
        total += 1
        ok += 1 if res["ok"] and res["len"] > 0 else 0
        rows.append((name, model, res))
        print(f"[{name}] model={model} ok={res['ok']} ms={res['ms']} len={res['len']}")
        print("   content:", res["content"][:120].replace("\n", " | "))
        if res["err"]:
            print("   err:", res["err"])
    print(f"\nRESULT: {ok}/{total} 通过")
    print(json.dumps([{k: r[2][k] for k in ("ok", "ms", "len")} for r in rows], ensure_ascii=False))
    return 0 if ok == total else 1


if __name__ == "__main__":
    sys.exit(main())
