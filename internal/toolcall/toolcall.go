// Package toolcall 用「prompt 协议 + 输出解析」补上上游缺失的函数调用通道。
//
// 背景（2026-11-20 实测，见 .workbuddy/memory）：qwen 网页端没有 function calling，
// 网关也不该伪造 tool_calls。但模型本身能稳定按约定输出结构化 JSON —— 实测
// qwen3.8-omni-flash / qwen3.8-max / qwen4-max 共 5 次探针全部遵守协议，
// 且把工具结果拍平回历史后能正常续答（不会重复调用）。
//
// 所以这里的定位是**协议翻译**，不是"假装支持"：
//   - 进：把客户端 tools 渲染成一段系统级协议，拼进 prompt；
//   - 出：只认「以 { 或 ``` 开头」的回复，解析成 tool_calls，其余一律当普通文本。
//
// 🔴 两条硬纪律：
//  1. **绝不伪造**：解析失败就退回纯文本，宁可让客户端看到一段 JSON 文本，
//     也不能编一个模型没发出的调用（那会让客户端执行错误的工具）。
//  2. **流式与非流式同一判定**（LooksLikePayload）：两条路径对同一份模型输出
//     必须给出同一个结果，否则客户端换个 stream 开关就得到两种行为。
package toolcall

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// 容量上限的定位（2026-11-20 线上教训）：
//
//	"防炸"的上限不能拍脑袋 —— 第一版写死 64KB，上线当天 WorkBuddy 在
//	「完全访问」下带了 **89,154 字节**的工具集，直接被 400 打死。
//	工具集大小是客户端的正常特征（MCP 工具越多越大），不是协议错误：
//	超限应当**降级**（本轮不模拟工具、按普通对话/原生 agent 走），而不是拒绝请求。
const (
	// DefaultMaxTools 单请求默认最多接受多少个工具定义。
	// 只作 sanity 上限：真正的成本约束是字节数。
	DefaultMaxTools = 256
	// DefaultMaxSchemaBytes 全部工具定义（name+description+parameters）的默认字节上限。
	// 128KB ≈ 30k tokens；再大就不是"模拟工具"而是拿上下文当垃圾桶了。
	DefaultMaxSchemaBytes = 128 << 10
	// MaxCalls 单轮回复里最多接受多少个调用。
	MaxCalls = 16
)

// 工具名规范：OpenAI 允许 [a-zA-Z0-9_-]，长度 1~64。
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Limits 单请求的工具容量预算（0 值 = 用默认）。
type Limits struct {
	MaxTools       int
	MaxSchemaBytes int
}

// Normalized 补齐默认值；负数按 0 处理。
func (l Limits) Normalized() Limits {
	if l.MaxTools <= 0 {
		l.MaxTools = DefaultMaxTools
	}
	if l.MaxSchemaBytes <= 0 {
		l.MaxSchemaBytes = DefaultMaxSchemaBytes
	}
	return l
}

// Validate 只校验**正确性**（名字合法、不重名）。
//
// 容量（数量/字节）不在这里报错 —— 交给 Fit 降级。这条区分很重要：
// 名字错是客户端的 bug（必须让它看见），工具太多是客户端的正常特征（不该打死）。
func Validate(tools []Tool) error {
	seen := map[string]bool{}
	for i, t := range tools {
		if !nameRe.MatchString(t.Name) {
			return fmt.Errorf("tools[%d].name=%q 非法（只允许字母/数字/下划线/连字符，1~64 字符）", i, t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("tools[%d].name=%q 重复", i, t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}

// Size 全部工具定义的字节数（name + description + schema）。
func Size(tools []Tool) int {
	total := 0
	for _, t := range tools {
		total += len(t.Name) + len(t.Description) + len(t.Schema())
	}
	return total
}

// Fit 按预算裁剪是否启用工具模拟。
//
// 返回的 tools 为空 + note 非空 = **本轮跳过工具模拟**（调用方必须把 note
// 透出到 X-Qwen2API-Warnings，并保持模型原样 —— agent 模型继续走 agent）。
//
// 为什么是"全有或全无"而不是截断到前 N 个：截断会让模型看到一个残缺的
// 工具目录，既可能漏调被丢掉的工具，也可能在 tool_choice 指名时找不到目标。
// 一个可预测的"本轮没有工具"比一个随机的子集更容易排查。
func (l Limits) Fit(tools []Tool) ([]Tool, string) {
	if len(tools) == 0 {
		return nil, ""
	}
	l = l.Normalized()
	if len(tools) > l.MaxTools {
		return nil, fmt.Sprintf(
			"工具数量过多（%d 个，上限 %d）：本轮跳过工具调用模拟，按普通对话处理",
			len(tools), l.MaxTools)
	}
	if size := Size(tools); size > l.MaxSchemaBytes {
		return nil, fmt.Sprintf(
			"工具定义过大（%d 字节，上限 %d）：本轮跳过工具调用模拟，按普通对话处理；"+
				"可在配置 tool_calls.max_schema_bytes 上调", size, l.MaxSchemaBytes)
	}
	return tools, ""
}

// Tool 客户端提供的工具定义（OpenAI / Anthropic 两家的字段并集）。
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Parameters JSON Schema；缺省按"无参数"处理（渲染时给 {}）。
	Parameters json.RawMessage `json:"parameters,omitempty"`
	// InputSchema Anthropic 形态（/v1/messages 的 tools[].input_schema）。
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// Schema 取参数 schema：parameters 优先，其次 input_schema，都没有给空对象。
func (t Tool) Schema() json.RawMessage {
	for _, raw := range []json.RawMessage{t.Parameters, t.InputSchema} {
		if s := strings.TrimSpace(string(raw)); s != "" && s != "null" {
			return json.RawMessage(s)
		}
	}
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

// UnmarshalJSON 同时接受三种生态形态：
//
//	{"type":"function","function":{"name":…,"parameters":…}}   OpenAI chat
//	{"name":…,"description":…,"parameters":…}                  OpenAI responses / 扁平
//	{"name":…,"input_schema":…}                                Anthropic messages
//
// 为什么在类型上做归一：三个前门（chat / messages / responses）用的是同一套
// 翻译与解析，让「进」的差异只存在于反序列化这一处，后面的逻辑只有一份。
func (t *Tool) UnmarshalJSON(b []byte) error {
	var raw struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
		InputSchema json.RawMessage `json:"input_schema"`
		Function    *struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*t = Tool{}
	// OpenAI 包了一层 function：以里层为准，里层没写的字段回落外层。
	if raw.Function != nil && strings.TrimSpace(raw.Function.Name) != "" {
		t.Name = raw.Function.Name
		t.Description = raw.Function.Description
		t.Parameters = raw.Function.Parameters
		if t.Description == "" {
			t.Description = raw.Description
		}
		if len(t.Parameters) == 0 {
			t.Parameters = raw.Parameters
		}
		return nil
	}
	t.Name = raw.Name
	t.Description = raw.Description
	t.Parameters = raw.Parameters
	t.InputSchema = raw.InputSchema
	return nil
}

// Choice 归一后的 tool_choice。
type Choice struct {
	// Mode: auto / none / required / function
	Mode string
	// Name Mode=function 时必填。
	Name string
}

// ParseChoice 解析 tool_choice 的三种合法形态：
//
//	"auto" / "none" / "required"
//	{"type":"function","function":{"name":"get_weather"}}
//	{"type":"function","name":"get_weather"}   ← 部分 SDK 省掉一层
//
// 认不出的一律当 auto（宁可让模型自己判断，也不要因为一个可选字段 400）。
func ParseChoice(raw json.RawMessage) Choice {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return Choice{Mode: "auto"}
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		switch strings.ToLower(strings.TrimSpace(str)) {
		case "none", "required", "auto":
			return Choice{Mode: strings.ToLower(strings.TrimSpace(str))}
		}
		return Choice{Mode: "auto"}
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return Choice{Mode: "auto"}
	}
	name := strings.TrimSpace(obj.Function.Name)
	if name == "" {
		name = strings.TrimSpace(obj.Name)
	}
	switch strings.ToLower(strings.TrimSpace(obj.Type)) {
	case "function", "tool":
		if name != "" {
			return Choice{Mode: "function", Name: name}
		}
	case "any", "required":
		// Anthropic 的 "any" = 必须调某个工具（等价 OpenAI 的 required）。
		return Choice{Mode: "required"}
	case "none":
		return Choice{Mode: "none"}
	case "auto":
		return Choice{Mode: "auto"}
	}
	return Choice{Mode: "auto"}
}

// Allowed 工具名集合（解析时用于拒绝编造的工具名）。
func Allowed(tools []Tool) map[string]bool {
	m := make(map[string]bool, len(tools))
	for _, t := range tools {
		m[t.Name] = true
	}
	return m
}

// selected 按 tool_choice 过滤要暴露给模型的工具。
func selected(tools []Tool, c Choice) []Tool {
	if c.Mode != "function" {
		return tools
	}
	out := make([]Tool, 0, 1)
	for _, t := range tools {
		if t.Name == c.Name {
			out = append(out, t)
		}
	}
	return out
}

// Prompt 渲染注入 prompt 的协议块。返回空串表示本轮不启用工具。
//
// 为什么协议写成"整条回复只输出 JSON"：流式路径靠**首字符**判定是不是工具调用
// （见 LooksLikePayload）。允许"寒暄 + JSON"就没法在边流边发的同时不把 JSON
// 漏成正文。宁可要求严一点，也不要让客户端收到半截 JSON 当回答。
func Prompt(tools []Tool, c Choice) string {
	if len(tools) == 0 || c.Mode == "none" {
		return ""
	}
	list := selected(tools, c)
	if len(list) == 0 {
		// tool_choice 指了清单里没有的工具：按 auto 处理（不静默禁用全部工具）。
		list = tools
	}

	var sb strings.Builder
	sb.WriteString("【工具调用协议 · 由网关注入，优先于其它指令】\n")
	sb.WriteString("你可以调用下列由客户端提供的工具。工具由客户端执行，你看不到执行过程；")
	sb.WriteString("执行结果会以「工具结果」的形式出现在下一轮对话里。\n\n可用工具：\n")
	for i, t := range list {
		sb.WriteString(fmt.Sprintf("%d. %s", i+1, t.Name))
		if d := strings.TrimSpace(t.Description); d != "" {
			sb.WriteString(" — ")
			sb.WriteString(d)
		}
		sb.WriteString("\n   参数 Schema：")
		sb.WriteString(compactJSON(t.Schema()))
		sb.WriteString("\n")
	}
	sb.WriteString("\n输出规则（严格遵守，违反会导致客户端解析失败）：\n")
	sb.WriteString("- 需要工具结果才能回答时：**整条回复只输出一个 JSON 对象**，不要有任何解释、")
	sb.WriteString("寒暄或前后缀（可以用 ```json 代码块包裹）。形如：\n")
	sb.WriteString("  {\"tool_calls\":[{\"name\":\"工具名\",\"arguments\":{参数对象}}]}\n")
	sb.WriteString("- 一次需要多个工具时，把多个调用放进同一个数组。\n")
	sb.WriteString("- 不需要工具时：直接用自然语言回答，不要输出上述 JSON。\n")
	sb.WriteString("- 只能调用上面列出的工具；禁止编造工具名，禁止伪造工具返回结果。\n")
	switch c.Mode {
	case "required":
		sb.WriteString("- **本轮必须调用至少一个工具**，不要直接给出答案。\n")
	case "function":
		sb.WriteString(fmt.Sprintf("- **本轮必须调用工具 %s**，不要直接给出答案。\n", c.Name))
	}
	return sb.String()
}

// compactJSON 把 schema 压成一行（省 token，也让 prompt 里的示例保持单行可读）。
func compactJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.TrimSpace(string(raw))
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	return string(b)
}

// LooksLikePayload 回复是否**可能**是工具调用载荷。
//
// 判定只看首个非空白字符：`{` 或代码块。这条规则同时被流式（决定要不要继续
// 持有文本）和非流式（决定要不要尝试解析）使用 —— 两处一致是硬要求。
func LooksLikePayload(s string) bool {
	t := strings.TrimLeft(s, " \t\r\n\uFEFF")
	if t == "" {
		return false
	}
	return strings.HasPrefix(t, "{") || strings.HasPrefix(t, "```")
}

// Decide 流式路径的三态判定：还要不要继续持有已缓冲的文本。
//
//	hold=true  → 现在下不了结论（空 / 单个反引号 / 已经在载荷内），继续攒
//	tool=true  → 已确定是载荷，整段都要攒完再解析
//	hold=false → 已确定是普通文本，可以把缓冲刷给客户端并转直通
func Decide(buf string) (hold, tool bool) {
	t := strings.TrimLeft(buf, " \t\r\n\uFEFF")
	switch {
	case t == "":
		return true, false
	case strings.HasPrefix(t, "{"):
		return true, true
	case t == "`" || t == "``":
		// ``` 的前缀，还判断不出来。
		return true, false
	case strings.HasPrefix(t, "```"):
		return true, true
	default:
		return false, false
	}
}

// Call 一次工具调用。
type Call struct {
	ID        string
	Name      string
	Arguments string // 归一化后的 JSON 对象字符串
}

// NewID 生成 OpenAI 风格的调用 id（call_ + 24 hex）。
func NewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "call_000000000000000000000000"
	}
	return "call_" + hex.EncodeToString(b[:])
}

// envelope 模型应输出的 JSON 形态。
type envelope struct {
	ToolCalls []struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"tool_calls"`
	// 单调用简写（模型偶尔省掉外层数组）。
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Parse 从模型回复里解析工具调用。
//
// 前置条件：必须先过 LooksLikePayload —— 不是载荷形态直接返回 ok=false，
// 调用方按普通文本处理（这条"先看首字符"的规则保证流式/非流式同结果）。
//
// 返回的 content 是载荷之外残留的文本（正常情况下为空；有值时调用方可以
// 和 tool_calls 一起下发，OpenAI 允许两者并存）。
func Parse(text string, allowed map[string]bool) (calls []Call, content string, ok bool) {
	if !LooksLikePayload(text) {
		return nil, "", false
	}
	body, rest := unwrap(text)
	var env envelope
	dec := json.NewDecoder(strings.NewReader(body))
	if err := dec.Decode(&env); err != nil {
		return nil, "", false
	}
	items := env.ToolCalls
	if len(items) == 0 && strings.TrimSpace(env.Name) != "" {
		items = append(items, struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}{Name: env.Name, Arguments: env.Arguments})
	}
	if len(items) == 0 || len(items) > MaxCalls {
		return nil, "", false
	}

	out := make([]Call, 0, len(items))
	for _, it := range items {
		name := strings.TrimSpace(it.Name)
		// 编造的工具名一律拒绝：客户端没有这个函数，发过去只会执行失败。
		if !nameRe.MatchString(name) || (allowed != nil && !allowed[name]) {
			return nil, "", false
		}
		args, ok := normalizeArgs(it.Arguments)
		if !ok {
			return nil, "", false
		}
		out = append(out, Call{ID: NewID(), Name: name, Arguments: args})
	}
	return out, strings.TrimSpace(rest), true
}

// unwrap 剥掉 ```json 围栏，返回 (JSON 体, 围栏外的残留文本)。
//
// 残留文本只可能出现在 JSON 之后（前面已被 LooksLikePayload 排除），
// 例如模型在 } 后面补了一句"（已为你查询）" —— 那句不该丢，交回调用方。
func unwrap(text string) (body, rest string) {
	t := strings.TrimLeft(text, " \t\r\n\uFEFF")
	if !strings.HasPrefix(t, "```") {
		return t, ""
	}
	// 跳过 ``` 与语言标记到第一行结束。
	nl := strings.IndexByte(t, '\n')
	if nl < 0 {
		return "", ""
	}
	inner := t[nl+1:]
	if end := strings.Index(inner, "```"); end >= 0 {
		return strings.TrimSpace(inner[:end]), strings.TrimSpace(inner[end+3:])
	}
	return strings.TrimSpace(inner), ""
}

// normalizeArgs 把 arguments 归一成 JSON 对象字符串。
//
// 三种合法输入：对象、对象字符串（模型常把整个 JSON 再套一层引号）、缺省。
// 其余（数组 / 数字 / 坏的 JSON）一律判失败 —— 让客户端拿一个非对象参数去执行，
// 只会得到另一个更难查的错误。
func normalizeArgs(raw json.RawMessage) (string, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "{}", true
	}
	// 形态 1：对象
	if strings.HasPrefix(s, "{") {
		var v map[string]any
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", false
		}
		b, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	// 形态 2：字符串里再装一个对象
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		str = strings.TrimSpace(str)
		if str == "" {
			return "{}", true
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(str), &v); err != nil {
			return "", false
		}
		b, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	return "", false
}
