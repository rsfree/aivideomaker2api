// Package official 的模型注册表。
//
// 官方 REST 的 7 个模型**入参各不相同**（实测 2026-11-09）：
//
//	模型         duration 类型                 分辨率写法            必填
//	minimax      number(5..20)                 "720p"/"1080p"        content
//	t2v          "5"|"8"                       —                     prompt, aspectRatio
//	i2v          "5"|"8"                       —                     image
//	t2v_v3       "5"|"10"|"15"|"20"            —                     prompt, aspectRatio
//	i2v_v3       "5"|"10"|"15"|"20"            —                     image
//	seedance20   number                        number 480/720        duration, ratio, resolution
//	wan30        "5"|"10"|"15"                 "480P"/"720P"/"1080P" prompt, ratio, resolution
//	happyhorse   number                        "720P"/"1080P"        prompt, resolution
//
// 把这些差异**收在一张表里**，翻译层只按表办事 —— 免得"改一个模型要翻三处 if"。
package official

import (
	"fmt"
	"strings"
)

// FieldStyle 决定某个字段怎么落到 JSON 上。
type FieldStyle int

const (
	StyleNone   FieldStyle = iota
	StyleString            // 原样字符串
	StyleNumber            // 数字
)

// Model 是一个官方模型的入参规格。
type Model struct {
	Name string // 官方端点名（`/api/v1/generate/{Name}`）
	// Duration 用字符串枚举还是数字（两者的**取值合法性**由上游兜底校验，
	// 这里只负责类型正确 —— 猜枚举值容易在站点调整后静默错位）。
	DurationAsString bool
	// Resolutions 为空表示该模型没有 resolution 入参。
	Resolutions []string
	// ResolutionAsNumber：seedance20 的 resolution 要**数字**（480/720/1080），
	// 其余模型要字符串（"720p" / "720P"）。这是实测出来的，不是猜的。
	ResolutionAsNumber bool
	// NeedsImage 表示必须提供首帧图。
	NeedsImage bool
	// NeedsPrompt 表示必须有文本。
	NeedsPrompt bool
	// RatioField 是宽高比字段名（不同模型叫 aspectRatio / ratio）。
	RatioField string
	// PromptField / ImageField 是文本与图片字段名。
	PromptField string
	ImageField  string
}

var models = map[string]Model{
	"minimax": {
		Name: "minimax", PromptField: "content", RatioField: "aspectRatio",
		Resolutions: []string{"720p", "1080p"}, NeedsPrompt: true,
	},
	"t2v": {
		Name: "t2v", DurationAsString: true, PromptField: "prompt",
		RatioField: "aspectRatio", NeedsPrompt: true,
	},
	"i2v": {
		Name: "i2v", DurationAsString: true, ImageField: "image", NeedsImage: true,
	},
	"t2v_v3": {
		Name: "t2v_v3", DurationAsString: true, PromptField: "prompt",
		RatioField: "aspectRatio", NeedsPrompt: true,
	},
	"i2v_v3": {
		Name: "i2v_v3", DurationAsString: true, ImageField: "image", NeedsImage: true,
	},
	"seedance20": {
		Name: "seedance20", RatioField: "ratio",
		Resolutions: []string{"480", "720", "1080"}, ResolutionAsNumber: true,
	},
	"wan30": {
		Name: "wan30", DurationAsString: true, PromptField: "prompt", RatioField: "ratio",
		Resolutions: []string{"480P", "720P", "1080P"}, NeedsPrompt: true,
	},
	"happyhorse": {
		Name: "happyhorse", PromptField: "prompt",
		Resolutions: []string{"720P", "1080P"}, NeedsPrompt: true,
	},
}

// aliases 把调用方可能写的名字收敛到官方端点名。
var aliases = map[string]string{
	"seedance": "seedance20", "seedance2": "seedance20", "seedance2.0": "seedance20",
	"wan": "wan30", "wan3": "wan30", "wan2.3": "wan30",
	"happy_horse": "happyhorse", "happyhorse1": "happyhorse",
	"t2v-v3": "t2v_v3", "i2v-v3": "i2v_v3",
	"minimaxh3": "minimax", "minimax_h3": "minimax", "minimax-h3": "minimax",
}

// Lookup 按模型名查规格（大小写不敏感，支持别名）。
func Lookup(name string) (Model, bool) {
	key := strings.ToLower(strings.TrimSpace(name))
	// 去掉常见的渠道前缀：`official/seedance20`、`aivideomaker/wan30`
	if i := strings.LastIndex(key, "/"); i >= 0 {
		key = key[i+1:]
	}
	if canon, ok := aliases[key]; ok {
		key = canon
	}
	m, ok := models[key]
	return m, ok
}

// Names 返回全部官方模型名（给 /healthz 与面板用）。
func Names() []string {
	out := make([]string, 0, len(models))
	for k := range models {
		out = append(out, k)
	}
	return out
}

// NormalizeResolution 把调用方给的分辨率映射成该模型接受的写法。
//
// 三个模型三套写法（`720p` / `720P` / `720`），调用方不该被迫记住 —— 这里统一收口。
func (m Model) NormalizeResolution(raw string) (string, bool) {
	if len(m.Resolutions) == 0 {
		return "", true // 该模型没有分辨率入参
	}
	if strings.TrimSpace(raw) == "" {
		return m.Resolutions[0], true // 默认取第一个（通常是免费/最低档）
	}
	want := strings.TrimSpace(raw)
	// 剥掉 `p`/`P` 后缀再比较，这样 720p / 720P / 720 都能命中
	digits := strings.TrimRight(strings.ToLower(want), "p")
	for _, cand := range m.Resolutions {
		cd := strings.TrimRight(strings.ToLower(cand), "p")
		if cd == digits {
			return cand, true
		}
	}
	return "", false
}

// DurationValue 产出该模型接受的 duration 类型。
func (m Model) DurationValue(d int) any {
	if m.DurationAsString {
		return fmt.Sprint(d)
	}
	return d
}

// ResolutionValue 产出该模型接受的 resolution 类型。
func (m Model) ResolutionValue(res string) any {
	if m.ResolutionAsNumber {
		if n, err := strconvAtoi(res); err == nil {
			return n
		}
	}
	return res
}

func strconvAtoi(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not a number: %q", s)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
