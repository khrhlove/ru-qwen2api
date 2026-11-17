# x5sec 协议逆向笔记

抓取时间：2026-11-17 21:30–22:20
目标：`chat.qwen.ai` 的 `POST /api/v2/chat/completions` 风控

---

## 1. 双层结构

```
请求 → [WAF 层] → [x5sec 层] → 业务
        阿里 WAF      阿里 sufei-punish
        (acw_sc__v2)  (captcha)
```

| 层 | 触发条件 | 表现 | 能否纯 HTTP 通过 |
|---|---|---|---|
| WAF | 缺 UA / IP 信誉低 | `200 + aliyun_waf_aa` HTML（16KB） | 补齐请求头可以 |
| x5sec | 通过 WAF 后仍判定高风险 | `FAIL_SYS_USER_VALIDATE` + `punish?x5secdata=` | **不能**（需 captcha） |

补齐的请求头集合（实测能穿过 WAF 进入 x5sec 层）：

```
User-Agent: <真实 Chrome UA>
Accept: application/json, text/plain, */*
Content-Type: application/json
Accept-Language: zh-CN,zh;q=0.9
Origin / Referer: https://chat.qwen.ai
source: web          version: 0.3.45        bx-v: 2.5.37
bx-ua / bx-umidtoken: defaultFY2_load_failed with timeout@@https://chat.qwen.ai@@<ms>
Sec-Fetch-Dest/Mode/Site, sec-ch-ua, sec-ch-ua-mobile, sec-ch-ua-platform
```

---

## 2. 接口链路

```
1) POST /api/v2/chat/completions
   → {"ret":["FAIL_SYS_USER_VALIDATE","RGV587_ERROR::SM::…"],
      "data":{"url":"…/_____tmd_____/punish?x5secdata=…&x5step=2&action=captcha&pureCaptcha="}}

2) GET  …/_____tmd_____/punish?x5secdata=…
   → HTML（注入 requestInfo，加载 sufei-punish 全套 SDK）

3) GET  …/_____tmd_____/punishTextFetch?x5secdata=…
   → 全部 UI 文案（泄露验证类型）

4) GET  …/_____tmd_____/gridConnectGet?token=<32hex>&appKey=X82Y__0e22225ae5a53732c71d6fe2
   → {"data":{"encryptToken":"<长 hex>"}}    ← 加密的挑战数据

5) GET  …/_____tmd_____/report?x5secdata=…
   → {"code":0,"dt":"success","ec":200,"result":{"code":0,"sig":"from bx"}}
```

`appKey` 常量：`X82Y__0e22225ae5a53732c71d6fe2`

---

## 3. SDK 资源清单

| 文件 | 大小 | 作用 |
|---|---|---|
| `g.alicdn.com/sd/punish/0.0.4/program.wasm` | 21KB（gzip 后解压 69KB） | 小工具（hash/随机） |
| `g.alicdn.com/secdev/sec-captcha/0.0.20/program.wasm` | 30KB | **libwebp**（图片编解码），非加密 |
| `g.alicdn.com/bsop-static/sufei-punish/0.1.130/build/punishpage.min.js` | 128KB | 惩罚页调度 |
| `g.alicdn.com/bsop-static/scratch-captcha/0.0.59/index.js` | **908KB** | **核心：验证码 + 加密** |
| `o.alicdn.com/secdev/storage-br/secaptcha/362f987b88325959.js` | 32KB | 补充库（无加密关键字） |

**结论：WASM 不是加密引擎**（一个是工具函数，一个是 libwebp），真正逻辑在 `scratch-captcha/index.js`。

---

## 4. 加密分析（关键成果）

`scratch-captcha/index.js` 里的实现是**标准算法**，没有自研加密：

```js
// 路径 A：Web Crypto（浏览器支持时优先）
crypto.subtle.digest("SHA-256", data).then(buf => new Uint8Array(buf).subarray(0, 16))   // key = SHA256(x)[:16]
crypto.subtle.importKey("raw", key, {name:"AES-CBC"}, false, ["decrypt"])
crypto.subtle.decrypt({name:"AES-CBC", iv}, key, cipher)

// 路径 B：纯 JS 兜底
function Ac(c, a, n) {  // c=密文, a=key(16B), n=iv(16B)
  if (c.length % 16) throw Error("aes128CbcDecrypt: cipher length must be a multiple of 16");
  if (a.length !== 16) throw Error("aes128CbcDecrypt: key length must be 16");
  if (n.length !== 16) throw Error("aes128CbcDecrypt: iv length must be 16");
  ...  // 完整 AES-128 密钥扩展 + 解密轮函数
}
```

### 运行时 hook 抓到的硬编码密钥

```json
{"op": "importKey", "fmt": "raw", "algo": "AES-CBC", "usage": "decrypt",
 "key": "326c33794d415374614f656749375450"}
```

| 项 | 值 |
|---|---|
| hex | `326c33794d415374614f656749375450` |
| ASCII | **`2l3yMAStaOegI7TP`** |
| 长度 | 16 字节（AES-128） |
| 稳定性 | 两次独立实验取值相同 ⇒ 硬编码常量 |

### 设备指纹

`na()` 函数采集 WebGL 指纹：`UNMASKED_VENDOR_WEBGL` + `UNMASKED_RENDERER_WEBGL`，用于派生另一条 key 路径。

---

## 5. 七种验证类型

从 `punishTextFetch` 返回的 CSS 类名与文案提取：

| 类型 | CSS 类 | 交互 |
|---|---|---|
| 拼图 | `captchacappuzzle` | 拖动拼图块到缺口 |
| 滑块 v2 | `captchacapslidev2` | 横向拖动 |
| 场景 | `captchascene` | 场景内点选 |
| 点选 | `captchaclick` | 按描述点选图片 |
| 连线 | `captchaconnect` | 拖拽连线 |
| 拖拽 | `captchadrag` | 把满足描述的图片拖到指定区域 |
| 信息购买 | `captchainfopurchase` | 特殊场景 |

服务端按风险等级下发其中一种，前端用 `slideStyle:{width:300,height:40}`、`immediate:true` 初始化。

---

## 6. 未完成项（如实记录）

| 项 | 状态 | 阻塞原因 |
|---|---|---|
| `encryptToken` 完整值 | 只截到 700 字符 | `gridConnectGet` 只在部分场景被调用，重跑时未复现 |
| AES `iv` | 未抓到 | 未捕获到 `subtle.decrypt` 调用（`importKey` 之后 SDK 未继续） |
| 提交验证结果的报文格式 | 未获取 | 同上 |
| 七种验证码的识别与轨迹 | 未实现 | 依赖上面两项 |

**实验环境的核心矛盾**：能触发验证的 HTTP 客户端过不了 WAF；能过 WAF 的浏览器 profile 又被判为低风险，根本不触发验证。缺少「既能触发、又能通过」的可控实验对。

---

## 7. 复现方法

```bash
# 1) 抓惩罚页 HTML
curl -s -o waf.html 'https://chat.qwen.ai/' \
  -H 'User-Agent: <真实UA>' ...

# 2) 分析 WASM
python3 wasm_analyze.py     # 解析 section / import / export / strings

# 3) 运行时 hook 抓 key
python3 hook_crypto.py      # 注入 add_init_script，劫持 crypto.subtle.*

# 4) 用 key 尝试解密
from Crypto.Cipher import AES
key = bytes.fromhex("326c33794d415374614f656749375450")
```

产物：`waf.html`、`captcha_trace.json`、`crypto2.json`、`etoken.txt`（均在 `/tmp/x5sec-poc/`）
