// Package pool 是多账号池：一个实例管 N 个账号，按账号各自的并发额度选号。
//
// 为什么必须按账号分片：上游的限制是**按账号**算的（"The premium plan can only run 2 task
// at a time."），而槽位从任务创建一直占到终态。所以并发不是"进程级的一个数字"，而是
// **每个账号一把闸门**；选号时优先挑"此刻真有免费槽位"的号，才能把每个号的额度打满。
//
// 一个实例一个账号（原项目的"账号级语义"）与本池化不冲突：池里只有一个号时，行为与
// 单账号部署逐字一致。
//
// 凭据只落在 `auths/*.json`（0600），**绝不进日志 / 响应体 / 任务表**；任务表里只记
// 账号**名字**。
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/aivideomaker2api/internal/apierr"
	"github.com/rsfree/aivideomaker2api/internal/cookie"
	"github.com/rsfree/aivideomaker2api/internal/jsonx"
	"github.com/rsfree/aivideomaker2api/internal/upstream"
)

// Spec 是一个账号的持久化描述（auths/<name>.json）。
type Spec struct {
	Name   string `json:"name"`
	Cookie string `json:"cookie"`
	// Enabled 用指针：**字段缺失 = 启用**。
	// 用 bool 的话，手写一份没有 enabled 字段的 auths 文件会让账号静默停用 ——
	// 「配了不生效」是本项目最忌讳的形状。
	Enabled     *bool  `json:"enabled,omitempty"`
	Note        string `json:"note,omitempty"`
	MaxInFlight int    `json:"max_in_flight,omitempty"`

	// SourcePath 是这个 spec 实际来自哪个文件（`json:"-"` 不落盘）。
	// 账号名可以和文件名不同（历史遗留：acc1.json 里 name=prod-1），
	// 所以删/改必须按**文件路径**，不能按 `名字+.json` 拼。
	SourcePath string `json:"-"`
}

// IsEnabled 把"字段缺失"解释成启用。
func (s Spec) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// State 是从上游学到的、可持久化的事实。
type State struct {
	Credits    int    `json:"credits"`
	HasCredits bool   `json:"has_credits"`
	Plan       string `json:"plan,omitempty"`
	// UserID 是站点侧的**用户身份**（不是会话）。同一个用户可能有多份 auth_session，
	// 池里必须按它去重 —— 见 dropDuplicateSessions。
	UserID  string `json:"user_id,omitempty"`
	Email   string `json:"email,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Captcha bool   `json:"captcha"`
	// CaptchaUntilMs 是"这个会话还要被闸门挡多久"的**保守估计**（毫秒）。
	//
	// 为什么不用布尔：实测（2026-11-08）闸门是**按会话**的速度型开关，会自己衰减。
	// 用一个到期时间而不是永久标记，才能"立刻绕开、过一会儿自动重新给机会"——
	// 只靠周期探测（默认 300s）会让一个已经恢复的号白等五分钟。
	CaptchaUntilMs int64  `json:"captcha_until_ms,omitempty"`
	Reachable      bool   `json:"reachable"`
	LastProbeMs    int64  `json:"last_probe_ms,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	FailCount      int    `json:"fail_count,omitempty"`
	CooldownUntil  int64  `json:"cooldown_until_ms,omitempty"`
}

// Options 是池的构造参数。
type Options struct {
	AuthDir   string
	StateFile string
	// MaxInFlight > 0 时用它（显式覆盖优先）；= 0 时按账号额度自动
	// （读 ai.queryUserPermission.maxQueueLength：premium 2 / pro 4）。
	MaxInFlight    int
	AcquireTimeout time.Duration
	PollInterval   time.Duration
	WatchTimeout   time.Duration
	ProbeInterval  time.Duration
	// Upstream 是**模板**：每个账号克隆一份并把 Cookie 换掉。
	Upstream upstream.Config
}

// Account 是一个账号 = 一个上游客户端 + 一把闸门 + 一份学到的状态。
type Account struct {
	Name    string
	Note    string
	Enabled bool
	Client  *upstream.Client
	Queue   *upstream.Queue

	mu     sync.RWMutex
	state  State
	cookie string
	// specPath 是这个账号凭据文件的实际路径（可能不等于 AuthDir/<name>.json）。
	specPath string
}

// View 是面板/健康检查看到的账号快照（**不含凭据原文**）。
type View struct {
	Name         string `json:"name"`
	Enabled      bool   `json:"enabled"`
	Note         string `json:"note,omitempty"`
	CookieMasked string `json:"cookie_masked"`
	Credits      *int   `json:"credits"`
	// Email/UserID 只是**展示与去重**用的身份信息，不含任何凭据。
	Email         string         `json:"email,omitempty"`
	UserID        string         `json:"user_id,omitempty"`
	Plan          string         `json:"plan,omitempty"`
	Limit         int            `json:"limit"`
	Captcha       *bool          `json:"captcha"`
	Reachable     bool           `json:"reachable"`
	LastProbe     string         `json:"last_probe,omitempty"`
	LastError     string         `json:"last_error,omitempty"`
	CooldownUntil string         `json:"cooldown_until,omitempty"`
	InUse         int            `json:"in_use"`
	Capacity      int            `json:"capacity"`
	Queue         upstream.Stats `json:"queue"`
}

// Pool 是账号集合。
type Pool struct {
	opts Options
	logf func(string, ...any)

	mu       sync.RWMutex
	accounts map[string]*Account
	order    []string

	stateMu sync.Mutex
	stop    chan struct{}
	closed  chan struct{}
}

// New 从 auth_dir 加载账号并建好客户端。
func New(opts Options, logf func(string, ...any)) (*Pool, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if opts.AuthDir == "" {
		opts.AuthDir = "./auths"
	}
	if opts.AcquireTimeout <= 0 {
		opts.AcquireTimeout = 600 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 10 * time.Second
	}
	if opts.WatchTimeout <= 0 {
		opts.WatchTimeout = 3600 * time.Second
	}
	p := &Pool{
		opts:     opts,
		logf:     logf,
		accounts: map[string]*Account{},
		stop:     make(chan struct{}),
		closed:   make(chan struct{}),
	}
	if err := os.MkdirAll(opts.AuthDir, 0o700); err != nil {
		return nil, fmt.Errorf("create auth dir: %w", err)
	}
	specs, err := loadSpecs(opts.AuthDir)
	if err != nil {
		return nil, err
	}
	for _, spec := range specs {
		if err := p.addLocked(spec); err != nil {
			// 单个账号坏掉不该让整个服务起不来 —— 但要留一条响亮的日志。
			logf("[pool] 账号 %s 加载失败（忽略该号）：%v", spec.Name, err)
		}
	}
	p.loadState()
	p.applyStateCapacity()
	return p, nil
}

// applyStateCapacity 用**上次探测到的**额度给队列定起步容量。
//
// 为什么需要：auto 模式下队列起步是保守的 2 槽（探测不可用时的回落值），真正的额度
// 靠后台探测补齐。但探测可能失败（跨境抖动一次就够），也可能被 `probe_interval=0`
// 关掉 —— 那时账号会一直以 2 槽跑，而 state.json 里明明记着 4：重启后吞吐腰斩，
// 面板上「提交槽位 2」与「额度 4」还自相矛盾（实测截图抓到的就是这个）。
//
// 显式 `max_in_flight` 覆盖时不碰：那是运维的明示决定，优先于历史探测结果。
func (p *Pool) applyStateCapacity() {
	if p.opts.MaxInFlight > 0 {
		return
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, acc := range p.accounts {
		acc.mu.RLock()
		limit := acc.state.Limit
		acc.mu.RUnlock()
		if limit > 0 {
			acc.Queue.RaiseCapacity(limit)
		}
	}
}

func loadSpecs(dir string) ([]Spec, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Spec
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			continue
		}
		var spec Spec
		if err := json.Unmarshal(raw, &spec); err != nil {
			continue
		}
		if spec.Name == "" {
			spec.Name = strings.TrimSuffix(ent.Name(), ".json")
		}
		if strings.TrimSpace(spec.Cookie) == "" {
			continue
		}
		spec.SourcePath = filepath.Join(dir, ent.Name())
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (p *Pool) addLocked(spec Spec) error {
	cookieHeader, notes := cookie.Normalize(spec.Cookie)
	for _, note := range notes {
		p.logf("[pool] 账号 %s 的凭据：%s", spec.Name, note)
	}
	if !cookie.HasAuthSession(cookieHeader) {
		return fmt.Errorf("凭据里没有 %s（至少需要 auth_session=...）", cookie.AuthCookieName)
	}
	cfg := p.opts.Upstream
	cfg.Cookie = cookieHeader
	if cfg.Logf == nil {
		name := spec.Name
		cfg.Logf = func(format string, args ...any) {
			p.logf("[upstream %s] "+format, append([]any{name}, args...)...)
		}
	}
	client, err := upstream.New(cfg)
	if err != nil {
		return err
	}
	capacity := spec.MaxInFlight
	if capacity <= 0 {
		capacity = p.opts.MaxInFlight
	}
	auto := capacity <= 0
	if auto {
		// 探测不可用时的回落值：**不能猜成"没限制"**，那会把上游配额直接撞满。
		capacity = 2
	}
	queue := upstream.NewQueue(client, upstream.QueueOptions{
		Capacity:       capacity,
		AcquireTimeout: p.opts.AcquireTimeout,
		PollInterval:   p.opts.PollInterval,
		WatchTimeout:   p.opts.WatchTimeout,
		Logf:           p.logf,
	})
	acc := &Account{
		Name: spec.Name, Note: spec.Note, Enabled: spec.IsEnabled(),
		Client: client, Queue: queue, cookie: cookieHeader,
		specPath: spec.SourcePath,
	}
	p.accounts[spec.Name] = acc
	p.order = append(p.order, spec.Name)
	sort.Strings(p.order)
	if auto {
		go p.probeCapacity(acc)
	}
	return nil
}

// probeCapacity 后台探账号额度，探到就**补许可**（只放宽）。
//
// 为什么不在这条路径上同步等：新账号的**首条**请求要现建上游，而这个探测是跨境的
// 一次完整往返（新连接还要先做 TLS 握手），会把"受理即返"从毫秒拖到秒级。
func (p *Pool) probeCapacity(acc *Account) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	perm, err := acc.Client.GetPermission(ctx)
	if err != nil {
		p.logf("[pool] 账号 %s 额度探测失败（保持 %d 槽）：%v", acc.Name, acc.Queue.Capacity(), err)
		return
	}
	limit := intOf(perm["maxQueueLength"])
	plan := stringOf(perm["planName"])
	if limit <= 0 {
		p.logf("[pool] 账号 %s 读不到 maxQueueLength（planName=%s）⇒ 保持 %d 槽",
			acc.Name, plan, acc.Queue.Capacity())
		return
	}
	final := acc.Queue.RaiseCapacity(limit)
	acc.mu.Lock()
	acc.state.Limit = limit
	acc.state.Plan = plan
	acc.mu.Unlock()
	p.logf("[pool] 账号 %s 额度探测完成：%d 槽（planName=%s），闸门现为 %d 槽", acc.Name, limit, plan, final)
	p.saveState()
}

// StartBackground 起周期探测（余额 / 闸门 / 套餐）。
func (p *Pool) StartBackground() {
	if p.opts.ProbeInterval <= 0 {
		return
	}
	go func() {
		defer close(p.closed)
		ticker := time.NewTicker(p.opts.ProbeInterval)
		defer ticker.Stop()
		p.ProbeAll()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.ProbeAll()
			}
		}
	}()
}

// Close 停掉后台探测并释放连接。
func (p *Pool) Close() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	p.mu.RLock()
	accounts := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		accounts = append(accounts, a)
	}
	p.mu.RUnlock()
	for _, a := range accounts {
		a.Client.Close()
	}
}

// ProbeAll 采一轮全量账号状态。
func (p *Pool) ProbeAll() {
	p.mu.RLock()
	accounts := make([]*Account, 0, len(p.accounts))
	for _, name := range p.order {
		if a, ok := p.accounts[name]; ok {
			accounts = append(accounts, a)
		}
	}
	p.mu.RUnlock()
	for _, acc := range accounts {
		if !acc.Enabled {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		_ = p.Probe(ctx, acc.Name)
		cancel()
	}
}

// Probe 只读地采一个账号：余额 / 套餐 / 闸门。**不抛异常**（一轮失败不该让整轮上报停摆）。
func (p *Pool) Probe(ctx context.Context, name string) error {
	acc, ok := p.Account(name)
	if !ok {
		return fmt.Errorf("account %q not found", name)
	}
	var firstErr error
	credits, err := acc.Client.GetCredits(ctx)
	acc.mu.Lock()
	if err != nil {
		acc.state.Reachable = false
		acc.state.LastError = classifyError(err)
		firstErr = err
	} else {
		acc.state.Credits = credits
		acc.state.HasCredits = true
		acc.state.Reachable = true
		acc.state.LastError = ""
	}
	acc.state.LastProbeMs = time.Now().UnixMilli()
	acc.mu.Unlock()

	if err != nil {
		p.saveState()
		return firstErr
	}

	if perm, permErr := acc.Client.GetPermission(ctx); permErr == nil {
		acc.mu.Lock()
		acc.state.Limit = intOf(perm["maxQueueLength"])
		acc.state.Plan = stringOf(perm["planName"])
		limit := acc.state.Limit
		acc.mu.Unlock()
		if p.opts.MaxInFlight <= 0 && limit > 0 {
			acc.Queue.RaiseCapacity(limit)
		}
	}
	// 身份：同一个用户可能被登录出多份会话（重新登录一次就是一份新的 auth_session）。
	// 读一次 id/email 存进状态，用于按人维度去重 + 面板展示。
	if user, uErr := acc.Client.GetUser(ctx); uErr == nil {
		acc.mu.Lock()
		if id := jsonx.String(user["id"]); id != "" {
			acc.state.UserID = id
		}
		if em := jsonx.String(user["email"]); em != "" {
			acc.state.Email = em
		}
		acc.mu.Unlock()
	}

	// 闸门是**动态**的（按速率翻转），每轮现问，不能缓存 —— 顺带把"开了多久才衰减"
	// 变成可回看的时间序列。
	acc.Client.InvalidateCaptchaCache()
	if gate, gateErr := acc.Client.NeedsCaptcha(ctx, true); gateErr == nil {
		acc.mu.Lock()
		acc.state.Captcha = gate
		// 探测结果是"此刻"的事实：开着就再挡 2 分钟，关了立刻放行。
		// 不写这一行的话，一次 429 会把号挡到下次探测（默认 300s），即使闸门早就开了。
		if gate {
			acc.state.CaptchaUntilMs = time.Now().Add(captchaCooldown).UnixMilli()
		} else {
			acc.state.CaptchaUntilMs = 0
		}
		acc.mu.Unlock()
	}
	acc.Client.InvalidateCaptchaCache()
	p.saveState()
	p.dropDuplicateSessions(acc)
	return nil
}

// dropDuplicateSessions 把"同一个用户的第二份会话"停掉。
//
// 为什么必须做（实测踩到）：同一个号重新登录一次就是一份新的 auth_session，
// 旧的照样有效。两份都留在池里时：
//   - 槽位算成 8，实际并发全打到同一个账号上（额度是账号的，不是会话的）；
//   - 闸门按会话计，但两边一起用会更快把它推上去；
//   - 面板上看着"号多了"，实际吞吐没变。
//
// 保留谁：余额高的；余额相同按名字序取第一个（确定性，便于解释）。
func (p *Pool) dropDuplicateSessions(keep *Account) {
	keep.mu.RLock()
	uid := keep.state.UserID
	keep.mu.RUnlock()
	if uid == "" {
		return
	}

	p.mu.RLock()
	same := make([]*Account, 0, 2)
	for _, a := range p.accounts {
		a.mu.RLock()
		if a.state.UserID == uid {
			same = append(same, a)
		}
		a.mu.RUnlock()
	}
	p.mu.RUnlock()
	if len(same) < 2 {
		return
	}
	sort.Slice(same, func(i, j int) bool {
		ci, cj := creditsOf(same[i]), creditsOf(same[j])
		if ci != cj {
			return ci > cj
		}
		return same[i].Name < same[j].Name
	})
	winner := same[0]
	for _, a := range same[1:] {
		if !a.Enabled {
			continue
		}
		if err := p.SetEnabled(a.Name, false); err == nil {
			a.mu.Lock()
			a.state.LastError = fmt.Sprintf("与 %s 是同一个用户（重复会话，已自动停用）", winner.Name)
			a.mu.Unlock()
			p.logf("[pool] 账号 %s 与 %s 是同一个用户 ⇒ 停用重复会话", a.Name, winner.Name)
		}
	}
	p.saveState()
}

// ReportFailure 把一个账号的业务失败记进冷却判定。
//
// **只有"账号本身的问题"才计账**：内容审核、参数错误是调用方的输入问题，把它们算进
// 账号失败会让一个正常号被无辜冷却。
func (p *Pool) ReportFailure(name string, err error) {
	if err == nil {
		return
	}
	if upstream.IsContentRejection(err) {
		return
	}
	// 参数 / schema 类失败是**请求形状**的问题（我们的锅或调用方的锅），不是账号的锅。
	// 把它算进账号失败会让一个健康号被无辜冷却 —— 实测：一次 referenceImageUrls 发成
	// null，站点回 invalid_type，三条请求直接把账号冷却了 30s。
	if upstream.IsParameterLike(err) {
		return
	}
	// Turnstile 闸门是**站点级的动态开关**（按速率翻转），不是账号坏了：
	// 把它算成账号失败会导致"闸门开着 → 3 条失败 → 账号冷却 30s → 后续请求报
	// 账号全部冷却"，把关卡问题伪装成账号问题（本轮实测踩到）。
	var upErr *apierr.UpstreamError
	if errors.As(err, &upErr) && (upErr.Code == "CAPTCHA_REQUIRED" || upErr.Code == "CAPTCHA_REJECTED") {
		// 不是账号坏了，但**这个号现在确实过不去** ⇒ 记一个"闸门到期时间"，
		// 让接下来的请求先走别的号；到期时间短（2 分钟）以便闸门一衰减就自动复用。
		if acc, ok := p.Account(name); ok {
			acc.mu.Lock()
			acc.state.Captcha = true
			acc.state.CaptchaUntilMs = time.Now().Add(captchaCooldown).UnixMilli()
			acc.mu.Unlock()
			p.saveState()
		}
		return
	}
	acc, ok := p.Account(name)
	if !ok {
		return
	}
	acc.mu.Lock()
	acc.state.FailCount++
	if acc.state.FailCount >= 3 {
		backoff := time.Duration(1<<minInt(acc.state.FailCount-3, 7)) * 30 * time.Second
		if backoff > 6*time.Hour {
			backoff = 6 * time.Hour
		}
		acc.state.CooldownUntil = time.Now().Add(backoff).UnixMilli()
		p.logf("[pool] 账号 %s 连续失败 %d 次 ⇒ 冷却 %s", name, acc.state.FailCount, backoff)
	}
	acc.state.LastError = classifyError(err)
	acc.mu.Unlock()
	p.saveState()
}

// ReportSuccess 清掉失败计数与冷却。
func (p *Pool) ReportSuccess(name string) {
	acc, ok := p.Account(name)
	if !ok {
		return
	}
	acc.mu.Lock()
	if acc.state.FailCount != 0 || acc.state.CooldownUntil != 0 {
		acc.state.FailCount = 0
		acc.state.CooldownUntil = 0
	}
	// 提交成功是"这个号现在能过闸门"的最强证据 ⇒ 立刻解除闸门标记。
	acc.state.Captcha = false
	acc.state.CaptchaUntilMs = 0
	acc.mu.Unlock()
}

// captchaCooldown 是"撞到闸门后先绕开这个号"的时长。
//
// 保守取 2 分钟：太短会在闸门还没衰减时反复撞（白烧请求），太长会在闸门已经放开后
// 还把它晾着。站点侧实测衰减是 5~10 分钟，但 2 分钟足够躲开连续几条的连环 429。
const captchaCooldown = 2 * time.Minute

// ErrNoAccount 表示池里没有可用账号（未配置 / 全部停用 / 全部冷却）。
type ErrNoAccount struct{ Reason string }

func (e *ErrNoAccount) Error() string { return e.Reason }

// Acquire 选一个账号并**占住它的一个槽位**。
//
// 选号顺序（每一步都是显式的）：
//  1. 跳过停用的、冷却中的；
//  2. 优先"此刻真有免费槽位"的号；
//  3. 负载率低的优先（让每个号同时被填满，而不是把一个号撞满再换）；
//  4. 余额多的优先（免费窗口之外的那部分请求才不会撞上余额不足）。
//
// 所有号都满时**阻塞等待**（不直接失败）—— 上游的语义就是排队，客户端看到的是"还在
// queued"，而不是一个需要自己重试的错误。
func (p *Pool) Acquire(ctx context.Context) (*Account, error) {
	deadline := time.Now().Add(p.opts.AcquireTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	announced := false
	for {
		candidates := p.candidates()
		if len(candidates) == 0 {
			p.mu.RLock()
			total := len(p.accounts)
			accounts := make([]*Account, 0, total)
			for _, a := range p.accounts {
				accounts = append(accounts, a)
			}
			p.mu.RUnlock()
			switch {
			case total == 0:
				return nil, &ErrNoAccount{Reason: "no account is configured (auths/ is empty)"}
			case !anyEnabled(accounts):
				return nil, &ErrNoAccount{Reason: "all accounts are disabled"}
			default:
				// 别只说"都在冷却"：要给**多久之后能再试**，否则运维只能反复重试。
				wait := earliestRecovery(accounts)
				msg := "all enabled accounts are cooling down after repeated failures"
				if wait > 0 {
					msg += fmt.Sprintf(" (earliest recovery in %s)", wait.Round(time.Second))
				}
				return nil, &ErrNoAccount{Reason: msg}
			}
		}
		for _, acc := range candidates {
			if acc.Queue.TryAcquire() {
				return acc, nil
			}
		}
		if !announced {
			announced = true
			p.logf("[pool] 所有账号的槽位都占满 ⇒ 排队等待（最多 %s）", p.opts.AcquireTimeout)
		}
		if time.Now().After(deadline) {
			return nil, upstream.ErrNoSlot
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (p *Pool) candidates() []*Account {
	p.mu.RLock()
	accounts := make([]*Account, 0, len(p.accounts))
	for _, name := range p.order {
		if a, ok := p.accounts[name]; ok {
			accounts = append(accounts, a)
		}
	}
	p.mu.RUnlock()

	now := time.Now().UnixMilli()
	live := accounts[:0]
	for _, acc := range accounts {
		if !acc.Enabled {
			continue
		}
		acc.mu.RLock()
		cooling := acc.state.CooldownUntil > now
		acc.mu.RUnlock()
		if cooling {
			continue
		}
		live = append(live, acc)
	}
	// 闸门感知：有"未被闸门挡"的号时，先把被挡的号摘出去。
	//
	// 实测依据（2026-11-08）：闸门是**会话级**的（同一时刻两个 cookie 在两个出口上
	// 状态完全一致，一个 ON 一个 off），撞上去只会拿到 429、白占一次提交。
	// 全部被挡时**仍然返回它们**：让请求去打一次、拿到可操作的真实原因，
	// 比直接报"没有可用账号"更有用。
	now2 := time.Now().UnixMilli()
	open := live[:0]
	for _, acc := range live {
		acc.mu.RLock()
		gated := acc.state.CaptchaUntilMs > now2
		acc.mu.RUnlock()
		if !gated {
			open = append(open, acc)
		}
	}
	if len(open) > 0 {
		live = open
	}

	sort.SliceStable(live, func(i, j int) bool {
		a, b := live[i], live[j]
		aFree := a.Queue.InUse() < a.Queue.Capacity()
		bFree := b.Queue.InUse() < b.Queue.Capacity()
		if aFree != bFree {
			return aFree
		}
		aRatio := loadRatio(a)
		bRatio := loadRatio(b)
		if aRatio != bRatio {
			return aRatio < bRatio
		}
		return creditsOf(a) > creditsOf(b)
	})
	return live
}

func anyEnabled(accounts []*Account) bool {
	for _, acc := range accounts {
		if acc.Enabled {
			return true
		}
	}
	return false
}

// earliestRecovery 返回"最早一个账号还要冷却多久"（0 = 没有在冷却的）。
func earliestRecovery(accounts []*Account) time.Duration {
	now := time.Now()
	best := time.Duration(0)
	for _, acc := range accounts {
		acc.mu.RLock()
		until := acc.state.CooldownUntil
		acc.mu.RUnlock()
		if until <= now.UnixMilli() {
			return 0
		}
		d := time.Until(time.UnixMilli(until))
		if best == 0 || d < best {
			best = d
		}
	}
	return best
}

func loadRatio(a *Account) float64 {
	capacity := a.Queue.Capacity()
	if capacity <= 0 {
		return 1
	}
	return float64(a.Queue.InUse()) / float64(capacity)
}

func creditsOf(a *Account) int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.state.HasCredits {
		return 0
	}
	return a.state.Credits
}

// Account 按名字取账号。**只给内部用**（凭据在 Account 上，不要在响应里带出去）。
func (p *Pool) Account(name string) (*Account, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	acc, ok := p.accounts[name]
	return acc, ok
}

// Count 是账号数量。
func (p *Pool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// Snapshot 返回全部账号的面板视图。
func (p *Pool) Snapshot() []View {
	p.mu.RLock()
	accounts := make([]*Account, 0, len(p.accounts))
	for _, name := range p.order {
		if a, ok := p.accounts[name]; ok {
			accounts = append(accounts, a)
		}
	}
	p.mu.RUnlock()

	out := make([]View, 0, len(accounts))
	for _, acc := range accounts {
		acc.mu.RLock()
		st := acc.state
		acc.mu.RUnlock()
		v := View{
			Name: acc.Name, Enabled: acc.Enabled, Note: acc.Note,
			CookieMasked: cookie.Mask(acc.cookie),
			Plan:         st.Plan, Limit: st.Limit, Reachable: st.Reachable,
			LastError: st.LastError, InUse: acc.Queue.InUse(),
			Capacity: acc.Queue.Capacity(), Queue: acc.Queue.Stats(),
			Email: st.Email, UserID: st.UserID,
		}
		if st.HasCredits {
			credits := st.Credits
			v.Credits = &credits
		}
		if st.LastProbeMs > 0 {
			v.LastProbe = time.UnixMilli(st.LastProbeMs).Format(time.RFC3339)
			captcha := st.Captcha
			v.Captcha = &captcha
		}
		if st.CooldownUntil > 0 {
			v.CooldownUntil = time.UnixMilli(st.CooldownUntil).Format(time.RFC3339)
		}
		out = append(out, v)
	}
	return out
}

// Add 新增账号（写 auths/<name>.json）。同名拒绝 —— 静默覆盖会丢掉正在跑的任务归属。
func (p *Pool) Add(spec Spec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("account name is required")
	}
	if strings.TrimSpace(spec.Cookie) == "" {
		return fmt.Errorf("cookie is required")
	}
	if spec.Enabled == nil {
		enabled := true
		spec.Enabled = &enabled
	}
	p.mu.Lock()
	if _, exists := p.accounts[spec.Name]; exists {
		p.mu.Unlock()
		return fmt.Errorf("account %q already exists", spec.Name)
	}
	// 🔴 同一份 cookie 换个名字再导入 = 池里两个"号"其实是一个会话。
	//
	// 后果不是重复计数那么轻：两个条目会各自占 4 槽（实际并发翻倍打到同一个会话上），
	// 而且闸门是会话级的 —— 一个撞上，另一个也一起进 Turnstile。这不是"多一个号"，
	// 是"一个号被当成两个用"，必须挡住。
	want, _ := cookie.Normalize(spec.Cookie)
	for name, acc := range p.accounts {
		if acc.cookie != "" && acc.cookie == want {
			p.mu.Unlock()
			return fmt.Errorf("这行的 cookie 与已有账号 %q 完全相同（同一个会话换个名字不能算第二个号）", name)
		}
	}
	p.mu.Unlock()
	if err := p.writeSpec(spec); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addLocked(spec)
}

// Remove 删除账号（同时删掉它的 auths 文件）。
// Remove 删账号。
//
// 🔴 顺序很重要：**先删盘上的凭据文件，成功了才从内存摘掉**。
// 反过来的话，文件删失败（权限/挂载只读）会留下"内存里没了、盘上还在"的僵尸 ——
// 接口报错、面板上号消失，但服务一重启它又回来了（实测在 ai-prod 上踩到，
// 面板上 DELETE 返回 400，而 auths/<name>.json 仍在）。
func (p *Pool) Remove(name string) error {
	p.mu.RLock()
	acc, ok := p.accounts[name]
	p.mu.RUnlock()
	if !ok {
		return fmt.Errorf("account %q not found", name)
	}

	path := acc.specPath
	if path == "" {
		path = filepath.Join(p.opts.AuthDir, name+".json")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除凭据文件失败（账号仍保留在池中）：%w", err)
	}

	p.mu.Lock()
	delete(p.accounts, name)
	p.order = removeString(p.order, name)
	p.mu.Unlock()

	acc.Client.Close()
	p.saveState()
	return nil
}

// SetEnabled 启停账号。
func (p *Pool) SetEnabled(name string, enabled bool) error {
	p.mu.Lock()
	acc, ok := p.accounts[name]
	if ok {
		acc.Enabled = enabled
	}
	p.mu.Unlock()
	if !ok {
		return fmt.Errorf("account %q not found", name)
	}
	specPath := acc.specPath
	if specPath == "" {
		specPath = filepath.Join(p.opts.AuthDir, name+".json")
	}
	spec, err := p.readSpec(specPath, name)
	if err != nil {
		return err
	}
	spec.Enabled = &enabled
	spec.SourcePath = specPath
	return p.writeSpec(spec)
}

func (p *Pool) writeSpec(spec Spec) error {
	raw, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	path := spec.SourcePath
	if path == "" {
		path = filepath.Join(p.opts.AuthDir, spec.Name+".json")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (p *Pool) readSpec(path, fallbackName string) (Spec, error) {
	var spec Spec
	raw, err := os.ReadFile(path)
	if err != nil {
		return spec, err
	}
	err = json.Unmarshal(raw, &spec)
	if spec.Name == "" {
		spec.Name = fallbackName
	}
	spec.SourcePath = path
	return spec, err
}

// ---- 状态持久化 ----

func (p *Pool) loadState() {
	if p.opts.StateFile == "" {
		return
	}
	raw, err := os.ReadFile(p.opts.StateFile)
	if err != nil {
		return
	}
	var stored map[string]State
	if err := json.Unmarshal(raw, &stored); err != nil {
		return
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for name, st := range stored {
		if acc, ok := p.accounts[name]; ok {
			acc.mu.Lock()
			acc.state = st
			acc.mu.Unlock()
		}
	}
}

func (p *Pool) saveState() {
	if p.opts.StateFile == "" {
		return
	}
	p.stateMu.Lock()
	defer p.stateMu.Unlock()

	p.mu.RLock()
	stored := make(map[string]State, len(p.accounts))
	for name, acc := range p.accounts {
		acc.mu.RLock()
		stored[name] = acc.state
		acc.mu.RUnlock()
	}
	p.mu.RUnlock()

	raw, err := json.MarshalIndent(stored, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(p.opts.StateFile)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.opts.StateFile + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.opts.StateFile)
}

// ---- 小工具 ----

func classifyError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len([]rune(msg)) > 300 {
		msg = string([]rune(msg)[:300])
	}
	return msg
}

func intOf(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	case string:
		var n int
		_, _ = fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func removeString(in []string, want string) []string {
	out := in[:0]
	for _, v := range in {
		if v != want {
			out = append(out, v)
		}
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
