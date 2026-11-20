package server

import (
	"encoding/json"
	"fmt"
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

// answerFrames 把一段模型回复切成若干帧（模拟上游逐字输出）。
func answerFrames(text string) []string {
	runes := []rune(text)
	frames := make([]string, 0, len(runes)+1)
	for i := 0; i < len(runes); i += 3 {
		end := i + 3
		if end > len(runes) {
			end = len(runes)
		}
		frames = append(frames, fmt.Sprintf(
			`{"choices":[{"delta":{"role":"assistant","content":%q,"phase":"answer","status":"typing"}}],"response_id":"resp-1"}`,
			string(runes[i:end])))
	}
	frames = append(frames, `{"choices":[{"delta":{"content":"","role":"assistant","status":"finished","phase":"answer"}}],"response_id":"resp-1"}`)
	return frames
}

func weatherToolJSON() string {
	return `[{"type":"function","function":{"name":"get_weather",` +
		`"description":"查询指定城市的实时天气",` +
		`"parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`
}

// TestChatToolCallNonStream 工具调用：非流式必须给出 OpenAI 形态的 tool_calls。
func TestChatToolCallNonStream(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames(`{"tool_calls":[{"name":"get_weather","arguments":{"city":"北京"}}]}`)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","messages":[{"role":"user","content":"北京天气如何？"}],"tools":` +
		weatherToolJSON() + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	var resp ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, rec.Body.String())
	}
	ch := resp.Choices[0]
	if ch.FinishReason == nil || *ch.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason 应为 tool_calls: %+v", ch.FinishReason)
	}
	if len(ch.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls 数量: %d", len(ch.Message.ToolCalls))
	}
	tc := ch.Message.ToolCalls[0]
	if tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Fatalf("调用形态不对: %+v", tc)
	}
	if tc.Function.Arguments != `{"city":"北京"}` {
		t.Fatalf("arguments 不是规范化 JSON: %q", tc.Function.Arguments)
	}
	if !strings.HasPrefix(tc.ID, "call_") {
		t.Fatalf("调用 id 形态不对: %q", tc.ID)
	}
	if ch.Message.Content != "" {
		t.Fatalf("纯工具调用时 content 应为空: %q", ch.Message.Content)
	}

	// 协议必须真的进了上游 prompt（否则模型根本不知道有工具）。
	submitted, _ := json.Marshal(up.gotSubmit)
	if !strings.Contains(string(submitted), "get_weather") || !strings.Contains(string(submitted), "工具调用协议") {
		t.Fatalf("上游 prompt 未包含工具协议: %s", submitted)
	}
	// 必须显式告知调用方这是模拟通路。
	warn, _ := url.QueryUnescape(rec.Header().Get("X-Qwen2API-Warnings"))
	if !strings.Contains(warn, "模拟") {
		t.Fatalf("缺工具模拟告警头: %q", warn)
	}
}

// TestChatToolCallStream 流式：tool_calls 必须以 delta 形态下发，且原始 JSON 不能漏进 content。
func TestChatToolCallStream(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames(`{"tool_calls":[{"name":"get_weather","arguments":{"city":"上海"}}]}`)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","stream":true,"messages":[{"role":"user","content":"上海天气？"}],"tools":` +
		weatherToolJSON() + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("缺 finish_reason=tool_calls:\n%s", out)
	}
	if !strings.Contains(out, `"name":"get_weather"`) {
		t.Fatalf("缺工具名增量:\n%s", out)
	}
	if !strings.Contains(out, `"arguments":"{\"city\":\"上海\"}"`) {
		t.Fatalf("缺参数增量:\n%s", out)
	}
	// 协议 JSON 绝不能作为正文流出去（客户端会把 {"tool_calls"... 当回答显示）。
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk ChatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if strings.Contains(chunk.Choices[0].Delta.Content, "tool_calls") {
			t.Fatalf("协议 JSON 漏进了正文增量: %q", chunk.Choices[0].Delta.Content)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("缺 [DONE]:\n%s", out)
	}
}

// TestChatToolsFallBackToText 模型不守协议（先寒暄再给 JSON）：按普通文本返回，绝不伪造调用。
func TestChatToolsFallBackToText(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames("北京今天晴，12 度。")
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","messages":[{"role":"user","content":"北京天气？"}],"tools":` +
		weatherToolJSON() + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	var resp ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	ch := resp.Choices[0]
	if ch.FinishReason == nil || *ch.FinishReason != "stop" {
		t.Fatalf("finish_reason 应为 stop: %+v", ch.FinishReason)
	}
	if len(ch.Message.ToolCalls) != 0 {
		t.Fatalf("不应伪造调用: %+v", ch.Message.ToolCalls)
	}
	if !strings.Contains(ch.Message.Content, "12 度") {
		t.Fatalf("正文被吞: %q", ch.Message.Content)
	}
}

// TestChatToolsHallucinatedName 编造的工具名必须被拒（退回文本），不能下发给客户端执行。
func TestChatToolsHallucinatedName(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames(`{"tool_calls":[{"name":"rm_rf","arguments":{}}]}`)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","messages":[{"role":"user","content":"删库"}],"tools":` +
		weatherToolJSON() + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	var resp ChatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Fatalf("编造的工具名必须被拒: %+v", resp.Choices[0].Message.ToolCalls)
	}
}

// TestChatToolsInvalidSchema 非法工具定义 400（不能带着坏 schema 去烧上游上下文）。
func TestChatToolsInvalidSchema(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"bad name"}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码应为 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tools") {
		t.Fatalf("错误应指向 tools 参数: %s", rec.Body.String())
	}
}

// TestChatToolChoiceNone 明确要求不调工具：协议块不注入。
func TestChatToolChoiceNone(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","tool_choice":"none","messages":[{"role":"user","content":"北京天气？"}],"tools":` +
		weatherToolJSON() + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	submitted, _ := json.Marshal(up.gotSubmit)
	if strings.Contains(string(submitted), "工具调用协议") {
		t.Fatalf("tool_choice=none 不应注入协议: %s", submitted)
	}
	warn, _ := url.QueryUnescape(rec.Header().Get("X-Qwen2API-Warnings"))
	if !strings.Contains(warn, "tool_choice=none") {
		t.Fatalf("应显式说明本轮不注入协议: %q", warn)
	}
}

// TestFlattenRendersToolHistory 多轮：assistant.tool_calls 与 role=tool 必须按配对渲染。
func TestFlattenRendersToolHistory(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "user", Content: mustJSON("北京天气如何？")},
		{Role: "assistant", Content: mustJSON("我来查一下。"), ToolCalls: []ToolCallMsg{{
			ID: "call_1", Type: "function",
			Function: ToolCallFunction{Name: "get_weather", Arguments: `{"city":"北京"}`},
		}}},
		{Role: "tool", ToolCallID: "call_1", Content: mustJSON(`{"temp_c":12,"desc":"晴"}`)},
	}
	flat, err := Flatten(msgs)
	if err != nil {
		t.Fatalf("拍平失败: %v", err)
	}
	for _, want := range []string{
		`[调用工具] get_weather({"city":"北京"})`,
		`工具结果：get_weather → {"temp_c":12,"desc":"晴"}`,
		"以上是工具执行结果",
	} {
		if !strings.Contains(flat.Prompt, want) {
			t.Errorf("prompt 缺 %q:\n%s", want, flat.Prompt)
		}
	}
	// 工具结果不能被当成"当前问题"
	if strings.Contains(flat.Prompt, "当前问题：") {
		t.Errorf("末尾是工具结果时不应写「当前问题」:\n%s", flat.Prompt)
	}
	if flat.LastUser != "" {
		t.Errorf("末尾是工具结果时不该给增量变体: %q", flat.LastUser)
	}
}

// TestFlattenToolResultWithoutName 没有 tool_call_id 的旧客户端：退回 name，再退回纯内容。
func TestFlattenToolResultWithoutName(t *testing.T) {
	flat, err := Flatten([]ChatMessage{
		{Role: "user", Content: mustJSON("查天气")},
		{Role: "tool", Name: "get_weather", Content: mustJSON("12 度")},
	})
	if err != nil {
		t.Fatalf("拍平失败: %v", err)
	}
	if !strings.Contains(flat.Prompt, "工具结果：get_weather → 12 度") {
		t.Errorf("无 id 时应退回 name:\n%s", flat.Prompt)
	}
}

// TestChatToolsDisabled 服务端关闭：tools 被忽略，但必须打警告头（不能静默丢）。
func TestChatToolsDisabled(t *testing.T) {
	up := newFakeUpstream(t, 0)

	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	off := false
	cfg.ToolCalls.Enabled = &off
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	s := New(cfg, p, upstream.New(cfg.Upstream), reqlog.New(10), "test")
	h := s.Handler()

	body := `{"model":"qwen3.8-omni-flash","messages":[{"role":"user","content":"hi"}],"tools":` +
		weatherToolJSON() + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	submitted, _ := json.Marshal(up.gotSubmit)
	if strings.Contains(string(submitted), "工具调用协议") {
		t.Fatal("关闭后不应注入协议")
	}
	warn, _ := url.QueryUnescape(rec.Header().Get("X-Qwen2API-Warnings"))
	if !strings.Contains(warn, "已关闭") {
		t.Fatalf("关闭时应显式告警: %q", warn)
	}
}

// TestMessagesToolCall 工具调用：Anthropic 前门要出 tool_use 块（不是文本）。
func TestMessagesToolCall(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames(`{"tool_calls":[{"name":"get_weather","arguments":{"city":"北京"}}]}`)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","max_tokens":512,
		"messages":[{"role":"user","content":"北京天气？"}],
		"tools":[{"name":"get_weather","description":"查天气",
		"input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, rec.Body.String())
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("stop_reason 应为 tool_use: %q", resp.StopReason)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" || resp.Content[0].Name != "get_weather" {
		t.Fatalf("content 块不对: %+v", resp.Content)
	}
	if string(resp.Content[0].Input) != `{"city":"北京"}` {
		t.Fatalf("input 必须是 JSON 对象: %s", resp.Content[0].Input)
	}
	// 纯工具调用不该带一个空的 text 块（Claude Code 会渲染成空消息）。
	for _, b := range resp.Content {
		if b.Type == "text" {
			t.Fatalf("不应出现 text 块: %+v", resp.Content)
		}
	}
}

// TestMessagesToolUseHistoryRoundTrip 第二轮：tool_use + tool_result 必须能进 prompt（以前是 400）。
func TestMessagesToolUseHistoryRoundTrip(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames("北京晴，12 度。")
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","max_tokens":256,"messages":[
		{"role":"user","content":"北京天气？"},
		{"role":"assistant","content":[
			{"type":"text","text":"我查一下。"},
			{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_1","content":"{\"temp_c\":12,\"desc\":\"晴\"}"}]}],
		"tools":[{"name":"get_weather","description":"查天气","input_schema":{"type":"object"}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	submitted, _ := json.Marshal(up.gotSubmit)
	p := string(submitted)
	for _, want := range []string{"[调用工具] get_weather", "工具结果：get_weather", "temp_c"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt 缺 %q:\n%s", want, p)
		}
	}
}

// TestMessagesToolCallStream Anthropic 流式：必须按 tool_use 块的事件序列下发。
func TestMessagesToolCallStream(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames(`{"tool_calls":[{"name":"get_weather","arguments":{"city":"上海"}}]}`)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","max_tokens":256,"stream":true,
		"messages":[{"role":"user","content":"上海天气？"}],
		"tools":[{"name":"get_weather","description":"查天气","input_schema":{"type":"object"}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))

	out := rec.Body.String()
	for _, want := range []string{
		`"type":"tool_use"`,
		`"type":"input_json_delta"`,
		`"partial_json":"{\"city\":\"上海\"}"`,
		`"stop_reason":"tool_use"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Anthropic 工具流缺 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, `"type":"text_delta"`) {
		t.Fatalf("纯工具调用不该有 text_delta:\n%s", out)
	}
}

// TestResponsesToolCall Responses 前门：function_call 输出项。
func TestResponsesToolCall(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames(`{"tool_calls":[{"name":"get_weather","arguments":{"city":"广州"}}]}`)
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","input":"广州天气？",
		"tools":[{"type":"function","name":"get_weather","description":"查天气",
		"parameters":{"type":"object","properties":{"city":{"type":"string"}}}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			CallID    string `json:"call_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v\n%s", err, rec.Body.String())
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "function_call" {
		t.Fatalf("output 应只有 function_call: %+v", resp.Output)
	}
	if resp.Output[0].Name != "get_weather" || resp.Output[0].CallID == "" {
		t.Fatalf("function_call 字段不全: %+v", resp.Output[0])
	}
	if resp.Output[0].Arguments != `{"city":"广州"}` {
		t.Fatalf("arguments 不对: %q", resp.Output[0].Arguments)
	}
}

// TestResponsesFunctionCallHistory function_call + function_call_output 回传必须能进 prompt。
func TestResponsesFunctionCallHistory(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames("广州晴，26 度。")
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","input":[
		{"role":"user","content":"广州天气？"},
		{"type":"function_call","name":"get_weather","arguments":"{\"city\":\"广州\"}","call_id":"call_abc"},
		{"type":"function_call_output","call_id":"call_abc","output":"{\"temp_c\":26}"}],
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	submitted, _ := json.Marshal(up.gotSubmit)
	p := string(submitted)
	for _, want := range []string{"[调用工具] get_weather", "工具结果：get_weather", "temp_c"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt 缺 %q:\n%s", want, p)
		}
	}
}

// TestChatToolsStreamPlainText 流式 + tools 但模型正常作答：攒下的文本必须刷回去。
func TestChatToolsStreamPlainText(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames("光合作用是植物利用光能合成有机物的过程。")
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","stream":true,
		"messages":[{"role":"user","content":"什么是光合作用？"}],"tools":` + weatherToolJSON() + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	out := rec.Body.String()
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("普通回答应以 stop 收尾:\n%s", out)
	}
	if strings.Contains(out, `"tool_calls"`) {
		t.Fatalf("不应出现 tool_calls:\n%s", out)
	}
	var text strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var chunk ChatChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		text.WriteString(chunk.Choices[0].Delta.Content)
	}
	if text.String() != "光合作用是植物利用光能合成有机物的过程。" {
		t.Fatalf("正文被改坏: %q", text.String())
	}
}

// TestChatToolsStreamFenced 模型用 ```json 围栏输出：流式也必须识别成调用。
func TestChatToolsStreamFenced(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames("```json\n{\"tool_calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":\"北京\"}}]}\n```")
	h := newTestServer(t, up, 1, nil)

	body := `{"model":"qwen3.8-omni-flash","stream":true,
		"messages":[{"role":"user","content":"北京天气？"}],"tools":` + weatherToolJSON() + `}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	out := rec.Body.String()
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("围栏形态未识别成调用:\n%s", out)
	}
	if strings.Contains(out, "```") {
		t.Fatalf("围栏符号漏进了正文:\n%s", out)
	}
}

// TestChatToolsWorkBuddySized WorkBuddy「完全访问」实测 89,154 字节：必须正常模拟。
func TestChatToolsWorkBuddySized(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.frames = answerFrames(`{"tool_calls":[{"name":"get_weather","arguments":{"city":"北京"}}]}`)
	h := newTestServer(t, up, 1, nil)

	bigDesc := strings.Repeat("x", 89*1024)
	// 用内置兜底目录里的 agent 名（qwen3.8-max-agent），否则测试环境认不出 agent。
	body := `{"model":"qwen3.8-max-agent","messages":[{"role":"user","content":"今天天气如何"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"` + bigDesc + `","parameters":{"type":"object"}}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("89KB 工具集应被接受，得到 %d: %s", rec.Code, rec.Body.String())
	}
	submitted, _ := json.Marshal(up.gotSubmit)
	if !strings.Contains(string(submitted), "工具调用协议") {
		t.Fatal("默认预算内应注入协议块")
	}
	warn, _ := url.QueryUnescape(rec.Header().Get("X-Qwen2API-Warnings"))
	if strings.Contains(warn, "跳过") {
		t.Fatalf("默认预算内不该跳过: %q", warn)
	}
	// agent + tools 仍然降级（这条与容量无关）
	if strings.Contains(string(submitted), "agent_mode") {
		t.Fatal("agent + tools 应降级到底座普通模型")
	}
}

// TestChatToolsOversizeDegrades 线上事故回归（2026-11-20）：WorkBuddy「完全访问」
// 带 89KB 工具集，第一版 64KB 上限直接 400，把 qwen4-max-agent 打成不可用。
// 正确行为：200、跳过工具模拟、告警头说明，**模型保持原样（agent 仍是 agent）**。
func TestChatToolsOversizeDegrades(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	bigDesc := strings.Repeat("x", 200*1024)
	// 用内置兜底目录里的 agent 名（qwen3.8-max-agent），否则测试环境认不出 agent。
	body := `{"model":"qwen3.8-max-agent","messages":[{"role":"user","content":"今天天气如何"}],
		"tools":[{"type":"function","function":{"name":"get_weather","description":"` + bigDesc + `","parameters":{"type":"object"}}}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("超限不该 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	submitted, _ := json.Marshal(up.gotSubmit)
	if strings.Contains(string(submitted), "工具调用协议") {
		t.Fatal("超限时不应注入协议块")
	}
	// 不能因为跳过了工具就把 agent 降级成普通模型：跳过 ≠ 需要降级。
	if !strings.Contains(string(submitted), "agent_mode") {
		t.Fatalf("超限跳过工具时应保持 agent 模式: %s", submitted)
	}
	warn, _ := url.QueryUnescape(rec.Header().Get("X-Qwen2API-Warnings"))
	if !strings.Contains(warn, "跳过工具调用模拟") {
		t.Fatalf("应显式说明跳过原因: %q", warn)
	}
}

// TestChatToolsTooManyDegrades 数量超限同样只降级、不 400。
func TestChatToolsTooManyDegrades(t *testing.T) {
	up := newFakeUpstream(t, 0)
	h := newTestServer(t, up, 1, nil)

	var tools []string
	for i := 0; i < 300; i++ {
		tools = append(tools, fmt.Sprintf(`{"type":"function","function":{"name":"t%d"}}`, i))
	}
	body := `{"model":"qwen3.8-omni-flash","messages":[{"role":"user","content":"hi"}],"tools":[` +
		strings.Join(tools, ",") + `]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if rec.Code != 200 {
		t.Fatalf("数量超限不该 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	warn, _ := url.QueryUnescape(rec.Header().Get("X-Qwen2API-Warnings"))
	if !strings.Contains(warn, "工具数量过多") {
		t.Fatalf("应说明数量超限: %q", warn)
	}
}
