package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/yuanjie-ai/qwen2api/internal/toolcall"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// ---------- Anthropic Messages 前门 ----------
//
// 目的：Claude Code / Cline 这类只认 /v1/messages 的客户端能零改造接入。
// 语义对齐：请求翻成 chat 形态复用同一条选号/轮换/冷却链路，响应（含 SSE）
// 翻回 Anthropic 事件序列。
//
// 🔴 诚实边界：不做 thinking block（Anthropic 的 thinking 需要签名，编一个
// 假 signature 会让客户端校验失败）；推理内容只走 chat 门的 reasoning_content。

type anthropicRequest struct {
	Model      string             `json:"model"`
	System     json.RawMessage    `json:"system"`
	Messages   []anthropicMessage `json:"messages"`
	MaxTokens  int                `json:"max_tokens"`
	Stream     bool               `json:"stream"`
	Tools      []toolcall.Tool    `json:"tools,omitempty"`
	ToolChoice json.RawMessage    `json:"tool_choice,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// anthropicToolUseBlock Anthropic 的工具调用内容块。
//
// Input 是 **JSON 对象**（不是字符串）—— 这是 Anthropic 与 OpenAI 的关键差异，
// 客户端（Claude Code）直接按对象读。
type anthropicToolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type anthropicResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []any          `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence string         `json:"stop_sequence,omitempty"`
	Usage        anthropicUsage `json:"usage"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// handleMessages `POST /v1/messages`。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	msgs, err := anthropicToChat(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// 工具调用：与 chat 门同一套翻译（见 internal/toolcall），协议块作为
	// 最后一条 system 注入。
	tools := req.Tools
	var toolNote string
	if len(tools) > 0 {
		if !s.conf().ToolCallEnabled() {
			tools = nil
			toolNote = "服务端已关闭工具调用模拟（tool_calls.enabled=false），本轮 tools 未生效"
		} else if err := toolcall.Validate(tools); err != nil {
			writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "tools")
			return
		}
	}
	if proto := toolcall.Prompt(tools, toolcall.ParseChoice(req.ToolChoice)); proto != "" {
		msgs = append(msgs, ChatMessage{Role: "system", Content: mustJSON(proto)})
	}
	if len(tools) > 0 {
		warns := []string{"工具调用由网关模拟（上游无函数调用通道）：模型可能不遵守协议，解析失败会退回纯文本"}
		if toolNote != "" {
			warns = append(warns, toolNote)
		}
		setMediaWarnings(w, warns)
	} else if toolNote != "" {
		setMediaWarnings(w, []string{toolNote})
	}
	flat, err := Flatten(msgs)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = s.conf().DefaultModel
	}

	// 图片：content 里的 image 块 → 上游 files[]（vision）。坏引用一律 400。
	imgRefs, imgErr := collectImageRefs(msgs)
	if imgErr != nil {
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", imgErr.Error(), "image")
		return
	}
	if len(imgRefs) > maxInputImages {
		writeErrorParam(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("图片最多 %d 张（本次 %d 张）", maxInputImages, len(imgRefs)), "image")
		return
	}
	// Agent 模型（qwen3.8-*-agent）走 agent_mode，其余走 t2t（§10.1）。
	// 别名只影响路由，响应回显客户端原名（同 chat 前门）。
	routeModel, _ := s.routeModel(model)
	upModel, spec, isAgent := s.agentSpecFor(routeModel)
	// Agent 自带服务端工具集，与客户端 tools 混用会打架（同 chat 门口径）。
	if isAgent && len(tools) > 0 {
		spec = upstream.TextSpec()
	}
	spec.ImageRefs = imgRefs
	sd, err := s.openUpstream(r.Context(), s.blobBase(r), upModel, s.conf().DefaultThinking, flat.Prompt, spec)
	if err != nil {
		writeError(w, httpStatusFor(err), errTypeFor(err), err.Error())
		return
	}
	defer sd.stream.Close()
	defer s.pool.Release(sd.acct)
	defer s.pool.MarkOK(sd.acct)

	id := "msg_" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24]

	if !req.Stream {
		var sb strings.Builder
		var usage *upstream.Usage
		for {
			ev, more, err := sd.stream.Next()
			if err != nil {
				writeError(w, httpStatusFor(err), errTypeFor(err), err.Error())
				return
			}
			if !more {
				break
			}
			if ev.Usage != nil {
				usage = ev.Usage
			}
			if ev.Text != "" {
				sb.WriteString(ev.Text)
			}
			if ev.Done {
				break
			}
		}
		// 工具解析：与 chat 门同一判定（回复以 { 或 ``` 开头才尝试），
		// 解析不通过就是普通文本，不伪造 tool_use。
		var (
			toolCalls []toolcall.Call
			text      = sb.String()
		)
		if len(tools) > 0 {
			if calls, rest, ok := toolcall.Parse(text, toolcall.Allowed(tools)); ok {
				toolCalls, text = calls, rest
			}
		}
		blocks := make([]any, 0, len(toolCalls)+1)
		if text != "" || len(toolCalls) == 0 {
			blocks = append(blocks, anthropicTextBlock{Type: "text", Text: text})
		}
		for _, c := range toolCalls {
			blocks = append(blocks, anthropicToolUseBlock{
				Type: "tool_use", ID: c.ID, Name: c.Name, Input: json.RawMessage(c.Arguments),
			})
		}
		stop := "end_turn"
		if len(toolCalls) > 0 {
			stop = "tool_use"
		}
		resp := anthropicResponse{
			ID: id, Type: "message", Role: "assistant", Model: model,
			Content:    blocks,
			StopReason: stop,
		}
		if usage != nil {
			resp.Usage = anthropicUsage{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens}
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 流式：严格按 Anthropic 事件序列。
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "internal_error", "响应写入器不支持流式")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	s.anthropicEvent(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	// 内容块是**惰性开启**的：Anthropic 要求每个 start 都有对应的 stop，
	// 而"本轮到底是文本还是 tool_use"要等整段回复才定。预先开一个空 text 块
	// 会在纯工具调用时留下一个 `text:""` 的鬼块（Claude Code 会渲染成空提示）。
	var (
		usage       *upstream.Usage
		blockIdx    int
		textStarted bool
		allowed     = toolcall.Allowed(tools)
		hold        = len(tools) > 0
		pending     strings.Builder
	)
	startTextBlock := func() {
		if textStarted {
			return
		}
		textStarted = true
		s.anthropicEvent(w, flusher, "content_block_start", map[string]any{
			"type": "content_block_start", "index": blockIdx,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	emitTextDelta := func(text string) {
		startTextBlock()
		s.anthropicEvent(w, flusher, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": blockIdx,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	}
	for {
		ev, more, err := sd.stream.Next()
		if err != nil {
			// 与 chat 前门同一口径：流内错误只给标准英文，详细原因留服务端。
			_, msg := clientError(http.StatusBadGateway, errTypeFor(err), err.Error())
			s.anthropicEvent(w, flusher, "error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": msg},
			})
			break
		}
		if !more {
			break
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		if ev.Text != "" {
			if !hold {
				emitTextDelta(ev.Text)
			} else {
				pending.WriteString(ev.Text)
				h, _ := toolcall.Decide(pending.String())
				if !h {
					hold = false
					emitTextDelta(pending.String())
					pending.Reset()
				}
			}
		}
		if ev.Done {
			break
		}
	}

	// 判定 + 收尾：解析不通过就按普通文本补发，绝不伪造 tool_use。
	var toolCalls []toolcall.Call
	if pending.Len() > 0 {
		if calls, rest, ok := toolcall.Parse(pending.String(), allowed); ok {
			toolCalls = calls
			if rest != "" {
				emitTextDelta(rest)
			}
		} else {
			emitTextDelta(pending.String())
		}
		pending.Reset()
	}
	// 正文块还没开过、也不是工具调用 ⇒ 空回复也要给一个 text 块，
	// 否则客户端等不到任何内容块（协议上是合法的，但表现为"卡住"）。
	if !textStarted && len(toolCalls) == 0 {
		startTextBlock()
	}
	stop := "end_turn"
	if len(toolCalls) > 0 {
		stop = "tool_use"
	}
	if textStarted {
		s.anthropicEvent(w, flusher, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": blockIdx,
		})
		blockIdx++
	}
	for _, c := range toolCalls {
		s.anthropicEvent(w, flusher, "content_block_start", map[string]any{
			"type": "content_block_start", "index": blockIdx,
			"content_block": map[string]any{
				"type": "tool_use", "id": c.ID, "name": c.Name, "input": map[string]any{},
			},
		})
		s.anthropicEvent(w, flusher, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": blockIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": c.Arguments},
		})
		s.anthropicEvent(w, flusher, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": blockIdx,
		})
		blockIdx++
	}
	usageOut := map[string]any{"output_tokens": 0}
	if usage != nil {
		usageOut["output_tokens"] = usage.OutputTokens
		usageOut["input_tokens"] = usage.InputTokens
	}
	s.anthropicEvent(w, flusher, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": usageOut,
	})
	s.anthropicEvent(w, flusher, "message_stop", map[string]any{"type": "message_stop"})
	flusher.Flush()
}

// handleCountTokens 粗略估算输入 token（Anthropic 客户端会调）。
//
// 口径：CJK 约 1 字 = 1 token，其余按 4 字符 = 1 token 估。**明确是估算**，
// 不做假精确 —— 真实用量由上游 usage 在正式响应里给出。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	var req anthropicRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	msgs, err := anthropicToChat(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// 工具协议的 token 也要算进去：客户端拿这个数判断能否塞进上下文，
	// 少算一段 schema 会让它在大工具集上误判。
	if len(req.Tools) > 0 && s.conf().ToolCallEnabled() {
		if err := toolcall.Validate(req.Tools); err != nil {
			writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "tools")
			return
		}
		if proto := toolcall.Prompt(req.Tools, toolcall.ParseChoice(req.ToolChoice)); proto != "" {
			msgs = append(msgs, ChatMessage{Role: "system", Content: mustJSON(proto)})
		}
	}
	flat, err := Flatten(msgs)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": EstimateTokens(flat.Prompt)})
}

// EstimateTokens 估算 token 数（CJK 1:1，ASCII 4:1）。
func EstimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if r >= 0x2E80 {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

func (s *Server) anthropicEvent(w http.ResponseWriter, f http.Flusher, event string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = io.WriteString(w, "event: "+event+"\ndata: ")
	_, _ = w.Write(raw)
	_, _ = io.WriteString(w, "\n\n")
	f.Flush()
}

// anthropicToChat Anthropic messages → 内部 chat messages。
//
// 工具块的处理（这是 Claude Code 能跑起来的关键）：
//   - assistant 里的 `tool_use` → ChatMessage.ToolCalls（拍平时渲染成「调用工具」）
//   - user 里的 `tool_result`  → 单独的 role=tool 消息（带 tool_call_id 回配对）
//
// 之前这两类块一律 400；现在有了工具模拟就得真的翻译，否则 Claude Code 的
// 第二轮历史（工具结果）根本进不来。
func anthropicToChat(req anthropicRequest) ([]ChatMessage, error) {
	var out []ChatMessage
	if sys := anthropicText(req.System); sys != "" {
		out = append(out, ChatMessage{Role: "system", Content: mustJSON(sys)})
	}
	for _, m := range req.Messages {
		// 纯字符串形态：最常见的单消息，直接过。
		var plain string
		if err := json.Unmarshal(m.Content, &plain); err == nil {
			out = append(out, ChatMessage{Role: m.Role, Content: mustJSON(plain)})
			continue
		}
		var parts []json.RawMessage
		if err := json.Unmarshal(m.Content, &parts); err != nil {
			return nil, errContentShape
		}
		var (
			keep    []json.RawMessage
			calls   []ToolCallMsg
			results []ChatMessage
		)
		for _, p := range parts {
			var head struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(p, &head)
			switch head.Type {
			case "tool_use":
				var tu struct {
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				}
				if err := json.Unmarshal(p, &tu); err != nil {
					return nil, fmt.Errorf("tool_use 块解析失败: %w", err)
				}
				args := strings.TrimSpace(string(tu.Input))
				if args == "" || args == "null" {
					args = "{}"
				}
				calls = append(calls, ToolCallMsg{
					ID: tu.ID, Type: "function",
					Function: ToolCallFunction{Name: tu.Name, Arguments: args},
				})
			case "tool_result":
				var tr struct {
					ToolUseID string          `json:"tool_use_id"`
					Content   json.RawMessage `json:"content"`
					IsError   bool            `json:"is_error"`
				}
				if err := json.Unmarshal(p, &tr); err != nil {
					return nil, fmt.Errorf("tool_result 块解析失败: %w", err)
				}
				body := anthropicText(tr.Content)
				if tr.IsError {
					body = "（工具报错）" + body
				}
				results = append(results, ChatMessage{
					Role: "tool", ToolCallID: tr.ToolUseID, Content: mustJSON(body),
				})
			case "document":
				return nil, errDocumentUnsupported
			default:
				keep = append(keep, p)
			}
		}
		// 有正文/图片就保留一条消息；只有工具调用时也要保留（拍平时要渲染调用）。
		if len(keep) > 0 || len(calls) > 0 {
			content, err := anthropicContentRaw(mustJSONParts(keep))
			if err != nil {
				return nil, err
			}
			out = append(out, ChatMessage{Role: m.Role, Content: content, ToolCalls: calls})
		}
		out = append(out, results...)
	}
	return out, nil
}

// mustJSONParts 把分段数组重新编码（空数组 ⇒ 空字符串 content）。
func mustJSONParts(parts []json.RawMessage) json.RawMessage {
	if len(parts) == 0 {
		return mustJSON("")
	}
	b, err := json.Marshal(parts)
	if err != nil {
		return mustJSON("")
	}
	return b
}

// anthropicText 解析 system 字段（字符串或分段数组）。
func anthropicText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []anthropicTextBlock
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// anthropicContentText 解析消息 content（字符串或分段数组）。
func anthropicContentText(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", errContentShape
	}
	var sb strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			sb.WriteString(p.Text)
		case "image", "image_url", "input_image":
			// 图片不参与文本（只用于 token 估算）；真正的多模态走 anthropicContentRaw。
		}
	}
	return sb.String(), nil
}

// errContentShape content 既不是字符串也不是分段数组。
var errContentShape = errors.New("content 形态不支持（既不是字符串也不是分段数组）")

// errDocumentUnsupported Anthropic document 块。
var errDocumentUnsupported = errors.New(
	"document 块暂不支持：上游要它自己 OSS 的附件条目 —— " +
		"请先用 /v1/files 上传，再在 content 里引用 file_id")

// anthropicContentRaw 把 Anthropic / Responses 的 content 归一成内部形态。
//
//   - 字符串 ⇒ 原样（转成 JSON 字符串）
//   - 分段数组 ⇒ 过滤：文本与图片保留（图片交给 collectImageRefs → 上游 files[]），
//     工具/文档块明确 400（静默丢块会让客户端以为工具跑过了）
func anthropicContentRaw(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return mustJSON(""), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return mustJSON(s), nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errContentShape
	}
	keep := make([]json.RawMessage, 0, len(parts))
	for _, p := range parts {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(p, &head)
		switch head.Type {
		case "text", "input_text", "image", "image_url", "input_image":
			keep = append(keep, p)
		case "tool_use", "tool_result":
			return nil, errToolsUnsupported
		case "document":
			return nil, errDocumentUnsupported
		default:
			// 未知块类型忽略（与 chat 门同口径）。
		}
	}
	return json.Marshal(keep)
}

func mustJSON(s string) json.RawMessage {
	raw, _ := json.Marshal(s)
	return raw
}
