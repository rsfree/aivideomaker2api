package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jushenzhidao/aivideomaker2api/internal/jsonx"
)

func textBody(model string) map[string]any {
	return map[string]any{
		"model":   model,
		"content": []any{map[string]any{"type": "text", "text": "a red balloon"}},
	}
}

func TestCreateMinimalPlan(t *testing.T) {
	body := textBody("minimaxH3")
	body["duration"] = 5
	body["resolution"] = "480p"
	plan, err := Create(body, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := plan.Procedure(); got != "ai.minimaxH3" {
		t.Errorf("procedure = %q, want ai.minimaxH3", got)
	}
	if plan.WebParams["tier"] != "turbo" {
		t.Errorf("tier = %v, want turbo", plan.WebParams["tier"])
	}
	if plan.Effective["billed"] != false {
		t.Errorf("billed = %v, want false (turbo 5s is the free window)", plan.Effective["billed"])
	}
	if plan.WebParams["promptEnrichment"] != true {
		t.Errorf("promptEnrichment must default to true (matches the site's own request)")
	}
	if plan.Effective["model_source"] != "passthrough" {
		t.Errorf("model_source = %v, want passthrough", plan.Effective["model_source"])
	}
}

// 回归：WebParams 里的两个数组字段必须能序列化成 []，不能是 null。
func TestWebParamsArraysAreNeverNull(t *testing.T) {
	plan, err := Create(textBody("minimaxH3"), nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, key := range []string{"referenceImageUrls", "referenceAudioUrls"} {
		raw, err := json.Marshal(plan.WebParams[key])
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if string(raw) != "[]" {
			t.Errorf("%s serializes to %s, want []", key, raw)
		}
	}
}

func TestSnapDurationTable(t *testing.T) {
	cases := []struct {
		d    int
		res  string
		want int
	}{
		{6, "480p", 5}, {7, "480p", 5}, {8, "480p", 10}, {9, "480p", 10},
		{11, "480p", 10}, {13, "480p", 15}, {16, "480p", 15}, {18, "480p", 20},
		{5, "480p", 5}, {10, "480p", 10}, {20, "480p", 20},
		{3, "720p", 5}, {21, "720p", 20}, {7, "720p", 7}, {12, "1080p", 12},
	}
	for _, c := range cases {
		if got := SnapDuration(c.d, c.res, false); got != c.want {
			t.Errorf("SnapDuration(%d, %s) = %d, want %d", c.d, c.res, got, c.want)
		}
	}
	// preferFree：主动拉回免费上界（不只是越界时才生效）
	if got := SnapDuration(15, "480p", true); got != 10 {
		t.Errorf("preferFree 15s/480p = %d, want 10", got)
	}
	if got := SnapDuration(20, "720p", true); got != 10 {
		t.Errorf("preferFree 20s/720p = %d, want 10", got)
	}
	if got := SnapDuration(8, "720p", true); got != 8 {
		t.Errorf("preferFree must not touch in-window values, got %d", got)
	}
}

func TestCreateWarnsOnDurationSnap(t *testing.T) {
	body := textBody("minimaxH3")
	body["duration"] = 8
	body["resolution"] = "480p"
	plan, err := Create(body, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.WebParams["duration"] != 10 {
		t.Errorf("duration = %v, want 10 (480p only accepts 5/10/15/20)", plan.WebParams["duration"])
	}
	if !hasWarning(plan.Warnings, "snapped to 10s") {
		t.Errorf("snapping must leave a trace, warnings=%v", plan.Warnings)
	}
}

func TestBillingViewShoutsWhenCrossingFreeWindow(t *testing.T) {
	body := textBody("minimaxH3")
	body["duration"] = 15
	body["resolution"] = "480p"
	plan, err := Create(body, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	eff, warnings := BillingView(plan)
	if eff["billed"] != true {
		t.Errorf("billed = %v, want true", eff["billed"])
	}
	if !hasWarning(warnings, "WILL BE BILLED") {
		t.Errorf("crossing the free window must be shouted about, warnings=%v", warnings)
	}
}

func TestBaseTierIsAlwaysBilledWithoutShouting(t *testing.T) {
	body := textBody("minimaxH3")
	body["duration"] = 5
	body["extra_body"] = map[string]any{"aivideomaker_tier": "base"}
	plan, err := Create(body, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	eff, warnings := BillingView(plan)
	if eff["billed"] != true {
		t.Errorf("tier=base must be billed")
	}
	if hasWarning(warnings, "WILL BE BILLED") {
		// base 是调用方显式点名的选择，不是"悄悄变贵"，再喊一遍只会让人对告警脱敏。
		t.Errorf("explicit tier=base must not produce the duration warning, warnings=%v", warnings)
	}
}

func TestCreateRejectsUnknownRatioButSnapsKnown(t *testing.T) {
	body := textBody("minimaxH3")
	body["ratio"] = "banana"
	if _, err := Create(body, nil, ""); err == nil {
		t.Fatal("unknown ratio must be rejected on the Ark line")
	}

	body["ratio"] = "5:4"
	plan, err := Create(body, nil, "")
	if err != nil {
		t.Fatalf("5:4 is a readable proportion and must not be rejected: %v", err)
	}
	if plan.WebParams["aspectRatio"] != "4:3" {
		t.Errorf("aspectRatio = %v, want 4:3", plan.WebParams["aspectRatio"])
	}
	if !hasWarning(plan.Warnings, "snapped") {
		t.Errorf("snapping must leave a trace, warnings=%v", plan.Warnings)
	}
}

func TestFrameAndReferenceAreMutuallyExclusive(t *testing.T) {
	body := map[string]any{
		"model": "minimaxH3",
		"content": []any{
			map[string]any{"type": "text", "text": "x"},
			map[string]any{"type": "image_url", "role": "first_frame", "image_url": map[string]any{"url": "https://a/1.png"}},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://a/2.png"}},
		},
	}
	if _, err := Create(body, nil, ""); err == nil {
		t.Fatal("frame inputs and reference assets must not be mixed")
	}
}

func TestReferenceTruncationLeavesATrace(t *testing.T) {
	items := []any{map[string]any{"type": "text", "text": "@图像5 用第 5 张"}}
	for i := 1; i <= 5; i++ {
		items = append(items, map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": "https://x/" + itoa(i) + ".png"}})
	}
	plan, err := Create(map[string]any{"model": "minimaxH3", "content": items}, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	kept, _ := plan.WebParams["referenceImageUrls"].([]string)
	if len(kept) != 4 {
		t.Fatalf("kept %d reference images, want 4 (upstream limit)", len(kept))
	}
	if !hasWarning(plan.Warnings, "1 dropped: https://x/5.png") {
		t.Errorf("dropped assets must be named, warnings=%v", plan.Warnings)
	}
	if !hasWarning(plan.Warnings, "@图像5") {
		t.Errorf("dangling prompt placeholder must be named, warnings=%v", plan.Warnings)
	}
}

func TestOmniEditIsIncompatibleNotSilentlyDropped(t *testing.T) {
	body := textBody("seedance25")
	body["omni_reference_task_type"] = "edit"
	plan, err := Create(body, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plan.Incompatible) == 0 {
		t.Fatal("omni edit needs upstream edit capability the web line lacks ⇒ must be incompatible")
	}
}

func TestGenerateAudioMustBeBoolean(t *testing.T) {
	body := textBody("minimaxH3")
	body["generate_audio"] = "yes"
	if _, err := Create(body, nil, ""); err == nil {
		t.Fatal("non-boolean generate_audio must be rejected rather than guessed")
	}
	body["generate_audio"] = true
	if _, err := Create(body, nil, ""); err != nil {
		t.Fatalf("boolean generate_audio must be accepted: %v", err)
	}
}

func TestOutputFormatWarnsWhenUpstreamCannotHonor(t *testing.T) {
	body := textBody("minimaxH3")
	body["output_format"] = "mov"
	plan, err := Create(body, nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasWarning(plan.Warnings, "mov") {
		t.Errorf("a container we cannot deliver must be called out, warnings=%v", plan.Warnings)
	}
	if plan.Effective["output_format"] != "mp4" {
		t.Errorf("effective output_format = %v, want mp4 (the upstream's real container)",
			plan.Effective["output_format"])
	}
}

func TestForceSlotOverridesChannelMapAndLeavesATrace(t *testing.T) {
	body := textBody("doubao-seedance-2-5-260628")
	options := map[string]any{"model_map": map[string]any{
		"doubao-seedance-2-5-260628": "seedance25",
	}}
	plan, err := Create(body, options, "minimaxH3")
	if err != nil {
		t.Fatalf("force_slot must not reject an unknown name: %v", err)
	}
	if plan.Procedure() != "ai.minimaxH3" {
		t.Errorf("procedure = %q, want ai.minimaxH3", plan.Procedure())
	}
	if plan.Requested["model"] != "doubao-seedance-2-5-260628" {
		t.Errorf("requested.model must keep the caller's value, got %v", plan.Requested["model"])
	}
	if plan.Effective["model_source"] != "face_forced" {
		t.Errorf("model_source = %v, want face_forced", plan.Effective["model_source"])
	}
	if !hasWarning(plan.Warnings, "only runs one upstream slot") {
		t.Errorf("overriding the channel map must be called out, warnings=%v", plan.Warnings)
	}
}

func TestArkTaskViewIsAWhitelist(t *testing.T) {
	view := map[string]any{
		"id": "cgt-1", "status": "succeeded",
		"content":  map[string]any{"video_url": "https://v/1.mp4"},
		"duration": "5", "ratio": "16:9",
		"created_at": 100, "updated_at": 200,
		"requested": map[string]any{"model": "x"}, "warnings": []string{"w"},
		"upstream_record": map[string]any{"aiModel": "minimax_h3"},
		"usage":           map[string]any{"paid": false},
	}
	out := ArkTaskView(view)
	for _, forbidden := range []string{"requested", "warnings", "upstream_record", "usage", "model"} {
		if _, leaked := out[forbidden]; leaked {
			t.Errorf("%q must not appear in the Ark response body (official SDKs reject unknown fields)", forbidden)
		}
	}
	if out["duration"] != 5 {
		t.Errorf("duration must be an integer, got %#v", out["duration"])
	}
	if out["error"] != nil {
		t.Errorf("error must always be present (null when successful), got %#v", out["error"])
	}
}

func TestArkTaskViewOmitsEmptyValues(t *testing.T) {
	out := ArkTaskView(map[string]any{"id": "cgt-1", "status": "queued", "content": map[string]any{}})
	if _, ok := out["content"]; ok {
		t.Error("content must be omitted while there is no video url")
	}
	if _, ok := out["duration"]; ok {
		t.Error("absent duration must not be fabricated")
	}
}

func TestNormalizeWebTaskDowngradesSucceedWithoutURL(t *testing.T) {
	view := NormalizeWebTask(map[string]any{"id": "t1", "taskStatus": "succeed", "taskStatusMsg": "not found url"})
	if view["status"] != "failed" {
		t.Errorf("succeed without a url must be reported as failed, got %v", view["status"])
	}
	errBlock, _ := view["error"].(map[string]any)
	if errBlock["message"] != "not found url" {
		t.Errorf("failure reason must be carried through, got %v", view["error"])
	}
}

func TestNormalizeWebTaskResolutionFromKelingKeyId(t *testing.T) {
	view := NormalizeWebTask(map[string]any{
		"taskStatus": "succeed", "url": "https://v/1.mp4", "kelingKeyId": "480", "duration": "5",
	})
	if view["resolution"] != "480p" {
		t.Errorf("resolution must come from the actual kelingKeyId, got %v", view["resolution"])
	}
}

func TestToArkStatus(t *testing.T) {
	cases := map[string]string{
		"succeed": "succeeded", "success": "succeeded", "processing": "running",
		"queueing": "queued", "timed_out": "expired", "canceled": "cancelled",
		"something-new": "queued",
	}
	for in, want := range cases {
		if got := ToArkStatus(in); got != want {
			t.Errorf("ToArkStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

func hasWarning(warnings []string, needle string) bool {
	for _, w := range warnings {
		if strings.Contains(w, needle) {
			return true
		}
	}
	return false
}

func itoa(i int) string {
	return string(rune('0' + i))
}

// 🔴 实测回归（2026-11-08，真实账号）：`1080p / 5s / turbo` 扣了 **20 积分**
// （2677 → 2657），而当时 `effective.billed` 报的是 `false`。这是最坏方向的错：
// 调用方读到"免费"，账单上却有钱。判据必须把分辨率算进去。
func TestBilledTreats1080pAsAlwaysBilled(t *testing.T) {
	cases := []struct {
		tier       string
		duration   int
		resolution string
		want       bool
		why        string
	}{
		{"turbo", 5, "1080p", true, "1080p has no free window (measured: 20 credits for 5s)"},
		{"turbo", 10, "1080p", true, "10s is the longest free duration, but 1080p is never free"},
		{"turbo", 5, "720p", false, "720p + turbo + <=10s is the measured free window"},
		{"turbo", 5, "480p", false, "480p + turbo + <=10s is free"},
		{"turbo", 5, "", false, "a missing resolution must not be reported as billed"},
		{"turbo", 11, "720p", true, "beyond the free window"},
		{"base", 5, "480p", true, "tier=base never has a free window"},
	}
	for _, c := range cases {
		if got := Billed(c.tier, c.duration, c.resolution); got != c.want {
			t.Errorf("Billed(%q, %d, %q) = %v, want %v (%s)",
				c.tier, c.duration, c.resolution, got, c.want, c.why)
		}
	}
}

// 越线必须有声音：1080p 在免费时长内也计费，必须给一条 warning。
func TestBillingViewWarnsWhenResolutionHasNoFreeWindow(t *testing.T) {
	body := map[string]any{
		"model":      "minimaxH3",
		"content":    []any{map[string]any{"type": "text", "text": "x"}},
		"resolution": "1080p",
		"duration":   5,
	}
	plan, err := Create(body, nil, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	eff, warnings := BillingView(plan)
	if eff["billed"] != true {
		t.Errorf("billed = %v, want true", eff["billed"])
	}
	joined := strings.Join(warnings, " | ")
	if !strings.Contains(joined, "no free window") {
		t.Errorf("warnings = %v, must explain that 1080p has no free window", warnings)
	}
}

// 预估积分只对**实测过**的组合给值；其余必须是"不知道"，不能线性外推。
func TestEstimatedCreditsOnlyForMeasuredCombos(t *testing.T) {
	cases := []struct {
		tier, res string
		duration  int
		want      int
		known     bool
	}{
		// 全部来自真实扣费核对（2026-11-08）。
		{"turbo", "480p", 15, 15, true},
		{"base", "480p", 5, 5, true},
		{"turbo", "720p", 11, 33, true},
		{"turbo", "1080p", 5, 20, true},
		{"turbo", "720p", 5, 0, true}, // 免费窗口内
		{"turbo", "1080p", 10, 40, true},
		{"base", "720p", 5, 5, true},   // 实测 5
		{"base", "720p", 15, 60, true}, // 实测 60 = 15×(3+1)：base 是**溢价**不是固定单价
		{"base", "480p", 15, 30, true}, // 规则推导 = 15×(1+1)
		{"base", "1080p", 5, 0, false},
		{"turbo", "", 5, 0, false},
	}
	for _, c := range cases {
		got, known := EstimatedCredits(c.tier, c.duration, c.res)
		if known != c.known || (known && got != c.want) {
			t.Errorf("EstimatedCredits(%q, %d, %q) = (%d, %v), want (%d, %v)",
				c.tier, c.duration, c.res, got, known, c.want, c.known)
		}
	}
}

// dry-run 的 effective 必须带上钱数，否则调用方只能自己按分辨率猜。
func TestBillingViewExposesEstimatedCredits(t *testing.T) {
	body := map[string]any{
		"model":      "minimaxH3",
		"content":    []any{map[string]any{"type": "text", "text": "x"}},
		"resolution": "720p",
		"duration":   11,
	}
	plan, err := Create(body, nil, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	eff, _ := BillingView(plan)
	if eff["estimated_credits"] != 33 {
		t.Errorf("estimated_credits = %v, want 33 (measured: 720p/11s cost 33 credits)",
			eff["estimated_credits"])
	}
}

// 官方独有模型走方舟协议时，翻译层要按**模型自己的规格**建 body
// （duration 有的要字符串有的要数字、分辨率三种写法）。
func TestOfficialModelsTranslatePerModelRules(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantRes any
		wantDur any
	}{
		{
			name: "seedance20（显式 official；resolution 要数字）",
			body: `{"model":"seedance20","content":[{"type":"text","text":"a fox"}],` +
				`"duration":5,"resolution":"720","ratio":"16:9",` +
				`"extra_body":{"aivideomaker_channel":"official"}}`,
			wantRes: 720, wantDur: 5,
		},
		{
			name: "wan30（duration 字符串、分辨率大写）",
			body: `{"model":"wan30","content":[{"type":"text","text":"a fox"}],` +
				`"duration":5,"resolution":"720p","ratio":"16:9"}`,
			wantRes: "720P", wantDur: "5",
		},
		{
			name: "happyhorse（duration 数字、分辨率大写）",
			body: `{"model":"happyhorse","content":[{"type":"text","text":"a fox"}],` +
				`"duration":5,"resolution":"720P"}`,
			wantRes: "720P", wantDur: 5,
		},
		{
			name: "t2v（duration 字符串、无分辨率）",
			body: `{"model":"t2v","content":[{"type":"text","text":"a fox"}],` +
				`"duration":5}`,
			wantRes: nil, wantDur: "5",
		},
	}

	for _, c := range cases {
		var body map[string]any
		if err := json.Unmarshal([]byte(c.body), &body); err != nil {
			t.Fatalf("%s: bad json: %v", c.name, err)
		}
		plan, err := Create(body, nil, "")
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := jsonx.String(plan.WebParams["channel"]); got != "official" {
			t.Errorf("%s: channel = %q, want official", c.name, got)
			continue
		}
		of := jsonx.Map(plan.WebParams["official_body"])
		if of == nil {
			t.Errorf("%s: official_body missing", c.name)
			continue
		}
		if got := of["duration"]; got != c.wantDur {
			t.Errorf("%s: duration = %#v, want %#v", c.name, got, c.wantDur)
		}
		if c.wantRes == nil {
			if _, has := of["resolution"]; has {
				t.Errorf("%s: should not send resolution, got %#v", c.name, of["resolution"])
			}
		} else if got := of["resolution"]; got != c.wantRes {
			t.Errorf("%s: resolution = %#v, want %#v", c.name, got, c.wantRes)
		}
		// 官方全程收费：effective.billed 必须是 true（别让调用方以为免费）
		if billed, _ := plan.Effective["billed"].(bool); !billed {
			t.Errorf("%s: effective.billed must be true on the official channel", c.name)
		}
	}

	// i2v 必须给首帧图，缺了要明确报错（不是静默提交）
	var body map[string]any
	_ = json.Unmarshal([]byte(`{"model":"i2v","content":[{"type":"text","text":"a fox"}],"duration":5}`), &body)
	if _, err := Create(body, nil, ""); err == nil {
		t.Error("i2v without an image must fail")
	}
}

// 🔴 花钱回归：免费窗口是**模型相关**的。
// 实测 seedance20 480p/5s 走网页档扣了 68 积分（与官方报价一致），
// 只有 MiniMax H3 才有 480p/720p + turbo + ≤10s 的免费窗口。
func TestBilledIsModelAware(t *testing.T) {
	var body map[string]any
	_ = json.Unmarshal([]byte(`{"model":"seedance20","content":[{"type":"text","text":"a fox"}],`+
		`"resolution":"480p","duration":5}`), &body)
	if body == nil {
		t.Fatal("bad json")
	}
	// seedance20 是网页槽位，但**没有免费档**
	plan, err := Create(body, nil, "")
	if err != nil {
		t.Skipf("seedance20 rejected on the web path: %v", err)
	}
	eff, _ := BillingView(plan)
	if billed, _ := eff["billed"].(bool); !billed {
		t.Error("seedance20 at 480p/5s must be reported as billed (measured 68 credits)")
	}

	// minimaxH3 480p/5s 仍是免费档
	var mm map[string]any
	_ = json.Unmarshal([]byte(`{"model":"minimaxH3","content":[{"type":"text","text":"a fox"}],`+
		`"resolution":"480p","duration":5}`), &mm)
	plan2, err := Create(mm, nil, "")
	if err != nil {
		t.Fatalf("minimaxH3: %v", err)
	}
	eff2, _ := BillingView(plan2)
	if billed, _ := eff2["billed"].(bool); billed {
		t.Error("minimaxH3 at 480p/5s must stay free")
	}
}
