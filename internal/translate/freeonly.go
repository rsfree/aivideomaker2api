package translate

import (
	"fmt"
	"math"
	"strings"

	"github.com/rsfree/aivideomaker2api/internal/jsonx"
)

// free-only 部署策略：本微服务对外**只提供不扣费的模型**。
//
// 免费集合只有四档（实测口径，不是站点文案）：
//
//	480p @ 5s / 10s      720p @ 5s / 8s      （tier=turbo，网页会话）
//
// 任何落在这四档之外的请求都**向上吸附**到最近的免费档，不报错：
//
//	extra_body.aivideomaker_channel = official  → 丢弃（官方通道每单必扣）
//	extra_body.aivideomaker_tier    = base      → 丢弃（base 档免费窗口内也扣，+1 积分/秒）
//	resolution = 1080p / 其它                    → 720p（往上没有免费档，只能下到最高免费档）
//	duration = 6s                                → 8s（720p）／10s（480p）
//	duration = 11s+                              → 该分辨率的免费上界（480p→10s，720p→8s）
//
// 为什么吸附而不是 400：调用方是网关，1080p / 15s 是它给所有模型发的一份通用参数；
// 拒绝会让整条链路失败，而吸附只会让这条片子落在免费档 —— 对"只提供免费档"的
// 服务来说，后者才是契约内的行为。**往上的方向是刻意的**：宁可给长一点，
// 也不要把调用方要的 6s 悄悄剪成 5s。
//
// 但**改写不等于无声**：每个被改写/丢弃的字段都以 `原名→实际值` 形式返回，由上层写进
// warnings、服务日志（marker=free_only_adjusted）与任务账本。静默的对象是调用方，
// 不是运维 —— "谁把 1080p/15s 发进来了"必须还能查到。
//
// 不在这里处理**官方独有模型**（t2v / i2v / wan30 / seedance20 / HappyHorse）：
// 它们没有任何免费档，吸附也变不出一条免费路径。那属于"本服务不提供这个模型"，
// 由 Create 报错，不属于"参数有隐患"。
const (
	optChannel = "aivideomaker_channel"
	optTier    = "aivideomaker_tier"
)

// SanitizeFree 把请求体改写成"只可能落在免费档"的形状。返回新 map（不改调用方的原 map），
// 以及被改写/丢弃的字段清单。
func SanitizeFree(body map[string]any) (map[string]any, []string) {
	if body == nil {
		return nil, nil
	}
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	adjusted := []string{}

	// 1) 通道与质量档：这两个开关本身就是"付费开关"，一律删掉。
	if extra := jsonx.Map(out["extra_body"]); extra != nil {
		cp := make(map[string]any, len(extra))
		for k, v := range extra {
			cp[k] = v
		}
		if v := strings.ToLower(strings.TrimSpace(jsonx.String(cp[optChannel]))); v != "" && v != "web" {
			adjusted = append(adjusted, fmt.Sprintf("%s=%q→web", optChannel, v))
		}
		delete(cp, optChannel)
		if v := strings.ToLower(strings.TrimSpace(jsonx.String(cp[optTier]))); v != "" && v != "turbo" {
			adjusted = append(adjusted, fmt.Sprintf("%s=%q→turbo", optTier, v))
		}
		delete(cp, optTier)
		if len(cp) == 0 {
			delete(out, "extra_body")
		} else {
			out["extra_body"] = cp
		}
	}

	// 2) 分辨率：免费档只有 480p / 720p，1080p 无论多短都计费（实测 5s 扣 20 积分）。
	// 1080p 只能**往下**到 720p —— 往上不存在免费档。
	res := DefaultResolution
	if raw := jsonx.String(out["resolution"]); raw != "" {
		low := strings.ToLower(strings.TrimSpace(raw))
		if WebResolutions[low] {
			out["resolution"] = low // 只归一化大小写（`720P` → `720p`），仍是免费档
			res = low
		} else {
			out["resolution"] = DefaultResolution
			res = DefaultResolution
			adjusted = append(adjusted, fmt.Sprintf("resolution=%q→%s", raw, DefaultResolution))
		}
	}

	// 3) 时长：**向上**吸附到该分辨率最近的免费档；超过上界则取上界。
	// 类型错（字符串 / 对象）没法吸附，按"没给"处理（Create 默认 5s）。
	if raw, ok := out["duration"]; ok && raw != nil && jsonx.String(raw) != "" {
		if d, ok := jsonx.Float(raw); ok {
			if d == -1 {
				// 站点语义是"智能时长"，Create 会映射成 5s（免费），无需改写。
			} else {
				secs := int(math.Round(d))
				snapped := SnapFreeDuration(res, secs)
				if snapped != secs {
					adjusted = append(adjusted, fmt.Sprintf("duration=%ds→%ds", secs, snapped))
				}
				out["duration"] = snapped
			}
		} else {
			adjusted = append(adjusted, fmt.Sprintf("duration=%q→5s", jsonx.String(raw)))
			delete(out, "duration")
		}
	}

	// 4) frames：只在 duration 缺省时才轮到它决定时长（见 Create），所以只在那种情况下处理。
	if _, hasDuration := out["duration"]; !hasDuration {
		if raw, ok := out["frames"]; ok && raw != nil && jsonx.String(raw) != "" {
			if f, ok := jsonx.Float(raw); ok {
				secs := int(math.Max(1, math.Round(f/24)))
				snapped := SnapFreeDuration(res, secs)
				if snapped != secs {
					// 直接落成 duration：留 frames 会让 Create 再算一遍原始时长并撞枚举校验。
					out["duration"] = snapped
					delete(out, "frames")
					adjusted = append(adjusted, fmt.Sprintf("frames=%v(%ds)→%ds", raw, secs, snapped))
				}
			} else {
				adjusted = append(adjusted, fmt.Sprintf("frames=%q→5s", jsonx.String(raw)))
				delete(out, "frames")
			}
		}
	}

	return out, adjusted
}

// SnapFreeDuration 把一个时长**向上**吸附到该分辨率的免费枚举。
//
//	480p: 5s / 10s     720p: 5s / 8s
//
// 小于首档取首档；超过末档取末档（再往上没有免费档，除非换分辨率 —— 那是另一件事）。
// 枚举本身是升序的（见 DurationAllowed），这里不排序、不猜。
func SnapFreeDuration(res string, secs int) int {
	opts := DurationOptions(res)
	if len(opts) == 0 {
		return 5
	}
	best := opts[0]
	for _, v := range opts {
		if v >= secs {
			return v
		}
		best = v
	}
	return best
}
