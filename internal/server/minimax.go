package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/rsfree/aivideomaker2api/internal/apierr"
	"github.com/rsfree/aivideomaker2api/internal/jsonx"
	"github.com/rsfree/aivideomaker2api/internal/pool"
	"github.com/rsfree/aivideomaker2api/internal/ratio"
	"github.com/rsfree/aivideomaker2api/internal/store"
	"github.com/rsfree/aivideomaker2api/internal/translate"
	"github.com/rsfree/aivideomaker2api/internal/upstream"
)

// MiniMax 官方视频生成协议面。
//
//	POST /v1/video_generation            → {task_id, base_resp}
//	GET  /v1/query/video_generation      → {task_id, status, file_id?, video_width?, video_height?, base_resp}
//	GET  /v1/files/retrieve              → {file: {…, download_url}, base_resp}
//
// 与方舟线**同一个管线**，只是外壳与错误约定不同：
//
//   - MiniMax 的风格是**HTTP 恒 200、错误装在 `base_resp.status_code` 里** ——
//     这条面必须照做，否则调用方（MiniMax SDK / new-api 的 minimax 渠道）会把
//     一个 400 当成"网络错误"而不是"参数错了"。
//   - 因此**上游失败也不在 POST 上报错**：与方舟线一致地走"受理即返"，失败在
//     查询时表现为 `status="Fail"` + `error_message`。
//
// 面级策略与本服务其它面一致：**槽位恒为 `minimaxH3`**（web 线唯一已实测存在的生成
// procedure），调用方写的 model 名不参与选路 —— 每次改写都在 warnings 里留痕。
const (
	miniMaxCreatePath   = "/v1/video_generation"
	miniMaxQueryPath    = "/v1/query/video_generation"
	miniMaxRetrievePath = "/v1/files/retrieve"
	miniMaxSlot         = "minimaxH3"
)

// miniMaxBaseResp 是 MiniMax 的统一错误信封。code：0 成功 / 1002 限流 / 1004 鉴权 /
// 1008 余额 / 1026 内容 / 2013 参数 / 1000 内部。
type miniMaxBaseResp struct {
	StatusCode int    `json:"status_code"`
	StatusMsg  string `json:"status_msg"`
}

func baseResp(code int, msg string) miniMaxBaseResp {
	if msg == "" && code == 0 {
		msg = "success"
	}
	return miniMaxBaseResp{StatusCode: code, StatusMsg: msg}
}

// miniMaxStatus 把**归一化后**的 Ark 状态翻成 MiniMax 词表。
//
// 站点侧的 submitted / preparing / queueing 在本服务已经折叠成 `queued`，
// 这里统一报 `Queueing`（对调用方而言都是"还没开始跑"）。
var miniMaxStatus = map[string]string{
	"queued": "Queueing", "running": "Processing", "succeeded": "Success",
	"failed": "Fail", "cancelled": "Fail", "expired": "Fail",
}

// miniMaxCode 把内部错误归到 MiniMax 的 base_resp 码。
//
// 报文一律用**脱敏后**的说法（clientMessage）—— MiniMax 面同样是出口，
// 上游 procedure 名、铸造服务地址这些内部标识不能从这里漏出去。
func miniMaxCode(err error) miniMaxBaseResp {
	var paramErr *apierr.ParamError
	var upErr *apierr.UpstreamError
	var noAccount *pool.ErrNoAccount

	switch {
	case errors.As(err, &paramErr):
		return baseResp(2013, paramErr.Message)
	case errors.As(err, &noAccount):
		return baseResp(1004, "no usable account in the pool: "+noAccount.Reason)
	case errors.As(err, &upErr):
		msg := clientMessage(upErr)
		switch {
		case upErr.Code == "CAPTCHA_REQUIRED" || upErr.Code == "CAPTCHA_REJECTED" ||
			upErr.Code == "QUEUE_TIMEOUT" || upErr.HTTPStatus == 429:
			return baseResp(1002, msg)
		case upErr.HTTPStatus == 401:
			return baseResp(1004, msg)
		case upErr.Code == "NOT_FOUND":
			return baseResp(2013, msg)
		case upstream.IsContentRejection(upErr):
			return baseResp(1026, msg)
		}
		blob := strings.ToLower(upErr.Error())
		for _, marker := range []string{"balance", "insufficient", "quota", "credits"} {
			if strings.Contains(blob, marker) {
				return baseResp(1008, msg)
			}
		}
		return baseResp(1000, msg)
	}
	return baseResp(1000, "internal error")
}

// ---------------------------------------------------------------- create --

func (s *Server) handleCreateMiniMax(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	body, err := readJSONObject(w, r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"task_id": "", "base_resp": miniMaxCode(err)})
		return
	}
	arkBody, notes, err := miniMaxToArkBody(body)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"task_id": "", "base_resp": miniMaxCode(err)})
		return
	}
	plan, err := translate.Create(arkBody, nil, miniMaxSlot)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"task_id": "", "base_resp": miniMaxCode(err)})
		return
	}
	plan.Warnings = append(notes, plan.Warnings...)

	effective, warnings := translate.BillingView(plan)
	if dryRun(r, arkBody) {
		writeJSON(w, http.StatusOK, map[string]any{
			"dry_run": true, "ok": true, "task_id": "",
			"requested": plan.Requested, "effective": effective,
			"warnings": warnings, "unsupported": plan.Unsupported,
			"upstream_payload": plan.WebParams,
			"base_resp":        baseResp(0, ""),
		})
		return
	}

	entry := s.newEntry(plan, "minimax", effective, warnings)
	if err := s.store.Put(entry); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"task_id": "", "base_resp": miniMaxCode(err)})
		return
	}
	if s.cfg.SubmitInline {
		s.submit(entry, plan)
	} else {
		s.submitAsync(entry, plan)
	}
	writeJSON(w, http.StatusOK, map[string]any{"task_id": entry.ID, "base_resp": baseResp(0, "")})
}

// ---------------------------------------------------------------- query --

func (s *Server) handleQueryMiniMax(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	taskID := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if taskID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"task_id": "", "status": "Fail", "base_resp": baseResp(2013, "task_id is required"),
		})
		return
	}
	entry, ok := s.store.Get(taskID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"task_id": taskID, "status": "Fail",
			"base_resp": baseResp(2013, "task not found"),
		})
		return
	}
	view, err := s.taskView(r.Context(), entry)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"task_id": taskID, "status": "Fail", "base_resp": miniMaxCode(err),
			"error_message": clientMessageOf(err),
		})
		return
	}

	status := miniMaxStatus[jsonx.String(view["status"])]
	if status == "" {
		status = "Queueing"
	}
	out := map[string]any{
		"task_id": taskID, "status": status, "base_resp": baseResp(0, ""),
	}
	if status == "Success" {
		width, height := s.videoSize(entry, view)
		out["file_id"] = taskID
		out["video_width"] = width
		out["video_height"] = height
	}
	if status == "Fail" {
		out["error_message"] = failureMessage(view)
	}
	writeJSON(w, http.StatusOK, out)
}

// ------------------------------------------------------------- retrieve --

func (s *Server) handleRetrieveMiniMax(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuth(w, r) {
		return
	}
	fileID := strings.TrimSpace(r.URL.Query().Get("file_id"))
	if fileID == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"file": nil, "base_resp": baseResp(2013, "file_id is required"),
		})
		return
	}
	entry, ok := s.store.Get(fileID)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{
			"file": nil, "base_resp": baseResp(2013, "file not found"),
		})
		return
	}

	// 取一次视图：既保证状态是最新的，也顺手把 source_url 记下来（下载要走它回源）。
	view, err := s.taskView(r.Context(), entry)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"file": nil, "base_resp": miniMaxCode(err),
		})
		return
	}
	if jsonx.String(view["status"]) != "succeeded" {
		writeJSON(w, http.StatusOK, map[string]any{
			"file": nil, "base_resp": baseResp(2013, "file not ready"),
		})
		return
	}
	entry, _ = s.store.Get(fileID)
	download := entry.VideoURL
	if download == "" {
		if s.media != nil && s.media.Enabled() {
			download = s.media.DownloadURL(entry.ID, entry.SourceURL)
		} else {
			download = entry.SourceURL
		}
	}
	if download == "" {
		writeJSON(w, http.StatusOK, map[string]any{
			"file": nil, "base_resp": baseResp(2013, "file not ready"),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"file": map[string]any{
			"file_id":      entry.ID,
			"bytes":        nil,
			"created_at":   entry.CreatedAtMs / 1000,
			"filename":     entry.ID + ".mp4",
			"purpose":      "video_generation",
			"download_url": download,
		},
		"base_resp": baseResp(0, ""),
	})
}

// ---------------------------------------------------------------- 翻译层 --

var miniMaxSizeRe = regexp.MustCompile(`^(\d{2,5})\s*[x×]\s*(\d{2,5})$`)

// miniMaxToArkBody 把 MiniMax 形状的入参翻成方舟请求体（纯函数）。
//
// 字段别名按**官方 SDK / new-api 渠道实际会发的名字**收（`first_frame_image`、
// `subject_reference`、`prompt_optimizer` 等），返回的 notes 是"被归一化/被改写"
// 的说明 —— 必须并进 warnings，映射不静默。
func miniMaxToArkBody(body map[string]any) (map[string]any, []string, error) {
	notes := []string{}

	model := jsonx.String(body["model"])
	if model == "" {
		model = miniMaxSlot
		notes = append(notes, "model omitted — this face always runs the web line's only verified "+
			"upstream slot ("+miniMaxSlot+")")
	}

	prompt := firstNonEmpty(
		jsonx.Raw(body["prompt"]),
		jsonx.Raw(body["content"]),
		jsonx.Raw(body["input"]),
	)
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil, notes, apierr.Param("prompt is required", "prompt")
	}

	// size 可以同时给出比例与分辨率（`1920x1080`）；单给 ratio / resolution 也行。
	sizeRatio, sizeResolution := parseMiniMaxSize(jsonx.String(body["size"]))
	if sizeRatio == "" {
		sizeRatio, sizeResolution = parseMiniMaxSize(jsonx.String(body["video_size"]))
	}
	ratioStr := firstNonEmpty(
		jsonx.String(body["aspect_ratio"]),
		jsonx.String(body["aspectRatio"]),
		sizeRatio,
	)
	resolution, resolutionNote := normalizeMiniMaxResolution(
		firstNonEmpty(jsonx.String(body["resolution"]), jsonx.String(body["quality"]), sizeResolution))
	if resolutionNote != "" {
		notes = append(notes, resolutionNote)
	}

	firstFrame := firstNonEmpty(
		jsonx.String(body["first_frame_image"]),
		jsonx.String(body["image"]),
		jsonx.String(body["image_url"]),
		jsonx.String(body["imageUrl"]),
	)
	lastFrame := firstNonEmpty(
		jsonx.String(body["last_frame_image"]),
		jsonx.String(body["last_frame_url"]),
		jsonx.String(body["lastFrameUrl"]),
	)

	refImages := stringList(body["reference_image_urls"], body["referenceImageUrls"])
	if len(refImages) == 0 {
		// MiniMax S2V：subject_reference: [{type, image: [url, …]}]
		for _, raw := range jsonx.Slice(body["subject_reference"]) {
			refImages = append(refImages, stringList(jsonx.Map(raw)["image"])...)
		}
	}
	refVideo := firstNonEmpty(jsonx.String(body["reference_video_url"]), jsonx.String(body["referenceVideoUrl"]))
	refAudios := stringList(body["reference_audio_urls"], body["referenceAudioUrls"])
	if len(refAudios) == 0 {
		if one := jsonx.String(body["reference_audio_url"]); one != "" {
			refAudios = []string{one}
		}
	}

	content := []any{map[string]any{"type": "text", "text": prompt}}
	for _, u := range refImages {
		content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
	}
	for _, u := range refAudios {
		content = append(content, map[string]any{"type": "audio_url", "audio_url": map[string]any{"url": u}})
	}
	if refVideo != "" {
		content = append(content, map[string]any{"type": "video_url", "video_url": map[string]any{"url": refVideo}})
	}
	if firstFrame != "" {
		content = append(content, map[string]any{
			"type": "image_url", "role": "first_frame", "image_url": map[string]any{"url": firstFrame}})
	}
	if lastFrame != "" {
		content = append(content, map[string]any{
			"type": "image_url", "role": "last_frame", "image_url": map[string]any{"url": lastFrame}})
	}

	arkBody := map[string]any{"model": model, "content": content}
	if ratioStr != "" {
		arkBody["ratio"] = ratioStr
	}
	if resolution != "" {
		arkBody["resolution"] = resolution
	}
	if raw, ok := body["duration"]; ok && raw != nil {
		arkBody["duration"] = raw
	} else if raw, ok := body["seconds"]; ok && raw != nil {
		arkBody["duration"] = raw
	} else if raw, ok := body["duration_seconds"]; ok && raw != nil {
		arkBody["duration"] = raw
	}

	extra := map[string]any{}
	// MiniMax 官方的 `prompt_optimizer` **默认关**，而本站点默认开 —— 这里必须把
	// 调用方的取值显式传下去，否则"没写 prompt_optimizer"会被读成"要增强"。
	if raw, ok := body["prompt_optimizer"]; ok && raw != nil {
		extra["aivideomaker_prompt_enrichment"] = raw
	}
	extraBody := jsonx.Map(body["extra_body"])
	tierValue := jsonx.String(body["tier"])
	if tierValue == "" && extraBody != nil {
		tierValue = jsonx.String(extraBody["aivideomaker_tier"])
	}
	if tierValue != "" {
		extra["aivideomaker_tier"] = tierValue
	}
	if raw := firstNonEmpty(
		jsonx.String(body["captcha_token"]),
		jsonx.String(body["turnstile_token"]),
		jsonx.String(body["token"]),
	); raw != "" {
		extra["aivideomaker_captcha_token"] = raw
	}
	if extraBody != nil {
		if b, ok := extraBody["aivideomaker_prefer_free"].(bool); ok {
			extra["aivideomaker_prefer_free"] = b
		}
		if v := jsonx.String(extraBody["aivideomaker_captcha_token"]); v != "" {
			extra["aivideomaker_captcha_token"] = v
		}
	}
	if len(extra) > 0 {
		arkBody["extra_body"] = extra
	}
	return arkBody, notes, nil
}

// parseMiniMaxSize 把 `1920x1080` 拆成 (比例, 分辨率)。
// 高度决定档位：>=1080 → 1080p，>=720 → 720p，其余 480p（与站点既有的分档一致）。
func parseMiniMaxSize(raw string) (string, string) {
	m := miniMaxSizeRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", ""
	}
	var height int
	fmt.Sscanf(m[2], "%d", &height)
	resolution := "480p"
	switch {
	case height >= 1080:
		resolution = "1080p"
	case height >= 720:
		resolution = "720p"
	}
	return ratio.SimplifyWxH(m[1] + "x" + m[2]), resolution
}

// normalizeMiniMaxResolution 认 MiniMax 的档位写法（`768P` / `1080P` / `720p`）。
//
// 认不出的**不猜成 480p**（Node 版那样做是静默降档）：留空让下游按默认 720p 走，
// 并写明"没认出、用了默认值"，调用方在 warnings 里能看见。
func normalizeMiniMaxResolution(raw string) (string, string) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimSuffix(s, "p")
	switch s {
	case "":
		return "", ""
	case "480":
		return "480p", ""
	case "720", "768":
		return "720p", ""
	case "1080", "1920":
		return "1080p", ""
	}
	return "", fmt.Sprintf("resolution %q was not recognized (expected 480P / 720P / 768P / 1080P) — "+
		"the default %s is used", raw, translate.DefaultResolution)
}

// videoSize 返回成片的像素尺寸。
//
// **实测值优先**（`ratio.MeasuredPixels` 是真实出片尺寸），没测过的组合才按比例推算 ——
// MiniMax 契约里这个字段是必给的，给 null 会让客户端渲染不出预览；但推算值只在
// "已知比例 + 已知档位"上做，且不写回实测表（那就是拿推算冒充实测了）。
func (s *Server) videoSize(entry *store.Entry, view map[string]any) (int, int) {
	resolution := jsonx.String(view["resolution"])
	if resolution == "" {
		resolution = jsonx.String(entry.Effective["resolution"])
	}
	ratioStr := jsonx.String(view["ratio"])
	if ratioStr == "" {
		ratioStr = jsonx.String(entry.Effective["aspectRatio"])
	}
	if w, h := ratio.PixelsFor(resolution, ratioStr); w > 0 && h > 0 {
		return w, h
	}
	height := 480
	switch resolution {
	case "720p":
		height = 720
	case "1080p":
		height = 1080
	}
	nums := strings.SplitN(ratioStr, ":", 2)
	if len(nums) != 2 {
		return evenInt(height * 16 / 9), height
	}
	var x, y int
	fmt.Sscanf(nums[0], "%d", &x)
	fmt.Sscanf(nums[1], "%d", &y)
	if x <= 0 || y <= 0 {
		return evenInt(height * 16 / 9), height
	}
	return evenInt(height * x / y), height
}

func evenInt(v int) int {
	if v%2 != 0 {
		return v - 1
	}
	return v
}

// failureMessage 取一条失败的对外说明（已脱敏；审核类用上游原话）。
func failureMessage(view map[string]any) string {
	if block := jsonx.Map(view["error"]); block != nil {
		if msg := jsonx.String(block["message"]); msg != "" {
			return msg
		}
	}
	if summary := jsonx.String(view["submit_error_quote"]); summary != "" {
		return summary
	}
	return "video generation failed"
}

// clientMessageOf 是给非 UpstreamError 的兜底说明（不许把内部错误原文漏出去）。
func clientMessageOf(err error) string {
	var upErr *apierr.UpstreamError
	if errors.As(err, &upErr) {
		return clientMessage(upErr)
	}
	return "the request failed"
}

// stringList 把"数组"或"单值"统一成 []string（表单里常见单值，JSON 里是数组）。
func stringList(values ...any) []string {
	var out []string
	for _, value := range values {
		switch typed := value.(type) {
		case nil:
			continue
		case string:
			if s := strings.TrimSpace(typed); s != "" {
				out = append(out, s)
			}
		case []any:
			for _, item := range typed {
				if s := jsonx.String(item); s != "" {
					out = append(out, s)
				}
			}
		case []string:
			out = append(out, typed...)
		}
	}
	return out
}
