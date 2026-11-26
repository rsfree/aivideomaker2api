package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/rsfree/aivideomaker2api/internal/store"
	"github.com/rsfree/aivideomaker2api/internal/translate"
)

// free-only 的核心契约：危险参数**静默丢弃**，请求照常成功，真正发到上游的是免费档。
func TestFreeOnlyCoercesUpstreamPayload(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.FreeOnly = true }, "")

	code, created := h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks",
		`{"model":"minimaxH3","content":[{"type":"text","text":"a cat"}],
		  "resolution":"1080p","duration":15,
		  "extra_body":{"aivideomaker_channel":"official","aivideomaker_tier":"base"}}`, nil)
	if code != http.StatusOK {
		t.Fatalf("create = %d (%v) —— free-only 必须照常受理，只是降级到免费档", code, created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("missing id")
	}

	payload, _ := h.site.lastCreate.Load().(string)
	if payload == "" {
		t.Fatal("upstream create was never called")
	}
	// 1080p→720p；15s 在 720p 上吸附到上界 8s。
	for _, want := range []string{`"tier":"turbo"`, `"resolution":"720p"`, `"duration":8`} {
		if !strings.Contains(payload, want) {
			t.Errorf("upstream payload missing %s — 危险参数没有在上游侧消失: %s", want, payload)
		}
	}
	if strings.Contains(payload, "official") {
		t.Errorf("upstream payload still carries the official channel: %s", payload)
	}

	entry, ok := h.store.Get(id)
	if !ok {
		t.Fatal("task not persisted")
	}
	if got := entry.Effective["billed"]; got != false {
		t.Errorf("effective.billed = %v, want false (这条线不该产生账单)", got)
	}
	if got := entry.Effective["resolution"]; got != "720p" {
		t.Errorf("effective.resolution = %v, want 720p", got)
	}
	if got := entry.Effective["duration"]; got != 8 {
		t.Errorf("effective.duration = %v, want 8 (15s snaps up to the 720p ceiling)", got)
	}
	if !strings.Contains(strings.Join(entry.Warnings, "\n"), "free-only policy: adjusted") {
		t.Errorf("warnings must keep the trace of dropped parameters: %v", entry.Warnings)
	}
	if got := entry.Requested["resolution"]; got != "1080p" {
		t.Errorf("requested.resolution = %v, want the caller's original 1080p", got)
	}
}

// 官方创建路由整条禁用：它存在的意义就是花钱。
func TestFreeOnlyBlocksOfficialRoute(t *testing.T) {
	h := newHarness(t, func(c *Config) {
		c.FreeOnly = true
		c.Official = OfficialConfig{APIKey: "ak_test", BaseURL: "http://127.0.0.1:1", Timeout: 1}
	}, "")

	code, body := h.do(t, http.MethodPost, "/official/api/v3/contents/generations/tasks",
		`{"model":"minimaxH3","content":[{"type":"text","text":"a cat"}],"resolution":"720p","duration":5}`,
		nil)
	if code != http.StatusBadRequest {
		t.Fatalf("official create = %d (%v), want 400", code, body)
	}
	if h.site.createHits != 0 {
		t.Errorf("free-only still hit the upstream %d times via /official/", h.site.createHits)
	}
}

// 官方独有模型没有免费档：明确拒绝，而不是"丢参数后发一个奇怪的请求"。
func TestFreeOnlyRejectsBilledModels(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.FreeOnly = true }, "")

	code, body := h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks",
		`{"model":"wan30","content":[{"type":"text","text":"a cat"}]}`, nil)
	if code == http.StatusOK {
		t.Fatalf("wan30 accepted under free-only: %v", body)
	}
	msg, _ := body["error"].(map[string]any)
	if msg == nil || !strings.Contains(strings.ToLower(str(msg["message"])), "free-only") {
		t.Errorf("error message must name the free-only policy: %v", body)
	}

	// 模型列表同样不列：列出来等于暗示"这里能跑"。
	code, listed := h.do(t, http.MethodGet, "/v1/models", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d", code)
	}
	for _, item := range toList(listed["data"]) {
		m, _ := item.(map[string]any)
		id := str(m["id"])
		if id == "wan30" || id == "seedance20" {
			t.Errorf("free-only still advertises billed model %q", id)
		}
		if id == "minimaxH3" {
			extra, _ := m["aivideomaker"].(map[string]any)
			if extra["channel"] != "web" || extra["official_ready"] != false {
				t.Errorf("minimaxH3 still advertises the official line: %v", extra)
			}
		}
	}
}

// 升级前的在途任务（WebParams 里已有 official）不能借着重试继续花钱。
func TestFreeOnlyRefusesOfficialPlanAtSubmit(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.FreeOnly = true }, "")

	entry := &store.Entry{ID: "cgt-legacy", Status: "queued", Model: "minimaxH3"}
	if err := h.store.Put(entry); err != nil {
		t.Fatal(err)
	}
	h.server.submit(entry, &translate.Plan{WebParams: map[string]any{"channel": "official"}})

	got, ok := h.store.Get("cgt-legacy")
	if !ok {
		t.Fatal("task vanished")
	}
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed (official submission must be refused under free-only)", got.Status)
	}
	if h.site.createHits != 0 {
		t.Errorf("upstream was hit %d times — free-only leaked a billed submission", h.site.createHits)
	}
}

// 计费红线探针：预测免费却被扣费 ⇒ 打 marker 日志 + 计数。
func TestFreeLineBilledMarker(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.FreeOnly = true }, "")
	var buf bytes.Buffer
	h.server.log = slog.New(slog.NewTextHandler(&buf, nil))

	entry := &store.Entry{
		ID: "t-1", TaskID: "up-1", Account: "acc1", Model: "minimaxH3",
		Effective: map[string]any{"billed": false, "resolution": "720p", "duration": 5},
		WebParams: map[string]any{"tier": "turbo"},
	}

	// 免费窗口内却被扣费 —— 正是"该账号/设备免费额度用尽后站点改扣费"的形态。
	h.server.noteFreeLineBilled(entry, map[string]any{
		"usage": map[string]any{"paid": true, "credits": 0},
	})
	if got := h.server.freeLineBilled.Load(); got != 1 {
		t.Fatalf("freeLineBilled = %d, want 1", got)
	}
	logged := buf.String()
	for _, want := range []string{"free_line_billed", "720p", "acc1"} {
		if !strings.Contains(logged, want) {
			t.Errorf("log line missing %q: %s", want, logged)
		}
	}

	// 意料之中的扣费（effective.billed=true）不该再刷标记。
	paidPlan := &store.Entry{
		ID: "t-2", Effective: map[string]any{"billed": true}, WebParams: map[string]any{},
	}
	h.server.noteFreeLineBilled(paidPlan, map[string]any{
		"usage": map[string]any{"paid": true, "credits": 0},
	})
	if got := h.server.freeLineBilled.Load(); got != 1 {
		t.Errorf("freeLineBilled = %d after a predicted-billed task, want 1", got)
	}

	// 未扣费不计数。
	h.server.noteFreeLineBilled(entry, map[string]any{
		"usage": map[string]any{"paid": false, "credits": 1},
	})
	if got := h.server.freeLineBilled.Load(); got != 1 {
		t.Errorf("freeLineBilled = %d after a free task, want 1", got)
	}
}

func str(v any) string { s, _ := v.(string); return s }

func toList(v any) []any { l, _ := v.([]any); return l }
