// Package slots 解析「调用方模型名 → 上游槽位」，以及渠道级选项头 X-Channel-Options。
//
// # 模型映射（model_map）
//
//	| 键 | 语义 |
//	| --- | --- |
//	| `model_map` | 模型名 → 上游槽位 的映射表：**精确键** + **至多一条 `*` 兜底**。别名 `upstream_model_map` |
//	| ~~`model`~~ | 已撤除（渠道钉住槽位）。遗留它 ⇒ 渠道配置错误，**不静默忽略** |
//
// # 判定顺序
//
//	① 映射表**精确**键命中            → slot = 表值                source=model_map
//	② 映射表的 **`*` 兜底**命中        → slot = 表值                source=model_map
//	③ 名字本身是已知上游槽位（剥后缀后）→ slot = 名字（逐字透传）   source=passthrough
//	④ 其余                            → 400（报文列出已知槽位与改法）
//
// ② 排在 ③ 前面是刻意的：兜底＝渠道声明"除我明确列出的以外，一律落这一档"，
// 它**要**能覆盖调用方写出的槽位名，否则运维"这个渠道只跑某一档"的意图会落空。
// 代价是：调用方点名别的模型会被改写 ⇒ 每次改写都进 warnings 并留 `model_source` 证据。
//
// 🔴 不留任何名字猜测表：曾经有一张"名字里含 seedance 就走 seedance20"的正则表，
// 它把 5 个不同代次的原生 ID 静默压进同一个槽位且不回告警（实测账单差 7.3 倍）。
// "哪个名字落哪个槽位"是**控制面知识**，只能由渠道用 model_map 声明。
package slots

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jushenzhidao/aivideomaker2api/internal/apierr"
	"github.com/jushenzhidao/aivideomaker2api/internal/jsonx"
)

// HeaderName 是渠道选项头的规范写法（HTTP 头名大小写不敏感）。
const HeaderName = "X-Channel-Options"

const (
	modelMapKey   = "model_map"
	modelMapAlias = "upstream_model_map"
	// CatchAll 是**唯一**被识别的通配形态、且至多一条。
	CatchAll = "*"
	// ProcedurePrefix 是站点 tRPC procedure 的命名前缀（`ai.minimaxH3` ⇒ `ai.`）。
	ProcedurePrefix = "ai."
)

// supportedKeys 是本层认的渠道键（其余只告警不生效）。
var supportedKeys = []string{modelMapKey, modelMapAlias}

// removedKeys 是**撤除过**的渠道键：出现即报错，不静默忽略 ——
// 以为它还生效的运维会以为这个渠道只跑某个槽位，而实际上任何合法槽位名都会被逐字发出。
var removedKeys = []string{"model"}

// SiteModelKeys 是站点网页端的 11 个模型键（i18n 文案，原项目 docs/web-reverse/model-inventory.md）。
// ⚠️ 它回答"站点认识哪些模型名"，**不回答**"哪条 procedure 存在"——两件事别混。
var SiteModelKeys = []string{
	"default", "HappyHorse", "seedance20", "wan27", "seedance2", "seedance25",
	"kling2_5", "kling3", "ltx23", "veo3Fast", "veo31Fast",
}

// VerifiedSlots 是**已实测**存在 procedure 的槽位。
// 唯一证据：三次真实提交的成片 URL 里一律是 `minimax_h3`，且 `ai.minimaxH3` 是唯一
// 被观测到的生成 procedure（报告 AVM12-OPEN-UPSTREAM）。
var verifiedSlots = map[string]bool{"minimaxH3": true}

// 解析来源，进证据字段（effective.model_source）。
const (
	SourceModelMap    = "model_map"
	SourcePassthrough = "passthrough"
	// SourceFaceForced 是**面级策略**强制落定的槽位。
	//
	// 目前有两个面用它：`/v1/videos`（只跑免费线）与 MiniMax 面（只跑 web 线唯一已实测的
	// procedure）。它与已撤除的渠道键 `model` 不是一回事：那个是渠道配置要钉住槽位，
	// 这个是面自己的对外契约、由代码传入、每次改写都留痕。
	//
	// ⚠️ 取值刻意**不叫** `free_only`：MiniMax 面会原样尊重调用方的时长/分辨率
	// （可能计费），把它的来源标成 `free_only` 会让排障的人误判计费口径 ——
	// 证据字段的第一要求是**不撒谎**，而不是"看起来更省事"。
	SourceFaceForced = "face_forced"
)

var resolutionSuffixRe = regexp.MustCompile(`(?i)_(480p|720p|1080p)\s*$`)

// KnownSlots 返回槽位值域（站点模型键 ∪ 已实测槽位，保序去重）。
func KnownSlots() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(SiteModelKeys)+1)
	for _, k := range SiteModelKeys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	extra := make([]string, 0, len(verifiedSlots))
	for k := range verifiedSlots {
		extra = append(extra, k)
	}
	sort.Strings(extra)
	for _, k := range extra {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func isKnownSlot(name string) bool {
	for _, k := range KnownSlots() {
		if k == name {
			return true
		}
	}
	return false
}

// BareModelName 剥掉 `provider/` 段与分辨率后缀，留下可与槽位比对的名字。
func BareModelName(model string) string {
	name := strings.TrimSpace(model)
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = strings.TrimSpace(name[i+1:])
	}
	return strings.TrimSpace(resolutionSuffixRe.ReplaceAllString(name, ""))
}

// ParamError 是本层产生的参数错误（对外 400 InvalidParameter）。
type ParamError = apierr.ParamError

func configError(msg string) error {
	// 对外仍是 400 InvalidParameter，但 `param` 指向头名、报文里写明"这是渠道配置问题"
	// —— 否则调用方会在自己的 model 字段上找半天，而问题在渠道那一侧。
	return apierr.Param(msg, HeaderName)
}

// ParseChannelOptions 把头原文解析成 map。**只做结构校验**，语义校验在 ResolveModel 里。
//
// 空串 = 没配。非 JSON / 非对象一律拒绝 —— 一个坏掉的头不该被当成"没配"
// （那会静默用默认透传跑掉，而运维以为自己的映射生效了）。
func ParseChannelOptions(raw string) (map[string]any, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return nil, nil
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return nil, configError(fmt.Sprintf(
			"X-Channel-Options is not valid JSON (%v); it must be a JSON object, e.g. "+
				`{"model_map": {"doubao-seedance-2-0-260128": "seedance20"}}`, err))
	}
	if obj == nil {
		return nil, configError("X-Channel-Options must be a JSON object, not null — supported keys: " +
			strings.Join(supportedKeys, ", "))
	}
	return obj, nil
}

// ModelResolution 是一次请求的模型解析结果（**证据的载体**：调用方写了什么 → 上游拿到什么）。
type ModelResolution struct {
	Slot     string
	Source   string
	Verified bool
	Warnings []string
}

func rejectRemovedKeys(options map[string]any) error {
	for _, key := range removedKeys {
		if _, ok := options[key]; ok {
			return configError(fmt.Sprintf(
				"X-Channel-Options.%s was removed: the channel-level pinned slot is gone. Use model_map "+
					"instead — either an exact key (the name the caller sends → the upstream slot name) or "+
					`the single "*" catch-all — or have callers send the slot name verbatim. Remove the %q `+
					"key from the channel configuration.", key, key))
		}
	}
	return nil
}

func modelMap(options map[string]any) (map[string]string, error) {
	raw, hasRaw := options[modelMapKey]
	alias, hasAlias := options[modelMapAlias]
	if hasRaw && hasAlias && !sameJSON(raw, alias) {
		return nil, configError("X-Channel-Options.model_map and upstream_model_map are both set and differ; " +
			"keep exactly one (they are two names for the same table)")
	}
	src := raw
	if !hasRaw {
		src = alias
	}
	if src == nil {
		return nil, nil
	}
	obj := jsonx.Map(src)
	if obj == nil {
		return nil, configError(`X-Channel-Options.model_map must be a JSON object of ` +
			`{"<name the caller sends>": "<upstream slot>"}`)
	}
	out := make(map[string]string, len(obj))
	folded := make(map[string]string, len(obj))
	// 遍历顺序不确定 ⇒ 先把键排序，让"重复键"报错稳定可复现。
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		name := strings.TrimSpace(key)
		slot := strings.TrimSpace(jsonx.String(obj[key]))
		if name == "" || slot == "" {
			return nil, configError(fmt.Sprintf("model_map entries must be non-empty strings (got %q: %q)",
				key, jsonx.String(obj[key])))
		}
		if strings.Contains(name, "*") && name != CatchAll {
			return nil, configError(fmt.Sprintf(
				"model_map key %q uses a wildcard pattern; only the bare %q catch-all is supported (at most "+
					"one entry). Write the names out as exact keys (e.g. \"doubao-seedance-2-0-260128\"), or "+
					"keep a single %q entry as the fallback for names the table does not list.",
				name, CatchAll, CatchAll))
		}
		if !isKnownSlot(slot) {
			return nil, configError(fmt.Sprintf(
				"model_map[%q]=%q is not a known upstream slot (expected one of: %s)",
				name, slot, strings.Join(KnownSlots(), ", ")))
		}
		low := strings.ToLower(name)
		if prev, dup := folded[low]; dup {
			return nil, configError(fmt.Sprintf(
				"model_map has duplicate entries for %q and %q (JSON would silently keep only one of them)",
				name, prev))
		}
		folded[low] = name
		out[name] = slot
	}
	return out, nil
}

func sameJSON(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ab) == string(bb)
}

func resolveByName(name string, table map[string]string) (slot, source, note string, ok bool) {
	if s, hit := table[name]; hit {
		return s, SourceModelMap, fmt.Sprintf(
			"model %q → upstream slot %q (exact hit in the channel model_map)", name, s), true
	}
	if s, hit := table[CatchAll]; hit {
		return s, SourceModelMap, fmt.Sprintf(
			"model %q → upstream slot %q (the channel's model_map %q fallback overrides every name it does "+
				"not list explicitly)", name, s, CatchAll), true
	}
	if isKnownSlot(name) {
		// 透明转发：请求值 = 执行值，没有可留痕的改写。
		return name, SourcePassthrough, "", true
	}
	return "", SourcePassthrough, "", false
}

// ResolveModel 把调用方模型名解析成 (槽位, 来源, 是否已实测, 告警)。**不猜、不兜底**。
//
// forceSlot 是**面级策略**（不是渠道配置）：给了它就跳过 ②③④ 三条判定 ——
// 连"不认识的名字"也不会 400（那正是"全部兜底到免费档"的含义）；槽位改写成它、
// 来源标 face_forced，而**调用方的名字一字不改**（requested.model 仍是原值，证据不丢）。
//
// ⚠️ 渠道配置的校验（撤除键 / 非法通配）在它之前照旧执行 —— 强制槽位不该让一份
// 坏配置静默通过。
func ResolveModel(model string, options map[string]any, forceSlot string) (ModelResolution, error) {
	if options == nil {
		options = map[string]any{}
	}
	if err := rejectRemovedKeys(options); err != nil {
		return ModelResolution{}, err
	}
	name := BareModelName(model)
	if name == "" {
		return ModelResolution{}, apierr.Param("model is required", "model")
	}
	table, err := modelMap(options)
	if err != nil {
		return ModelResolution{}, err
	}

	var warnings []string
	intended, intendedSource, intendedNote, intendedOK := resolveByName(name, table)

	var slot, source string
	switch {
	case forceSlot != "":
		slot, source = forceSlot, SourceFaceForced
		if intended != forceSlot {
			var what string
			if intendedOK {
				what = fmt.Sprintf("would have gone to upstream slot %q", intended)
				if intendedNote != "" {
					what += " (" + intendedNote + ")"
				} else {
					what += " (passthrough)"
				}
			} else {
				what = "would have been refused (unknown upstream slot)"
			}
			warnings = append(warnings, fmt.Sprintf(
				"model %q %s, but this face only runs one upstream slot: the request goes to %q instead",
				name, what, forceSlot))
		}
	case !intendedOK:
		hint := " No model_map is configured on this channel."
		if len(table) > 0 {
			keys := make([]string, 0, len(table))
			for k := range table {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			hint = " The channel model_map declares: " + strings.Join(keys, ", ") + "."
		}
		return ModelResolution{}, apierr.Param(fmt.Sprintf(
			"unknown model %q. This layer forwards the model name verbatim, so it must be one of the "+
				"upstream slots: %s.%s Add an exact entry (or a single %q catch-all) to the channel's "+
				"X-Channel-Options.model_map.",
			name, strings.Join(KnownSlots(), ", "), hint, CatchAll), "model")
	default:
		slot, source = intended, intendedSource
		if intendedNote != "" {
			warnings = append(warnings, intendedNote)
		}
	}

	verified := verifiedSlots[slot]
	if !verified {
		warnings = append(warnings, fmt.Sprintf(
			"upstream slot %q has a known site model but no verified procedure: it is sent as %q, which is "+
				"inferred from the naming pattern and has not been confirmed against the site yet",
			slot, ProcedureForSlot(slot)))
	}

	var unknown []string
	for k := range options {
		known := false
		for _, s := range supportedKeys {
			if k == s {
				known = true
				break
			}
		}
		if !known {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		warnings = append(warnings, fmt.Sprintf(
			"X-Channel-Options keys %s are not supported by this layer and were ignored (supported: %s)",
			strings.Join(unknown, ", "), strings.Join(supportedKeys, ", ")))
	}
	return ModelResolution{Slot: slot, Source: source, Verified: verified, Warnings: warnings}, nil
}

// ProcedureForSlot 是槽位 → 站点 tRPC procedure 名。
//
// 🔴 这里是一个**未验证假设**：全站点只实测出 `ai.minimaxH3` 一条生成 procedure，
// 其余槽位的 procedure 名按同一命名形态推断。因此未实测槽位照发，但 ResolveModel
// 会带告警、证据里标 verified=false。
func ProcedureForSlot(slot string) string {
	// 站点自己的 procedure 名**不一定等于** UI 上的模型键（大小写、缩写都可能不同）。
	// 这张表只放**实测存在**的映射，证据见 docs/evidence/api-surface.md。
	//
	// 🔴 典型坑：UI 叫 `HappyHorse`，站点 procedure 却是小写 `ai.happyhorse`；
	// 照名字拼会拿到 404 "No mutation-procedure on path ai.HappyHorse"。
	if proc, ok := verifiedProcedures[slot]; ok {
		return proc
	}
	return ProcedurePrefix + slot
}

// verifiedProcedures 是实测存在的 槽位 → procedure 映射。
//
// 只写探到过的：`ai.seedance20` / `ai.seedance25` / `ai.happyhorse` / `ai.ltx23` /
// `ai.minimaxH3` 返回 401（存在、需鉴权）；`ai.seedance2` / `ai.wan27` / `ai.kling3` /
// `ai.kling2_5` / `ai.veo3Fast` / `ai.veo31Fast` / `ai.default` 返回 404（**不存在**）。
var verifiedProcedures = map[string]string{
	"minimaxH3":  "ai.minimaxH3",
	"HappyHorse": "ai.happyhorse",
	"seedance20": "ai.seedance20",
	"seedance25": "ai.seedance25",
	"ltx23":      "ai.ltx23",
}
