package pool

// 大池子的选号与预热调度。
//
// 目标：负载在整池均匀摊开（用得最少的先上），同时让"下一步要用的那批"
// 凭据提前变新，请求路径上不用等续期。

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestPickEvenlyByServed(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Token: "bearer:a"},
		{Name: "b", Token: "bearer:b"},
		{Name: "c", Token: "bearer:c"},
	}, "")

	// 每轮"取 → 成功 → 释放"，统计 9 次的选择顺序。
	// served 只在 MarkOK（请求真的成功）时 +1，所以失败不会把号"用旧"。
	var got []string
	for i := 0; i < 9; i++ {
		a, ok := p.Pick()
		if !ok {
			t.Fatal("应能选出账号")
		}
		got = append(got, a.Name)
		p.MarkOK(a)
		p.Release(a)
	}
	want := []string{"a", "b", "c", "a", "b", "c", "a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 次取到 %s，期望 %s（顺序=%v）", i+1, got[i], want[i], got)
		}
	}
}

func TestPickTieBreaksByInflight(t *testing.T) {
	// served 相同时优先"在途最少"：并发场景下不能把请求堆到同一个号上。
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Token: "bearer:a"},
		{Name: "b", Token: "bearer:b"},
	}, "")
	a1, _ := p.Pick()
	a2, _ := p.Pick()
	if a1.Name == a2.Name {
		t.Fatalf("第二个请求应选另一个号，实际都选了 %s", a1.Name)
	}
}

func TestWarmCandidatesFollowsUseOrder(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{
		{Name: "hot", Token: "bearer:a", RefreshToken: "rt-a"},
		{Name: "cold", Token: "bearer:b", RefreshToken: "rt-b"},
		{Name: "norefresh", Token: "bearer:c"},
	}, "")
	soon := time.Now().Add(time.Minute)
	p.mu.Lock()
	p.accounts[0].served = 5
	p.accounts[0].tokenExp = soon
	p.accounts[1].served = 0
	p.accounts[1].tokenExp = soon
	p.accounts[2].served = 0
	p.accounts[2].tokenExp = soon
	p.mu.Unlock()

	// 没有 refresh_token 的号根本进不了候选（预热只能靠 refresh）。
	// target=1 时只覆盖"用得最少"的那个：cold。
	if names := p.WarmCandidates(1, 3*time.Minute, 8); len(names) != 1 || names[0] != "cold" {
		t.Fatalf("target=1 应只预热 cold，实际 %v", names)
	}
	// target=2 时按 served 升序补上 hot（它用得比 cold 多，排在后面）。
	names := p.WarmCandidates(2, 3*time.Minute, 8)
	if len(names) != 2 || names[0] != "cold" || names[1] != "hot" {
		t.Fatalf("target=2 应按使用次数排序 [cold hot]，实际 %v", names)
	}
}

func TestWarmCandidatesSkipsFreshTokens(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Token: "bearer:a", RefreshToken: "rt-a"},
	}, "")
	p.mu.Lock()
	p.accounts[0].tokenExp = time.Now().Add(30 * time.Minute) // 还早
	p.mu.Unlock()

	if names := p.WarmCandidates(8, 3*time.Minute, 8); len(names) != 0 {
		t.Fatalf("凭据还新鲜时不该预热，实际 %v", names)
	}

	p.mu.Lock()
	p.accounts[0].tokenExp = time.Now().Add(-time.Minute) // 已过期
	p.mu.Unlock()
	if names := p.WarmCandidates(8, 3*time.Minute, 8); len(names) != 1 {
		t.Fatalf("已过期的凭据应被预热，实际 %v", names)
	}
}

// 每个号有每日额度：图/视频都打满的号接不了这两类活，
// 热池位置要让给今天还没干活的号 —— 这就是"额度驱动的热池轮换"。
//
// 不这么做的话，同一批号会热到打满额度，之后每个越过额度的请求都要在
// 请求路径上现找一个冷号续期（多付一次 refresh）。
func TestWarmCandidatesYieldsToUnspentAccounts(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.MaxImagePerDay = 6
	cfg.Pool.MaxVideoPerDay = 1
	p := New(cfg.Pool, []config.Account{
		{Name: "burned", Token: "bearer:a", RefreshToken: "rt-a"},
		{Name: "fresh", Token: "bearer:b", RefreshToken: "rt-b"},
	}, "")

	now := time.Now()
	today := now.Format("2006-01-02")
	p.mu.Lock()
	// burned：票还活着且马上过期（按老排序它会被优先续），但额度已打满。
	p.accounts[0].tokenExp = now.Add(time.Minute)
	p.accounts[0].imageDay, p.accounts[0].imageCount = today, cfg.Pool.MaxImagePerDay
	p.accounts[0].videoDay, p.accounts[0].videoCount = today, cfg.Pool.MaxVideoPerDay
	// fresh：票已过期（按老排序它排在后面），但今天一点额度都没用。
	p.accounts[1].tokenExp = now.Add(-time.Hour)
	p.accounts[1].served = 0
	p.mu.Unlock()

	if names := p.WarmCandidates(1, 3*time.Minute, 8); len(names) != 1 || names[0] != "fresh" {
		t.Fatalf("额度打满的号应让位给还没干活的号，实际 %v", names)
	}
}

func TestQuotaLeft(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.MaxImagePerDay = 6
	cfg.Pool.MaxVideoPerDay = 1
	p := New(cfg.Pool, []config.Account{{Name: "a", Token: "bearer:a"}}, "")
	a := p.accounts[0]
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().Add(-24 * time.Hour).Format("2006-01-02")

	if got := p.quotaLeft(a, today); got != 2 {
		t.Fatalf("没干活应剩 2 类，实际 %d", got)
	}
	a.imageDay, a.imageCount = today, 6
	if got := p.quotaLeft(a, today); got != 1 {
		t.Fatalf("图打满应剩 1 类（视频），实际 %d", got)
	}
	a.videoDay, a.videoCount = today, 1
	if got := p.quotaLeft(a, today); got != 0 {
		t.Fatalf("两类都打满应为 0，实际 %d", got)
	}
	// 跨天后自动恢复：日期不是今天 ⇒ 视为没干过活（清零发生在选号时）。
	if got := p.quotaLeft(a, yesterday); got != 2 {
		t.Fatalf("跨天应恢复 2 类，实际 %d", got)
	}
	// 额度 0 = 不限，永远算可用。
	cfg.Pool.MaxImagePerDay, cfg.Pool.MaxVideoPerDay = 0, 0
	p.SetConfig(cfg.Pool)
	if got := p.quotaLeft(a, today); got != 2 {
		t.Fatalf("不限额时应恒为 2，实际 %d", got)
	}
}

func TestWarmCandidatesRespectsLimit(t *testing.T) {
	accs := make([]config.Account, 0, 10)
	for i := 0; i < 10; i++ {
		accs = append(accs, config.Account{
			Name: fmt.Sprintf("a%02d", i), Token: "bearer:x",
			RefreshToken: "rt",
		})
	}
	p := New(config.Default().Pool, accs, "")
	p.mu.Lock()
	for _, a := range p.accounts {
		a.tokenExp = time.Now().Add(-time.Minute)
	}
	p.mu.Unlock()

	if names := p.WarmCandidates(10, 3*time.Minute, 3); len(names) != 3 {
		t.Fatalf("单轮上限应生效，实际 %d 个", len(names))
	}
}

func TestSetTokenParsesExpiry(t *testing.T) {
	// 过期时间从 JWT 的 exp 解出来，而不是猜一个 TTL。
	exp := time.Now().Add(900 * time.Second).Unix()
	tok := fakeJWT(t, exp)

	p := New(config.Default().Pool, []config.Account{{Name: "a"}}, "")
	p.SetToken("a", "bearer:"+tok)
	got, ok := p.TokenExpiry("a")
	if !ok || got.Unix() != exp {
		t.Fatalf("应解出 exp=%d，实际 %v ok=%v", exp, got, ok)
	}
}

// fakeJWT 造一个只有 payload 有意义的 JWT（不验签，TokenExpiry 只读 exp）。
func fakeJWT(t *testing.T, exp int64) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": exp, "iat": exp - 900})
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestExportKeepsRefreshToken(t *testing.T) {
	// refresh_token 必须进 accounts.json，否则重启后整池重新登录。
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Token: "bearer:t", RefreshToken: "rt-1", Email: "a@b.c", Password: "pw"},
	}, "")
	out := p.Export()
	if len(out) != 1 || out[0].RefreshToken != "rt-1" {
		t.Fatalf("Export 丢了 refresh_token: %+v", out)
	}

	// 重新加载后仍然可用（模拟重启）
	p2 := New(config.Default().Pool, out, "")
	if p2.RefreshToken("a") != "rt-1" {
		t.Fatal("重启后应保留 refresh_token")
	}
}

func TestSetRefreshParsesExpiry(t *testing.T) {
	// refresh_token 的 30 天到期时间也从 JWT 解出来，保养任务靠它排序。
	exp := time.Now().Add(30 * 24 * time.Hour).Unix()
	rt := fakeJWT(t, exp)

	p := New(config.Default().Pool, []config.Account{{Name: "a"}}, "")
	p.SetRefresh("a", rt)
	views := p.Snapshot()
	if len(views) != 1 || !views[0].RefreshKnown || views[0].RefreshLeftS <= 29*86400 {
		t.Fatalf("refresh 到期时间没解析出来: %+v", views)
	}
}

func TestRefreshExpiringPicksSoonest(t *testing.T) {
	soon := fakeJWT(t, time.Now().Add(24*time.Hour).Unix())
	later := fakeJWT(t, time.Now().Add(10*24*time.Hour).Unix())
	far := fakeJWT(t, time.Now().Add(60*24*time.Hour).Unix())
	p := New(config.Default().Pool, []config.Account{
		{Name: "later", Password: "p"},
		{Name: "soon", Password: "p"},
		{Name: "far", Password: "p"},
	}, "")
	p.SetRefresh("later", later)
	p.SetRefresh("soon", soon)
	p.SetRefresh("far", far)

	// 3 天窗口内只有 soon；排序按到期时间。
	got := p.RefreshExpiring(3*24*time.Hour, 10)
	if len(got) != 1 || got[0] != "soon" {
		t.Fatalf("应只保养 soon，实际 %v", got)
	}
	// 窗口放大到 15 天：soon 在前、later 在后，far 仍不进。
	got = p.RefreshExpiring(15*24*time.Hour, 10)
	if len(got) != 2 || got[0] != "soon" || got[1] != "later" {
		t.Fatalf("应按到期时间升序 [soon later]，实际 %v", got)
	}
	// limit 生效
	if got := p.RefreshExpiring(15*24*time.Hour, 1); len(got) != 1 {
		t.Fatalf("limit 未生效: %v", got)
	}
}

func TestMissingRefresh(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Password: "p"},
		{Name: "b", Password: "p", RefreshToken: "rt"},
		{Name: "c"}, // 没有密码：补不了，也不算缺口
	}, "")
	if got := p.MissingRefresh(); got != 1 {
		t.Fatalf("缺口应只有 a，实际 %d", got)
	}
}

// 热池轮换：环的顺序必须稳定（按名字），每次推进一段，走完一圈要能绕回来。
//
// 不能按 alive/served 排 —— 那两个值每 tick 都在动，游标切出来的
// "下一批"会跟着漂移，等于没轮换。
func TestRotateHotWindowAdvances(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{
		{Name: "f", Token: "bearer:f", RefreshToken: "rt-f"},
		{Name: "e", Token: "bearer:e", RefreshToken: "rt-e"},
		{Name: "d", Token: "bearer:d", RefreshToken: "rt-d"},
		{Name: "c", Token: "bearer:c", RefreshToken: "rt-c"},
		{Name: "b", Token: "bearer:b", RefreshToken: "rt-b"},
		{Name: "a", Token: "bearer:a", RefreshToken: "rt-a"},
	}, "")

	w1 := p.RotateHotWindow(2)
	w2 := p.RotateHotWindow(2)
	w3 := p.RotateHotWindow(2)
	w4 := p.RotateHotWindow(2)
	if fmt.Sprint(w1) != "[a b]" || fmt.Sprint(w2) != "[c d]" || fmt.Sprint(w3) != "[e f]" {
		t.Fatalf("应按名字顺序推进，实际 %v / %v / %v", w1, w2, w3)
	}
	if fmt.Sprint(w4) != "[a b]" {
		t.Fatalf("走完一圈应绕回 [a b]，实际 %v", w4)
	}
	if !p.HotRotating() {
		t.Fatal("轮换后应标记为已启用")
	}
}

// 轮换只认"修得动"的号：失效、没有 refresh_token、额度打满的都不进环。
func TestRotateHotWindowSkipsUnfixable(t *testing.T) {
	cfg := config.Default()
	cfg.Pool.MaxImagePerDay, cfg.Pool.MaxVideoPerDay = 6, 1
	p := New(cfg.Pool, []config.Account{
		{Name: "ok1", Token: "bearer:1", RefreshToken: "rt-1"},
		{Name: "dead", Token: "bearer:2", RefreshToken: "rt-2"},
		{Name: "noref", Token: "bearer:3"},
		{Name: "spent", Token: "bearer:4", RefreshToken: "rt-4"},
	}, "")
	now := time.Now()
	today := now.Format("2006-01-02")
	p.mu.Lock()
	p.accounts[1].authDead = true
	p.accounts[3].imageDay, p.accounts[3].imageCount = today, 6
	p.accounts[3].videoDay, p.accounts[3].videoCount = today, 1
	p.mu.Unlock()

	got := p.RotateHotWindow(8)
	if fmt.Sprint(got) != "[ok1]" {
		t.Fatalf("只应有 ok1 进环，实际 %v", got)
	}
}

// 启用轮换后，预热只覆盖当前窗口：否则旧窗口会被无限续期，轮换等于没做。
func TestWarmCandidatesOnlyWindow(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{
		{Name: "a", Token: "bearer:a", RefreshToken: "rt-a"},
		{Name: "b", Token: "bearer:b", RefreshToken: "rt-b"},
		{Name: "c", Token: "bearer:c", RefreshToken: "rt-c"},
	}, "")
	soon := time.Now().Add(time.Minute)
	p.mu.Lock()
	for _, a := range p.accounts {
		a.tokenExp = soon
	}
	p.mu.Unlock()

	// c 不在窗口里，即使它更该续也不该被续。
	p.RotateHotWindow(2) // 环 [a b c] 取前 2 → a,b
	names := p.WarmCandidates(8, 3*time.Minute, 8)
	if fmt.Sprint(names) != "[a b]" {
		t.Fatalf("应只预热窗口内的 [a b]，实际 %v", names)
	}
}

// 换批后的重叠期：新窗口优先保活，旧窗口的会话继续维持（不立刻撤）。
func TestHotNamesPrefersWindow(t *testing.T) {
	p := New(config.Default().Pool, []config.Account{
		{Name: "old", Token: "bearer:a", RefreshToken: "rt-a"}, // 老窗口：过期、served 0（本该排第一）
		{Name: "new", Token: "bearer:z", RefreshToken: "rt-z"}, // 新窗口：还活着、served 9
	}, "")
	p.mu.Lock()
	p.accounts[0].served = 0
	p.accounts[0].tokenExp = time.Now().Add(-time.Hour)
	p.accounts[1].served = 9
	p.accounts[1].tokenExp = time.Now().Add(10 * time.Minute)
	// 把 old 移出环，于是 size=1 的窗口只会落到 new 上。
	p.accounts[0].RefreshToken = ""
	p.mu.Unlock()

	if got := p.RotateHotWindow(1); fmt.Sprint(got) != "[new]" {
		t.Fatalf("窗口应只有 new，实际 %v", got)
	}
	if names := p.HotNames(1); len(names) != 1 || names[0] != "new" {
		t.Fatalf("窗口内的号应优先保活，实际 %v", names)
	}
	// 关掉轮换后窗口清空，回到 alive/served 排序。
	p.ClearHotWindow()
	if p.HotRotating() {
		t.Fatal("清空后不该还标记为轮换中")
	}
}
