package upstream

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// jwtWithExp 造一个只有 payload 有意义的 JWT（TokenExpiry 不验签）。
func jwtWithExp(t *testing.T, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestLooksLikeSessionTicket 判据只有寿命：access 900 秒，会话票 30 天。
//
// 这条守的是入口：会话票当 Bearer 发，chat 域 401、积分域 NOT_LOGIN（2026-11-20
// 实测），而本地只读 exp 的调度会把它当"还能用 30 天"—— 收下它等于制造僵尸号。
func TestLooksLikeSessionTicket(t *testing.T) {
	cases := []struct {
		name string
		cred string
		want bool
	}{
		{"access 900s", "bearer:" + jwtWithExp(t, time.Now().Add(900*time.Second)), false},
		{"session 30d", "bearer:" + jwtWithExp(t, time.Now().Add(30*24*time.Hour)), true},
		{"裸 JWT（浏览器里直接拷的）", jwtWithExp(t, time.Now().Add(7*24*time.Hour)), true},
		// cookie jar 本来就是长寿命会话，鉴权走 Cookie 头而不是 Bearer，
		// 按寿命判会误伤 —— 这条是防误杀的。
		{"cookie jar", "cookie:token=" + jwtWithExp(t, time.Now().Add(7*24*time.Hour)) + "; sid=x", false},
		{"非 JWT", "plain-token", false},
		{"空凭据", "", false},
	}
	for _, c := range cases {
		if got := LooksLikeSessionTicket(c.cred); got != c.want {
			t.Fatalf("%s: 期望 %v，实际 %v", c.name, c.want, got)
		}
	}
}
