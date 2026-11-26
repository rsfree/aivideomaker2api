package translate

import (
	"strings"
	"testing"
)

func textOnly(model string) map[string]any {
	return map[string]any{
		"model":   model,
		"content": []any{map[string]any{"type": "text", "text": "a cat"}},
	}
}

// 免费档只有四档：480p@5/10s、720p@5/8s。其余一律**向上吸附**，不报错。
func TestSanitizeFreeSnapsIntoFreeTier(t *testing.T) {
	body := textOnly("minimaxH3")
	body["resolution"] = "1080p"
	body["duration"] = 15
	body["extra_body"] = map[string]any{
		"aivideomaker_channel":           "official",
		"aivideomaker_tier":              "base",
		"aivideomaker_prompt_enrichment": true,
	}

	got, adjusted := SanitizeFree(body)

	// 1080p → 720p（往上没有免费档）；15s → 720p 的免费上界 8s。
	if got["resolution"] != "720p" {
		t.Errorf("resolution = %v, want 720p", got["resolution"])
	}
	if got["duration"] != 8 {
		t.Errorf("duration = %v, want 8 (720p free ceiling)", got["duration"])
	}
	extra, _ := got["extra_body"].(map[string]any)
	if extra == nil {
		t.Fatal("extra_body lost — the harmless keys must survive")
	}
	if _, ok := extra["aivideomaker_channel"]; ok {
		t.Error("aivideomaker_channel survived — the official line bills every call")
	}
	if _, ok := extra["aivideomaker_tier"]; ok {
		t.Error("aivideomaker_tier survived — tier=base is billed even inside the free window")
	}
	if extra["aivideomaker_prompt_enrichment"] != true {
		t.Error("harmless extra_body keys must be preserved verbatim")
	}

	joined := strings.Join(adjusted, " | ")
	for _, want := range []string{"channel", "tier", `resolution="1080p"→720p`, "duration=15s→8s"} {
		if !strings.Contains(joined, want) {
			t.Errorf("adjustment trace missing %q: %s", want, joined)
		}
	}

	// 原 map 不能被改写（它还要进 requested 证据）。
	if body["resolution"] != "1080p" || body["duration"] != 15 {
		t.Errorf("SanitizeFree mutated the caller's body: %v", body)
	}
}

func TestSanitizeFreeSnappingTable(t *testing.T) {
	cases := []struct {
		name    string
		res     any
		dur     any
		wantRes any
		wantDur any // nil = key must be absent
	}{
		{"480p/5s 不动", "480p", 5, "480p", 5},
		{"480p/10s 不动", "480p", 10, "480p", 10},
		{"720p/5s 不动", "720p", 5, "720p", 5},
		{"720p/8s 不动", "720p", 8, "720p", 8},
		{"720P 归一化", "720P", 5, "720p", 5},
		{"字符串时长", "480p", "5", "480p", 5},
		{"智能时长 -1", "720p", -1, "720p", -1},
		// 往上吸附：6s → 8s（720p）/ 10s（480p）
		{"720p/6s→8s", "720p", 6, "720p", 8},
		{"480p/6s→10s", "480p", 6, "480p", 10},
		{"480p/8s→10s", "480p", 8, "480p", 10},
		{"480p/4s→5s", "480p", 4, "480p", 5},
		// 超过上界取上界（该分辨率再往上没有免费档）
		{"720p/10s→8s", "720p", 10, "720p", 8},
		{"720p/15s→8s", "720p", 15, "720p", 8},
		{"480p/11s→10s", "480p", 11, "480p", 10},
		{"480p/20s→10s", "480p", 20, "480p", 10},
		// 1080p 只能往下；时长按 720p 吸附
		{"1080p/5s→720p/5s", "1080p", 5, "720p", 5},
		{"1080p/10s→720p/8s", "1080p", 10, "720p", 8},
		// 非数字时长没法吸附 ⇒ 按没给处理
		{"非数字时长", "480p", "abc", "480p", nil},
	}
	for _, c := range cases {
		body := textOnly("minimaxH3")
		body["resolution"] = c.res
		body["duration"] = c.dur
		got, _ := SanitizeFree(body)

		if got["resolution"] != c.wantRes {
			t.Errorf("%s: resolution = %v, want %v", c.name, got["resolution"], c.wantRes)
		}
		if c.wantDur == nil {
			if v, ok := got["duration"]; ok {
				t.Errorf("%s: duration = %v, want dropped", c.name, v)
			}
		} else if got["duration"] != c.wantDur {
			t.Errorf("%s: duration = %v, want %v", c.name, got["duration"], c.wantDur)
		}
	}
}

func TestSnapFreeDuration(t *testing.T) {
	cases := []struct {
		res  string
		in   int
		want int
	}{
		{"480p", 1, 5}, {"480p", 5, 5}, {"480p", 6, 10}, {"480p", 10, 10}, {"480p", 99, 10},
		{"720p", 0, 5}, {"720p", 5, 5}, {"720p", 6, 8}, {"720p", 8, 8}, {"720p", 9, 8},
	}
	for _, c := range cases {
		if got := SnapFreeDuration(c.res, c.in); got != c.want {
			t.Errorf("SnapFreeDuration(%s, %d) = %d, want %d", c.res, c.in, got, c.want)
		}
	}
}

func TestSanitizeFreeSnapsBillableFrames(t *testing.T) {
	body := textOnly("minimaxH3")
	body["frames"] = 360 // 360/24 = 15s → 720p 上界 8s
	got, adjusted := SanitizeFree(body)
	if got["frames"] != nil {
		t.Errorf("frames = %v survived; 15s would be billed", got["frames"])
	}
	if got["duration"] != 8 {
		t.Errorf("duration = %v, want 8 (snapped from 15s)", got["duration"])
	}
	if !strings.Contains(strings.Join(adjusted, " | "), "frames=360") {
		t.Errorf("adjustment trace missing the frames rewrite: %v", adjusted)
	}

	body = textOnly("minimaxH3")
	body["frames"] = 120 // 5s，免费，原样保留交给 Create
	got, _ = SanitizeFree(body)
	if got["frames"] == nil {
		t.Error("5s frames must survive (free tier)")
	}
}

// Create 在 free-only 下：不出错、吸附到免费档、但 requested 仍保留调用方原值。
func TestCreateFreeOnlyCoercesToFreeTier(t *testing.T) {
	body := textOnly("minimaxH3")
	body["resolution"] = "1080p"
	body["duration"] = 6
	body["extra_body"] = map[string]any{
		"aivideomaker_channel": "official",
		"aivideomaker_tier":    "base",
	}

	plan, err := Create(body, nil, "", true)
	if err != nil {
		t.Fatalf("Create(freeOnly) must not reject billing-risk parameters: %v", err)
	}
	if got := plan.Effective["resolution"]; got != DefaultResolution {
		t.Errorf("effective resolution = %v, want %s", got, DefaultResolution)
	}
	if got := plan.Effective["duration"]; got != 8 {
		t.Errorf("effective duration = %v, want 8 (6s snaps up to 8s)", got)
	}
	if got := plan.Effective["billed"]; got != false {
		t.Errorf("effective billed = %v, want false", got)
	}
	if got := plan.WebParams["tier"]; got != "turbo" {
		t.Errorf("upstream tier = %v, want turbo", got)
	}
	if got := plan.WebParams["channel"]; got != nil {
		t.Errorf("upstream channel = %v, want absent (web line)", got)
	}
	// 证据：调用方写的原值必须留在 requested 里。
	if got := plan.Requested["resolution"]; got != "1080p" {
		t.Errorf("requested.resolution = %v, want the caller's 1080p", got)
	}
	if got := plan.Requested["duration"]; got != 6 {
		t.Errorf("requested.duration = %v, want 6", got)
	}
	if len(plan.FreeOnlyAdjusted) == 0 {
		t.Error("FreeOnlyAdjusted is empty — the operator loses the only trace of the rewrite")
	}
}

// 官方独有模型没有免费档：不能"吸附"糊过去，必须明说不提供。
func TestCreateFreeOnlyRejectsOfficialOnlyModel(t *testing.T) {
	_, err := Create(textOnly("wan30"), nil, "", true)
	if err == nil {
		t.Fatal("wan30 has no free window and must be refused")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "free-only") {
		t.Errorf("error = %v, want it to name the free-only policy", err)
	}
}

// 关掉开关时行为必须与从前逐字节一致（1080p 仍然 400，不静默降级）。
func TestCreateWithoutFreeOnlyStillRejects1080p(t *testing.T) {
	body := textOnly("minimaxH3")
	body["resolution"] = "1080p"
	if _, err := Create(body, nil, "", false); err == nil {
		t.Fatal("1080p must stay rejected when free_only is off")
	}
}
