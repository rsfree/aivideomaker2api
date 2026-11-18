// Package panel 是内置运维面板（账号池 / 任务 / 健康度）。
//
// 资源经 go:embed 打进二进制（随服务部署，无外部构建步骤）：
//
//	index.html  页面骨架
//	app.js      全部前端逻辑（独立文件而非内联，为了启用无需 unsafe-inline 的严格 CSP）
//	style.css   样式（含 prefers-color-scheme 暗色与 prefers-reduced-motion 降级）
//
// 安全头对"面板页面与全部 /panel/api/* 响应"统一生效：CSP 限制脚本只能来自本服务，
// 禁止被 iframe 嵌套（防点击劫持），禁 MIME 嗅探，并声明不泄露 Referer 出去。
//
// 鉴权：`panel.password`（留空则回落到 api_key；两者都空 = 不校验，仅本机自用）。
// 会话是一个 HMAC 签名的到期时间戳 —— **服务端不存 session**，所以重启即失效（可接受，
// 且比"内存里一坨 session 表"更难出错）。密钥在启动时随机生成，不落盘。
package panel

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "embed"

	"github.com/jushenzhidao/aivideomaker2api/internal/debughist"
	"github.com/jushenzhidao/aivideomaker2api/internal/pool"
	"github.com/jushenzhidao/aivideomaker2api/internal/store"
)

//go:embed index.html
var indexHTML []byte

//go:embed app.js
var appJS []byte

//go:embed style.css
var styleCSS []byte

// csp 是严格内容安全策略（无需 unsafe-inline）：
//
//	default-src 'none'        默认全禁，逐个开口
//	script-src 'self'         只跑同源脚本（app.js）；页面无内联事件处理器/内联脚本
//	style-src 'self'          样式只在 style.css 里（无 style="" 属性）
//	connect-src 'self'        前端 fetch 只能打本服务
//	img-src 'self' data:      图标/内联图
//	frame-ancestors 'none'    禁止被任何站点 iframe 嵌套（点击劫持）
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"connect-src 'self'; img-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

const sessionCookie = "aivm_panel"

// Deps 是面板需要的只读依赖。
type Deps struct {
	Pool          *pool.Pool
	Store         *store.Store
	ServiceName   string
	Version       string
	AuthMode      string
	BaseURL       string
	MediaEnabled  bool
	Started       time.Time
	ProbeInterval time.Duration
	// OfficialEnabled / OfficialAccount 让面板只读地展示官方 REST 通路的状态。
	// 🔴 刻意**不传 key 原文**：面板只显示"有没有配"和余额/模型，凭据永不出现在响应里。
	OfficialEnabled bool
	OfficialAccount func(ctx context.Context) (map[string]any, error)

	// PlanRequest 只做**纯翻译**（不联网、不花钱）：给定方舟请求体，
	// 返回 {effective, warnings, unsupported, incompatible}。
	// 这是调试页的核心 —— 先看清楚"会走哪条路、要不要钱"，再决定发不发。
	PlanRequest func(body map[string]any) (map[string]any, error)
	// SubmitRequest 真正创建一条任务（可能计费）。由 server 侧实现，
	// 复用与公网 API **完全相同**的那条链路，避免"调试页能跑、正式接口跑不通"。
	SubmitRequest func(ctx context.Context, body map[string]any) (map[string]any, error)
	// DebugHistory 记录调试页的 dry-run / 真跑（可空 = 不记录）。
	DebugHistory *debughist.Recorder
}

type panel struct {
	deps     Deps
	password string
	key      []byte
}

// New 返回挂在 `/panel` 下的 http.Handler。
func New(deps Deps, password string) http.Handler {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		// 极端情况下的兜底：仍能工作，只是会话密钥可预测（面板不是对外服务）。
		key = []byte(fmt.Sprintf("%d", time.Now().UnixNano()))
	}
	return &panel{deps: deps, password: strings.TrimSpace(password), key: key}
}

func (p *panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	switch {
	case r.URL.Path == "/panel" || r.URL.Path == "/panel/":
		p.serveStatic(w, r, "text/html; charset=utf-8", indexHTML)
	case r.URL.Path == "/panel/app.js":
		p.serveStatic(w, r, "application/javascript; charset=utf-8", appJS)
	case r.URL.Path == "/panel/style.css":
		p.serveStatic(w, r, "text/css; charset=utf-8", styleCSS)
	case strings.HasPrefix(r.URL.Path, "/panel/api/"):
		p.serveAPI(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *panel) serveAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/panel/api/")

	// 未设密码 = 不校验（本机自用）；设了就必须带有效会话。
	authed := p.authed(r)
	switch {
	case path == "session":
		writeJSON(w, http.StatusOK, map[string]any{
			"authed": authed, "requires_password": p.password != "",
		})
		return
	case path == "login":
		p.handleLogin(w, r)
		return
	case path == "logout":
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: "", Path: "/panel", MaxAge: -1,
			HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if !authed {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}

	switch {
	case path == "overview" && r.Method == http.MethodGet:
		p.handleOverview(w)
	case path == "accounts" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"accounts": p.deps.Pool.Snapshot()})
	case path == "accounts" && r.Method == http.MethodPost:
		p.handleAddAccount(w, r)
	case path == "accounts/import" && r.Method == http.MethodPost:
		p.handleImportAccounts(w, r)
	case strings.HasPrefix(path, "accounts/"):
		p.handleAccountAction(w, r, strings.TrimPrefix(path, "accounts/"))
	case path == "debug/history" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"history": p.deps.DebugHistory.List(50)})
	case path == "debug/plan" && r.Method == http.MethodPost:
		p.handleDebugPlan(w, r)
	case path == "debug/submit" && r.Method == http.MethodPost:
		p.handleDebugSubmit(w, r)
	case path == "official" && r.Method == http.MethodGet:
		p.handleOfficial(w, r)
	case path == "config" && r.Method == http.MethodGet:
		p.handleConfig(w)
	case path == "tasks" && r.Method == http.MethodGet:
		p.handleTasks(w, r)
	default:
		http.NotFound(w, r)
	}
}

// requestIsHTTPS 判断"浏览器到本服务这一跳"是不是 HTTPS。
//
// 直连时看 r.TLS；反代后 nginx 已经终止 TLS，只能看 X-Forwarded-Proto。
func requestIsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func (p *panel) handleLogin(w http.ResponseWriter, r *http.Request) {
	if p.password == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if !hmac.Equal([]byte(body.Password), []byte(p.password)) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "密码不正确"})
		return
	}
	expires := time.Now().Add(12 * time.Hour).Unix()
	value := fmt.Sprintf("%d.%s", expires, p.sign(expires))
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/panel",
		Expires: time.Unix(expires, 0), HttpOnly: true, SameSite: http.SameSiteLaxMode,
		// 🔴 只有 HTTPS 才带 Secure —— 但**必须**在 HTTPS 下带上：
		// 没有 Secure 的会话 Cookie 在任何一次明文回退里都会被浏览器照发。
		// 反代后面 r.TLS 恒为 nil（nginx 已经终止 TLS），所以还要看 X-Forwarded-Proto；
		// 两者都判才能既覆盖直连也覆盖 nginx，还不破坏本机 http://127.0.0.1 调试。
		Secure: requestIsHTTPS(r),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *panel) handleOverview(w http.ResponseWriter) {
	views := p.deps.Pool.Snapshot()
	inUse, capacity, enabled := 0, 0, 0
	for _, v := range views {
		inUse += v.InUse
		capacity += v.Capacity
		if v.Enabled {
			enabled++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":          p.deps.ServiceName,
		"version":          p.deps.Version,
		"auth":             p.deps.AuthMode,
		"base_url":         p.deps.BaseURL,
		"uptime_sec":       int(time.Since(p.deps.Started).Seconds()),
		"accounts_total":   len(views),
		"accounts_enabled": enabled,
		"slots_in_use":     inUse,
		"slots_capacity":   capacity,
		"tasks":            p.deps.Store.Count(""),
		"tasks_charged":    p.deps.Store.CountCharged(),
		"task_store":       p.deps.Store.Describe(),
		"media_enabled":    p.deps.MediaEnabled,
		"probe_interval":   p.deps.ProbeInterval.String(),
		"now":              time.Now().Format(time.RFC3339),
	})
}

func (p *panel) handleAddAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Cookie string `json:"cookie"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "账号名不能为空"})
		return
	}
	enabled := true
	spec := pool.Spec{
		Name: strings.TrimSpace(body.Name), Cookie: body.Cookie,
		Note: body.Note, Enabled: &enabled,
	}
	if err := p.deps.Pool.Add(spec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 新号先探一次，面板上立刻能看到余额/额度（探不到也不影响账号已入库）。
	go p.deps.Pool.ProbeAll()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleImportAccounts 批量导入账号。
//
// 输入是一段文本，**一行一个号**，分隔符容忍 `----` / `,` / 制表符 / 空白：
//
//	acc3----auth_session=xxxxx
//	acc4, auth_session=yyyyy
//
// 为什么要有这个接口：本站**没有密码登录**（`auth.loginWithPassword` 对任何凭据都返回
// NOT_FOUND，连站点自己的前端按钮都是坏的），只剩 Google OAuth 与**邮箱魔法链接**两条路。
// 所以"号"只能靠浏览器登录一次拿到 `auth_session` cookie 再灌进来 —— 一次灌几十个时，
// 逐个点面板太蠢。
// auth_session 的值是一串不可预测的随机字符（实测 40 位）。
// 只认"像 token 的字符"，避免把 `auth_session=` 后面那段标记文本（如 `;` 或引号）吃进来。
var authSessionRe = regexp.MustCompile(`auth_session=([A-Za-z0-9._~+/=-]{8,})`)

func (p *panel) handleImportAccounts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}

	// 一次导入最多 1MB 文本：正常几十行号也就几十 KB，超过基本是贴错了东西。
	if len(body.Text) > 1<<20 {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "导入文本过大（上限 1MB）"})
		return
	}

	type result struct {
		Line  string `json:"line"`
		Name  string `json:"name,omitempty"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	var results []result
	added := 0

	for _, raw := range strings.Split(body.Text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, cookie, hint := parseAccountLine(line)
		res := result{Line: line, Name: name}
		switch {
		case hint != "":
			res.Error = hint
		case name == "" || cookie == "":
			res.Error = "分成两段：`账号名` 与 `auth_session=...`（分隔符可用 ---- 、逗号或制表符）"
		default:
			enabled := true
			if err := p.deps.Pool.Add(pool.Spec{Name: name, Cookie: cookie, Enabled: &enabled}); err != nil {
				res.Error = err.Error()
			} else {
				res.OK = true
				added++
			}
		}
		results = append(results, res)
	}

	if added > 0 {
		// 新号先探一次，面板上立刻能看到余额/额度/闸门。
		go p.deps.Pool.ProbeAll()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added, "results": results})
}

// parseAccountLine 把一行拆成 (名字, cookie)；hint 非空表示"这行不该被当成 cookie"。
//
// 导入要"宽进"——实际粘贴过来的东西五花八门：整段 curl、浏览器复制的 `a=1; b=2`、
// 只有一串 session 值、或者别的实例导出的 JSON。只要一行里**能找到 auth_session**，
// 就应该收下；找不到才报错，并且报**为什么**。
//
// 支持的写法（名字都可省略）：
//
//	acc3----auth_session=xxx          acc3, auth_session=xxx
//	acc3\tauth_session=xxx            acc3----a=1; auth_session=xxx; b=2
//	acc3----<40 位裸 session>          auth_session=xxx
//	{"name":"acc3","cookie":"auth_session=xxx"}
//	curl ... -H 'cookie: auth_session=xxx' ...
func parseAccountLine(line string) (name, cookie, hint string) {
	// 1) 别的实例导出的 JSON（一行一个对象）
	if strings.HasPrefix(strings.TrimSpace(line), "{") {
		var obj struct {
			Name   string `json:"name"`
			Cookie string `json:"cookie"`
		}
		if err := json.Unmarshal([]byte(line), &obj); err == nil && obj.Cookie != "" {
			if m := authSessionRe.FindStringSubmatch(obj.Cookie); m != nil {
				n := strings.TrimSpace(obj.Name)
				if n == "" {
					n = deriveName(m[1])
				}
				return n, "auth_session=" + m[1], ""
			}
		}
		return "", "", `JSON 里没有 auth_session（需要 {"name":"…","cookie":"auth_session=…"}）`
	}

	// 2) 从任意位置抽出 auth_session（整段 curl / 多条 cookie 都能命中）
	if m := authSessionRe.FindStringSubmatch(line); m != nil {
		token := m[1]
		prefix := line[:strings.Index(line, m[0])]
		// 名字只取**分隔符之前**那一段：`acc7----theme=dark; auth_session=…` 的名字是 acc7，
		// 不是 `acc7----theme=dark`（整段 cookie 串里其它键不属于名字）。
		if sep := accountSep(prefix); sep != "" {
			prefix = strings.SplitN(prefix, sep, 2)[0]
		}
		// 整段 curl 这类还有空格/引号/斜杠的，取第一个词，别把命令行当名字。
		if fields := strings.Fields(prefix); len(fields) > 0 {
			prefix = fields[0]
		}
		prefix = strings.Trim(prefix, "-\t ,'\";|:：")
		// 还是"不像名字"（含 URL/路径/等号）就退回按 token 派生，保证稳定且可读。
		if prefix == "" || strings.ContainsAny(prefix, "'\"/:;=\\") || len(prefix) > 40 {
			name = deriveName(token)
		} else {
			name = prefix
		}
		return name, "auth_session=" + token, ""
	}

	// 3) `名字<分隔符><裸 session 值>`（很多人只复制 session，不带 auth_session= 前缀）
	if sep := accountSep(line); sep != "" {
		parts := strings.SplitN(line, sep, 2)
		n := strings.TrimSpace(parts[0])
		rest := strings.TrimSpace(parts[1])
		if len(rest) >= 20 && !strings.ContainsAny(rest, " \t@=;") {
			if n == "" {
				n = deriveName(rest)
			}
			return n, "auth_session=" + rest, ""
		}
	}

	// 4) 整行就是一串裸 session 值
	trimmed := strings.TrimSpace(line)
	if len(trimmed) >= 20 && !strings.ContainsAny(trimmed, " \t@=;-") {
		return deriveName(trimmed), "auth_session=" + trimmed, ""
	}

	// 4) 看起来是别的格式 —— 给出**具体**可执行的说法，而不是一句"格式不对"
	if strings.Contains(line, "@") && strings.Contains(line, "----") {
		parts := strings.Split(line, "----")
		if len(parts) >= 5 {
			return "", "", "这是「邮箱----密码----clientId----refreshToken」格式：请用 tools/magic-import.py 自动换会话（本站没有密码登录）"
		}
		return "", "", "这是「邮箱----密码」格式：本站没有密码登录，只能用 auth_session 或走魔法链接（tools/magic-import.py）"
	}
	return "", "", "没找到 auth_session。支持：账号名----auth_session=… 、只贴 session 值、整段 cookie 串、或 {\"name\":…,\"cookie\":…}"
}

// accountSep 返回这一行用的分隔符（没有则空）。
func accountSep(line string) string {
	for _, cand := range []string{"----", "\t", ",", "  "} {
		if strings.Contains(line, cand) {
			return cand
		}
	}
	return ""
}

// deriveName 给没有名字的行起一个稳定、可读的名字（同一个 token 永远得到同一个名字）。
func deriveName(token string) string {
	tail := token
	if len(tail) > 6 {
		tail = tail[len(tail)-6:]
	}
	return "acc-" + tail
}

func (p *panel) handleAccountAction(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.SplitN(rest, "/", 2)
	name := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if r.URL.Query().Get("action") != "" {
		action = r.URL.Query().Get("action")
	}

	switch {
	case action == "toggle" && r.Method == http.MethodPost:
		var body struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
			return
		}
		if err := p.deps.Pool.SetEnabled(name, body.Enabled); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case action == "probe" && r.Method == http.MethodPost:
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			_ = p.deps.Pool.Probe(ctx, name)
		}()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": "探测已开始，稍后刷新"})
	case action == "" && r.Method == http.MethodDelete:
		if err := p.deps.Pool.Remove(name); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		http.NotFound(w, r)
	}
}

func (p *panel) handleTasks(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	entries := p.deps.Store.List(limit, 0, "")
	rows := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		warnings := len(e.Warnings)
		// `paid` 是**三态**：true（确认扣费）/ false（确认免费）/ nil（未到终态、还不知道）。
		// 把 nil 画成 false 等于声称"这次不花钱"—— 那是替上游撒谎，面板上必须区分。
		var paid any
		var credits any
		if raw, ok := e.Usage["paid"].(bool); ok {
			paid = raw
		}
		if e.Usage != nil {
			credits = e.Usage["credits"]
		}
		rows = append(rows, map[string]any{
			"id": e.ID, "model": e.Model, "status": e.Status,
			"account": e.Account, "upstream_task": e.TaskID,
			"created_at": time.UnixMilli(e.CreatedAtMs).Format("01-02 15:04:05"),
			"shape":      e.Shape, "warnings": warnings,
			"error": e.SubmitErrorCode,
			// 名字带 upstream_ 前缀是刻意的：这个 `credits` 与 `paid` **反相**
			// （免费任务记 1、扣费任务记 0），谁把它当金额读都会读反。
			"paid": paid, "upstream_credits": credits,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": rows})
}

// ---- 会话 ----

func (p *panel) authed(r *http.Request) bool {
	if p.password == "" {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	expiresRaw, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	expires, err := strconv.ParseInt(expiresRaw, 10, 64)
	if err != nil || time.Now().Unix() > expires {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(p.sign(expires)))
}

func (p *panel) sign(expires int64) string {
	mac := hmac.New(sha256.New, p.key)
	fmt.Fprintf(mac, "%s|%d", p.password, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

// ---- 静态资源 ----

func (p *panel) serveStatic(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	h := w.Header()
	h.Set("Content-Type", contentType)
	// no-cache 的语义是"可缓存、但每次使用前必须校验"，配合 ETag 命中即回 304。
	// 一个缓存头都不发的话，浏览器会按启发式规则长期缓存，升级后仍跑旧 app.js
	// —— 面板出现「新后端 + 旧前端」的错位。
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

// handleOfficial 返回官方 REST 通路的**只读**状态：是否启用、余额、可跑模型、限额。
//
// 拿不到就**如实报错**（面板上显示"读不到"），不要伪造 0 余额 —— 那会让运维以为号没钱了。
func (p *panel) handleOfficial(w http.ResponseWriter, r *http.Request) {
	if !p.deps.OfficialEnabled || p.deps.OfficialAccount == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	acct, err := p.deps.OfficialAccount(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "account": acct})
}

// handleConfig 输出**可公开的**运行参数快照（不含任何凭据）。
//
// 排障时最常见的问题是"线上到底跑的哪套参数"，而这个答案不该靠 ssh 翻文件。
func (p *panel) handleConfig(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"service":        p.deps.ServiceName,
		"version":        p.deps.Version,
		"auth_mode":      p.deps.AuthMode,
		"base_url":       p.deps.BaseURL,
		"media_enabled":  p.deps.MediaEnabled,
		"probe_interval": p.deps.ProbeInterval.String(),
		"official":       p.deps.OfficialEnabled,
		"account_count":  len(p.deps.Pool.Snapshot()),
		"uptime_seconds": int(time.Since(p.deps.Started).Seconds()),
	})
}

// handleDebugPlan 干跑一次翻译：只算，不发。
//
// 为什么要有这一步：官方通路的 `billed` 与网页通路完全不是一套规则
// （网页 480p/720p+turbo+≤10s 免费，官方**每次**都扣积分）。
// 调试页先给结论，避免"点一下就花钱"。
func (p *panel) handleDebugPlan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Request map[string]any `json:"request"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if p.deps.PlanRequest == nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": "调试未启用"})
		return
	}
	plan, err := p.deps.PlanRequest(body.Request)
	if err != nil {
		p.deps.DebugHistory.Append(debughist.Record{
			Kind: "plan", Model: modelOf(body.Request), OK: false, Error: err.Error(),
			Request: body.Request,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	eff, _ := plan["effective"].(map[string]any)
	billed, _ := eff["billed"].(bool)
	p.deps.DebugHistory.Append(debughist.Record{
		Kind: "plan", Model: modelOf(body.Request), Channel: strOf(eff["channel"]),
		Billed: billed, OK: true, Request: body.Request,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plan": plan})
}

// handleDebugSubmit 真跑一条。必须显式 confirm，且**计费的单要额外 allow_billed**。
//
// 面板上误点一个按钮就花 15 积分，这种事不该发生 —— 两道闸：
// confirm（我知道这是真请求）+ allow_billed（我知道这次要花钱）。
func (p *panel) handleDebugSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Request     map[string]any `json:"request"`
		Confirm     bool           `json:"confirm"`
		AllowBilled bool           `json:"allow_billed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body"})
		return
	}
	if !body.Confirm {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "需要 confirm=true"})
		return
	}
	if p.deps.SubmitRequest == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "调试未启用"})
		return
	}

	// 先算一遍：计费的单必须二次确认。
	if p.deps.PlanRequest != nil {
		if plan, err := p.deps.PlanRequest(body.Request); err == nil {
			if billed, _ := plan["effective"].(map[string]any)["billed"].(bool); billed && !body.AllowBilled {
				writeJSON(w, http.StatusOK, map[string]any{
					"ok": false,
					"error": "这次请求会**计费**（effective.billed=true）。确认要花钱请勾选「允许计费」。" +
						"先看 dry-run 的价目表再决定。",
					"plan": plan,
				})
				return
			}
		}
	}

	out, err := p.deps.SubmitRequest(r.Context(), body.Request)
	if err != nil {
		p.deps.DebugHistory.Append(debughist.Record{
			Kind: "submit", Model: modelOf(body.Request), OK: false, Error: err.Error(),
			Request: body.Request,
		})
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	rec := debughist.Record{Kind: "submit", Model: modelOf(body.Request), OK: true, Request: body.Request}
	if plan, perr := p.deps.PlanRequest(body.Request); perr == nil {
		if eff, ok := plan["effective"].(map[string]any); ok {
			rec.Channel = strOf(eff["channel"])
			rec.Billed, _ = eff["billed"].(bool)
		}
	}
	rec.TaskID = strOf(out["id"])
	p.deps.DebugHistory.Append(rec)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": out})
}

func modelOf(req map[string]any) string {
	if req == nil {
		return ""
	}
	return strOf(req["model"])
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}
