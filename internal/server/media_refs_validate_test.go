package server

// 参考图载体的准入校验。
//
// 这组用例来自 2026-11-19 的 image-adapter 探针实测：截断的 PNG 魔数完好，
// 嗅探放行 → 坏图被上传 → 上游才报 image_edit_input_unreadable，
// 而网关把它包成 502「稍后重试」，一条请求烧掉一个账号。
// 所以断言点必须落在「本地拒绝 + 归因客户端」上，而不是「上游报错后返回什么」。

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// probeTruncatedPNG 是 2026-11-19 探针实际使用的那一段坏图（45 字节）：
// PNG 头完整、IDAT 被截断。Go 报 "not enough pixel data"。
const probeTruncatedPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk"

func TestResolveImageRefRejectsTruncatedImage(t *testing.T) {
	enc := base64.StdEncoding.EncodeToString
	raw, err := base64.StdEncoding.DecodeString(probeTruncatedPNG)
	if err != nil {
		t.Fatalf("夹具自身不是合法 base64: %v", err)
	}

	// 反向对照 1：合法 PNG 必须通过。
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := resolveImageRef(context.Background(), "data:image/png;base64,"+enc(buf.Bytes())); err != nil {
		t.Fatalf("合法 PNG 被拒: %v", err)
	}

	// 反向对照 2：校验和有瑕疵但**能渲染**的图必须放行。
	// 这个常量 macOS ImageIO 能读出 1×1，Go 报 zlib invalid checksum ——
	// 拿 Go 的严格性当准入门会把真实可用的图一起拒掉。
	if _, _, _, err := resolveImageRef(context.Background(), "data:image/png;base64,"+onePixelPNG); err != nil {
		t.Fatalf("可渲染但 checksum 有瑕疵的 PNG 被误杀: %v", err)
	}

	// 截断图：必须本地拒绝，且归因是客户端输入（⇒ 400、不换号、不罚账号）。
	for name, ref := range map[string]string{
		"data URI":  "data:image/png;base64," + probeTruncatedPNG,
		"裸 base64": probeTruncatedPNG,
	} {
		_, _, _, err := resolveImageRef(context.Background(), ref)
		if err == nil {
			t.Fatalf("%s：截断图应被拒（这正是探针实测到的漏口，raw=%d 字节）", name, len(raw))
		}
		if got := upstream.KindOf(err); got != upstream.KindBadInput {
			t.Fatalf("%s：应归因为 bad_input（⇒ 400 且不换号），得到 %v（err=%v）", name, got, err)
		}
		if upstream.RetrySafe(err) {
			t.Fatalf("%s：坏图不该换号重试", name)
		}
	}

	// 既不是 base64 也不是 URL：同样必须归到客户端输入错误。
	if _, _, _, err := resolveImageRef(context.Background(), "!!!not-base64!!!"); upstream.KindOf(err) != upstream.KindBadInput {
		t.Fatalf("非法引用应归因为 bad_input，得到 %v（err=%v）", upstream.KindOf(err), err)
	}
}

// webp 没有标准库解码器：不能因为「解不出来」就拒掉合法 webp。
// 这里用一个只有 RIFF/WEBP 头的样本，断言它仍然被放行（靠魔数），
// 把「解码器缺失」与「图确实坏了」分开。
func TestValidateImageBytesSkipsFormatsWithoutDecoder(t *testing.T) {
	webp := make([]byte, 32)
	copy(webp[0:4], "RIFF")
	copy(webp[8:12], "WEBP")
	if err := validateImageBytes(webp, "image/webp"); err != nil {
		t.Fatalf("webp 不该被解码器缺失误杀: %v", err)
	}
}
