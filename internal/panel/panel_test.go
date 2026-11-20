package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsfree/aivideomaker2api/internal/pool"
	"github.com/rsfree/aivideomaker2api/internal/store"
	"github.com/rsfree/aivideomaker2api/internal/upstream"
)

func newPanel(t *testing.T, password string) (http.Handler, *store.Store) {
	t.Helper()
	return newPanelWithDeps(t, password, nil)
}

// newPanelWithDeps 允许用例注入 Deps（调试页那几个回调）。
func newPanelWithDeps(t *testing.T, password string, mutate func(*Deps)) (http.Handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	raw := []byte(`{"name":"acc1","cookie":"auth_session=test-session"}`)
	if err := os.WriteFile(filepath.Join(dir, "acc1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := pool.New(pool.Options{
		AuthDir: dir, MaxInFlight: 1, AcquireTimeout: time.Second,
		Upstream: upstream.Config{BaseURL: "http://127.0.0.1:1", TrustEnv: false},
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
	_ = st.Put(&store.Entry{ID: "cgt-1", Model: "minimaxH3", Upstream: "web", Status: "queued"})

	deps := Deps{
		Pool: p, Store: st, ServiceName: "aivideomaker2api", Version: "test",
		AuthMode: "key", BaseURL: "https://aivideomaker.ai", Started: time.Now(),
		ProbeInterval: 5 * time.Minute,
	}
	if mutate != nil {
		mutate(&deps)
	}
	return New(deps, password), st
}

// panelDo 发一次面板 API 请求（不带会话 cookie 时 Unauthorized 也算正常路径）。
func panelDo(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestPanelServesStrictCSP(t *testing.T) {
	handler, _ := newPanel(t, "")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q", csp)
	}
	if strings.Contains(csp, "unsafe-inline") {
		t.Error("panel must not need unsafe-inline (scripts/styles live in their own files)")
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("panel must refuse to be framed (clickjacking)")
	}
	if !strings.Contains(rec.Body.String(), "aivideomaker2api") {
		t.Error("index.html was not embedded")
	}
}

func TestPanelStaticIsCacheableButValidated(t *testing.T) {
	handler, _ := newPanel(t, "")
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/panel/app.js", nil))
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("static assets need an ETag (otherwise a stale app.js survives upgrades)")
	}
	if got := first.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	req := httptest.NewRequest(http.MethodGet, "/panel/app.js", nil)
	req.Header.Set("If-None-Match", etag)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, req)
	if second.Code != http.StatusNotModified {
		t.Errorf("revalidation status = %d, want 304", second.Code)
	}
}

func TestPanelLoginFlow(t *testing.T) {
	handler, _ := newPanel(t, "s3cret")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/accounts", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", rec.Code)
	}

	bad := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/login", strings.NewReader(`{"password":"nope"}`))
	handler.ServeHTTP(bad, req)
	if bad.Code != http.StatusUnauthorized {
		t.Errorf("wrong password => %d, want 401", bad.Code)
	}

	ok := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/panel/api/login", strings.NewReader(`{"password":"s3cret"}`))
	handler.ServeHTTP(ok, req)
	if ok.Code != http.StatusOK {
		t.Fatalf("login => %d (%s)", ok.Code, ok.Body.String())
	}
	cookies := ok.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login must set a session cookie")
	}
	cookie := cookies[0]

	authed := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/panel/api/accounts", nil)
	req.AddCookie(cookie)
	handler.ServeHTTP(authed, req)
	if authed.Code != http.StatusOK {
		t.Fatalf("authed accounts => %d", authed.Code)
	}
	if strings.Contains(authed.Body.String(), "test-session") {
		t.Error("the API must never return the raw credential (masked form only)")
	}
}

// 解冻按钮是"冷却中的号唯一的出路"：接口必须真的换掉设备标识并回传新值。
func TestAccountThawRotatesVisitorID(t *testing.T) {
	h, _ := newPanel(t, "")

	_, before := panelDo(t, h, http.MethodGet, "/panel/api/accounts", "")
	prev := ""
	for _, raw := range before["accounts"].([]any) {
		if acc, _ := raw.(map[string]any); acc["name"] == "acc1" {
			prev, _ = acc["visitor_id"].(string)
		}
	}

	code, out := panelDo(t, h, http.MethodPost, "/panel/api/accounts/acc1/thaw", "")
	if code != http.StatusOK {
		t.Fatalf("thaw status = %d (%v)", code, out)
	}
	id, _ := out["visitor_id"].(string)
	if len(id) != 32 {
		t.Fatalf("visitor_id = %q, want 32 hex chars", id)
	}
	if id == prev {
		t.Errorf("visitor_id unchanged (%q): thaw must rotate it, otherwise the account "+
			"immediately hits the same quota wall again", id)
	}

	_, after := panelDo(t, h, http.MethodGet, "/panel/api/accounts", "")
	for _, raw := range after["accounts"].([]any) {
		acc, _ := raw.(map[string]any)
		if acc["name"] != "acc1" {
			continue
		}
		if acc["visitor_id"] != id {
			t.Errorf("snapshot visitor_id = %v, want %q", acc["visitor_id"], id)
		}
	}
}

func TestPanelOverviewAndTasks(t *testing.T) {
	handler, _ := newPanel(t, "")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview => %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"accounts_total":1`) {
		t.Errorf("overview body = %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/tasks?limit=10", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "cgt-1") {
		t.Errorf("tasks => %d %s", rec.Code, rec.Body.String())
	}
}

// 面板必须把 `paid` 原样三态透出：true / false / null。
// 把"未到终态"画成 false 等于替上游声称"这次不花钱"。
func TestPanelTasksExposePaidTriState(t *testing.T) {
	handler, st := newPanel(t, "")
	paidTrue := true
	_ = st.Put(&store.Entry{ID: "cgt-paid", Model: "minimaxH3", Status: "succeeded",
		Usage: map[string]any{"paid": paidTrue, "credits": 30}})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/tasks?limit=10", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `"paid":true`) {
		t.Errorf("tasks body must carry paid=true, got %s", body)
	}
	if !strings.Contains(body, `"upstream_credits":30`) {
		t.Errorf("tasks body must carry the upstream credits figure (named so nobody reads it as an amount), got %s", body)
	}

	// 另一条未到终态：paid 必须是 null，不能是 false。
	if !strings.Contains(body, `"paid":null`) {
		t.Errorf("an unfinished task must report paid=null (unknown, not free), got %s", body)
	}
}

// 反代后 r.TLS 恒为 nil ⇒ 必须看 X-Forwarded-Proto 才能给会话 Cookie 打上 Secure。
// 没有 Secure 的会话 Cookie 会在任何一次明文回退里被浏览器照发。

func TestLoginCookieIsSecureBehindProxy(t *testing.T) {
	handler, _ := newPanel(t, "s3cret")

	// 1) nginx 转发（https）
	req := httptest.NewRequest(http.MethodPost, "/panel/api/login",
		strings.NewReader(`{"password":"s3cret"}`))
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login => %d", rec.Code)
	}
	if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, "Secure") {
		t.Errorf("behind an https proxy the session cookie must be Secure, got %q", sc)
	}

	// 2) 本机 http 调试：不能带 Secure，否则浏览器根本不存
	req = httptest.NewRequest(http.MethodPost, "/panel/api/login",
		strings.NewReader(`{"password":"s3cret"}`))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if sc := rec.Header().Get("Set-Cookie"); strings.Contains(sc, "Secure") {
		t.Errorf("plain-http debugging must not get a Secure cookie, got %q", sc)
	}
}

// 导入解析要"宽进"：现实中粘过来的东西五花八门，只要含 auth_session 就该收下。
func TestParseAccountLine(t *testing.T) {
	cases := []struct {
		line, wantName, wantCookie string
	}{
		{"acc3----auth_session=abc123def456", "acc3", "auth_session=abc123def456"},
		{"acc4, auth_session=def456abc789", "acc4", "auth_session=def456abc789"},
		{"acc5\tauth_session=ghi789abc123", "acc5", "auth_session=ghi789abc123"},
		{"acc6----0123456789abcdef0123456789abcdef", "acc6", "auth_session=0123456789abcdef0123456789abcdef"},
		{"0123456789abcdef0123456789abcdef", "acc-abcdef", "auth_session=0123456789abcdef0123456789abcdef"},
		{"acc7----theme=dark; auth_session=xyz789abc123; lang=zh", "acc7", "auth_session=xyz789abc123"},
		{`curl 'https://aivideomaker.ai/api/x' -H 'cookie: auth_session=curl123abc456'`, "curl", "auth_session=curl123abc456"},
		{`{"name":"acc8","cookie":"auth_session=json123abc456"}`, "acc8", "auth_session=json123abc456"},
	}
	for _, c := range cases {
		name, cookie, hint := parseAccountLine(c.line)
		if hint != "" {
			t.Errorf("%q => unexpected hint %q", c.line, hint)
			continue
		}
		if name != c.wantName || cookie != c.wantCookie {
			t.Errorf("%q => (%q, %q), want (%q, %q)", c.line, name, cookie, c.wantName, c.wantCookie)
		}
	}

	if _, cookie, hint := parseAccountLine("someone@outlook.com----qsbtxkbm8180886"); cookie != "" || !strings.Contains(hint, "密码登录") {
		t.Errorf("email/password line must be rejected with an explanation, got cookie=%q hint=%q", cookie, hint)
	}
	if _, _, hint := parseAccountLine("label----a@b.com----pw----cid----M.C512_token"); !strings.Contains(hint, "magic-import") {
		t.Errorf("refresh-token line should point at tools/magic-import.py, got %q", hint)
	}
	if _, _, hint := parseAccountLine("just-one-token-x"); hint == "" {
		t.Error("a line without auth_session must be rejected")
	}
}

// 调试页的**花钱闸门**：真跑必须 confirm；计费的单还必须 allow_billed。
// 面板上误点一下就扣 15 积分，这种事不该发生。
func TestDebugSubmitRequiresExplicitConsent(t *testing.T) {
	var submitted int
	handler, _ := newPanelWithDeps(t, "", func(d *Deps) {
		d.PlanRequest = func(body map[string]any) (map[string]any, error) {
			return map[string]any{"effective": map[string]any{"billed": true}}, nil
		}
		d.SubmitRequest = func(ctx context.Context, body map[string]any) (map[string]any, error) {
			submitted++
			return map[string]any{"id": "cgt-test"}, nil
		}
	})

	// 1) 没 confirm ⇒ 400，绝不提交
	code, out := panelDo(t, handler, http.MethodPost, "/panel/api/debug/submit",
		`{"request":{"model":"minimaxH3"},"allow_billed":true}`)
	if code != http.StatusBadRequest {
		t.Errorf("without confirm => %d, want 400", code)
	}
	if submitted != 0 {
		t.Fatal("must not submit without confirm")
	}

	// 2) confirm 了但这单计费、又没 allow_billed ⇒ 拒绝并说明原因
	code, out = panelDo(t, handler, http.MethodPost, "/panel/api/debug/submit",
		`{"request":{"model":"minimaxH3"},"confirm":true}`)
	if code != http.StatusOK || out["ok"] != false {
		t.Errorf("billed without allow_billed => %d %v, want 200 ok=false", code, out)
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "计费") {
		t.Errorf("the refusal must explain that it costs credits, got %q", msg)
	}
	if submitted != 0 {
		t.Fatal("must not submit a billed request without allow_billed")
	}

	// 3) 两道闸都过 ⇒ 真的提交
	code, out = panelDo(t, handler, http.MethodPost, "/panel/api/debug/submit",
		`{"request":{"model":"minimaxH3"},"confirm":true,"allow_billed":true}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("with both flags => %d %v", code, out)
	}
	if submitted != 1 {
		t.Errorf("submitted = %d, want 1", submitted)
	}
}

// dry-run 只翻译、不提交。
func TestDebugPlanNeverSubmits(t *testing.T) {
	var submitted int
	handler, _ := newPanelWithDeps(t, "", func(d *Deps) {
		d.PlanRequest = func(body map[string]any) (map[string]any, error) {
			return map[string]any{"effective": map[string]any{"billed": false, "channel": "web"}}, nil
		}
		d.SubmitRequest = func(ctx context.Context, body map[string]any) (map[string]any, error) {
			submitted++
			return nil, nil
		}
	})
	code, out := panelDo(t, handler, http.MethodPost, "/panel/api/debug/plan",
		`{"request":{"model":"minimaxH3"}}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("plan => %d %v", code, out)
	}
	if submitted != 0 {
		t.Error("dry-run must never submit")
	}
}

// 总览必须回答两个运维问题：今天跑得怎么样、现在能不能接单。
func TestOverviewReportsTodayAndReadiness(t *testing.T) {
	h, st := newPanel(t, "")
	now := time.Now().UnixMilli()
	_ = st.Put(&store.Entry{ID: "ok-1", Model: "minimaxH3", Status: "succeeded", Account: "acc1", CreatedAtMs: now})
	_ = st.Put(&store.Entry{ID: "bad-submit", Model: "minimaxH3", Status: "failed", Account: "acc1",
		CreatedAtMs: now, SubmitErrorCode: "SubmissionFailed",
		SubmitError: "ai.minimaxH3: MINIMAX_H3_FREE_DAILY_LIMIT"})
	_ = st.Put(&store.Entry{ID: "bad-runtime", Model: "minimaxH3", Status: "failed", Account: "acc1", CreatedAtMs: now})

	code, out := panelDo(t, h, http.MethodGet, "/panel/api/overview", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%v)", code, out)
	}
	today, _ := out["today"].(map[string]any)
	if today == nil {
		t.Fatal("overview must carry a `today` block")
	}
	if got := fmt.Sprint(today["succeeded"]); got != "1" {
		t.Errorf("today.succeeded = %s, want 1", got)
	}
	if got := fmt.Sprint(today["failed"]); got != "2" {
		t.Errorf("today.failed = %s, want 2", got)
	}

	// 提交期失败与运行期失败必须分开归因：前者有码，后者只能归 RuntimeFailure。
	byCode, _ := today["fail_by_code"].(map[string]any)
	if byCode["SubmissionFailed"] == nil || byCode["RuntimeFailure"] == nil {
		t.Errorf("fail_by_code must separate submit-time and runtime failures, got %v", byCode)
	}

	// 内部归因（含上游 procedure 名）只出现在面板，供运维直接定位。
	reasons, _ := out["fail_reasons"].([]any)
	if len(reasons) == 0 {
		t.Fatal("fail_reasons must not be empty")
	}
	found := false
	for _, raw := range reasons {
		r, _ := raw.(map[string]any)
		if sample, _ := r["sample"].(string); strings.Contains(sample, "MINIMAX_H3_FREE_DAILY_LIMIT") {
			found = true
		}
	}
	if !found {
		t.Errorf("fail_reasons must carry the internal attribution sample, got %v", reasons)
	}

	// 可用性四分类必须齐全（数值可能为 0，但键要在）。
	for _, key := range []string{"accounts_ready", "accounts_cooling", "accounts_gated", "accounts_disabled"} {
		if _, ok := out[key]; !ok {
			t.Errorf("overview missing %q", key)
		}
	}
}

// 任务列表要能按状态/账号过滤，并带上内部归因（面板排障用）。
func TestTasksFilterAndCarryInternalReason(t *testing.T) {
	h, st := newPanel(t, "")
	now := time.Now().UnixMilli()
	_ = st.Put(&store.Entry{ID: "f-1", Model: "minimaxH3", Status: "failed", Account: "a1",
		CreatedAtMs: now, SubmitErrorCode: "SubmissionFailed",
		SubmitError: "ai.minimaxH3: MINIMAX_H3_FREE_DAILY_LIMIT"})
	_ = st.Put(&store.Entry{ID: "s-1", Model: "minimaxH3", Status: "succeeded", Account: "a2", CreatedAtMs: now})

	code, out := panelDo(t, h, http.MethodGet, "/panel/api/tasks?status=failed", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d (%v)", code, out)
	}
	rows, _ := out["tasks"].([]any)
	if len(rows) != 1 {
		t.Fatalf("status=failed returned %d rows, want 1 (%v)", len(rows), rows)
	}
	row, _ := rows[0].(map[string]any)
	if row["id"] != "f-1" {
		t.Errorf("id = %v, want f-1", row["id"])
	}
	if got, _ := row["submit_error"].(string); !strings.Contains(got, "MINIMAX_H3_FREE_DAILY_LIMIT") {
		t.Errorf("submit_error = %q, must carry the internal attribution", got)
	}

	// 账号过滤
	_, out = panelDo(t, h, http.MethodGet, "/panel/api/tasks?account=a2", "")
	rows, _ = out["tasks"].([]any)
	if len(rows) != 1 {
		t.Fatalf("account=a2 returned %d rows, want 1", len(rows))
	}
	if row, _ := rows[0].(map[string]any); row["id"] != "s-1" {
		t.Errorf("account filter picked %v, want s-1", row["id"])
	}

	// active 是 queued+running 的别名：预置的 cgt-1 是 queued，应命中。
	_, out = panelDo(t, h, http.MethodGet, "/panel/api/tasks?status=active", "")
	rows, _ = out["tasks"].([]any)
	if len(rows) != 1 {
		t.Fatalf("status=active returned %d rows, want 1 (the queued seed task)", len(rows))
	}
}
