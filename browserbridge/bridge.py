#!/usr/bin/env python3
"""browser-bridge：用真实浏览器会话代发 chat.qwen.ai 请求，绕开 x5sec 风控。

为什么需要它：直连（含 socks5 代理）在 chat/completions 上会被阿里 x5sec 拦成
RGV587；把浏览器导出的 cookie 复制给 HTTP 客户端也会撞上 WAF JS 挑战。只有真实
浏览器环境（执行 JS + 携带前端动态风控参数）能通过，因此把"发请求"交给浏览器。

接口（OpenAI 兼容子集）：
  GET  /healthz
  POST /v1/chat/completions   {model, messages:[{role,content}], stream?:bool}

流式：stream=true 时按 SSE 返回增量（首字 2-3s，总时长与上游生成一致）。
并发：WORKERS=N 起 N 个浏览器实例，各自独立 profile，吞吐近似 ×N。

环境变量：
  QWEN_EMAIL / QWEN_PWD   首次启动自动登录用
  BRIDGE_PROFILE          浏览器 profile 目录（持久登录态）
  WORKERS                 并发 worker 数，默认 1
  HEADLESS=1|0            默认 1
  PORT                    默认 8790
  CHAT_TIMEOUT            单次生成等待上限秒数，默认 90
"""
from __future__ import annotations

import json
import os
import queue
import shutil
import socket
import subprocess
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from playwright.sync_api import sync_playwright

BASE = "https://chat.qwen.ai"
HERE = os.path.dirname(os.path.abspath(__file__))
PROFILE = os.environ.get("BRIDGE_PROFILE", os.path.join(HERE, "profile"))
WORKERS = int(os.environ.get("WORKERS", "3"))
HEADLESS = os.environ.get("HEADLESS", "1") == "1"
EMAIL = os.environ.get("QWEN_EMAIL", "")
PWD = os.environ.get("QWEN_PWD", "")


def _parse_accounts(raw: str):
    out = []
    for item in raw.split(","):
        item = item.strip()
        if not item:
            continue
        if ":" in item:
            e, p = item.split(":", 1)
        else:
            # 只给邮箱时，密码沿用 QWEN_PWD
            e, p = item, PWD
        if e.strip() and p:
            out.append((e.strip(), p))
    return out


ACCOUNTS = _parse_accounts(os.environ.get("QWEN_ACCOUNTS", "")) or ([(EMAIL, PWD)] if EMAIL and PWD else [])

# 代理池：每个账号一条固定出口（socks5），与网关侧 upstream.proxy 同源
PROXY_HOST = os.environ.get("BRIDGE_PROXY_HOST", "pool.livetest.cn:2090")
PROXY_PWD = os.environ.get("BRIDGE_PROXY_PWD", "")
ACCOUNTS_FILE = os.path.join(HERE, "accounts.json")

_ACC_CACHE = None


def accounts() -> list:
    """账号清单（name/email/password）。缺文件时退回单账号环境变量。"""
    global _ACC_CACHE
    if _ACC_CACHE is None:
        try:
            with open(ACCOUNTS_FILE) as f:
                _ACC_CACHE = json.load(f)
        except Exception:
            _ACC_CACHE = []
    return _ACC_CACHE


def account_for(idx: int) -> dict:
    accs = accounts()
    if accs:
        return accs[idx % len(accs)]
    return {"name": "main", "email": EMAIL, "password": PWD}


RELAY_BASE = int(os.environ.get("BRIDGE_RELAY_PORT", "11080"))


def relay_bin() -> str:
    cand = os.path.join(os.path.dirname(sys.executable), "pproxy")
    return cand if os.path.exists(cand) else "pproxy"


def start_relay(idx: int, account_name: str):
    """起本地无认证 socks5 中继 → 上游带认证 socks5。

    Chromium 不支持 socks5 用户名/密码认证（Playwright 会直接报
    "Browser does not support socks5 proxy authentication"），
    所以必须在本机把认证剥掉。
    """
    if not (PROXY_PWD and account_name):
        return None, 0
    port = RELAY_BASE + idx
    # 崩溃重试时旧 relay 可能还占着端口，先清干净
    try:
        out = subprocess.run(["lsof", "-ti", f":{port}"], capture_output=True, text=True).stdout
        for pid in out.split():
            subprocess.run(["kill", "-9", pid], capture_output=True)
        time.sleep(0.5)
    except Exception:
        pass
    upstream = f"socks5://{PROXY_HOST}#poolclient-{account_name}:{PROXY_PWD}"
    proc = subprocess.Popen(
        [relay_bin(), "-l", f"http://127.0.0.1:{port}", "-r", upstream],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    # 等端口真正可连（pproxy 冷启动通常 1-3s，代理链路慢时更久）
    ready = False
    for _ in range(30):
        time.sleep(0.5)
        if proc.poll() is not None:
            raise RuntimeError(f"relay exited on :{port}")
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                ready = True
                break
        except Exception:
            continue
    if not ready:
        proc.kill()
        raise RuntimeError(f"relay not ready on :{port}")
    return proc, port
PORT = int(os.environ.get("PORT", "8790"))
BIND_HOST = os.environ.get("BIND_HOST", "127.0.0.1")
CHAT_TIMEOUT = float(os.environ.get("CHAT_TIMEOUT", "90"))

AGENT_MODELS = {
    "qwen4-max-agent": "Qwen4-Max",
    "qwen3.8-max-agent": "Qwen3.8-Max",
    "qwen3.8-flash-agent": "Qwen3.8-Flash",
}
NORMAL_MODELS = {
    "qwen3.7-plus": "Qwen3.7-Plus",
    "qwen4-max": "Qwen4-Max",
    "qwen3.8-max": "Qwen3.8-Max",
    "qwen3.8-omni-flash": "Qwen3.8-Omni-Flash",
    "qwen3.7-max": "Qwen3.7-Max",
    "qwen3.5-omni-plus": "Qwen3.5-Omni-Plus",
}

# 选择器统一集中：前端一改版只需要动这一处。
# 关键：这些元素大多是 div[role=button]，带 `button[...]` 前缀会全部匹配不到。
SEL = {
    "input": ['textarea', '[contenteditable="true"]', 'input[placeholder*="询问"]'],
    "send": ['[aria-label="发送"]', '[aria-label="Send"]', '[class*="send-button"]'],
    "stop": ['[aria-label="停止"]', '[aria-label="Stop"]'],
    "new_chat": ['[aria-label="新建对话"]', '[aria-label="New chat"]'],
    "mode": ['[aria-label="选择模式"]'],
    "model_selector": ['[aria-label="Select Model"]', '[aria-label="模型"]'],
}

# 取助手文本：必须剔除 A/B 对比弹窗（.smrm）与按钮，否则会混入
# 「跳过」「回复 1 / 回复 2」「此反馈将帮助我们…」等噪音。
ASSISTANT_JS = r"""
() => {
  const all = [...document.querySelectorAll('.qwen-chat-message-assistant')]
    .filter(e => !e.closest('[class*="smrm"]'));
  if (!all.length) return '';
  const el = all[all.length - 1].cloneNode(true);
  el.querySelectorAll('button').forEach(b => b.remove());
  return (el.innerText || '').trim();
}
"""

POPUP_JS = """
() => {
  const b = document.body.innerText || '';
  return {
    ab: b.includes('你更喜欢哪个回复') || b.includes('你喜欢这个回复吗'),
    rate: b.includes('此任务成功了吗') || b.includes('这张图片符合预期吗'),
  };
}
"""

_jobs: "queue.Queue[Job]" = queue.Queue()
_ACTIVE = set()  # 已就绪的 worker 编号


class Job:
    def __init__(self, model: str, prompt: str, stream: bool = False):
        self.model = model
        self.prompt = prompt
        self.stream = stream
        self.deltas: "queue.Queue" = queue.Queue()
        self.done = threading.Event()
        self.text = ""
        self.error = ""
        self.ms = 0


def log(*a):
    print(time.strftime("%H:%M:%S"), *a, flush=True)


# ---------------------------------------------------------------- 元素定位
def pick(page, key: str):
    for s in SEL[key]:
        loc = page.locator(s)
        try:
            if loc.count():
                return loc
        except Exception:
            continue
    raise RuntimeError(f"selector not found: {key} -> {SEL[key]}")


def wait_visible(page, key: str, timeout_ms: int = 20000):
    deadline = time.time() + timeout_ms / 1000
    while time.time() < deadline:
        try:
            if pick(page, key).count():
                return True
        except Exception:
            pass
        time.sleep(0.3)
    return False


# ---------------------------------------------------------------- 浏览器侧逻辑
def ensure_login(page, ctx, email: str = "", pwd: str = ""):
    email = email or EMAIL
    pwd = pwd or PWD
    # 直接进 /auth：已登录会跳回聊天页；先访首页再跳会导致密码框不渲染（实测踩过）
    page.goto(BASE + "/auth", wait_until="domcontentloaded", timeout=120000)
    time.sleep(5)
    body = page.inner_text("body") or ""
    if "新建对话" in body:
        return True
    if not (email and pwd):
        log("[login] no credentials")
        return False
    log("[login] start", email.split("@")[0] + "@***")
    try:
        page.wait_for_selector('input[name="email"]', timeout=30000)
    except Exception:
        page.goto(BASE + "/auth", wait_until="domcontentloaded")
        page.wait_for_selector('input[name="email"]', timeout=40000)
    page.fill('input[name="email"]', email)
    page.click('button:has-text("使用密码登录")', force=True)
    time.sleep(4)
    page.wait_for_selector('input[type="password"]', timeout=60000)
    page.fill('input[type="password"]', pwd)
    page.locator('button[type="submit"]').last.click(timeout=15000, force=True)
    ok = False
    for _ in range(50):  # 最多等 25s
        time.sleep(0.5)
        body = page.inner_text("body") or ""
        if "新建对话" in body:
            ok = True
            break
        # 出现验证码/风控提示就提前放弃，避免白等
        if "验证" in body[:400] or "滑动" in body[:400]:
            log("[login] challenge detected")
            break
    log("[login] result", ok, email.split("@")[0] + "@***")
    return ok


def new_chat(page):
    pick(page, "new_chat").first.click(force=True)
    try:
        page.wait_for_selector(SEL["input"][0], state="visible", timeout=8000)
    except Exception:
        pass


def set_agent_mode(page, on: bool):
    """切换 Agent 模式（幂等）。判断依据是输入区是否有「Agent 模式」标签。"""
    is_agent = page.locator('text=Agent 模式').count() > 0
    if is_agent == on:
        return
    pick(page, "mode").first.click(force=True)
    target = "Agent 模式" if on else "自动"
    item = page.locator(f'[role="menuitem"]:has-text("{target}")')
    item.first.wait_for(state="visible", timeout=25000)
    item.first.click(timeout=15000)
    try:
        if on:
            page.locator('text=Agent 模式').first.wait_for(state="visible", timeout=20000)
        else:
            page.wait_for_selector('text=Agent 模式', state="detached", timeout=20000)
    except Exception:
        pass


def pick_model(page, label: str):
    """选模型；菜单里没有该名字时回退到当前模型，而不是报错。

    网关会按自己的模型表往下发（qwen3.7-max / qwen4-max…），网页端的可选项
    并不总是同名。硬报错会让整条请求失败，降级用当前模型更合理。
    """
    pick(page, "model_selector").first.click()
    item = page.locator(f'[role="menuitem"]:has-text("{label}"), [role="option"]:has-text("{label}")')
    try:
        item.first.wait_for(state="visible", timeout=25000)
    except Exception:
        page.keyboard.press("Escape")
        log("[warn] model not in menu, fallback:", label)
        return
    item.first.click(timeout=15000, force=True)
    try:
        page.wait_for_selector('[role="menu"]', state="hidden", timeout=12000)
    except Exception:
        pass


def dismiss_popups(page):
    """关闭 A/B 评价弹窗与任务评价卡。只在检测到文案时动手。"""
    try:
        st = page.evaluate(POPUP_JS)
    except Exception:
        return
    if not (st.get("ab") or st.get("rate")):
        return
    page.keyboard.press("Escape")
    time.sleep(0.8)
    try:
        st = page.evaluate(POPUP_JS)
    except Exception:
        return
    if st.get("ab"):
        for text in ("我更喜欢这个回复", "关闭", "跳过"):
            loc = page.locator(f'text={text}')
            if loc.count():
                try:
                    loc.first.click(timeout=5000)
                    break
                except Exception:
                    continue
    if st.get("rate"):
        loc = page.locator('button:has-text("关闭"), [aria-label="关闭"]')
        if loc.count():
            try:
                loc.first.click(timeout=5000)
            except Exception:
                pass
    time.sleep(0.8)


def read_answer(page) -> str:
    try:
        return page.evaluate(ASSISTANT_JS)
    except Exception:
        return ""


def send_and_wait(page, prompt: str, on_delta=None) -> str:
    """发送并等待完成。on_delta 会在生成过程中持续收到新增文本（流式用）。"""
    box = pick(page, "input").first
    box.click()
    box.fill(prompt)
    time.sleep(0.25)
    send = pick(page, "send")
    if not send.count():
        box.press("Enter")
    else:
        send.last.click()

    prev = ""
    seen_stop = False
    deadline = time.time() + CHAT_TIMEOUT

    def flush(txt: str):
        nonlocal prev
        if on_delta and txt.startswith(prev) and len(txt) > len(prev):
            on_delta(txt[len(prev):])
        # 非前缀增长（重写/截断）时下一轮整体重推
        if not txt.startswith(prev):
            prev = ""
            if on_delta and txt:
                on_delta(txt)
        prev = txt

    while time.time() < deadline:
        time.sleep(0.2)
        txt = read_answer(page)
        flush(txt)
        if page.locator(SEL["stop"][0]).count():
            seen_stop = True
            continue
        if seen_stop:
            time.sleep(0.2)  # 二次确认，避免按钮瞬时抖动
            if page.locator(SEL["stop"][0]).count() == 0:
                dismiss_popups(page)
                final = read_answer(page)
                flush(final)
                return final
        elif txt:
            # 没见过停止按钮：极快回复可能整段落在两次轮询之间
            dismiss_popups(page)
            return txt

    dismiss_popups(page)
    txt = read_answer(page)
    raise TimeoutError(f"generation timeout (text_len={len(txt)})")


def extract_answer(main_text: str, prompt: str) -> str:
    """剥离界面噪音，保留思考过程与答案。"""
    t = main_text or ""
    for cut in ("此任务成功了吗？", "你更喜欢哪个回复", "你喜欢这个回复吗",
                "此反馈将帮助我们评估并提升", "这张图片符合预期吗"):
        if cut in t:
            t = t.split(cut)[0]
    for noise in ("已经完成思考", "思考中", "已深度思考", "已复制", "重新生成",
                  "跳过", "人工智能生成的内容可能不准确。"):
        t = t.replace(noise, "")
    return t.strip()


def ensure_ready(page):
    try:
        if pick(page, "input").count() > 0:
            return
    except Exception:
        pass
    try:
        page.goto(BASE, wait_until="domcontentloaded", timeout=60000)
        time.sleep(4)
    except Exception as e:  # noqa: BLE001
        log("[ensure] reload err", repr(e)[:120])


def do_job(page, job: Job):
    marks = []
    t = time.time()

    def mark(name):
        nonlocal t
        now = time.time()
        marks.append(f"{name}={now - t:.2f}")
        t = now

    new_chat(page)
    mark("new_chat")
    wait_visible(page, "input", 20000)
    mark("wait_input")
    agent = job.model in AGENT_MODELS
    log("[job] start", job.model, "agent=", agent, "stream=", job.stream)
    set_agent_mode(page, agent)
    mark("set_mode")
    label = AGENT_MODELS.get(job.model) or NORMAL_MODELS.get(job.model, "Qwen3.7-Plus")
    pick_model(page, label)
    mark("pick_model")
    log("[job] model:", label)

    on_delta = (lambda d: job.deltas.put(d)) if job.stream else None
    main = send_and_wait(page, job.prompt, on_delta=on_delta)
    mark("generate")
    job.text = extract_answer(main, job.prompt)
    # 空答案视为失败（实测偶发：页面已结束但助手消息节点为空），交给上层重试
    if not job.text:
        raise RuntimeError(f"empty answer (raw_len={len(main)})")
    log("[job] ok", job.model, "len=", len(job.text), "| timing:", " ".join(marks))


def profile_for(idx: int) -> str:
    """每个 worker 独占一个 profile 目录，**不做复制**。

    复制 profile 会让多个 worker 共享同一个 refresh_token；上游 refresh 轮换时
    后刷的那份会把先刷的踢下线，表现为"跑一阵子就有 worker 掉登录"。
    独立目录 + 各自登录一次，才能拿到互不干扰的会话。
    """
    if idx == 0:
        return PROFILE
    return f"{PROFILE}-{idx}"


def worker(idx: int):
    for attempt in range(3):
        try:
            _worker_once(idx)
            return
        except Exception as e:  # noqa: BLE001
            log(f"[worker{idx}] crash({attempt + 1}/3)", type(e).__name__, str(e)[:160])
            time.sleep(5)
    log(f"[worker{idx}] gave up")


def _worker_once(idx: int):
    prof = profile_for(idx)
    acc = account_for(idx)
    email, pwd = acc.get("email", ""), acc.get("password", "")
    time.sleep(idx * 3)  # 轻错峰；真正的隔离靠"不同账号 + 不同出口 IP"
    relay, port = start_relay(idx, acc.get("name", ""))
    proxy = {"server": f"http://127.0.0.1:{port}"} if port else None
    with sync_playwright() as p:
        ctx = p.chromium.launch_persistent_context(
            prof,
            channel="chrome",
            headless=HEADLESS,
            proxy=proxy,
            # 默认 headless UA 带 "HeadlessChrome"，是主动给风控递名片；换成正常 Chrome UA
            user_agent=("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
                        "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36"),
            args=["--disable-blink-features=AutomationControlled"],
            locale="zh-CN",
            viewport={"width": 1280, "height": 800},
        )
        page = ctx.pages[0] if ctx.pages else ctx.new_page()
        page.set_default_timeout(90000)

        # 先确认出口 IP，避免多 worker 落同一条出口而不自知
        egress = "?"
        if proxy:
            try:
                page.goto("https://api.ipify.org", wait_until="domcontentloaded", timeout=60000)
                egress = (page.inner_text("body") or "").strip()[:40]
            except Exception as e:  # noqa: BLE001
                log(f"[worker{idx}] egress probe failed", repr(e)[:100])

        ok = ensure_login(page, ctx, email, pwd)
        if ok:
            _ACTIVE.add(idx)
        log(f"[worker{idx}] ready login={ok} acct={acc.get('name')} egress={egress}")
        while True:
            job = _jobs.get()
            t0 = time.time()
            try:
                ensure_ready(page)
                try:
                    do_job(page, job)
                except Exception as first:  # noqa: BLE001
                    log(f"[worker{idx}] retry after", type(first).__name__, str(first)[:140])
                    ensure_ready(page)
                    do_job(page, job)
            except Exception as e:  # noqa: BLE001
                job.error = f"{type(e).__name__}: {e}"[:400]
            finally:
                job.ms = int((time.time() - t0) * 1000)
                job.done.set()
                job.deltas.put(None)  # 流式端退出信号


# ---------------------------------------------------------------- HTTP 侧
class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _json(self, code: int, obj: dict):
        raw = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *a):
        pass

    def do_GET(self):
        if self.path == "/healthz":
            return self._json(200, {"status": "ok", "workers": WORKERS, "active": len(_ACTIVE)})
        return self._json(404, {"error": "not found"})

    # ---- SSE 辅助
    def _chunk(self, data: bytes):
        self.wfile.write(f"{len(data):X}\r\n".encode() + data + b"\r\n")
        self.wfile.flush()

    def _sse(self, obj: dict):
        self._chunk(b"data: " + json.dumps(obj, ensure_ascii=False).encode() + b"\n\n")

    # ---------- qwen 原生协议兼容层（供 qwen2api 直连，零改 Go 代码）----------
    def _qwen_new_chat(self):
        """POST /api/v2/chats/new → {success, request_id, data:{id}}"""
        n = int(self.headers.get("Content-Length") or 0)
        try:
            self.rfile.read(n)
        except Exception:
            pass
        chat_id = "bb-" + uuid.uuid4().hex[:12]
        self._json(200, {"success": True, "request_id": uuid.uuid4().hex,
                         "data": {"id": chat_id}})

    def _qwen_chat(self):
        """POST /api/v2/chat/completions?chat_id= → qwen 风格 SSE

        qwen2api 的 wireEvent 结构：choices[0].delta.{role,content,phase,status}，
        首帧还看 response.created。phase=answer 是正文，think 是思考。
        """
        n = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(n) or b"{}")
        except Exception:
            return self._json(400, {"success": False, "data": {"code": "BadRequest"}})
        model = body.get("model") or "qwen3.7-plus"
        prompt = ""
        for m in reversed(body.get("messages") or []):
            if m.get("role") == "user":
                c = m.get("content")
                prompt = c if isinstance(c, str) else json.dumps(c, ensure_ascii=False)
                break
        chat_id = "bb-" + uuid.uuid4().hex[:12]
        rid = uuid.uuid4().hex[:16]

        if not prompt:
            return self._json(400, {"success": False, "data": {"code": "EmptyPrompt"}})

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream; charset=utf-8")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()

        def emit(obj):
            self._chunk(b"data: " + json.dumps(obj, ensure_ascii=False).encode() + b"\n\n")

        try:
            emit({"response.created": {"chat_id": chat_id, "response_id": rid, "parent_id": ""},
                  "response_id": rid, "choices": [{"delta": {"role": "assistant"}}]})
            # 走"完成后分片"而不是实时增量：A/B 对比弹窗的内容会随原始流混进来
            # （extract_answer 的清理只作用在最终文本上），实时推会污染正文。
            job = Job(model, prompt, stream=False)
            _jobs.put(job)
            job.done.wait(timeout=CHAT_TIMEOUT + 90)
            text = job.text if not job.error else ""
            for i in range(0, len(text), 24):
                emit({"response_id": rid, "choices": [{"delta": {"content": text[i:i+24], "phase": "answer"}}]})
            if job.error:
                emit({"response_id": rid, "error": {"code": "bridge_error", "message": job.error},
                      "choices": [{"delta": {"status": "finished"}, "finish_reason": "stop"}]})
            else:
                emit({"response_id": rid, "_bridge_ms": job.ms, "usage": {},
                      "choices": [{"delta": {"status": "finished"}, "finish_reason": "stop"}]})
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            try:
                self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
            except Exception:
                pass

    def do_POST(self):
        if self.path.startswith("/api/v2/chats/new"):
            return self._qwen_new_chat()
        if self.path.startswith("/api/v2/chat/completions"):
            return self._qwen_chat()
        if self.path != "/v1/chat/completions":
            return self._json(404, {"error": "not found"})
        n = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(n) or b"{}")
        except Exception:
            return self._json(400, {"error": {"message": "invalid json"}})
        model = body.get("model") or "qwen3.7-plus"
        stream = bool(body.get("stream"))
        msgs = body.get("messages") or []
        prompt = ""
        for m in reversed(msgs):
            if m.get("role") == "user":
                c = m.get("content")
                prompt = c if isinstance(c, str) else json.dumps(c, ensure_ascii=False)
                break
        if not prompt:
            return self._json(400, {"error": {"message": "no user message"}})

        job = Job(model, prompt, stream=stream)
        _jobs.put(job)
        rid = "chatcmpl-" + uuid.uuid4().hex[:12]
        created = int(time.time())

        # ---------------- 流式
        if stream:
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream; charset=utf-8")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            base = {"id": rid, "object": "chat.completion.chunk", "created": created, "model": model}
            try:
                self._sse({**base, "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}]})
                while True:
                    d = job.deltas.get()
                    if d is None:
                        break
                    self._sse({**base, "choices": [{"index": 0, "delta": {"content": d}, "finish_reason": None}]})
                if job.error:
                    self._sse({**base, "choices": [{"index": 0, "delta": {"content": f"[bridge error] {job.error}"}, "finish_reason": "stop"}]})
                else:
                    self._sse({**base, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "_bridge_ms": job.ms})
                self._chunk(b"data: [DONE]\n\n")
            except (BrokenPipeError, ConnectionResetError):
                pass
            finally:
                try:
                    self.wfile.write(b"0\r\n\r\n")
                    self.wfile.flush()
                except Exception:
                    pass
            return

        # ---------------- 非流式
        job.done.wait(timeout=CHAT_TIMEOUT + 60)
        if job.error:
            return self._json(502, {"error": {"message": job.error, "type": "bridge_error"}})
        if not job.done.is_set():
            return self._json(504, {"error": {"message": "timeout", "type": "timeout"}})
        return self._json(
            200,
            {
                "id": rid,
                "object": "chat.completion",
                "created": created,
                "model": model,
                "choices": [{"index": 0, "message": {"role": "assistant", "content": job.text}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
                "_bridge_ms": job.ms,
            },
        )


def main():
    for i in range(WORKERS):
        threading.Thread(target=worker, args=(i,), daemon=True).start()
    log(f"[http] listening {BIND_HOST}:{PORT} workers={WORKERS}")
    ThreadingHTTPServer((BIND_HOST, PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
