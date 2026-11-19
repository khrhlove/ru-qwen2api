package server

// 图片 / 视频出口的契约一致性用例。
//
// 这一组钉的不是「跑得通」，而是三件容易悄悄退化的事：
//  1. 收下但没生效的字段必须进 X-Qwen2API-Warnings（image-adapter 不变量 2）；
//  2. watermark 在图片与视频两条路上同语义（true = 保留水印）；
//  3. 任务对象的 content 两个键在非成功态也必须存在。

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

func warnList(t *testing.T, h string) []string {
	t.Helper()
	if h == "" {
		return nil
	}
	s, err := url.QueryUnescape(h)
	if err != nil {
		t.Fatalf("警告头不是 URL 编码: %v", err)
	}
	return strings.Split(s, "\n")
}

func hasWarnPrefix(ws []string, prefix string) bool {
	for _, w := range ws {
		if strings.HasPrefix(w, prefix) {
			return true
		}
	}
	return false
}

func TestImageWarningsSurfaceLostFields(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	// 精确比例 + 无额外字段 ⇒ 一条警告都没有（不能把正常请求也变成「有降级」）。
	clean := &ImageRequest{Prompt: "a cat", Size: "1920x1080"}
	if ws := imageWarnings(clean, "16:9", true); len(ws) != 0 {
		t.Fatalf("干净请求不应有警告，得到 %v", ws)
	}

	// 像素吸附：3:2 没有对应档位，必须告知构图会变。
	snap := &ImageRequest{Prompt: "a cat", Size: "1536x1024"}
	ws := imageWarnings(snap, "16:9", true)
	if !hasWarnPrefix(ws, `size="1536x1024" 已按最接近比例吸附为 16:9`) {
		t.Fatalf("吸附缺失警告: %v", ws)
	}

	// 比例写法无法映射 ⇒ 交给上游默认，也要说。
	unmapped := &ImageRequest{Prompt: "a cat", Size: "3:2"}
	ws = imageWarnings(unmapped, "", true)
	if !hasWarnPrefix(ws, `size="3:2" 无法映射`) {
		t.Fatalf("无法映射缺失警告: %v", ws)
	}

	// mask 带 image 时收下但不转发 —— image-adapter 点名的反模式，必须可见。
	masked := &ImageRequest{Prompt: "a cat", Mask: "https://x/m.png"}
	ws = imageWarnings(masked, "1:1", true)
	if !hasWarnPrefix(ws, "mask 已收下但未生效") {
		t.Fatalf("mask 缺失警告: %v", ws)
	}

	// quality=auto 是显式默认，不该刷警告；high 必须警告。
	if ws := imageWarnings(&ImageRequest{Prompt: "x", Quality: "auto"}, "", true); len(ws) != 0 {
		t.Fatalf("quality=auto 不该警告: %v", ws)
	}
	if ws := imageWarnings(&ImageRequest{Prompt: "x", Quality: "high"}, "", true); !hasWarnPrefix(ws, `quality="high" 未生效`) {
		t.Fatalf("quality 缺失警告: %v", ws)
	}

	// 未知键（方舟扩展 / vendor extras）一律显式列出。
	extra := &ImageRequest{Prompt: "x", Extras: map[string]any{"seed": 1, "background": "opaque"}}
	ws = imageWarnings(extra, "", true)
	if !hasWarnPrefix(ws, `字段 "background"`) || !hasWarnPrefix(ws, `字段 "seed"`) {
		t.Fatalf("未知键缺失警告: %v", ws)
	}
	// 顺序必须稳定（map 迭代无序会让客户端日志跳动）。
	if ws[0] != `字段 "background" 在本上游没有对应实现，未生效` {
		t.Fatalf("未知键警告未排序: %v", ws)
	}

	// watermark=false 但去水印没启用 ⇒ 承诺兑现不了。
	ws = imageWarnings(&ImageRequest{Prompt: "x", Watermark: boolPtr(false)}, "", false)
	if !hasWarnPrefix(ws, "watermark=false 无法兑现") {
		t.Fatalf("水印不可兑现缺失警告: %v", ws)
	}
	// 反过来：要水印时不需要 strip，不该有这条。
	ws = imageWarnings(&ImageRequest{Prompt: "x", Watermark: boolPtr(true)}, "", false)
	if hasWarnPrefix(ws, "watermark=false 无法兑现") {
		t.Fatalf("watermark=true 不该报不可兑现: %v", ws)
	}
}

func TestMediaWarningsHeaderRoundTrip(t *testing.T) {
	rec := httptest.NewRecorder()
	setMediaWarnings(rec, []string{"第一条 中文", "第二条"})
	if got := rec.Header().Get("X-Qwen2API-Warnings"); got == "" {
		t.Fatal("没有写出警告头")
	}
	got := warnList(t, rec.Header().Get("X-Qwen2API-Warnings"))
	if len(got) != 2 || got[0] != "第一条 中文" || got[1] != "第二条" {
		t.Fatalf("警告头往返失败: %#v", got)
	}
}

// 视频任务对象：非成功态也必须有两个键。
//
// 按「键是否存在」判定的客户端（含强类型反序列化到 map 的实现）在
// running → succeeded 的切换里会踩空，而那一刻恰好是最需要读 content 的时候。
func TestVideoPayloadContentKeysAlwaysPresent(t *testing.T) {
	for _, st := range []string{VideoStatusQueued, VideoStatusRunning, VideoStatusFailed, VideoStatusPolishing} {
		rec := &videoRecord{
			ID: "cgt-20261119220000-abcdef12", Model: "happyhorse11",
			Status: st, CreatedAt: 1, UpdatedAt: 2, Credits: 30,
		}
		p := videoTaskPayload(rec)
		content, ok := p["content"].(map[string]any)
		if !ok {
			t.Fatalf("%s: content 不是对象: %#v", st, p["content"])
		}
		if _, ok := content["video_url"]; !ok {
			t.Fatalf("%s: 缺 video_url 键", st)
		}
		if _, ok := content["last_frame_url"]; !ok {
			t.Fatalf("%s: 缺 last_frame_url 键", st)
		}
		if content["video_url"] != nil {
			t.Fatalf("%s: 非成功态 video_url 必须为 null", st)
		}
	}
}

// edits 是 multipart，watermark 走表单字段而不是 JSON —— 解析口径要能区分
// 「没传」与「传了个认不出的值」：前者用默认，后者也不能把请求否掉。
func TestFormBoolReadsMultipartAndDefaults(t *testing.T) {
	mk := func(v string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/images/edits",
			strings.NewReader("watermark="+v))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return r
	}
	if v, set := formBool(mk("true"), "watermark"); !set || !v {
		t.Fatalf("true 应解析为 (true,true)，得到 (%v,%v)", v, set)
	}
	if v, set := formBool(mk("0"), "watermark"); !set || v {
		t.Fatalf("0 应解析为 (false,true)，得到 (%v,%v)", v, set)
	}
	if v, set := formBool(mk(""), "watermark"); set || v {
		t.Fatalf("空值应视为未指定，得到 (%v,%v)", v, set)
	}
	if v, set := formBool(mk("maybe"), "watermark"); set || v {
		t.Fatalf("认不出的值应视为未指定，得到 (%v,%v)", v, set)
	}
}

// newImageServerWithStorage 与 newImageServer 同构，但托管已启用且 endpoint 不可达。
//
// 目的是制造一个「后处理一定失败」的环境：这样「成功」就只可能来自
// **真的跳过了后处理**，而不是恰好没配存储。
func newImageServerWithStorage(t *testing.T, up *imageUpstream) http.Handler {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	cfg.ImageModel = "qwen-image-3.0"
	cfg.Storage = config.Storage{
		Endpoint: "http://127.0.0.1:1", Bucket: "b",
		AccessKey: "k", SecretKey: "s",
		ImageStripWatermark: true,
	}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	srv := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(10), "test")
	return srv.Handler()
}

// 图片的 watermark 必须真的改变交付路径，而不只是解析字段。
//
// 对照法：同一环境下（托管开启、产物域不可解析）
//   - 缺省（= 去水印）必须失败 —— 它真的去下载了；
//   - watermark=true 必须成功且回上游原链 —— 它跳过了下载。
//
// 只断言「200」排除不了「polish 被静默跳过」这种情况，所以必须两个一起看。
func TestImagesWatermarkTrueSkipsPolish(t *testing.T) {
	up := newImageUpstream(t)
	h := newImageServerWithStorage(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a cat"}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("缺省路径应去水印并因产物不可下载而失败，却成功了（后处理被跳过？）: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/images/generations",
		strings.NewReader(`{"prompt":"a cat","watermark":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("watermark=true 应跳过下载并成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cdn.example/img.png") {
		t.Fatalf("watermark=true 应原样交付上游地址: %s", rec.Body.String())
	}
}
