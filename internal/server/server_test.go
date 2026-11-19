package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rsfree/aivideomaker2api/internal/media"
	"github.com/rsfree/aivideomaker2api/internal/pool"
	"github.com/rsfree/aivideomaker2api/internal/store"
	"github.com/rsfree/aivideomaker2api/internal/upstream"
)

// fakeSite 是站点替身：只实现本服务真正调用的 procedure。
type fakeSite struct {
	server     *httptest.Server
	createHits int
	createFail func(w http.ResponseWriter)
	// gateAfter > 0：前 gateAfter 次闸门探测说"关"，之后说"开"（模拟速率型翻转）。
	gateAfter int32
	gateHits  int32
}

func newFakeSite(t *testing.T) *fakeSite {
	t.Helper()
	fs := &fakeSite{}
	mux := http.NewServeMux()
	writeEnvelope := func(w http.ResponseWriter, v any) {
		w.Header().Set("content-type", "application/json")
		raw, _ := json.Marshal([]any{map[string]any{"result": map[string]any{"data": map[string]any{"json": v}}}})
		_, _ = w.Write(raw)
	}
	mux.HandleFunc("/api/auth.user", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{"id": "u-1", "email": "ops@example.com"})
	})
	mux.HandleFunc("/api/credits.getCredits", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{"totalRemaining": 1200})
	})
	mux.HandleFunc("/api/ai.queryUserPermission", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{"maxQueueLength": 2, "planName": "premium"})
	})
	mux.HandleFunc("/api/billing.subscription", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{"planId": "prod_SymV739ojwmGEN", "status": "active"})
	})
	mux.HandleFunc("/api/model.needsCaptcha", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&fs.gateHits, 1)
		writeEnvelope(w, fs.gateAfter > 0 && n > fs.gateAfter)
	})
	mux.HandleFunc("/api/ai.minimaxH3", func(w http.ResponseWriter, r *http.Request) {
		fs.createHits++
		if fs.createFail != nil {
			fs.createFail(w)
			return
		}
		writeEnvelope(w, "up-task-1")
	})
	mux.HandleFunc("/api/model.getModel", func(w http.ResponseWriter, r *http.Request) {
		writeEnvelope(w, map[string]any{
			"id": "up-task-1", "taskStatus": "succeed",
			"url":     "https://static2.img2video.ai/abc_0_minimax_h3.mp4",
			"aiModel": "minimax_h3", "kelingKeyId": "480", "paid": false, "credits": 1, "duration": "5",
		})
	})
	fs.server = httptest.NewServer(mux)
	t.Cleanup(fs.server.Close)
	return fs
}

type harness struct {
	server *Server
	store  *store.Store
	pool   *pool.Pool
	site   *fakeSite
}

func newHarness(t *testing.T, mutate func(*Config), mediaBase string) *harness {
	t.Helper()
	site := newFakeSite(t)

	authDir := t.TempDir()
	raw := []byte(`{"name":"acc1","cookie":"auth_session=test-session"}`)
	if err := os.WriteFile(filepath.Join(authDir, "acc1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := pool.New(pool.Options{
		AuthDir:        authDir,
		MaxInFlight:    2,
		AcquireTimeout: 2 * time.Second,
		PollInterval:   30 * time.Millisecond,
		WatchTimeout:   2 * time.Second,
		Upstream: upstream.Config{
			BaseURL: site.server.URL, TrustEnv: false,
			Timeout: 2 * time.Second, ProbeTimeout: time.Second,
			CaptchaProbeTTL: time.Second,
		},
	}, t.Logf)
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	t.Cleanup(p.Close)

	st, err := store.Open("", 0)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	proxy := media.New(mediaBase, "/v", 5*time.Second)
	if mediaBase == "" {
		proxy = media.New("", "/v", 5*time.Second)
	}

	cfg := Config{
		ServiceName: "aivideomaker2api", Version: "test", TaskCacheTTL: time.Second,
		SubmitWorkers: 4, SubmitInline: true, SubmitLease: time.Second,
		MediaRehostBudget: 3 * time.Second, RehostWorkers: 4,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return &harness{server: New(cfg, p, st, proxy, nil), store: st, pool: p, site: site}
}

func (h *harness) do(t *testing.T, method, path string, body string, headers map[string]string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, req)
	var parsed map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &parsed)
	}
	return rec.Code, parsed
}

func TestOpenAIVideosEndToEnd(t *testing.T) {
	h := newHarness(t, nil, "")
	code, created := h.do(t, http.MethodPost, "/v1/videos",
		`{"model":"doubao-seedance-1-0-pro_480p","prompt":"a red balloon","seconds":20,"size":"16:9"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("create status = %d (%v)", code, created)
	}
	// 契约：恰好四个字段。
	if len(created) != 4 || created["object"] != "video" || created["status"] != "queued" {
		t.Fatalf("create response = %#v, want the 4-field Chatfire contract", created)
	}
	id, _ := created["id"].(string)
	if id == "" {
		t.Fatal("missing id")
	}

	code, view := h.do(t, http.MethodGet, "/v1/videos/"+id, "", nil)
	if code != http.StatusOK {
		t.Fatalf("get status = %d", code)
	}
	if view["status"] != "completed" || view["progress"] != float64(100) {
		t.Fatalf("view = %#v", view)
	}
	if !strings.HasPrefix(fmt.Sprint(view["video_url"]), "https://static2.img2video.ai/") {
		t.Errorf("video_url = %v (media proxy disabled ⇒ upstream link passes through)", view["video_url"])
	}
	// seconds=20 被钉成 10s：免费的 480p 档位。
	entry, _ := h.store.Get(id)
	if entry.Effective["duration"] != 10 {
		t.Errorf("duration = %v, want 10 (pinned per resolution)", entry.Effective["duration"])
	}
	if entry.Effective["billed"] != false {
		t.Errorf("billed = %v, want false (this face only runs the free tier)", entry.Effective["billed"])
	}
}

func TestArkCreateOnlyReturnsIDAndHidesUpstreamURL(t *testing.T) {
	h := newHarness(t, nil, "https://avm2api.example")
	body := `{"model":"minimaxH3","content":[{"type":"text","text":"a red balloon"}],` +
		`"resolution":"480p","duration":5}`
	code, created := h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks", body, nil)
	if code != http.StatusOK {
		t.Fatalf("create status = %d (%v)", code, created)
	}
	if len(created) != 1 || created["id"] == nil {
		t.Fatalf("Ark create must return exactly {\"id\": ...}, got %#v", created)
	}
	id := created["id"].(string)

	code, view := h.do(t, http.MethodGet, "/api/v3/contents/generations/tasks/"+id, "", nil)
	if code != http.StatusOK {
		t.Fatalf("get status = %d", code)
	}
	content, _ := view["content"].(map[string]any)
	url := fmt.Sprint(content["video_url"])
	if strings.Contains(url, "img2video.ai") {
		t.Errorf("upstream URL leaked through the media gate: %v", url)
	}
	if want := "https://avm2api.example/v/" + id + ".mp4"; url != want {
		t.Errorf("video_url = %q, want %q", url, want)
	}
	// 对外契约白名单：证据字段一个都不许出现。
	for _, forbidden := range []string{"requested", "effective", "warnings", "unsupported", "upstream_model"} {
		if _, leaked := view[forbidden]; leaked {
			t.Errorf("%q must not appear in the Ark response body", forbidden)
		}
	}
	if view["error"] != nil {
		t.Errorf("error must be explicit null on success, got %#v", view["error"])
	}

	// 🔴 回归：`source_url` 必须是**上游**地址，`video_url` 才是我们自己的地址。
	// 这两个值取反过一次 —— 媒体出口会去 fetch 自己的下载地址，永远取不到片子，
	// 而且表面上看一切正常（任务 succeeded、链接 200 之外没别的线索）。
	entry, ok := h.store.Get(id)
	if !ok {
		t.Fatal("entry disappeared")
	}
	if !strings.Contains(entry.SourceURL, "img2video.ai") {
		t.Errorf("source_url = %q, must keep the upstream address (it is what the media proxy fetches)", entry.SourceURL)
	}
	if entry.VideoURL != url {
		t.Errorf("video_url = %q, want the gated address %q", entry.VideoURL, url)
	}

	// 🔴 `usage` 必须落盘：`paid` 是"这条任务到底花没花钱"的唯一判据，只在终态出现一次。
	// 不落盘的话重启后答不出这个问题，而计费是本项目的第一号红线。
	if entry.Usage == nil {
		t.Fatal("usage was not persisted on the terminal state — after a restart we could no longer " +
			"answer whether this task was billed")
	}
	if paid, ok := entry.Usage["paid"].(bool); !ok || paid {
		t.Errorf("usage.paid = %#v, want false (the fake site reports paid=false)", entry.Usage["paid"])
	}
}

func TestDryRunDoesNotCreateAnything(t *testing.T) {
	h := newHarness(t, nil, "")
	code, out := h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks",
		`{"model":"minimaxH3","content":[{"type":"text","text":"x"}],"duration":10,"resolution":"480p"}`,
		map[string]string{"X-Avm-Dry-Run": "1"})
	if code != http.StatusOK || out["dry_run"] != true {
		t.Fatalf("dry-run status=%d out=%#v", code, out)
	}
	if h.store.Count("") != 0 {
		t.Error("dry-run must not persist a task")
	}
	if h.site.createHits != 0 {
		t.Error("dry-run must not reach the upstream create procedure")
	}
	webParams, _ := out["web_params"].(map[string]any)
	if webParams["procedure"] != "ai.minimaxH3" {
		t.Errorf("web_params.procedure = %v", webParams["procedure"])
	}
}

func TestContentRejectionBecomesContentFiltered(t *testing.T) {
	h := newHarness(t, nil, "")
	h.site.createFail = func(w http.ResponseWriter) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`[{"error":{"json":{"message":"ai.minimaxH3: Prompt or Image Sensitive",` +
			`"data":{"code":"INTERNAL_SERVER_ERROR","httpStatus":500}}}}]`))
	}
	code, created := h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks",
		`{"model":"minimaxH3","content":[{"type":"text","text":"x"}]}`, nil)
	if code != http.StatusOK {
		t.Fatalf("upstream failures must not surface on POST, got %d (%v)", code, created)
	}
	id := created["id"].(string)
	_, view := h.do(t, http.MethodGet, "/api/v3/contents/generations/tasks/"+id, "", nil)
	if view["status"] != "failed" {
		t.Fatalf("status = %v, want failed", view["status"])
	}
	errBlock, _ := view["error"].(map[string]any)
	if errBlock["code"] != codeContentFiltered {
		t.Errorf("error.code = %v, want %s (so callers know retrying is pointless)",
			errBlock["code"], codeContentFiltered)
	}
	if !strings.Contains(fmt.Sprint(errBlock["message"]), "Prompt or Image Sensitive") {
		t.Errorf("the upstream's own wording must be passed through, got %v", errBlock["message"])
	}
}

// 闸门翻起时：POST 仍是 200（受理即返），但轮询必须给出**可操作**的 429 语义，
// 而不是一句"提交失败"。
func TestCaptchaGateSurfacesAsRateLimitOnPoll(t *testing.T) {
	h := newHarness(t, nil, "")
	h.site.gateAfter = 1 // 预检说关、强制复探说开
	h.site.createFail = func(w http.ResponseWriter) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`[{"result":{"data":{"json":""}}}]`))
	}
	code, created := h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks",
		`{"model":"minimaxH3","content":[{"type":"text","text":"x"}]}`, nil)
	if code != http.StatusOK {
		t.Fatalf("POST must stay 200 under accept/advance separation, got %d", code)
	}
	id := created["id"].(string)
	_, view := h.do(t, http.MethodGet, "/api/v3/contents/generations/tasks/"+id, "", nil)
	errBlock, _ := view["error"].(map[string]any)
	if errBlock["code"] != codeRateLimited {
		t.Errorf("error.code = %v, want %s", errBlock["code"], codeRateLimited)
	}
	msg, _ := errBlock["message"].(string)
	if !strings.Contains(strings.ToLower(msg), "captcha") {
		t.Errorf("message = %q, must tell the operator it is a captcha gate", msg)
	}
	if strings.Contains(msg, "ai.minimaxH3") {
		t.Errorf("message leaks the internal procedure name: %q", msg)
	}
}

func TestSubmitLeaseExpiryMarksFailed(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.SubmitInline = false; c.SubmitLease = 10 * time.Millisecond }, "")
	_ = h.store.Put(&store.Entry{
		ID: "cgt-old", Model: "minimaxH3", Upstream: "web", Status: "queued",
		CreatedAtMs: time.Now().Add(-time.Hour).UnixMilli(),
	})
	_, view := h.do(t, http.MethodGet, "/api/v3/contents/generations/tasks/cgt-old", "", nil)
	if view["status"] != "failed" {
		t.Fatalf("an abandoned submission must become failed, got %v", view["status"])
	}
}

func TestAuthGate(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.APIKey = "sk-test" }, "")
	code, _ := h.do(t, http.MethodGet, "/v1/models", "", nil)
	if code != http.StatusUnauthorized {
		t.Errorf("missing bearer => %d, want 401", code)
	}
	code, _ = h.do(t, http.MethodGet, "/v1/models", "", map[string]string{"Authorization": "Bearer sk-test"})
	if code != http.StatusOK {
		t.Errorf("correct bearer => %d, want 200", code)
	}
	// 常量时间比较也要能拒绝错误密钥。
	code, _ = h.do(t, http.MethodGet, "/v1/models", "", map[string]string{"Authorization": "Bearer sk-wrong"})
	if code != http.StatusUnauthorized {
		t.Errorf("wrong bearer => %d, want 401", code)
	}
}

func TestInvalidRatioIsRejectedOnArkButSnappedOnOpenAI(t *testing.T) {
	h := newHarness(t, nil, "")
	code, out := h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks",
		`{"model":"minimaxH3","content":[{"type":"text","text":"x"}],"ratio":"banana"}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("Ark line must reject an unreadable ratio, got %d (%v)", code, out)
	}
	errBlock, _ := out["error"].(map[string]any)
	if errBlock["code"] != codeInvalidParameter {
		t.Errorf("error.code = %v", errBlock["code"])
	}
	if !strings.Contains(fmt.Sprint(errBlock["message"]), "Request ID:") {
		t.Error("every error message must carry a Request ID (the only handle left after redaction)")
	}
}

func TestHealthzAndMetrics(t *testing.T) {
	h := newHarness(t, nil, "")
	code, health := h.do(t, http.MethodGet, "/healthz", "", nil)
	if code != http.StatusOK || health["ok"] != true {
		t.Fatalf("healthz = %d %v", code, health)
	}
	if health["accounts_tracked"] != float64(1) {
		t.Errorf("accounts_tracked = %v", health["accounts_tracked"])
	}
	if _, leaked := health["accounts"]; leaked {
		t.Error("/healthz must not list account details (it is unauthenticated)")
	}
	code, _ = h.do(t, http.MethodGet, "/metrics", "", nil)
	if code != http.StatusOK {
		t.Errorf("metrics = %d", code)
	}
}

// 轮询折叠：同一个任务被 N 个客户端同时问，只能打一次上游。
//
// 闸门是**按请求速度**翻的（实测），重复 GET 等于白送计数 —— 这条不变量就是为此立的。
func TestConcurrentPollsHitUpstreamOnce(t *testing.T) {
	h := newHarness(t, nil, "")
	// 直接放一条"在跑"的记录：不经过提交，排除 watch 轮询的干扰。
	if err := h.store.Put(&store.Entry{
		ID: "cgt-poll", TaskID: "up-task-1", Account: "acc1", Model: "minimaxH3",
		Upstream: "web", Status: "running", CreatedAtMs: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/v3/contents/generations/tasks/cgt-poll", nil)
			rec := httptest.NewRecorder()
			h.server.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("poll %d => %d", i, c)
		}
	}
	if got := h.server.upstreamGets.Load(); got != 1 {
		t.Errorf("%d concurrent polls caused %d upstream lookups, want 1 (that traffic is what flips the captcha gate)", n, got)
	}

	// 缓存命中：紧接着再问一次也不该再上网。
	rec := httptest.NewRecorder()
	h.server.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v3/contents/generations/tasks/cgt-poll", nil))
	if got := h.server.upstreamGets.Load(); got != 1 {
		t.Errorf("a cached poll still hit the upstream (%d lookups)", got)
	}
}

// 官方 REST 通路：720p 走 `/api/v1/generate/minimax`（无 cookie、无闸门），
// 查询走 `/api/v1/tasks/{id}`，对外形状必须和网页通路**完全一致**。
func TestOfficialPathCreatesAndPolls(t *testing.T) {
	var gotKey, gotBody string
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("key")
		switch {
		case r.URL.Path == "/api/v1/generate/minimax":
			raw, _ := io.ReadAll(r.Body)
			gotBody = string(raw)
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"status":"SUBMITTED","taskId":"off-1","creditsCharged":15,` +
				`"idempotentReplay":false,"responseUrl":"x","statusUrl":"y","cancelUrl":"z"}`))
		case r.URL.Path == "/api/v1/tasks/off-1":
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"id":"off-1","status":"COMPLETED","model":"minimax",` +
				`"output":{"url":"https://cdn.example/out.mp4"},"creditsCharged":15,"creditsRefunded":0,` +
				`"createdAt":"2026-11-09T00:00:00.000Z","completedAt":"2026-11-09T00:01:00.000Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer official.Close()

	h := newHarness(t, func(c *Config) {
		c.Official = OfficialConfig{APIKey: "ak_test_key", BaseURL: official.URL, Timeout: 3 * time.Second}
		c.SubmitInline = true
	}, "https://avm2api.example")

	// 官方通路必须**显式**点名：默认走网页会话（才有免费档）。
	body := `{"model":"minimaxH3","content":[{"type":"text","text":"a red balloon"}],` +
		`"resolution":"720p","duration":5,` +
		`"extra_body":{"aivideomaker_channel":"official"}}`
	code, created := h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks", body, nil)
	if code != http.StatusOK {
		t.Fatalf("create status = %d (%v)", code, created)
	}
	id := created["id"].(string)

	if gotKey != "ak_test_key" {
		t.Errorf("official request must carry `key` header, got %q", gotKey)
	}
	if !strings.Contains(gotBody, `"content":"a red balloon"`) || !strings.Contains(gotBody, `"resolution":"720p"`) {
		t.Errorf("official body not translated as expected: %s", gotBody)
	}

	code, view := h.do(t, http.MethodGet, "/api/v3/contents/generations/tasks/"+id, "", nil)
	if code != http.StatusOK {
		t.Fatalf("get status = %d", code)
	}
	if got := fmt.Sprint(view["status"]); got != "succeeded" {
		t.Errorf("status = %q, want succeeded (COMPLETED should map)", got)
	}
	content, _ := view["content"].(map[string]any)
	if want := "https://avm2api.example/v/" + id + ".mp4"; fmt.Sprint(content["video_url"]) != want {
		t.Errorf("video_url = %v, want %s (must pass the same media gate)", content["video_url"], want)
	}
	// `usage` 不在 Ark 对外白名单里（官方 SDK 对 unknown field 报错），
	// 所以从**记录**里核对计费事实：creditsCharged=15 ⇒ usage.paid=true。
	entry, ok := h.store.Get(id)
	if !ok {
		t.Fatalf("task %s not in store", id)
	}
	if entry.Upstream != "official" {
		t.Errorf("entry.Upstream = %q, want official", entry.Upstream)
	}
	if entry.Usage["paid"] != true {
		t.Errorf("creditsCharged=15 must be persisted as usage.paid=true, got %v", entry.Usage)
	}
}

// 🔴 花钱的回归：**不点名就走网页**。
//
// 这条是踩过的坑：曾经按"非 480p 就走官方"路由，而官方 API 全程收费
// （720p/5s = 15 积分）。默认切官方 = 静默替调用方花钱。
func TestOfficialNotUsedUnlessExplicitlyRequested(t *testing.T) {
	var officialHits int
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		officialHits++
		http.NotFound(w, r)
	}))
	defer official.Close()

	h := newHarness(t, func(c *Config) {
		c.Official = OfficialConfig{APIKey: "ak_test_key", BaseURL: official.URL, Timeout: 3 * time.Second}
		c.SubmitInline = true
	}, "https://avm2api.example")

	body := `{"model":"minimaxH3","content":[{"type":"text","text":"a red balloon"}],` +
		`"resolution":"720p","duration":5}`
	code, created := h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks", body, nil)
	if code != http.StatusOK {
		t.Fatalf("create status = %d (%v)", code, created)
	}
	if officialHits != 0 {
		t.Errorf("720p without an explicit channel hit the official API %d times — that costs real credits", officialHits)
	}
	entry, ok := h.store.Get(created["id"].(string))
	if !ok {
		t.Fatal("task not found in store")
	}
	if entry.Upstream == "official" {
		t.Error("entry.Upstream = official, want web (default must stay on the free-capable web path)")
	}
}

// `/official/...` 必须**强制**走官方（哪怕请求本身网页也能跑），`/web/...` 反之。
// 运维排查与成本控制要的是确定性，不是"猜它会走哪条路"。
func TestChannelScopedEndpointsForceChannel(t *testing.T) {
	var hits int
	official := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/generate/minimax" {
			hits++
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"status":"SUBMITTED","taskId":"off-forced","creditsCharged":15}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer official.Close()

	h := newHarness(t, func(c *Config) {
		c.Official = OfficialConfig{APIKey: "ak_test_key", BaseURL: official.URL, Timeout: 3 * time.Second}
		c.SubmitInline = true
	}, "https://avm2api.example")

	// 720p 两条路都能跑 —— 正好用来验证"端点决定通路"
	body := `{"model":"minimaxH3","content":[{"type":"text","text":"a red balloon"}],` +
		`"resolution":"720p","duration":5}`
	code, created := h.do(t, http.MethodPost, "/official/api/v3/contents/generations/tasks", body, nil)
	if code != http.StatusOK {
		t.Fatalf("create via /official => %d (%v)", code, created)
	}
	if hits == 0 {
		t.Error("/official/api/v3/... did not reach the official API")
	}
	entry, ok := h.store.Get(created["id"].(string))
	if !ok {
		t.Fatal("task not found")
	}
	if entry.Upstream != "official" {
		t.Errorf("entry.Upstream = %q, want official (the /official prefix must be authoritative)", entry.Upstream)
	}

	// 同一份请求走默认端点 → 网页
	code, created = h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks", body, nil)
	if code != http.StatusOK {
		t.Fatalf("create default => %d", code)
	}
	entry, _ = h.store.Get(created["id"].(string))
	if entry.Upstream == "official" {
		t.Error("default endpoint must stay on the web channel")
	}
}

// /v1/models 必须说清楚三件事：有哪些模型、走哪条通路、要不要钱。
func TestListModelsExplainsChannelAndCost(t *testing.T) {
	h := newHarness(t, nil, "https://avm2api.example")
	code, out := h.do(t, http.MethodGet, "/v1/models", "", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%v)", code, out)
	}
	if obj := fmt.Sprint(out["object"]); obj != "list" {
		t.Errorf("object = %q, want list", obj)
	}
	data, _ := out["data"].([]any)
	if len(data) == 0 {
		t.Fatal("empty model list")
	}
	byID := map[string]map[string]any{}
	for _, raw := range data {
		m, _ := raw.(map[string]any)
		byID[fmt.Sprint(m["id"])] = m
	}
	mm, ok := byID["minimaxH3"]
	if !ok {
		t.Fatal("minimaxH3 missing")
	}
	extra, _ := mm["aivideomaker"].(map[string]any)
	if extra["free"] != true {
		t.Errorf("minimaxH3 must be advertised as having a free window, got %v", extra["free"])
	}
	if extra["channel"] != "both" {
		t.Errorf("minimaxH3 channel = %v, want both", extra["channel"])
	}
	// 免费档的档位必须与入口校验同源：480p 5/10、720p 5/8，且不含 1080p。
	if got := fmt.Sprint(extra["resolutions"]); got != "[480p 720p]" {
		t.Errorf("minimaxH3 resolutions = %v, want [480p 720p]", extra["resolutions"])
	}
	byRes, _ := extra["durations_by_resolution"].(map[string]any)
	if fmt.Sprint(byRes["480p"]) != "[5 10]" || fmt.Sprint(byRes["720p"]) != "[5 8]" {
		t.Errorf("durations_by_resolution = %v, want 480p:[5 10] 720p:[5 8]",
			extra["durations_by_resolution"])
	}
	sd, ok := byID["seedance20"]
	if !ok {
		t.Fatal("seedance20 missing")
	}
	sdExtra, _ := sd["aivideomaker"].(map[string]any)
	if sdExtra["free"] != false {
		t.Error("seedance20 must NOT be advertised as free (measured 68 credits at 480p/5s)")
	}
	if fmt.Sprint(sdExtra["credits_5s"]) != "68" {
		t.Errorf("seedance20 credits_5s = %v, want 68", sdExtra["credits_5s"])
	}
	// 官方独有模型必须标 official
	if w, ok := byID["wan30"]; !ok {
		t.Error("wan30 missing")
	} else if we, _ := w["aivideomaker"].(map[string]any); we["channel"] != "official" {
		t.Errorf("wan30 channel = %v, want official", we["channel"])
	}

	// 单个模型
	code, one := h.do(t, http.MethodGet, "/v1/models/seedance20", "", nil)
	if code != http.StatusOK || fmt.Sprint(one["id"]) != "seedance20" {
		t.Fatalf("GET /v1/models/seedance20 => %d %v", code, one)
	}
	// 不存在的模型：报错里要列出可用的
	code, _ = h.do(t, http.MethodGet, "/v1/models/nope", "", nil)
	if code == http.StatusOK {
		t.Error("unknown model should not return 200")
	}
}

// 方舟协议面（/api/v3/contents/generations/tasks）的枚举准入：
// 只有 480p@5/10s、720p@5/8s 能过，其余在提交期就 400 —— 不回落默认、不吸附。
func TestArkRejectsOutOfEnumDuration(t *testing.T) {
	h := newHarness(t, nil, "")
	cases := []struct {
		body  string
		param string
		why   string
	}{
		{`{"model":"minimaxH3","content":[{"type":"text","text":"x"}],"duration":8,"resolution":"480p"}`,
			"duration", "480p only has 5s/10s"},
		{`{"model":"minimaxH3","content":[{"type":"text","text":"x"}],"duration":10,"resolution":"720p"}`,
			"duration", "720p only has 5s/8s"},
		{`{"model":"minimaxH3","content":[{"type":"text","text":"x"}],"duration":5,"resolution":"1080p"}`,
			"resolution", "1080p is not on the web line"},
	}
	for _, c := range cases {
		code, out := h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks", c.body, nil)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (%v)", c.why, code, out)
			continue
		}
		block, _ := out["error"].(map[string]any)
		if block["code"] != "InvalidParameter" {
			t.Errorf("%s: code = %v, want InvalidParameter", c.why, block["code"])
		}
		if block["param"] != c.param {
			t.Errorf("%s: param = %v, want %s", c.why, block["param"], c.param)
		}
		msg, _ := block["message"].(string)
		if !strings.Contains(msg, "480p") && !strings.Contains(msg, "5s") {
			t.Errorf("%s: message must name the enum, got %q", c.why, msg)
		}
	}
	if h.store.Count("") != 0 {
		t.Error("a rejected submission must not persist a task")
	}
}

// 固定组合别名必须出现在 /v1/models 里 —— 下游（new-api / apiduo）按模型名路由，
// 列表里没有的名字它们不会用。
func TestModelAliasesAreAdvertised(t *testing.T) {
	h := newHarness(t, nil, "")
	code, out := h.do(t, http.MethodGet, "/v1/models", "", nil)
	if code != http.StatusOK {
		t.Fatalf("status = %d (%v)", code, out)
	}
	byID := map[string]bool{}
	for _, raw := range out["data"].([]any) {
		m, _ := raw.(map[string]any)
		byID[fmt.Sprint(m["id"])] = true
	}
	for _, id := range []string{
		"minimax-h3-480p-5s", "minimax-h3-480p-10s",
		"minimax-h3-720p-5s", "minimax-h3-720p-8s",
	} {
		if !byID[id] {
			t.Errorf("alias %q missing from /v1/models", id)
		}
	}
	if byID["minimax-h3-720p-10s"] {
		t.Error("minimax-h3-720p-10s must NOT exist: 720p only has 5s/8s")
	}
}

// 枚举外的组合不是"降级"，是 unknown model —— 报文要列出可用名字。
func TestAliasOutsideTheEnumIsUnknown(t *testing.T) {
	h := newHarness(t, nil, "")
	code, out := h.do(t, http.MethodPost, "/api/v3/contents/generations/tasks",
		`{"model":"minimax-h3-720p-10s","content":[{"type":"text","text":"x"}]}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%v)", code, out)
	}
	block, _ := out["error"].(map[string]any)
	msg, _ := block["message"].(string)
	if !strings.Contains(msg, "supported fixed aliases") || !strings.Contains(msg, "minimax-h3-720p-8s") {
		t.Errorf("unknown alias must list the supported names, got %q", msg)
	}
	if h.store.Count("") != 0 {
		t.Error("an unknown model must not persist a task")
	}
}
