package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// badImageRef 参考图本身不可用 —— 客户端的输入问题，不是上游故障。
//
// 🔴 归到 KindBadInput 一把决定三件事：400（而不是 502）、不换号重试、
// 不给账号记失败。早先这类错误是裸 error，一路落到 502 + 换号，
// 于是一个坏 base64 会逐个号试过去，而客户端被告知「稍后重试」。
func badImageRef(detail string) error {
	return &upstream.Error{Kind: upstream.KindBadInput, Op: "参考图", Detail: detail}
}

// validateImageBytes 完整解码一次参考图，确认它不是「头部合法、数据残缺」的坏图。
//
// 🔴 只嗅魔数不够：截断的 PNG 前 8 字节完全合法，嗅探一路放行，坏图被上传成
// blob 交给上游，上游才报 image_edit_input_unreadable。那时会话已经建好、
// 号已经占了，而错误被包装成 502「稍后重试」—— 客户端只会拿同一张坏图重试。
// 2026-11-19 探针实测：13 条坏图请求消耗了 13 个不同账号。
//
// 只对**本地有解码器**的格式做完整解码（png/jpeg/gif）。webp 没有标准库
// 解码器，「解不出来就拒」会把合法 webp 一起误杀 —— 那种情况宁可放行。
//
// 🔴 判据是「**渲染不了**」，不是「Go 解不出来」：
//   - 拦：不是图片（unknown format）、数据被截断（not enough pixel data / unexpected EOF）
//   - 放：校验和有瑕疵但结构完整 —— macOS ImageIO 能正常渲染，只是 Go 更严格
//     （实测：社区通用的 1x1 PNG 常量 sips 读出 1×1，Go 报 zlib invalid checksum）。
// 拿"解不出来"当准入门会把这类真实可用的图一起拒掉，那是另一种说谎。
func validateImageBytes(raw []byte, mime string) error {
	switch mime {
	case "image/png", "image/jpeg", "image/gif":
	default:
		return nil
	}
	if _, _, err := image.Decode(bytes.NewReader(raw)); err != nil && unrenderableImage(err) {
		return badImageRef("图片无法渲染（不是图片或数据被截断）: " + err.Error())
	}
	return nil
}

// unrenderableImage 判断解码错误是「这张图根本画不出来」还是「只是不够干净」。
//
// 只认结构性缺失：非图片、像素数据不足、流被截断。
// checksum / CRC 类错误不在此列 —— 它们不影响渲染，只影响严格性。
func unrenderableImage(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, bad := range []string{
		"unknown format",
		"not enough pixel data",
		"unexpected eof",
		"unsupported format",
	} {
		if strings.Contains(msg, bad) {
			return true
		}
	}
	return false
}

// resolveImageRef 把 `image` 字段的三种形态统一解成字节。
//
//   - `data:image/png;base64,…` → 直接解码（mime 从 data URI 头取）
//   - `http(s)://…`            → 下载（带浏览器 UA：CDN 的 WAF 会把 curl 类 UA 挡成 404）
//   - 裸 base64                → 解码后按魔数嗅探 mime（SDK 常把 PNG 声明成 octet-stream）
//
// 三种形态都要收，是 image-adapter 的既定契约（`image` 支持 URL / data URI / 裸 base64）。
func resolveImageRef(ctx context.Context, ref string) (data []byte, mime, filename string, err error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, "", "", badImageRef("空的图片引用")
	}

	if strings.HasPrefix(ref, "data:") {
		comma := strings.IndexByte(ref, ',')
		if comma < 0 {
			return nil, "", "", errors.New("data URI 缺少逗号")
		}
		head := ref[len("data:"):comma]
		mime = "image/png"
		if i := strings.IndexByte(head, ';'); i >= 0 {
			if head[:i] != "" {
				mime = head[:i]
			}
		}
		if !strings.Contains(head, "base64") {
			return nil, "", "", errors.New("只支持 base64 编码的 data URI")
		}
		raw, decErr := base64.StdEncoding.DecodeString(ref[comma+1:])
		if decErr != nil {
			return nil, "", "", badImageRef("data URI 的 base64 解码失败: " + decErr.Error())
		}
		if vErr := validateImageBytes(raw, mime); vErr != nil {
			return nil, "", "", vErr
		}
		return raw, mime, "ref" + extForMime(mime), nil
	}

	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		raw, dlErr := fetchMedia(ctx, ref)
		if dlErr != nil {
			return nil, "", "", dlErr
		}
		m := sniffMime(raw)
		if vErr := validateImageBytes(raw, m); vErr != nil {
			return nil, "", "", vErr
		}
		return raw, m, "ref" + extForMime(m), nil
	}

	// 裸 base64
	raw, decErr := base64.StdEncoding.DecodeString(ref)
	if decErr != nil {
		// 也可能是 URL-safe 变体
		raw, decErr = base64.RawURLEncoding.DecodeString(ref)
		if decErr != nil {
			return nil, "", "", badImageRef("既不是 data URI / URL，也不是合法 base64")
		}
	}
	m := sniffMime(raw)
	if m == "" {
		return nil, "", "", badImageRef("裸 base64 解出来后不是已知图片格式")
	}
	if vErr := validateImageBytes(raw, m); vErr != nil {
		return nil, "", "", vErr
	}
	return raw, m, "ref" + extForMime(m), nil
}

// sniffMime 按魔数嗅探（不信任调用方声明的 Content-Type）。
func sniffMime(b []byte) string {
	if len(b) < 12 {
		return ""
	}
	switch {
	case b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
		return "image/png"
	case b[0] == 0xFF && b[1] == 0xD8:
		return "image/jpeg"
	case b[0] == 'G' && b[1] == 'I' && b[2] == 'F':
		return "image/gif"
	case string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

func extForMime(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".bin"
}

// readMultipartImages 解析 /v1/images/edits 的 multipart（OpenAI 的载体差异，语义同 generations）。
//
// 返回 refs（data URI 形态，方便与 JSON 门共用同一条上传链）。
func readMultipartImages(r *http.Request) (prompt string, refs []string, err error) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		return "", nil, fmt.Errorf("multipart 解析失败: %w", err)
	}
	prompt = strings.TrimSpace(r.FormValue("prompt"))

	collect := func(field string) error {
		files := r.MultipartForm.File[field]
		for _, fh := range files {
			f, openErr := fh.Open()
			if openErr != nil {
				return fmt.Errorf("读取 %s 失败: %w", field, openErr)
			}
			raw, readErr := io.ReadAll(io.LimitReader(f, 32<<20))
			f.Close()
			if readErr != nil {
				return readErr
			}
			if len(raw) == 0 {
				// 载体歧义：HTML 未选文件就是长度 0 的 part ⇒ 按"没传"处理。
				continue
			}
			mime := sniffMime(raw)
			if mime == "" {
				return fmt.Errorf("%s 不是已知图片格式", fh.Filename)
			}
			refs = append(refs, "data:"+mime+";base64,"+base64.StdEncoding.EncodeToString(raw))
		}
		return nil
	}
	if err := collect("image"); err != nil {
		return "", nil, err
	}
	if err := collect("image[]"); err != nil {
		return "", nil, err
	}
	// 文本字段形态的 image（URL）也收。
	for _, v := range r.MultipartForm.Value["image"] {
		if strings.HasPrefix(v, "http") {
			refs = append(refs, v)
		}
	}
	return prompt, refs, nil
}

var _ = time.Second
