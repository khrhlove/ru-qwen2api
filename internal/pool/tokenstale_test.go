package pool

// 票型与"本地新鲜度"的两条硬约束。
//
// 背景（2026-11-20 线上实测）：main 号存的是 30 天会话 JWT，当 Bearer 发
// chat 域 401、积分域 HTTP 200 + NOT_LOGIN；而本地所有调度只读 exp，
// 把它当成"还能用 28 天"。结果是一个坏号既不会被修、也不会被跳过：
// 选号优先它、余额轮询反复刷它、保活又因为它"离过期还远"从不碰它。

import (
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestMarkTokenStaleDropsLocalFreshness(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Token: "bearer:" + fakeJWT(t, time.Now().Add(10*time.Minute).Unix())},
	}, "")
	if _, ok := p.TokenExpiry("a"); !ok {
		t.Fatal("前置：应能从 JWT 解出 exp")
	}

	p.MarkTokenStale("a")

	if exp, ok := p.TokenExpiry("a"); ok {
		t.Fatalf("作废后不应再被当成新鲜票，仍解出 %v", exp)
	}
	if p.Credential("a") == "" {
		t.Fatal("只作废新鲜度，票本身必须留着（换票失败时还要由上游判定）")
	}
}

func TestCreditStaleBacksOffAfterFailure(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{{Name: "a"}}, "")

	if got := p.CreditStale(time.Minute); len(got) != 1 {
		t.Fatalf("从没读过应被选中，实际 %v", got)
	}

	p.SetCreditBalance("a", -1) // 读失败

	// 不退避的代价：面板 5s 轮询 = 每 5 秒重打一次注定失败的上游，
	// 而 noteCreditErr 的 5 分钟去重会把频次完全掩盖。
	if got := p.CreditStale(time.Minute); len(got) != 0 {
		t.Fatalf("刚失败过应退避到 TTL，实际仍选中 %v", got)
	}
	if got := p.CreditStale(0); len(got) != 1 {
		t.Fatalf("force 刷新（ttl=0）不应受退避限制，实际 %v", got)
	}
}

func TestNewDropsSessionTicket(t *testing.T) {
	// 30 天 = 会话 JWT（2026-11-20 实测形态），不能当 access token 收下。
	session := "bearer:" + fakeJWT(t, time.Now().Add(30*24*time.Hour).Unix())
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Token: session, Password: "pw"},
	}, "")

	if got := p.Credential("a"); got != "" {
		t.Fatalf("会话 JWT 应在入口被丢弃，实际 %q", got)
	}
	if _, ok := p.TokenExpiry("a"); ok {
		t.Fatal("被丢弃的票不该留下 exp")
	}

	// 900 秒的真 access token 不受影响。
	access := "bearer:" + fakeJWT(t, time.Now().Add(15*time.Minute).Unix())
	q := New(config.Default().Pool, []config.Account{
		{Name: "b", Token: access},
	}, "")
	if q.Credential("b") == "" {
		t.Fatal("15 分钟的 access token 不该被当成会话票")
	}
}
