package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jushenzhidao/aivideomaker2api/internal/apierr"
)

// fakeSite 是一个**完全离线**的站点替身：只实现本服务真正调用的那几个 procedure。
type fakeSite struct {
	server     *httptest.Server
	createHits int32
	createBody []byte
	createFunc func(w http.ResponseWriter, n int32)
	needsHits  int32
	uploads    int32
	// gateAfter > 0 时：前 gateAfter 次闸门探测返回 false，之后返回 true
	// （模拟站点"跑满若干条后闸门翻起"的动态行为）。
	gateAfter int32
}

func newFakeSite(t *testing.T) *fakeSite {
	t.Helper()
	fs := &fakeSite{}
	mux := http.NewServeMux()
	envelope := func(v any) []byte {
		b, _ := json.Marshal([]any{map[string]any{"result": map[string]any{"data": map[string]any{"json": v}}}})
		return b
	}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(envelope(v))
	}
	mux.HandleFunc("/api/auth.user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "u-1", "email": "tester@example.com"})
	})
	mux.HandleFunc("/api/credits.getCredits", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"totalRemaining": 4321})
	})
	mux.HandleFunc("/api/ai.queryUserPermission", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"maxQueueLength": 4, "planName": "pro"})
	})
	mux.HandleFunc("/api/model.needsCaptcha", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&fs.needsHits, 1)
		writeJSON(w, fs.gateAfter > 0 && n > fs.gateAfter)
	})
	mux.HandleFunc("/api/ai.minimaxH3", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&fs.createHits, 1)
		body, _ := io.ReadAll(r.Body)
		fs.createBody = body
		if fs.createFunc != nil {
			fs.createFunc(w, n)
			return
		}
		writeJSON(w, "task-123")
	})
	mux.HandleFunc("/api/model.getModel", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"id": "task-123", "taskStatus": "succeed", "url": "https://cdn.example/v.mp4",
			"aiModel": "minimax_h3", "kelingKeyId": "480", "paid": false, "credits": 1, "duration": "5",
		})
	})
	mux.HandleFunc("/api/uploads.getPresignedUrl", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fs.uploads, 1)
		writeJSON(w, map[string]any{
			"uploadUrl": fs.server.URL + "/put", "publicUrl": "https://static.img2video.ai/a.png",
			"headers": map[string]any{"Content-Type": "image/png"}, "maxBytes": 10 * 1024 * 1024,
		})
	})
	mux.HandleFunc("/put", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	fs.server = httptest.NewServer(mux)
	t.Cleanup(fs.server.Close)
	return fs
}

func (fs *fakeSite) client(t *testing.T, mutate func(*Config)) *Client {
	t.Helper()
	cfg := Config{
		BaseURL:            fs.server.URL,
		Cookie:             "auth_session=test",
		Timeout:            5 * time.Second,
		ProbeTimeout:       2 * time.Second,
		TrustEnv:           false,
		SubmitRetries:      2,
		SubmitRetryBackoff: time.Millisecond,
		MediaRetries:       1,
		MediaFetchTimeout:  2 * time.Second,
		CaptchaProbeTTL:    5 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	client, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestCreateSendsWhitelistedBody(t *testing.T) {
	fs := newFakeSite(t)
	client := fs.client(t, nil)
	id, err := client.Create(context.Background(), map[string]any{
		"content": "a red balloon", "duration": 5, "resolution": "480p", "tier": "turbo",
		"promptEnrichment": true, "procedure": "ai.minimaxH3",
		"referenceImageUrls": []string{"https://static.img2video.ai/a.png"},
	}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "task-123" {
		t.Fatalf("task id = %q", id)
	}
	var body map[string]any
	if err := json.Unmarshal(fs.createBody, &body); err != nil {
		t.Fatalf("create body is not JSON: %v", err)
	}
	inner := body["0"].(map[string]any)["json"].(map[string]any)
	for _, forbidden := range []string{"model", "procedure"} {
		if _, leaked := inner[forbidden]; leaked {
			t.Errorf("%q must not reach the upstream body (the site encodes the model in the procedure path)", forbidden)
		}
	}
	for _, want := range []string{"content", "aspectRatio", "duration", "resolution", "tier",
		"promptEnrichment", "visitorId", "token", "referenceImageUrls"} {
		if _, ok := inner[want]; !ok {
			t.Errorf("upstream body is missing %q", want)
		}
	}
	if inner["aspectRatio"] != "16:9" {
		t.Errorf("aspectRatio default = %v, want 16:9", inner["aspectRatio"])
	}
	if inner["token"] != nil {
		t.Errorf("token must be null when the gate is closed, got %v", inner["token"])
	}
}

// 空 taskId 是站点的"静默拒"。必须**强制复探闸门**再定性：
// 闸门翻起 ⇒ 429 CAPTCHA_REQUIRED（可等它衰减）；闸门关着 ⇒ 参数/余额问题。
// 含糊地报"可能是验证码也可能是参数"会让运维两边瞎猜。
func TestCreateClassifiesEmptyTaskIDByGateState(t *testing.T) {
	fs := newFakeSite(t)
	fs.gateAfter = 1 // 预检说"关"，之后的强制复探说"开"
	fs.createFunc = func(w http.ResponseWriter, n int32) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`[{"result":{"data":{"json":""}}}]`)) // 空串
	}
	client := fs.client(t, func(c *Config) { c.SubmitRetries = 0 })
	_, err := client.Create(context.Background(), map[string]any{"content": "x"}, "")
	upErr, ok := err.(*apierr.UpstreamError)
	if !ok {
		t.Fatalf("err = %T %v, want *apierr.UpstreamError", err, err)
	}
	if upErr.Code != "CAPTCHA_REQUIRED" {
		t.Errorf("code = %q, want CAPTCHA_REQUIRED (the gate was open, that is the actionable fact)",
			upErr.Code)
	}
	if upErr.HTTPStatus != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", upErr.HTTPStatus)
	}
}

// 闸门关着却被拒时，报文必须带上**当前余额**这个事实 —— 站点对"余额不足"也是静默拒，
// 不带余额的话运维只能在"参数错"与"没钱"之间盲猜。
func TestRejectedWithGateClosedReportsBalance(t *testing.T) {
	fs := newFakeSite(t)
	fs.createFunc = func(w http.ResponseWriter, n int32) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`[{"result":{"data":{"json":""}}}]`))
	}
	client := fs.client(t, func(c *Config) { c.SubmitRetries = 0 })
	_, err := client.Create(context.Background(), map[string]any{"content": "x"}, "")
	if err == nil {
		t.Fatal("an empty task id must surface as an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "balance") {
		t.Errorf("message = %q, must mention the balance (the site rejects silently on insufficient credits)", msg)
	}
	if !strings.Contains(msg, "4321") {
		t.Errorf("message = %q, must carry the actual balance from credits.getCredits", msg)
	}
}

// 回归：纯文生视频的请求体里，两个数组字段必须是 `[]` 而不是 `null`。
//
// 站点 schema（zod）要求数组，收到 null 会回 `invalid_type expected array received null`
// —— 也就是**每一条纯文生视频都会失败**。本地假站点不校验形状，所以这条必须显式断言
// JSON 类型，而不是"键存在"。
func TestCreateSendsEmptyArraysNotEmptyNull(t *testing.T) {
	fs := newFakeSite(t)
	client := fs.client(t, nil)
	_, err := client.Create(context.Background(), map[string]any{
		"content": "a red balloon", "duration": 5, "resolution": "480p", "tier": "turbo",
		"promptEnrichment": true, "procedure": "ai.minimaxH3",
		// 刻意不给两个数组字段：translate 产出的就是 typed-nil []string
		"referenceImageUrls": []string(nil),
		"referenceAudioUrls": []string(nil),
	}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(fs.createBody, &body); err != nil {
		t.Fatalf("create body is not JSON: %v", err)
	}
	inner := body["0"].(map[string]any)["json"].(map[string]any)
	for _, key := range []string{"referenceImageUrls", "referenceAudioUrls"} {
		value, present := inner[key]
		if !present {
			t.Errorf("%q is missing", key)
			continue
		}
		if _, isArray := value.([]any); !isArray {
			t.Errorf("%q = %#v (%T), want a JSON array — the site rejects null with "+
				"invalid_type expected array", key, value, value)
		}
	}
}

func TestCreateRetriesWhenQueueIsFull(t *testing.T) {
	fs := newFakeSite(t)
	fs.createFunc = func(w http.ResponseWriter, n int32) {
		if n == 1 {
			// 站点原话（生产实测 19 条）：这是"账号并发全满"，不是参数或内容问题。
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`[{"error":{"json":{"message":"The queue is full. The pro plan can only run 4 task at a time.","data":{"code":"INTERNAL_SERVER_ERROR","httpStatus":500}}}}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"result":{"data":{"json":"task-ok"}}}]`))
	}
	client := fs.client(t, nil)
	id, err := client.Create(context.Background(), map[string]any{"content": "x", "duration": 5}, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "task-ok" || atomic.LoadInt32(&fs.createHits) != 2 {
		t.Fatalf("id=%q hits=%d, want task-ok after 2 attempts", id, fs.createHits)
	}
}

func TestCreateDoesNotRetryContentRejection(t *testing.T) {
	fs := newFakeSite(t)
	fs.createFunc = func(w http.ResponseWriter, n int32) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`[{"error":{"json":{"message":"ai.minimaxH3: Prompt or Image Sensitive","data":{"code":"INTERNAL_SERVER_ERROR","httpStatus":500}}}}]`))
	}
	client := fs.client(t, nil)
	_, err := client.Create(context.Background(), map[string]any{"content": "x"}, "")
	if err == nil {
		t.Fatal("content rejection must surface as an error")
	}
	if atomic.LoadInt32(&fs.createHits) != 1 {
		t.Errorf("content rejection must never be retried (hits=%d)", fs.createHits)
	}
	if !IsContentRejection(err) {
		t.Error("the error must be classified as a content rejection so the API can return 400 ContentFiltered")
	}
}

func TestCreateDoesNotRetryReadTimeout(t *testing.T) {
	fs := newFakeSite(t)
	fs.createFunc = func(w http.ResponseWriter, n int32) {
		time.Sleep(120 * time.Millisecond)
		_, _ = w.Write([]byte(`[{"result":{"data":{"json":"too-late"}}}]`))
	}
	client := fs.client(t, func(c *Config) { c.Timeout = 40 * time.Millisecond })
	_, err := client.Create(context.Background(), map[string]any{"content": "x"}, "")
	if err == nil {
		t.Fatal("a timed-out create must surface as an error")
	}
	if got := atomic.LoadInt32(&fs.createHits); got != 1 {
		// 请求可能已被上游接收 ⇒ 重发＝重复建任务＝重复扣费。
		t.Errorf("read timeouts must never be retried (hits=%d)", got)
	}
}

func TestSessionProbes(t *testing.T) {
	fs := newFakeSite(t)
	client := fs.client(t, nil)
	ctx := context.Background()
	if credits, err := client.GetCredits(ctx); err != nil || credits != 4321 {
		t.Fatalf("credits=%d err=%v", credits, err)
	}
	perm, err := client.GetPermission(ctx)
	if err != nil || perm["maxQueueLength"] == nil {
		t.Fatalf("permission=%v err=%v", perm, err)
	}
	if limit, _ := jsonxInt(perm["maxQueueLength"]); limit != 4 {
		t.Errorf("maxQueueLength = %v, want 4", perm["maxQueueLength"])
	}
}

func TestNeedsCaptchaIsCached(t *testing.T) {
	fs := newFakeSite(t)
	client := fs.client(t, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if gate, err := client.NeedsCaptcha(ctx, false); err != nil || gate {
			t.Fatalf("gate=%v err=%v", gate, err)
		}
	}
	if got := atomic.LoadInt32(&fs.needsHits); got != 1 {
		t.Errorf("captcha probe hit upstream %d times, want 1 within the TTL", got)
	}
	if _, err := client.NeedsCaptcha(ctx, true); err != nil {
		t.Fatalf("force probe: %v", err)
	}
	if got := atomic.LoadInt32(&fs.needsHits); got != 2 {
		t.Errorf("force=true must bypass the cache (hits=%d)", got)
	}
}

func TestGetTaskReturnsSiteRecord(t *testing.T) {
	fs := newFakeSite(t)
	client := fs.client(t, nil)
	rec, err := client.GetTask(context.Background(), "task-123", 0)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if rec["taskStatus"] != "succeed" {
		t.Errorf("record = %v", rec)
	}
}

func TestUploadBytesGoesThroughPresignAndPUT(t *testing.T) {
	fs := newFakeSite(t)
	client := fs.client(t, nil)
	png := []byte("\x89PNG\r\n\x1a\n" + "0000000000000000")
	up, err := client.UploadBytes(context.Background(), png, "inline", false, NewBudget(5*time.Second))
	if err != nil {
		t.Fatalf("UploadBytes: %v", err)
	}
	if up.PublicURL != "https://static.img2video.ai/a.png" {
		t.Errorf("publicUrl = %q", up.PublicURL)
	}
	if up.ContentType != "image/png" || up.Kind != "image" {
		t.Errorf("sniffed type = %s/%s", up.ContentType, up.Kind)
	}
	if atomic.LoadInt32(&fs.uploads) != 1 {
		t.Errorf("presign called %d times, want 1", fs.uploads)
	}
}

func TestQueueEnforcesCapacity(t *testing.T) {
	q := NewQueue(nil, QueueOptions{Capacity: 1, AcquireTimeout: 50 * time.Millisecond})
	if !q.TryAcquire() {
		t.Fatal("first acquire must succeed")
	}
	if q.TryAcquire() {
		t.Fatal("second acquire must fail while the slot is held")
	}
	if err := q.Acquire(context.Background()); err == nil {
		t.Fatal("blocking acquire must time out when the only slot is held")
	}
	q.Release()
	if !q.TryAcquire() {
		t.Fatal("released slot must be reused")
	}
}

func TestQueueRaiseCapacityOnlyGrows(t *testing.T) {
	q := NewQueue(nil, QueueOptions{Capacity: 2})
	if got := q.RaiseCapacity(4); got != 4 {
		t.Errorf("raise to 4 => %d", got)
	}
	if got := q.RaiseCapacity(1); got != 4 {
		t.Errorf("must never shrink, got %d", got)
	}
	acquired := 0
	for q.TryAcquire() {
		acquired++
	}
	if acquired != 4 {
		t.Errorf("acquired %d permits, want 4", acquired)
	}
}

// 回归：响应头先到、body 后到时，必须还能读到 body。
//
// 之前的实现是 `resp, err := Do(); cancel(); ReadAll(resp.Body)` —— cancel 立刻中止
// 请求上下文，慢网络下 ReadAll 返回 `context canceled`。本地 httptest 的响应体通常在
// Do 返回时就已缓冲完，所以这条**只有"头部先刷出去 + body 延迟"的假站点能复现**。
func TestTRPCSurvivesSlowBody(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(120 * time.Millisecond)
		_, _ = io.WriteString(w, `[{"result":{"data":{"json":{"totalRemaining":7}}}}]`)
	}))
	defer slow.Close()

	client, err := New(Config{
		BaseURL: slow.URL, Cookie: "auth_session=test",
		Timeout: 5 * time.Second, ProbeTimeout: 5 * time.Second, TrustEnv: false,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer client.Close()
	credits, err := client.GetCredits(context.Background())
	if err != nil {
		t.Fatalf("a slow body must still be read: %v", err)
	}
	if credits != 7 {
		t.Errorf("credits = %d, want 7", credits)
	}
}

// 已经铸过新 token 仍被拒 ⇒ 不重试。每次重试都要再铸一个 token，而铸造本身是一次
// 站点侧风控信号：一次注定失败的请求烧掉 6 个 token 会主动抬高被封概率。
func TestCaptchaRejectedIsNotRetried(t *testing.T) {
	rejected := &apierr.UpstreamError{Code: "CAPTCHA_REJECTED", HTTPStatus: 429, Message: "x"}
	if retryableSubmitError(rejected) {
		t.Error("CAPTCHA_REJECTED must not be retried")
	}
	required := &apierr.UpstreamError{Code: "CAPTCHA_REQUIRED", HTTPStatus: 429, Message: "x"}
	if !retryableSubmitError(required) {
		t.Error("CAPTCHA_REQUIRED (no token yet) is still retryable")
	}
}

func TestMinterUnconfiguredIsQuiet(t *testing.T) {
	m := NewMinter(MinterConfig{})
	if m.Configured() {
		t.Error("empty url must mean 'not configured'")
	}
	if _, ok := m.Mint(context.Background()); ok {
		t.Error("unconfigured minter must report failure, never a fake token")
	}
}

func TestMinterPicksUpToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/turnstile/mint" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprint(w, `{"token":"tok-1","source":"pool"}`)
	}))
	defer srv.Close()
	m := NewMinter(MinterConfig{URL: srv.URL, Timeout: time.Second})
	token, ok := m.Mint(context.Background())
	if !ok || token != "tok-1" {
		t.Fatalf("token=%q ok=%v", token, ok)
	}
	if m.LastSource() != "pool" {
		t.Errorf("source = %q", m.LastSource())
	}
}

// jsonxInt 是本测试文件用的小工具（生产代码走 jsonx.Int）。
func jsonxInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case json.Number:
		n, err := x.Int64()
		return int(n), err == nil
	case int:
		return x, true
	}
	return 0, false
}
