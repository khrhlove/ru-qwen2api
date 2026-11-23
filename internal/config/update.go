package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Field 面板「配置」页的一个字段。
//
// 只在这里登记的字段才允许从面板写回 —— 白名单而不是黑名单：
// 配置里有 accounts / api_keys / panel.session_secret 这类"写错就把服务锁死"的项，
// 黑名单漏一个就是线上事故。
type Field struct {
	// Key 点分路径，如 `pool.max_rotate`。
	Key   string `json:"key"`
	Label string `json:"label"`
	// Type int | bool | string | enum | kv
	//
	// kv 是 map[string]string（如 video.aliases）：面板渲染成「每行 key=value」，
	// 提交时仍是 JSON 对象。为什么不用 string 存 JSON 文本：那会让「写错一个引号」
	// 变成一次启动失败，而 kv 的解析错误能在保存时就指到具体哪一行。
	Type string `json:"type"`
	// Min / Max 仅 int 有效（闭区间）。
	Min int `json:"min,omitempty"`
	Max int `json:"max,omitempty"`
	// Enum 仅 enum 有效。
	Enum []string `json:"enum,omitempty"`
	// Hot 改完是否**立即生效**（false = 必须重启，面板会明确标出来）。
	Hot bool `json:"hot"`
	// Hint 给用户的一句说明（为什么有这个开关 / 改了的后果）。
	Hint string `json:"hint,omitempty"`
}

// Fields 面板可编辑字段（顺序即表单顺序）。
var Fields = []Field{
	{Key: "default_model", Label: "Модель по умолчанию", Type: "string", Hot: true,
		Hint: "Используется, если клиент не передал model."},
	{Key: "default_thinking", Label: "Режим рассуждения", Type: "enum",
		Enum: []string{"auto", "thinking", "fast"}, Hot: true,
		Hint: "auto — автоматически, thinking — с рассуждением, fast — быстрый ответ."},
	{Key: "expose_reasoning", Label: "Передавать рассуждение", Type: "bool", Hot: true,
		Hint: "Добавляет reasoning_content в ответ."},
	{Key: "image_model", Label: "Модель изображений", Type: "string", Hot: true,
		Hint: "Пустое значение оставляет модель на выбор Qwen."},
	{Key: "image_timeout_s", Label: "Время ожидания изображения, с", Type: "int", Min: 30, Max: 900, Hot: true,
		Hint: "Лимит одной попытки генерации."},
	{Key: "model_aliases", Label: "Псевдонимы текстовых моделей", Type: "kv", Hot: true,
		Hint: "Формат: название клиента=модель Qwen, по одной паре на строку."},
	{Key: "blob_base_url", Label: "Адрес хранилища изображений", Type: "string", Hot: true,
		Hint: "Qwen должен иметь доступ к этому адресу, чтобы получить вложение."},

	{Key: "upstream.egress_ip_url", Label: "Адрес проверки исходящего IP", Type: "string", Hot: true,
		Hint: "Значение «-» отключает проверку."},
	{Key: "upstream.timeout_s", Label: "Время ожидания ответа Qwen, с", Type: "int", Min: 10, Max: 900, Hot: false,
		Hint: "Для изменения тайм-аута нужен перезапуск."},

	{Key: "pool.max_rotate", Label: "Лимит попыток смены аккаунта", Type: "int", Min: 1, Max: 10, Hot: true,
		Hint: "Для одного запроса."},
	{Key: "pool.cooldown_soft_s", Label: "Пауза после ограничения, с", Type: "int", Min: 30, Max: 86400, Hot: true,
		Hint: "После временного ограничения запросов."},
	{Key: "pool.cooldown_auth_s", Label: "Пауза после ошибки входа, с", Type: "int", Min: 60, Max: 86400, Hot: true,
		Hint: "После ответов 401 и 403."},
	{Key: "pool.max_inflight_per_account", Label: "Одновременных запросов на аккаунт", Type: "int", Min: 1, Max: 8, Hot: true,
		Hint: "Лимит запросов в работе."},
	{Key: "pool.max_image_per_day", Label: "Изображений в день", Type: "int", Min: 0, Max: 50, Hot: true,
		Hint: "На один аккаунт. 0 — без локального ограничения."},
	{Key: "pool.max_video_per_day", Label: "Видеозадач в день", Type: "int", Min: 0, Max: 50, Hot: true,
		Hint: "Используется, если не заданы цены в кредитах."},
	{Key: "pool.max_video_credits_per_day", Label: "Кредитов на видео в день", Type: "int", Min: 0, Max: 100000, Hot: true,
		Hint: "На один аккаунт. 0 — учитывать фактический баланс."},
	{Key: "pool.warm_chats_per_account", Label: "Подготовленных диалогов на аккаунт", Type: "int", Min: 0, Max: 5, Hot: true,
		Hint: "0 — не создавать диалоги заранее."},
	{Key: "pool.chat_pool_ttl_s", Label: "Срок подготовленного диалога, с", Type: "int", Min: 60, Max: 86400, Hot: true,
		Hint: "По истечении срока шлюз отбросит пустой диалог."},

	// 预热节奏：流量变了就调这里，不用发版。改完下一轮 tick 生效。
	{Key: "pool.keepalive.tick_s", Label: "Интервал обслуживания, с", Type: "int", Min: 5, Max: 300, Hot: true,
		Hint: "Проверка сессий и подготовленных соединений."},
	{Key: "pool.keepalive.token_window_s", Label: "Обновлять сессию за, с", Type: "int", Min: 60, Max: 1800, Hot: true,
		Hint: "Укажите больше интервала обслуживания."},
	{Key: "pool.keepalive.conc_factor", Label: "Коэффициент подготовки", Type: "int", Min: 1, Max: 10, Hot: true,
		Hint: "Определяет число соединений по пиковой нагрузке."},
	{Key: "pool.keepalive.adopt_batch", Label: "Обновлений токена за проход", Type: "int", Min: 1, Max: 100, Hot: true,
		Hint: "Верхний предел для одного прохода."},
	{Key: "pool.keepalive.adopt_per_min", Label: "Обновлений токена в минуту", Type: "int", Min: 1, Max: 60, Hot: true,
		Hint: "Ограничивает частоту фоновых входов."},
	{Key: "pool.keepalive.parallel", Label: "Параллельные проверки", Type: "int", Min: 1, Max: 32, Hot: true,
		Hint: "Число одновременно обслуживаемых аккаунтов."},
	{Key: "pool.keepalive.batch_max", Label: "Аккаунтов за проход", Type: "int", Min: 4, Max: 512, Hot: true,
		Hint: "Верхний предел для одного цикла обслуживания."},
	{Key: "pool.keepalive.target_max", Label: "Максимум подготовленных аккаунтов", Type: "int", Min: 8, Max: 4096, Hot: true,
		Hint: "Верхний предел при росте нагрузки."},
	{Key: "pool.keepalive.hot_rotate_s", Label: "Интервал смены активной группы, с", Type: "int", Min: 0, Max: 86400, Hot: true,
		Hint: "0 — отключить плановую смену."},

	{Key: "video.root_model", Label: "Модель видео", Type: "string", Hot: true,
		Hint: "Идентификатор модели на стороне Qwen."},
	{Key: "video.resolution", Label: "Разрешение видео", Type: "enum",
		Enum: []string{"720P", "1080P"}, Hot: true,
		Hint: "Используется, если клиент не задал разрешение."},
	{Key: "video.duration", Label: "Длительность видео, с", Type: "int", Min: 5, Max: 15, Hot: true,
		Hint: "Поддерживаются 5, 10 и 15 секунд."},
	{Key: "video.size", Label: "Формат видео", Type: "string", Hot: true,
		Hint: "Например, 16:9, 1:1 или 9:16."},
	{Key: "video.aliases", Label: "Псевдонимы моделей видео", Type: "kv", Hot: true,
		Enum: []string{"happyhorse11", "happyhorse", "wan27"},
		Hint: "Формат: название клиента=модель Qwen."},
}

// fieldByKey 查字段定义。
func fieldByKey(key string) (Field, bool) {
	for _, f := range Fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// Normalize 按字段类型校验并归一化一个值（JSON 解出来的 any）。
func (f Field) Normalize(v any) (any, error) {
	switch f.Type {
	case "int":
		var n int
		switch x := v.(type) {
		case float64:
			n = int(x)
			if float64(n) != x {
				return nil, fmt.Errorf("%s: введите целое число", f.Label)
			}
		case int:
			n = x
		case json.Number:
			i, err := x.Int64()
			if err != nil {
				return nil, fmt.Errorf("%s: введите целое число", f.Label)
			}
			n = int(i)
		default:
			return nil, fmt.Errorf("%s: введите целое число", f.Label)
		}
		if n < f.Min || n > f.Max {
			return nil, fmt.Errorf("%s: допустимый диапазон от %d до %d, получено %d", f.Label, f.Min, f.Max, n)
		}
		return n, nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s: выберите true или false", f.Label)
		}
		return b, nil
	case "enum":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s: требуется строка", f.Label)
		}
		for _, e := range f.Enum {
			if e == s {
				return s, nil
			}
		}
		return nil, fmt.Errorf("%s: допустимые значения: %s; получено %q", f.Label, strings.Join(f.Enum, " / "), s)
	case "kv":
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: укажите пары ключ=значение, по одной на строку", f.Label)
		}
		// 防呆：别名是「几个」的量级，200 条必然是脚本灌错了。
		if len(m) > 200 {
			return nil, fmt.Errorf("%s: максимум 200 записей, получено %d", f.Label, len(m))
		}
		out := make(map[string]string, len(m))
		for k, val := range m {
			key := strings.TrimSpace(k)
			if key == "" {
				return nil, fmt.Errorf("%s: ключ не может быть пустым", f.Label)
			}
			str, isStr := val.(string)
			if !isStr {
				return nil, fmt.Errorf("%s: значение %q должно быть строкой", f.Label, key)
			}
			str = strings.TrimSpace(str)
			if str == "" {
				return nil, fmt.Errorf("%s: у %q нет значения", f.Label, key)
			}
			if len(f.Enum) > 0 {
				okEnum := false
				for _, e := range f.Enum {
					if e == str {
						okEnum = true
						break
					}
				}
				if !okEnum {
					return nil, fmt.Errorf("%s: для %q допустимы значения %s, получено %q",
						f.Label, key, strings.Join(f.Enum, " / "), str)
				}
			}
			out[key] = str
		}
		return out, nil
	default: // string
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s: требуется строка", f.Label)
		}
		return strings.TrimSpace(s), nil
	}
}

// Apply 把面板提交的 patch 合并进 raw（原始 config.json 的 map）。
//
// 只处理 Fields 里登记过的键：
//   - 字段存在 → 校验后写入（点分路径会逐层建子 map）
//   - 字段不存在 → 返回错误（拒绝写未知键，避免面板把配置写坏）
//
// 返回 applied（值真的变了的键）与 needRestart（变了但不支持热更的键）。
func Apply(raw map[string]any, patch map[string]any) (applied, needRestart []string, err error) {
	keys := make([]string, 0, len(patch))
	for k := range patch {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 稳定输出，便于测试与前端展示

	for _, key := range keys {
		f, ok := fieldByKey(key)
		if !ok {
			return nil, nil, fmt.Errorf("Поле %q недоступно для изменения через панель", key)
		}
		val, err := f.Normalize(patch[key])
		if err != nil {
			return nil, nil, err
		}
		old, had := getPath(raw, key)
		if had && equalAny(old, val) {
			continue // 没变就别动文件（避免无意义的重启标记）
		}
		setPath(raw, key, val)
		applied = append(applied, key)
		if !f.Hot {
			needRestart = append(needRestart, key)
		}
	}
	return applied, needRestart, nil
}

// getPath 读取点分路径。
func getPath(m map[string]any, key string) (any, bool) {
	parts := strings.Split(key, ".")
	cur := any(m)
	for _, p := range parts {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// setPath 写入点分路径（中间层不存在则建 map）。
func setPath(m map[string]any, key string, val any) {
	parts := strings.Split(key, ".")
	cur := m
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = val
			return
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
}

// equalAny 宽松比较（JSON 数字解出来都是 float64，int 与 float64 要能相等）。
func equalAny(a, b any) bool {
	fa, aok := toFloat(a)
	fb, bok := toFloat(b)
	if aok && bok {
		return fa == fb
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// FieldValues 从当前配置里取每个字段的值（供面板渲染表单）。
func FieldValues(c Config) map[string]any {
	out := make(map[string]any, len(Fields))
	b, err := json.Marshal(c)
	if err != nil {
		return out
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return out
	}
	for _, f := range Fields {
		if v, ok := getPath(raw, f.Key); ok && v != nil {
			out[f.Key] = v
			continue
		}
		// 缺值有两种原因，都不能让表单显示成空白：
		//   · 字段带 omitempty（如 blob_base_url），空值被 json 省掉 ⇒ 用零值；
		//   · 指针类开关（expose_reasoning 的 nil = 默认 true）⇒ 按"实际生效值"填。
		switch f.Key {
		case "expose_reasoning":
			out[f.Key] = c.ReasoningEnabled()
		default:
			switch f.Type {
			case "int":
				out[f.Key] = 0
			case "bool":
				out[f.Key] = false
			case "kv":
				// 空表给空对象而不是空串：前端按「键值对」渲染，类型不一致会画错。
				out[f.Key] = map[string]any{}
			default:
				out[f.Key] = ""
			}
		}
	}
	return out
}
