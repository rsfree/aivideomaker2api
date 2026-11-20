// Package translate 是 Ark 协议 ↔ aivideomaker 网页端内部接口的**纯翻译层**。
//
// 全部是纯函数，零网络、零副作用。这是整个兼容层的核心逻辑，也是测试真正该压的地方。
//
// 翻译方向：
//
//	Ark create body  →  web params（站点 tRPC 接口的字段形状）
//	站点任务记录       →  内部视图（带 upstream_record 等证据字段）
//	内部视图           →  Ark task object（对外契约，见 ArkTaskView：白名单裁剪）
//
// ⚠️ 后两跳分开是刻意的：内部视图必须保留排障要用的证据（上游实际执行的模型、原始
// 站点记录、计费），而 `GET /tasks/{id}` 只能回官方 schema 里的字段（官方 SDK 对
// unknown field 是报错而非忽略）。把"证据"与"对外契约"混成一个 map，就会二选一地牺牲一头。
//
// Seedance 2.5「全能参考」的三条专属字段（omni_reference_task_type / output_format /
// generate_audio）在这里是**建模**的，不是丢进 unsupported 了事：能承接的承接、
// 上游明确不具备的进 incompatible（真实提交前 400、dry-run 仍可零成本看到）、
// 只是弱化的进 warnings。
//
// 参考素材按上游标称上限**截断**（图 4 / 视频 1 / 音频 2），但**每一次截断都留痕**：
// 告警里写明丢了几项、丢的是哪些 URL，并点名因此悬空的提示词占位符（@视频n）。
// 截断而不留痕，是本项目最贵的一类缺陷 —— 调用方会以为整份素材都生效了。
package translate

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rsfree/aivideomaker2api/internal/apierr"
	"github.com/rsfree/aivideomaker2api/internal/jsonx"
	"github.com/rsfree/aivideomaker2api/internal/official"
	"github.com/rsfree/aivideomaker2api/internal/ratio"
	"github.com/rsfree/aivideomaker2api/internal/slots"
)

func paramErr(msg, param string) error { return apierr.Param(msg, param) }

// WebResolutions 是**网页免费线**的分辨率档位（写死两档）。
//
// 只保留实测免费的两档。1080p 从网页线移除：它无论多短都计费（2026-11-08 实测
// 1080p/5s 扣 20 积分），留在网页面上只会让调用方误以为免费。显式点名 official
// 的通路在更早的分叉里已经返回，不受本表约束 —— 那条线按秒计费。
var WebResolutions = map[string]bool{"480p": true, "720p": true}

// WebResolutionList 是上表的展示顺序（错误信息用）。
var WebResolutionList = []string{"480p", "720p"}

// DefaultResolution 是调用方未点名分辨率时代入的站点档位（**唯一真源**）：
// Create 靠它定档位，/v1/videos 的「钉死时长」表也按它查键 —— 两处各写一个字面量
// 会出现"同一个 720p 请求、一个被钉住另一个没钉"这种只差一个字面量的隐形分歧。
const DefaultResolution = "720p"

// RatioSnapWarnDrift 超过这个偏差就**升级告警措辞**（百分比，以落点为分母），但不拒绝。
//
// 🔴 吸附默认一律放行：偏差再大也只是"留有痕地吸附"，绝不退化成 400 —— 对调用方
// 来说，"出一档相近的片"远好过"整个请求失败"。代价是可能拿到画面构成明显不同的
// 成片，所以这里不是不管，而是把偏差写清楚、越过阈值时升级措辞并给出可替换写法。
const RatioSnapWarnDrift = 15.0

// RatioExamples 是各合法比例的一个**可直接替换**的 WxH 写法 —— 报错时给调用方指一条明路。
var RatioExamples = map[string]string{
	"16:9": "1920x1080", "4:3": "1024x768", "1:1": "1024x1024",
	"3:4": "768x1024", "9:16": "1080x1920", "21:9": "2520x1080",
}

// SizePresets 是语义词 → 档位。**给不给这张表由调用方决定**：OpenAI 面的 size 收这些词
// 很自然；方舟线是官方契约，不往里加非标准值。
var SizePresets = map[string]string{
	"portrait": "9:16", "landscape": "16:9", "square": "1:1", "ultrawide": "21:9",
}

// DurationAllowed 是各分辨率**唯一允许**的时长（写死的离散表）。
//
//	480p → 5s / 10s
//	720p → 5s / 8s
//
// 口径来源：2026-11-19 站点侧确认 —— 480p 只收 5/10，720p 只收 5/8。
// （更早的"720p 连续 5–20"来自当时的观察，已被这条更窄的枚举取代。）
// 其余取值（6/7/9/11…）发出去只会被上游拒；与其换一个看不懂的 502，不如在入口
// 报 400 并列出合法值。这里**不设任何吸附例外** —— 改时长就是改账单。
var DurationAllowed = map[string][]int{
	"480p": {5, 10},
	"720p": {5, 8},
}

// DurationOptions 返回某分辨率允许的时长；未知档位返回 nil。
func DurationOptions(resolution string) []int {
	return DurationAllowed[strings.ToLower(strings.TrimSpace(resolution))]
}

// durationChoices 把允许值渲染成 `5s / 10s`（错误信息用）。
func durationChoices(resolution string) string {
	opts := DurationOptions(resolution)
	parts := make([]string, 0, len(opts))
	for _, v := range opts {
		parts = append(parts, fmt.Sprintf("%ds", v))
	}
	return strings.Join(parts, " / ")
}

// enumSummary 把**整张档位表**渲染成一句话：`480p: 5s / 10s; 720p: 5s / 8s`。
//
// 400 报文必须一次讲清"能传什么"：只报当前分辨率，调用方改分辨率后还要再撞一次；
// 直接给全矩阵，改哪个字段都心里有数。
func enumSummary() string {
	parts := make([]string, 0, len(WebResolutionList))
	for _, res := range WebResolutionList {
		parts = append(parts, res+": "+durationChoices(res))
	}
	return strings.Join(parts, "; ")
}

// ModelAlias 把「分辨率 + 时长」编码进模型名。
//
// 🔴 这是一张**闭集**：每个名字都是显式声明的一条，精确匹配、没有正则、没有猜测。
// （slots 层有过反面教训：一张"名字含 seedance 就走 seedance20"的正则表，把 5 个
// 代次静默压进同一槽位，账单差 7.3 倍。）查不到就是查不到 —— 由 slots 层报 unknown model。
//
// 好处不在于"少写两个字段"，而在于**组合在路由层就被钉死**：只支持免费档的网关
// （new-api / apiduo 这类按模型名路由与计价的下游）拿到模型名就等于拿到了合法参数。
type ModelAlias struct {
	Name       string // 对外模型名
	Slot       string // 上游槽位
	Resolution string // 钉死的分辨率
	Duration   int    // 钉死的时长
}

// Pins 表示这个别名把分辨率/时长**钉死**了。
//
// Resolution 与 Duration 同时为空 = **纯槽位别名**：只把名字映射到上游槽位，
// 参数仍由请求体决定（枚举照旧校验）。这类别名存在的原因是**上游网关的计费模型**：
// 按「模型名 + 请求体参数」计费的网关（apiduo 的 per_second × factors）必须让
// duration/resolution 出现在**请求体**里；把参数编码进模型名会让它读不到，退回
// 按 ratio 估算（实测预扣 $187.50，而正确值是 $0.05）。
func (a ModelAlias) Pins() bool { return a.Resolution != "" || a.Duration > 0 }

// ModelAliases 是全部别名。钉死参数的条目取值必须与 DurationAllowed 一致 ——
// 有一条单测（TestModelAliasesStayInsideTheEnum）守着，别手工漂移。
var ModelAliases = []ModelAlias{
	// 纯槽位别名：给"按请求体参数计费"的网关用（minimax-h3 + duration/resolution）。
	// 注意它必须排在固定组合别名**之前**不影响匹配（精确匹配，不互相覆盖）。
	{Name: "minimax-h3", Slot: "minimaxH3"},
	{"minimax-h3-480p-5s", "minimaxH3", "480p", 5},
	{"minimax-h3-480p-10s", "minimaxH3", "480p", 10},
	{"minimax-h3-720p-5s", "minimaxH3", "720p", 5},
	{"minimax-h3-720p-8s", "minimaxH3", "720p", 8},
}

// aliasHint 给"看起来像固定别名、但表里没有"的名字准备一条**可操作**的 400。
//
// 它只影响错误文案的分流，不参与路由：路由仍然只认 LookupModelAlias 的精确命中。
// 没有这条，调用方会拿到 slots 层的"必须是一个上游槽位" —— 而别名本来就不是槽位。
func aliasHint(name string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if !strings.HasPrefix(n, "minimax-h3-") && !strings.HasPrefix(n, "minimaxh3-") {
		return "", false
	}
	names := make([]string, 0, len(ModelAliases))
	for _, a := range ModelAliases {
		names = append(names, a.Name)
	}
	return fmt.Sprintf(
		"unknown model %q: supported fixed aliases are %s (the enum is — %s). "+
			"If you need another combination, send model %q and pass resolution/duration explicitly — "+
			"values outside the enum are rejected",
		name, strings.Join(names, ", "), enumSummary(), "minimaxH3"), true
}

// LookupModelAlias 精确匹配别名（大小写不敏感，回显仍用调用方原名）。
func LookupModelAlias(name string) (ModelAlias, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, a := range ModelAliases {
		if a.Name == n {
			return a, true
		}
	}
	return ModelAlias{}, false
}

// FreeMaxDuration 是免费窗口的上界：turbo 且不超过它就不计费。
const FreeMaxDuration = 10

// OmniTaskTypes 是 Seedance 2.5「全能参考」的任务类型。
//
//	auto / reference  普通全模态生成与参考驱动 —— 站点侧有对应能力
//	edit / extend     视频编辑与延长 —— 需要上游具备编辑/延长能力，而 web 线只有
//	                  minimax-H3 的文生/图生/参考
var OmniTaskTypes = []string{"auto", "reference", "edit", "extend"}

var omniNeedingAbsentCaps = map[string]bool{"edit": true, "extend": true}

// OutputFormats 是输出容器。站点实测成片 URL 一律 .mp4；mov 是 2.5 才引入的高色深容器。
var OutputFormats = []string{"mp4", "mov"}

// UpstreamOutputFormat 是上游实际产出的容器。
const UpstreamOutputFormat = "mp4"

// refLimits 是上游站点为参考素材标称的上限（中英双份一致）：
//
//	参考图片（最多 4 张）/ 参考视频（最多 1 个）/ 参考音频（最多 2 条）
var refLimits = map[string]struct {
	Label string
	Limit int
}{
	"reference_image": {"参考图片/reference_image", 4},
	"reference_video": {"参考视频/reference_video", 1},
	"reference_audio": {"参考音频/reference_audio", 2},
}

// PassthroughUnsupported 是 Ark 参数里站点侧无法承接的：列进 unsupported 而不是静默丢弃。
// ⚠️ generate_audio / omni_reference_task_type / output_format 已**移出**本表 ——
// 它们现在是被建模的字段。
var PassthroughUnsupported = []string{
	"watermark", "seed", "camera_fixed", "return_last_frame",
	"draft", "service_tier", "priority", "callback_url", "safety_identifier",
	"tools", "execution_expires_after",
}

// refTokenRe 匹配提示词里的素材占位引用：`@图像1` / `@视频2` / `@音频1`。
// 编号是 **1-based**，对应 content[] 里同类素材的出现顺序。
var refTokenRe = regexp.MustCompile(`@\s*(图像|视频|音频)\s*(\d+)`)

// NormalizeRatio 把 ratio / size 入参翻译成上游枚举值 + 映射说明（**凡改写必留痕**）。
//
// fallback 为空 = 方舟线严格契约（认不出 ⇒ 400）；非空 = OpenAI 面（认不出 ⇒ 落兜底档，
// 但**照样写 warning**，不是静默改写）。
//
// `W:H` 比例串（5:4 / 9:21 / 2.35:1）**两线都认** —— 调用方写哪种形态不该由我们挑。
func NormalizeRatio(raw, label, fallback string, presets map[string]string) (string, []string, error) {
	spec := ratio.SiteUI
	strict := true
	if fallback != "" {
		spec = spec.WithFallback(fallback)
		strict = false
	}
	opt := ratio.Options{
		Strict:          strict,
		AllowProportion: true,
		Presets:         presets,
		WarnDrift:       RatioSnapWarnDrift,
		Examples:        RatioExamples,
	}
	got, err := ratio.Resolve(raw, spec, opt)
	if err != nil {
		return "", nil, paramErr(fmt.Sprintf("%s: invalid enum value %q", label, strings.TrimSpace(raw)), label)
	}
	if got.Silent() || got.Note == "" {
		return got.Ratio, nil, nil
	}
	return got.Ratio, []string{label + "=" + got.Note}, nil
}

// ResolveDuration 只做一件事：**枚举校验**。
//
//	480p → 5s / 10s
//	720p → 5s / 8s
//
// 命中返回原值，否则返回参数错误（报文列出枚举）。这里**没有**任何吸附路径：
// 不就近取整、不因 preferFree 拉回、不因越界钳制 —— 改一个字都不是调用方要的时长。
// 想省钱的调用方应当自己从枚举里选，而不是让我们替他改。
func ResolveDuration(duration int, resolution string) (int, error) {
	opts := DurationOptions(resolution)
	if len(opts) == 0 {
		return 0, paramErr(fmt.Sprintf(
			"resolution: %q has no supported duration table (the web line supports: %s)",
			resolution, strings.Join(WebResolutionList, ", ")), "resolution")
	}
	for _, v := range opts {
		if v == duration {
			return v, nil
		}
	}
	return 0, paramErr(fmt.Sprintf(
		"duration: %ds is not supported for %s (supported — %s)",
		duration, resolution, enumSummary()), "duration")
}

// RefIndicesInPrompt 抽出提示词里的 `@图像n` / `@视频n` / `@音频n` 引用编号（1-based，去重保序）。
//
// 上游按 content[] 里同类素材的**出现顺序**解析这些占位符。素材被截断后，超出范围的
// 编号会指向不存在的素材；调用方写错编号也有同样后果 —— 两种都必须被点名。
func RefIndicesInPrompt(prompt string) map[string][]int {
	out := map[string][]int{"图像": {}, "视频": {}, "音频": {}}
	for _, m := range refTokenRe.FindAllStringSubmatch(prompt, -1) {
		kind := m[1]
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		dup := false
		for _, v := range out[kind] {
			if v == n {
				dup = true
				break
			}
		}
		if !dup {
			out[kind] = append(out[kind], n)
		}
	}
	return out
}

func promptRefWarnings(prompt string, groups map[string][]string) []string {
	labels := map[string]string{"图像": "reference_image", "视频": "reference_video", "音频": "reference_audio"}
	found := RefIndicesInPrompt(prompt)
	var warnings []string
	for _, kind := range []string{"图像", "视频", "音频"} {
		label := labels[kind]
		available := len(groups[label])
		var dangling []int
		for _, n := range found[kind] {
			if n < 1 || n > available {
				dangling = append(dangling, n)
			}
		}
		if len(dangling) == 0 {
			continue
		}
		sort.Ints(dangling)
		shown := make([]string, 0, len(dangling))
		for _, n := range dangling {
			shown = append(shown, fmt.Sprintf("@%s%d", kind, n))
		}
		warnings = append(warnings, fmt.Sprintf(
			"prompt references %s but only %d %s asset(s) are forwarded — those placeholders cannot resolve",
			strings.Join(shown, ", "), available, label))
	}
	return warnings
}

type contentItems struct {
	texts   []string
	first   []string
	last    []string
	refImg  []string
	refVid  []string
	refAud  []string
	unknown []string
}

func parseContentItems(items []any) contentItems {
	var out contentItems
	for _, raw := range items {
		it := jsonx.Map(raw)
		if it == nil {
			out.unknown = append(out.unknown, "content[] (non-object)")
			continue
		}
		typ := jsonx.String(it["type"])
		if typ == "text" {
			if text := jsonx.Raw(it["text"]); strings.TrimSpace(text) != "" {
				out.texts = append(out.texts, text)
			}
			continue
		}
		var url string
		for _, key := range []string{"image_url", "video_url", "audio_url"} {
			if holder := jsonx.Map(it[key]); holder != nil {
				if u := jsonx.String(holder["url"]); u != "" {
					url = u
					break
				}
			}
		}
		role := jsonx.String(it["role"])
		switch typ {
		case "image_url":
			switch role {
			case "first_frame":
				out.first = append(out.first, url)
			case "last_frame":
				out.last = append(out.last, url)
			default:
				out.refImg = append(out.refImg, url)
			}
		case "video_url":
			out.refVid = append(out.refVid, url)
		case "audio_url":
			out.refAud = append(out.refAud, url)
		default:
			out.unknown = append(out.unknown, fmt.Sprintf("content[].type=%q", typ))
		}
	}
	return out
}

// Plan 是一次创建请求的完整翻译结果。
type Plan struct {
	Requested    map[string]any `json:"requested"`
	Effective    map[string]any `json:"effective"`
	Warnings     []string       `json:"warnings"`
	Unsupported  []string       `json:"unsupported"`
	Incompatible []string       `json:"incompatible"`
	WebParams    map[string]any `json:"web_params"`
	CaptchaToken string         `json:"captcha_token"`
}

// Procedure 是本次要调的站点 tRPC procedure（站点把模型编在路径上）。
func (p *Plan) Procedure() string {
	return jsonx.String(p.WebParams["procedure"])
}

// Create 把 Ark 创建任务请求体翻译成站点参数（纯函数，不联网）。
//
// forceSlot 是**面级策略**入口 —— "这个面只跑某一个上游槽位"，由面的代码传入
// （当前只有 /v1/videos 用它"只跑免费线"）。它**不改写 requested.model**。
func Create(body map[string]any, channelOptions map[string]any, forceSlot string) (*Plan, error) {
	if body == nil {
		return nil, paramErr("body must be a JSON object", "")
	}
	// 三个切片都**从空切片起步**：nil 会序列化成 null，而调用方（以及我们自己的前端）
	// 期望的是"一个空的告警列表"，不是"没有这个字段"。
	warnings := []string{}
	unsupported := []string{}
	incompatible := []string{}

	// `model` 决定**上游槽位**（= 站点 procedure 名里那一段）。站点创建体里**没有**
	// model 字段 ⇒ "落到哪个模型"在这里表现为**换 procedure**。改模型 = 换计费档位，
	// 所以这一步绝不静默：每次命中都留告警与证据字段。
	model := jsonx.String(body["model"])
	if model == "" {
		return nil, paramErr("model is required", "model")
	}
	// 别名只改**解析用**的槽位名；`model` 保持调用方原名，回显必须是原名
	// （否则下游按回显名做路由，下一轮就找不到模型了）。
	modelRaw := model
	alias, hasAlias := LookupModelAlias(model)
	if !hasAlias {
		if hint, ok := aliasHint(model); ok {
			return nil, paramErr(hint, "model")
		}
	}
	baseModel := model
	if hasAlias {
		baseModel = alias.Slot
		if alias.Pins() {
			warnings = append(warnings, fmt.Sprintf(
				`model %q is a fixed alias: %s @ %ds (resolution and duration are pinned; `+
					"conflicting body fields are rejected)", modelRaw, alias.Resolution, alias.Duration))
		} else {
			warnings = append(warnings, fmt.Sprintf(
				`model %q is an alias for upstream slot %q — resolution and duration come from the `+
					"body (the enum is — %s)", modelRaw, alias.Slot, enumSummary()))
		}
	}
	modelRes, err := slots.ResolveModel(baseModel, channelOptions, forceSlot)
	if err != nil {
		// 网页槽位不认这个名字 —— 如果官方支持，就落官方（t2v / wan30 / happyhorse …）。
		// 这类模型网页端**根本没有**，报"unknown model"对调用方毫无帮助。
		if m, ok := official.Lookup(model); ok {
			items := jsonx.Slice(body["content"])
			if len(items) == 0 {
				return nil, paramErr("content is required", "content")
			}
			return createOfficialPlan(m, body, parseContentItems(items),
				jsonx.Map(body["extra_body"]), warnings, unsupported, incompatible)
		}
		return nil, err
	}
	warnings = append(warnings, modelRes.Warnings...)

	items := jsonx.Slice(body["content"])
	if len(items) == 0 {
		return nil, paramErr("content is required", "content")
	}
	ci := parseContentItems(items)
	unsupported = append(unsupported, ci.unknown...)

	// 帧输入与参考素材互斥（站点硬约束）
	hasFrame := len(ci.first) > 0 || len(ci.last) > 0
	hasRef := len(ci.refImg) > 0 || len(ci.refVid) > 0 || len(ci.refAud) > 0
	if hasFrame && hasRef {
		return nil, paramErr("frame inputs (imageUrl/lastFrameUrl) and reference assets are mutually exclusive", "")
	}

	// Ark SDK 会把"未建模字段"塞进 extra_body，真正发出时合并到顶层 —— 两处都要看，
	// 否则调用方以为生效了（例如 extra_body.watermark），实际被丢。
	extra := jsonx.Map(body["extra_body"])

	// 🔴 官方模型（seedance20 / wan30 / happyhorse / t2v / i2v …）走**独立翻译**：
	// 它们的入参规则和网页 minimax 完全不同（duration 有时是字符串枚举、分辨率三种写法、
	// 必填字段也不一样）。放在这里分叉，后面的网页校验一行都不用改。
	if m, ok := official.Lookup(model); ok && wantsOfficial(extra) {
		return createOfficialPlan(m, body, ci, extra, warnings, unsupported, incompatible)
	}
	for _, k := range PassthroughUnsupported {
		if _, inBody := body[k]; inBody && body[k] != nil {
			unsupported = append(unsupported, k)
			continue
		}
		if extra != nil {
			if v, ok := extra[k]; ok && v != nil {
				unsupported = append(unsupported, k)
			}
		}
	}

	// `WxH` 形态（如 1024x1792）在这里被归一化 / 就近吸附；认不出的仍然 400。
	// ratioRaw 留着：`requested` 区块回显的是**调用方写的值**，不是归一化后的值。
	ratioRaw := body["ratio"]
	ratioStr := jsonx.String(ratioRaw)
	ratioValue, notes, err := NormalizeRatio(ratioStr, "ratio", "", nil)
	if err != nil {
		return nil, err
	}
	warnings = append(warnings, notes...)
	aspect := ""
	if ratioValue != "" && ratioValue != "adaptive" {
		aspect = ratioValue
	}

	resolution := DefaultResolution
	if hasAlias && alias.Pins() {
		resolution = alias.Resolution
	}
	if raw, ok := body["resolution"]; ok && raw != nil && jsonx.String(raw) != "" {
		r := jsonx.String(raw)
		if hasAlias && alias.Pins() && r != alias.Resolution {
			return nil, paramErr(fmt.Sprintf(
				"resolution: model %q pins %s — omit the field, or use %q (request said %q)",
				modelRaw, alias.Resolution, modelRaw, r), "resolution")
		}
		if !WebResolutions[r] {
			return nil, paramErr(fmt.Sprintf(
				"resolution: invalid enum value %q (supported — %s)", r, enumSummary()), "resolution")
		}
		resolution = r
	}

	var duration *int
	rawDuration := body["duration"]
	if rawDuration != nil && jsonx.String(rawDuration) != "" {
		d, ok := jsonx.Float(rawDuration)
		if !ok {
			// 原实现这里会把非法类型漏成 500（已知缺陷，已登记备查）。
			// 移植版改判 400：把"客户端传错参数"报成"服务端故障"是纯损失。
			return nil, paramErr(fmt.Sprintf(
				"duration: expected a number of seconds, got %v (supported — %s)",
				rawDuration, enumSummary()), "duration")
		}
		if d == -1 {
			five := 5
			duration = &five
			warnings = append(warnings, "duration=-1 (intelligent) mapped to 5s")
		} else {
			v := int(math.Round(d))
			duration = &v
		}
	} else if framesRaw, ok := body["frames"]; ok && framesRaw != nil && jsonx.String(framesRaw) != "" {
		f, ok := jsonx.Float(framesRaw)
		if !ok {
			return nil, paramErr(fmt.Sprintf("frames: expected a number, got %v", framesRaw), "frames")
		}
		v := int(math.Max(1, math.Round(f/24)))
		duration = &v
		warnings = append(warnings, fmt.Sprintf("frames=%v converted to %ds at 24fps", framesRaw, v))
	}

	preferFree := false
	if extra != nil {
		if b, ok := extra["aivideomaker_prefer_free"].(bool); ok {
			preferFree = b
		}
	}
	if hasAlias && alias.Pins() {
		if duration != nil && *duration != alias.Duration {
			return nil, paramErr(fmt.Sprintf(
				"duration: model %q pins %ds — omit the field, or use %q (request said %ds)",
				modelRaw, alias.Duration, modelRaw, *duration), "duration")
		}
		pinned := alias.Duration
		duration = &pinned
	}
	base := 5
	if duration != nil {
		base = *duration
	}
	resolved, err := ResolveDuration(base, resolution)
	if err != nil {
		return nil, err
	}
	duration = &resolved
	if preferFree {
		// 开关还在解析（不发 400），但它已经不能改变任何时长：枚举就是全部。
		// 静默忽略会让调用方以为自己省到了钱，所以必须留痕。
		warnings = append(warnings, "aivideomaker_prefer_free has no effect: the web free tier is a fixed "+
			"enum (480p: 5s/10s, 720p: 5s/8s) and any other duration is rejected")
	}

	if aspect != "" && len(ci.first) > 0 && ratioValue != "adaptive" {
		warnings = append(warnings, `Ark requires ratio="adaptive" when role=first_frame; ratio is derived `+
			"from the image anyway")
	}

	// ---------------------------------------------------- Seedance 2.5 专属字段 --

	var omni string
	if raw := jsonx.Pick(body, extra, "omni_reference_task_type"); raw != nil && jsonx.String(raw) != "" {
		omni = jsonx.String(raw)
		if !contains(OmniTaskTypes, omni) {
			return nil, paramErr(fmt.Sprintf("omni_reference_task_type: invalid enum value %q (expected one of: %s)",
				omni, strings.Join(OmniTaskTypes, ", ")), "omni_reference_task_type")
		}
		if omniNeedingAbsentCaps[omni] {
			incompatible = append(incompatible, fmt.Sprintf(
				"omni_reference_task_type=%q requires upstream video edit/extend, which the web line does not "+
					"provide (it is minimax-H3 text-to-video / image-to-video / reference only) — use "+
					`"reference" or "auto"`, omni))
		}
		if omni == "edit" {
			if ratioValue != "" && ratioValue != "adaptive" {
				warnings = append(warnings, `omni_reference_task_type="edit" requires ratio="adaptive" upstream`)
			}
			if rawDuration != nil && jsonx.String(rawDuration) != "" && jsonx.String(rawDuration) != "-1" {
				warnings = append(warnings, `omni_reference_task_type="edit" requires duration=-1 upstream`)
			}
		}
	}

	var outputFormat string
	if raw := jsonx.Pick(body, extra, "output_format"); raw != nil && jsonx.String(raw) != "" {
		outputFormat = strings.ToLower(jsonx.String(raw))
		if !contains(OutputFormats, outputFormat) {
			return nil, paramErr(fmt.Sprintf("output_format: invalid enum value %q (expected one of: %s)",
				outputFormat, strings.Join(OutputFormats, ", ")), "output_format")
		}
		if outputFormat != UpstreamOutputFormat {
			warnings = append(warnings, fmt.Sprintf(
				"output_format=%q requested, but the upstream renders %s only — the delivered file will be "+
					"%s, not %s", outputFormat, UpstreamOutputFormat, UpstreamOutputFormat, outputFormat))
		}
	}

	var generateAudio *bool
	if raw := jsonx.Pick(body, extra, "generate_audio"); raw != nil {
		b, ok := raw.(bool)
		if !ok {
			return nil, paramErr("generate_audio must be a boolean", "generate_audio")
		}
		generateAudio = &b
		warnings = append(warnings, "generate_audio is not a station-side switch — the upstream decides the "+
			"audio track; the requested value is recorded and echoed back, but audio presence is never guaranteed")
	}

	// ---------------------------------------------------------------- 素材组装 --

	// 参考素材按上游标称上限截断，**每一次截断都必须留痕**。
	refImagesIn := append(skipFirst(nonEmpty(ci.first)), nonEmpty(ci.refImg)...)
	refImages, droppedImages := truncate(refImagesIn, "reference_image")
	refVideos, droppedVideos := truncate(nonEmpty(ci.refVid), "reference_video")
	refAudios, droppedAudios := truncate(nonEmpty(ci.refAud), "reference_audio")

	for _, pair := range []struct {
		label   string
		kept    []string
		dropped []string
	}{
		{"reference_image", refImages, droppedImages},
		{"reference_video", refVideos, droppedVideos},
		{"reference_audio", refAudios, droppedAudios},
	} {
		if len(pair.dropped) == 0 {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s: %d supplied, upstream accepts at most %d — %d dropped: %s",
			pair.label, len(pair.kept)+len(pair.dropped), len(pair.kept),
			len(pair.dropped), strings.Join(pair.dropped, ", ")))
	}
	if len(refVideos) > 0 {
		warnings = append(warnings, "reference_video is forwarded as referenceVideoUrl (accepted upstream; "+
			"verified live on the 480p/5s free tier)")
	}
	if len(refAudios) > 0 {
		warnings = append(warnings, "reference_audio is forwarded as referenceAudioUrls (accepted upstream; "+
			"verified live on the 480p/5s free tier)")
	}

	params := map[string]any{
		"content":            strings.TrimSpace(strings.Join(ci.texts, "\n")),
		"imageUrl":           firstOrNil(ci.first),
		"lastFrameUrl":       firstOrNil(ci.last),
		"referenceImageUrls": refImages,
		"referenceVideoUrl":  firstOrNil(refVideos),
		"referenceAudioUrls": refAudios,
		"duration":           *duration,
		"resolution":         resolution,
		"tier":               "turbo",
		// 站点侧的"提示词增强"。**默认开**（与站点自己前端发的请求一致）。
		// 关掉：extra_body.aivideomaker_prompt_enrichment = false。
		"promptEnrichment": true,
	}
	if aspect != "" {
		params["aspectRatio"] = aspect
	}

	// 通路选择：默认走网页会话（480p/720p turbo ≤10s 免费）；
	// 只有调用方**显式**点名 official 才走官方 REST —— 官方 API **全程收费**，
	// 默认切过去等于替调用方花钱（实测 720p/5s 就扣 15 积分）。
	if ch := strings.ToLower(strings.TrimSpace(jsonx.String(extra["aivideomaker_channel"]))); ch != "" {
		switch ch {
		case "official", "web":
			params["channel"] = ch
		default:
			warnings = append(warnings, "extra_body.aivideomaker_channel="+ch+
				" ignored (expected \"official\" or \"web\")")
		}
	}

	// 提示词里的 @图像n / @视频n / @音频n 必须能落到**真实发出去的**素材上。
	warnings = append(warnings, promptRefWarnings(jsonx.String(params["content"]), map[string][]string{
		"reference_image": refImages,
		"reference_video": refVideos,
		"reference_audio": refAudios,
	})...)

	if extra != nil {
		if override := jsonx.String(extra["aivideomaker_tier"]); override == "turbo" || override == "base" {
			params["tier"] = override
		} else if override != "" {
			warnings = append(warnings, fmt.Sprintf(
				"extra_body.aivideomaker_tier=%q ignored (expected turbo|base)", override))
		}
		if enrichRaw, ok := extra["aivideomaker_prompt_enrichment"]; ok && enrichRaw != nil && jsonx.String(enrichRaw) != "" {
			if flag, ok := jsonx.LooseBool(enrichRaw); ok {
				params["promptEnrichment"] = flag
			} else {
				warnings = append(warnings, fmt.Sprintf(
					"extra_body.aivideomaker_prompt_enrichment=%q ignored (expected true|false); the default "+
						"(true) applies", jsonx.String(enrichRaw)))
			}
		}
	}

	requested := map[string]any{
		"model":                    model,
		"content":                  items,
		"ratio":                    nullable(ratioRaw),
		"resolution":               body["resolution"],
		"duration":                 body["duration"],
		"frames":                   body["frames"],
		"output_format":            outputFormat,
		"generate_audio":           generateAudio,
		"omni_reference_task_type": nullableString(omni),
		"watermark":                body["watermark"],
		"seed":                     body["seed"],
	}
	aspectEcho := any("auto(from image)")
	if aspect != "" {
		aspectEcho = aspect
	}
	effective := map[string]any{
		"aspectRatio":    aspectEcho,
		"duration":       *duration,
		"resolution":     resolution,
		"output_format":  UpstreamOutputFormat,
		"billed":         Billed(jsonx.String(params["tier"]), *duration, resolution),
		"model":          modelRes.Slot,
		"model_source":   modelRes.Source,
		"model_verified": modelRes.Verified,
	}
	// 别名是"调用方写了一个组合名"，不是"他直接点了槽位" —— 排障时这两者要能分开，
	// 否则看到 model=minimaxH3 会以为调用方写的就是这个。
	if hasAlias {
		effective["model_alias"] = modelRaw
	}

	webParams := map[string]any{}
	for k, v := range params {
		webParams[k] = v
	}
	webParams["procedure"] = slots.ProcedureForSlot(modelRes.Slot)

	// 🔴 网页端每个模型的**入参形态都不一样**（实测，见 docs/evidence/api-surface.md）：
	//   minimaxH3 : content/duration/resolution/tier/aspectRatio/imageUrl…
	//   seedance20: prompt/image/video/audio/resolution("480")/duration/visitorId
	//   seedance25: prompt/image/duration
	//   happyhorse: prompt/duration/resolution
	//   ltx23     : imageUrl/lastFrameUrl/middleFrame1-3Url/resolution/duration/token
	//
	// 本层只翻译了 minimaxH3 那一套。其它槽位若照搬，上游会回一大段 zod 报错
	// （调用方看不懂，还以为是参数写错了）。这里**提前拒绝并指路**。
	if !webShapeAdapted(modelRes.Slot) {
		return nil, paramErr(fmt.Sprintf(
			"model %q is not adapted on the web channel yet (its upstream body differs from minimaxH3). "+
				"Use the official channel instead: \"extra_body\":{\"aivideomaker_channel\":\"official\"} "+
				"or POST /official/api/v3/contents/generations/tasks", modelRes.Slot), "model")
	}

	unsupported = dedupeSorted(unsupported)
	captcha := ""
	if extra != nil {
		captcha = jsonx.String(extra["aivideomaker_captcha_token"])
	}

	return &Plan{
		Requested:    requested,
		Effective:    effective,
		Warnings:     warnings,
		Unsupported:  unsupported,
		Incompatible: incompatible,
		WebParams:    webParams,
		CaptchaToken: captcha,
	}, nil
}

// FreeWindowResolutions 是有免费窗口的分辨率集合。
//
// 🔴 **1080p 不在里面**：实测（2026-11-08，真实账号）`1080p / 5s / turbo` 扣了 **20 积分**
// （4 积分/秒 × 5s），而当时 `effective.billed` 报的是 `false`。这是最坏的一类错误方向 ——
// 调用方读到"免费"，账单上却有钱。1080p 无论多短都计费，判据里必须带上它。
var AlwaysBilledResolutions = map[string]bool{"1080p": true}

// Billed 判断这条参数组合会不会真花钱。
//
// 三个独立条件，缺一不可：质量档（base）没有免费窗口；时长超过免费上界；分辨率本身
// 没有免费窗口（1080p）。**只按时长判断是错的** —— 那正是实测抓到的 bug。
func Billed(tier string, duration int, resolution string) bool {
	if strings.EqualFold(tier, "base") {
		return true
	}
	if duration > FreeMaxDuration {
		return true
	}
	// ⚠️ 用**显式黑名单**而不是"不在白名单就计费"：resolution 缺失（走默认档）时
	// 不能凭空喊计费 —— 误报计费会让调用方白等、白改参数；漏报计费才是真花钱。
	// 但已知会计费的档位必须诚实。
	return AlwaysBilledResolutions[strings.ToLower(strings.TrimSpace(resolution))]
}

// turboRates 是**快速档（turbo）在计费区**的每秒单价（积分/秒）。
//
// 全部来自 2026-11-08 的真实扣费核对（每例前后各读一次余额）：
//
//	480p / turbo / 15s → 15 积分   (1 积分/秒)
//	720p / turbo / 11s → 33 积分   (3 积分/秒)
//	1080p/ turbo /  5s → 20 积分   (4 积分/秒)
var turboRates = map[string]int{"480p": 1, "720p": 3, "1080p": 4}

// basePremiumPerSecond 是质量档（base）相对 turbo 的每秒溢价。
//
// 三个实测点吻合同一条规则（**base = turbo 价 + 1 积分/秒**，免费窗口内 turbo 记 0）：
//
//	720p / base /  5s →  5 积分 = 5 × (0 + 1)
//	720p / base / 15s → 60 积分 = 15 × (3 + 1)
//	480p / base /  5s →  5 积分 = 5 × (0 + 1)
//
// ⚠️ 别把它写成"base 恒为 1 积分/秒"：15s 那次实测是 **60** 而不是 15，
// 直接按 1/秒 外推会把账单低估 4 倍。溢价模型才同时解释这三个点。
const basePremiumPerSecond = 1

// EstimatedCredits 给出这条请求**预计**要花多少积分；只有实测覆盖过的组合才返回 ok=true。
func EstimatedCredits(tier string, duration int, resolution string) (int, bool) {
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "" {
		tier = "turbo"
	}
	res := strings.ToLower(strings.TrimSpace(resolution))
	rate, known := turboRates[res]
	if !known || duration <= 0 {
		return 0, false
	}
	// 1080p + 质量档没有实测点，规则推导值（25/5s）不对外声称。
	if res == "1080p" && tier == "base" {
		return 0, false
	}
	if strings.EqualFold(tier, "base") {
		if !Billed("turbo", duration, res) {
			return duration * basePremiumPerSecond, true
		}
		return duration * (rate + basePremiumPerSecond), true
	}
	if !Billed(tier, duration, res) {
		return 0, true
	}
	return duration * rate, true
}

// BillingNote 一句话说清这条上游怎么计费（/healthz 与 effective 都引用它）。
func BillingNote() string {
	return "web upstream: minimaxH3 free tier accepts 480p @ 5s/10s and 720p @ 5s/8s " +
		"(tier=turbo); every other duration or resolution is rejected at the edge, tier=base is always billed"
}

// BillingView 渲染计费口径。
//
// tier=base 一律计费；tier=turbo 且 ≤10s **免费**。判据只有一个：effective.billed
// —— 站点任务记录里的 `paid` 才是最终事实（`credits` 与它反相，别用它判断）。
func BillingView(plan *Plan) (map[string]any, []string) {
	eff := make(map[string]any, len(plan.Effective)+4)
	for k, v := range plan.Effective {
		eff[k] = v
	}
	tier := jsonx.String(plan.WebParams["tier"])
	if tier == "" {
		tier = "turbo"
	}
	duration, _ := jsonx.Int(eff["duration"])
	resolution := jsonx.String(plan.WebParams["resolution"])
	// 官方通路**恒计费**：不能套用网页那条"480p/720p + turbo + ≤10s 免费"的规则。
	// 否则 dry-run 会告诉调用方 billed=false，而实际下单扣了 15 积分（实测过）。
	// 🔴 免费窗口是**模型相关**的，不只是时长/分辨率。
	//
	// 实测（2026-11-09）：`seedance20` 480p/5s 走网页档，余额 2406 → 2338，
	// 扣 **68 积分** —— 与官方 `quote/seedance20 @480` 完全一致。
	// 站点的免费档只有 MiniMax H3（下拉菜单写的就是「480P 免费」）。
	// 只按 tier/duration/resolution 判断会给出"免费"的错误结论，直接害人。
	proc := jsonx.String(plan.WebParams["procedure"])
	freeModel := proc == "" || proc == "ai.minimaxH3" // 空 = 官方分支（另有判断）
	switch {
	case jsonx.String(plan.WebParams["channel"]) == "official":
		eff["billed"] = true
	case !freeModel:
		eff["billed"] = true
		eff["billing_note"] = "only MiniMax H3 has a free window (480p/720p + tier=turbo + <=10s); " +
			"every other model is billed (seedance20 480p/5s measured at 68 credits)"
	default:
		eff["billed"] = Billed(tier, duration, resolution)
	}
	eff["tier"] = tier
	// 分辨率/比例以**站点实际收到的参数**为准（web_params 是真正发给上游的那份）。
	if r := jsonx.String(plan.WebParams["resolution"]); r != "" {
		eff["resolution"] = r
	}
	if a := jsonx.String(plan.WebParams["aspectRatio"]); a != "" {
		eff["aspectRatio"] = a
	}
	// 计费说明：**谁先写谁说了算**。上面按模型/通路已经写过更准的，这里不能覆盖 ——
	// 踩过一次：seedance20 明明计费，说明却写着"480p/720p … 免费 up to 10s"。
	if jsonx.String(eff["billing_note"]) == "" {
		if jsonx.String(plan.WebParams["channel"]) == "official" {
			eff["billing_note"] = "official REST API bills every generation " +
				"(minimax 720p/5s = 15 credits, 1080p/5s = 20, wan30 5s@720P = 75, seedance20 5s@720P = 147)"
		} else {
			eff["billing_note"] = BillingNote()
		}
	}
	// 钱数比"计费：是/否"更有用 —— 但只有实测过的组合才给估计，其余显式 null。
	if credits, ok := EstimatedCredits(tier, duration, resolution); ok {
		eff["estimated_credits"] = credits
	} else {
		eff["estimated_credits"] = nil
	}

	warnings := append([]string{}, plan.Warnings...)
	// ★ 越线**必须**有声音：duration=15 会静默进计费区 —— effective.billed=true 而
	// warnings 为空，调用方除非自己读 billed 字段，否则不知道这条请求要真花钱。
	// 只在**因时长**越线时喊：tier=base 是调用方显式点名的选择，不是"悄悄变贵"。
	if tier != "base" && duration > FreeMaxDuration {
		warnings = append(warnings, fmt.Sprintf(
			"duration %ds is outside the free window (%ds) — this request WILL BE BILLED (tier=%s); pass "+
				"extra_body.aivideomaker_prefer_free=true to snap it down to %ds instead",
			duration, FreeMaxDuration, tier, FreeMaxDuration))
	}
	// 分辨率越线同样必须喊：这是**唯一一个"时长在免费区内却仍然扣钱"**的组合，
	// 而它此前被静默放过一次（实测 1080p/5s 扣 20 积分、billed 报 false）。
	if tier != "base" && duration <= FreeMaxDuration && AlwaysBilledResolutions[strings.ToLower(strings.TrimSpace(resolution))] {
		warnings = append(warnings, fmt.Sprintf(
			"resolution %s has no free window — this request WILL BE BILLED (tier=%s) even though the "+
				"duration (%ds) is within the free window; use 480p or 720p for the free tier",
			resolution, tier, duration))
	}
	return eff, warnings
}

// 站点侧 taskStatus 的词表（succeed / processing / queueing…）。
var webStatusToArk = map[string]string{
	"submitted": "queued", "pending": "queued", "preparing": "queued",
	"queueing": "queued", "queued": "queued",
	"processing": "running", "running": "running", "in_progress": "running",
	"succeed": "succeeded", "success": "succeeded", "completed": "succeeded",
	"failed": "failed", "fail": "failed", "error": "failed",
	"cancelled": "cancelled", "canceled": "cancelled",
	// 官方第 6 态 expired（任务超时）。站点若把超时任务标成这几个词，必须落到它自己的
	// 档位而不是被默认成 queued —— "超时"与"还在排队"对调用方是完全相反的两个结论。
	"expired": "expired", "expire": "expired", "timeout": "expired",
	"timedout": "expired", "timed_out": "expired",
}

// ToArkStatus 把站点 taskStatus 归一化成 Ark 状态词（指标标签也必须走这里）。
func ToArkStatus(raw string) string {
	if v, ok := webStatusToArk[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return v
	}
	return "queued"
}

// NormalizeWebTask 把站点任务记录翻成**内部**任务视图（带排障证据）。
//
// 站点记录的字段名自成一套（taskStatus / aiModel / url / credits / kelingKeyId），
// 其中 **kelingKeyId 才是真实分辨率**（"480" / "1080"）—— 请求里写的分辨率未必等于
// 实际产出的档位。
func NormalizeWebTask(raw map[string]any) map[string]any {
	if raw == nil {
		raw = map[string]any{}
	}
	statusRaw := strings.ToLower(jsonx.String(raw["taskStatus"]))
	if statusRaw == "" {
		statusRaw = "submitted"
	}
	arkStatus := ToArkStatus(statusRaw)
	// 站点会把**没有产出 URL** 的任务也留在 succeed（taskStatusMsg="not found url"）。
	// 照搬映射会让调用方看到"succeeded 但 content.video_url 为 null"这种自相矛盾的状态，
	// 下游据此判成功、拿到空链接 ⇒ 直接按失败处理，原因沿用 taskStatusMsg。
	url := jsonx.String(raw["url"])
	if arkStatus == "succeeded" && url == "" {
		arkStatus = "failed"
	}
	succeeded := arkStatus == "succeeded"

	var errBlock any
	if arkStatus == "failed" {
		msg := jsonx.String(raw["taskStatusMsg"])
		if msg == "" {
			msg = "video generation failed"
		}
		errBlock = map[string]any{"code": "GenerationFailed", "message": msg}
	}

	var videoURL any
	if succeeded {
		videoURL = url
	}
	var credits any = raw["credits"]
	paid := false
	if b, ok := raw["paid"].(bool); ok {
		paid = b
	}

	resKey := jsonx.String(raw["kelingKeyId"])
	var resolution any
	if isDigits(resKey) {
		resolution = resKey + "p"
	}

	return map[string]any{
		"id":             raw["id"],
		"model":          raw["aiModel"],
		"upstream_model": raw["aiModel"],
		"status":         arkStatus,
		"error":          errBlock,
		"content":        map[string]any{"video_url": videoURL},
		// ⚠️ 站点侧 `credits` 与 `paid` **不是一回事**（paid=false 时记 credits=1）。
		// 判断这次是否花钱只看 paid。官方那两个 token 字段**不再伪造 0**：站点不提供
		// token 用量，编一个 0 等于声称"本次消耗 0 token"，比不给更糟。
		"usage":      map[string]any{"credits": credits, "paid": paid},
		"created_at": epoch(raw["createdAt"]),
		"updated_at": epoch(firstNonNil(raw["completedAt"], raw["createdAt"])),
		"duration":   raw["duration"],
		"ratio":      raw["aspectRatio"],
		"resolution": resolution,
		// ⚠️ 键名是 upstream_record，**不是** upstream：视图最后一跳用 `upstream` 表示
		// 上游**种类**（"web"）并会覆盖同名键 ⇒ 原始站点记录若放 upstream 会在那一跳被丢掉，
		// 而它正是"上游到底跑了什么"的唯一原始证据。
		"upstream_record": raw,
	}
}

// ArkTaskFields 是对外契约白名单：能出现在 `GET /api/v3/contents/generations/tasks/{id}`
// 响应体里的字段，必须**同时**满足两个条件：
//
//  1. 在官方「查询视频生成任务」的响应参数表里 —— 官方 SDK（Java/Go）对 unknown field
//     是**报错**而非忽略，多一个键就是坏一个客户端；
//  2. 我们**真的知道它的值**（值为空就不给键）。
//
// 适配层的证据（upstream_model / upstream_record / requested / effective / warnings /
// unsupported / usage / resolution）一律不进响应体 —— 证据换通道，而不是消失。
var ArkTaskFields = []string{"id", "status", "error", "content", "duration", "ratio", "created_at", "updated_at"}

// ArkTaskView 把内部视图收窄成对外响应体：白名单 + 只给有值的字段。
//
// 唯一例外是 `error`：恒给（官方规定任务成功时显式返回 null，那是**确定**的信息，
// 不是"不知道"）。`content` 只在真有出片地址时才给。
//
// ⚠️ `duration` 转成整数：站点任务记录里它是**字符串** "5"，原样透传会让强类型 SDK
// 反序列化直接抛 —— 这就是"跟原生 response 对不上"的元凶之一。
func ArkTaskView(view map[string]any) map[string]any {
	out := make(map[string]any, len(ArkTaskFields))
	for _, key := range ArkTaskFields {
		val, ok := view[key]
		if !ok {
			continue
		}
		switch key {
		case "content":
			content := jsonx.Map(val)
			url := ""
			if content != nil {
				url = jsonx.String(content["video_url"])
			}
			if url == "" {
				continue
			}
			block := map[string]any{"video_url": url}
			if lf := jsonx.String(content["last_frame_url"]); lf != "" {
				block["last_frame_url"] = lf
			}
			out["content"] = block
		case "error":
			if m := jsonx.Map(val); m != nil {
				out["error"] = map[string]any{"code": m["code"], "message": m["message"]}
			} else {
				out["error"] = nil
			}
		case "duration", "created_at", "updated_at":
			if n, ok := jsonx.Int(val); ok {
				out[key] = n
			}
		default:
			if val != nil && jsonx.String(val) != "" {
				out[key] = val
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- helpers --

func truncate(urls []string, kind string) (kept, dropped []string) {
	limit := refLimits[kind].Limit
	if len(urls) <= limit {
		// 返回**非 nil 的空切片**：nil 会一路序列化成 null，而站点 schema 要求数组。
		if urls == nil {
			urls = []string{}
		}
		return urls, nil
	}
	return urls[:limit], urls[limit:]
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// skipFirst 等价于 Python 的 `seq[1:]`：空切片返回空切片。
// ⚠️ Go 里对空切片做 `s[1:]` 会直接 panic —— 纯文生视频（no frame）是最常见的请求，
// 这条路径必须安全。
func skipFirst(in []string) []string {
	if len(in) <= 1 {
		return nil
	}
	return in[1:]
}

func firstOrNil(in []string) any {
	if len(in) == 0 {
		return nil
	}
	return in[0]
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func dedupeSorted(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func nullable(v any) any {
	if v == nil {
		return nil
	}
	s := jsonx.String(v)
	if s == "" {
		return nil
	}
	return strings.TrimSpace(jsonx.Raw(v))
}

func formatNum(v int) string { return strconv.Itoa(v) }

// nullableString 让"没指定"在 JSON 里是 null，而不是空串 —— 空串会被读成"指定了空值"。
func nullableString(v string) any {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil && jsonx.String(v) != "" {
			return v
		}
	}
	return nil
}

// epoch 把 ISO8601 转成 epoch 秒；认不出返回 nil（不猜）。
func epoch(v any) any {
	s := jsonx.String(v)
	if s == "" {
		return nil
	}
	if t, err := parseISO(s); err == nil {
		return int(t.Unix())
	}
	return nil
}

// wantsOfficial 判断这次请求是否该走官方 REST。
//
// 两条依据：
//  1. 模型**只有官方的**（seedance20 / wan30 / happyhorse / t2v / i2v / v3 系）⇒ 直接走；
//  2. minimax 两边都有 ⇒ 只有调用方**显式**点名 official 才走（官方全程收费，不替人决定）。
//
// wantsOfficial 只看调用方有没有**显式**点名。
//
// 为什么不能"官方独有模型就自动走"：`seedance20` / `HappyHorse` 这些名字在
// **网页槽位里也存在**（网页端能跑，走会话 + 闸门）。自动切官方等于替调用方
// 从"免费/低价档"切到"官方收费档"，价格差可达十倍。
func wantsOfficial(extra map[string]any) bool {
	return strings.EqualFold(strings.TrimSpace(jsonx.String(extra["aivideomaker_channel"])), "official")
}

// createOfficialPlan 把方舟请求翻译成官方 REST 的 body。
//
// 对外仍然是**方舟协议**（`/api/v3/contents/generations/tasks`），只是落到不同的上游。
func createOfficialPlan(m official.Model, body map[string]any, ci contentItems,
	extra map[string]any, warnings, unsupported, incompatible []string) (*Plan, error) {

	prompt := strings.TrimSpace(strings.Join(ci.texts, "\n"))
	if m.NeedsPrompt && prompt == "" {
		return nil, paramErr("content text is required for model "+m.Name, "content")
	}

	duration := 5
	if v, ok := jsonx.Int(firstNonNil(body["duration"], extra["duration"])); ok && v > 0 {
		duration = v
	}
	if m.DurationAsString && (m.Name == "t2v" || m.Name == "i2v") && duration != 5 && duration != 8 {
		return nil, paramErr(fmt.Sprintf("%s only supports duration 5 or 8 (got %d)", m.Name, duration), "duration")
	}

	rawRes := firstNonEmptyStr(jsonx.String(body["resolution"]), jsonx.String(extra["resolution"]))
	resolution, ok := m.NormalizeResolution(rawRes)
	if !ok {
		return nil, paramErr(fmt.Sprintf("resolution %q is not supported by %s (allowed: %s)",
			rawRes, m.Name, strings.Join(m.Resolutions, ", ")), "resolution")
	}

	ratio := firstNonEmptyStr(
		jsonx.String(body["ratio"]), jsonx.String(body["aspect_ratio"]),
		jsonx.String(extra["ratio"]), jsonx.String(extra["aspect_ratio"]),
	)

	of := map[string]any{"duration": m.DurationValue(duration)}
	if prompt != "" || m.NeedsPrompt {
		of[m.PromptField] = prompt
	}
	if len(m.Resolutions) > 0 {
		of["resolution"] = m.ResolutionValue(resolution)
	}
	if m.RatioField != "" && ratio != "" {
		of[m.RatioField] = ratio
	}

	if m.NeedsImage {
		img := ""
		if len(ci.first) > 0 {
			img = ci.first[0]
		} else if len(ci.refImg) > 0 {
			img = ci.refImg[0]
		}
		if img == "" {
			return nil, paramErr("model "+m.Name+" requires a first-frame image (content.image_url)", "content")
		}
		of[m.ImageField] = img
	}

	if len(ci.last) > 0 || len(ci.refVid) > 0 || len(ci.refAud) > 0 || len(ci.refImg) > 1 {
		unsupported = append(unsupported, "last_frame / reference_video / reference_audio 官方接口不支持，已忽略")
	}

	eff := map[string]any{
		"model": m.Name, "channel": "official",
		"duration": duration, "resolution": resolution, "aspectRatio": ratio,
		// 官方 API 的 quote 里 `billable` 恒为 false（schema 写死），**不代表免费**。
		// 所以这里恒填 true，并给一句人话，避免调用方误判。
		"billed": true,
		"billing_note": "official REST API charges credits for every generation " +
			"(minimax 720p/5s = 15 credits; see README price table)",
	}
	warnings = append(warnings, "routed to the OFFICIAL REST API ("+m.Name+"): "+
		"this channel bills credits for every generation, and has no Turnstile gate")

	return &Plan{
		Requested: body, Effective: eff, Warnings: warnings,
		Unsupported: unsupported, Incompatible: incompatible,
		WebParams: map[string]any{
			"channel": "official", "official_model": m.Name, "official_body": of,
		},
	}, nil
}

// firstNonEmptyStr 取第一个非空字符串（本文件内的小工具）。
// webShapeAdapted 表示本层的翻译是否覆盖了该网页槽位的入参形态。
//
// 目前只有 minimaxH3 —— 其余 4 个（seedance20/25、happyhorse、ltx23）字段名与语义都不同。
func webShapeAdapted(slot string) bool {
	switch slot {
	case "minimaxH3":
		return true
	case "seedance25":
		// 这条历史上就是按 minimax 那套参数在用（omni edit 的兼容性判断也依赖它），
		// 没有实测证据说它一定不通 ⇒ 保持原样，不额外拦。
		return true
	default:
		// seedance20 / happyhorse / ltx23：实测入参完全不同（详见上一条注释）。
		return false
	}
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// OfficialPlanFor 用**同一份请求体**再翻一次官方 plan。
//
// 用途：请求落到网页通路但**要计费**时，server 可以改走官方 REST —— 同样花钱，
// 但官方没有 Turnstile 闸门、有 idempotency、还有正式的错误码。
// 第二个返回值 false 表示"这个名字官方没有"。
func OfficialPlanFor(body map[string]any) (*Plan, bool, error) {
	if body == nil {
		return nil, false, paramErr("body must be a JSON object", "")
	}
	model := jsonx.String(body["model"])
	m, ok := official.Lookup(model)
	if !ok {
		return nil, false, nil
	}
	items := jsonx.Slice(body["content"])
	if len(items) == 0 {
		return nil, true, paramErr("content is required", "content")
	}
	plan, err := createOfficialPlan(m, body, parseContentItems(items),
		jsonx.Map(body["extra_body"]), []string{}, []string{}, []string{})
	if err != nil {
		return nil, true, err
	}
	return plan, true, nil
}
