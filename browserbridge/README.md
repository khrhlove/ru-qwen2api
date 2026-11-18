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

## qwen 原生协议兼容层（供 qwen2api 直连）

为了让网关不用改一行 Go 代码就能切到 bridge，实现了两个 qwen 原生端点：

| 端点 | 行为 |
|---|---|
| `POST /api/v2/chats/new` | 返回 `{success, request_id, data:{id:"bb-xxx"}}` |
| `POST /api/v2/chat/completions?chat_id=` | 返回 qwen 风格 SSE：`response.created` → `choices[].delta.{content,phase}` → `status:"finished"` |

对齐的是 qwen2api 的 `internal/upstream/sse.go::wireEvent` 结构（`delta.content` + `phase`）。界面噪音（`已经完成思考`、`跳过`）在推送前就清掉了。

### 验证结果（2026-11-17 23:50）

把 `config.json` 的 `upstream.base_url` 指到 `http://172.30.38.1:8790` 后：

| 测试 | 结果 |
|---|---|
| 网关非流式 | ✅ 9.86s |
| 网关流式 | ✅ 9.73s，`delta.content:"收到"`，qwen2api 正确解析 |

**链路完整打通**：客户端 → qwen2api → bridge（qwen 协议）→ 浏览器 → 回传。

### 🔴 硬阻塞：socks5 代理拦截内网地址

网关日志里的真实报错：

```
err=chats/new:: Post "http://172.30.38.1:8790/api/v2/chats/new":
    socks5: CONNECT 被拒（网络不可达）
```

qwen2api 给每个账号配了 socks5 出口（`upstream.proxy`，账号→固定 IP），**所有出站请求都走代理**，包括指向内网 `172.30.38.1` 的 bridge 请求 —— 代理当然连不上内网。

测试时临时 `proxy.enabled=false` 才打通。但那会丢掉"账号→固定出口"的保护，**不能长期这么跑**。

正确解法是让 qwen2api 支持 **no_proxy**（内网/回环地址直连），在 `internal/upstream/proxy.go` 里加一个判断：

```go
// 私有网段 / 回环 ⇒ 不走代理
func isDirectHost(host string) bool {
    ip := net.ParseIP(host)
    return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}
```

**在此之前，bridge 只能手动切换使用**：改 `base_url` + 临时关 proxy，用完立刻回滚。

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

## 为什么不能像账号池那样"轻量化"（2026-11-18 00:30 实测）

qwen2api 自己的账号池是 **HTTP 直连**：一个账号一个 HTTP 会话，3000+ 账号能做连接池、能做预热。看上去 bridge 也该这样，于是验证了「一个浏览器 + N 个账号 context」的轻量方案（每账号只占一个 context，不做 UI 渲染）。

在干净 IP（node-5，`waf=0`）上实测：

| 方式 | 结果 |
|---|---|
| `ctx.request.post`（浏览器网络栈 + 完整 cookie，无 JS） | **RGV587** `FAIL_SYS_USER_VALIDATE` |
| `page.evaluate(fetch)`（页面内发请求） | 请求挂起不返回 |

**结论：x5sec 卡的是 JS 执行 + 真实渲染环境，不是 cookie。**

所以「一个账号一个轻量 context」这条捷径不成立：只要走 API 调用就被 RGV587 拦，必须真的把页面渲染出来、点发送按钮。

这就是为什么 bridge 的上限是「浏览器渲染能力」（5 个），而不是账号数（3135 个）。两层的差别：

| | qwen2api 账号池 | browser bridge |
|---|---|---|
| 通道 | HTTP 直连 + socks5 出口 | 真实浏览器渲染 |
| 账号规模 | 3000+（轮换） | 5（并发） |
| 单请求成功率 | 取决于风控，需轮换兜底 | 实测 100% |
| 瓶颈 | IP 画像 / RGV587 | 单机渲染 |

**广度与深度的取舍**：账号池用数量换可用性（单个号可能被拦，换下一个），bridge 用真实环境换成功率（但只能开 5 个）。两者是互补关系，不是替代关系。

## Worker 上限：不能无限扩（2026-11-18 00:00 实测）

| WORKERS | 登录 | 压测结果 | 结论 |
|---|---|---|---|
| 3 | 3/3 | RPM **15.9**，9/9 | 稳定 |
| 5 | 5/5 | RPM **18-20**，12/12 | **甜点** |
| 10 | 10/10 全部登录成功 | **6/20 超时 504** | 崩 |

**10 个失败的原因不是账号或 CPU**：

```
[worker2] retry after TimeoutError Locator.click: Timeout 90000ms exceeded.
  - waiting for locator("[aria-label="Select Model"]").first
  - locator resolved to           ← 元素存在，但点不动
```

CPU 只用了 10 核左右（服务器 64 核），内存 22G/47G。是**单机无 GPU 环境下 10 个 Chrome 同时渲染**，Playwright 的可交互性检查等不到元素稳定。

**更严重的后果**：10 个账号同时登录 + 连打 20 个请求之后，`/auth` 直接返回阿里 WAF 挑战页：

```html
<meta name="aliyun_waf_aa" content="...">   ← 整站被挑战
```

之后所有 worker 登录都超时（`wait_for_selector` 等不到邮箱框），bridge 完全不可用。**要冷却一段时间才能恢复。**

### 结论

- **单机上限 5 个**（RPM 18-20），10 个会同时打爆浏览器渲染和 IP 风控
- **账号数不是约束**（3135 个足够），**IP 和单机渲染才是**
- 要更高吞吐只能**加机器**（每台 5 个），不能加 worker
- **不要再对同一 IP 做扩容压力测试** —— 一次 10 并发的代价是整站被 WAF 挑战，恢复要等冷却

## 账号保活体系现状（2026-11-18 10:50）

按需求调整了保活参数，并查清了 refresh 链路的真实状态。

### 已调整的参数

| 项 | 原值 | 新值 | 依据 |
|---|---|---|---|
| `refreshMaintainWindow` | 3 天 | **10 天** | refresh_token TTL 30 天，留 20 天余量滚动 |
| `refreshMaintainBatch` | 2 | **32** | 单轮保养 32 个，20s 一轮 ⇒ 3100 个号约 33 分钟轮一遍 |
| `doRefresh` 超时 | 10s | **25s** | 经 socks5 到 auth.qwen.ai 实测 TTFB 4.4s，10s 必超时 |
| `refreshTokenOnly` 调用方 | 15s | **45s** | 必须比内层 25s 长，否则预算用不上 |
| access 预刷新窗口 | `token_window_s=180` | 不变 | 15 分钟 TTL，提前 3 分钟刷新 |

### 关键修复：refresh 超时导致 refresh_token 永远补不上

改之前日志满屏：

```
续期失败: auths/refresh:: Get "https://auth.qwen.ai/api/v2/auths/refresh": context deadline exceeded
```

refresh 走 socks5 出口到 auth 域，实测 TTFB **4.4s**（本机直连 0.15s），叠加并发争抢后 10s 预算必然超时。结果 `refresh_token` 一个都存不下来（`has_refresh: 0`），账号只能靠密码重登，于是大量 `auth_dead`。

**不能改成直连**：实测直连 `auth.qwen.ai` 返回 `405 + HTML 错误页`（该域对机房 IP 有防护）。

放宽到 25s 后：

| 指标 | 改前 | 改后 |
|---|---|---|
| `has_refresh` | **0** | **3035** |
| `auth_dead` | 100 | 56 |
| `ready` | 3035 | 3079 |

### 仍然未解的问题：refresh_token 用不了（401）

落盘验证通过 —— refresh_token 是 297B 的 JWT，与 access token 不同（`same=False`），格式正确。但用它换 access 时：

```
续期失败: auths/refresh: HTTP 401 Unauthorized
```

近 5 分钟统计：失败 41 条，**成功 0 条**。

**判断**：这批账号的 refresh 链路已在上游侧失效（可能是 refresh_token 一次性，或与当初 signin 的会话绑定）。代码注释里 2026-10-30 记的「refresh_token 可重复使用」，到 11-18 已经不成立了。

⇒ 无法靠代码解决，只能换新号（新 signin 出来的 refresh_token 是新鲜的）。

### 分层刷新设计（代码已实现）

```
请求 → access 有效（>180s）→ 直接用
     → access 快过期        → 用 refresh_token 换新 access（预刷新）
     → refresh 失效（401）  → 密码 signin 重登
     → signin 也被拒       → 标记 auth_dead，切下一个号
```

请求路径上打到失效号时会立刻刷新或切号兜底（`internal/server/chat.go` 的 relogin 分支），不阻塞调用方。

## 吞吐优化：RPM 从 5 提升到 20（2026-11-17 23:40）

压测脚本 `bench_rpm.py`（固定并发跑 N 个请求，统计墙钟/成功率/分位耗时）。

| 配置 | RPM | 成功 | P50 | P95 | max |
|---|---|---|---|---|---|
| WORKERS=3，超时 90s | **5.1** | 9/9 | 10.9s | 19.0s | **105.8s** |
| WORKERS=5，超时 90s | 4.0 | 9/10 | 9.1s | 18.0s | 33.4s（1×504） |
| **WORKERS=3，超时 60s** | **15.9** | 9/9 | 8.8s | 11.4s | 20.5s |
| **WORKERS=5，超时 60s** | **20.3** | 12/12 | 13.1s | 17.6s | 19.5s |
| WORKERS=5，超时 60s（复测） | 18.1 | 12/12 | 9.6s | 22.7s | 30.7s |

**关键结论：卡住 RPM 的不是并发数，而是长尾。**

`CHAT_TIMEOUT=90` 时，一个卡死的请求要占住 worker 90 秒；再叠加一次重试就是 180 秒。3 个 worker 里只要有一个被拖住，整轮吞吐就塌了 —— 表里 max=105.8s 就是这种情况。

把超时压到 **60s** 后，卡住的请求被快速切断并重试，长尾从 105s 收敛到 20-30s，RPM 直接翻了 3-4 倍。

**注意反直觉的一点**：5 worker 在 90s 超时下比 3 worker 还差（4.0 vs 5.1），因为更多 worker 只是让更多请求同时卡住。**先修超时，再加并发**。

线上配置（systemd drop-in）：

```
/etc/systemd/system/qwen-browserbridge.service.d/workers.conf   Environment=WORKERS=5
/etc/systemd/system/qwen-browserbridge.service.d/bind.conf      Environment=BIND_HOST=172.30.38.1
/etc/systemd/system/qwen-browserbridge.service.d/timeout.conf   Environment=CHAT_TIMEOUT=60
```

## 端到端验证（ai-prod，2026-11-17 23:30）

| 测试 | 结果 |
|---|---|
| bridge 非流式（容器内 `wget` → 172.30.38.1:8790） | ✅ `{"content":"收到"}` |
| bridge 流式（宿主机 curl） | ✅ 13.1s / 6 帧 / 正常 `[DONE]` |
| qwen2api 网关非流式（127.0.0.1:39201） | ✅ 3.73s，带真实 usage（prompt 1885 / cached 512） |
| qwen2api 网关流式（qwen2api.1task.cn） | ✅ 5.82s，role → content → stop 完整 |

**结论**：bridge 与网关各自健康、网络连通（容器可直达 bridge）。但两者目前**互相独立**——网关仍走自己的直连通道，没有自动 fallback 到 bridge。

### 接入网关为什么不能只改配置

qwen2api 走的是 **qwen 原生协议**：
1. `POST /api/v2/chats/new` 建会话
2. `POST /api/v2/chat/completions?chat_id=` 打开 SSE（`response.created` / `response.output_text.delta` 等事件）
3. 另有 files 上传、history 回捞

而 bridge 提供的是 **OpenAI 风格 `/v1/chat/completions`**。协议不同，把 `upstream.base_url` 指过来不会工作。

两条可行路径：

| 路径 | 改动 | 风险 |
|---|---|---|
| A. bridge 增加 qwen 原生协议兼容层（`/api/v2/chats/new` + 对应 SSE 事件） | 只改 Python | 低（Go 代码不动），应急时改 `base_url` 即可切换 |
| B. Go 侧在 `handleChat` 的风控分支里加 bridge 调用 | 改生产核心路径 | 高（需改路由与错误处理，测试面大） |

推荐 A：让 bridge 冒充上游，网关零改动即可切换。

## 文件

- `bridge.py` — 服务本体（含 SSE 流式与多 worker）
- `test_bridge.py` — 5 用例验收脚本
- `test_stream.py` — 流式验收（首字/分片/总量）
- `test_concurrent.py` — 并发验收（墙钟 vs 串行）
- `profile/` — 浏览器 profile（含登录态，**不要提交到仓库**）
