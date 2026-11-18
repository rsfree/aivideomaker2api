// Package apierr 是本服务的错误边界。
//
// 三个来源各自有**明确归属**，因为对外的处理方式完全不同：
//
//	ParamError     调用方的请求本身不合法        → 400 InvalidParameter（报文可以直接给）
//	UpstreamError  上游（网页端内部接口）报错     → 按语义映射 HTTP/code，且**必须脱敏**
//	普通 error     我们自己/环境的问题            → 502 / 500，报文只给类别级事实
//
// 为什么单独成包：翻译层、槽位解析、上游客户端都要产生同一类错误，而它们之间不能互相
// import（translate → slots 已经是单向）。把边界类型放在最底层，三个包都只依赖它。
package apierr

import "fmt"

// ParamError 是调用方请求本身不合法（对应 Ark 的 InvalidParameter）。
type ParamError struct {
	Message string
	Param   string
}

func (e *ParamError) Error() string { return e.Message }

// Param 构造一个参数错误。
func Param(message, param string) *ParamError {
	return &ParamError{Message: message, Param: param}
}

// UpstreamError 是 web 逆向线（tRPC over /api）返回的错误。
//
// Transport 是**传输层**异常名（`ConnectError` / `ReadTimeout` / …），只在"根本没拿到
// 响应"时填。它的用途是让**对外脱敏**后的报文仍能区分"超时（可重试）"与"连不上"
// —— 这类事实调用方需要，而且不含任何内部标识。
//
// MinterLastError 只在 Turnstile 铸造失败时填（归因：`unreachable: …` = 网络层到不了；
// `HTTP 503` = 到得了但铸造失败）。两者的修法南辕北辙，报文必须能分开。
type UpstreamError struct {
	Procedure       string
	Message         string
	Code            string
	HTTPStatus      int
	Transport       string
	MinterLastError string
}

func (e *UpstreamError) Error() string {
	if e.Procedure == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Procedure, e.Message)
}

// NewUpstream 构造上游错误；code 为空时落到 TRPC_ERROR。
func NewUpstream(procedure, message, code string, httpStatus int) *UpstreamError {
	if code == "" {
		code = "TRPC_ERROR"
	}
	return &UpstreamError{Procedure: procedure, Message: message, Code: code, HTTPStatus: httpStatus}
}

// CaptchaRequired 是动态验证码闸门开启时的特化（→ 429 RateLimitExceeded）。
func CaptchaRequired(message, minterLastError string) *UpstreamError {
	return &UpstreamError{
		Procedure:       "ai.minimaxH3",
		Message:         message,
		Code:            "CAPTCHA_REQUIRED",
		HTTPStatus:      429,
		MinterLastError: minterLastError,
	}
}
