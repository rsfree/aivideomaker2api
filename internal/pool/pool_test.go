package pool

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rsfree/aivideomaker2api/internal/upstream"
)

func newTestPool(t *testing.T, dir string, names ...string) *Pool {
	t.Helper()
	for _, name := range names {
		spec := Spec{Name: name, Cookie: "auth_session=test-session-" + name}
		raw, _ := json.Marshal(spec)
		if err := os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o600); err != nil {
			t.Fatalf("write spec: %v", err)
		}
	}
	p, err := New(Options{
		AuthDir: dir,
		// 显式额度 ⇒ 不触发后台额度探测（测试必须零外发）。
		MaxInFlight:    2,
		AcquireTimeout: 300 * time.Millisecond,
		Upstream:       upstream.Config{BaseURL: "http://127.0.0.1:1", TrustEnv: false},
	}, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestPoolLoadsAccountsAndSpreadsLoad(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1", "acc2")
	if p.Count() != 2 {
		t.Fatalf("count = %d, want 2", p.Count())
	}
	ctx := context.Background()
	first, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	second, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if first.Name == second.Name {
		t.Errorf("two acquisitions went to the same account (%s) while both had free slots", first.Name)
	}
	for _, acc := range []*Account{first, second} {
		if acc.Queue.InUse() != 1 {
			t.Errorf("%s in_use = %d, want 1", acc.Name, acc.Queue.InUse())
		}
	}
	first.Queue.Release()
	second.Queue.Release()
}

func TestPoolQueuesWhenEverySlotIsBusy(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "only")
	ctx := context.Background()
	a, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	b, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	// 两个槽位都用完了 ⇒ 第三次必须**阻塞到超时**，而不是直接失败给客户端。
	start := time.Now()
	if _, err := p.Acquire(ctx); err == nil {
		t.Fatal("third acquire must not succeed while all slots are held")
	} else if err != upstream.ErrNoSlot {
		t.Fatalf("want ErrNoSlot, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("it must wait for the acquire timeout, returned after %s", elapsed)
	}
	a.Queue.Release()
	b.Queue.Release()
}

func TestPoolSkipsDisabledAccount(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1", "acc2")
	if err := p.SetEnabled("acc1", false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	acc, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if acc.Name != "acc2" {
		t.Errorf("acquired %s, want the enabled account acc2", acc.Name)
	}
	acc.Queue.Release()
}

func TestPoolDisablingEverythingIsALoudError(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")
	_ = p.SetEnabled("acc1", false)
	_, err := p.Acquire(context.Background())
	if err == nil {
		t.Fatal("no enabled account must not silently succeed")
	}
	if _, ok := err.(*ErrNoAccount); !ok {
		t.Errorf("want ErrNoAccount, got %T %v", err, err)
	}
}

func TestPoolMissingEnabledFieldMeansEnabled(t *testing.T) {
	dir := t.TempDir()
	// 手写一份没有 enabled 字段的文件：必须被当作启用（否则就是「配了不生效」）。
	raw := []byte(`{"name":"acc1","cookie":"auth_session=test-session-acc1"}`)
	if err := os.WriteFile(filepath.Join(dir, "acc1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(Options{
		AuthDir: dir, MaxInFlight: 1, AcquireTimeout: time.Second,
		Upstream: upstream.Config{BaseURL: "http://127.0.0.1:1", TrustEnv: false},
	}, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	if _, err := p.Acquire(context.Background()); err != nil {
		t.Fatalf("an account without an `enabled` field must still be usable: %v", err)
	}
}

func TestPoolAddRejectsDuplicateAndBadCookie(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")
	if err := p.Add(Spec{Name: "acc1", Cookie: "auth_session=x"}); err == nil {
		t.Error("adding a duplicate account must fail (silent overwrite would orphan running tasks)")
	}
	if err := p.Add(Spec{Name: "acc2", Cookie: "not-a-session"}); err == nil {
		t.Error("a credential without auth_session must be rejected at add time")
	}
	if err := p.Add(Spec{Name: "acc3", Cookie: "auth_session=abc"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if p.Count() != 2 {
		t.Errorf("count = %d, want 2", p.Count())
	}
}

func TestPoolSnapshotNeverLeaksCookie(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")
	views := p.Snapshot()
	if len(views) != 1 {
		t.Fatalf("snapshot has %d views", len(views))
	}
	masked := views[0].CookieMasked
	if masked == "auth_session=test-session-acc1" {
		t.Fatal("snapshot must never carry the raw credential")
	}
	if masked == "" {
		t.Error("a masked form is expected (operators need to tell accounts apart)")
	}
}

func TestPoolFailureCooldownOnlyAfterRepeatedFailures(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")
	acc, _ := p.Account("acc1")
	p.ReportFailure("acc1", &upstream.UpstreamError{Procedure: "ai.minimaxH3", Message: "boom", HTTPStatus: 502})
	acc.mu.RLock()
	if acc.state.CooldownUntil != 0 {
		t.Error("a single failure must not cool the account down")
	}
	acc.mu.RUnlock()
	for i := 0; i < 3; i++ {
		p.ReportFailure("acc1", &upstream.UpstreamError{Procedure: "ai.minimaxH3", Message: "boom", HTTPStatus: 502})
	}
	acc.mu.RLock()
	cooling := acc.state.CooldownUntil > time.Now().UnixMilli()
	acc.mu.RUnlock()
	if !cooling {
		t.Error("repeated failures must put the account into cooldown")
	}
	p.ReportSuccess("acc1")
	acc.mu.RLock()
	if acc.state.CooldownUntil != 0 {
		t.Error("a success must clear the cooldown")
	}
	acc.mu.RUnlock()
}

// 参数 / schema 类失败是请求形状的问题，不该把健康号冷却掉。
func TestPoolParameterLikeFailureIsNotAnAccountFault(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")
	acc, _ := p.Account("acc1")
	for i := 0; i < 5; i++ {
		p.ReportFailure("acc1", &upstream.UpstreamError{
			Procedure: "ai.minimaxH3", HTTPStatus: 500,
			Message: `[{"code":"invalid_type","expected":"array","received":"null",` +
				`"path":["referenceImageUrls"],"message":"Expected array, received null"}]`,
		})
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if acc.state.FailCount != 0 || acc.state.CooldownUntil != 0 {
		t.Error("a request-shape error must not cool the account down")
	}
}

// 闸门是站点级动态开关，不是账号坏了 —— 把它算成账号失败会让后续请求报
// "账号全部冷却"，把关卡问题伪装成账号问题。
func TestPoolCaptchaGateIsNotAnAccountFault(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")
	acc, _ := p.Account("acc1")
	for i := 0; i < 5; i++ {
		p.ReportFailure("acc1", &upstream.UpstreamError{
			Procedure: "ai.minimaxH3", Code: "CAPTCHA_REQUIRED", HTTPStatus: 429,
			Message: "the upstream is gating submissions behind a captcha",
		})
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if acc.state.FailCount != 0 || acc.state.CooldownUntil != 0 {
		t.Errorf("captcha gate must not cool the account down (fail_count=%d cooldown=%d)",
			acc.state.FailCount, acc.state.CooldownUntil)
	}
}

func TestPoolCooldownMessageNamesRecoveryTime(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")
	acc, _ := p.Account("acc1")
	// 直接种一个冷却（模拟连续真实失败后的状态）。
	acc.mu.Lock()
	acc.state.CooldownUntil = time.Now().Add(45 * time.Second).UnixMilli()
	acc.mu.Unlock()

	_, err := p.Acquire(context.Background())
	if err == nil {
		t.Fatal("a cooling account must not be handed out")
	}
	var noAccount *ErrNoAccount
	if !errors.As(err, &noAccount) {
		t.Fatalf("err = %T %v, want ErrNoAccount", err, err)
	}
	if !strings.Contains(noAccount.Reason, "cooling") {
		t.Errorf("reason = %q, must say it is cooling", noAccount.Reason)
	}
	if !strings.Contains(noAccount.Reason, "recovery") {
		t.Errorf("reason = %q, must tell the operator how long to wait", noAccount.Reason)
	}
}

func TestPoolContentRejectionIsNotAnAccountFault(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")
	acc, _ := p.Account("acc1")
	for i := 0; i < 5; i++ {
		p.ReportFailure("acc1", &upstream.UpstreamError{
			Procedure: "ai.minimaxH3", Message: "ai.minimaxH3: Prompt or Image Sensitive", HTTPStatus: 500})
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	if acc.state.FailCount != 0 || acc.state.CooldownUntil != 0 {
		t.Error("content rejections are the caller's input problem — they must not cool a healthy account down")
	}
}

// 重启后要用**上次探测到的**额度给队列起步：否则 auto 模式会退回保守的 2 槽，
// 而 state.json 里明明记着 4 —— 吞吐腰斩，面板上「提交槽位」与「额度」自相矛盾。
func TestPoolRestoresCapacityFromState(t *testing.T) {
	dir := t.TempDir()
	spec := Spec{Name: "acc1", Cookie: "auth_session=test-session-acc1"}
	raw, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(dir, "acc1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")
	state := map[string]State{"acc1": {Limit: 4, Plan: "pro", HasCredits: true, Credits: 100}}
	blob, _ := json.Marshal(state)
	if err := os.WriteFile(statePath, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	p, err := New(Options{
		AuthDir: dir, StateFile: statePath,
		// 0 ⇒ auto：不写死额度，走"按账号额度自动"那条路
		MaxInFlight:    0,
		AcquireTimeout: 300 * time.Millisecond,
		Upstream:       upstream.Config{BaseURL: "http://127.0.0.1:1", TrustEnv: false},
	}, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	acc, ok := p.Account("acc1")
	if !ok {
		t.Fatal("account missing")
	}
	if got := acc.Queue.Capacity(); got != 4 {
		t.Errorf("queue capacity = %d after restart, want 4 (the last probed entitlement)", got)
	}
}

// 闸门是**会话级**的（实测：同一时刻两个 cookie 在两个出口上状态完全一致），
// 撞上去只会拿 429、白占一次提交 ⇒ 有未过闸门的号时必须先走它。
func TestPoolSkipsGatedAccountWhenAnotherIsOpen(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1", "acc2")

	acc1, _ := p.Account("acc1")
	acc1.mu.Lock()
	acc1.state.Captcha = true
	acc1.state.CaptchaUntilMs = time.Now().Add(2 * time.Minute).UnixMilli()
	acc1.mu.Unlock()

	got, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer got.Queue.Release()
	if got.Name != "acc2" {
		t.Errorf("Acquire picked %q, want acc2 (acc1 is inside its captcha window)", got.Name)
	}
}

// 但**全部**被挡时不能报"没有可用账号"：让请求真去打一次，拿到可操作的原因。
func TestPoolFallsBackToGatedAccountWhenAllAreGated(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")

	acc1, _ := p.Account("acc1")
	acc1.mu.Lock()
	acc1.state.Captcha = true
	acc1.state.CaptchaUntilMs = time.Now().Add(2 * time.Minute).UnixMilli()
	acc1.mu.Unlock()

	got, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("a fully-gated pool must still hand out an account: %v", err)
	}
	got.Queue.Release()
}

// 撞到闸门要"绕开 2 分钟"，成功后立刻解除；探测到关闭也立刻解除。
func TestPoolCaptchaWindowOpensAgain(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")
	acc, _ := p.Account("acc1")

	p.ReportFailure("acc1", &upstream.UpstreamError{
		Procedure: "ai.minimaxH3", Code: "CAPTCHA_REQUIRED", HTTPStatus: 429, Message: "gate",
	})
	acc.mu.RLock()
	until := acc.state.CaptchaUntilMs
	acc.mu.RUnlock()
	if until <= time.Now().UnixMilli() {
		t.Fatal("a captcha hit must open a (short) skip window")
	}

	p.ReportSuccess("acc1")
	acc.mu.RLock()
	still := acc.state.CaptchaUntilMs
	acc.mu.RUnlock()
	if still != 0 {
		t.Errorf("a successful submit must clear the captcha window, got %d", still)
	}
}

// 同一个会话(同一份 cookie)换个名字再导入，不能算"多一个号"：
// 两个条目会各自占满额度、并同时被闸门带走。必须挡住。
func TestPoolRejectsDuplicateCookie(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")

	enabled := true
	err := p.Add(Spec{Name: "acc1-clone", Cookie: "auth_session=test-session-acc1", Enabled: &enabled})
	if err == nil {
		t.Fatal("adding the same session under a new name must be rejected")
	}
	if !strings.Contains(err.Error(), "同一个会话") {
		t.Errorf("the error should explain why, got %v", err)
	}
}

// 同一个用户的两份会话（重新登录就有了）必须在池里被识别成"一个号"：
// 否则槽位虚高（8 槽其实是一个账号），并发全压到同一份额度上。
func TestProbeDisablesDuplicateSessionsOfSameUser(t *testing.T) {
	mux := http.NewServeMux()
	env := func(w http.ResponseWriter, v any) {
		w.Header().Set("content-type", "application/json")
		raw, _ := json.Marshal([]any{map[string]any{"result": map[string]any{"data": map[string]any{"json": v}}}})
		_, _ = w.Write(raw)
	}
	mux.HandleFunc("/api/auth.user", func(w http.ResponseWriter, r *http.Request) {
		env(w, map[string]any{"id": "u-shared", "email": "same@example.com"})
	})
	mux.HandleFunc("/api/credits.getCredits", func(w http.ResponseWriter, r *http.Request) {
		env(w, map[string]any{"totalRemaining": 100})
	})
	mux.HandleFunc("/api/ai.queryUserPermission", func(w http.ResponseWriter, r *http.Request) {
		env(w, map[string]any{"maxQueueLength": 4, "planName": "pro"})
	})
	mux.HandleFunc("/api/model.needsCaptcha", func(w http.ResponseWriter, r *http.Request) {
		env(w, false)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	for _, name := range []string{"acc1", "acc2"} {
		spec := Spec{Name: name, Cookie: "auth_session=test-session-" + name}
		raw, _ := json.Marshal(spec)
		if err := os.WriteFile(filepath.Join(dir, name+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, err := New(Options{
		AuthDir: dir, MaxInFlight: 2, AcquireTimeout: 300 * time.Millisecond,
		Upstream: upstream.Config{BaseURL: srv.URL, TrustEnv: false, Timeout: 2 * time.Second, ProbeTimeout: time.Second},
	}, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)

	ctx := context.Background()
	if err := p.Probe(ctx, "acc1"); err != nil {
		t.Fatalf("probe acc1: %v", err)
	}
	if err := p.Probe(ctx, "acc2"); err != nil {
		t.Fatalf("probe acc2: %v", err)
	}

	a1, _ := p.Account("acc1")
	a2, _ := p.Account("acc2")
	if !a1.Enabled {
		t.Error("acc1 should stay enabled (same credits ⇒ name order keeps it)")
	}
	if a2.Enabled {
		t.Error("acc2 is the same user as acc1 and must be disabled")
	}
	a2.mu.RLock()
	note := a2.state.LastError
	a2.mu.RUnlock()
	if !strings.Contains(note, "重复会话") {
		t.Errorf("acc2 should explain why it was disabled, got %q", note)
	}
}

// 删号必须"先盘后内存"：文件删不掉时账号要留着。
// 反过来的话会留下"内存里没了、盘上还在"的僵尸 —— 面板上消失、重启又回来。
func TestRemoveKeepsAccountWhenFileCannotBeDeleted(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := p.Remove("acc1"); err == nil {
		t.Fatal("Remove should fail when the credential file cannot be deleted")
	}
	if _, ok := p.Account("acc1"); !ok {
		t.Error("the account must stay in the pool when its file could not be removed")
	}

	_ = os.Chmod(dir, 0o700)
	if err := p.Remove("acc1"); err != nil {
		t.Fatalf("Remove after fixing perms: %v", err)
	}
	if _, ok := p.Account("acc1"); ok {
		t.Error("account should be gone after a successful Remove")
	}
	if _, err := os.Stat(filepath.Join(dir, "acc1.json")); !os.IsNotExist(err) {
		t.Error("credential file should be deleted")
	}
}

// 文件名和账号名可以不一致（历史遗留：acc1.json 里 name=prod-1）。
// 删/改必须按**实际文件路径**，否则会报成功但文件还在 —— 重启就复活。
func TestRemoveAndToggleUseActualSpecPath(t *testing.T) {
	dir := t.TempDir()
	// 文件名 acc1.json，账号名 prod-1
	raw, _ := json.Marshal(Spec{Name: "prod-1", Cookie: "auth_session=test-session-acc1"})
	file := filepath.Join(dir, "acc1.json")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(Options{
		AuthDir: dir, MaxInFlight: 2, AcquireTimeout: 300 * time.Millisecond,
		Upstream: upstream.Config{BaseURL: "http://127.0.0.1:1", TrustEnv: false},
	}, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)

	if err := p.SetEnabled("prod-1", false); err != nil {
		t.Fatalf("toggle by account name should write the real file: %v", err)
	}
	var spec Spec
	b, _ := os.ReadFile(file)
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.IsEnabled() {
		t.Error("toggle did not persist to acc1.json")
	}

	if err := p.Remove("prod-1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("the real credential file (acc1.json) must be deleted, not a guessed prod-1.json")
	}
}

// 额度耗尽必须换来"长冷却"，而不是 30 秒后又回去撞同一堵墙。
// 解冻必须清掉**所有**不健康标记并换标识：只清冷却会立刻再撞同一堵墙
// （额度记在标识上，旧标识已经用尽）。
func TestThawClearsCooldownAndRotatesVisitor(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")
	defer p.Close()

	p.ReportFailure("acc1", errors.New("ai.minimaxH3: MINIMAX_H3_FREE_DAILY_LIMIT"))
	acc, ok := p.Account("acc1")
	if !ok {
		t.Fatal("account missing")
	}
	acc.mu.Lock()
	if acc.state.CooldownUntil == 0 {
		acc.mu.Unlock()
		t.Fatal("setup: a quota failure must put the account into cooldown")
	}
	acc.state.FailCount = 3
	acc.state.Captcha = true
	acc.state.CaptchaUntilMs = time.Now().Add(time.Hour).UnixMilli()
	acc.mu.Unlock()

	id, err := p.Thaw("acc1")
	if err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if len(id) != 32 {
		t.Errorf("visitor id = %q (len %d), want 32 hex chars", id, len(id))
	}
	for _, v := range p.Snapshot() {
		if v.Name != "acc1" {
			continue
		}
		if v.CooldownUntilMs != 0 {
			t.Errorf("cooldown = %d, want cleared", v.CooldownUntilMs)
		}
		if v.CaptchaUntilMs != 0 {
			t.Errorf("captcha_until = %d, want cleared", v.CaptchaUntilMs)
		}
		if v.FailCount != 0 {
			t.Errorf("fail_count = %d, want 0", v.FailCount)
		}
		if v.VisitorID != id {
			t.Errorf("visitor_id = %q, want %q", v.VisitorID, id)
		}
		if !v.Enabled {
			t.Error("thaw must not disable the account")
		}
	}
}

// 轮换设备标识必须**落盘**，否则重启后又回到共享默认值，整池额度再次合并。
func TestRotateVisitorIDPersistsToSpecFile(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")
	defer p.Close()

	id, err := p.RotateVisitorID("acc1")
	if err != nil {
		t.Fatalf("RotateVisitorID: %v", err)
	}
	if len(id) != 32 {
		t.Errorf("visitor id = %q (len %d), want 32 hex chars", id, len(id))
	}

	raw, err := os.ReadFile(filepath.Join(dir, "acc1.json"))
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("spec is not JSON: %v", err)
	}
	if spec["visitor_id"] != id {
		t.Errorf("spec visitor_id = %v, want %q", spec["visitor_id"], id)
	}
	// 其它字段不能被抹掉：这是"读改写整份文件"的回归点。
	if spec["cookie"] != "auth_session=test-session-acc1" {
		t.Errorf("cookie was clobbered: %v", spec["cookie"])
	}

	// 轮换后该号必须立刻可复用（新标识 = 新额度），不能被旧的 2h 冷却按住。
	for _, v := range p.Snapshot() {
		if v.Name != "acc1" {
			continue
		}
		if v.CooldownUntilMs > time.Now().Add(time.Minute).UnixMilli() {
			t.Errorf("cooldown=%d, want cleared after rotation", v.CooldownUntilMs)
		}
		if v.VisitorID != id {
			t.Errorf("snapshot visitor_id = %q, want %q", v.VisitorID, id)
		}
	}
}

func TestQuotaExhaustedGetsLongCooldown(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "acc1")

	quotaErr := errors.New("ai.minimaxH3: MINIMAX_H3_FREE_DAILY_LIMIT")
	p.ReportFailure("acc1", quotaErr)

	acc, ok := p.Account("acc1")
	if !ok {
		t.Fatal("account missing")
	}
	acc.mu.RLock()
	until, last, hits := acc.state.CooldownUntil, acc.state.LastError, acc.state.QuotaHits
	acc.mu.RUnlock()

	if until <= time.Now().Add(time.Hour).UnixMilli() {
		t.Errorf("cooldown until=%d, want at least 1h ahead", until)
	}
	if hits != 1 {
		t.Errorf("quota_hits = %d, want 1", hits)
	}
	if !strings.Contains(last, "额度") {
		t.Errorf("last_error = %q, should explain quota exhaustion", last)
	}

	// 再撞一次要延长（翻倍），而不是重置成同一档。
	before := until
	p.ReportFailure("acc1", quotaErr)
	acc.mu.RLock()
	after := acc.state.CooldownUntil
	acc.mu.RUnlock()
	if after <= before {
		t.Errorf("second hit must extend the cooldown: %d -> %d", before, after)
	}
}

// 额度冷却的边界：2h 起步 / 翻倍 / 封顶 UTC 次日零点 / 保底 15 分钟。
func TestQuotaCooldownBounds(t *testing.T) {
	day := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	if got := quotaCooldown(1, day).Sub(day); got != 2*time.Hour {
		t.Errorf("first hit = %v, want 2h", got)
	}
	if got := quotaCooldown(3, day).Sub(day); got != 8*time.Hour {
		t.Errorf("third hit = %v, want 8h", got)
	}
	// 20:00 UTC：2h 翻倍到 4h 会越过零点 ⇒ 封顶在次日 0 点。
	late := time.Date(2026, 11, 19, 20, 0, 0, 0, time.UTC)
	if got := quotaCooldown(2, late); !got.Equal(time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("near midnight = %v, want UTC next midnight", got)
	}
	// 23:55 UTC：零点只剩 5 分钟 ⇒ 保底 15 分钟。
	last := time.Date(2026, 11, 19, 23, 55, 0, 0, time.UTC)
	if got := quotaCooldown(1, last).Sub(last); got < 15*time.Minute {
		t.Errorf("floor violated: %v", got)
	}
}

// 停用号不能让"最早恢复时间"归零 —— 否则 429 永远给不出 Retry-After。
func TestEarliestRecoveryIgnoresDisabledAccounts(t *testing.T) {
	dir := t.TempDir()
	p := newTestPool(t, dir, "active", "disabled-one")

	// 停用一个号（它的 CooldownUntil 恒为 0）。
	disabled := false
	if err := p.SetEnabled("disabled-one", disabled); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	// 让唯一的启用号进入 2h 额度冷却。
	p.ReportFailure("active", errors.New("ai.minimaxH3: MINIMAX_H3_FREE_DAILY_LIMIT"))

	wait := p.EarliestRecovery()
	if wait <= time.Hour {
		t.Errorf("EarliestRecovery = %v, want the active account's cooldown (>=1h); "+
			"a disabled account must not zero it out", wait)
	}
}

// 探测成功不能抹掉"冷却原因"：余额可读 ≠ 免费额度没用完。
func TestProbeKeepsCooldownReason(t *testing.T) {
	mux := http.NewServeMux()
	env := func(w http.ResponseWriter, v any) {
		w.Header().Set("content-type", "application/json")
		raw, _ := json.Marshal([]any{map[string]any{"result": map[string]any{"data": map[string]any{"json": v}}}})
		_, _ = w.Write(raw)
	}
	mux.HandleFunc("/api/auth.user", func(w http.ResponseWriter, r *http.Request) {
		env(w, map[string]any{"id": "u1", "email": "a@example.com"})
	})
	mux.HandleFunc("/api/credits.getCredits", func(w http.ResponseWriter, r *http.Request) {
		env(w, map[string]any{"totalRemaining": 4554})
	})
	mux.HandleFunc("/api/ai.queryUserPermission", func(w http.ResponseWriter, r *http.Request) {
		env(w, map[string]any{"maxQueueLength": 4, "planName": "pro"})
	})
	mux.HandleFunc("/api/model.needsCaptcha", func(w http.ResponseWriter, r *http.Request) {
		env(w, false)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	raw, _ := json.Marshal(Spec{Name: "acc1", Cookie: "auth_session=test-session-acc1"})
	if err := os.WriteFile(filepath.Join(dir, "acc1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := New(Options{
		AuthDir: dir, MaxInFlight: 2, AcquireTimeout: 300 * time.Millisecond,
		Upstream: upstream.Config{BaseURL: srv.URL, TrustEnv: false,
			Timeout: 2 * time.Second, ProbeTimeout: time.Second},
	}, t.Logf)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(p.Close)

	p.ReportFailure("acc1", errors.New("ai.minimaxH3: MINIMAX_H3_FREE_DAILY_LIMIT"))
	if err := p.Probe(context.Background(), "acc1"); err != nil {
		t.Fatalf("probe: %v", err)
	}

	for _, v := range p.Snapshot() {
		if v.Name != "acc1" {
			continue
		}
		if !strings.Contains(v.LastError, "额度") {
			t.Errorf("last_error = %q, want the cooldown reason to survive a successful probe", v.LastError)
		}
		if v.CooldownUntilMs <= time.Now().UnixMilli() {
			t.Error("cooldown should still be active")
		}
	}
}
