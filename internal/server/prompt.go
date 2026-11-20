package server

import (
	"errors"
	"fmt"
	"strings"
)

// errEmptyInput Responses 前门：input 为空。
var errEmptyInput = errors.New("input 不能为空（支持字符串或 {role, content} 数组）")

// errToolsUnsupported 工具调用：上游网页端没有函数调用通道。
var errToolsUnsupported = errors.New(
	"工具调用暂不支持：qwen 网页端没有函数调用通道，网关不伪造 tool_calls —— " +
		"请改用普通对话，或在客户端侧关闭工具")

// FlattenResult 拍平结果。
type FlattenResult struct {
	Prompt string
	// LastUser 本轮用户输入的原文（只有"最后一条是 user"时才给）。
	//
	// 用途是**粘性命中时只发这一条**（见 submitPrompts）：上游会话里已经有
	// 前几轮的历史，再把全量拍平文本发一遍等于把历史写进 prompt 两遍 ——
	// 多付 token，还可能让模型对"哪份历史为准"产生歧义。
	// 空串 = 本轮末尾不是 user（续写场景），调用方必须退回全量。
	LastUser string
	// Degraded 有值表示这轮做了信息损失处理（多轮/系统提示被合并），
	// 只进服务端日志，不下发响应（对齐 rsfree 的"降级说明不下发"口径）。
	Degraded string
}

// Flatten 把 OpenAI messages 拍平成上游单条 prompt。
//
// 为什么要拍平：上游每次只发一条 user 消息，而上下文靠 chat_id 的 parent 链承载。
// 网关每次请求都新建会话（无状态），所以必须把历史显式写进文本。
//
// 形态选择的取舍：
//   - **单条 user、无 system、无历史** ⇒ 原文直出。这是绝大多数客户端的第一轮，
//     直出能让模型行为与网页端完全一致（不引入任何包装词）；
//   - 其余情况 ⇒ 结构化拼接（系统指令 / 对话历史 / 当前问题），标签清晰可读。
func Flatten(msgs []ChatMessage) (FlattenResult, error) {
	type item struct {
		role string
		text string
	}
	// 调用 id → 工具名：role=tool 的消息只有 tool_call_id，不建索引就只能
	// 把结果渲染成一段没有来处的 JSON（模型无法判断这是哪个工具的输出）。
	callNames := map[string]string{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if id := strings.TrimSpace(tc.ID); id != "" {
				if name := strings.TrimSpace(tc.Function.Name); name != "" {
					callNames[id] = name
				}
			}
		}
	}

	items := make([]item, 0, len(msgs))
	for _, m := range msgs {
		// 图片不参与拍平：它走 collectImageRefs → 上游 files[]（file_class=vision），
		// 文本只拼文字部分。
		text, err := m.Text()
		if err != nil {
			return FlattenResult{}, err
		}
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "system", "developer":
			role = "system"
		case "tool", "function":
			// 工具结果对上游没有对应概念，按「工具结果」标签并入历史。
			// 🔴 不能并成 user：模型会把工具输出当成用户说的话，进而答非所问。
			text = renderToolResult(text, m, callNames)
			role = "tool"
		case "assistant":
			if len(m.ToolCalls) > 0 {
				text = renderAssistantCalls(text, m.ToolCalls)
			}
		default:
			role = "user"
		}
		items = append(items, item{role: role, text: text})
	}
	if len(items) == 0 {
		return FlattenResult{}, fmt.Errorf("messages 为空")
	}

	// 直出路径：单条 user 且是最后一条。
	if len(items) == 1 && items[0].role == "user" {
		return FlattenResult{Prompt: items[0].text, LastUser: items[0].text}, nil
	}

	var systems []string
	var turns []item
	for _, it := range items {
		if it.role == "system" {
			if s := strings.TrimSpace(it.text); s != "" {
				systems = append(systems, s)
			}
			continue
		}
		turns = append(turns, it)
	}
	if len(turns) == 0 {
		return FlattenResult{Prompt: strings.Join(systems, "\n\n")}, nil
	}

	var sb strings.Builder
	degraded := ""
	if len(systems) > 0 {
		sb.WriteString("系统指令：\n")
		sb.WriteString(strings.Join(systems, "\n\n"))
		sb.WriteString("\n\n")
	}

	// 最后一条通常是当前问题；之前的是历史。
	last := turns[len(turns)-1]
	history := turns[:len(turns)-1]
	if len(history) > 0 {
		sb.WriteString("对话历史：\n")
		for _, h := range history {
			sb.WriteString(roleLabel(h.role))
			sb.WriteString("：")
			sb.WriteString(strings.TrimSpace(h.text))
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
		degraded = fmt.Sprintf("多轮已拍平：%d 条历史 + 当前问题", len(history))
	}
	switch last.role {
	case "assistant":
		// 末尾是 assistant（预填续写场景）：标注清楚，别让模型以为在自问自答。
		sb.WriteString("请接着上一条助手回复继续：\n")
		degraded = joinDegraded(degraded, "末尾 assistant 作为续写前缀")
	case "tool":
		// 末尾是工具结果：这是"执行完了，接着答"的场景，不能写成"当前问题"。
		// 带上「工具结果：」标签（末尾这一段没有走历史行的标签逻辑）。
		sb.WriteString("以上是工具执行结果，请据此继续回答用户最初的问题（不要重复调用同一工具）：\n工具结果：")
		degraded = joinDegraded(degraded, "末尾工具结果作为续答输入")
	default:
		sb.WriteString("当前问题：\n")
	}
	sb.WriteString(strings.TrimSpace(last.text))

	if len(systems) > 0 {
		degraded = joinDegraded(degraded, fmt.Sprintf("system 已并入 prompt（%d 条）", len(systems)))
	}
	// 只有末尾是 user 才给增量变体：末尾是 assistant 时上游要的是"续写"，
	// 单独发那一条会让模型以为那是用户在说话，语义完全变了。
	lastUser := ""
	if last.role == "user" {
		lastUser = strings.TrimSpace(last.text)
	}
	return FlattenResult{Prompt: sb.String(), LastUser: lastUser, Degraded: degraded}, nil
}

func joinDegraded(a, b string) string {
	if a == "" {
		return b
	}
	return a + "；" + b
}

// roleLabel 拍平后历史里的角色标签。
func roleLabel(role string) string {
	switch role {
	case "assistant":
		return "助手"
	case "tool":
		return "工具结果"
	default:
		return "用户"
	}
}

// renderAssistantCalls 把上一轮 assistant 的工具调用写进历史文本。
//
// 形态：`<正文>\n[调用工具] get_weather({"city":"北京"})`
//
// 为什么参数也要写：模型下一轮要靠这段判断"这个结果是不是我那次调用的"，
// 只写工具名会让它在参数不同的两次调用之间混淆（实测会重复调用）。
func renderAssistantCalls(text string, calls []ToolCallMsg) string {
	var sb strings.Builder
	if t := strings.TrimSpace(text); t != "" {
		sb.WriteString(t)
		if len(calls) > 0 {
			sb.WriteString("\n")
		}
	}
	for i, c := range calls {
		if i > 0 {
			sb.WriteString("\n")
		}
		args := strings.TrimSpace(c.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		sb.WriteString("[调用工具] ")
		sb.WriteString(strings.TrimSpace(c.Function.Name))
		sb.WriteString("(")
		sb.WriteString(args)
		sb.WriteString(")")
	}
	return sb.String()
}

// renderToolResult 把 role=tool 的消息渲染成「工具结果」条目。
//
// 只返回 `工具名 → 内容`（标签由调用方加）：历史行已有「工具结果：」前缀，
// 这里再带一次就成了「工具结果：[工具结果] …」。
//
// 工具名优先用 tool_call_id 反查（多工具并行时唯一可靠的配对方式）；
// 查不到就退回消息自带 name，再退回纯内容 —— 宁可不带名字，也不猜错名字。
func renderToolResult(text string, m ChatMessage, names map[string]string) string {
	name := ""
	if id := strings.TrimSpace(m.ToolCallID); id != "" {
		name = names[id]
	}
	if name == "" {
		name = strings.TrimSpace(m.Name)
	}
	body := strings.TrimSpace(text)
	if name != "" {
		return name + " → " + body
	}
	return body
}
