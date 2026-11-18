// Package ratio 比例（ratio）↔ 像素（WxH）的通用映射层。
//
// 三层职责刻意分开：
//
//  1. **档位表不是全局常量**。现在只有一条 web 站点线（六档），将来接别的上游，
//     可选比例大概率不同 ⇒ 档位表是 Spec 实例：换上游 = 换 spec，不改判定逻辑。
//  2. **输入形态只会越来越多**。今天是 `16:9` / `1920x1080` / `5:4`，明天可能是
//     `portrait` 或平台预设名。用 **resolver 链**：每种形态一个函数，按优先级依次
//     尝试，第一个认领的胜出。判定顺序因此是**显式**的。
//  3. **比例与像素是两张表**。档位表决定"能出什么比例"，像素表决定"这档实际出多大"。
//     像素表**只填实测值**，没测过的一律返回 nil —— 编造一个尺寸比不给尺寸糟糕得多。
//
// 判定出口（Resolution.Mode）：
//
//	exact       精确命中档位表 / 非比例枚举 —— 不留痕
//	alias       别名映射（keep_ratio → adaptive）
//	preset      语义预设（portrait → 9:16）
//	dimension   WxH 像素约分后**正好**命中档位
//	proportion  W:H 比例串（可读，但不在档位表里）→ 就近吸附
//	snapped     有比例值但约不进档位 → 就近吸附（有损，必写偏差）
//	fallback    连比例都读不出来 → 兜底档
//	absent      没传（= 没指定，交给上游默认，不是兜底）
//
// 后六种**一律留痕**：它们都改变了调用方想要的结果。
package ratio

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

var (
	// wxhRe 认**尺寸**（`1920x1080` = 像素宽高）⇒ 先约分，再拿约分结果比档位。
	wxhRe = regexp.MustCompile(`^(\d{1,6})\s*[xX×*]\s*(\d{1,6})$`)
	// proportionRe 认**比例本身**（`5:4` ⇒ 1.25）⇒ 直接拿数值比远近，不需要约分。
	// 允许小数是必要的：`2.35:1`（宽银幕）是真实写法，强行约分只会得出怪分数而无益。
	proportionRe = regexp.MustCompile(`^\s*(\d{1,6}(?:\.\d+)?)\s*:\s*(\d{1,6}(?:\.\d+)?)\s*$`)
)

// Spec 是一份「上游能出哪些比例」的描述。
//
// Order 是**有序**的：就近吸附靠它做平手时的确定性 tie-break（平手取靠前者）。
// Extra 放**非比例**的枚举（如 `adaptive` = 跟随输入图），它们不参与吸附比较。
type Spec struct {
	Order    []string
	Extra    []string
	Fallback string
	Name     string
}

// SiteUI 是站点 UI 的 Aspect Ratio 选择器（截图取证：六档，16:9 默认选中）。
// 兜底取 16:9 正是为了与这个默认一致 —— 兜底不该制造"比上游默认更意外"的落差。
var SiteUI = Spec{
	Order:    []string{"16:9", "4:3", "1:1", "3:4", "9:16", "21:9"},
	Extra:    []string{"adaptive"},
	Fallback: "16:9",
	Name:     "site-ui",
}

// WithFallback 换掉兜底档位、其余不变。
func (s Spec) WithFallback(fallback string) Spec {
	s.Fallback = fallback
	return s
}

// AllValues 返回档位表 + 非比例枚举。
func (s Spec) AllValues() map[string]bool {
	out := make(map[string]bool, len(s.Order)+len(s.Extra))
	for _, r := range s.Order {
		out[r] = true
	}
	for _, r := range s.Extra {
		out[r] = true
	}
	return out
}

// RatioValue 把 `16:9` 变成 1.777…（仅供**远近比较**用，不参与对外输出）。
func RatioValue(ratio string) float64 {
	w, h, found := strings.Cut(ratio, ":")
	if !found {
		return 0
	}
	wf, err1 := strconv.ParseFloat(strings.TrimSpace(w), 64)
	hf, err2 := strconv.ParseFloat(strings.TrimSpace(h), 64)
	if err1 != nil || err2 != nil || hf == 0 {
		return 0
	}
	return wf / hf
}

// SimplifyWxH 把 `1920x1080` 约成 `16:9`；非 WxH 形态返回 ""。
func SimplifyWxH(text string) string {
	m := wxhRe.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return ""
	}
	w, _ := strconv.ParseInt(m[1], 10, 64)
	h, _ := strconv.ParseInt(m[2], 10, 64)
	if w <= 0 || h <= 0 {
		return ""
	}
	r := new(big.Rat).SetFrac64(w, h)
	return r.Num().String() + ":" + r.Denom().String()
}

// ParseProportion 把 `5:4` 变成 1.25；非「数值:数值」形态返回 (0,false)。
//
// 只认**冒号两侧都是正数**的形态：`0:9` / `16:0` 返回 false —— 它们不是比例，
// 是坏输入；放行会让除零/无穷把任意值都吸到同一档。
func ParseProportion(text string) (float64, bool) {
	m := proportionRe.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	w, err1 := strconv.ParseFloat(m[1], 64)
	h, err2 := strconv.ParseFloat(m[2], 64)
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, false
	}
	return w / h, true
}

// Snap 把任意宽高比吸附到本档位表里**相对距离**最近的一档。
//
// 用相对距离（对数差）而不是绝对差：比例域跨越 0.56~2.33，横屏那一端的绝对差
// 天然被放大，只有乘性比较才能让横屏与竖屏共用一把尺子。
func (s Spec) Snap(value float64) string {
	best := ""
	bestDist := math.Inf(1)
	for _, r := range s.Order {
		rv := RatioValue(r)
		if rv <= 0 || value <= 0 {
			continue
		}
		d := math.Abs(math.Log(rv / value))
		if d < bestDist-1e-12 {
			best, bestDist = r, d
		}
	}
	if best == "" {
		return s.Fallback
	}
	return best
}

// DriftPct 是 value 相对**落点**的偏差百分比。
//
// 以落点为分母 ⇒ 语义是"实际出的比你想要的窄/宽 X%"，比用较小值做分母
// （`exp(|log 差|)-1`，会系统性高估）更贴近肉眼感受。
func (s Spec) DriftPct(value float64, landed string) float64 {
	out := RatioValue(landed)
	if out == 0 {
		return 0
	}
	return math.Abs(value-out) / out * 100
}

// MeasuredPixels 是各档位**真实出片**的像素尺寸。判据是成片实际分辨率，
// 不是文档或推算。
//
// 只填实测过的值 —— 没测过的组合返回 nil，绝不靠"按比例推算"补表：
// 推算值看着合理，但站点常常取整/对齐到 16 的倍数，推算会稳定地错一档。
//
// 两个反例（本表存在的理由）：
//   - 4:3 @720p：从 480p 的 640x480 按比例算得 960x720，实测是 928x704；
//   - 16:9 @1080p：按 16:9 精确算是 1920x1080，实测是 1904x1080。
var MeasuredPixels = map[[2]string][2]int{
	{"480p", "16:9"}:  {864, 480},
	{"480p", "4:3"}:   {640, 480},
	{"480p", "3:4"}:   {480, 640},
	{"480p", "21:9"}:  {1120, 480},
	{"720p", "16:9"}:  {1248, 704},
	{"720p", "4:3"}:   {928, 704},
	{"720p", "1:1"}:   {704, 704},
	{"720p", "3:4"}:   {704, 928},
	{"720p", "9:16"}:  {704, 1248},
	{"1080p", "16:9"}: {1904, 1080},
	{"1080p", "1:1"}:  {1080, 1080},
	{"1080p", "9:16"}: {1080, 1904},
}

// PixelsFor 该分辨率下该比例**实测**的出片尺寸；未实测返回 (0,0)。
//
// 参数顺序是 (resolution, ratio)：调用点几乎总是"先定了分辨率，再问这个比例出多大"，
// 反过来写会让每个调用点都要回去确认一遍（本仓库自己的单测就踩过）。
func PixelsFor(resolution, ratio string) (int, int) {
	p, ok := MeasuredPixels[[2]string{strings.ToLower(strings.TrimSpace(resolution)), strings.TrimSpace(ratio)}]
	if !ok {
		return 0, 0
	}
	return p[0], p[1]
}

// RatioOf 像素尺寸 → 档位（约分后命中就用它，否则就近吸附）。
func RatioOf(w, h int, spec Spec) string {
	if w <= 0 || h <= 0 {
		return spec.Fallback
	}
	r := new(big.Rat).SetFrac64(int64(w), int64(h))
	exact := r.Num().String() + ":" + r.Denom().String()
	if spec.AllValues()[exact] {
		return exact
	}
	v, _ := new(big.Rat).SetFrac64(int64(w), int64(h)).Float64()
	return spec.Snap(v)
}

// Resolution 是一次解析的完整结果 —— 带 Mode 是为了让**留痕**由调用方统一决定。
type Resolution struct {
	Ratio string
	Mode  string
	Note  string
}

// Silent 表示这次解析不该产生噪声：健康请求被告警淹没就没人看了。
func (r Resolution) Silent() bool { return r.Mode == "exact" || r.Mode == "absent" }

// Options 是解析时的可选开关 —— 都是"表"，加一项不需要动判定逻辑。
type Options struct {
	// Aliases 别名 → 档位（`keep_ratio` → `adaptive`）。空表即关闭。
	Aliases map[string]string
	// Presets 语义预设 → 档位（`portrait` → `9:16`）。空表 ⇒ 不改变既有行为。
	Presets map[string]string
	// Strict = 认不出就返回错误（方舟线严格契约）；false = 落到 spec.Fallback。
	Strict bool
	// AllowProportion 是否接受 `W:H` 比例串。**默认收**：调用方写哪种形态不该由我们挑。
	AllowProportion bool
	// WarnDrift 超过该百分比时升级措辞（0 = 不升级）。越线**一律不拒绝**。
	WarnDrift float64
	// Examples 档位 → 一个可直接替换的 WxH 写法，供偏差过大时指路。
	Examples map[string]string
}

// ErrUnknownRatio 表示认不出的比例（Strict 模式下返回）。
type ErrUnknownRatio struct{ Raw string }

func (e *ErrUnknownRatio) Error() string {
	return fmt.Sprintf("invalid ratio value %q", e.Raw)
}

type resolver func(string, Spec, Options) (Resolution, bool)

// resolvers 的顺序是**显式**的。加一种输入形态 = 在这里插一个函数，其余不动。
var resolvers = []resolver{resEnum, resAlias, resPreset, resDimension, resProportion}

// Resolve 把任意写法的 size / ratio 解析成上游档位。
func Resolve(raw string, spec Spec, opt Options) (Resolution, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Resolution{Ratio: "", Mode: "absent"}, nil
	}
	for _, r := range resolvers {
		if got, ok := r(s, spec, opt); ok {
			return got, nil
		}
	}
	if !opt.Strict {
		parts := append([]string{}, spec.Order...)
		parts = append(parts, spec.Extra...)
		return Resolution{
			Ratio: spec.Fallback,
			Mode:  "fallback",
			Note: fmt.Sprintf("%q is not a recognized aspect ratio (expected one of %s, a WxH size like "+
				"1920x1080, or a W:H proportion like 16:9); fell back to %q",
				s, strings.Join(parts, "/"), spec.Fallback),
		}, nil
	}
	return Resolution{}, &ErrUnknownRatio{Raw: s}
}

func resEnum(s string, spec Spec, _ Options) (Resolution, bool) {
	if spec.AllValues()[s] {
		return Resolution{Ratio: s, Mode: "exact"}, true
	}
	return Resolution{}, false
}

func resAlias(s string, _ Spec, opt Options) (Resolution, bool) {
	mapped, ok := opt.Aliases[s]
	if !ok {
		return Resolution{}, false
	}
	if mapped == s {
		return Resolution{Ratio: mapped, Mode: "exact"}, true
	}
	return Resolution{
		Ratio: mapped, Mode: "alias",
		Note: fmt.Sprintf("%q maps to %q upstream (a single behavior there)", s, mapped),
	}, true
}

func resPreset(s string, _ Spec, opt Options) (Resolution, bool) {
	mapped, ok := opt.Presets[s]
	if !ok {
		return Resolution{}, false
	}
	return Resolution{
		Ratio: mapped, Mode: "preset",
		Note: fmt.Sprintf("%q is a preset for %q", s, mapped),
	}, true
}

func resDimension(s string, spec Spec, opt Options) (Resolution, bool) {
	wxh := SimplifyWxH(s)
	if wxh == "" {
		return Resolution{}, false
	}
	if spec.AllValues()[wxh] {
		return Resolution{
			Ratio: wxh, Mode: "dimension",
			Note: fmt.Sprintf("%q reduces to %q", s, wxh),
		}, true
	}
	v := RatioValue(wxh)
	landed := spec.Snap(v)
	return Resolution{
		Ratio: landed, Mode: "snapped",
		Note: snapNote(s, wxh, landed, spec.DriftPct(v, landed), opt),
	}, true
}

func resProportion(s string, spec Spec, opt Options) (Resolution, bool) {
	if !opt.AllowProportion {
		return Resolution{}, false
	}
	v, ok := ParseProportion(s)
	if !ok {
		return Resolution{}, false
	}
	landed := spec.Snap(v)
	return Resolution{
		Ratio: landed, Mode: "proportion",
		Note: snapNote(s, "", landed, spec.DriftPct(v, landed), opt),
	}, true
}

// snapNote 组装吸附说明。偏差**必须写出来**：差 1.6% 看不出来，差 25% 是另一个画面
// —— 只说"改了"不说"改了多少"，调用方无法判断该接受还是该换尺寸。
//
// asText 只在"原始写法与约分结果不同"时才给（`1024x1792` (4:7)）；比例串自己
// 就是比例文本（`5:4`），再括一遍会是 `size="5:4" (5:4)` 这种赘述。
func snapNote(raw, asText, landed string, drift float64, opt Options) string {
	shown := ""
	if asText != "" {
		shown = " (" + asText + ")"
	}
	var tail string
	if opt.WarnDrift > 0 && drift > opt.WarnDrift {
		example := opt.Examples[landed]
		tail = fmt.Sprintf("supported ratio %q (differs by %.1f%%, well beyond %g%%) — the output will "+
			"look noticeably different; consider %q instead", landed, drift, opt.WarnDrift, example)
	} else {
		tail = fmt.Sprintf("supported ratio %q (differs by %.1f%%) — the output aspect ratio will not be "+
			"exactly what you asked for", landed, drift)
	}
	return fmt.Sprintf("%q%s is not one of the upstream ratios; snapped to the nearest %s", raw, shown, tail)
}
