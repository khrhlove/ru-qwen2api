package server

// 余额读取遇到"票被上游拒绝"时的处置。
//
// 背景（2026-11-20 线上实测）：main 号存的是 30 天会话 JWT，当 Bearer 发
// chat 域 401、积分域 HTTP 200 + NOT_LOGIN；而它的 exp 还剩 28 天，
// 所有"本地新鲜度"判断都认为它可用 —— 于是余额轮询反复刷它、没有任何路径修它。
//
// 这里钉住两条：① 被拒后要换票再读一次（不是重试同一张票）；
// ② 只换一次 —— 视频提交路径上多一次 signin 就是多几秒首字。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// creditsRepairServer 假上游：credits 前 reject 次返回 NOT_LOGIN，之后返回余额；
// refresh 换回一张 15 分钟的真 JWT（这样"是否作废新鲜度"才可断言）。
func creditsRepairServer(t *testing.T, reject int32) (s *Server, credits, refreshes *int32) {
	t.Helper()
	fresh := jwtExpiring(t, time.Now().Add(15*time.Minute))
	var cN, rN int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/entitlement/credits/credits/history"):
			if atomic.AddInt32(&cN, 1) <= reject {
				// 线上原样：HTTP 200 + 信封 401。
				_, _ = w.Write([]byte(`{"traceId":"x","httpCode":401,"success":false,` +
					`"errorCode":"NOT_LOGIN","errorMsg":"User not logged in"}`))
				return
			}
			_, _ = w.Write([]byte(`{"traceId":"x","httpCode":200,"success":true,` +
				`"data":{"totalCredits":40,"subscriptionCredits":0,"bonusCredits":40}}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/auths/refresh"):
			atomic.AddInt32(&rN, 1)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"success":true,"data":{"access_token":%q}}`, fresh)))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default()
	cfg.Upstream.BaseURL = srv.URL
	cfg.Upstream.AuthBaseURL = srv.URL
	cfg.Upstream.EntitlementBaseURL = srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{
		Name: "a0", Email: "a0@x.com", Password: "pw", RefreshToken: "rt-old",
		// 本地看着还新鲜（10 分钟）—— 正是"永远不会被修"的那种形态。
		Token: "bearer:" + jwtExpiring(t, time.Now().Add(10*time.Minute)),
	}}
	p := pool.New(cfg.Pool, cfg.Accounts, cfg.StateFile)
	return New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(10), "test"), &cN, &rN
}

func TestRefreshOneCreditReloginsAfterNotLogin(t *testing.T) {
	s, credits, refreshes := creditsRepairServer(t, 1)

	if !s.refreshOneCredit(context.Background(), "a0") {
		t.Fatal("换票后应能读到余额")
	}
	if bal, known := s.pool.CreditBalance("a0"); !known || bal != 40 {
		t.Fatalf("余额应为 40 且 known，实际 %d/%v", bal, known)
	}
	if n := atomic.LoadInt32(credits); n != 2 {
		t.Fatalf("应读两次（被拒 → 换票 → 重读），实际 %d", n)
	}
	if n := atomic.LoadInt32(refreshes); n != 1 {
		t.Fatalf("应换票一次，实际 %d", n)
	}
}

func TestRefreshOneCreditRepairsOnlyOnce(t *testing.T) {
	// 换票后仍被拒 ⇒ 不再换第二张，把号交给后台（token 作废）。
	s, credits, refreshes := creditsRepairServer(t, 99)

	if s.refreshOneCredit(context.Background(), "a0") {
		t.Fatal("读不到余额就该返回 false，让视频路径按'未知'放行")
	}
	if n := atomic.LoadInt32(credits); n != 2 {
		t.Fatalf("只该多读一次，实际 %d 次", n)
	}
	if n := atomic.LoadInt32(refreshes); n != 1 {
		t.Fatalf("只该换一次票，实际 %d 次（每多一次 signin 都是几秒首字）", n)
	}
	if _, ok := s.pool.TokenExpiry("a0"); ok {
		t.Fatal("失败后必须作废本地新鲜度，否则它永远不进入续期候选")
	}
}
