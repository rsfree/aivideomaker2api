// Package openaivideos 是 OpenAI `/v1/videos` 兼容面的纯翻译层（零网络、零副作用）。
//
// 对齐的契约是 Chatfire「OpenaiVideos格式 / Seedance 豆包即梦」的两份 OpenAPI：
//
//	POST /v1/videos        表单或 JSON → {id, object, status, created_at}
//	GET  /v1/videos/{id}   → {id, object, status, progress, video_url, created_at}
//
// 「务必一样」：对外**只**返回契约里声明的字段，一个不多 —— 调用方按 Chatfire 文档
// 写的解析代码必须原样可用。内部仍走 translate.Create 的完整翻译（截断留痕 / 计费口径 /
// 前置拒绝），两条入口共享同一条纪律。
//
// 字段对应关系（OpenAI 形 → Ark 形）：
//
//	model     → 原样保留为 requested.model，但**不决定上游槽位**（见下）
//	            `_480p/_720p/_1080p` 后缀**仍然**决定分辨率档位
//	prompt    → content[] 里的 text 项
//	seconds   → duration（整数秒）—— ⚠️ **本端点按分辨率写死**（PinnedDurations）：
//	            480p 一律 10s、720p 一律 8s；调用方显式传的值会被**覆盖**，且在 notes 留痕
//	size      → ratio（比例枚举原样；也接受 WxH，约不进枚举则就近吸附并留痕）
//	input_reference       → content[] 的 reference_image 项
//	first_frame_image     → content[] 的 first_frame 帧通道
//	last_frame_image      → content[] 的 last_frame 帧通道
//
// 首帧 / 首尾帧 / 参考图三种场景互斥（上游硬约束），混用由 translate.Create 前置 400。
//
// 🔴 **本端点只跑免费线**：调用方写什么模型名都**不参与选路** —— 上游槽位恒为 FreeOnlySlot
// （站点上唯一实测存在 procedure、也是唯一有实测免费窗口的那一条）。连带两处后果，
// 都是**有意的对外契约变更**：
//
//  1. X-Channel-Options.model_map **在本面上不生效**（面级策略优先于渠道映射；每次覆盖都留痕）；
//  2. **未命中的名字不再 400** —— 本面的调用方是 OpenAI SDK 用户，他们能拿到的信息只有
//     "请求失败了"，给一档免费成片远好过给失败。
//
// 分辨率同受这条政策约束：`_1080p` 一律**降级为 720p**（站点对 1080p 没有实测免费线），
// 随后照 PinnedDurations 钉到 8s —— 即"本面上的任何请求都落在免费档"。
// ⚠️ 方舟线 `/tasks` **不受本政策影响**（那条线的调用方可以自己点档、也自己承担费用）。
package openaivideos

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jushenzhidao/aivideomaker2api/internal/apierr"
	"github.com/jushenzhidao/aivideomaker2api/internal/jsonx"
	"github.com/jushenzhidao/aivideomaker2api/internal/sniff"
	"github.com/jushenzhidao/aivideomaker2api/internal/translate"
)

// Path 是本面的基础路径。
const Path = "/v1/videos"

// FreeOnlySlot 是**本端点唯一允许落到的上游槽位**（免费线）。
//
// 取值依据是站点事实、不是偏好：全站**唯一实测存在**的生成 procedure 是 `ai.minimaxH3`，
// 它也是唯一有实测免费窗口的模型（480p/10s 与 720p/8s 实测 paid=false）。
// 🔴 改它 = 改本端点的计费口径。
const FreeOnlySlot = "minimaxH3"

// freeResolutionDowngrade 是分辨率降级表：站点对 1080p **没有**实测免费线 ⇒ 本端点收到的
// `_1080p` 一律降级到 720p，时长再由 PinnedDurations 照常钉到 8s（只降级不钉时长仍会越线）。
var freeResolutionDowngrade = map[string]string{"1080p": "720p"}

// PinnedDurations 是分辨率 → **写死时长**（/v1/videos 专属）。语义是**无条件覆盖**
// 调用方传的 seconds，不是"缺省值"、也不是"仅越界时吸附" —— 调用方写 15s / 20s / 5s 一样被改。
//
// 两条都落在站点**已实测免费**的组合上（口径来自真实任务记录，不是页面文案）：
//   - 480p → 10s：站点对 480p 只收离散档位 5/10/15/20，10s 是这四档里已实测 paid=false 的最长档；
//   - 720p → 8s：合法时长连续 5–20，但免费线**到 8s 为止**（8s 实测 paid=false，9s/10s 实测 paid=true）。
//
// ⇒ 等价于「把 /v1/videos 钉死在免费区」，也是该端点 billed=false 恒成立的原因。
var PinnedDurations = map[string]int{"480p": 10, "720p": 8}

var resolutionSuffixRe = regexp.MustCompile(`(?i)_(480p|720p|1080p)\s*$`)

// size 枚举 → Ark ratio。keep_ratio / adaptive 都表示"由输入图决定"，上游不区分二者。
var sizeAliases = map[string]string{"adaptive": "adaptive", "keep_ratio": "adaptive"}

// sizeFallback 是 size **认不出**时落到的档位（只对 /v1/videos 生效，Ark /tasks 线仍 400）。
// 取 16:9 的依据是站点 UI 里它被默认选中 ⇒ 与上游默认一致。
const sizeFallback = "16:9"

// statusToOpenAI 是 Ark 状态 → OpenAI 状态词表。
var statusToOpenAI = map[string]string{
	"queued": "queued", "running": "in_progress", "succeeded": "completed",
	"failed": "failed", "cancelled": "failed",
}

// KnownCreateFields 是本面认的创建字段（其余只进 notes，不静默）。
var KnownCreateFields = map[string]bool{
	"model": true, "prompt": true, "seconds": true, "size": true,
	"input_reference": true, "first_frame_image": true, "last_frame_image": true,
}

// DecodeMediaValue 把调用方给的媒体值统一成 translate 认识的形态（URL 或 data URI）。
//
//   - http(s):// / data: 原样放行；
//   - b64Mode 时，裸值按 base64 解码，magic bytes 嗅探 MIME 后包成 data URI；
//   - 其余（既非 URL 也未声明 b64）原样透传，让上游给出明确的报错 —— 不猜。
func DecodeMediaValue(value, field string, b64Mode bool) (string, error) {
	s := strings.TrimSpace(value)
	if s == "" {
		return "", apierr.Param(field+" is empty", field)
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "data:") {
		return s, nil
	}
	if !b64Mode {
		return s, nil
	}
	clean := regexp.MustCompile(`\s+`).ReplaceAllString(s, "")
	buf, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return "", apierr.Param(field+": invalid base64 payload", field)
	}
	info := sniff.File(buf)
	if info.ContentType == "application/octet-stream" {
		info.ContentType = "image/png"
	}
	return "data:" + info.ContentType + ";base64," + base64.StdEncoding.EncodeToString(buf), nil
}

// SizeToRatio 把 size 字段翻成 Ark ratio，附映射说明（被改写的一定留痕）。
//
// 归一化逻辑（枚举原样 / WxH 约分 / 比例串与尺寸约不进枚举时**就近吸附** / 认不出时兜底）
// **只有一份**，在 translate.NormalizeRatio；这里只叠加 /v1/videos 自己的 keep_ratio 别名
// 与 sizeFallback。
//
// 🔴 与本服务 Ark 线在**同一个函数上的唯一区别**就是这个 fallback：这里认不出的值**不 400**，
// 落到 16:9 并留痕（兜底**照样进 notes**，所以不是静默改写）。
func SizeToRatio(size string) (string, []string, error) {
	s := strings.TrimSpace(size)
	if s == "" {
		return "", nil, nil
	}
	if mapped, ok := sizeAliases[s]; ok {
		if s == mapped {
			return mapped, nil, nil
		}
		return mapped, []string{fmt.Sprintf(
			`size=%q mapped to ratio=%q (the upstream has a single image-derived behavior; the distinction `+
				"does not exist upstream)", s, mapped)}, nil
	}
	// 语义词（portrait / landscape / square / ultrawide）走 SizePresets ——
	// 方舟线不传它（官方契约不收非标准值），只有 OpenAI 面认。
	return translate.NormalizeRatio(s, "size", sizeFallback, translate.SizePresets)
}

// BodyFromOpenAI 把 /v1/videos 创建字段翻成 Ark 请求体 + notes。
//
// notes 是"字段被改写/被忽略"的说明，调用方必须并进 warnings —— 映射不静默。
// 不在这里调用 translate.Create：它要重用同一份入口（计费、dry-run、前置拒绝都只在一处）。
func BodyFromOpenAI(fields map[string]any, referenceFormat string) (map[string]any, []string, error) {
	var notes []string

	// 键的遍历顺序不确定 ⇒ 排序后按字典序报，让"未知字段"提示稳定。
	unknown := make([]string, 0, len(fields))
	for k := range fields {
		if !KnownCreateFields[k] {
			unknown = append(unknown, k)
		}
	}
	sortStrings(unknown)
	for _, k := range unknown {
		notes = append(notes, fmt.Sprintf(
			`%q is not part of the OpenAI /v1/videos (Seedance) contract and was ignored`, k))
	}

	model := jsonx.String(fields["model"])
	if model == "" {
		return nil, nil, apierr.Param("model is required", "model")
	}
	prompt := strings.TrimSpace(jsonx.Raw(fields["prompt"]))
	if prompt == "" {
		return nil, nil, apierr.Param("prompt is required", "prompt")
	}

	// model 后缀决定分辨率档位（doubao-seedance-1-0-pro_1080p → 1080p）。
	var resolution string
	if m := resolutionSuffixRe.FindStringSubmatch(model); m != nil {
		resolution = strings.ToLower(m[1])
	}
	// 免费线约束：1080p **降级**。必须在**查表之前**降级，否则时长仍按表外原值透传
	// （= 仍可能计费）；且必须**留痕** —— 调用方要 1080p、拿到 720p 是一次改档。
	if downgraded, ok := freeResolutionDowngrade[resolution]; ok {
		notes = append(notes, fmt.Sprintf(
			`resolution %q downgraded to %q: this endpoint only runs the free tier, and the upstream has no `+
				"measured free window at %s", resolution, downgraded, resolution))
		resolution = downgraded
	}

	var duration *int
	if raw, ok := fields["seconds"]; ok && raw != nil && jsonx.String(raw) != "" {
		d, ok := jsonx.Int(raw)
		if !ok {
			return nil, nil, apierr.Param(
				fmt.Sprintf("seconds: expected an integer number of seconds, got %v", raw), "seconds")
		}
		duration = &d
	}

	// 「写死时长」：按分辨率覆盖 seconds（**无条件**，含调用方显式传值）。
	// ⚠️ 查表的键必须是**最终生效**的分辨率，而不是 resolution：本函数里 resolution 只可能
	// 来自 model 后缀，缺后缀时 body 里没有这个键、下游按 DefaultResolution 兜底 ⇒ 若按
	// resolution 直接查表，同一个 720p 请求会因"model 写没写后缀"分成"被钉住"与"没钉住"
	// 两种结果（9s 一次免费、一次计费），而调用方看不出差别。
	resKey := resolution
	if resKey == "" {
		resKey = translate.DefaultResolution
	}
	if pinned, ok := PinnedDurations[resKey]; ok {
		switch {
		case duration == nil:
			notes = append(notes, fmt.Sprintf("seconds omitted — this endpoint pins %s to %ds", resKey, pinned))
		case *duration != pinned:
			notes = append(notes, fmt.Sprintf(
				"seconds=%d overridden to %ds: this endpoint fixes the duration per resolution (%s → %ds)",
				*duration, pinned, resKey, pinned))
		}
		v := pinned
		duration = &v
	}

	ratioStr, sizeNotes, err := SizeToRatio(jsonx.String(fields["size"]))
	if err != nil {
		return nil, nil, err
	}
	notes = append(notes, sizeNotes...)

	b64Mode := strings.EqualFold(strings.TrimSpace(referenceFormat), "b64")

	refsRaw := fields["input_reference"]
	if refsRaw == nil || jsonx.String(refsRaw) == "" {
		refsRaw = []any{}
	}
	var refsList []any
	switch v := refsRaw.(type) {
	case []any:
		refsList = v
	case string, map[string]any:
		refsList = []any{v}
	default:
		refsList = []any{v}
	}

	// 归一成一个**扁平**列表。三种输入形态都要能落地：
	//   · JSON body 给数组（最正常）
	//   · 表单里的**重复部件**（input_reference 天生就是一个列表）
	//   · 「数组写成了字符串」—— `--form 'input_reference=["u1","u2"]'`
	// 🔴 第三种可能出现在**顶层**（JSON body 里写成字符串）或**元素上**（表单里天生是列表），
	// 两处都要救、都要摊平、都要**留痕**。
	var items []any
	rescued := 0
	for i, item := range refsList {
		parsed, wasList, err := listish(item, fmt.Sprintf("input_reference[%d]", i))
		if err != nil {
			return nil, nil, err
		}
		if wasList {
			items = append(items, parsed...)
			rescued += len(parsed)
			continue
		}
		items = append(items, parsed...)
	}
	if rescued > 0 {
		notes = append(notes, fmt.Sprintf(
			"input_reference was given as a JSON string and was parsed into %d entries — prefer repeating "+
				"the form field (`--form input_reference=...` once per file) or a JSON body", rescued))
	}

	var refURLs []string
	for i, item := range items {
		url := urlOf(item)
		if url == "" {
			continue
		}
		decoded, err := DecodeMediaValue(url, fmt.Sprintf("input_reference[%d]", i), b64Mode)
		if err != nil {
			return nil, nil, err
		}
		refURLs = append(refURLs, decoded)
	}

	frameMedia := func(key string) (string, error) {
		raw := fields[key]
		parsed, _, err := listish(raw, key)
		if err != nil {
			return "", err
		}
		if len(parsed) == 0 || (len(parsed) == 1 && parsed[0] == nil) {
			return "", nil
		}
		if len(parsed) > 1 {
			// 帧字段是**单值**通道。数组意味着调用方把"多张参考图"塞进来了 —— 取第一张
			// 就是静默丢素材（本项目的红线），必须明确拒并指路。
			return "", apierr.Param(fmt.Sprintf(
				"%s: 帧字段只接受单值，收到 %d 项 —— 多张参考图请改用 input_reference", key, len(parsed)), key)
		}
		first := parsed[0]
		url := urlOf(first)
		if url == "" {
			// `urlOf` 对认不出的形态返回空串，而调用方那边看起来是"提交成功、只是没有首帧"
			// —— **参考素材静默丢失是本项目的红线之一**。所以这里**明确拒**。
			return "", apierr.Param(fmt.Sprintf(
				"%s: 无法识别这个取值（%T）—— 请给 URL 字符串、{\"url\": ...} 或 "+
					"{\"image_url\": {\"url\": ...}}；**帧字段不接受数组**（多张参考图请用 input_reference）",
				key, first), key)
		}
		return DecodeMediaValue(url, key, b64Mode)
	}

	firstFrame, err := frameMedia("first_frame_image")
	if err != nil {
		return nil, nil, err
	}
	lastFrame, err := frameMedia("last_frame_image")
	if err != nil {
		return nil, nil, err
	}

	content := []any{map[string]any{"type": "text", "text": prompt}}
	for _, u := range refURLs {
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
	}
	if firstFrame != "" {
		content = append(content, map[string]any{
			"type": "image_url", "role": "first_frame", "image_url": map[string]any{"url": firstFrame}})
	}
	if lastFrame != "" {
		content = append(content, map[string]any{
			"type": "image_url", "role": "last_frame", "image_url": map[string]any{"url": lastFrame}})
	}

	body := map[string]any{"model": model, "content": content}
	if resolution != "" {
		body["resolution"] = resolution
	}
	if ratioStr != "" {
		body["ratio"] = ratioStr
	}
	if duration != nil {
		body["duration"] = *duration
	}
	return body, notes, nil
}

// TaskView 把内部任务视图翻成 Chatfire `/v1/videos/{id}` 契约形状（**恰好六个字段**）。
//
//   - status：queued / in_progress / completed / failed（OpenAI Videos 家族词表）；
//   - progress：终态成功 100，其余 0 —— 上游没有可读的百分比，不编造中间值；
//   - video_url：仅 completed 且真有产出时给值，其余为 null；
//   - created_at：epoch 秒；上游取不到时回退到**本服务创建记录**的时间。
func TaskView(view map[string]any, createdAtFallback int64) map[string]any {
	status, ok := statusToOpenAI[jsonx.String(view["status"])]
	if !ok {
		status = "queued"
	}
	completed := status == "completed"
	var videoURL any
	if completed {
		if content := jsonx.Map(view["content"]); content != nil {
			if u := jsonx.String(content["video_url"]); u != "" {
				videoURL = u
			}
		}
	}
	createdAt, ok := jsonx.Int(view["created_at"])
	if !ok || createdAt <= 0 {
		createdAt = int(createdAtFallback)
	}
	progress := 0
	if completed {
		progress = 100
	}
	return map[string]any{
		"id":         view["id"],
		"object":     "video",
		"status":     status,
		"progress":   progress,
		"video_url":  videoURL,
		"created_at": createdAt,
	}
}

// ---------------------------------------------------------------- helpers --

// listish 把「数组被写成了字符串」救回来；救不回来就**明确拒**。
//
// Chatfire 的 curl 里很常见的写法是 `--form 'input_reference=["u1","u2"]'` —— 值是
// **一个 JSON 字符串**而不是重复的部件。原样透传会变成 referenceImageUrls 里的**一个垃圾项**
// （既不是 URL 也不是 data URI），一路静默到转存阶段才炸。
// 只认以 `[` / `{` 开头的串 —— URL 与裸 base64 都不会这么开头，所以不误伤。
func listish(value any, field string) ([]any, bool, error) {
	s, isString := value.(string)
	if !isString {
		if value == nil {
			return nil, false, nil
		}
		return []any{value}, false, nil
	}
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "[") && !strings.HasPrefix(trimmed, "{") {
		return []any{value}, false, nil
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return nil, false, apierr.Param(fmt.Sprintf(
			"%s: 看着像要传 JSON 数组/对象，但不是合法 JSON：%q —— 多值请在表单里**重复该字段**"+
				"（`--form a --form a`），或改用 JSON body", field, truncateRunes(trimmed, 80)), field)
	}
	if list, ok := parsed.([]any); ok {
		return list, true, nil
	}
	return []any{parsed}, true, nil
}

// urlOf 兼容三种元素形态：url 字符串 / {url} / OpenAI 官方的 {image_url:{url}}。
func urlOf(item any) string {
	switch v := item.(type) {
	case string:
		return strings.TrimSpace(v)
	case map[string]any:
		if u := jsonx.String(v["url"]); u != "" {
			return u
		}
		switch holder := v["image_url"].(type) {
		case map[string]any:
			return jsonx.String(holder["url"])
		case string:
			return strings.TrimSpace(holder)
		}
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func sortStrings(in []string) {
	// 小切片，插入排序足够；避免为一个提示再引 sort。
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
