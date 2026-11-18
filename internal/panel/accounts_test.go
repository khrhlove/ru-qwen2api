package panel

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
)

func TestAcctStatusOf(t *testing.T) {
	cases := []struct {
		name string
		v    pool.View
		want string
	}{
		{"失效", pool.View{State: "auth_dead"}, "dead"},
		{"冷却", pool.View{State: "cooling"}, "cooling"},
		{"正常", pool.View{State: "ready"}, "ready"},
		// 出图额度用尽且没有余额 ⇒ 这个号暂时什么都做不了，单列一档
		{"额度用尽", pool.View{State: "ready", ImageLimit: 6, ImageToday: 6, CreditKnown: true, CreditBalance: 0}, "exhausted"},
		// 额度用尽但还有积分 ⇒ 还能做视频，不算用尽
		{"额度用尽但有积分", pool.View{State: "ready", ImageLimit: 6, ImageToday: 6, CreditKnown: true, CreditBalance: 40}, "ready"},
		// 余额未知不判用尽（宁可放行，让上游兜底）
		{"余额未知", pool.View{State: "ready", ImageLimit: 6, ImageToday: 6}, "ready"},
	}
	for _, c := range cases {
		if got := acctStatusOf(c.v, 30); got != c.want {
			t.Fatalf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestSummarize(t *testing.T) {
	views := []pool.View{
		{State: "ready", ImageLimit: 6, ImageToday: 2, CreditKnown: true, CreditBalance: 90, HasRefresh: true, TokenKnown: true, TokenLeftS: 600},
		{State: "ready", ImageLimit: 6, ImageToday: 6, CreditKnown: true, CreditBalance: 0},
		{State: "cooling", ImageLimit: 6, ImageToday: 0},
		{State: "auth_dead"},
	}
	st := summarize(views, 30)

	if st.Total != 4 || st.Ready != 1 || st.Cooling != 1 || st.Dead != 1 || st.Exhausted != 1 {
		t.Fatalf("状态计数错: %+v", st)
	}
	// 剩余张数：第一个号剩 4，第二个 0，第三个 6（冷却也计入余量，它只是暂时不可用）
	if st.ImgLeft != 10 {
		t.Fatalf("剩余张数应 10，实际 %d", st.ImgLeft)
	}
	if st.Credits != 90 || st.CreditsKnown != 2 {
		t.Fatalf("余额汇总错: %+v", st)
	}
	// 90 分 ÷ 30 = 3 条 720P 5s
	if st.VidLeft != 3 {
		t.Fatalf("可做视频数应 3，实际 %d", st.VidLeft)
	}
	if st.RefreshKnown != 1 || st.TokenFresh != 1 {
		t.Fatalf("保活口径错: %+v", st)
	}
	// 另外三个号没有 JWT ⇒ 凭据寿命未知；热池只有第一个（可用 + 票热）
	if st.TokenExpired != 0 || st.TokenUnknown != 3 {
		t.Fatalf("凭据计数错: %+v", st)
	}
	if st.Hot != 1 {
		t.Fatalf("热池应 1 个，实际 %d", st.Hot)
	}
}

// 热池的定义要能和选号口径对上：冷却/额度用尽的号即使票是热的也不算。
func TestAcctHot(t *testing.T) {
	cases := []struct {
		name string
		v    pool.View
		want bool
	}{
		{"可用且票剩 10 分钟", pool.View{State: "ready", TokenKnown: true, TokenLeftS: 600}, true},
		{"可用但票只剩 30 秒", pool.View{State: "ready", TokenKnown: true, TokenLeftS: 30}, false},
		{"可用但票已过期", pool.View{State: "ready", TokenKnown: true, TokenLeftS: -120}, false},
		{"票寿命未知", pool.View{State: "ready"}, false},
		{"冷却中", pool.View{State: "cooling", TokenKnown: true, TokenLeftS: 600}, false},
		{"额度用尽", pool.View{State: "ready", ImageLimit: 6, ImageToday: 6, CreditKnown: true, CreditBalance: 0, TokenKnown: true, TokenLeftS: 600}, false},
	}
	for _, c := range cases {
		if got := acctHot(c.v, 30); got != c.want {
			t.Fatalf("%s: got %v want %v", c.name, got, c.want)
		}
	}
	// "有效"只看票本身，不看状态 —— 冷却中的号票也是有效的。
	if !tokenAlive(pool.View{TokenKnown: true, TokenLeftS: 1}) || tokenAlive(pool.View{TokenKnown: true, TokenLeftS: 0}) {
		t.Fatal("tokenAlive 口径不对")
	}
}


func TestAtoiOr(t *testing.T) {
	if atoiOr("", 7) != 7 || atoiOr("abc", 7) != 7 || atoiOr("12", 7) != 12 {
		t.Fatal("atoiOr 行为不对")
	}
}

// fakeJWT 造一个只带 exp 的 JWT：池和面板都只读 payload.exp，不需要真签名。
func fakeJWT(exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(exp.Unix(), 10) + `}`))
	return "eyJhbGciOiJIUzI1NiJ9." + payload + ".sig"
}

// 面板的账号列表必须能按凭据寿命筛/排 —— 这是"状态可用但票过期"那批号的唯一出口。
// 口径：alive = 未过期（>0s）、expired = 已过期、unknown = 不是 JWT；
// hot  = 状态可用 且 token 剩余 >2 分钟（与池选号同口径）。
func TestAccountsTokenFilterAndSort(t *testing.T) {
	now := time.Now()
	p, _ := withPanel(func(c *config.Config) {
		c.Accounts = []config.Account{
			{Name: "hot", Email: "hot@x", Token: fakeJWT(now.Add(10 * time.Minute))},
			{Name: "edge", Email: "edge@x", Token: fakeJWT(now.Add(30 * time.Second))},
			{Name: "cold", Email: "cold@x", Token: fakeJWT(now.Add(-2 * time.Hour))},
			{Name: "opaque", Email: "opaque@x", Token: "not-a-jwt"},
		}
	})
	ck := withCookie(doLogin(p, "admin", "pw-test"))
	if ck == nil {
		t.Fatal("登录没拿到会话 cookie")
	}

	type page struct {
		Items []pool.View `json:"items"`
		Total int         `json:"total"`
		Stats acctStats   `json:"stats"`
	}
	get := func(qs string) page {
		rec := getWith(p, "/panel/api/accounts?size=50&"+qs, ck)
		if rec.Code != 200 {
			t.Fatalf("%s: HTTP %d %s", qs, rec.Code, rec.Body.String())
		}
		var out page
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: 解析失败 %v", qs, err)
		}
		return out
	}

	if got := get("token=alive"); got.Total != 2 {
		t.Fatalf("token=alive 应 2 个，实际 %d（%v）", got.Total, got.Items)
	}
	if got := get("token=expired"); got.Total != 1 || got.Items[0].Name != "cold" {
		t.Fatalf("token=expired 应只有 cold，实际 %+v", got.Items)
	}
	if got := get("token=unknown"); got.Total != 1 || got.Items[0].Name != "opaque" {
		t.Fatalf("token=unknown 应只有 opaque，实际 %+v", got.Items)
	}
	// edge 只剩 30s ⇒ 票有效但不是热号
	if got := get("state=hot"); got.Total != 1 || got.Items[0].Name != "hot" {
		t.Fatalf("state=hot 应只有 hot，实际 %+v", got.Items)
	}

	d := get("sort=token_desc")
	want := []string{"hot", "edge", "cold", "opaque"}
	for i, n := range want {
		if i >= len(d.Items) || d.Items[i].Name != n {
			t.Fatalf("token_desc 顺序应为 %v，实际 %+v", want, d.Items)
		}
	}
	a := get("sort=token_asc")
	if len(a.Items) != 4 || a.Items[0].Name != "opaque" {
		t.Fatalf("token_asc 应把未知/过期放前面，实际 %+v", a.Items)
	}

	st := d.Stats
	if st.Total != 4 || st.TokenFresh != 2 || st.TokenExpired != 1 || st.TokenUnknown != 1 || st.Hot != 1 {
		t.Fatalf("汇总口径错: %+v", st)
	}
}

