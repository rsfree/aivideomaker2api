// Package jsonx 是 JSON 动态结构的取值助手。
//
// 为什么需要它：本服务对外有三个协议壳（方舟 / OpenAI / 面板），入参全部是
// `map[string]any`；Go 的 encoding/json 把数字解成 float64、把 `null` 解成 nil，
// 而翻译层要判的是"调用方**有没有**传这个字段"与"传的是什么类型"。
// 每个包各写一份 `v.(string)` 断言，必然在"字符串化的数字"这类边界上漂移。
//
// 约定：所有取值函数**不 panic、不猜**——认不出就返回零值与 false，由调用方决定
// 是拒绝还是告警。这条与上游适配层的口径一致：静默改写比失败更贵。
package jsonx

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// String 取字符串形态。数字与布尔会被字面化（`5` → `"5"`），其余返回 ""。
func String(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// Raw 取原样字符串（不做 TrimSpace），用于"提示词"这类要保留空白的字段。
func Raw(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return String(v)
	}
}

// Float 取数值。字符串里是合法数字也认（表单上传天生是字符串）。
func Float(v any) (float64, bool) {
	switch x := v.(type) {
	case nil:
		return 0, false
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// Int 取整数。小数部分不为零时按"不是整数"处理（返回 false）——
// 调用方据此给出"expected an integer"而不是静默截断。
func Int(v any) (int, bool) {
	f, ok := Float(v)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// Bool 取布尔。**不认** "true"/1 这类宽松写法的地方用这个（严格）；
// 需要宽松语义时用 LooseBool（它会明确返回"认不出"）。
func Bool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// LooseBool 宽松解析布尔：JSON `true`、数字 0/1、字符串 true/false/1/0/yes/no/on/off。
// 认不出返回 (false, false) —— 调用方必须告警，不许当 false 用。
func LooseBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case float64:
		if x == 0 {
			return false, true
		}
		if x == 1 {
			return true, true
		}
		return false, false
	case int:
		if x == 0 {
			return false, true
		}
		if x == 1 {
			return true, true
		}
		return false, false
	case json.Number:
		if x.String() == "0" {
			return false, true
		}
		if x.String() == "1" {
			return true, true
		}
		return false, false
	}
	switch strings.ToLower(String(v)) {
	case "true", "1", "yes", "on":
		return true, true
	case "false", "0", "no", "off":
		return false, true
	}
	return false, false
}

// Map 取对象。`map[string]interface{}` 之外的形态一律 nil。
func Map(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

// Slice 取数组。
func Slice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

// Has 判断键是否存在且非 nil（"传了 null" 与"没传"在 Go 里都是 nil，
// 对本服务足够：上游对两者一视同仁）。
func Has(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	return ok && v != nil
}

// Pick 顶层优先、其次 extra_body（方舟 SDK 把未建模字段塞在 extra_body 里）。
func Pick(body, extra map[string]any, key string) any {
	if v, ok := body[key]; ok && v != nil {
		return v
	}
	if extra != nil {
		if v, ok := extra[key]; ok && v != nil {
			return v
		}
	}
	return nil
}

// Marshal 紧凑 JSON（tRPC 的 input 会被百分号编码进 query，
// 空格会被编成 %20 或 +，两种都被站点接受，但紧凑写法让日志更可比）。
func Marshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
