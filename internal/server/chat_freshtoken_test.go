package server

// 401 后续期的"本地新鲜度"陷阱（2026-11-20 会话 JWT 事故）。
//
// relogin 开头有一句"还新鲜就别换"的短路，本意是省掉等锁期间的重复换票。
// 但上游刚回 401 时，这张票已经不被认可，本地 exp 还剩多少都不作数：
// 短路会让换票原地返回 true，接着用同一张死票重试（必然再 401），
// 最后 MarkAuth 把号标成 auth_dead —— 而 auth_dead 不在任何自愈候选里。
//
// 这条测试的账号 token 是"10 分钟后过期"的真 JWT：旧的实现会认为它新鲜、
// 拒绝换票，从而失败；新实现先 MarkTokenStale 再换票。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

func TestChatRenewsSameAccountWhenFreshTokenRejected(t *testing.T) {
	up := newFakeUpstream(t, 1) // 第一次调用回 401，之后正常

	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.Upstream.AuthBaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{
		Name:  "a0",
		Token: "bearer:" + jwtExpiring(t, time.Now().Add(10*time.Minute)),
	}}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	p.SetRefresh("a0", "rt-0")
	h := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(10), "test").Handler()

	body := `{"model":"qwen3.7-plus","messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("401 后换票重试应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if up.gotRefresh == 0 {
		t.Fatal("上游已拒绝这张票，不能因为本地 exp 还新鲜就跳过换票（会拿同一张死票重试）")
	}
}
