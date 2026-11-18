package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rsfree/aivideomaker2api/internal/apierr"
	"github.com/rsfree/aivideomaker2api/internal/debughist"
	"github.com/rsfree/aivideomaker2api/internal/jsonx"
	"github.com/rsfree/aivideomaker2api/internal/pool"
	"github.com/rsfree/aivideomaker2api/internal/store"
	"github.com/rsfree/aivideomaker2api/internal/translate"
	"github.com/rsfree/aivideomaker2api/internal/upstream"
)

// contentRejectionQuotes 是内容审核的**上游原话**（随响应透传）。
//
// 🔴 只透传这**两句话**，不透传整个错误串：后者形如 `ai.minimaxH3: Prompt or Image
// Sensitive`，前缀是站点内部 procedure 名（= 模型路由细节），属于脱敏纪律要挡掉的东西。
// 键是**小写匹配子串**，值是对外原话（保留上游的大小写与措辞）。
var contentRejectionQuotes = []struct{ marker, quote string }{
	{"prompt or image sensitive", "Prompt or Image Sensitive"},
	{"flagged by a content checker",
		"The content could not be processed because it contained material flagged by a content checker."},
}

// submitAsync 把推进（转存 → 抢槽位 → create）压到后台，POST 立即返回 id。
//
// 名额用**信号量**而不是无界 goroutine：推进线程绝大部分时间阻塞在账号闸门上，不产生额外
// 上游流量 ⇒ 名额偏小不会"少打上游"，只会让上游槽位空转。超额时 goroutine 在信号量上排队，
// **对调用方无感**（id 早已返回，状态仍是 queued）。
func (s *Server) submitAsync(entry *store.Entry, plan *translate.Plan) {
	go func() {
		s.submitSem <- struct{}{}
		defer func() { <-s.submitSem }()
		s.submit(entry, plan)
	}()
}

// submit 执行一次完整的提交。**任何失败都写回记录**（对外表现为轮询到的 failed），
// 而不是让调用方在 POST 上等一个可能几百秒的错误。
func (s *Server) submit(entry *store.Entry, plan *translate.Plan) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.SubmitLease)
	defer cancel()

	// 🔴 默认走**网页会话**：它才有免费档（480p/720p + tier=turbo + ≤10s）。
	//
	// 官方 REST **全程收费**（实测引号：720p/5s=15 分、1080p/5s=20 分、wan30 5s=75 分）。
	// 之前这里写成"非 480p 就走官方"，是拿 `quote.billable=false` 当成了免费依据 ——
	// 那个字段在 schema 里是 `const: false`，恒定值，纯误导。默认切官方 = 替调用方花钱。
	//
	// 要官方通路必须**显式**：`extra_body.aivideomaker_channel="official"`。
	// 路由：**默认网页**（它才有免费档）；官方只在调用方显式点名时使用。
	// 哪些模型该走哪条路，由 `GET /v1/models` 列出来，调用时自己选。
	if strings.EqualFold(jsonx.String(plan.WebParams["channel"]), "official") {
		if s.official == nil {
			// 明确报错，别默默退回网页 —— 有些模型网页端根本没有，退回去只会换一个更难懂的错。
			s.markFailed(entry.ID, apierr.Param(
				"this model is only available on the official REST API, but no official api key is configured "+
					"(set official.api_key / AVM_OFFICIAL_API_KEY)", "model"))
			return
		}
		s.submitOfficial(ctx, entry, plan)
		return
	}

	account, err := s.pool.Acquire(ctx)
	if err != nil {
		s.markFailed(entry.ID, err)
		return
	}
	// 账号在**占槽成功之后**立刻写回记录：提交失败时面板与日志里也能看出是哪个号试过。
	// （只在成功时记录会让"失败了但不知道用的哪个号"变成排障死胡同。）
	if _, patchErr := s.store.Patch(entry.ID, func(e *store.Entry) { e.Account = account.Name }); patchErr != nil {
		s.log.Warn("persist account binding failed", "task", entry.ID, "err", patchErr)
	}

	released := false
	release := func() {
		if !released {
			released = true
			account.Queue.Release()
		}
	}

	// 素材必须转存到**站点自己的 CDN**：外链会按 Content-Type 白名单被拒
	//（实测过一个 .jpg 外链返回非标准 MIME image/jpg，字节其实是 PNG）。
	params, err := s.rehost(ctx, account, plan.WebParams)
	if err != nil {
		release()
		s.markFailed(entry.ID, err)
		return
	}

	taskID, err := account.Client.Create(ctx, params, plan.CaptchaToken)
	if err != nil {
		release()
		s.pool.ReportFailure(account.Name, err)
		s.markFailed(entry.ID, err)
		s.recordSubmitFailure(entry, "submit", "web", err.Error())
		s.log.Warn("submit failed", "task", entry.ID, "account", account.Name, "err", err)
		return
	}
	s.pool.ReportSuccess(account.Name)

	if _, err := s.store.Patch(entry.ID, func(e *store.Entry) {
		e.TaskID = taskID
		e.Account = account.Name
		e.SubmittedAtMs = time.Now().UnixMilli()
		e.Status = "queued"
	}); err != nil {
		s.log.Error("persist upstream task id failed", "task", entry.ID, "err", err)
	}

	// 槽位由盯梢 goroutine 在**终态**释放 —— 上游的槽位语义就是"跑到出片"。
	account.Queue.Watch(taskID)
	s.log.Info("submitted", "task", entry.ID, "upstream_task", taskID, "account", account.Name)

	// 账本：公网 API 跑的单也要进历史（面板只记调试页那些动作，缺了这半边就不叫账本）。
	s.recordSubmit(entry, plan, "web", taskID, nil)
}

// markFailed 把一条记录转成终态失败，并写下**可区分的**失败事实。
//
// 写三个键：
//   - `SubmitErrorCode`  —— 对外码：审核 = ContentFiltered，闸门 = RateLimitExceeded，
//     其余 = SubmissionFailed（保持原值 ⇒ 老调用方零感知）。
//   - `SubmitErrorQuote` —— 仅审核时非空 = **上游原话**。
//   - `SubmitError`      —— 内部归因全文，只进日志与面板，**不进对外响应体**。
//
// 只有一个对外码会让下游无法判断"该不该重试"（生产实测：59 条审核失败里有 34 次是
// 调用方盲目重发，把失败率从 6.6% 抬到 15.7%）。ContentFiltered 就是那个"别再试了"的信号。
func (s *Server) markFailed(entryID string, err error) {
	code := codeSubmitFailed
	quote := ""
	hint := ""
	var upErr *apierr.UpstreamError
	if errors.As(err, &upErr) {
		if upstream.IsContentRejection(upErr) {
			code = codeContentFiltered
			quote = quoteForContentRejection(upErr)
		} else if upErr.Code == "CAPTCHA_REQUIRED" {
			code = codeRateLimited
		}
		// 已脱敏的对外说法：让"轮询到的失败"可操作（等闸门 / 换素材 / 加余额），
		// 而不是一句"提交失败"。它是**我们的话**，与 quote（上游原话）分开存。
		hint = clientMessage(upErr)
	}
	_, patchErr := s.store.Patch(entryID, func(e *store.Entry) {
		e.Status = "failed"
		e.SubmitErrorCode = code
		e.SubmitErrorQuote = quote
		e.SubmitErrorHint = hint
		e.SubmitError = err.Error()
	})
	if patchErr != nil {
		s.log.Error("persist submit failure failed", "task", entryID, "err", patchErr)
	}
	s.cache.drop(entryID)
}

func quoteForContentRejection(err *apierr.UpstreamError) string {
	blob := strings.ToLower(err.Error())
	for _, c := range contentRejectionQuotes {
		if strings.Contains(blob, c.marker) {
			return c.quote
		}
	}
	return ""
}

// ---------------------------------------------------------------- rehost --

type rehostJob struct {
	key   string // `imageUrl` / `referenceImageUrls[0]` …
	value string
	index int // >=0 表示数组元素
	base  string
}

// rehost 把外链 / 内联 data URI 媒体转存到站点 CDN，返回**新**的 params。
//
// ⚠️ 并行执行（项数上限 7，互为独立）：串行时总耗时 = 各项之和（3 张图各 3s ⇒ 9s），
// 并行后 ≈ 最慢一项。预算语义不变（任一项发现超预算就抛 504，**点名**是哪一步哪一项）；
// 失败语义不变：任一项失败 ⇒ 整次创建失败。
func (s *Server) rehost(ctx context.Context, account *pool.Account, params map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[k] = v
	}

	var jobs []rehostJob
	for _, key := range []string{"imageUrl", "lastFrameUrl", "referenceVideoUrl"} {
		if value := jsonx.String(out[key]); value != "" {
			jobs = append(jobs, rehostJob{key: key, value: value, index: -1})
		}
	}
	for _, key := range []string{"referenceImageUrls", "referenceAudioUrls"} {
		values := jsonx.Slice(out[key])
		if values == nil {
			if typed, ok := out[key].([]string); ok {
				for i, v := range typed {
					if strings.TrimSpace(v) != "" {
						jobs = append(jobs, rehostJob{key: key, value: v, index: i})
					}
				}
			}
			continue
		}
		for i, raw := range values {
			if v := jsonx.String(raw); v != "" {
				jobs = append(jobs, rehostJob{key: key, value: v, index: i})
			}
		}
	}
	if len(jobs) == 0 {
		return out, nil // 纯文生视频：连 worker 都不必起
	}

	budget := upstream.NewBudget(s.cfg.MediaRehostBudget)
	type result struct {
		job       rehostJob
		publicURL string
		err       error
	}
	results := make([]result, len(jobs))
	sem := make(chan struct{}, minInt(s.cfg.RehostWorkers, len(jobs)))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func(i int, job rehostJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			publicURL, err := s.uploadOne(ctx, account, job.value, budget)
			results[i] = result{job: job, publicURL: publicURL, err: err}
		}(i, job)
	}
	wg.Wait()

	// 按提交顺序回收 ⇒ 第一个错误就是"第一项"的错误，与串行时代一致。
	for _, res := range results {
		if res.err != nil {
			return nil, res.err
		}
		if res.job.index >= 0 {
			values := jsonx.Slice(out[res.job.key])
			if values == nil {
				if typed, ok := out[res.job.key].([]string); ok {
					typed[res.job.index] = res.publicURL
					out[res.job.key] = typed
					continue
				}
				continue
			}
			values[res.job.index] = res.publicURL
			out[res.job.key] = values
			continue
		}
		out[res.job.key] = res.publicURL
	}
	return out, nil
}

// uploadOne 处理**单个**媒体值。
//
// 已经是站点 CDN 的地址原样放过，不重复上传 —— 那既浪费一次上传，也会让同一份素材
// 在站点侧多存一份。
func (s *Server) uploadOne(ctx context.Context, account *pool.Account, value string,
	budget *upstream.Budget) (string, error) {
	if isSiteCDN(value) {
		return value, nil
	}
	if strings.HasPrefix(value, "data:") {
		buf, mime, err := decodeDataURI(value)
		if err != nil {
			// 解不开的 data URI 原样透传，让上游给出更明确的报错（不猜）。
			return value, nil
		}
		_ = mime
		upload, err := account.Client.UploadBytes(ctx, buf, "inline", false, budget)
		if err != nil {
			return "", err
		}
		if upload.PublicURL == "" {
			return "", apierr.NewUpstream("uploads.getPresignedUrl", "upstream returned no publicUrl", "", 0)
		}
		return upload.PublicURL, nil
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		upload, err := account.Client.UploadURL(ctx, value, false, budget)
		if err != nil {
			return "", err
		}
		if upload.PublicURL == "" {
			return "", apierr.NewUpstream("uploads.getPresignedUrl", "upstream returned no publicUrl", "", 0)
		}
		return upload.PublicURL, nil
	}
	return value, nil
}

func isSiteCDN(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "img2video.ai" || strings.HasSuffix(host, ".img2video.ai")
}

func decodeDataURI(value string) ([]byte, string, error) {
	rest := strings.TrimPrefix(value, "data:")
	semi := strings.Index(rest, ";base64,")
	if semi < 0 {
		return nil, "", fmt.Errorf("not a base64 data URI")
	}
	mime := rest[:semi]
	payload := rest[semi+len(";base64,"):]
	buf, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", err
	}
	return buf, mime, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// submitOfficial 走官方 REST 下单（`POST /api/v1/generate/{model}`）。
//
// 官方接口是**同步返回**的：一次调用就给出 taskId 与 creditsCharged，
// 不需要"受理即返 + 后台推进"，也就不需要槽位和闸门重试。
func (s *Server) submitOfficial(ctx context.Context, entry *store.Entry, plan *translate.Plan) {
	// 官方模型的 body 由 translate 按模型规格**已经建好**（每个模型的 duration 类型、
	// 分辨率写法都不一样，见 internal/official/models.go）。这里只负责发。
	if pre := jsonx.Map(plan.WebParams["official_body"]); pre != nil {
		model := jsonx.String(plan.WebParams["official_model"])
		if model == "" {
			model = "minimax"
		}
		res, err := s.official.Generate(ctx, model, pre)
		if err != nil {
			s.markFailed(entry.ID, err)
			s.log.Warn("official submit failed", "task", entry.ID, "model", model, "err", err)
			return
		}
		s.persistOfficial(ctx, entry, res, model)
		return
	}

	// 落到这里说明是"显式点名 official 的 minimax"（没有预建 body），按传统字段拼。
	wp := plan.WebParams
	body := map[string]any{
		"content":  jsonx.String(wp["content"]),
		"duration": intOfAny(wp["duration"]),
		"tier":     firstNonEmptyStr(jsonx.String(wp["tier"]), "turbo"),
	}
	if r := jsonx.String(wp["resolution"]); r == "720p" || r == "1080p" {
		body["resolution"] = r
	}
	// 官方的枚举叫 "auto"，站点侧叫 "adaptive"。
	if a := jsonx.String(wp["aspectRatio"]); a != "" {
		if a == "adaptive" || a == "auto" {
			body["aspectRatio"] = "auto"
		} else {
			body["aspectRatio"] = a
		}
	}
	for _, k := range []string{"imageUrl", "lastFrameUrl", "referenceVideoUrl"} {
		if v := jsonx.String(wp[k]); v != "" {
			body[k] = v
		}
	}
	if refs, ok := wp["referenceImageUrls"].([]string); ok && len(refs) > 0 {
		body["referenceImageUrls"] = refs
	}
	if refs, ok := wp["referenceAudioUrls"].([]string); ok && len(refs) > 0 {
		body["referenceAudioUrls"] = refs
	}

	res, err := s.official.Generate(ctx, "minimax", body)
	if err != nil {
		s.markFailed(entry.ID, err)
		s.log.Warn("official submit failed", "task", entry.ID, "err", err)
		return
	}
	s.persistOfficial(ctx, entry, res, "minimax")
}

// persistOfficial 把官方返回的 taskId / 计费落库。两条官方分支共用。
func (s *Server) persistOfficial(ctx context.Context, entry *store.Entry, res map[string]any, model string) {
	taskID := jsonx.String(res["taskId"])
	charged := intOfAny(res["creditsCharged"])

	if _, err := s.store.Patch(entry.ID, func(e *store.Entry) {
		e.TaskID = taskID
		e.Account = "official"
		e.Upstream = "official"
		e.UpstreamModel = model
		e.SubmittedAtMs = time.Now().UnixMilli()
		e.Status = "queued"
		if charged > 0 {
			e.Usage = map[string]any{"paid": true, "credits": charged}
		}
	}); err != nil {
		s.log.Error("persist official task id failed", "task", entry.ID, "err", err)
	}
	s.log.Info("submitted", "task", entry.ID, "upstream_task", taskID,
		"account", "official", "model", model, "credits", charged, "channel", "official")
	s.recordSubmit(entry, nil, "official", taskID, map[string]any{"credits": charged})
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func intOfAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// recordSubmit 往账本里写一条**真实提交**。
//
// plan 可能为空（官方分支手里只有 entry）——那就退回到 entry 自己的字段，
// 宁可少几个细节，也别因为拿不到 plan 就整条不记。
func (s *Server) recordSubmit(entry *store.Entry, plan *translate.Plan, channel, taskID string, extra map[string]any) {
	if s.history == nil || !s.history.Enabled() {
		return
	}
	model := entry.Model
	billed := false
	if plan != nil {
		if m := jsonx.String(plan.Effective["model"]); m != "" {
			model = m
		}
		billed, _ = plan.Effective["billed"].(bool)
	}
	if v, ok := extra["credits"].(int); ok && v > 0 {
		billed = true
	}
	s.history.Append(debughist.Record{
		Kind: "submit", Model: model, Channel: channel, Billed: billed,
		OK: true, TaskID: entry.ID,
	})
}

// recordSubmitFailure 记一次失败的提交（失败也要留痕，否则"为什么没跑"无从追）。
func (s *Server) recordSubmitFailure(entry *store.Entry, kind, channel, errText string) {
	if s.history == nil || !s.history.Enabled() {
		return
	}
	s.history.Append(debughist.Record{
		Kind: kind, Model: entry.Model, Channel: channel, OK: false, Error: errText, TaskID: entry.ID,
	})
}

// forcedWeb 允许调用方强制留在网页通路（`extra_body.aivideomaker_channel="web"`）。
//
// 存在意义：有人就是要用网页那张账单/那条额度，或者官方 key 想省着用。
// 但**不做默认** —— 默认必须是更稳的那条。
func forcedWeb(plan *translate.Plan) bool {
	if plan == nil || plan.WebParams == nil {
		return false
	}
	return strings.EqualFold(jsonx.String(plan.WebParams["channel"]), "web")
}
