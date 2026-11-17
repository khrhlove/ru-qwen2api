# qwen2api-browserbridge

用**真实浏览器会话**代发 `chat.qwen.ai` 请求，绕过 x5sec 风控。为 qwen2api 网关提供一条可用的兜底通道。

## 为什么需要它

2026-11-17 实测结论（证据见下文）：

| 探测 | 结果 |
|---|---|
| `GET /api/v2/models/`（4 个不同出口） | 全部 200，内容完整 |
| `POST /api/v2/chat/completions`（同 token、完整头） | `FAIL_SYS_USER_VALIDATE` + `_____tmd_____/punish?x5secdata=…` |
| 换 `version` 头 0.3.26 / 0.3.45 / 0.3.50 | 三次结果完全一致 |
| 把浏览器导出的 cookie 复制到 requests 直连 | 仍返回阿里 WAF JS 挑战页 |
| 浏览器 UI 里点发送 | 正常返回 |

⇒ IP 没被封、账号没被封、版本号不是原因。上游要求**通过人机验证并携带前端动态风控参数**，只有真实浏览器环境能过。纯 HTTP 客户端（含 socks5 代理）都过不了。

## 架构

```
网关/客户端 --HTTP--> bridge.py (8790) --队列--> 单个 Playwright worker
                                                    |
                                              Chrome + 持久 profile
                                                    |
                                              chat.qwen.ai 页面 UI
```

- **单 worker 串行**：Playwright sync API 非线程安全，所有请求进 `queue.Queue` 排队，HTTP 侧等待结果。
- **持久 profile**：登录态存在 `./profile`，重启不用重新登录；首次启动可用 `QWEN_EMAIL`/`QWEN_PWD` 自动走 UI 登录。
- **自愈**：每单开始检查页面可用性，异常则整页重载；单次失败自动重试一遍。
- **回复提取**：只取 `.qwen-chat-message-assistant` 最后一个节点，避开评价弹窗。

## 用法

```bash
# 依赖（uv 管理）
uv venv --python 3.13 && uv pip install playwright

# 启动（首次会用账号密码登录；之后复用 profile）
QWEN_EMAIL=you@example.com QWEN_PWD=yourpass HEADLESS=1 PORT=8790 \
  .venv/bin/python bridge.py

# 调用
curl -s http://127.0.0.1:8790/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen4-max-agent","messages":[{"role":"user","content":"只回复两个字：收到"}]}'
```

响应为 OpenAI 兼容结构，额外带 `_bridge_ms`（本次耗时）。

支持的模型：

- Agent：`qwen4-max-agent` / `qwen3.8-max-agent` / `qwen3.8-flash-agent`（自动切「Agent 模式」并选模型）
- 普通：`qwen3.7-plus` / `qwen4-max` / `qwen3.8-max` / `qwen3.8-omni-flash` / `qwen3.7-max` / `qwen3.5-omni-plus`

## 流式与并发

### 流式（SSE）

```bash
curl -N http://127.0.0.1:8790/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3.7-plus","messages":[{"role":"user","content":"用三句话介绍杭州"}],"stream":true}'
```

- 首 chunk 带 `role`，随后按增量推 `content`，结束给 `finish_reason=stop` + `data: [DONE]`
- 实测：首个 chunk **7.3s**（上游开始吐字的时间），全文 17.4s。相比等全文，感知延迟降一半以上
- 流式推的是**上游原始流**（含思考过程与「已经完成思考」标记）——SSE 增量只能追加不能撤回，如果中途清理会让已推内容与实际错位；需要纯答案就用非流式

### 多账号并发（可用，3 个是上限）

```bash
# accounts.json: [{"name":"main","email":"...","password":"..."}, ...]
WORKERS=3 .venv/bin/python bridge.py      # 默认就是 3
```

每个 worker 用**不同账号** + 独立 profile 目录 + 直连。关键：**不同账号**才可行，同一账号多开必被风控（实测 3 次独立登录后所有请求无响应）。

实测（2026-11-17 22:14，3 账号）：

| 并发 | 轮次 | 单请求耗时 | 墙钟 | 成功率 |
|---|---|---|---|---|
| 3 | 冷启动后首轮 | 7.57 / 7.39 / 7.97s | **7.97s** | 3/3 |
| 3 | 紧接连压 | 9.29 / 18.82 / 24.95s | 24.95s | 3/3 |
| 5 | 首测 | 18.9 / 12.2 / 29.3s + 2×502 | 29.3s | 3/5 |
| 5 | 放宽等待后 | 10.8 / 6.6 / 21.2 / 21.5s + 1×150s超时 | — | 4/5 |

**结论**：
- 首轮接近完美线性（3 个请求 8 秒完成，串行需 23 秒）
- 连压会让上游排队、提速归零 ⇒ 生产上保持 **5–10s 请求间隔**
- **5 个账号能同时登录**（早先"3 个上限"是 `ensure_login` 的路径 bug：先访首页再跳 `/auth` 会导致密码框不渲染，被误判成风控）
- 但 5 并发成功率降到 3–4/5（菜单渲染超时、单请求 150s 卡死），**稳定性不如 3**
- 吞吐：3 并发约 15–22 次/分钟（单 worker 为 4–7 次/分钟）
- **账号数的价值在轮换与抗封，不在并发**：单 IP 稳定并发 3，要提吞吐只能加机器

### 代理池（实测不可用）

| 出口 | TTFB | 可用于浏览器 |
|---|---|---|
| 2086 HTTP（每请求换 IP） | 1.45–4.73s | 否 |
| 2088 socks5（每请求换 IP） | 2.59–3.81s | 否 |
| 2089 socks5（域名哈希） | 2.55–3.61s | 否 |
| 11088 回国出口 | 0.50–0.96s | 否（仅 HTTP/1.1，浏览器加载 94s） |

**Chromium 不支持 socks5 用户名密码认证**（`Browser does not support socks5 proxy authentication`），必须用 pproxy 在本机剥认证。代码保留（`start_relay` + `BRIDGE_PROXY_HOST/PWD`），换成支持 HTTP/2 的低延迟代理后可直接启用。

### 预热

同 context 下多标签预热几乎零成本：3 个 tab 依次 `goto` 合计 **0.6s**（冷启动单标签 5–10s）。bridge 的持久 profile 已经吃掉了这部分收益。


## 实测数据（2026-11-17 19:35，四模型验收）


| 模型 | 总耗时 | new_chat | set_mode | pick_model | generate |
|---|---|---|---|---|---|
| qwen3.7-plus | **7.7s** | 0.12 | 0.00 | 0.43 | 7.16 |
| qwen4-max-agent | 13.3s | 0.18 | 0.11 | 0.27 | 12.75 |
| qwen4-max-agent | 15.0s | 0.13 | 0.15 | 0.31 | 14.43 |
| qwen3.8-max | 16.9s | 0.12 | 0.00 | 0.43 | 16.37 |

**本地固定开销合计 0.55–0.59s，占总耗时 3%–7%；其余 93%–97% 是上游推理。**

优化前后对比：

| 阶段 | 优化前 | 优化后 | 手段 |
|---|---|---|---|
| new_chat | 1.33s | 0.12–0.18s | `wait_for_selector` 替代 `sleep(1.2)` |
| set_mode | 0.51s（普通）/ 2.81s（Agent） | 0.00 / 0.11–0.15s | 菜单项 `wait_for` + 模式标签出现即返回 |
| pick_model | 2.51s | 0.27–0.43s | 等菜单项可见即点，点完等 `[role=menu]` 收起 |
| **固定开销合计** | **4.35s** | **0.57s** | 降 87% |

日志里每次请求都会打一行 `timing: new_chat=… set_mode=… pick_model=… generate=…`，便于定位回归。

## 三个稳定性要点（踩过的坑，改版必看）

1. **元素不是 `button`**：`[aria-label="发送"]`、`[aria-label="选择模式"]`、`[aria-label="Select Model"]` 实际是 `div[role=button]`，加 `button[...]` 前缀会全部落空。现在统一走 `SEL` 常量 + 多候选 `pick()`，一处修改全局生效。
2. **完成判定用按钮状态，不用文本长度**：生成中按钮是 `[aria-label="停止"]`，结束变回 `[aria-label="发送"]`。早期用「文本长度稳定」判定，导致短回复（如「收到」）每次卡满超时。**超时必须抛错走重试**——曾出现 90s 超时却把 7 字符中间态当结果返回的 bug。
3. **A/B 弹窗用文案检测**：`[class*="feedback"]` 在正常页面也恒为 true，不可靠。现在用 `document.body.innerText` 匹配「你更喜欢哪个回复」/「此任务成功了吗」，命中才 Esc 或点选项，避免每轮全页取文本。



## 限制（重要）

1. **吞吐极低**：单浏览器串行，实测 7.7-16.9s/请求（约 3.5-7 次/分钟）。本地开销已压到 0.6s，剩余全是上游推理时间；要提吞吐只能加 worker（每 worker 需独立账号+profile）或做 SSE 流式改善首字延迟。
2. **依赖 UI 选择器**：选择器集中在 `bridge.py` 的 `SEL` 常量里，前端改版只需改那一处；`.qwen-chat-message-assistant` 若改名也要同步。
3. **思考过程混在正文里**：Agent 模式返回包含模型思考（英文），如需纯答案要在上层再切分。
4. **只支持非流式**：`stream=true` 未实现，调用方需按非流式处理。
5. **每次都是新对话**：不做上下文续接，多轮 messages 只取最后一条 user。
6. **登录态会过期**：`profile/` 失效后需重启并带账号密码重新登录。

## 部署（ai-prod / 210.121.44.245）

```bash
# 1) 依赖
apt-get install -y python3-venv python3-pip
python3 -m venv /opt/browserbridge/.venv
/opt/browserbridge/.venv/bin/pip install playwright requests
/opt/browserbridge/.venv/bin/playwright install --only-shell chromium
/opt/browserbridge/.venv/bin/playwright install-deps chromium

# 2) 代码（bridge.py + accounts.json）
scp bridge.py accounts.json root@<host>:/opt/browserbridge/

# 3) systemd
cat > /etc/systemd/system/qwen-browserbridge.service <<'EOF'
[Unit]
Description=Qwen Browser Bridge
After=network.target
[Service]
WorkingDirectory=/opt/browserbridge
Environment=WORKERS=3
Environment=HEADLESS=1
Environment=PORT=8790
Environment=HOME=/root
Environment=BIND_HOST=172.30.38.1
ExecStart=/opt/browserbridge/.venv/bin/python /opt/browserbridge/bridge.py
Restart=always
RestartSec=8
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now qwen-browserbridge
```

**容器直连要点**：qwen2api 容器在 `qwen2api_default`（172.30.38.0/24，网关 172.30.38.1）。把 `BIND_HOST` 设成该网关 IP，容器里直接访问 `http://172.30.38.1:8790/v1/...`，不经过 nginx、不暴露公网。

宿主机开了 ufw（INPUT 默认 DROP），必须放行容器网段，否则容器连不通：

```bash
ufw allow from 172.30.38.0/24 to any port 8790 proto tcp
```

实测（容器内 `wget --post-file`）：返回 `{"content":"收到"}`，端到端打通。

## 文件

- `bridge.py` — 服务本体（含 SSE 流式与多 worker）
- `test_bridge.py` — 5 用例验收脚本
- `test_stream.py` — 流式验收（首字/分片/总量）
- `test_concurrent.py` — 并发验收（墙钟 vs 串行）
- `profile/` — 浏览器 profile（含登录态，**不要提交到仓库**）
