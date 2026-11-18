package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/jushenzhidao/aivideomaker2api/internal/apierr"
	"github.com/jushenzhidao/aivideomaker2api/internal/jsonx"
	"github.com/jushenzhidao/aivideomaker2api/internal/media"
	"github.com/jushenzhidao/aivideomaker2api/internal/openaivideos"
	"github.com/jushenzhidao/aivideomaker2api/internal/pool"
	"github.com/jushenzhidao/aivideomaker2api/internal/slots"
	"github.com/jushenzhidao/aivideomaker2api/internal/sniff"
	"github.com/jushenzhidao/aivideomaker2api/internal/store"
	"github.com/jushenzhidao/aivideomaker2api/internal/translate"
)

const (
	// 请求体上限 = 站点**合法的最坏组合**（图 4×10MB + 视频 50MB + 音频 2×15MB = 120MB）
	// + 余量。合法请求永远超不过它；而它能在解析**落盘之前**挡住一个巨大的件。
	maxFormBody = 128 * 1024 * 1024
)

// ---------------------------------------------------------------- create --

func (s *Server) handleCreateArk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	body, err := readJSONObject(w, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	body = applyForcedChannel(r, body)
	options, err := slots.ParseChannelOptions(r.Header.Get(slots.HeaderName))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	plan, err := translate.Create(body, options, "")
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// `incompatible` 是**上游明确不具备**的能力：dry-run 下照常返回（零成本可见），
	// 真实提交前必须被拒 —— 否则调用方会拿到一个"参数被静默吞掉"的成片。
	if len(plan.Incompatible) > 0 && !dryRun(r, body) {
		s.writeError(w, r, apierr.Param(strings.Join(plan.Incompatible, "; "), ""))
		return
	}

	effective, warnings := translate.BillingView(plan)
	if dryRun(r, body) {
		writeJSON(w, http.StatusOK, map[string]any{
			"dry_run": true, "ok": true,
			"requested": plan.Requested, "effective": effective,
			"warnings": warnings, "unsupported": plan.Unsupported,
			"incompatible": plan.Incompatible, "web_params": plan.WebParams,
		})
		return
	}

	entry := s.newEntry(plan, "ark", effective, warnings)
	if err := s.store.Put(entry); err != nil {
		s.writeError(w, r, fmt.Errorf("persist task: %w", err))
		return
	}
	if s.cfg.SubmitInline {
		s.submit(entry, plan)
	} else {
		s.submitAsync(entry, plan)
	}
	// 形状与受理分离之前**逐字一致**：这个端点的调用方是官方 SDK 与直连客户，
	// 契约一个键都不能动。
	writeJSON(w, http.StatusOK, map[string]any{"id": entry.ID})
}

func (s *Server) handleCreateOpenAI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	fields, referenceFormat, err := readOpenAIFields(w, r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	arkBody, notes, err := openaivideos.BodyFromOpenAI(fields, referenceFormat)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	// 面级策略：本端点**只跑免费线**。槽位恒为 FreeOnlySlot，调用方写什么模型名都不参与选路。
	plan, err := translate.Create(arkBody, nil, openaivideos.FreeOnlySlot)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	plan.Warnings = append(notes, plan.Warnings...)

	effective, warnings := translate.BillingView(plan)
	if dryRun(r, arkBody) {
		writeJSON(w, http.StatusOK, map[string]any{
			"dry_run": true, "ok": true,
			"requested": plan.Requested, "effective": effective,
			"warnings": warnings, "unsupported": plan.Unsupported,
			"incompatible": plan.Incompatible, "web_params": plan.WebParams,
		})
		return
	}

	entry := s.newEntry(plan, "openai", effective, warnings)
	if err := s.store.Put(entry); err != nil {
		s.writeError(w, r, fmt.Errorf("persist task: %w", err))
		return
	}
	if s.cfg.SubmitInline {
		s.submit(entry, plan)
	} else {
		s.submitAsync(entry, plan)
	}
	now := time.Now().Unix()
	writeJSON(w, http.StatusOK, map[string]any{
		"id": entry.ID, "object": "video", "status": "queued", "created_at": now,
	})
}

func (s *Server) newEntry(plan *translate.Plan, shape string, effective map[string]any,
	warnings []string) *store.Entry {
	return &store.Entry{
		ID:          newArkID(),
		Model:       jsonx.String(plan.Requested["model"]),
		Upstream:    "web",
		Shape:       shape,
		Requested:   plan.Requested,
		Effective:   effective,
		Warnings:    warnings,
		Unsupported: plan.Unsupported,
		Status:      "queued",
		CreatedAtMs: time.Now().UnixMilli(),
	}
}

// ------------------------------------------------------------------- get --

func (s *Server) handleGetArk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	entry, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		s.writeError(w, r, apierr.NewUpstream("model.getModel", "task not found", "NOT_FOUND", http.StatusNotFound))
		return
	}
	view, err := s.taskView(r.Context(), entry)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, translate.ArkTaskView(view))
}

func (s *Server) handleGetOpenAI(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	entry, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		s.writeError(w, r, apierr.NewUpstream("model.getModel", "task not found", "NOT_FOUND", http.StatusNotFound))
		return
	}
	view, err := s.taskView(r.Context(), entry)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, openaivideos.TaskView(view, entry.CreatedAtMs/1000))
}

// taskView 是**两条查询路径共用的收尾**：拿到内部视图（含证据），并套上出口闸门。
//
// 抽成函数的原因：两条路径各写一份，字段集必然漂。
func (s *Server) taskView(ctx context.Context, entry *store.Entry) (map[string]any, error) {
	// 1) 提交失败 / 租约过期：不依赖上游，直接给终态。
	if entry.TaskID == "" {
		if entry.Status == "failed" {
			return s.failedView(entry), nil
		}
		if time.Since(time.UnixMilli(entry.CreatedAtMs)) > s.cfg.SubmitLease {
			// 租约到期仍没有 taskId ⇒ 推进 goroutine 已死（进程重启 / 崩溃）。
			// 标 failed，否则客户端会**永远** queued。
			_, _ = s.store.Patch(entry.ID, func(e *store.Entry) {
				e.Status = "failed"
				e.SubmitErrorCode = codeSubmitFailed
				e.SubmitError = "submission lease expired before the upstream accepted the task"
			})
			updated, _ := s.store.Get(entry.ID)
			if updated != nil {
				entry = updated
			}
			return s.failedView(entry), nil
		}
		return s.queuedView(entry), nil
	}

	// 2) 缓存：非终态 TTL 内复用，终态永久复用。
	if cached, ok := s.cache.get(entry.ID); ok {
		return cached, nil
	}

	// 3) 同一任务的并发查询折叠成一次上游调用（闸门按请求速度翻，重复 GET 是白送计数）。
	return s.coalesce(ctx, entry.ID, func() (map[string]any, error) {
		// 官方通路：任务在官方那侧，按 `/api/v1/tasks/{id}` 查，不走网页会话。
		if entry.Upstream == "official" && s.official != nil {
			return s.officialTaskView(ctx, entry)
		}
		acc, ok := s.pool.Account(entry.Account)
		if !ok {
			return nil, &apierr.UpstreamError{
				Procedure:  "model.getModel",
				Message:    "the account that owns this task is no longer in the pool",
				Code:       "UPSTREAM_UNAVAILABLE",
				HTTPStatus: http.StatusServiceUnavailable,
			}
		}
		raw, err := acc.Client.GetTask(ctx, entry.TaskID, 0)
		if err != nil {
			return nil, err
		}
		s.upstreamGets.Add(1)
		view := translate.NormalizeWebTask(raw)
		s.applyEvidence(view, entry)

		// 🔴 **先取上游地址，再过出口闸门**：闸门会把 content.video_url 换成本服务自己的
		// 下载地址，换完之后再取就成了"自环"——媒体出口会去 fetch 自己的 URL，永远取不到片子。
		// 这一步的顺序是有意的，别调换。
		upstreamURL := ""
		if content := jsonx.Map(view["content"]); content != nil {
			upstreamURL = jsonx.String(content["video_url"])
		}

		status := jsonx.String(view["status"])
		if status == "succeeded" {
			s.applyMediaGate(view, entry)
		}

		if terminalStatus[status] {
			// 终态落到记录里：列表视图与重启后的兜底都靠它。
			// `source_url` 只存**上游**地址（媒体出口回源用）；`video_url` 存**对外**地址。
			gatedURL := ""
			if content := jsonx.Map(view["content"]); content != nil {
				gatedURL = jsonx.String(content["video_url"])
			}
			_, _ = s.store.Patch(entry.ID, func(e *store.Entry) {
				e.Status = status
				e.SourceURL = firstNonEmpty(e.SourceURL, upstreamURL)
				e.VideoURL = gatedURL
				e.UpstreamModel = jsonx.String(view["upstream_model"])
				// `usage` 是终态才有的证据（`paid` 决定这条任务有没有花钱），必须落盘：
				// 缓存会过期、进程会重启，而"花没花"这个问题永远要能回答。
				if usage := jsonx.Map(view["usage"]); usage != nil {
					e.Usage = usage
				}
			})
			// 🔴 失败也可能是**花过钱**的：实测 720p/11s 因内容审核失败，但上游照样扣了 33 积分。
			// 这条日志是运维唯一能主动看到"钱白花了"的地方 —— 面板上那句话太安静了。
			if status == "failed" {
				if usage := jsonx.Map(view["usage"]); usage != nil {
					if paid, _ := usage["paid"].(bool); paid {
						s.log.Warn("task failed but was still billed",
							"task", entry.ID, "account", entry.Account,
							"upstream_task", entry.TaskID, "usage", usage)
					}
				}
			}
		}
		s.cache.put(entry.ID, view, pollTTL(entry, status))
		return view, nil
	})
}

// pollTTL 给"非终态"的视图挑一个缓存有效期：任务越老，上游变化越慢，轮询就可以更稀。
//
// 上限 20s 是刻意的：再稀就会让调用方看到明显滞后的状态（它按秒轮询，突然 1 分钟
// 不动会以为服务卡了）。省下来的主要不是带宽，而是**闸门计数**。
func pollTTL(entry *store.Entry, status string) time.Duration {
	if terminalStatus[status] {
		return 0 // 终态永久复用（put 内部按终态处理，这里的 0 只是"用默认"）
	}
	age := time.Since(time.UnixMilli(entry.CreatedAtMs))
	switch {
	case age > 3*time.Minute:
		return 20 * time.Second
	case age > 90*time.Second:
		return 15 * time.Second
	default:
		return 10 * time.Second
	}
}

// coalesce 把"同一个任务的并发查询"折成一次上游调用。
//
// 调用方按秒轮询；缓存写回之前到达的请求原本会各自打一次上游。这些重复 GET 在闸门
// 眼里就是"提交速度"（实测闸门按请求速度翻），折叠之后同一时刻只有一个请求真的上网。
func (s *Server) coalesce(ctx context.Context, id string, fn func() (map[string]any, error)) (map[string]any, error) {
	f, loaded := s.flights.LoadOrStore(id, &viewFlight{done: make(chan struct{})})
	fl := f.(*viewFlight)
	if loaded {
		select {
		case <-fl.done:
			if fl.err != nil {
				return nil, fl.err
			}
			return cloneView(fl.view), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	defer func() {
		close(fl.done)
		s.flights.Delete(id)
	}()
	view, err := fn()
	fl.view, fl.err = view, err
	return view, err
}

// applyEvidence 把**本地任务记录**里的证据字段并进视图。
//
// `model` = 调用方请求的模型；★ `upstream_model` = 上游实际执行的模型（站点记录里的
// aiModel）。只有前者时，调用方无从知道上游换了模型（请求 doubao-…，成片的却是
// minimax_h3）。查不到时**如实给 nil**，绝不用请求值顶上 —— 那等于把"看不到"变成
// "看到一个假的"。
func (s *Server) applyEvidence(view map[string]any, entry *store.Entry) {
	view["id"] = entry.ID
	view["model"] = entry.Model
	view["upstream"] = entry.Upstream
	view["requested"] = entry.Requested
	view["effective"] = entry.Effective
	view["warnings"] = entry.Warnings
	view["unsupported"] = entry.Unsupported
}

// applyMediaGate 是成片出口的**出口闸门**：上游 URL 永远不许出现在对外响应体里。
//
// 把 content.video_url 换成本服务自己的下载地址。这是**恒等**替换 —— 与"有没有搬过"
// 无关，所以任务状态照实上报 succeeded，不必引入"成功了但拿不到地址"的中间态。
func (s *Server) applyMediaGate(view map[string]any, entry *store.Entry) {
	if s.media == nil || !s.media.Enabled() {
		return
	}
	content := jsonx.Map(view["content"])
	if content == nil {
		return
	}
	source := jsonx.String(content["video_url"])
	if source == "" {
		return
	}
	// ⚠️ 返回**新 map**：view 是缓存的浅拷贝，原地改会污染缓存。
	view["content"] = map[string]any{"video_url": s.media.DownloadURL(entry.ID, source)}
}

func (s *Server) queuedView(entry *store.Entry) map[string]any {
	view := map[string]any{
		"id": entry.ID, "status": "queued", "error": nil,
		"created_at": entry.CreatedAtMs / 1000, "updated_at": entry.CreatedAtMs / 1000,
	}
	if entry.Usage != nil {
		view["usage"] = entry.Usage
	}
	s.applyEvidence(view, entry)
	return view
}

func (s *Server) failedView(entry *store.Entry) map[string]any {
	code := entry.SubmitErrorCode
	if code == "" {
		code = codeSubmitFailed
	}
	message := entry.SubmitErrorQuote
	if message == "" {
		message = entry.SubmitErrorHint
	}
	if message == "" {
		message = submissionFailedMessage
	}
	view := map[string]any{
		"id": entry.ID, "status": "failed",
		"error":      map[string]any{"code": code, "message": message},
		"created_at": entry.CreatedAtMs / 1000, "updated_at": entry.CreatedAtMs / 1000,
	}
	if entry.Usage != nil {
		view["usage"] = entry.Usage
	}
	s.applyEvidence(view, entry)
	return view
}

// ---------------------------------------------------------------- list --

func (s *Server) handleListArk(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	limit := queryInt(r, "limit", 20)
	offset := queryInt(r, "offset", 0)
	entries := s.store.List(limit, offset, "")
	items := make([]any, 0, len(entries))
	for _, entry := range entries {
		if cached, ok := s.cache.get(entry.ID); ok {
			items = append(items, translate.ArkTaskView(cached))
			continue
		}
		// 列表刻意**不打上游**（原实现也是"本进程内"）：一个任务几十次轮询已经够贵了。
		// 状态以本地记录为准，逐条查询才是权威口径。
		view := s.localView(entry)
		items = append(items, translate.ArkTaskView(view))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "total": s.store.Count(""),
	})
}

func (s *Server) localView(entry *store.Entry) map[string]any {
	if entry.Status == "failed" {
		return s.failedView(entry)
	}
	view := map[string]any{
		"id": entry.ID, "status": entry.Status, "error": nil,
		"created_at": entry.CreatedAtMs / 1000, "updated_at": entry.CreatedAtMs / 1000,
	}
	if entry.VideoURL != "" && entry.Status == "succeeded" {
		view["content"] = map[string]any{"video_url": entry.VideoURL}
	}
	s.applyEvidence(view, entry)
	return view
}

// --------------------------------------------------------------- models --

// --------------------------------------------------------------- health --

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("deep") == "1" {
		// 只有显式 deep=1 才打上游，避免存活探针把上游当依赖。
		s.pool.ProbeAll()
	}
	snapshot := s.pool.Snapshot()
	inUse, capacity := 0, 0
	for _, v := range snapshot {
		inUse += v.InUse
		capacity += v.Capacity
	}
	info := map[string]any{
		"ok":         true,
		"service":    s.cfg.ServiceName,
		"version":    s.cfg.Version,
		"upstream":   "web",
		"billing":    translate.BillingNote(),
		"auth":       authMode(s.cfg.APIKey),
		"uptime_sec": int(time.Since(s.started).Seconds()),
		// 号池规模：**只给数量，不列明细** —— /healthz 不鉴权，明细就是别人的账号
		// （余额与指纹）。明细走面板。
		"accounts_tracked": len(snapshot),
		"accounts_enabled": countEnabled(snapshot),
		// 官方通路是否启用（只给布尔；key 不出现）。
		"official_enabled": s.official != nil,
		"tasks_tracked":    s.store.Count(""),
		"task_store":       s.store.Describe(),
		"task_cache_ttl":   s.cfg.TaskCacheTTL.Seconds(),
		"submit_workers":   s.cfg.SubmitWorkers,
		"slots_in_use":     inUse,
		"slots_capacity":   capacity,
		"media_proxy":      mediaDescribe(s.media),
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// 指标里带账号名（label）⇒ 与 API 同权。Prometheus 抓取侧按 bearer token 配即可。
	if !s.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	snapshot := s.pool.Snapshot()
	b.WriteString("# HELP aivm_accounts Account count by state.\n# TYPE aivm_accounts gauge\n")
	enabled := countEnabled(snapshot)
	fmt.Fprintf(&b, "aivm_accounts{state=\"enabled\"} %d\naivm_accounts{state=\"disabled\"} %d\n",
		enabled, len(snapshot)-enabled)
	b.WriteString("# HELP aivm_slots Upstream submission slots.\n# TYPE aivm_slots gauge\n")
	for _, v := range snapshot {
		fmt.Fprintf(&b, "aivm_slots{account=%q,state=\"in_use\"} %d\n", v.Name, v.InUse)
		fmt.Fprintf(&b, "aivm_slots{account=%q,state=\"capacity\"} %d\n", v.Name, v.Capacity)
	}
	b.WriteString("# HELP aivm_tasks_tracked Tasks in the local store.\n# TYPE aivm_tasks_tracked gauge\n")
	fmt.Fprintf(&b, "aivm_tasks_tracked %d\n", s.store.Count(""))
	// 扣费条数是本项目的**业务红线指标**：免费档跑多少都不心疼，一旦这个数涨了，
	// 说明有人在为生成付费 —— 这是运维唯一需要被主动提醒的事实。
	// 真正打到上游的任务查询次数。它的绝对值不重要，**相对提交数**才是：
	// 轮询折叠/缓存失效时这条线会陡增 —— 而闸门数的是请求速度。
	b.WriteString("# HELP aivm_upstream_task_gets_total Task lookups actually sent upstream.\n# TYPE aivm_upstream_task_gets_total counter\n")
	fmt.Fprintf(&b, "aivm_upstream_task_gets_total %d\n", s.upstreamGets.Load())
	b.WriteString("# HELP aivm_tasks_charged Tasks confirmed billed by the upstream (usage.paid=true).\n# TYPE aivm_tasks_charged gauge\n")
	fmt.Fprintf(&b, "aivm_tasks_charged %d\n", s.store.CountCharged())
	b.WriteString("# HELP aivm_uptime_seconds Process uptime.\n# TYPE aivm_uptime_seconds counter\n")
	fmt.Fprintf(&b, "aivm_uptime_seconds %d\n", int(time.Since(s.started).Seconds()))
	_, _ = w.Write([]byte(b.String()))
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service": s.cfg.ServiceName, "version": s.cfg.Version,
		"endpoints": []string{
			"POST /api/v3/contents/generations/tasks",
			"GET  /api/v3/contents/generations/tasks/{id}",
			"POST /v1/videos",
			"GET  /v1/videos/{id}",
			"POST /v1/video_generation",
			"GET  /v1/query/video_generation?task_id=",
			"GET  /v1/files/retrieve?file_id=",
			"GET  /v1/models",
			"GET  /healthz",
		},
	})
}

// ---------------------------------------------------------------- media --

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	if s.media == nil || !s.media.Enabled() {
		http.NotFound(w, r)
		return
	}
	name := path.Base(strings.TrimPrefix(r.URL.Path, s.media.Path()+"/"))
	id := media.StripExt(name)
	entry, ok := s.store.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	source := entry.SourceURL
	if source == "" {
		// 没被任何一次查询记下源地址 ⇒ 当场回上游取一次（下载端点不该因为
		// "还没人轮询过"就 404）。
		acc, found := s.pool.Account(entry.Account)
		if !found || entry.TaskID == "" {
			http.NotFound(w, r)
			return
		}
		raw, err := acc.Client.GetTask(r.Context(), entry.TaskID, 0)
		if err != nil {
			http.Error(w, "the video source is temporarily unavailable from this service", http.StatusBadGateway)
			return
		}
		view := translate.NormalizeWebTask(raw)
		content := jsonx.Map(view["content"])
		source = jsonx.String(content["video_url"])
		if source != "" {
			_, _ = s.store.Patch(entry.ID, func(e *store.Entry) {
				e.SourceURL = source
				e.Status = jsonx.String(view["status"])
			})
		}
	}
	if source == "" {
		http.NotFound(w, r)
		return
	}
	s.media.Serve(w, r, source, id+media.ExtensionOf(source))
}

// -------------------------------------------------------------- helpers --

func readJSONObject(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, apierr.Param("body is required (and must be at most 128MB)", "")
	}
	if len(raw) == 0 {
		return nil, apierr.Param("body is required", "")
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var body map[string]any
	if err := dec.Decode(&body); err != nil {
		return nil, apierr.Param(fmt.Sprintf("invalid JSON body: %v", err), "")
	}
	if body == nil {
		return nil, apierr.Param("body must be a JSON object", "")
	}
	return body, nil
}

// readOpenAIFields 解析 `/v1/videos` 的入参：JSON body 或 multipart 表单都支持。
//
// 表单里的文件部件会被读成 data URI（之后由转存管线解字节、传站点 CDN）——
// 这是 Chatfire 文档里最常见的 curl 形态。
func readOpenAIFields(w http.ResponseWriter, r *http.Request) (map[string]any, string, error) {
	contentType := r.Header.Get("Content-Type")
	referenceFormat := r.Header.Get("input-reference-format")

	if !strings.HasPrefix(strings.ToLower(contentType), "multipart/form-data") {
		body, err := readJSONObject(w, r)
		return body, referenceFormat, err
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseMultipartForm(maxFormBody); err != nil {
		return nil, "", apierr.Param(fmt.Sprintf("invalid multipart body: %v", err), "")
	}
	fields := map[string]any{}
	for key, vals := range r.MultipartForm.Value {
		if key == "input-reference-format" {
			referenceFormat = vals[0]
			continue
		}
		if len(vals) == 1 {
			fields[key] = vals[0]
			continue
		}
		list := make([]any, 0, len(vals))
		for _, v := range vals {
			list = append(list, v)
		}
		fields[key] = list
	}
	for key, headers := range r.MultipartForm.File {
		urls := make([]string, 0, len(headers))
		for _, fh := range headers {
			if fh.Size > sniff.MaxUploadBytes {
				return nil, "", apierr.Param(fmt.Sprintf(
					"上传的参考素材过大：%d 字节，超过单件上限 %d 字节（%dMB）",
					fh.Size, sniff.MaxUploadBytes, sniff.MaxUploadBytes/1048576), key)
			}
			file, err := fh.Open()
			if err != nil {
				return nil, "", apierr.Param("failed to open uploaded part: "+err.Error(), key)
			}
			buf, err := io.ReadAll(io.LimitReader(file, sniff.MaxUploadBytes))
			file.Close()
			if err != nil {
				return nil, "", apierr.Param("failed to read uploaded part: "+err.Error(), key)
			}
			info := sniff.File(buf)
			contentType := info.ContentType
			if contentType == "application/octet-stream" {
				contentType = "image/png"
			}
			urls = append(urls, "data:"+contentType+";base64,"+base64.StdEncoding.EncodeToString(buf))
		}
		if key == "input_reference" {
			list := make([]any, 0, len(urls))
			for _, u := range urls {
				list = append(list, u)
			}
			fields[key] = list
			continue
		}
		if len(urls) > 0 {
			fields[key] = urls[0]
		}
	}
	return fields, referenceFormat, nil
}

func queryInt(r *http.Request, key string, fallback int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return fallback
	}
	return n
}

func authMode(apiKey string) string {
	if apiKey == "" {
		return "open"
	}
	return "key"
}

func countEnabled(views []pool.View) int {
	n := 0
	for _, v := range views {
		if v.Enabled {
			n++
		}
	}
	return n
}

func mediaDescribe(proxy *media.Proxy) map[string]any {
	if proxy == nil {
		return map[string]any{"enabled": false}
	}
	return proxy.Describe()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// officialTaskView 把官方 `GET /api/v1/tasks/{id}` 翻成与网页通路**同构**的视图。
//
// 两个通路对外的形状必须一致（调用方不该因为走了哪条路而拿到不同的字段），
// 所以这里复用同一套 media gate / evidence 流程。
func (s *Server) officialTaskView(ctx context.Context, entry *store.Entry) (map[string]any, error) {
	raw, err := s.official.Task(ctx, entry.TaskID)
	if err != nil {
		return nil, err
	}
	s.upstreamGets.Add(1)

	status := strings.ToUpper(jsonx.String(raw["status"]))
	view := map[string]any{
		"id":             entry.ID,
		"model":          jsonx.String(raw["model"]),
		"upstream_model": jsonx.String(raw["model"]),
		"status":         officialStatus(status),
		"error":          nil,
		"content":        map[string]any{"video_url": nil},
		"usage": map[string]any{
			"credits": intOfAny(raw["creditsCharged"]),
			"paid":    intOfAny(raw["creditsCharged"]) > 0,
		},
		"created_at":      jsonx.String(raw["createdAt"]),
		"updated_at":      jsonx.String(firstNonNilValue(raw["completedAt"], raw["createdAt"])),
		"duration":        entry.Effective["duration"],
		"ratio":           entry.Effective["aspectRatio"],
		"resolution":      entry.Effective["resolution"],
		"upstream_record": raw,
	}

	// 失败原因：官方放在 output.error / output.msg 里。
	if st := jsonx.String(view["status"]); st == "failed" {
		out := jsonx.Map(raw["output"])
		msg := ""
		if out != nil {
			msg = firstNonEmptyStr(jsonx.String(out["error"]), jsonx.String(out["msg"]))
		}
		if msg == "" {
			msg = "video generation failed"
		}
		view["error"] = map[string]any{"code": "GenerationFailed", "message": msg}
	}

	// 出片：官方给 output.url（公网地址），这里过同一道出口闸门换成我们的 /v/ 地址。
	if jsonx.String(view["status"]) == "succeeded" {
		if out := jsonx.Map(raw["output"]); out != nil {
			if u := jsonx.String(out["url"]); u != "" {
				view["content"] = map[string]any{"video_url": u}
				entry.SourceURL = firstNonEmpty(entry.SourceURL, u)
			} else {
				// 官方说 COMPLETED 却没给 url —— 与网页端同样的自相矛盾，按失败处理。
				view["status"] = "failed"
				view["error"] = map[string]any{
					"code": "GenerationFailed", "message": "upstream reported COMPLETED without an output url",
				}
			}
		}
	}
	if jsonx.String(view["status"]) == "succeeded" {
		s.applyMediaGate(view, entry)
	}

	s.applyEvidence(view, entry)

	if terminalStatus[jsonx.String(view["status"])] {
		gatedURL := ""
		if content := jsonx.Map(view["content"]); content != nil {
			gatedURL = jsonx.String(content["video_url"])
		}
		_, _ = s.store.Patch(entry.ID, func(e *store.Entry) {
			e.Status = jsonx.String(view["status"])
			e.VideoURL = gatedURL
			e.UpstreamModel = jsonx.String(view["upstream_model"])
			if usage := jsonx.Map(view["usage"]); usage != nil {
				e.Usage = usage
			}
		})
	}
	s.cache.put(entry.ID, view, pollTTL(entry, jsonx.String(view["status"])))
	return view, nil
}

// officialStatus 把官方的 5 态映射到本服务对外的三态。
func officialStatus(s string) string {
	switch s {
	case "COMPLETED":
		return "succeeded"
	case "FAILED", "CANCEL":
		return "failed"
	case "PROGRESS":
		return "running"
	default: // SUBMITTED
		return "queued"
	}
}

func firstNonNilValue(vals ...any) any {
	for _, v := range vals {
		if v != nil && v != "" {
			return v
		}
	}
	return nil
}

// PlanDebug 是调试页的**纯翻译**入口：不联网、不花钱。
//
// 复用与公网创建接口完全相同的 translate.Create / BillingView，
// 所以"调试页看到的"和"真正提交时会发生的"是同一套判断 —— 不会出现两套口径。
func PlanDebug(body map[string]any) (map[string]any, error) {
	plan, err := translate.Create(body, nil, "")
	if err != nil {
		return nil, err
	}
	// BillingView 返回的 warnings **已经包含** plan.Warnings 与计费相关的补充，
	// 再拼一次会出现"同一条提示刷两遍"（实测过）。
	effective, warnings := translate.BillingView(plan)
	return map[string]any{
		"effective":    effective,
		"warnings":     warnings,
		"unsupported":  plan.Unsupported,
		"incompatible": plan.Incompatible,
		"procedure":    plan.Procedure(),
	}, nil
}

// debugRecorder 是最小 ResponseWriter：只为在同进程里复用 handleCreateArk。
type debugRecorder struct {
	status int
	body   []byte
}

func (d *debugRecorder) Header() http.Header  { return http.Header{} }
func (d *debugRecorder) WriteHeader(code int) { d.status = code }
func (d *debugRecorder) Write(b []byte) (int, error) {
	if d.status == 0 {
		d.status = http.StatusOK
	}
	d.body = append(d.body, b...)
	return len(b), nil
}

// DebugSubmit 用**与公网接口同一条链路**创建一条任务。
//
// 为什么绕一圈构造内部请求：创建逻辑（鉴权之外的部分）散在 handleCreateArk 里，
// 包括 storage、异步推进、dry-run、错误形态。复制一份就意味着两边迟早不一致；
// 这里直接复用那个 handler，把"鉴权"换成服务自己的 key。
func (s *Server) DebugSubmit(ctx context.Context, body map[string]any) (map[string]any, error) {
	raw, err := jsonx.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"/api/v3/contents/generations/tasks", strings.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+s.cfg.APIKey)

	rec := &debugRecorder{}
	s.handleCreateArk(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.body, &out); err != nil {
		return nil, fmt.Errorf("unexpected create response: %s", string(rec.body))
	}
	if rec.status >= 400 {
		msg, _ := out["error"].(map[string]any)
		if msg != nil {
			return nil, fmt.Errorf("%v", msg["message"])
		}
		return nil, fmt.Errorf("create failed with status %d", rec.status)
	}
	return out, nil
}
