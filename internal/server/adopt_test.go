package server

// 补 refresh_token 的节奏控制（2026-11-18 线上教训）。
//
// 背景：adopt 原来是"每轮 batch 个"，tick 20s ⇒ 36 次密码登录/分钟。
// 108 个坏号在重启后被逐个重登，10 分钟判死 66 个，还把主循环拖到 45~60s。
// 现在两道闸门：热池够用就不补；不够用时按分钟配额滚动补。

import (
	"context"
	"fmt"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// 能续期的号已经覆盖热池目标 ⇒ 一个 signin 都不发。
func TestAdoptSkipsWhenRefreshPoolIsEnough(t *testing.T) {
	cfg := config.Default()
	cfg.StateFile = ""
	cfg.Accounts = nil
	// warmTarget 在零流量下是 8（rate*15*1.5+8 的下限）。
	for i := 0; i < 10; i++ {
		cfg.Accounts = append(cfg.Accounts, config.Account{
			Name: fmt.Sprintf("cap%d", i), Token: "bearer:t", RefreshToken: "rt", Password: "p",
		})
	}
	cfg.Accounts = append(cfg.Accounts, config.Account{Name: "noref", Token: "bearer:t", Password: "p"})

	p := pool.New(cfg.Pool, cfg.Accounts, "")
	if p.RefreshCapable() < 8 {
		t.Fatalf("用例前提不成立：能续期 %d < target 8", p.RefreshCapable())
	}
	s := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(10), "test")
	s.adoptCarry = 1 // 假装攒了配额

	if s.adoptOnce(context.Background(), 0) {
		t.Fatal("热池够用时不该补 refresh_token")
	}
	if got := p.MissingRefresh(); got != 1 {
		t.Fatalf("缺口应保持 1（不补），实际 %d", got)
	}
	if s.adoptCarry != 0 {
		t.Fatalf("池子够用时攒的配额应清零，实际 %v", s.adoptCarry)
	}
}

// 分钟配额：perMin=2、tick=20s ⇒ 每 3 轮补 1 个，60 轮（20 分钟）约 40 个。
func TestTakeAdoptBudget(t *testing.T) {
	carry := 0.0
	total := 0
	for i := 0; i < 60; i++ {
		n, c := takeAdoptBudget(carry, 2, 20, 12)
		total += n
		carry = c
	}
	if total != 40 {
		t.Fatalf("20 分钟应补 40 个（2/分钟），实际 %d", total)
	}

	// 上限 batch 生效：perMin 很大时也不会一轮打出一串。
	n, c := takeAdoptBudget(0, 600, 20, 3)
	if n != 3 || c > 3 {
		t.Fatalf("应被 batch=3 截断，实际 n=%d carry=%v", n, c)
	}
	// 关掉（perMin=0）⇒ 不补。
	if n, c := takeAdoptBudget(5, 0, 20, 12); n != 0 || c != 0 {
		t.Fatalf("perMin=0 应不补，实际 n=%d carry=%v", n, c)
	}
}
