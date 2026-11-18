package openaivideos

import (
	"strings"
	"testing"
)

func TestPinnedDurationPerResolution(t *testing.T) {
	body, notes, err := BodyFromOpenAI(map[string]any{
		"model": "doubao-seedance-1-0-pro_480p", "prompt": "p", "seconds": 5,
	}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body["duration"] != 10 {
		t.Errorf("480p must be pinned to 10s, got %v", body["duration"])
	}
	if body["resolution"] != "480p" {
		t.Errorf("resolution = %v", body["resolution"])
	}
	if !has(notes, "overridden to 10s") {
		t.Errorf("overriding seconds must be reported, notes=%v", notes)
	}
}

func TestDurationPinnedEvenWithoutSuffix(t *testing.T) {
	// 缺后缀时下游按 DefaultResolution 兜底 —— 查表必须用**最终生效**的分辨率，
	// 否则同一个 720p 请求会因"写没写后缀"分成"被钉住"与"没钉住"两种结果。
	body, _, err := BodyFromOpenAI(map[string]any{"model": "x", "prompt": "p", "seconds": 9}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body["duration"] != 8 {
		t.Errorf("720p (default) must be pinned to 8s, got %v", body["duration"])
	}
}

func Test1080pIsDowngradedForTheFreeTier(t *testing.T) {
	body, notes, err := BodyFromOpenAI(map[string]any{
		"model": "doubao-seedance-1-0-pro_1080p", "prompt": "p",
	}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body["resolution"] != "720p" {
		t.Errorf("1080p must be downgraded to 720p (no measured free window), got %v", body["resolution"])
	}
	if body["duration"] != 8 {
		t.Errorf("after downgrade the duration must be 8s, got %v", body["duration"])
	}
	if !has(notes, "downgraded") {
		t.Errorf("downgrading must be reported, notes=%v", notes)
	}
}

func TestSizeAliasesAndFallback(t *testing.T) {
	body, notes, err := BodyFromOpenAI(map[string]any{
		"model": "x", "prompt": "p", "size": "keep_ratio",
	}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body["ratio"] != "adaptive" {
		t.Errorf("keep_ratio must map to adaptive, got %v", body["ratio"])
	}
	if !has(notes, "keep_ratio") {
		t.Errorf("the alias must be reported, notes=%v", notes)
	}

	body, notes, err = BodyFromOpenAI(map[string]any{"model": "x", "prompt": "p", "size": "banana"}, "")
	if err != nil {
		t.Fatalf("this face must not 400 on an unrecognized size: %v", err)
	}
	if body["ratio"] != "16:9" {
		t.Errorf("unrecognized size must fall back to 16:9, got %v", body["ratio"])
	}
	if !has(notes, "fell back") {
		t.Errorf("falling back must be reported, notes=%v", notes)
	}
}

func TestFrameArrayIsRejectedNotTruncated(t *testing.T) {
	_, _, err := BodyFromOpenAI(map[string]any{
		"model": "x", "prompt": "p",
		"first_frame_image": []any{"https://a/1.png", "https://a/2.png"},
	}, "")
	if err == nil {
		t.Fatal("an array in a frame field must be rejected (taking the first would silently drop assets)")
	}
}

func TestInputReferenceAsJSONStringIsRescued(t *testing.T) {
	body, notes, err := BodyFromOpenAI(map[string]any{
		"model": "x", "prompt": "p",
		"input_reference": `["https://a/1.png","https://a/2.png"]`,
	}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	content := body["content"].([]any)
	// text + 2 images
	if len(content) != 3 {
		t.Fatalf("content has %d items, want 3 (text + 2 rescued refs)", len(content))
	}
	if !has(notes, "parsed into 2 entries") {
		t.Errorf("rescuing a JSON-string array must be reported, notes=%v", notes)
	}
}

func TestTaskViewHasExactlyTheContractFields(t *testing.T) {
	view := TaskView(map[string]any{
		"id": "cgt-1", "status": "succeeded",
		"content":    map[string]any{"video_url": "https://v/1.mp4"},
		"created_at": 123, "warnings": []string{"x"}, "upstream_model": "minimax_h3",
	}, 999)
	want := []string{"id", "object", "status", "progress", "video_url", "created_at"}
	if len(view) != len(want) {
		t.Fatalf("view has %d fields, want exactly %d: %#v", len(view), len(want), view)
	}
	for _, k := range want {
		if _, ok := view[k]; !ok {
			t.Errorf("missing contract field %q", k)
		}
	}
	if view["status"] != "completed" || view["progress"] != 100 || view["video_url"] != "https://v/1.mp4" {
		t.Errorf("completed mapping wrong: %#v", view)
	}
}

func TestTaskViewQueuedHasNoURLAndZeroProgress(t *testing.T) {
	view := TaskView(map[string]any{"id": "cgt-1", "status": "queued"}, 999)
	if view["video_url"] != nil || view["progress"] != 0 {
		t.Errorf("queued mapping wrong: %#v", view)
	}
	if view["created_at"] != 999 {
		t.Errorf("created_at must fall back to the local record, got %v", view["created_at"])
	}
}

func has(notes []string, needle string) bool {
	for _, n := range notes {
		if strings.Contains(n, needle) {
			return true
		}
	}
	return false
}
