package server

import (
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/rsfree/aivideomaker2api/internal/apierr"
	"github.com/rsfree/aivideomaker2api/internal/pool"
	"github.com/rsfree/aivideomaker2api/internal/translate"
	"github.com/rsfree/aivideomaker2api/internal/upstream"
)

// 对外错误码白名单。
//
// 🔴 不许把上游的 code 原样透传：tRPC 会把**站点自己的**错误码带进来，那是上游的内部
// 词表 —— 出现在我们的 code 字段里就是泄漏，也是把实现细节写进了对外契约。
const (
	codeInvalidParameter = "InvalidParameter"
	codeAuthentication   = "AuthenticationError"
	codeRateLimited      = "RateLimitExceeded"
	codeQueueFull        = "TaskQueueFull"
	codeTaskNotFound     = "TaskNotFound"
	codeContentFiltered  = "ContentFiltered"
	codeSubmitFailed     = "SubmissionFailed"
	codeUpstreamError    = "UpstreamError"
	codeUpstreamDown     = "UpstreamUnavailable"
)

// 上游能直接对应的状态码白名单。
//
// **504 是我们自己产生的**（取参考文件链接或整个转存阶段超过预算）—— 不放进来的话会被
// 压成 502，调用方就分不清"上游坏了"与"你给的链接太慢/太大"。
var passThroughStatus = map[int]bool{400: true, 401: true, 403: true, 404: true, 429: true, 504: true}

const submissionFailedMessage = "the task could not be submitted to the upstream"

// statusToHTTP 把上游错误映射成对外状态码。
func statusToHTTP(err *apierr.UpstreamError) int {
	switch {
	case err == nil:
		return http.StatusBadGateway
	case err.Code == "CAPTCHA_REQUIRED" || err.Code == "CAPTCHA_REJECTED" || err.Code == "QUEUE_TIMEOUT":
		return http.StatusTooManyRequests
	case upstream.IsContentRejection(err):
		// 内容拒绝 ⇒ 400（方舟官方同类错误就是 400）。不识别就会被压成 502，
		// 调用方误当服务故障反复重试（重试必败，白耗配额）。
		return http.StatusBadRequest
	case err.Procedure == "download" && err.HTTPStatus == http.StatusForbidden:
		// 引用素材链接不可用：这是**调用方提供的素材**问题（请求内容不合法），
		// 不是权限问题 —— 原样回 403 会被误读成"我的凭据过期了"，触发无谓的换 key。
		return http.StatusBadRequest
	case passThroughStatus[err.HTTPStatus]:
		return err.HTTPStatus
	}
	return http.StatusBadGateway
}

// codeFor 把内部错误归到**对外**错误码白名单。
func codeFor(err *apierr.UpstreamError) string {
	if upstream.IsContentRejection(err) {
		return codeContentFiltered
	}
	switch err.Code {
	case "CAPTCHA_REQUIRED":
		return codeRateLimited
	case "QUEUE_TIMEOUT":
		return codeQueueFull
	case "NOT_FOUND":
		return codeTaskNotFound
	}
	return codeUpstreamError
}

// clientMessage 把上游错误翻成**对外可用**的一句话：不出现任何内部标识。
//
// 🔴 为什么必须做：上游错误原文里塞满了内部/实现标识，而它会原样进客户端报文：
// 站点 procedure 名、铸造服务主机与端口、运维口令、内部 runbook 编号、上游原始报文。
// 这些是白送的攻击面与实现细节，调用方拿到也办不了事。
//
// 「失败在**哪一阶段**」对调用方是**可行动的**（"你给的链接慢" vs "上游自己慢"），
// 而 procedure 名是内部标识 ⇒ 翻成阶段说法，原文一律不出现。
//
// ⚠️ 这里**不拼 Request ID** —— 那件事只由 writeError 负责（信封层统一加）。
func clientMessage(err *apierr.UpstreamError) string {
	timedOut := err.HTTPStatus == http.StatusGatewayTimeout ||
		strings.Contains(err.Transport, "Timeout") || err.Transport == "Timeout"
	phase := ""
	switch err.Procedure {
	case "download":
		phase = "fetching a reference file you supplied"
	case "uploads.getPresignedUrl":
		phase = "preparing the reference upload"
	case "uploads.PUT":
		phase = "uploading a reference file to the upstream"
	}

	// `SUBMISSION_REJECTED` 的 Message 是**我们自己写的、已脱敏**的（含余额这类事实），
	// 原样给调用方比重新生成一句泛化话有用。它不含 procedure 名 / 主机 / 凭据。
	if err.Code == "SUBMISSION_REJECTED" {
		return err.Message
	}

	var why string
	switch {
	case phase != "":
		if timedOut {
			why = phase + " did not finish within the configured budget — a faster or smaller file may help"
		} else {
			why = phase + " failed"
		}
	case upstream.IsContentRejection(err):
		why = "the input content was rejected by the upstream's content policy — adjust the prompt or the " +
			"reference media and retry; repeating the same request will fail again"
	case err.Code == "CAPTCHA_REQUIRED":
		why = "the upstream currently requires a fresh captcha token for this account (a dynamic, " +
			"velocity-based gate, not an account property); wait for the gate to decay, or spread your " +
			"submissions out"
	case err.Code == "CAPTCHA_REJECTED":
		why = "the upstream rejected a freshly minted captcha token (it is gating this account or network " +
			"harder than a plain token can satisfy); wait for the gate to decay, slow down, or use a " +
			"different egress/session"
	case err.Code == "QUEUE_TIMEOUT":
		why = "the upstream concurrency limit is saturated and no slot was freed within the wait budget; " +
			"retry later or lower your concurrency"
	case err.Code == "NOT_FOUND":
		why = "the upstream has no record of this task"
	case timedOut:
		// 🔴 **绝不能**建议"重试一下"：读超时时上游可能已经把任务建起来了。
		// 实测（2026-11-08）：一次 `720p/base/5s` 提交读超时，我们记 `failed` + `paid=null`，
		// 但余额随后少了 5 积分 —— 上游确实建了任务并出了片。此时重试 = 再建一条 = 再扣一次钱。
		// 正确的话是"先去上游核对，再决定要不要重发"。
		why = "the upstream did not respond in time: it may or may not have created the task " +
			"(a task created this way is not tracked here and may still be billed). Check the " +
			"upstream task list before resubmitting — a blind retry can create and charge a second task"
	case err.HTTPStatus == http.StatusTooManyRequests:
		why = "the upstream rate-limited this request"
	case err.Transport != "":
		why = "the upstream is unreachable"
	case err.HTTPStatus >= 400 && err.HTTPStatus < 500:
		why = "the upstream rejected this request"
	default:
		why = "the upstream call failed"
	}
	tail := ""
	if err.HTTPStatus != 0 {
		tail = " (HTTP " + itoa(err.HTTPStatus) + ")"
	}
	return "The request failed because " + why + tail + "."
}

// writeError 输出**方舟形状**的错误信封（code / message / param / type）。
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadGateway
	code := codeUpstreamError
	message := "The request failed because the upstream call failed."
	param := ""

	var paramErr *apierr.ParamError
	var upErr *apierr.UpstreamError
	var noAccount *pool.ErrNoAccount

	switch {
	case errors.As(err, &paramErr):
		status, code, message, param = http.StatusBadRequest, codeInvalidParameter, paramErr.Message, paramErr.Param
	case errors.Is(err, upstream.ErrNoSlot):
		status, code = http.StatusTooManyRequests, codeQueueFull
		message = "no upstream slot was freed within the wait budget; retry later or lower your concurrency"
	case errors.As(err, &noAccount):
		status, code = http.StatusServiceUnavailable, codeUpstreamDown
		message = "no usable account in the pool: " + noAccount.Reason
	case errors.As(err, &upErr):
		status, code, message = statusToHTTP(upErr), codeFor(upErr), clientMessage(upErr)
		// 全量原文留在**服务日志**里（脱敏只作用于出口）。
		s.log.Warn("upstream error", "code", upErr.Code, "status", upErr.HTTPStatus,
			"procedure", upErr.Procedure, "detail", upErr.Error())
	default:
		s.log.Error("unhandled error", "err", err)
	}

	envelope := map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": withRequestID(message, requestID(r)),
			"param":   param,
			"type":    "InvalidRequest",
		},
	}
	writeJSON(w, status, envelope)
}

// withRequestID 让 message 以 `Request ID: {id}` 结尾 —— **全服务唯一负责这件事的地方**。
//
// 两个作用：① 与官方形态一致；② **脱敏之后调用方仍有唯一抓手** —— 报障时报这个 ID，
// 运维能在日志里捞到全量上游原文。幂等：已经有就不再拼。
func withRequestID(message, rid string) string {
	if rid == "" || strings.Contains(message, "Request ID:") {
		return message
	}
	return message + " Request ID: " + rid
}

// requireAuth 校验 Bearer（key 模式下）。常量时间比较，避免按字符泄露密钥。
func (s *Server) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.APIKey == "" {
		return true
	}
	token := bearer(r)
	if !hmac.Equal([]byte(token), []byte(s.cfg.APIKey)) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
			"code":    codeAuthentication,
			"message": withRequestID("invalid or missing bearer token", requestID(r)),
			"param":   "",
			"type":    "InvalidRequest",
		}})
		return false
	}
	return true
}

func bearer(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && strings.EqualFold(auth[:7], "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

// dryRun 判定：头 `X-Avm-Dry-Run: 1` 或 `extra_body.aivideomaker_dry_run=true`。
func dryRun(r *http.Request, body map[string]any) bool {
	if r.Header.Get("X-Avm-Dry-Run") == "1" {
		return true
	}
	extra := map[string]any(nil)
	if m, ok := body["extra_body"].(map[string]any); ok {
		extra = m
	}
	if extra != nil {
		if b, ok := extra["aivideomaker_dry_run"].(bool); ok && b {
			return true
		}
	}
	return false
}

// ensure translate is referenced (文档锚点：错误语义与翻译层同源).
var _ = translate.BillingNote

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
