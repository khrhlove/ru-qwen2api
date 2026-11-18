package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// SignIn 账号密码登录，返回**带形态前缀的凭据**（`bearer:<access_token>`）。
//
// 🔴 实测（2026-10-28）三条硬事实：
//  1. 端点仍在**业务域** `chat.qwen.ai/api/v2/auths/signin`；
//     换到 `auth.qwen.ai` 会得到 `Bad_Request: Missing origin`。
//  2. 密码字段是 **sha256hex(明文)**，不是明文。
//  3. 签发的 token **只在 `Set-Cookie`** 里（body 是账号资料，没有 token），
//     且它是 **cookie 形态** —— 当 Bearer 发会 401。这就是本仓要区分形态的原因。
//
// ⚠️ signin 有 IP 级频率墙（同出口连登多号会吃 aliyun_waf 挑战页），
// 所以只在"凭据失效且配了密码"时才调用，不要拿它当轮询。
func (c *Client) SignIn(ctx context.Context, email, password string) (string, error) {
	access, _, err := c.SignInSession(ctx, email, password)
	return access, err
}

// SignInSession 登录并返回**两份**凭据：
//
//	access  = bearer:<jwt>  业务请求用，实测 TTL 只有 900 秒
//	session = cookie:<jwt>  signin 签发的会话，用于 /auths/refresh 换新 access
//
// 🔴 为什么要留 session：access 会过期，而 refresh 只需要这张 refresh_token。
// 以前把它丢掉，等于每次过期都要用密码**从头登录**一次 —— 对 3000 个账号
// 既慢又是风控暴露面（每次 signin 都可能吃 WAF 挑战页）。
//
// 🔴 2026-11-18 上游改版（线上实测，之前几百个号"密码正确却登不进去"的真因）：
//   - `{chat}/api/v2/auths/signin` **只下发会话 cookie**，而且这个 cookie 已经
//     换不出 access_token —— `/auths/refresh` 恒返回 `401 未授权`。
//     继续用它做密码重登 = 100% 失败，还会被误判成"账号需要人工验证"。
//   - `{auth}/api/v2/auths/signin` 直接返回 `data.access_token`（281 字符，
//     可写端点）**并**在 Set-Cookie 里下发 `refresh_token`（297 字符），一步到位。
//
// 所以先打 auth 域；只有拿不到 access_token 时才回落到旧流程。
func (c *Client) SignInSession(ctx context.Context, email, password string) (access, session string, err error) {
	sum := sha256.Sum256([]byte(password))
	body, err := json.Marshal(map[string]string{
		"email":    email,
		"password": hex.EncodeToString(sum[:]),
	})
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	authBase := strings.TrimRight(c.cfg.AuthBaseURL, "/")
	if authBase == "" {
		authBase = c.base
	}
	access, session, err = c.signInAt(ctx, authBase, body)
	if err == nil && access != "" {
		return access, session, nil
	}
	// 认证类错误（密码错、账号被限）是确定的：别再拿同一份密码去打一次 chat 域，
	// 那只会把 signin 频率翻倍。
	if err != nil && KindOf(err) == KindAuth {
		return "", "", err
	}
	// 回落：旧流程（chat 域 signin + 用 token cookie 换票）。
	return c.signInAt(ctx, c.base, body)
}

// signInAt 向指定域发一次登录，返回带形态前缀的 access 与 refresh_token。
//
// 新流程（auth 域）两样都在响应里；旧流程（chat 域）只有 token cookie，
// 这时按老办法用它换一次 access_token。
func (c *Client) signInAt(ctx context.Context, host string, body []byte) (access, session string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		host+"/api/v2/auths/signin", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	// 跨域（auth）时要带 x-request-origin 且 sec-fetch-site=same-site，
	// 否则上游回 `Bad_Request: Missing origin`。
	req.Header = c.buildHeadersFor("", c.base+"/", strings.TrimRight(host, "/") != c.base)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", transportErr("signin", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if at := signinAccessToken(raw); at != "" {
		for _, ck := range resp.Cookies() {
			if ck.Name == "refresh_token" && ck.Value != "" {
				session = ck.Value
			}
		}
		return ModeBearer + ":" + at, session, nil
	}

	// 旧流程：token 在 Set-Cookie，body 只有账号资料。
	var token string
	for _, ck := range resp.Cookies() {
		if ck.Name == "token" && ck.Value != "" {
			token = ck.Value
			break
		}
	}
	if token == "" {
		if _, err := decodeEnvelope(resp, raw, "signin"); err != nil {
			return "", "", err
		}
		return "", "", NewErr(KindAuth, "signin", resp.StatusCode, "",
			"登录成功但响应里没有 token cookie（上游形态可能又变了）", true)
	}

	// 🔴 第二段不能省：signin 签发的 cookie token **只能读、不能写** ——
	// 用它打 /api/v2/chats/new 会得到 Unauthorized（2026-10-28 实测），
	// 而 /api/v2/models/ 却正常。写端点要的是 refresh 换来的 access_token。
	at, refreshToken, err := c.doRefresh(ctx, "token="+token)
	if err != nil {
		return "", "", err
	}
	return ModeBearer + ":" + at, refreshToken, nil
}

// signinAccessToken 从登录响应体里取新版下发的 access_token。
//
// 只认 `data.access_token`：`data.token` 是会话 cookie 的 JWT（只能读、不能写，
// 2026-11-18 实测当 Bearer 发依然 401），别拿它冒充 access token。
func signinAccessToken(raw []byte) string {
	var env struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ""
	}
	return strings.TrimSpace(env.Data.AccessToken)
}

// Refresh 用 cookie 凭据换 access_token（Bearer 形态）。
//
// 端点 `GET {auth}/api/v2/auths/refresh`：跨域调用，必须带 x-request-origin
// 且 sec-fetch-site=same-site（见 buildHeadersFor）。
func (c *Client) Refresh(ctx context.Context, cred string) (string, error) {
	mode, value := SplitCredential(cred)
	cookie := value
	if mode == ModeCookie && !IsCookieJar(value) {
		cookie = "token=" + value
	}
	access, _, err := c.doRefresh(ctx, cookie)
	return access, err
}

// RefreshWithToken 用 **refresh_token** 换新的 access token。
//
// 🔴 这是"账号保活"的正确姿势（2026-10-30 实测）：
//   - signin 签发的 access token TTL 只有 **900 秒**；
//   - 第一次 refresh 会在 Set-Cookie 里下发 `refresh_token`（约 297B 的 JWT）；
//   - 这个 refresh_token **可以重复使用**（连续 3 次都换到了新 access，不轮换）；
//   - 而 signin 的 token cookie 是一次性的（第二次 refresh 就报"令牌已撤销"）。
//
// 所以只要在登录时把 refresh_token 存下来，之后就不再需要密码 ——
// 既省掉每次过期都重登的开销，也避开 signin 那条最容易吃 WAF 的路径。
func (c *Client) RefreshWithToken(ctx context.Context, refreshToken string) (access, next string, err error) {
	rt := strings.TrimSpace(refreshToken)
	if i := strings.Index(rt, ":"); i > 0 && !strings.Contains(rt, ".") {
		rt = rt[i+1:]
	}
	if rt == "" {
		return "", "", NewErr(KindAuth, "auths/refresh", 0, "", "refresh_token 为空", false)
	}
	return c.doRefresh(ctx, "refresh_token="+rt)
}

// doRefresh 发一次 refresh 请求。cookieHeader 是完整的 Cookie 头内容。
// 返回值 next 是新下发的 refresh_token（首轮才有，后续通常为空）。
func (c *Client) doRefresh(ctx context.Context, cookieHeader string) (access, next string, err error) {
	// 🔴 refresh_token 绑定签发时的出口 IP：同一个 token 直连会返回
	// 「令牌已撤销，请重新登录」，而走该账号的 socks5 出口就是 200。
	// 2026-11-18 实测：a7 的 rt 走 poolclient-a7 → 200/access 281B；
	// 同一个 rt 从服务器直连 → 401 令牌已撤销。
	//
	// 所以代理开启时必须带账号上下文，宁可在这里明确失败，也不要静默直连
	// 把一批好好的 refresh_token 全打成「已撤销」。
	if c.cfg.Proxy.Enabled && AccountOf(ctx) == "" {
		return "", "", NewErr(KindAuth, "auths/refresh", 0, "",
			"refresh 缺少账号上下文（refresh_token 绑定出口 IP，直连会被判令牌已撤销）", false)
	}
	base := strings.TrimRight(c.cfg.AuthBaseURL, "/")
	if base == "" {
		base = c.base
	}
	// 超时预算 25s：经 socks5 出口到 auth 域实测 TTFB 4.4s（本机直连 0.15s，
	// 但该域对机房 IP 直连返回 405，只能走代理），再叠加并发争抢，10s 必然超时。
	// 2026-11-18 线上实测：原 10s 下 refresh 几乎全失败（满屏 context deadline
	// exceeded），refresh_token 因此永远是空的，账号只能靠密码重登。
	//
	// 🔴 调用方的 ctx 必须比它长，否则内层预算用不上（见 refreshTokenOnly 的 45s）。
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v2/auths/refresh", nil)
	if err != nil {
		return "", "", err
	}
	req.Header = c.buildHeadersFor("", c.base+"/", true)
	req.Header.Set("Cookie", cookieHeader)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", transportErr("auths/refresh", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", transportErr("auths/refresh 读响应", err)
	}
	payload, err := decodeEnvelope(resp, raw, "auths/refresh")
	if err != nil {
		return "", "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return "", "", NewErr(KindUnknown, "auths/refresh", resp.StatusCode, "", "响应不是预期 JSON", false)
	}
	if strings.TrimSpace(out.AccessToken) == "" {
		return "", "", NewErr(KindAuth, "auths/refresh", resp.StatusCode, "",
			"未返回 access_token（refresh_token 可能已失效）", true)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "refresh_token" && ck.Value != "" {
			next = ck.Value
		}
	}
	return out.AccessToken, next, nil
}

// DetectAuthMode 探测裸 token 的形态：先按 cookie 试，再按 bearer 试。
//
// 为什么必须探测：signin 签发的是 cookie token，网页端的 Bearer 是另一种凭据，
// **两者不可互换**，而且同时发会互相覆盖（Bearer 优先且无效即 401）。
// 已带前缀的凭据直接返回，不产生额外请求。
func (c *Client) DetectAuthMode(ctx context.Context, cred string) (string, error) {
	if mode, value := SplitCredential(cred); mode != "" || value == "" {
		return mode, nil
	}
	_, value := SplitCredential(cred)
	var firstErr error
	for _, mode := range []string{ModeCookie, ModeBearer} {
		if _, err := c.ListModels(ctx, WithMode(mode, value)); err == nil {
			return mode, nil
		} else if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = NewErr(KindAuth, "探测凭据形态", 0, "", "两种形态都不可用", true)
	}
	return "", firstErr
}

// NormalizeCredential 把用户输入的凭据整理成带形态的形态（不探测，只按写法推断）。
// 给"启动时来不及探测"的场景用：完整 jar ⇒ cookie，其余 ⇒ 空形态（待探测）。
func NormalizeCredential(cred string) string {
	c := strings.TrimSpace(cred)
	if mode, value := SplitCredential(c); mode != "" {
		return WithMode(mode, value)
	}
	return c
}
