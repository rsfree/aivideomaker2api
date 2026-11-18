package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestMiniMaxEndToEnd(t *testing.T) {
	h := newHarness(t, nil, "")

	// create —— MiniMax 的风格是 HTTP 恒 200，错误装在 base_resp 里。
	code, created := h.do(t, http.MethodPost, "/v1/video_generation",
		`{"model":"MiniMax-Hailuo-02","prompt":"a red balloon","duration":5,`+
			`"resolution":"768P","aspect_ratio":"16:9"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("create status = %d (%v)", code, created)
	}
	resp, _ := created["base_resp"].(map[string]any)
	if resp["status_code"] != float64(0) {
		t.Fatalf("base_resp = %v, want status_code 0", resp)
	}
	taskID, _ := created["task_id"].(string)
	if taskID == "" {
		t.Fatal("missing task_id")
	}

	// query
	code, queried := h.do(t, http.MethodGet, "/v1/query/video_generation?task_id="+taskID, "", nil)
	if code != http.StatusOK {
		t.Fatalf("query status = %d", code)
	}
	if queried["status"] != "Success" {
		t.Fatalf("status = %v, want Success (%v)", queried["status"], queried)
	}
	if queried["file_id"] != taskID {
		t.Errorf("file_id = %v, want the task id", queried["file_id"])
	}
	// 480p / 16:9 是**实测**尺寸（864x480），不是按比例算的 853x480。
	if queried["video_width"] != float64(864) || queried["video_height"] != float64(480) {
		t.Errorf("video size = %vx%v, want 864x480 (measured pixels take precedence)",
			queried["video_width"], queried["video_height"])
	}

	// retrieve —— 拿到可下载地址
	code, file := h.do(t, http.MethodGet, "/v1/files/retrieve?file_id="+taskID, "", nil)
	if code != http.StatusOK {
		t.Fatalf("retrieve status = %d", code)
	}
	block, _ := file["file"].(map[string]any)
	if block == nil {
		t.Fatalf("file = %v, want an object", file["file"])
	}
	if url, _ := block["download_url"].(string); !strings.Contains(url, "img2video.ai") {
		t.Errorf("download_url = %v (media proxy disabled ⇒ upstream link passes through)", block["download_url"])
	}
	if block["purpose"] != "video_generation" {
		t.Errorf("purpose = %v", block["purpose"])
	}
}

func TestMiniMaxErrorsUseBaseRespNotHTTPStatus(t *testing.T) {
	h := newHarness(t, nil, "")
	code, body := h.do(t, http.MethodPost, "/v1/video_generation", `{"prompt":""}`, nil)
	if code != http.StatusOK {
		t.Fatalf("MiniMax must answer 200 even for bad input, got %d", code)
	}
	resp, _ := body["base_resp"].(map[string]any)
	if resp["status_code"] != float64(2013) {
		t.Errorf("base_resp = %v, want 2013 (invalid parameter)", resp)
	}
	if body["task_id"] != "" {
		t.Errorf("task_id = %v, want an empty string", body["task_id"])
	}
}

func TestMiniMaxQueryUnknownTask(t *testing.T) {
	h := newHarness(t, nil, "")
	code, body := h.do(t, http.MethodGet, "/v1/query/video_generation?task_id=cgt-does-not-exist", "", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["status"] != "Fail" {
		t.Errorf("status = %v, want Fail", body["status"])
	}
	resp, _ := body["base_resp"].(map[string]any)
	if resp["status_code"] != float64(2013) {
		t.Errorf("base_resp = %v, want 2013", resp)
	}
}

func TestMiniMaxContentRejectionSurfacesOnQuery(t *testing.T) {
	h := newHarness(t, nil, "")
	h.site.createFail = func(w http.ResponseWriter) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`[{"error":{"json":{"message":"ai.minimaxH3: Prompt or Image Sensitive",` +
			`"data":{"code":"INTERNAL_SERVER_ERROR","httpStatus":500}}}}]`))
	}
	_, created := h.do(t, http.MethodPost, "/v1/video_generation", `{"prompt":"x"}`, nil)
	taskID := created["task_id"].(string)
	_, queried := h.do(t, http.MethodGet, "/v1/query/video_generation?task_id="+taskID, "", nil)
	if queried["status"] != "Fail" {
		t.Fatalf("status = %v, want Fail", queried["status"])
	}
	msg, _ := queried["error_message"].(string)
	if !strings.Contains(msg, "Prompt or Image Sensitive") {
		t.Errorf("error_message = %q, must carry the upstream's own wording", msg)
	}
	if strings.Contains(msg, "ai.minimaxH3") {
		t.Errorf("error_message leaks the internal procedure name: %q", msg)
	}
}

func TestMiniMaxSizeAndSubjectReference(t *testing.T) {
	h := newHarness(t, nil, "")
	_, out := h.do(t, http.MethodPost, "/v1/video_generation",
		`{"prompt":"a cat","size":"1920x1080",`+
			`"subject_reference":[{"type":"character","image":["https://x/1.png","https://x/2.png"]}]}`,
		map[string]string{"X-Avm-Dry-Run": "1"})
	if out["dry_run"] != true {
		t.Fatalf("want dry-run output, got %v", out)
	}
	payload, _ := out["upstream_payload"].(map[string]any)
	if payload["resolution"] != "1080p" {
		t.Errorf("resolution = %v, want 1080p (derived from size height)", payload["resolution"])
	}
	if payload["aspectRatio"] != "16:9" {
		t.Errorf("aspectRatio = %v, want 16:9", payload["aspectRatio"])
	}
	// 经过一次 JSON 往返，数组是 []any（不是 []string）—— 断言前先转。
	refs, _ := payload["referenceImageUrls"].([]any)
	if len(refs) != 2 {
		t.Errorf("subject_reference produced %d reference images, want 2 (%v)", len(refs), payload["referenceImageUrls"])
	}
}

func TestMiniMaxMixedFrameAndReferenceIsRejected(t *testing.T) {
	h := newHarness(t, nil, "")
	code, body := h.do(t, http.MethodPost, "/v1/video_generation",
		`{"prompt":"x","first_frame_image":"https://x/1.png","reference_image_urls":["https://x/2.png"]}`, nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	resp, _ := body["base_resp"].(map[string]any)
	if resp["status_code"] != float64(2013) {
		t.Errorf("base_resp = %v, want 2013 (frame and reference are mutually exclusive)", resp)
	}
}

func TestMiniMaxUnknownResolutionWarnsInsteadOfSilentlyDowngrading(t *testing.T) {
	h := newHarness(t, nil, "")
	_, out := h.do(t, http.MethodPost, "/v1/video_generation",
		`{"prompt":"x","resolution":"512P"}`, map[string]string{"X-Avm-Dry-Run": "1"})
	warnings, _ := out["warnings"].([]any)
	found := false
	for _, w := range warnings {
		if strings.Contains(w.(string), "not recognized") {
			found = true
		}
	}
	if !found {
		t.Errorf("an unrecognized resolution must be reported, warnings=%v", warnings)
	}
	payload, _ := out["upstream_payload"].(map[string]any)
	if payload["resolution"] != "720p" {
		t.Errorf("resolution = %v, want the default 720p", payload["resolution"])
	}
}
