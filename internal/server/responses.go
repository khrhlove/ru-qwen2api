package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/toolcall"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// ---------- OpenAI Responses 前门 ----------
//
// 供 Codex 之类只认 /v1/responses 的客户端接入。请求被翻成 chat 形态，
// 响应按 Responses 的对象/事件形态回写。
//
// 文本与客户端函数调用（tools）都支持。工具走与 chat 同一套翻译
// （internal/toolcall）：进把 schema 写进 prompt，出把 JSON 解析成
// function_call 输出项。内置检索（web_search 等）仍不支持。

type responsesRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"`
	Instructions string          `json:"instructions"`
	Stream       bool            `json:"stream"`
	Tools        []toolcall.Tool `json:"tools,omitempty"`
	ToolChoice   json.RawMessage `json:"tool_choice,omitempty"`
}

// handleResponses `POST /v1/responses`。
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "请求体解析失败: "+err.Error())
		return
	}
	msgs, err := responsesToChat(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// 工具调用：与 chat 门同一套翻译，协议块作为最后一条 system 注入。
	tools := req.Tools
	var toolNote string
	if len(tools) > 0 {
		if !s.conf().ToolCallEnabled() {
			tools = nil
			toolNote = "服务端已关闭工具调用模拟（tool_calls.enabled=false），本轮 tools 未生效"
		} else {
			// 名字非法是客户端的 bug（400）；容量超限只是降级（跳过模拟）。
			if err := toolcall.Validate(tools); err != nil {
				writeErrorParam(w, http.StatusBadRequest, "invalid_request_error", err.Error(), "tools")
				return
			}
			var note string
			tools, note = s.conf().ToolCallLimits().Fit(tools)
			toolNote = joinDegraded(toolNote, note)
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

	// 图片：input_image → 上游 files[]（vision）。坏引用一律 400。
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

	id := "resp_" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24]
	msgID := "msg_" + strings.ReplaceAll(upstream.UUID(), "-", "")[:24]
	created := time.Now().Unix()

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
		text := sb.String()
		var toolCalls []toolcall.Call
		if len(tools) > 0 {
			if calls, rest, ok := toolcall.Parse(text, toolcall.Allowed(tools)); ok {
				toolCalls, text = calls, rest
			}
		}
		output := make([]any, 0, len(toolCalls)+1)
		if text != "" || len(toolCalls) == 0 {
			output = append(output, responsesMessageItem(msgID, text))
		}
		for _, c := range toolCalls {
			output = append(output, responsesFunctionCallItem(c))
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "response", "created_at": created, "model": model,
			"status": "completed",
			"output": output,
			"usage":  responsesUsage(usage),
		})
		return
	}

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

	seq := 0
	emit := func(event string, payload map[string]any) {
		payload["type"] = event
		payload["sequence_number"] = seq
		seq++
		s.anthropicEvent(w, flusher, event, payload)
	}

	emit("response.created", map[string]any{
		"response": map[string]any{
			"id": id, "object": "response", "created_at": created, "model": model, "status": "in_progress",
		},
	})

	// 输出项惰性开启：纯工具调用时不该先冒出一个空 message 项。
	var (
		usage       *upstream.Usage
		allowed     = toolcall.Allowed(tools)
		hold        = len(tools) > 0
		pending     strings.Builder
		textStarted bool
		text        strings.Builder
		outputIdx   int
		outputs     []any
	)
	startMessage := func() {
		if textStarted {
			return
		}
		textStarted = true
		emit("response.output_item.added", map[string]any{
			"output_index": outputIdx,
			"item":         map[string]any{"id": msgID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
		})
		emit("response.content_part.added", map[string]any{
			"item_id": msgID, "output_index": outputIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	emitTextDelta := func(chunk string) {
		startMessage()
		text.WriteString(chunk)
		emit("response.output_text.delta", map[string]any{
			"item_id": msgID, "output_index": outputIdx, "content_index": 0, "delta": chunk,
		})
	}
	for {
		ev, more, err := sd.stream.Next()
		if err != nil {
			// 同上：客户端只该看到机器可读的短句，内部细节留在服务端。
			_, msg := clientError(http.StatusBadGateway, errTypeFor(err), err.Error())
			emit("response.failed", map[string]any{
				"response": map[string]any{"id": id, "status": "failed",
					"error": map[string]any{"code": "upstream_error", "message": msg}},
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

	// 判定 + 收尾：解析不通过就按普通文本补发，绝不伪造 function_call。
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
	if !textStarted && len(toolCalls) == 0 {
		startMessage()
	}
	if textStarted {
		full := text.String()
		emit("response.output_text.done", map[string]any{
			"item_id": msgID, "output_index": outputIdx, "content_index": 0, "text": full,
		})
		emit("response.content_part.done", map[string]any{
			"item_id": msgID, "output_index": outputIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": full, "annotations": []any{}},
		})
		item := responsesMessageItem(msgID, full)
		emit("response.output_item.done", map[string]any{"output_index": outputIdx, "item": item})
		outputs = append(outputs, item)
		outputIdx++
	}
	for _, c := range toolCalls {
		// 三个事件缺一不可：Codex 靠 added 建项、靠 delta 拼参数、靠 done 收口。
		emit("response.output_item.added", map[string]any{
			"output_index": outputIdx,
			"item": map[string]any{
				"id": "fc_" + strings.TrimPrefix(c.ID, "call_"), "type": "function_call",
				"status": "in_progress", "name": c.Name, "arguments": "", "call_id": c.ID,
			},
		})
		emit("response.function_call_arguments.delta", map[string]any{
			"item_id": "fc_" + strings.TrimPrefix(c.ID, "call_"), "output_index": outputIdx,
			"delta": c.Arguments,
		})
		emit("response.function_call_arguments.done", map[string]any{
			"item_id": "fc_" + strings.TrimPrefix(c.ID, "call_"), "output_index": outputIdx,
			"arguments": c.Arguments,
		})
		item := responsesFunctionCallItem(c)
		emit("response.output_item.done", map[string]any{"output_index": outputIdx, "item": item})
		outputs = append(outputs, item)
		outputIdx++
	}
	emit("response.completed", map[string]any{
		"response": map[string]any{
			"id": id, "object": "response", "created_at": created, "model": model, "status": "completed",
			"output": outputs,
			"usage":  responsesUsage(usage),
		},
	})
	flusher.Flush()
}

// responsesMessageItem assistant 文本输出项。
func responsesMessageItem(id, text string) map[string]any {
	return map[string]any{
		"id": id, "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
}

// responsesFunctionCallItem 工具调用输出项。
//
// 🔴 两个 id 不能混：`call_id` 是**客户端回传时配对**用的（下一轮
// function_call_output 靠它找到这次调用），`id` 只是这个输出项自身。
// 混成一个字段的值，Codex 会在回传时对不上号。
func responsesFunctionCallItem(c toolcall.Call) map[string]any {
	return map[string]any{
		"id":   "fc_" + strings.TrimPrefix(c.ID, "call_"),
		"type": "function_call", "status": "completed",
		"name": c.Name, "arguments": c.Arguments, "call_id": c.ID,
	}
}

func responsesUsage(u *upstream.Usage) map[string]any {
	if u == nil {
		// 形状对齐成功路径：客户端不会为 0 做特判，少一个 key 就得自己兜。
		return map[string]any{
			"input_tokens": 0, "output_tokens": 0, "total_tokens": 0,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
		}
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	// OpenAI Responses 的 usage 形状与 Chat Completions 不同：
	// 明细挂在 input_tokens_details / output_tokens_details 下。
	out := map[string]any{
		"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens, "total_tokens": total,
		"input_tokens_details": map[string]any{"cached_tokens": u.CachedTokens()},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": u.OutputTokensDetails.ReasoningTokens,
		},
	}
	return out
}

// responsesToChat 解析 input 字段（字符串 或 消息数组）。
func responsesToChat(req responsesRequest) ([]ChatMessage, error) {
	var out []ChatMessage
	if req.Instructions != "" {
		out = append(out, ChatMessage{Role: "system", Content: mustJSON(req.Instructions)})
	}
	if len(req.Input) == 0 {
		return nil, errEmptyInput
	}
	var s string
	if err := json.Unmarshal(req.Input, &s); err == nil {
		out = append(out, ChatMessage{Role: "user", Content: mustJSON(s)})
		return out, nil
	}
	var items []struct {
		Role      string          `json:"role"`
		Type      string          `json:"type"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		Arguments string          `json:"arguments"`
		CallID    string          `json:"call_id"`
		Output    json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return nil, errEmptyInput
	}
	for _, it := range items {
		switch it.Type {
		case "function_call":
			// 上一轮 assistant 发起的调用：拍平时要渲染成「调用工具」，
			// 否则模型看到后面的 function_call_output 会不知道它来自哪里。
			args := strings.TrimSpace(it.Arguments)
			if args == "" {
				args = "{}"
			}
			callID := strings.TrimSpace(it.CallID)
			out = append(out, ChatMessage{
				Role: "assistant", Content: mustJSON(""),
				ToolCalls: []ToolCallMsg{{
					ID: callID, Type: "function",
					Function: ToolCallFunction{Name: it.Name, Arguments: args},
				}},
			})
			continue
		case "function_call_output":
			body := anthropicText(it.Output)
			out = append(out, ChatMessage{
				Role: "tool", ToolCallID: strings.TrimSpace(it.CallID), Content: mustJSON(body),
			})
			continue
		}
		role := it.Role
		if role == "" {
			role = "user"
		}
		content, err := anthropicContentRaw(it.Content)
		if err != nil {
			return nil, err
		}
		out = append(out, ChatMessage{Role: role, Content: content})
	}
	if len(out) == 0 {
		return nil, errEmptyInput
	}
	return out, nil
}
