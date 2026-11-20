package toolcall

import (
	"encoding/json"
	"strings"
	"testing"
)

func weather() []Tool {
	return []Tool{{
		Name:        "get_weather",
		Description: "查询指定城市的实时天气",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
	}}
}

func TestPromptContainsSchemaAndRules(t *testing.T) {
	p := Prompt(weather(), Choice{Mode: "auto"})
	for _, want := range []string{
		"get_weather", "查询指定城市的实时天气",
		`"city":{"type":"string"}`,
		"整条回复只输出一个 JSON 对象",
		`{"tool_calls":[{"name":"工具名","arguments":{参数对象}}]}`,
		"禁止编造工具名",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("协议块缺少 %q", want)
		}
	}
	if strings.Contains(p, "本轮必须调用") {
		t.Error("auto 模式不应出现强制调用条款")
	}
}

func TestPromptChoiceModes(t *testing.T) {
	// none ⇒ 不注入（客户端明确要求这轮不调工具）
	if got := Prompt(weather(), Choice{Mode: "none"}); got != "" {
		t.Errorf("tool_choice=none 应返回空串，得到 %q", got)
	}
	if got := Prompt(weather(), Choice{Mode: "required"}); !strings.Contains(got, "本轮必须调用至少一个工具") {
		t.Error("required 缺少强制条款")
	}
	// function ⇒ 只暴露那一个工具
	two := append(weather(), Tool{Name: "get_time", Description: "现在几点"})
	p := Prompt(two, Choice{Mode: "function", Name: "get_weather"})
	if !strings.Contains(p, "get_weather") || strings.Contains(p, "get_time") {
		t.Errorf("function 模式应只暴露指定工具：\n%s", p)
	}
	if !strings.Contains(p, "必须调用工具 get_weather") {
		t.Error("function 模式缺少强制条款")
	}
	// 指名了清单里没有的工具：退回 auto（不静默禁用全部工具）
	if p := Prompt(two, Choice{Mode: "function", Name: "nope"}); !strings.Contains(p, "get_time") {
		t.Error("指名未知工具时应退回全量工具")
	}
}

func TestParseChoice(t *testing.T) {
	cases := []struct {
		raw  string
		want Choice
	}{
		{``, Choice{Mode: "auto"}},
		{`"auto"`, Choice{Mode: "auto"}},
		{`"none"`, Choice{Mode: "none"}},
		{`"required"`, Choice{Mode: "required"}},
		{`{"type":"function","function":{"name":"get_weather"}}`, Choice{Mode: "function", Name: "get_weather"}},
		{`{"type":"function","name":"get_weather"}`, Choice{Mode: "function", Name: "get_weather"}},
		{`{"type":"oops"}`, Choice{Mode: "auto"}},
		{`123`, Choice{Mode: "auto"}},
	}
	for _, c := range cases {
		if got := ParseChoice(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("ParseChoice(%s) = %+v, want %+v", c.raw, got, c.want)
		}
	}
}

func TestParseBareJSON(t *testing.T) {
	text := `{"tool_calls":[{"name":"get_weather","arguments":{"city":"北京"}}]}`
	calls, content, ok := Parse(text, Allowed(weather()))
	if !ok || len(calls) != 1 {
		t.Fatalf("应解析出 1 个调用，得到 ok=%v calls=%d", ok, len(calls))
	}
	if calls[0].Name != "get_weather" || calls[0].Arguments != `{"city":"北京"}` {
		t.Errorf("调用内容不对: %+v", calls[0])
	}
	if !strings.HasPrefix(calls[0].ID, "call_") || len(calls[0].ID) != 29 {
		t.Errorf("调用 id 形态不对: %q", calls[0].ID)
	}
	if content != "" {
		t.Errorf("不应有残留文本: %q", content)
	}
}

func TestParseFenced(t *testing.T) {
	text := "```json\n{\"tool_calls\":[{\"name\":\"get_weather\",\"arguments\":{\"city\":\"上海\"}}]}\n```"
	calls, _, ok := Parse(text, Allowed(weather()))
	if !ok || len(calls) != 1 || calls[0].Arguments != `{"city":"上海"}` {
		t.Fatalf("围栏形态解析失败: ok=%v calls=%+v", ok, calls)
	}
	// 围栏后补的一句话不能丢
	text2 := "```json\n{\"tool_calls\":[{\"name\":\"get_weather\",\"arguments\":{}}]}\n```\n（已为你查询）"
	_, rest, ok := Parse(text2, Allowed(weather()))
	if !ok || rest != "（已为你查询）" {
		t.Errorf("围栏后的残留文本应保留，得到 %q (ok=%v)", rest, ok)
	}
}

func TestParseRejects(t *testing.T) {
	allowed := Allowed(weather())
	cases := map[string]string{
		"编造的工具名":        `{"tool_calls":[{"name":"rm_rf","arguments":{}}]}`,
		"参数不是对象":        `{"tool_calls":[{"name":"get_weather","arguments":[1,2]}]}`,
		"参数是坏 JSON":     `{"tool_calls":[{"name":"get_weather","arguments":"{oops"}]}`,
		"空数组":           `{"tool_calls":[]}`,
		"不是 JSON":       `{"tool_calls":[}`,
		"prose 开头":      "我来查一下：\n{\"tool_calls\":[{\"name\":\"get_weather\",\"arguments\":{}}]}",
		"纯文本":           "北京今天晴，12 度。",
		"合法但名字带空格":      `{"tool_calls":[{"name":"get weather","arguments":{}}]}`,
		"超过单轮调用上限":      `{"tool_calls":[` + strings.Repeat(`{"name":"get_weather","arguments":{}},`, MaxCalls) + `{"name":"get_weather","arguments":{}}]}`,
		"只有 code fence": ` ` + "```",
	}
	for name, text := range cases {
		if calls, _, ok := Parse(text, allowed); ok {
			t.Errorf("%s: 不应解析成功，得到 %+v", name, calls)
		}
	}
}

func TestParseArgumentsStringAndMissing(t *testing.T) {
	// 模型常把 arguments 再套一层字符串
	calls, _, ok := Parse(`{"tool_calls":[{"name":"get_weather","arguments":"{\"city\":\"广州\"}"}]}`, Allowed(weather()))
	if !ok || calls[0].Arguments != `{"city":"广州"}` {
		t.Fatalf("字符串参数未归一: ok=%v %+v", ok, calls)
	}
	// 无参工具
	calls, _, ok = Parse(`{"tool_calls":[{"name":"get_weather"}]}`, Allowed(weather()))
	if !ok || calls[0].Arguments != "{}" {
		t.Fatalf("缺省参数应为 {}: ok=%v %+v", ok, calls)
	}
}

func TestParseSingleCallShorthand(t *testing.T) {
	calls, _, ok := Parse(`{"name":"get_weather","arguments":{"city":"深圳"}}`, Allowed(weather()))
	if !ok || len(calls) != 1 {
		t.Fatalf("单调用简写应被接受: ok=%v %+v", ok, calls)
	}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		buf        string
		hold, tool bool
	}{
		{"", true, false},
		{"  \n", true, false},
		{"`", true, false},
		{"``", true, false},
		{"```", true, true},
		{"```json", true, true},
		{`{"tool_calls"`, true, true},
		{"北京", false, false},
		{"好的，", false, false},
		{"\n\n北京今天", false, false},
	}
	for _, c := range cases {
		hold, tool := Decide(c.buf)
		if hold != c.hold || tool != c.tool {
			t.Errorf("Decide(%q) = (%v,%v), want (%v,%v)", c.buf, hold, tool, c.hold, c.tool)
		}
	}
}

func TestValidateChecksNamesOnly(t *testing.T) {
	if err := Validate(weather()); err != nil {
		t.Errorf("合法清单报错: %v", err)
	}
	if err := Validate([]Tool{{Name: "a b"}}); err == nil {
		t.Error("非法名字应报错")
	}
	if err := Validate([]Tool{{Name: "dup"}, {Name: "dup"}}); err == nil {
		t.Error("重名应报错")
	}
	// 容量不是 Validate 的事：它只查正确性（超限由 Fit 降级）。
	many := make([]Tool, DefaultMaxTools+1)
	for i := range many {
		many[i] = Tool{Name: "t" + string(rune('a'+i%26)) + string(rune('a'+i/26))}
	}
	if err := Validate(many); err != nil {
		t.Errorf("数量超限不该在 Validate 报错: %v", err)
	}
}

func TestFit(t *testing.T) {
	// 正常：原样返回，没有说明
	if got, note := (Limits{}).Fit(weather()); len(got) != 1 || note != "" {
		t.Fatalf("正常清单应原样通过: %d %q", len(got), note)
	}
	// 数量超限：降级（返回空 + 说明），不报错
	many := make([]Tool, 5)
	for i := range many {
		many[i] = Tool{Name: "t" + string(rune('a'+i))}
	}
	got, note := Limits{MaxTools: 3}.Fit(many)
	if len(got) != 0 || !strings.Contains(note, "工具数量过多") {
		t.Fatalf("数量超限应降级: %d %q", len(got), note)
	}
	// 字节超限：降级
	big := []Tool{{Name: "big", Description: strings.Repeat("x", 2048)}}
	got, note = Limits{MaxSchemaBytes: 1024}.Fit(big)
	if len(got) != 0 || !strings.Contains(note, "工具定义过大") {
		t.Fatalf("字节超限应降级: %d %q", len(got), note)
	}
	// 默认预算的边界（2026-11-20 线上实测定的 32KB）：
	//   · 20KB 工具集 ⇒ 正常模拟（Claude Code / Cline 量级）
	//   · 89KB（WorkBuddy「完全访问」）⇒ 跳过（94KB 实测 45.8s 且 0 次调用）
	if got, note := (Limits{}).Fit([]Tool{{Name: "cc", Description: strings.Repeat("z", 20<<10)}}); len(got) != 1 || note != "" {
		t.Fatalf("20KB 工具集应正常模拟: %q", note)
	}
	if got, note := (Limits{}).Fit([]Tool{{Name: "wb", Description: strings.Repeat("y", 89<<10)}}); len(got) != 0 || !strings.Contains(note, "跳过") {
		t.Fatalf("89KB 工具集应跳过模拟（大目录既不快也不准）: %q", note)
	}
}

func TestSchemaPrefersParametersThenInputSchema(t *testing.T) {
	if got := string((Tool{Parameters: json.RawMessage(`{"a":1}`), InputSchema: json.RawMessage(`{"b":2}`)}).Schema()); got != `{"a":1}` {
		t.Errorf("parameters 应优先: %s", got)
	}
	if got := string((Tool{InputSchema: json.RawMessage(`{"b":2}`)}).Schema()); got != `{"b":2}` {
		t.Errorf("无 parameters 时应回落 input_schema: %s", got)
	}
	if got := string((Tool{}).Schema()); !strings.Contains(got, `"object"`) {
		t.Errorf("都没有时应给空对象 schema: %s", got)
	}
}

func TestToolUnmarshalShapes(t *testing.T) {
	cases := map[string]string{
		"openai chat": `{"type":"function","function":{"name":"get_weather","description":"查天气","parameters":{"type":"object"}}}`,
		"扁平":          `{"name":"get_weather","description":"查天气","parameters":{"type":"object"}}`,
		"anthropic":   `{"name":"get_weather","description":"查天气","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}`,
		"嵌套缺参数回落":     `{"name":"get_weather","parameters":{"type":"object","x":1},"function":{"name":"get_weather"}}`,
	}
	for name, raw := range cases {
		var tool Tool
		if err := json.Unmarshal([]byte(raw), &tool); err != nil {
			t.Fatalf("%s: 解析失败 %v", name, err)
		}
		if tool.Name != "get_weather" {
			t.Errorf("%s: name=%q", name, tool.Name)
		}
		if !strings.Contains(string(tool.Schema()), "object") {
			t.Errorf("%s: schema 丢失: %s", name, tool.Schema())
		}
		if err := Validate([]Tool{tool}); err != nil {
			t.Errorf("%s: 校验失败 %v", name, err)
		}
	}
}
