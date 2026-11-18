// Package server 是对外 HTTP 服务：方舟 / OpenAI Videos 两种协议壳 + 媒体出口 + 面板。
//
// 两条对外形状（方舟 / OpenAI）**共用同一条提交与查询管线** —— 翻译层只换"外壳"，
// 计费口径、dry-run、账号绑定、并发闸门在两条线上行为完全一致。
//
// # 受理 / 推进分离
//
// `POST` **毫秒级返回 id**，素材转存 / 抢上游槽位 / create 全在后台 goroutine 里完成 ——
// 客户端不再同步等"转存（最坏 120s）+ 等槽位（最坏 600s）+ 现场铸 token（2~45s）"。
//
// 两条线只在**外壳**上不同：
//   - `/v1/videos` ⇒ `{id, object, status:"queued", created_at}`（Chatfire 四字段契约）；
//   - 方舟 `/tasks` ⇒ `{"id"}` —— 形状与受理分离之前逐字一致（这个端点的调用方是官方
//     SDK 与直连客户，契约一个键都不能动）；变的只是"返回时上游受理**可能**还没完成"。
//
// 🔴 **失败语义**：上游侧失败（闸门 / 队列 / 站点 5xx / 转存超预算）**不在 POST 上报错**，
// 而是把记录转 `failed` —— 调用方按各自契约轮询即可看到终态。前置校验（鉴权 / 参数 /
// 渠道配置 / 能力 / dry-run）仍在本请求内**同步**返回，不受影响。
//
// 提交**租约**：后台最坏要 转存 120s + 等槽位 600s + 铸 token 45s + 若干 RTT ≈ 800s，
// 租约取 1200s —— 超过它仍没有 taskId，就判定提交 goroutine 已死（进程重启/崩溃）并把
// 记录标成 failed，否则客户端会**永远** queued。
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jushenzhidao/aivideomaker2api/internal/debughist"
	"github.com/jushenzhidao/aivideomaker2api/internal/jsonx"
	"github.com/jushenzhidao/aivideomaker2api/internal/media"
	"github.com/jushenzhidao/aivideomaker2api/internal/official"
	"github.com/jushenzhidao/aivideomaker2api/internal/pool"
	"github.com/jushenzhidao/aivideomaker2api/internal/store"
)

// Config 是服务参数。
type Config struct {
	ServiceName   string
	Version       string
	Listen        string
	APIKey        string
	TaskCacheTTL  time.Duration
	SubmitWorkers int
	// SubmitInline 让推进侧**同步**执行（POST 要等提交完成才返回）。
	// 只给测试与本地调试：生产误开等于把"受理即返"这个特性静默关掉。
	SubmitInline bool
	SubmitLease  time.Duration
	// MediaRehostBudget 是**整个转存阶段**的总预算（见 upstream.Budget）。
	MediaRehostBudget time.Duration
	// RehostWorkers 是转存并发上限（一次创建最多 7 个媒体项，互为独立）。
	RehostWorkers int
	// Official 是官方 REST API（`/api/v1/*`）的配置。APIKey 非空即启用。
	//
	// 两条通路的分工：官方 key 没有 cookie、没有 Turnstile 闸门，还多出
	// seedance20 / wan30 / happyhorse 这些网页端没有的模型 —— 有 key 就优先走它；
	// 没有 key 就退回网页会话（480p 免费档）。
	Official OfficialConfig
	// History 是调试/提交账本（可空）。
	History *debughist.Recorder
	// PanelHandler 挂在 /panel 下（nil = 不启用面板）。
	PanelHandler http.Handler
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

const (
	defaultLease        = 1200 * time.Second
	defaultTaskCacheTTL = 15 * time.Second
	maxSubmitWorkers    = 64
)

// OfficialConfig 是官方 REST API 通路。
type OfficialConfig struct {
	APIKey  string
	BaseURL string
	Timeout time.Duration
}

// Enabled 表示配置了 key。
func (c OfficialConfig) Enabled() bool { return strings.TrimSpace(c.APIKey) != "" }

// Server 是对外服务。
type Server struct {
	cfg   Config
	pool  *pool.Pool
	store *store.Store
	media *media.Proxy
	log   *slog.Logger
	mux   *http.ServeMux
	cache *viewCache
	// flights 把"同一个任务的并发查询"折成一次上游调用（single-flight）。
	//
	// 为什么必须做：调用方按秒轮询，N 个客户端同时问同一条任务时，缓存还没写回，
	// 每个请求都会打一次上游 —— 而闸门是按请求速度翻的（实测），这些重复 GET
	// 等于白送给闸门计数。折叠之后，同一时刻只有一个请求真的上网。
	flights sync.Map // id -> *viewFlight
	// upstreamGets 是**真正打到上游**的任务查询次数（单测与 /metrics 都靠它证明折叠生效）。
	upstreamGets atomic.Int64

	// official 非空表示官方通路可用（配置了 key）。
	official *official.Client
	// history 记录每次真实提交（含公网 API 来的），面板「调试」页与它共用一份账本。
	history *debughist.Recorder

	submitSem chan struct{}
	started   time.Time
}

// New 组装路由。mediaProxy 可以为 nil（= 成片出口未配置，原样透传上游链接）。
func New(cfg Config, p *pool.Pool, st *store.Store, proxy *media.Proxy, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.TaskCacheTTL == 0 {
		cfg.TaskCacheTTL = defaultTaskCacheTTL
	}
	if cfg.SubmitLease <= 0 {
		cfg.SubmitLease = defaultLease
	}
	if cfg.SubmitWorkers <= 0 {
		cfg.SubmitWorkers = 32
	}
	if cfg.SubmitWorkers > maxSubmitWorkers {
		cfg.SubmitWorkers = maxSubmitWorkers
	}
	if cfg.MediaRehostBudget <= 0 {
		cfg.MediaRehostBudget = 120 * time.Second
	}
	if cfg.RehostWorkers <= 0 {
		cfg.RehostWorkers = 7
	}
	if cfg.RehostWorkers > maxSubmitWorkers {
		cfg.RehostWorkers = maxSubmitWorkers
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 300 * time.Second
	}
	s := &Server{
		cfg: cfg, pool: p, store: st, media: proxy, log: logger,
		mux:       http.NewServeMux(),
		cache:     newViewCache(cfg.TaskCacheTTL, 1024),
		submitSem: make(chan struct{}, cfg.SubmitWorkers),
		started:   time.Now(),
		history:   cfg.History,
	}
	if cfg.Official.Enabled() {
		oc, err := official.New(official.Config{
			BaseURL: cfg.Official.BaseURL, APIKey: cfg.Official.APIKey, Timeout: cfg.Official.Timeout,
		})
		if err != nil {
			// 配置错不该让整个服务起不来：网页会话那条路还能用。
			s.log.Error("官方 API 配置无效，已忽略（继续用网页会话）", "err", err)
		} else {
			s.official = oc
			s.log.Info("官方 API 通路已启用（无闸门；优先于网页会话）")
		}
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/v3/contents/generations/tasks", s.handleCreateArk)
	s.mux.HandleFunc("GET /api/v3/contents/generations/tasks", s.handleListArk)
	s.mux.HandleFunc("GET /api/v3/contents/generations/tasks/{id}", s.handleGetArk)

	// 显式通路（方舟协议，同一套请求体，只换上游）：
	//
	//	/web/...      只跑网页会话（有免费档：480p/720p + turbo + ≤10s）
	//	/official/... 只跑官方 REST（无闸门，但**全程收费**）
	//
	// 为什么要有：`/api/v3/...` 是"自动"——官方独有模型会自动走官方，minimax 走网页。
	// 但运维排查与成本控制时需要**确定性**：同一份请求打到哪条路，不该靠猜。
	s.mux.HandleFunc("POST /web/api/v3/contents/generations/tasks", s.forceChannel("web", s.handleCreateArk))
	s.mux.HandleFunc("GET /web/api/v3/contents/generations/tasks/{id}", s.forceChannel("web", s.handleGetArk))
	s.mux.HandleFunc("POST /official/api/v3/contents/generations/tasks", s.forceChannel("official", s.handleCreateArk))
	s.mux.HandleFunc("GET /official/api/v3/contents/generations/tasks/{id}", s.forceChannel("official", s.handleGetArk))

	// 模型清单：告诉调用方"有哪些、走哪条路、要不要钱"。
	// 两条通路的价差可达十倍（minimax 15 vs seedance20 147），不能靠试。
	s.mux.HandleFunc("GET /v1/models", s.handleListModels)
	s.mux.HandleFunc("GET /v1/models/{id}", s.handleGetModel)

	s.mux.HandleFunc("POST /v1/videos", s.handleCreateOpenAI)
	s.mux.HandleFunc("GET /v1/videos/{id}", s.handleGetOpenAI)

	// MiniMax 官方形状（错误一律装在 base_resp 里、HTTP 恒 200）。
	s.mux.HandleFunc("POST "+miniMaxCreatePath, s.handleCreateMiniMax)
	s.mux.HandleFunc("GET "+miniMaxQueryPath, s.handleQueryMiniMax)
	s.mux.HandleFunc("GET "+miniMaxRetrievePath, s.handleRetrieveMiniMax)

	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)

	if s.media != nil && s.media.Enabled() {
		path := s.media.Path()
		s.mux.HandleFunc("GET "+path+"/", s.handleMedia)
	}
	if s.cfg.PanelHandler != nil {
		s.mux.Handle("/panel/", s.cfg.PanelHandler)
		s.mux.Handle("/panel", s.cfg.PanelHandler)
	}
	// ⚠️ 刻意**不带方法**：带 "GET " 会与 "/panel/"（无方法）冲突 —— ServeMux 认为
	// "GET / 方法更少但路径更宽"，两条规则无法排序就 panic。无方法的 "/" 是纯兜底，
	// 更具体的路径（含带方法的）永远优先。
	s.mux.HandleFunc("/", s.handleRoot)
}

// ServeHTTP 套上"请求 id + 访问日志 + panic 兜底"三件套。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rid := r.Header.Get("X-Request-Id")
	if rid == "" {
		rid = randomHex(6)
	}
	w.Header().Set("X-Request-Id", rid)
	r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, rid))

	started := time.Now()
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path, "panic", rec)
			writeJSON(recorder, http.StatusInternalServerError, map[string]any{"error": map[string]any{
				"code":    codeUpstreamError,
				"message": withRequestID("internal error", rid),
				"param":   "",
				"type":    "InvalidRequest",
			}})
		}
		// 轮询路径**成功**的那一半不噪：一个任务几十上百行 `GET …/tasks/{id} -> 200`
		// 会淹没真正的信息。失败（4xx/5xx）照打 —— 那是"调用方到底看到了什么"的唯一记录。
		quiet := isPollPath(r.URL.Path) && recorder.status < 400
		if !quiet {
			s.log.Info("request", "method", r.Method, "path", r.URL.Path,
				"status", recorder.status, "ms", time.Since(started).Milliseconds(), "rid", rid)
		}
	}()
	s.mux.ServeHTTP(recorder, r)
}

type requestIDKey struct{}

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

// isPollPath 判断是否是"会被秒级反复请求"的任务查询路径。
func isPollPath(path string) bool {
	return strings.Contains(path, "/contents/generations/tasks/") || strings.Contains(path, "/v1/videos/")
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// Flush 让流式响应（媒体下载）不被包装层挡住。
func (w *statusRecorder) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---- 视图缓存（轮询节流）----

type cachedView struct {
	view     map[string]any
	expires  time.Time
	terminal bool
}

var terminalStatus = map[string]bool{"succeeded": true, "failed": true, "cancelled": true, "expired": true}

// viewCache 挡住调用方的高频轮询，别把上游打到 429。
//
// 两条规则：
//   - **非终态**视图：TTL 内直接复用（任务要跑 ~60s，15s 的粒度不丢信息）；
//   - **终态**视图：**永久**复用 —— 记录已定格，多查一次只是白挨限流的风险。
//
// TTL <= 0 = 显式关闭（每次实时回上游）。只有**成功取到**的视图才进缓存。
// viewFlight 是一次进行中的上游查询；等待者共享它的结果。
type viewFlight struct {
	done chan struct{}
	view map[string]any
	err  error
}

type viewCache struct {
	ttl   time.Duration
	max   int
	mu    sync.Mutex
	items map[string]cachedView
}

func newViewCache(ttl time.Duration, max int) *viewCache {
	if max <= 0 {
		max = 1024
	}
	return &viewCache{ttl: ttl, max: max, items: map[string]cachedView{}}
}

func (c *viewCache) get(id string) (map[string]any, bool) {
	if c.ttl <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	hit, ok := c.items[id]
	if !ok {
		return nil, false
	}
	if !hit.terminal && time.Now().After(hit.expires) {
		delete(c.items, id)
		return nil, false
	}
	return cloneView(hit.view), true
}

// put 写入缓存。ttl <= 0 表示用默认值；终态永久。
func (c *viewCache) put(id string, view map[string]any, ttl time.Duration) {
	if c.ttl <= 0 || view == nil {
		return
	}
	status, _ := view["status"].(string)
	terminal := terminalStatus[status]
	if ttl <= 0 {
		ttl = c.ttl
	}
	expires := time.Now().Add(ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) >= c.max {
		// 超限先扔非终态（随时可重取），还不够就扔第一条。
		for k, v := range c.items {
			if !v.terminal {
				delete(c.items, k)
			}
		}
		for k := range c.items {
			if len(c.items) < c.max {
				break
			}
			delete(c.items, k)
		}
	}
	c.items[id] = cachedView{view: cloneView(view), expires: expires, terminal: terminal}
}

func (c *viewCache) drop(id string) {
	c.mu.Lock()
	delete(c.items, id)
	c.mu.Unlock()
}

func cloneView(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// newArkID 生成对外任务 id：`cgt-<日期>-<hex8>`，读得出创建日、又足够不透明。
func newArkID() string {
	return "cgt-" + time.Now().Format("20060102") + "-" + randomHex(4)
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 在现代系统上不会失败；真失败也不该让请求挂掉，
		// 用时间纳秒兜一个（可预测但唯一性仍够本地使用）。
		return fmt.Sprintf("%08x", time.Now().UnixNano())[:n*2]
	}
	return hex.EncodeToString(buf)
}

// channelCtxKey 用来在 handler 链里传递"这个端点强制走哪条通路"。
type channelCtxKey struct{}

// forceChannel 把一个 handler 包成"强制某条上游通路"。
func (s *Server) forceChannel(channel string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(context.WithValue(r.Context(), channelCtxKey{}, channel)))
	}
}

// forcedChannel 读当前端点强制的通路（空 = 自动）。
func forcedChannel(r *http.Request) string {
	v, _ := r.Context().Value(channelCtxKey{}).(string)
	return v
}

// applyForcedChannel 把端点级的强制通路写进请求体的 extra_body，
// 这样翻译层只有**一个**入口判断，不需要知道请求是从哪个 URL 进来的。
func applyForcedChannel(r *http.Request, body map[string]any) map[string]any {
	ch := forcedChannel(r)
	if ch == "" || body == nil {
		return body
	}
	extra := jsonx.Map(body["extra_body"])
	if extra == nil {
		extra = map[string]any{}
	} else {
		// 不改调用方原 map（它还要进 Requested 证据）
		cp := make(map[string]any, len(extra)+1)
		for k, v := range extra {
			cp[k] = v
		}
		extra = cp
	}
	extra["aivideomaker_channel"] = ch
	body["extra_body"] = extra
	return body
}
