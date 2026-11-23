package server

// 账号保活：在请求路径之外，提前把「接下来会被用到」的账号凭据换新。
//
// 为什么需要它（2026-10-30 实测）：
//   - 上游 access token TTL 只有 **900 秒**；
//   - signin 的 token cookie 一次性，但 refresh 下发的 refresh_token **可重复使用**；
//   - 3000 个账号均匀轮询时，每个号被用到时大概率已过期 —— 不预热的话
//     每个请求都要先花几百毫秒续期（甚至重新登录），延迟直接翻倍。
//
// 预热的顺序与选号一致（served 升序），所以"预热的就是下一步要用的"。
// 这样做的额外开销是每 20 秒最多 8 次 refresh，与请求量解耦、也不碰密码。

import (
	"context"
	"hash/fnv"
	"log"
	"sync"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

const (
	// refreshMaintainWindow refresh_token 剩余不到这个时间就提前换新。
	// 它 TTL 是 30 天，留 3 天余量足够吸收停机/抖动。
	refreshMaintainWindow = 10 * 24 * time.Hour
	// refreshMaintainBatch 单轮最多保养几个（到期最急的先做）。
	//
	// 🔴 别拍脑袋放大。保养的动作是**密码 signin**（adoptOne），不是 refresh：
	// 每轮 N 个 × 每 tick 20s ⇒ 每分钟 3N 次 signin。batch=32 就是每天 13.8 万次，
	// 直接把 signin 打成风控（2026-11-18 实测：改成 32 后满屏 401，auth_dead
	// 从 98 涨到 100+，refresh_token 一个都补不上）。
	//
	// 原值 2 是有账的：refresh TTL 30 天、保养窗口 10 天 ⇒ 每号每 20 天一次
	// ⇒ 3000 号约 150 次/天，3N=6 次/分钟完全够，还留了大量余量。
	refreshMaintainBatch = 2
	// capTick 容量快照的打印间隔（见 logCapacity）。
	capTick = 5 * time.Minute
	// refreshRetryBackoff 续期失败后的静默期。
	//
	// 预热带是按 tick 轮转的：不压住失败号，它就会被每轮重新选中，对同一张可能
	// 已经作废的 refresh_token 持续打 auth 域 —— 既无意义，还容易招风控。
	// 取 5 分钟，与失败日志的去重窗口一致：一个号一次故障只留一条日志、一次处置。
	// ⚠️ 只压「结果未知」的失败。401 不进这张表：票已被撤销是确定性结论，
	// 再退避只会推迟唯一有效的修复动作（见 afterRefreshFailure）。
	refreshRetryBackoff = 5 * time.Minute
	// refreshLockShards 换票互斥的分片数（见 lockRefresh）。
	refreshLockShards = 64
	// startupWarmBatch 启动首轮的续期批量上限（斜坡）。
	//
	// 为什么要有：重启后整池 token 都过期，首轮 warmOnce 会按满批（默认 20~48 个）
	// 连打 auth 域，紧接着 prewarmChats 再打一批 chats/new —— 这些请求集中在池子
	// 那几个出口 IP 上。2026-11-20 两次线上部署都在启动后 1~4 分钟出现
	// `RGV587 / x5sec` 风控窗（详见 .workbuddy/memory）。首轮压到 8 个、
	// 且不预建会话，把"重启即突发"抹平；steady state 不受影响。
	startupWarmBatch = 8
)

// keepaliveTick 预热轮询间隔（配置可热更，改完下一轮生效）。
func (s *Server) keepaliveTick() time.Duration {
	ka := s.conf().Pool.Keepalive.Effective()
	return time.Duration(ka.TickS) * time.Second
}

// KeepAlive 后台保活循环（由 main 启动，测试里不跑）。
func (s *Server) KeepAlive(ctx context.Context) {
	// 启动就先跑一轮：重启后整池凭据都过期，等 20 秒才热身会让首批请求很慢。
	// 新凭据（access + refresh_token）必须落盘，否则重启后又要全量重登。
	// 用"最多两分钟写一次"折中：2000+ 账号的 accounts.json 有几百 KB，
	// 每个 tick 都写是浪费，而丢两分钟的续期结果无所谓（重新续一次就好）。
	lastSave := time.Now()
	saveIfDue := func(changed bool) {
		if !changed || time.Since(lastSave) < 2*time.Minute {
			return
		}
		if err := s.saveAccounts(); err != nil {
			log.Printf("[qwen2api] Не удалось сохранить обновлённую сессию: %v", err)
			return
		}
		lastSave = time.Now()
	}

	// runOne 跑一轮完整预热：补 refresh_token → 续 access → 预建会话/保活连接
	// → 保养 refresh_token。
	//
	// 🔴 peak 每轮只取一次：它是"上一轮到现在"的并发峰值，几个环节必须共用
	// 同一个数 —— 让每个函数各调一次 Swap(0) 的话，第一个调用者就把峰值吃掉，
	// 后面的环节全看到 0，等于白算。
	// 容量快照：5 分钟一条，用来回答"什么时候开始不够用了"。
	// 只打一行，便于 `docker logs | grep capacity` 直接看趋势。
	lastCap := time.Time{}
	// 热池轮换的计时起点：第一次轮换在启动 hot_rotate_s 之后。
	// 不复用"启动即轮换"：重启后整池都过期，第一轮该按排序把正在用的号热起来。
	lastRotate := time.Now()

	runOne := func(first bool) {
		peak := int(s.inflightPeak.Swap(0))
		// 先推进窗口：本轮 warmOnce 就该续新窗口里的号（换批是重叠式的，
		// 旧窗口的票不会立刻失效，见 rotateHotPool）。
		s.rotateHotPool(peak, &lastRotate)
		// 顺带维护上游模型清单：请求路径要拿它拒绝"已下架的模型"（见
		// openUpstreamSpec），但校验本身绝不能去打上游 —— 所以刷新放在这里，
		// 每轮一次（缓存 10 分钟，绝大多数轮次是空转）。
		_ = s.models.List(ctx)
		changed := s.adoptOnce(ctx, peak)
		if first {
			// 斜坡：首轮限量续期、不预建会话（见 startupWarmBatch 的注释）。
			changed = s.warmOnceCapped(ctx, peak, startupWarmBatch) || changed
		} else {
			changed = s.warmOnce(ctx, peak) || changed
			s.prewarmChats(ctx, peak)
		}
		changed = s.maintainRefresh(ctx) || changed
		saveIfDue(changed)
		if time.Since(lastCap) >= capTick {
			s.logCapacity(peak)
			lastCap = time.Now()
		}
	}

	runOne(true)
	for {
		select {
		case <-ctx.Done():
			// 退出前把最后一批续期结果写掉（重启后不用重登）。
			if err := s.saveAccounts(); err != nil {
				log.Printf("[qwen2api] 退出前Не удалось сохранить обновлённую сессию: %v", err)
			}
			return
		case <-time.After(s.keepaliveTick()):
			runOne(false)
		}
	}
}

// rotateHotPool 到点把热池窗口推进一段。
//
// 为什么是"重叠式"而不是直接换掉（2026-11-18）：新窗口的号没有预建会话，
// 换批瞬间的 chat 请求会掉进 +2~3s 的冷启动。这里只推进**续期**窗口：
// 新窗口立刻被续期 + 预建会话，旧窗口的票保留到自然过期（≤15 分钟），
// 这段时间两边都算热号（HotNames 把新窗口排在前面，prewarm 覆盖两倍目标）。
//
// 环的顺序和排除规则见 pool.RotateHotWindow。
func (s *Server) rotateHotPool(peak int, last *time.Time) {
	ka := s.conf().Pool.Keepalive.Effective()
	if ka.HotRotateS <= 0 {
		// 热更关掉轮换时必须清窗口，否则预热永远锁在最后那批号上。
		if s.pool.HotRotating() {
			s.pool.ClearHotWindow()
			log.Printf("[qwen2api] Плановая смена активной группы отключена. Подготовка идёт по порядку")
		}
		return
	}
	if time.Since(*last) < time.Duration(ka.HotRotateS)*time.Second {
		return
	}
	// 先记时间：即使这一轮没有可轮换的号，也别每个 tick 都重算一遍。
	*last = time.Now()
	size := s.warmTarget(peak)
	names := s.pool.RotateHotWindow(size)
	if len(names) == 0 {
		// 环里一个"修得动"的号都没有（例如启动时整池探测失败被冷却）。
		// 别白等一整个周期：1 分钟后重试。
		*last = time.Now().Add(time.Duration(ka.HotRotateS)*time.Second - time.Minute)
		log.Printf("[qwen2api] Нет доступных аккаунтов: проверьте сессии и квоту. Повтор через минуту")
		return
	}
	log.Printf("[qwen2api] Активная группа: %d аккаунтов (%s … %s). Прежние сессии истекут через 15 мин; период смены: %d с",
		len(names), names[0], names[len(names)-1], ka.HotRotateS)
}

// adoptOnce 给"还没 refresh_token 的号"补一张（用密码登录一次）。
//
// 两道闸门，缺一不可（2026-11-18 实测教训）：
//  1. **需求闸门**：能靠 refresh 续期的号已经够覆盖热池目标时，一个都不补。
//     补号的收益只是"以后不用密码重登"，池子够用时等于没有收益；
//     而每次补都是一次 signin —— 全链路最贵、最容易吃风控的动作。
//  2. **频率闸门**：不够用时也按"每分钟 N 个"滚动补，不按轮爆发。
//     原来 batch=12 × tick 20s = 36 次/分钟，10 分钟就把 66 个号打成 auth_dead，
//     还把主循环从 20s 拖到 45~60s（预热、会话预建全被拖慢）。
//
// 只补"接下来会被用到的那批"（served 升序）；并发固定 2 —— 密码登录快没有意义。
func (s *Server) adoptOnce(ctx context.Context, peak int) bool {
	ka := s.conf().Pool.Keepalive.Effective()
	target := s.warmTarget(peak)
	if s.pool.RefreshCapable() >= target {
		// 池子够用：把攒下的配额清零，别等哪天缺号了先补一串。
		s.adoptCarry = 0
		return false
	}
	budget := s.adoptBudget(ka)
	if budget <= 0 {
		return false
	}
	names := s.pool.AdoptCandidates(target, budget)
	if len(names) == 0 {
		return false
	}
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	changed := false
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if s.adoptOne(ctx, name) {
				mu.Lock()
				changed = true
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	return changed
}

// adoptBudget 取本轮可以补几个（滚动配额，见 takeAdoptBudget）。
func (s *Server) adoptBudget(ka config.Keepalive) int {
	n, carry := takeAdoptBudget(s.adoptCarry, ka.AdoptPerMin, ka.TickS, ka.AdoptBatch)
	s.adoptCarry = carry
	return n
}

// takeAdoptBudget 按"每分钟最多 N 个"折算本轮额度（纯函数，便于单测）。
//
// 攒够 1 个才还 1 个：tick=20s、perMin=2 时约每 3 轮补 1 个。
// 上限是 batch —— 防止主循环被拖慢时攒一大笔，下一轮又打出一串。
func takeAdoptBudget(carry float64, perMin, tickS, batch int) (int, float64) {
	if perMin <= 0 || tickS <= 0 || batch <= 0 {
		return 0, 0
	}
	carry += float64(perMin) * float64(tickS) / 60.0
	if carry > float64(batch) {
		carry = float64(batch)
	}
	// 加极小量再取整：0.666…×3 在浮点里是 1.9999999999999998，
	// 直接截断会长期少还一个配额（20 分钟少 1 次）。
	n := int(carry + 1e-9)
	return n, carry - float64(n)
}

// maintainRefresh 给"refresh_token 快到期"的号续一张。
//
// refresh_token 的 TTL 是 30 天；到期后 refresh 会失效，只能密码重登。
// 在到期前用密码换新的，就把"某天整批突然失效"摊成了平滑的后台任务：
// 3000 个号 ÷ 30 天 ≈ 每天 100 次，每轮 2 个绰绰有余。
func (s *Server) maintainRefresh(ctx context.Context) bool {
	names := s.pool.RefreshExpiring(refreshMaintainWindow, refreshMaintainBatch)
	if len(names) == 0 {
		return false
	}
	changed := false
	for _, n := range names {
		if s.adoptOne(ctx, n) { // 同一个动作：signin 换新 access + refresh_token
			changed = true
		}
	}
	if changed {
		log.Printf("[qwen2api] Обновление токенов: %d (до истечения менее трёх дней)", len(names))
	}
	return changed
}

// adoptOne 登录一次并把 access + refresh_token 都存下来。
func (s *Server) adoptOne(ctx context.Context, name string) bool {
	// 与预热 refresh 互斥：signin 会换掉整张会话，和 refresh 交叉执行时
	// 会把对方刚拿到的票作废（见 lockRefresh）。
	unlock := s.lockRefresh(name)
	defer unlock()
	// 与 refresh 共用退避表：登录失败（尤其被 WAF 拦）不能每轮都撞一次 ——
	// 一批密码失效的号足以把 signin 打成主要流量，还会挤占真正可修的重登名额。
	if !s.refreshDue(name, time.Now()) {
		return false
	}
	a := s.pool.Get(name)
	if a == nil || a.Password == "" {
		return false
	}
	// 🔴 超时不能长：这一步跑在保活主循环里，一个死号卡 30 秒
	// 就等于整轮预热（续期/预建会话/连接保活）全部顺延。失败本来就要退避，
	// 快失败才是对的（2026-11-18 实测：108 个死号把主循环拖到 45~60s 一轮）。
	cctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	access, refresh, err := s.up.SignInSession(upstream.WithAccount(cctx, name), a.Email, a.Password)
	if err != nil {
		s.noteRefreshErr(name, err)
		// 401（密码错）/ 400（未激活、封禁）是**永久**失败：再试一万次也一样，
		// 只会白吃 WAF 并一直占着重登队列最前面。403 除外 —— 那多半是 WAF
		// 挑战页，退避一轮再来才是对的。
		if upstream.KindOf(err) == upstream.KindAuth && upstream.StatusOf(err) != 403 {
			s.pool.MarkAuth(a, "signin: "+err.Error())
			log.Printf("[qwen2api] Qwen отклонил повторный вход. Аккаунт %s отключён: %v", name, err)
		}
		return false
	}
	s.pool.SetToken(name, access)
	if refresh != "" {
		s.pool.SetRefresh(name, refresh)
	}
	s.clearRefreshErr(name) // 修好了就别留着上一次的失败时间戳
	// 成功必须留痕：否则「自愈」是静默的，出问题时分不清「还没轮到它」和
	// 「早已修好」，只剩一条失败日志可看。
	log.Printf("[qwen2api] Аккаунт %s: фоновый вход выполнен, сессия обновлена", name)
	return true
}

// warmBatch 每轮预热多少个 —— 跟着目标数一起缩放。
//
// 两个约束同时要满足：
//  1. **热满要快**：取 target/8 让热满耗时稳定在约 8 轮，与规模无关
//     （原来是常量 8，并发一上来热满 512 个要 21 分钟，期间请求全在撞冷号）；
//  2. **保活要跟上连接 TTL**：连接只活 90 秒，一轮必须走够 target×tick/90 个，
//     否则先保活的那批早就被回收了 —— 这一轮等于白跑。
//
// 下限 4 保证低流量时也能热起来，上限走配置（batch_max，默认 48）。
func warmBatch(target, max, tickS int) int {
	if max <= 0 {
		max = 48
	}
	if tickS <= 0 {
		tickS = 20
	}
	b := target / 8
	// 连接只活 TTL，一轮走不完就得在 TTL 内轮一遍：
	// 一个 TTL 里能跑 rounds 轮，每轮至少要覆盖 ceil(target/rounds) 个。
	ttlS := int(upstream.IdleConnTimeout / time.Second)
	rounds := ttlS / tickS
	if rounds < 1 {
		rounds = 1
	}
	if connNeed := (target + rounds - 1) / rounds; connNeed > b {
		b = connNeed
	}
	if b < 4 {
		b = 4
	}
	if b > max {
		b = max
	}
	return b
}

// warmTarget 估算"要随时保持可用的账号数"（token 口径）。
//
// 每个 access 活 15 分钟，所以速率为 r 次/分钟时需要 r*15 个可用凭据。
// 用最近 10 分钟的实际请求数推算，低流量时只热几个号（不做无用续期）。
//
// 并发兜底（2026-11-02）：只用速率会漏掉"短时间打满"的情况 —— 10 分钟内
// 只有 20 个请求，但全挤在 3 秒里，那 20 个号必须同时可用。所以再拿
// 上一轮的并发峰值算一遍，取较大者。
func (s *Server) warmTarget(peak int) int {
	ka := s.conf().Pool.Keepalive.Effective()
	const lookback = 10 * time.Minute
	cut := time.Now().Add(-lookback)
	n := 0
	// Recent(0) = 取满整个环形缓冲（容量 500）。写死 200 会在高流量时低估速率。
	for _, e := range s.log.Recent(0) {
		if e.Time.After(cut) {
			n++
		}
	}
	// 采样窗口可能被截断，按"至少 1 次"算，避免 0 导致不预热。
	rate := float64(n) / lookback.Minutes()
	target := int(rate*15*1.5) + 8
	if need := peak*ka.ConcFactor + 4; need > target {
		target = need
	}
	if target < 8 {
		target = 8
	}
	// 空闲期下限（默认 0 = 纯按流量算）。设大可以避免"突发第一个请求撞冷启动"。
	if min := ka.TargetMin; min > target {
		target = min
	}
	// 上限不是拍脑袋：热满时间必须 < access TTL，否则刚热完就开始过期。
	if target > ka.TargetMax {
		target = ka.TargetMax
	}
	return target
}

// chatTarget 会话/连接要覆盖多少个号。
//
// 🔴 与 warmTarget 分开（2026-11-02）：token 有 15 分钟 TTL，必须提前热一大片；
// 但空会话和出口连接只服务"同时在跑的请求" —— 并发 5 的场景下保活 512 条连接，
// 等于每 20 秒白打 500 次代理往返（每次 0.4~2 秒），把上游和代理都打热。
//
// 所以按并发峰值算，再用 token 目标封顶（不会超过"实际会用到的那批"）。
func (s *Server) chatTarget(peak, tokenTarget int) int {
	ka := s.conf().Pool.Keepalive.Effective()
	n := peak*ka.ConcFactor + 4
	if n < 4 {
		n = 4
	}
	if n > tokenTarget {
		n = tokenTarget
	}
	return n
}

// warmOnce 跑一轮 access token 续期。
func (s *Server) warmOnce(ctx context.Context, peak int) bool {
	return s.warmOnceCapped(ctx, peak, 0)
}

// cappedBatch 把批量压到 cap（cap<=0 = 不限制）。启动斜坡的唯一算法。
func cappedBatch(batch, cap int) int {
	if cap > 0 && batch > cap {
		return cap
	}
	return batch
}

// warmOnceCapped 预热一轮；cap>0 时把本轮批量压到 cap（启动斜坡用）。
func (s *Server) warmOnceCapped(ctx context.Context, peak int, cap int) bool {
	ka := s.conf().Pool.Keepalive.Effective()
	target := s.warmTarget(peak)
	batch := cappedBatch(warmBatch(target, ka.BatchMax, ka.TickS), cap)
	window := time.Duration(ka.TokenWindowS) * time.Second
	names := s.pool.WarmCandidates(target, window, batch)
	// 即使候选为空也要记：否则 /stats 的 warm.target 永远停在 0（观测失真）。
	s.noteWarm(target, batch, len(names), peak)
	if len(names) == 0 {
		return false
	}
	sem := make(chan struct{}, ka.Parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	changed := false
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if s.refreshTokenOnly(ctx, name) {
				mu.Lock()
				changed = true
				mu.Unlock()
			}
		}(n)
	}
	wg.Wait()
	return changed
}

// prewarmChats 给"马上要被用到的那批号"预建空会话，并把它们的出口连接保活。
//
// 覆盖范围按**选号面**算（warmTarget），不是按并发（chatTarget）——
// 这条是 2026-11-12 用线上数据纠正的：低流量下号轮转周期远大于连接寿命，
// 按并发保活等于让每个请求都重新握一次 SOCKS5+TLS。
// 详见 prewarmChats 里的注释。
//
// 池子已经满的号不会重复建会话（那是白花一次上游调用），但会发一个 HEAD 保住
// TCP+TLS —— 实测走住宅代理重新握手要 1.7 秒，比建会话便宜得多，收益却一样直接。
//
// 只补 t2t：对话占请求的绝大多数；t2i / image_edit / t2v 一次就要 50~90 秒，
// 建会话那 2.4s 占比可以忽略，不值得为它们维护池子。
func (s *Server) prewarmChats(ctx context.Context, peak int) {
	if !s.chats.Enabled() {
		// 池被关掉（per=0）⇒ 没有任何号"手里有货"。不清标志的话，选号会一直
		// 优先撞那些残留标志为 true 的号，而它们每次都要现建 3 秒。
		s.pool.SetChatReady(nil)
		return
	}
	if n := s.chats.Trim(); n > 0 {
		log.Printf("[qwen2api] Удалено просроченных подготовленных диалогов: %d", n)
	}
	ka := s.conf().Pool.Keepalive.Effective()
	// 🔴 need 用 warmTarget，不是 chatTarget（2026-11-12 线上实测修正）。
	//
	// chatTarget 的原始论证是"会话和连接只服务同时在跑的请求"，它隐含假设
	// **选号面 = 并发**。但这个假设不成立：选号面是"token 还活着的号"
	// （实测 139 个，而峰值并发只有 1~2）。低流量下同一个号要几十分钟才轮到
	// 一次，连接早被回收 —— 于是每次请求都在付一次 SOCKS5+TLS。日志里能直接
	// 看到：同一个号第一次 submit 6024ms、第二次 984ms。
	//
	// 保活面必须覆盖选号面，否则"省下来的保活"会以"每次多 2~5 秒首字"
	// 的形式十倍还回去。
	need := s.warmTarget(peak)
	// 换批后的重叠期：旧窗口的号还在服务，它们的会话不能立刻撤 ——
	// 覆盖范围翻倍，让新旧两批都有人管。等旧票过期后 HotNames 自然只剩
	// 新窗口那么多号，这里多出来的额度不会被浪费。
	if s.pool.HotRotating() {
		need *= 2
	}
	s.chatTargetNow.Store(int32(need))
	names := s.pool.HotNames(need)
	if len(names) == 0 {
		return
	}
	// 把"谁手里有货 **且本轮会被保活**"同步给选号（见 pool.SetChatReady）。
	//
	// 🔴 必须取交集，不能直接拿 ReadyAccounts：会话池里会残留"上一轮预热建的、
	// 但已经不在热点集里"的号（实测 ready=116 而保活面只有 80）。它们的会话是
	// 现成的，chatReady 一置真，选号就优先挑它们 —— 但它们的**连接没人管**，
	// 于是照样要付 2~5 秒握手。表现就是"命中率 99% 但 submit 还是 2.8 秒"。
	//
	// 交集之后：有货 ⇒ 一定在热点集 ⇒ 连接被游标保活。"命中"才真正等于"热"。
	ready := capReadyToHot(s.chats.ReadyAccounts(), names)
	// 位置在 Trim 之后：刚清掉的那批必须立刻从标志里消失。
	// 本轮后续补货写入时会单点置真（见 prewarmChat），不必等下一轮。
	s.pool.SetChatReady(ready)
	limit := warmBatch(need, ka.BatchMax, ka.TickS)
	// 按游标取一段，而不是"每轮都从头取 limit 个"：
	// HotNames 的排序键（alive / served / tokenExp）不会被预热自己改写，
	// 不轮转的话第一轮的前 8 个就是永远的前 8 个，其余号的连接永远冷着。
	names = s.rotateHot(names, limit)
	sem := make(chan struct{}, ka.Parallel)
	var wg sync.WaitGroup
	for _, n := range names {
		// 建会话要凭据：过期的号建了也会被上游拒（且白占一次代理往返）。
		// 但**连接保活不需要凭据**，所以过期号照样往下走 —— 下次请求用到它时
		// 请求路径会先续期，那条已经握好的连接正好复用。
		needChat := s.chats.Short(n, "t2t") > 0 && s.tokenFresh(n, time.Minute)
		wg.Add(1)
		go func(name string, needChat bool) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if needChat {
				cred := s.pool.Credential(name)
				if cred == "" {
					return
				}
				s.prewarmChat(ctx, name, cred, s.conf().DefaultModel, "t2t")
				return
			}
			// 这条号马上会被用到，保住它的出口连接就够了（实测省 1.7 秒握手）。
			_ = s.up.WarmConn(upstream.WithAccount(ctx, name))
		}(n, needChat)
	}
	wg.Wait()
}

// capReadyToHot 把"有预建会话"限制在"本轮会被保活"的号内。
//
// 为什么必须相交（2026-11-12 线上实测）：会话池会残留上一轮预热建的、但已经
// 不在热点集里的号（实测 ready=116，保活面只有 80）。它们的会话是现成的，
// chatReady 一置真，选号就优先挑它们 —— 但它们的连接没人管，照样要付
// 2~5 秒握手。表现就是"命中率 99% 却 submit 2.8 秒"。
//
// 相交之后语义才闭合：**有货 ⇒ 一定在热点集 ⇒ 连接被游标保活**，
// 于是"命中"同时等于"会话热"和"连接热"。
func capReadyToHot(ready map[string]struct{}, hot []string) map[string]struct{} {
	keep := make(map[string]struct{}, len(hot))
	for _, n := range hot {
		keep[n] = struct{}{}
	}
	for name := range ready {
		if _, ok := keep[name]; !ok {
			delete(ready, name)
		}
	}
	return ready
}

// rotateHot 从 names 里按游标环形取 limit 个。
//
// 存在的理由：HotNames 的排序是 (alive, served, tokenExp)，而预热本身不改这三个值。
// 如果每轮都取前 limit 个，头部那批会被反复热身，尾部的号永远轮不到 ——
// 偏偏选号（lessPick）也用同一套排序，于是 served 一旦摊开，请求打到的就是
// 一批从没保活过的冷号。游标让"多轮下来每个号都被轮到"，与 need 一起保证
// 一个 TTL 内覆盖完整个选号面。
func (s *Server) rotateHot(names []string, limit int) []string {
	if limit <= 0 || limit >= len(names) {
		return names
	}
	off := int(s.chatCursor.Load()) % len(names)
	out := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		out = append(out, names[(off+i)%len(names)])
	}
	s.chatCursor.Store(uint32((off + limit) % len(names)))
	return out
}

// noteInflight 记录一次 chat 请求进出，并维护"上一轮预热以来的并发峰值"。
//
// 用 CAS 而不是加锁：每个请求只进来一次，锁会成为高频请求下的争用点。
func (s *Server) noteInflight(delta int32) {
	if s == nil {
		return
	}
	n := s.inflightNow.Add(delta)
	if delta <= 0 || n <= 0 {
		return
	}
	for {
		p := s.inflightPeak.Load()
		if n <= p || s.inflightPeak.CompareAndSwap(p, n) {
			return
		}
	}
}

// noteWarm 记录最近一轮预热的参数（/stats 与面板观测用）。
func (s *Server) noteWarm(target, batch, actual, peak int) {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	s.warmStat = WarmStat{
		Target: target, Batch: batch, Actual: actual, Peak: peak, At: time.Now(),
	}
}

// WarmStat 最近一轮预热的参数快照。
type WarmStat struct {
	Target int       `json:"target"` // 估算需要保持可用的账号数（token 口径）
	Batch  int       `json:"batch"`  // 本轮取了几个候选
	Actual int       `json:"actual"` // 实际取到的（候选不足时会小于 batch）
	Peak   int       `json:"peak"`   // 本轮用到的并发峰值（会话/连接按它算）
	At     time.Time `json:"at"`
}

// WarmSnapshot 最近一轮预热参数。
func (s *Server) WarmSnapshot() WarmStat {
	s.warmMu.Lock()
	defer s.warmMu.Unlock()
	return s.warmStat
}

// refreshTokenOnly 只用 refresh_token 换新（不碰密码）。
//
// 失败分两种，处置必须不同 —— 这是 2026-11-02 那次"timeout 之后紧接着 401
// 令牌已撤销"暴露出来的：
//
//   - **明确失效**（HTTP 401）：这张票已经不能用了，清掉它。AdoptCandidates 只挑
//     "没有 refresh_token"的号，清空是唯一能让后台用密码补一张的入口。留着它只会
//     让预热每 20s 撞一次 401（日志被 5 分钟去重，看着像"偶发"，其实一直在打）。
//   - **结果未知**（超时 / 网络）：HTTP 超时 ≠ 没执行，上游很可能已经消费掉这张
//     票。所以既不拿它原地重试，也不立刻重登（可能白花一次最容易吃 WAF 的
//     signin），先静默 refreshRetryBackoff 再说。
//
// ⚠️ 清空不等于"马上修复"：这个号会退出预热队列，修复时机交回两处 ——
// 后台 adoptOnce（只补"接下来要用"的那批）或请求路径上的 relogin（真正要用时）。
// 这样不会为了一个冷号白花密码登录，也不会让它继续污染 auth 域。
func (s *Server) refreshTokenOnly(ctx context.Context, name string) bool {
	unlock := s.lockRefresh(name)
	defer unlock()
	// 拿到锁后复查：等锁这一小会儿里，请求路径可能已经把它续好了。
	// 再换一张纯属浪费 —— 每多换一次就多一次撞 401「令牌已撤销」的机会。
	ka := s.conf().Pool.Keepalive.Effective()
	if s.tokenFresh(name, time.Duration(ka.TokenWindowS)*time.Second) {
		return false
	}
	rt := s.pool.RefreshToken(name)
	if rt == "" {
		return false
	}
	if !s.refreshDue(name, time.Now()) {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second) // 必须比 doRefresh 的 25s 长
	defer cancel()
	access, next, err := s.up.RefreshWithToken(upstream.WithAccount(cctx, name), rt)
	if err != nil {
		// 打印 rt 指纹（前 8 + 后 6）：用来判断失败是「票旧」还是「出口不对」。
		// 与 accounts.json 里的值比对即可 —— 如果指纹不同，说明内存里的票
		// 是上一次 refresh 换下来、却没落盘的那张（saveIfDue 有 2 分钟节流）。
		fp := rt
		if len(fp) > 16 {
			fp = fp[:8] + "…" + fp[len(fp)-6:]
		}
		log.Printf("[qwen2api] %s: ошибка обновления сессии, метка токена %s (%d байт): %v", name, fp, len(rt), err)
		s.noteRefreshErr(name, err)
		s.afterRefreshFailure(name, err)
		return false
	}
	s.pool.SetToken(name, upstream.ModeBearer+":"+access)
	if next != "" {
		s.pool.SetRefresh(name, next)
	}
	s.clearRefreshErr(name)
	return true
}

// lockRefresh 按账号名取一把分片锁，返回解锁函数。
//
// 为什么需要它：同一个号可能被三条路径同时换票 —— 预热的 refresh、请求路径的
// relogin、后台的 signin。并发换票会让一方刚拿到的票被另一方作废（实测
// 「timeout 之后紧接着 401 令牌已撤销」的嫌疑来源），而每次多余失败都要用
// 密码 signin 收尾 —— 那正是最贵、最该避免的一步。
//
// 用固定分片而不是 per-account 的 map：3135 个号各一把锁要常驻一张表，
// 而真实并发换票只有个位数，64 片几乎不会碰撞。
func (s *Server) lockRefresh(name string) func() {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	mu := &s.refreshLocks[h.Sum32()%refreshLockShards]
	mu.Lock()
	return mu.Unlock
}

// afterRefreshFailure 按错误种类决定这个号下一步怎么办。
//
// 只对 401 清票：403 在 auth 域通常是 WAF/风控挑战（KindAuth 但票没坏），
// 清掉反而会把一个还能用的号推去密码重登 —— 那才是最容易被拦的一步。
func (s *Server) afterRefreshFailure(name string, err error) {
	if upstream.KindOf(err) != upstream.KindAuth || upstream.StatusOf(err) != 401 {
		return // 其余情况只退避（时间戳已由 noteRefreshErr 记下）
	}
	if s.pool.SetRefresh(name, "") {
		// 没票又没密码 = 已经没有任何续期手段。留在池里只会显示"可用"、
		// 却永远不被选中（lessPick 把它排最后），是最难排查的那种僵尸号。
		if a := s.pool.Get(name); a != nil && a.Password == "" {
			s.pool.MarkAuth(a, "refresh_token 已撤销，且未保存密码，无法重登")
			log.Printf("[qwen2api] Аккаунт %s отключён: токен обновления отозван, пароль не сохранён", name)
			return
		}
		// 打上 needLogin：AdoptCandidates 会把它插到队列最前面（趁请求还没
		// 碰到就修好），lessPick 也会把它排到最后（请求路径不再撞上几秒的 signin）。
		s.pool.MarkNeedLogin(name)
		// 🔴 401 是**确定性**失败：票已经清空，不存在「再撞同一张死票」的风险，
		// 退避在这里没有防护价值 —— 留着它只会把唯一有效的修复动作（密码重登）
		// 推迟满 5 分钟。实测 2026-11-13 17:06 一批 15 个号同时被撤销，最早的重登
		// 尝试落在 5m21s 后、最晚 7m49s 后，这段停摆本可以只有一个 tick（20s）。
		s.clearRefreshErr(name)
		log.Printf("[qwen2api] Аккаунт %s: истёк токен обновления (401), запланирован повторный вход", name)
	}
}

// refreshDue 上次失败是否已过静默期（见 refreshRetryBackoff）。
//
// 复用 creditErrMu：这张表只在预热路径上读写，频率极低，不值得再加一把锁。
func (s *Server) refreshDue(name string, now time.Time) bool {
	s.creditErrMu.Lock()
	defer s.creditErrMu.Unlock()
	last, ok := s.refreshErrAt[name]
	return !ok || now.Sub(last) >= refreshRetryBackoff
}

// clearRefreshErr 续期成功后抹掉失败记录，避免静默期把这个号白挡一轮。
func (s *Server) clearRefreshErr(name string) {
	s.creditErrMu.Lock()
	delete(s.refreshErrAt, name)
	s.creditErrMu.Unlock()
}

// noteRefreshErr 记录续期失败：一份数据两个用途 —— 日志去重 + 重试退避。
//
// 3000 个号同时失败时，逐条打日志会把日志刷爆，反而看不到别的问题；
// 而"上次失败是什么时候"正好也是 refreshDue 需要的退避起点，不必另开状态。
func (s *Server) noteRefreshErr(name string, err error) {
	s.creditErrMu.Lock()
	last, ok := s.refreshErrAt[name]
	now := time.Now()
	if ok && now.Sub(last) < 5*time.Minute {
		s.creditErrMu.Unlock()
		return
	}
	if s.refreshErrAt == nil {
		s.refreshErrAt = map[string]time.Time{}
	}
	s.refreshErrAt[name] = now
	s.creditErrMu.Unlock()
	log.Printf("[qwen2api] Аккаунт %s: не удалось продлить сессию: %v", name, err)
}

// logCapacity 打印一行容量快照。
//
// 目的只有一个：**事后能回答"从什么时候开始不够用的"**。
// 关键指标是 token_fresh / warmTarget —— 它低于一半就说明 signin 补不过来了，
// 那时候吞吐会先掉、然后开始 503。
func (s *Server) logCapacity(peak int) {
	ka := s.conf().Pool.Keepalive.Effective()
	target := s.warmTarget(peak)
	if min := ka.TargetMin; min > target {
		target = min
	}
	fresh := s.pool.TokenFreshCount()
	cps := s.chats.Stat()
	ready := cps.Ready

	// 最近 5 分钟的请求量与失败量
	cut := time.Now().Add(-capTick)
	var reqN, errN, s503 int
	for _, e := range s.log.Recent(0) {
		if !e.Time.After(cut) {
			continue
		}
		reqN++
		if e.Status >= 400 {
			errN++
		}
		if e.Status == 503 {
			s503++
		}
	}

	tag := ""
	if fresh*2 < target {
		tag = " WARN=token_fresh_low"
	}
	if s503 > 0 {
		tag += " WARN=503"
	}
	if errN*10 > reqN && reqN > 0 {
		tag += " WARN=err_rate_high"
	}
	rate := cps.HitPct
	log.Printf("[capacity] token_fresh=%d/%d chat_ready=%d hit=%.1f%% inflight=%d req5m=%d err5m=%d s503=%d%s",
		fresh, target, ready, rate, peak, reqN, errN, s503, tag)
}
