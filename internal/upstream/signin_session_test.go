package upstream

// 登录链路要产出**两份**凭据：access（15 分钟）与 refresh_token（可重复使用）。
//
// 这条测试钉的是一个真实事故：第一版把 signin 的 token cookie 当成"会话"
// 返回，于是 refresh_token 一个都没存下来，每个号 15 分钟后仍要重登。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestSignInSessionCapturesRefreshToken(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v2/auths/signin"):
			http.SetCookie(w, &http.Cookie{Name: "token", Value: "session-jwt"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/auths/refresh"):
			gotCookie = r.Header.Get("Cookie")
			// refresh_token 只在 refresh 响应里下发
			http.SetCookie(w, &http.Cookie{Name: "refresh_token", Value: "rt-jwt"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"at-jwt"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := config.Default().Upstream
	cfg.BaseURL = srv.URL
	cfg.AuthBaseURL = srv.URL
	cli := New(cfg)

	access, refresh, err := cli.SignInSession(context.Background(), "a@b.c", "pw")
	if err != nil {
		t.Fatalf("signin: %v", err)
	}
	if access != "bearer:at-jwt" {
		t.Fatalf("access 形态不对: %q", access)
	}
	if refresh != "rt-jwt" {
		t.Fatalf("应带上 refresh_token（来自 refresh 响应），实际 %q", refresh)
	}
	// 换 access 时必须用 signin 的 token cookie，而不是 refresh_token。
	if !strings.Contains(gotCookie, "token=session-jwt") {
		t.Fatalf("refresh 请求的 Cookie 不对: %q", gotCookie)
	}
}

func TestRefreshWithTokenSendsRefreshCookie(t *testing.T) {
	var gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"at2"}}`))
	}))
	defer srv.Close()

	cfg := config.Default().Upstream
	cfg.BaseURL = srv.URL
	cfg.AuthBaseURL = srv.URL
	cli := New(cfg)

	access, next, err := cli.RefreshWithToken(context.Background(), "rt-jwt")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if access != "at2" || next != "" {
		t.Fatalf("返回值不对: access=%q next=%q", access, next)
	}
	if gotCookie != "refresh_token=rt-jwt" {
		t.Fatalf("Cookie 头应为 refresh_token=…，实际 %q", gotCookie)
	}
}

// 2026-11-18 上游改版后的主路径：auth 域的 signin 直接下发 access_token
// 与 refresh_token，**不需要**再打 /auths/refresh。
//
// 这条是线上"密码正确却登不进去"的回归测试：旧代码只认 Set-Cookie 里的
// token，忽略 body 的 access_token，然后拿会话 cookie 去换票 —— 恒 401。
func TestSignInSessionUsesAccessTokenFromBody(t *testing.T) {
	var signinHits, refreshHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/api/v2/auths/signin"):
			signinHits++
			http.SetCookie(w, &http.Cookie{Name: "refresh_token", Value: "rt-body"})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"at-body","token":"session-jwt","token_type":"Bearer"}}`))
		case strings.HasSuffix(r.URL.Path, "/api/v2/auths/refresh"):
			refreshHits++
			_, _ = w.Write([]byte(`{"success":true,"data":{"access_token":"should-not-be-used"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := config.Default().Upstream
	cfg.BaseURL = srv.URL
	cfg.AuthBaseURL = srv.URL
	cli := New(cfg)

	access, refresh, err := cli.SignInSession(context.Background(), "a@b.c", "pw")
	if err != nil {
		t.Fatalf("signin: %v", err)
	}
	if access != "bearer:at-body" {
		t.Fatalf("应直接用 body 里的 access_token，实际 %q", access)
	}
	if refresh != "rt-body" {
		t.Fatalf("应取到 signin 下发的 refresh_token，实际 %q", refresh)
	}
	if signinHits != 1 || refreshHits != 0 {
		t.Fatalf("新版流程应只打一次 signin（实际 signin=%d refresh=%d）", signinHits, refreshHits)
	}
}

// data.token 是会话 cookie 的 JWT（只能读、不能写），不能当 access_token 用。
func TestSignInSessionIgnoresSessionTokenInBody(t *testing.T) {
	body := []byte(`{"success":true,"data":{"token":"session-jwt","token_type":"Bearer"}}`)
	if got := signinAccessToken(body); got != "" {
		t.Fatalf("不该把 data.token 当 access_token，实际 %q", got)
	}
	if got := signinAccessToken([]byte(`{"success":true,"data":{"access_token":"at"}}`)); got != "at" {
		t.Fatalf("应取 data.access_token，实际 %q", got)
	}
}
