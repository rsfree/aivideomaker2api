package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/aivideomaker2api/internal/apierr"
	"github.com/rsfree/aivideomaker2api/internal/cookie"
	"github.com/rsfree/aivideomaker2api/internal/jsonx"
	"github.com/rsfree/aivideomaker2api/internal/sniff"
)

// Terminal 是站点侧的终态词表。
var Terminal = map[string]bool{
	"succeed": true, "success": true, "completed": true,
	"failed": true, "error": true, "cancelled": true, "canceled": true,
}

// Success 是成功终态。
var Success = map[string]bool{"succeed": true, "success": true, "completed": true}

// Client 是网页端会话客户端。Cookie 至少要有 auth_session。
type Client struct {
	cfg    Config
	http   *http.Client
	probe  *http.Client
	mu     sync.Mutex
	userID string

	// visitorID 是**设备标识**，站点的免费额度按它计量（实测：同一账号同一时刻，
	// 换一个 32 位 hex 就能建出任务，用默认值则 429 MINIMAX_H3_FREE_DAILY_LIMIT）。
	// 因此它必须可运行时轮换，且读要加锁 —— 否则并发轮换会写坏正在用的请求体。
	visitorID string

	captchaMu  sync.Mutex
	captchaAt  time.Time
	captchaVal bool

	// 观测用计数（面板/健康检查展示，不加锁读允许轻微抖动）
	requests int64
	errors   int64

	// logf 是可选日志钩子（默认丢弃）。铸造结果这类"低频但关键"的事件走它。
	logf func(format string, args ...any)
}

// New 构造客户端。Cookie 会在构造时兜一次规范化（直接构造的调用方也不会踩坑）。
func New(cfg Config) (*Client, error) {
	cfg = cfg.withDefaults()
	normalized, notes := cookie.Normalize(cfg.Cookie)
	_ = notes // 规范化说明由调用方（配置加载/面板）负责展示
	if normalized == "" {
		return nil, errors.New("upstream client needs a session cookie (at least auth_session=...)")
	}
	cfg.Cookie = normalized
	httpClient := newHTTPClient(cfg)
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Client{
		cfg:       cfg,
		http:      httpClient,
		probe:     newProbeClient(cfg, httpClient),
		userID:    strings.TrimSpace(cfg.UserID),
		visitorID: cfg.VisitorID,
		logf:      logf,
	}, nil
}

// Close 释放连接。
func (c *Client) Close() { c.http.CloseIdleConnections() }

// VisitorID 返回当前设备标识。
//
// 空值回落到构造时的配置（withDefaults 已保证非空），所以调用方永远拿得到可用值。
func (c *Client) VisitorID() string {
	c.mu.Lock()
	v := c.visitorID
	c.mu.Unlock()
	if v != "" {
		return v
	}
	return c.cfg.VisitorID
}

// SetVisitorID 轮换设备标识。
//
// 为什么这是**额度**而不是**风控**上的动作：站点的免费额度记在 visitorId 上，
// 共用同一个默认值等于把整池号的额度合并成一份 —— 一个号跑完，池里所有号一起 429。
// 每个号有自己的标识，额度才真正按号分开。
func (c *Client) SetVisitorID(v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		return
	}
	c.mu.Lock()
	c.visitorID = v
	c.mu.Unlock()
}

// Config 返回（已补默认值的）配置副本。
func (c *Client) Config() Config { return c.cfg }

// BaseURL 是上游根地址。
func (c *Client) BaseURL() string { return c.cfg.BaseURL }

// MinterConfigured 表示接了 Turnstile 铸造服务。
func (c *Client) MinterConfigured() bool {
	return c.cfg.Minter != nil && c.cfg.Minter.Configured()
}

// ---------------------------------------------------------------- tRPC --

type trpcOptions struct {
	method        string
	referer       string
	timeout       time.Duration
	retries       int // GET 用；<0 表示用默认
	writeRetries  int // POST 用（只对"请求没发出去"的传输错误生效）
	returnsTaskID bool
}

func (c *Client) trpc(ctx context.Context, procedure string, input any, opt trpcOptions) (any, error) {
	if opt.method == "" {
		opt.method = http.MethodGet
	}
	if opt.referer == "" {
		opt.referer = Page
	}
	payload, err := jsonx.Marshal(map[string]any{"0": map[string]any{"json": input}})
	if err != nil {
		return nil, apierr.NewUpstream(procedure, "failed to encode tRPC input: "+err.Error(), "", 0)
	}
	if isVoidInput(input) {
		payload, _ = jsonx.Marshal(voidInput)
	}

	query := url.Values{}
	query.Set("batch", "1")
	query.Set("input", payload)
	target := c.cfg.BaseURL + "/api/" + procedure + "?" + query.Encode()

	attempts := 1
	if opt.method == http.MethodPost {
		attempts += maxInt(0, opt.writeRetries)
	} else {
		retries := opt.retries
		if retries < 0 {
			retries = 2
		}
		attempts += maxInt(0, retries)
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		reqCtx := ctx
		cancel := func() {}
		if opt.timeout > 0 {
			reqCtx, cancel = context.WithTimeout(ctx, opt.timeout)
		}
		req, buildErr := http.NewRequestWithContext(reqCtx, opt.method, target, nil)
		if buildErr != nil {
			cancel()
			return nil, apierr.NewUpstream(procedure, "failed to build request: "+buildErr.Error(), "", 0)
		}
		c.setHeaders(req, opt.referer)
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept", "*/*")
		if opt.method == http.MethodPost {
			req.Header.Set("origin", c.cfg.BaseURL)
			body := map[string]any{"0": map[string]any{"json": input}}
			if isVoidInput(input) {
				body = voidInput
			}
			raw, _ := jsonx.Marshal(body)
			req.Body = io.NopCloser(strings.NewReader(raw))
			req.ContentLength = int64(len(raw))
		}

		status, raw, doErr := doAndRead(c.http, req)
		cancel()
		if doErr != nil {
			name, safe := classifyTransport(doErr)
			lastErr = &apierr.UpstreamError{Procedure: procedure, Message: name + ": " + doErr.Error(), Transport: name}
			if attempt+1 < attempts && (opt.method != http.MethodPost || safe) {
				sleepCtx(ctx, 250*time.Millisecond*time.Duration(1<<attempt))
				continue
			}
			return nil, lastErr
		}

		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		var parsed any
		if err := dec.Decode(&parsed); err != nil {
			return nil, apierr.NewUpstream(procedure,
				fmt.Sprintf("non-JSON response (%d) %s", status, clip(string(raw), 200)), "", status)
		}
		items, _ := parsed.([]any)
		if len(items) == 0 {
			return nil, apierr.NewUpstream(procedure,
				fmt.Sprintf("empty tRPC batch response (%d)", status), "", status)
		}
		first, _ := items[0].(map[string]any)
		if errBlock := jsonx.Map(first["error"]); errBlock != nil {
			body := jsonx.Map(errBlock["json"])
			data := jsonx.Map(body["data"])
			msg := jsonx.String(body["message"])
			if msg == "" {
				msg = "unknown tRPC error"
			}
			code := jsonx.String(data["code"])
			httpStatus := status
			if n, ok := jsonx.Int(data["httpStatus"]); ok && n > 0 {
				httpStatus = n
			}
			return nil, apierr.NewUpstream(procedure, msg, code, httpStatus)
		}
		result := jsonx.Map(first["result"])
		data := jsonx.Map(result["data"])
		return data["json"], nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, apierr.NewUpstream(procedure, "transport error (no attempt completed)", "", 0)
}

// doAndRead 发一次请求并**读完**响应体。
//
// 🔴 为什么单独成函数：`resp, err := Do(); cancel(); io.ReadAll(resp.Body)` 是错的 ——
// cancel 会立刻中止请求上下文，慢网络下 ReadAll 直接返回 `context canceled`。
// 本地 httptest 因为响应体早已在缓冲里不会复现，只有真实网络才暴露（本轮实测踩到，
// 症状是"所有只读探测都 context canceled"）。**cancel 必须等 body 读完再调。**
func doAndRead(client *http.Client, req *http.Request) (status int, body []byte, err error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, readErr
}

// isVoidInput 标记"无参数调用"：tRPC 要的是位置式 undefined 编码，不是 `{"0":{"json":null}}`。
func isVoidInput(input any) bool {
	if input == nil {
		return true
	}
	if s, ok := input.(string); ok && s == "void" {
		return true
	}
	return false
}

func (c *Client) setHeaders(req *http.Request, referer string) {
	req.Header.Set("user-agent", UserAgent)
	req.Header.Set("accept-language", "zh-CN,zh;q=0.9")
	req.Header.Set("referer", c.cfg.BaseURL+referer)
	req.Header.Set("cookie", c.cfg.Cookie)
}

// ------------------------------------------------------------- session --

// GetUser 读会话身份。
func (c *Client) GetUser(ctx context.Context) (map[string]any, error) {
	v, err := c.trpc(ctx, "auth.user", nil, trpcOptions{timeout: c.cfg.ProbeTimeout})
	if err != nil {
		return nil, err
	}
	return jsonx.Map(v), nil
}

// GetUserID 读账号 id（列表接口要用）。空 id 不缓存，下一次还会再读。
func (c *Client) GetUserID(ctx context.Context) (string, error) {
	c.mu.Lock()
	cached := c.userID
	c.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	user, err := c.GetUser(ctx)
	if err != nil {
		return "", err
	}
	id := jsonx.String(user["id"])
	if id != "" {
		c.mu.Lock()
		c.userID = id
		c.mu.Unlock()
	}
	return id, nil
}

// GetCredits 权威余额：credits.getCredits → totalRemaining。
func (c *Client) GetCredits(ctx context.Context) (int, error) {
	v, err := c.trpc(ctx, "credits.getCredits", nil, trpcOptions{timeout: c.cfg.ProbeTimeout})
	if err != nil {
		return 0, err
	}
	m := jsonx.Map(v)
	n, ok := jsonx.Int(m["totalRemaining"])
	if !ok {
		return 0, apierr.NewUpstream("credits.getCredits", "response has no totalRemaining", "", 0)
	}
	return n, nil
}

// GetSubscription 读订阅详情（只读）。站点返回里没有套餐名
// （premium/pro 只存在于站点自己的商品描述里），套餐名由 planID 映射得到。
func (c *Client) GetSubscription(ctx context.Context) (map[string]any, error) {
	v, err := c.trpc(ctx, "billing.subscription", nil, trpcOptions{timeout: c.cfg.ProbeTimeout})
	if err != nil {
		return nil, err
	}
	return jsonx.Map(v), nil
}

// GetPermission 读账号权限/额度（ai.queryUserPermission，只读）。
//
// 返回 {hasPermission, planName, maxQueueLength, …} —— 其中 **maxQueueLength 就是该账号的
// 并发额度**（premium 2 / pro 4）。用它给提交闸门定槽位，而不是把并发写死在配置里。
func (c *Client) GetPermission(ctx context.Context) (map[string]any, error) {
	v, err := c.trpc(ctx, "ai.queryUserPermission", map[string]any{}, trpcOptions{timeout: c.cfg.ProbeTimeout})
	if err != nil {
		return nil, err
	}
	return jsonx.Map(v), nil
}

// NeedsCaptcha 判断这个账号现在要不要过 Turnstile（**短期缓存**）。
//
// 它是**动态风控开关，不是账号属性** —— 所以缓存只能很短（默认 5 秒），但对"同一账号
// 连续提交"这种最常见形态，它能省掉每条一次上游往返。
//
// force=true 强制真探：拿到空 taskId 之后应该用它 —— 那正是"缓存说不用过、其实闸门
// 已经翻起"的情形。
func (c *Client) NeedsCaptcha(ctx context.Context, force bool) (bool, error) {
	ttl := c.cfg.CaptchaProbeTTL
	if !force && ttl > 0 {
		c.captchaMu.Lock()
		at, val := c.captchaAt, c.captchaVal
		c.captchaMu.Unlock()
		if !at.IsZero() && time.Since(at) < ttl {
			return val, nil
		}
	}
	userID, err := c.GetUserID(ctx)
	if err != nil {
		return false, err
	}
	v, err := c.trpc(ctx, "model.needsCaptcha", map[string]any{"userId": userID},
		trpcOptions{timeout: c.cfg.ProbeTimeout})
	if err != nil {
		return false, err
	}
	value := false
	if b, ok := v.(bool); ok {
		value = b
	}
	if ttl > 0 {
		c.captchaMu.Lock()
		c.captchaAt, c.captchaVal = time.Now(), value
		c.captchaMu.Unlock()
	}
	return value, nil
}

// InvalidateCaptchaCache 丢掉缓存的闸门结论（面板手动刷新 / 强制真探前用）。
func (c *Client) InvalidateCaptchaCache() {
	c.captchaMu.Lock()
	c.captchaAt = time.Time{}
	c.captchaMu.Unlock()
}

// --------------------------------------------------------------- create --

// Create 创建视频任务，返回站点 taskId —— **带幂等安全边界内的重试**。
//
// 这条线**没有幂等键**，所以重试的许可条件只有一个 —— **上游确定没建任务**：
//
//	queue is full                ✅ 退避     账号并发满，隔几秒就有空位
//	CAPTCHA_REQUIRED (429)       ✅ 换 token 闸门/铸造失败，上游没到建任务
//	空 taskId（站点静默拒）        ✅         站点自己说"不收"
//	连接类错误（连不上/连接超时）    ✅         请求一个字节都没发出去
//	ReadTimeout / WriteTimeout   ⛔         请求可能已被接收 ⇒ 重发＝重复扣费
//	内容审核                      ⛔         重发还是拒
//	参数类 / 其他 4xx-5xx         ⛔         不是抖动
func (c *Client) Create(ctx context.Context, params map[string]any, token string) (string, error) {
	attempts := 1 + maxInt(0, c.cfg.SubmitRetries)
	current := token
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		id, err := c.createOnce(ctx, params, current)
		if err == nil {
			return id, nil
		}
		last = err
		ue, isUpstream := err.(*apierr.UpstreamError)
		if !isUpstream || attempt+1 >= attempts || !retryableSubmitError(ue) {
			return "", err
		}
		// 闸门/铸造失败 ⇒ 手上这个 token 已经废了（可能被用掉或已过期），下一轮重新探测 + 重铸；
		// 其余情况（并发满）保留它，因为上游根本没走到校验 token 那一步。
		if ue.Code == "CAPTCHA_REQUIRED" {
			current = ""
		}
		delay := c.cfg.SubmitRetryBackoff * time.Duration(1<<attempt)
		if !sleepCtx(ctx, delay) {
			return "", err
		}
	}
	if last != nil {
		return "", last
	}
	return "", apierr.NewUpstream("create", "submission failed without a specific error", "", 0)
}

// createOnce 是**单次**提交，不做任何业务级重试（重试在 Create 的壳里）。
//
// 闸门开启且没带 token 时**不提交**，直接返回 CaptchaRequired —— 上游此时会把
// token:null 静默拒掉（返回空串），既不报错也不建任务，所以必须自己把它变成显式失败。
func (c *Client) createOnce(ctx context.Context, params map[string]any, token string) (string, error) {
	procedure := jsonx.String(params["procedure"])
	if procedure == "" {
		procedure = CreateProcedure
	}

	if token == "" {
		gate, err := c.NeedsCaptcha(ctx, false)
		if err == nil && gate {
			token = c.mintToken(ctx)
			if token == "" {
				tried := c.MinterConfigured()
				last := ""
				if tried {
					last = c.cfg.Minter.LastError()
				}
				msg := "account currently requires a Turnstile captcha (model.needsCaptcha=true). " +
					"This is a dynamic, velocity-based gate, not an account property. "
				if tried {
					msg += c.minterFailureHint()
				}
				msg += "Supply a fresh Turnstile token, wait for it to decay, or spread the submissions out."
				return "", apierr.CaptchaRequired(msg, last)
			}
		}
	}

	var tokenValue any
	if token != "" {
		tokenValue = token
	}
	body := map[string]any{
		"content":            params["content"],
		"imageUrl":           params["imageUrl"],
		"lastFrameUrl":       params["lastFrameUrl"],
		"referenceImageUrls": orEmptySlice(params["referenceImageUrls"]),
		"referenceVideoUrl":  params["referenceVideoUrl"],
		"referenceAudioUrls": orEmptySlice(params["referenceAudioUrls"]),
		"aspectRatio":        orDefault(params["aspectRatio"], "16:9"),
		"duration":           orDefault(params["duration"], 5),
		"resolution":         orDefault(params["resolution"], "480p"),
		"tier":               orDefault(params["tier"], "turbo"),
		"promptEnrichment":   params["promptEnrichment"] == true,
		"visitorId":          c.VisitorID(),
		"token":              tokenValue,
	}

	created, err := c.trpc(ctx, procedure, body, trpcOptions{method: http.MethodPost, returnsTaskID: true})
	if err != nil {
		return "", err
	}
	taskID := jsonx.String(created)

	if taskID == "" {
		// 站点用**空串**表示拒绝（不报错）。三种成因，处理完全不同：
		//   ① 闸门翻了（动态、按速率）—— 有铸造服务就换新 token 重试一次；没有就明确 429；
		//   ② 手上这个 token 已过期/已被用过 —— 同上；
		//   ③ 参数/余额问题 —— 重发也是白搭。
		//
		// 所以先**强制**真探一次闸门：不能信 5 秒缓存 —— 空串很可能就是"缓存说不用过、
		// 其实已经翻起"（本轮实测：连跑 7 条后闸门翻起，第 8 条就被静默拒）。
		// 探测失败时按"需要 token"乐观处理，但结论要如实说成闸门，而不是含糊的"参数错"。
		gate, probeErr := c.NeedsCaptcha(ctx, true)
		if probeErr != nil {
			gate = true
		}
		if gate {
			if fresh := c.mintToken(ctx); fresh != "" {
				body["token"] = fresh
				if created, retryErr := c.trpc(ctx, procedure, body,
					trpcOptions{method: http.MethodPost, returnsTaskID: true}); retryErr == nil {
					if id := jsonx.String(created); id != "" {
						return id, nil
					}
				}
				// 🔴 薄荷了**新** token 仍被拒 ⇒ 不是"token 老死"，而是站点/CF 已经不认这条路
				// （本轮实测：CF 下发交互式挑战后，6 个新铸 token 全被静默丢弃）。
				// 这个状态**不可重试**：外层再重试只会继续烧 token，而每次铸造都是一次
				// 站点侧风控信号 —— 一次注定失败的请求烧掉 6 个 token 是在主动抬高被封的概率。
				return "", &apierr.UpstreamError{
					Procedure: procedure, Code: "CAPTCHA_REJECTED", HTTPStatus: 429,
					Message: "the upstream rejected a freshly minted Turnstile token: the site is gating this " +
						"account/session harder than a plain token can satisfy (an interactive challenge). " +
						"Wait for the gate to decay, slow down, or use a different egress/session",
				}
			}
			return "", apierr.CaptchaRequired(
				"the upstream is currently gating submissions behind a Turnstile captcha (a dynamic, "+
					"velocity-based gate, not an account property): the site silently accepted and dropped "+
					"this request (empty task id). Wait for the gate to decay, spread the submissions out, "+
					"or configure a token minter.", "")
		}
		// 闸门是关的却仍被拒 ⇒ 参数或余额问题。不再重试（重发同样的请求还是同样的结果）。
		//
		// 站点对"余额不足"也是**静默拒**（空串），不报错。所以这里把**当前余额**作为
		// 事实带进报文：运维看到 `balance: 1` 就知道付费档没戏，不用去猜参数。
		balance := ""
		if credits, err := c.GetCredits(ctx); err == nil {
			balance = fmt.Sprintf(" (current account balance: %d credits — a paid-tier request may exceed it)",
				credits)
		}
		return "", &apierr.UpstreamError{
			Procedure: procedure,
			// 这个码的存在意义是"报文是我们自己写的、已脱敏，可以原样给调用方"——
			// 否则 clientMessage 会按 code/status 重新生成一句泛化话，把余额这个事实丢掉。
			Code: "SUBMISSION_REJECTED",
			Message: "the request was rejected by the upstream even though the captcha gate is closed " +
				"(invalid parameters, or the paid-tier cost exceeds the account balance)" + balance,
		}
	}
	return taskID, nil
}

// mintToken 向铸造服务要一个 token；未配置或失败都返回空串（调用方据此降级）。
//
// 成功/失败都**打日志**：铸造是"能不能过闸门"的唯一手段，静默失败会让 429 变成
// 一个没有任何线索的黑洞（本轮实测：铸造服务因 CF 下发交互式挑战而失败，
// 而我们的日志里一条都没有，只能去翻 minter 自己的 /healthz）。
func (c *Client) mintToken(ctx context.Context) string {
	if !c.MinterConfigured() {
		return ""
	}
	token, ok := c.cfg.Minter.Mint(ctx)
	if !ok {
		c.logf("turnstile mint failed (last_error=%s) — falling back to waiting for the gate",
			orUnknown(c.cfg.Minter.LastError()))
		return ""
	}
	c.logf("turnstile minted (len=%d, source=%s, last_error=%s)", len(token),
		orUnknown(c.cfg.Minter.LastSource()), orUnknown(c.cfg.Minter.LastError()))
	return token
}

func orUnknown(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// minterFailureHint 把铸造失败的归因翻成**可操作**的修法。
//
// "minter configured but unavailable" 有两种完全不同的成因，修法南辕北辙：
// ① 网络层不可达（last_error 带 unreachable）—— 配置全对也取不到，minter 侧 served 纹丝不动，
// 几乎总是宿主防火墙丢了「桥接容器 → 宿主端口」的包；
// ② 可达但铸造失败 —— 指向 minter 自己（看它的 /healthz.last_failure）。
func (c *Client) minterFailureHint() string {
	last := c.cfg.Minter.LastError()
	if last == "" {
		last = "unknown"
	}
	if strings.Contains(last, "unreachable") {
		return "token minter configured but UNREACHABLE from this container (last_error=" + last + "). " +
			"With a host-network minter this is usually the HOST FIREWALL dropping bridge-to-host traffic: " +
			"allow the container subnet to the minter port (e.g. `ufw allow proto tcp from 172.16.0.0/12 to " +
			"any port 8899`) and verify from inside the container. "
	}
	return "token minter configured but mint failed (last_error=" + last + ") — check the minter's own " +
		"/healthz.last_failure. "
}

// retryableSubmitError 判定建任务失败里"上游确定没接单"的那几种。
//
// 判定顺序有讲究：**审核先判**（它也带 500/429 形态），再判闸门与并发满，最后才看传输层。
// 返回 false 的一律不重试 —— 宁可少救，不可多扣。
func retryableSubmitError(err *apierr.UpstreamError) bool {
	if err == nil {
		return false
	}
	if IsContentRejection(err) {
		return false
	}
	// ⚠️ 只重试"还没有 token"的情形；**已铸过新 token 仍被拒**（CAPTCHA_REJECTED）不重试。
	if err.Code == "CAPTCHA_REQUIRED" {
		return true
	}
	if err.Code == "CAPTCHA_REJECTED" {
		return false
	}
	if err.HTTPStatus == 429 {
		return true
	}
	if strings.Contains(strings.ToLower(err.Error()), "queue is full") {
		return true
	}
	if safeResendTransports[err.Transport] {
		return true
	}
	// 空 taskId 是站点"静默拒"的形态：它自己说没收，重发不会重复建。
	if strings.Contains(err.Error(), "create returned no task id") {
		return true
	}
	return false
}

// contentRejectionMarkers 是内容策略拒绝的实测措辞（上游原文）。
//
// 这类拒绝是"你的内容有问题"，不是"服务故障"：方舟官方对同类错误的契约就是 400
// （*SensitiveContentDetected 系列），而 web 上游把它包成 INTERNAL_SERVER_ERROR status=500
// ⇒ 不识别就会被压成 502，调用方误当服务故障**反复重试**（重试必败，白耗配额）。
//
// ⚠️ 匹配的是**上游原文措辞**，上游改词这里要跟着更新。
var contentRejectionMarkers = []string{
	"prompt or image sensitive",
	"flagged by a content checker",
	"could not be processed because it contained",
	"sensitive",
}

// IsContentRejection 判断这条失败是**内容审核**（非服务故障，重试无用）。
func IsContentRejection(err error) bool {
	if err == nil {
		return false
	}
	blob := strings.ToLower(err.Error())
	for _, marker := range contentRejectionMarkers {
		if strings.Contains(blob, marker) {
			return true
		}
	}
	return false
}

// parameterLikeMarkers 是站点 **输入 schema 校验** 的报错特征（zod）。
//
// 这类失败与账号无关：重发同样的请求还是同样的错，换号也一样。要修的是请求体。
var parameterLikeMarkers = []string{
	"invalid_type", "invalid_parameter", "invalid_enum_value", "invalid_union",
	"expected array", "expected string", "expected number", "expected boolean",
	"too_big", "too_small", "unrecognized_keys",
}

// quotaExhaustedMarkers 是"账号级免费额度用完"的实测措辞。
//
// 与"抖动"的本质区别：它**当天不会自愈**。站点用 429 返回，但冷却 30 秒再试
// 只会再撞一次（生产实测：同一账号连续白撞 4 次，每次都是一次真实的提交机会）。
var quotaExhaustedMarkers = []string{
	"free_daily_limit", // MINIMAX_H3_FREE_DAILY_LIMIT
	"daily limit",      // 文案变体
	"quota_exceeded",   // 通用写法
	"insufficient_quota",
}

// IsQuotaExhausted 判断失败是"这个账号的免费额度用完了"。
//
// 调用方据此做两件事：给该号**长冷却**（不是 30 秒）、并在本次请求内**换号重试**。
// 这类错误明确没接单（空 taskId / 显式 429），重试零副作用 —— 与超时那种
// "可能已经建了任务"的情形完全不同。
func IsQuotaExhausted(err error) bool {
	if err == nil {
		return false
	}
	blob := strings.ToLower(err.Error())
	for _, marker := range quotaExhaustedMarkers {
		if strings.Contains(blob, marker) {
			return true
		}
	}
	return false
}

// queueFullMarkers 是"这个号的并发槽位已满"的实测措辞。
//
// 站点用 **HTTP 500 + INTERNAL_SERVER_ERROR** 返回它（不是 429），原文：
//
//	The queue is full. The pro plan can only run 4 task at a time.
//
// 所以只能按文案识别 —— 不识别就会被当成上游 5xx 故障去冷却账号，
// 而它其实**几十秒到几分钟就自愈**（任务跑完释放槽位）。
var queueFullMarkers = []string{
	"queue is full",
	"can only run",
}

// IsQueueFull 判断失败是"该号的并发槽位满了"。
//
// 与额度耗尽、与真实 5xx 的区别：
//   - 额度耗尽：当天（实测是更久）不会自愈 ⇒ 长冷却 + 换号
//   - 并发满：  槽位一空就好 ⇒ 短绕行 + 换号，且**不计账号故障**
//   - 真实 5xx：连不上/服务端坏了 ⇒ 计故障、指数退避
func IsQueueFull(err error) bool {
	if err == nil {
		return false
	}
	blob := strings.ToLower(err.Error())
	for _, marker := range queueFullMarkers {
		if strings.Contains(blob, marker) {
			return true
		}
	}
	return false
}

// IsParameterLike 判断失败是否来自请求体形状（而不是账号状态）。
func IsParameterLike(err error) bool {
	if err == nil {
		return false
	}
	blob := strings.ToLower(err.Error())
	for _, marker := range parameterLikeMarkers {
		if strings.Contains(blob, marker) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- tasks --

// GetModel 读单条任务。**三种读法里最好的一种**：一次请求、权威、不依赖分页位置。
func (c *Client) GetModel(ctx context.Context, taskID string, timeout time.Duration) (map[string]any, error) {
	v, err := c.trpc(ctx, "model.getModel", map[string]any{"id": taskID}, trpcOptions{timeout: timeout})
	if err != nil {
		return nil, err
	}
	return jsonx.Map(v), nil
}

// List 读账号的生成记录（分页）。
func (c *Client) List(ctx context.Context, offset, limit int, sort string) (map[string]any, error) {
	if limit <= 0 {
		limit = 40
	}
	if sort == "" {
		sort = "desc"
	}
	userID, err := c.GetUserID(ctx)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"0": map[string]any{
		"json": map[string]any{
			"createdAt": nil, "offset": offset, "limit": limit, "createAtSort": sort, "userId": userID,
		},
		"meta": map[string]any{"values": map[string]any{"createdAt": []any{"undefined"}}, "v": 1},
	}}
	raw, _ := jsonx.Marshal(payload)
	query := url.Values{}
	query.Set("batch", "1")
	query.Set("input", raw)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.cfg.BaseURL+"/api/model.listModel?"+query.Encode(), nil)
	if err != nil {
		return nil, apierr.NewUpstream("model.listModel", err.Error(), "", 0)
	}
	c.setHeaders(req, ListPage)
	req.Header.Set("accept", "*/*")
	req.Header.Set("content-type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		name, _ := classifyTransport(err)
		return nil, &apierr.UpstreamError{Procedure: "model.listModel", Message: err.Error(), Transport: name}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var parsed any
	if err := dec.Decode(&parsed); err != nil {
		return nil, apierr.NewUpstream("model.listModel", fmt.Sprintf("non-JSON (%d)", resp.StatusCode), "", resp.StatusCode)
	}
	items, _ := parsed.([]any)
	first, _ := items[0].(map[string]any)
	if errBlock := jsonx.Map(first["error"]); errBlock != nil {
		inner := jsonx.Map(errBlock["json"])
		data := jsonx.Map(inner["data"])
		return nil, apierr.NewUpstream("model.listModel", jsonx.String(inner["message"]),
			jsonx.String(data["code"]), resp.StatusCode)
	}
	result := jsonx.Map(first["result"])
	data := jsonx.Map(result["data"])
	return jsonx.Map(data["json"]), nil
}

// FindInList 在最近一页记录里找任务。
func (c *Client) FindInList(ctx context.Context, taskID string, limit int) map[string]any {
	data, err := c.List(ctx, 0, limit, "desc")
	if err != nil {
		return nil
	}
	for _, raw := range jsonx.Slice(data["models"]) {
		m := jsonx.Map(raw)
		if jsonx.String(m["id"]) == taskID {
			return m
		}
	}
	return nil
}

// MintStatusToken 换状态流令牌（expiresInSec 约 300）。⚠️ 高频调用会 429，别当轮询用。
func (c *Client) MintStatusToken(ctx context.Context, taskID string) (string, error) {
	query := url.Values{}
	query.Set("id", taskID)
	query.Set("visitorId", c.VisitorID())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.cfg.BaseURL+"/api/model-status/token?"+query.Encode(), nil)
	if err != nil {
		return "", apierr.NewUpstream("model-status/token", err.Error(), "", 0)
	}
	c.setHeaders(req, Page)
	req.Header.Set("accept", "*/*")
	req.Header.Set("cache-control", "no-cache")
	req.Header.Set("pragma", "no-cache")
	resp, err := c.probe.Do(req)
	if err != nil {
		name, _ := classifyTransport(err)
		return "", &apierr.UpstreamError{Procedure: "model-status/token", Message: err.Error(), Transport: name}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", apierr.NewUpstream("model-status/token",
			fmt.Sprintf("%d: %s", resp.StatusCode, clip(string(body), 200)), "", resp.StatusCode)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", apierr.NewUpstream("model-status/token", "non-JSON response", "", resp.StatusCode)
	}
	token := jsonx.String(parsed["token"])
	if token == "" {
		return "", apierr.NewUpstream("model-status/token", "no token in response", "", resp.StatusCode)
	}
	return token, nil
}

// GetTask 读完整任务记录，逐级回落。
//
// 三种读法的实测取舍：
//
//	model.getModel      1 次请求  最权威，不需要 userId —— **首选**
//	model.listModel     1 次请求  要 userId，且任务得在请求的那一页里
//	model-status SSE    2 次请求  (a) 上游还没接单时**不发帧也不断开 → 挂死**
//	                             (b) /token 会 429
func (c *Client) GetTask(ctx context.Context, taskID string, timeout time.Duration) (map[string]any, error) {
	if timeout <= 0 {
		timeout = c.pollTimeout()
	}
	rec, err := c.GetModel(ctx, taskID, timeout)
	if err == nil && rec != nil {
		return rec, nil
	}
	if ue, ok := err.(*apierr.UpstreamError); ok && ue.Code == "NOT_FOUND" {
		return nil, err
	}

	if found := c.FindInList(ctx, taskID, 40); found != nil {
		return found, nil
	}

	if token, err := c.MintStatusToken(ctx, taskID); err == nil {
		query := url.Values{}
		query.Set("id", taskID)
		query.Set("token", token)
		query.Set("visitorId", c.VisitorID())
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet,
			c.cfg.BaseURL+"/api/model-status?"+query.Encode(), nil)
		if reqErr == nil {
			c.setHeaders(req, Page)
			req.Header.Set("accept", "text/event-stream")
			req.Header.Set("cache-control", "no-cache")
			req.Header.Set("pragma", "no-cache")
			sseClient := &http.Client{Transport: c.probe.Transport, Timeout: 12 * time.Second}
			if resp, doErr := sseClient.Do(req); doErr == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode < 400 {
					if frame := parseSSE(string(body)); frame != nil {
						if model := jsonx.Map(frame["model"]); model != nil {
							return model, nil
						}
						return frame, nil
					}
				}
			}
		}
	}

	if found := c.FindInList(ctx, taskID, 40); found != nil {
		return found, nil
	}
	return nil, apierr.NewUpstream("model.getModel", "task "+taskID+" not found", "NOT_FOUND", 404)
}

// WaitForTask 轮询到终态。onPoll 每次轮询后被调一次（含还没进终态的那些）。
//
// 🔴 onPoll 抛异常不影响盯梢：埋点坏掉不许把"放槽位"一起赔进去 —— 槽位是闸门，
// 卡住就是整条线停摆（原项目实测过的故障模式）。
func (c *Client) WaitForTask(ctx context.Context, taskID string, timeout, interval time.Duration,
	onPoll func(status string)) (status string, done bool) {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return "", false
		}
		rec, err := c.GetTask(ctx, taskID, c.pollTimeout())
		if err != nil {
			if ue, ok := err.(*apierr.UpstreamError); ok && ue.Code == "NOT_FOUND" {
				return "", false
			}
		}
		cur := strings.ToLower(jsonx.String(rec["taskStatus"]))
		if onPoll != nil {
			onPoll(cur)
		}
		if Terminal[cur] {
			return cur, true
		}
		if !sleepCtx(ctx, interval) {
			return cur, false
		}
	}
	return "", false
}

func (c *Client) pollTimeout() time.Duration {
	// 轮询单次的超时**故意比主调用短**：轮询是高频重复的只读查询，一次卡满主超时会把
	// 10s 的轮询间隔拖成"一分钟才看一次状态"；短超时 + 下一轮重来反而更快看到跃迁。
	t := c.cfg.Timeout
	if t > 20*time.Second {
		t = 20 * time.Second
	}
	if t < time.Second {
		t = time.Second
	}
	return t
}

// --------------------------------------------------------------- upload --

// Budget 是**整个转存阶段**共享的总预算。
//
// 为什么必须有一个"总"预算：一次创建最多有 7 个媒体项要转存（图 4 / 视频 1 / 音频 2），
// 每项各自有超时，但没有总闸时最坏情况是 7 × 单项链路的超时之和 —— 这几分钟全部发生在
// 调用方同步等待的那一个请求里，而且超时之后还可能继续跑完、**并把任务提交出去
// （那一步是计费的）**。
type Budget struct {
	total    time.Duration
	deadline time.Time
}

// NewBudget 建一个总预算。seconds <= 0 时按 1 秒兜底。
func NewBudget(d time.Duration) *Budget {
	if d <= 0 {
		d = time.Second
	}
	return &Budget{total: d, deadline: time.Now().Add(d)}
}

// Remaining 剩余预算。
func (b *Budget) Remaining() time.Duration { return time.Until(b.deadline) }

// Expired 预算是否耗尽。
func (b *Budget) Expired() bool { return b.Remaining() <= 0 }

// Elapsed 已用时长。
func (b *Budget) Elapsed() time.Duration { return b.total - b.Remaining() }

// ExhaustedError 预算耗尽时的错误（点名是哪一步）。
func (b *Budget) ExhaustedError(step string) error {
	return &apierr.UpstreamError{
		Procedure: step,
		Message: fmt.Sprintf("总预算 %s 已耗尽（已用 %.1fs）—— 参考文件太多或太慢；可调大 media.rehost_budget，"+
			"或把链接换成更快/更小的直链", b.total, b.Elapsed().Seconds()),
		HTTPStatus: 504,
	}
}

// stepTimeout 单项网络超时 = min(想要的, 总预算剩余)。总预算已耗尽 ⇒ 立刻 504。
func (b *Budget) stepTimeout(want time.Duration) (time.Duration, error) {
	if b != nil && b.Expired() {
		return 0, b.ExhaustedError("rehost")
	}
	if want <= 0 {
		want = 100 * time.Millisecond
	}
	if want > 300*time.Second {
		want = 300 * time.Second
	}
	if b == nil {
		return want, nil
	}
	if rem := b.Remaining(); rem < want {
		if rem < 100*time.Millisecond {
			return 0, b.ExhaustedError("rehost")
		}
		return rem, nil
	}
	return want, nil
}

// MediaUpload 是转存结果。
type MediaUpload struct {
	PublicURL   string
	ContentType string
	Kind        string
	FileName    string
	Size        int
	Width       int
	Height      int
}

// FetchMedia 把调用方给的**文件链接**取回内存：流式 + 硬字节上限 + 总时长预算。
//
// 🔴 不能用"一把读空"：一个"稳定地慢"（每 50 秒吐几个字节）的链接能一直拖着，而调用方
// 的超时早就到了 —— 调用方看到"超时"，我们却还在跑，甚至接着把任务提交出去（那一步计费）。
func (c *Client) FetchMedia(ctx context.Context, sourceURL string, budget *Budget) ([]byte, error) {
	attempts := 1 + maxInt(0, c.cfg.MediaRetries)
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		buf, err := c.fetchMediaOnce(ctx, sourceURL, budget)
		if err == nil {
			return buf, nil
		}
		last = err
		ue, ok := err.(*apierr.UpstreamError)
		if !ok || ue.Transport == "" || attempt+1 >= attempts {
			return nil, err
		}
		if budget != nil && budget.Expired() {
			return nil, err
		}
		if !sleepCtx(ctx, 500*time.Millisecond*time.Duration(1<<attempt)) {
			return nil, err
		}
	}
	return nil, last
}

func (c *Client) fetchMediaOnce(ctx context.Context, sourceURL string, budget *Budget) ([]byte, error) {
	step, err := budget.stepTimeout(c.cfg.MediaFetchTimeout)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, step)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, apierr.NewUpstream("download", "bad media url: "+err.Error(), "", 0)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		name, _ := classifyTransport(err)
		return nil, &apierr.UpstreamError{
			Procedure: "download", Message: name + ": 取参考文件失败: " + clip(sourceURL, 120),
			Transport: name, HTTPStatus: boolToStatus(name == "ReadTimeout", 504),
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, apierr.NewUpstream("download",
			fmt.Sprintf("%d for %s（参考文件链接不可用）", resp.StatusCode, clip(sourceURL, 120)), "", resp.StatusCode)
	}
	if declared := resp.ContentLength; declared > sniff.MaxUploadBytes {
		return nil, apierr.Param(fmt.Sprintf(
			"参考文件过大：链接声明 %d 字节，超过本站上限 %d 字节（%dMB）",
			declared, sniff.MaxUploadBytes, sniff.MaxUploadBytes/1048576), "input_reference")
	}
	var out bytes.Buffer
	chunk := make([]byte, 64*1024)
	for {
		n, readErr := resp.Body.Read(chunk)
		if n > 0 {
			if out.Len()+n > sniff.MaxUploadBytes {
				return nil, apierr.Param(fmt.Sprintf(
					"参考文件过大：已读 %d 字节，超过本站上限 %d 字节（%dMB）",
					out.Len()+n, sniff.MaxUploadBytes, sniff.MaxUploadBytes/1048576), "input_reference")
			}
			out.Write(chunk[:n])
		}
		if readErr != nil {
			if readErr == io.EOF {
				return out.Bytes(), nil
			}
			name, _ := classifyTransport(readErr)
			return nil, &apierr.UpstreamError{
				Procedure: "download", Message: "取参考文件中断: " + readErr.Error(),
				Transport: name, HTTPStatus: boolToStatus(name == "ReadTimeout", 504),
			}
		}
	}
}

// UploadURL 把一个 http(s) 链接转存到站点 CDN。
func (c *Client) UploadURL(ctx context.Context, sourceURL string, permanent bool, budget *Budget) (MediaUpload, error) {
	buf, err := c.FetchMedia(ctx, sourceURL, budget)
	if err != nil {
		return MediaUpload{}, err
	}
	base := filenameBase(sourceURL)
	return c.UploadBytes(ctx, buf, base, permanent, budget)
}

// UploadBytes 把字节流转存到站点 CDN。
func (c *Client) UploadBytes(ctx context.Context, buf []byte, name string, permanent bool, budget *Budget) (MediaUpload, error) {
	if len(buf) > sniff.MaxUploadBytes {
		return MediaUpload{}, apierr.Param(fmt.Sprintf(
			"参考文件过大：%d 字节，超过单件上限 %d 字节（%dMB）",
			len(buf), sniff.MaxUploadBytes, sniff.MaxUploadBytes/1048576), "input_reference")
	}
	info := sniff.File(buf)
	width, height := 0, 0
	if info.Kind == "image" {
		width, height = sniff.ImageDimensions(buf)
	}
	base := strings.TrimSuffix(name, "."+info.Ext)
	if base == "" {
		base = "file"
	}
	fileName := base + "." + info.Ext

	pre, err := c.presign(ctx, fileName, info.ContentType, len(buf), permanent, budget)
	if err != nil {
		return MediaUpload{}, err
	}
	if maxBytes, ok := jsonx.Int(pre["maxBytes"]); ok && maxBytes > 0 && len(buf) > maxBytes {
		return MediaUpload{}, apierr.Param(fmt.Sprintf(
			"%s too large: %d bytes > maxBytes %d (%dMB for %s)",
			info.Kind, len(buf), maxBytes, maxBytes/1048576, info.ContentType), "input_reference")
	}
	uploadURL := jsonx.String(pre["uploadUrl"])
	if uploadURL == "" {
		return MediaUpload{}, apierr.NewUpstream("uploads.getPresignedUrl", "no uploadUrl in response", "", 0)
	}

	putAttempts := 1 + maxInt(0, c.cfg.MediaRetries)
	for attempt := 0; attempt < putAttempts; attempt++ {
		headers := headerMap(pre["headers"])
		if headers["Content-Type"] == "" && headers["content-type"] == "" {
			headers["Content-Type"] = info.ContentType
		}
		putBudget := 30*time.Second + time.Duration(len(buf)/(1024*1024))*2*time.Second
		step, stepErr := budget.stepTimeout(putBudget)
		if stepErr != nil {
			return MediaUpload{}, stepErr
		}
		reqCtx, cancel := context.WithTimeout(ctx, step)
		req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodPut, uploadURL, bytes.NewReader(buf))
		if reqErr != nil {
			cancel()
			return MediaUpload{}, apierr.NewUpstream("uploads.PUT", reqErr.Error(), "", 0)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		status, body, doErr := doAndRead(c.http, req)
		cancel()
		if doErr != nil {
			name, _ := classifyTransport(doErr)
			if attempt+1 < putAttempts {
				if !sleepCtx(ctx, 500*time.Millisecond*time.Duration(1<<attempt)) {
					return MediaUpload{}, &apierr.UpstreamError{
						Procedure: "uploads.PUT", Message: doErr.Error(), Transport: name}
				}
				continue
			}
			return MediaUpload{}, &apierr.UpstreamError{
				Procedure: "uploads.PUT",
				Message:   fmt.Sprintf("%s: 转存 %d 字节失败", name, len(buf)),
				Transport: name, HTTPStatus: boolToStatus(name == "ReadTimeout", 504),
			}
		}
		if status < 400 {
			return MediaUpload{
				PublicURL:   jsonx.String(pre["publicUrl"]),
				ContentType: info.ContentType,
				Kind:        info.Kind,
				FileName:    fileName,
				Size:        len(buf),
				Width:       width,
				Height:      height,
			}, nil
		}
		// 预签名过期/签名失配 ⇒ 重签再传（同一份字节，不会多存一份）。
		// 预签名 URL 只有 **60 秒** 有效期 ⇒ 不设长退避，直接重签。
		if (status == 400 || status == 403) && attempt+1 < putAttempts {
			fresh, presignErr := c.presign(ctx, fileName, info.ContentType, len(buf), permanent, budget)
			if presignErr != nil {
				return MediaUpload{}, presignErr
			}
			pre = fresh
			if u := jsonx.String(pre["uploadUrl"]); u != "" {
				uploadURL = u
			}
			continue
		}
		return MediaUpload{}, apierr.NewUpstream("uploads.PUT",
			fmt.Sprintf("%d: %s", status, clip(string(body), 300)), "", status)
	}
	return MediaUpload{}, apierr.NewUpstream("uploads.PUT", "upload did not complete", "", 0)
}

func (c *Client) presign(ctx context.Context, fileName, contentType string, size int, permanent bool,
	budget *Budget) (map[string]any, error) {
	step, err := budget.stepTimeout(c.cfg.Timeout)
	if err != nil {
		return nil, err
	}
	v, err := c.trpc(ctx, "uploads.getPresignedUrl", map[string]any{
		"fileName": fileName, "contentType": contentType, "fileSize": size, "permanent": permanent,
	}, trpcOptions{method: http.MethodPost, timeout: step, writeRetries: c.cfg.MediaRetries})
	if err != nil {
		return nil, err
	}
	return jsonx.Map(v), nil
}

// ---------------------------------------------------------------- utils --

// parseSSE 取 SSE 里的第一个 data: {...} 帧。
func parseSSE(text string) map[string]any {
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "data:") {
			continue
		}
		payload := strings.TrimSpace(t[5:])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(payload), &parsed); err == nil {
			return parsed
		}
	}
	return nil
}

func classifyTransport(err error) (string, bool) {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		if opErr.Timeout() {
			return "ConnectTimeout", true
		}
		return "ConnectError", true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "ConnectError", true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "ReadTimeout", false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "ReadTimeout", false
	}
	return "TransportError", false
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func orEmptySlice(v any) []string {
	if s, ok := v.([]string); ok {
		if s == nil {
			// ⚠️ typed-nil 会通过类型断言并原样返回 ⇒ 序列化成 **null**，而站点 schema
			// 要求的是数组（真实报错：invalid_type expected array received null）。
			// 这条只有真实上游才暴露，本地假站点不校验形状（实测踩过）。
			return []string{}
		}
		return s
	}
	out := []string{}
	for _, item := range jsonx.Slice(v) {
		if s := jsonx.String(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func orDefault(v any, fallback any) any {
	if v == nil {
		return fallback
	}
	if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
		return fallback
	}
	return v
}

func headerMap(v any) map[string]string {
	out := map[string]string{}
	for k, val := range jsonx.Map(v) {
		out[k] = jsonx.String(val)
	}
	return out
}

func filenameBase(sourceURL string) string {
	trimmed := sourceURL
	if i := strings.Index(trimmed, "?"); i >= 0 {
		trimmed = trimmed[:i]
	}
	trimmed = strings.TrimRight(trimmed, "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	if decoded, err := url.QueryUnescape(trimmed); err == nil {
		trimmed = decoded
	}
	if trimmed == "" {
		return "file"
	}
	return trimmed
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func boolToStatus(cond bool, status int) int {
	if cond {
		return status
	}
	return 0
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
