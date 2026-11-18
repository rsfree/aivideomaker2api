// Package upstream 是 aivideomaker 网页端内部接口的客户端与提交闸门。
//
// 上游只有一条：**tRPC over `/api`，会话 cookie 认证**。
//
//	GET  /api/{procedure}?batch=1&input={"0":{"json":<input>}}
//	POST /api/{procedure}?batch=1&input=…   body: {"0":{"json":<input>}}
//
// 注意路径**没有 /trpc 前缀**，procedure 直接拼在 `/api` 之后（如 `/api/auth.user`）。
//
// 这条线有三个必须写进代码的事实：
//
//  1. **没有取消端点。** 任务一旦提交，只能删本地记录，上游照跑照扣。
//  2. **并发按账号额度**（premium 2 / pro 4），槽位从创建一直占到任务进入终态 ——
//     不是"创建请求返回就释放"。
//  3. **没有幂等键。** POST 重发一次就可能建出两个任务，而建任务就是计费动作
//     ⇒ "能不能重试"必须按"上游到底有没有接单"来判，不能用"失败就再试一次"。
//
// 所以本包把"提交"拆成三件事：闸门（Queue）、单次提交（createOnce）、
// 幂等安全边界内的重试（Create）。三者分开才能各自被测试钉住。
package upstream

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/jushenzhidao/aivideomaker2api/internal/apierr"
)

// 站点约定。
const (
	Page             = "/zh/ai-video-generator"
	ListPage         = "/zh/generations"
	DefaultVisitorID = "f29ee26edcb4e8b96ee17e277a384f6f"
	// CreateProcedure 是建任务 procedure 的**默认值 / 回落目标**。
	// 它是唯一已实测存在的生成 procedure（三次真实提交的成片 URL 里一律是 minimax_h3）。
	// 有渠道配置时由 slots.ResolveModel 解析出的槽位覆盖（见 createOnce）。
	CreateProcedure = "ai.minimaxH3"
	UserAgent       = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"
)

// voidInput 是 tRPC 的"无参数"编码：null + meta 标记 undefined。
var voidInput = map[string]any{"0": map[string]any{
	"json": nil,
	"meta": map[string]any{"values": []any{"undefined"}, "v": 1},
}}

// Config 是一个账号的上游客户端配置。
type Config struct {
	BaseURL   string
	Cookie    string
	UserID    string
	VisitorID string

	Timeout      time.Duration
	ProbeTimeout time.Duration
	TrustEnv     bool

	SubmitRetries      int
	SubmitRetryBackoff time.Duration
	MediaRetries       int
	MediaFetchTimeout  time.Duration
	CaptchaProbeTTL    time.Duration

	Minter *Minter

	// Logf 是可选的日志钩子（账号名由调用方在包装里带上）。
	Logf func(format string, args ...any)
}

func (c Config) withDefaults() Config {
	if c.BaseURL == "" {
		c.BaseURL = "https://aivideomaker.ai"
	}
	if c.VisitorID == "" {
		c.VisitorID = DefaultVisitorID
	}
	if c.Timeout <= 0 {
		c.Timeout = 120 * time.Second
	}
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = 8 * time.Second
	}
	if c.SubmitRetryBackoff < 0 {
		c.SubmitRetryBackoff = 0
	}
	if c.MediaFetchTimeout <= 0 {
		c.MediaFetchTimeout = 30 * time.Second
	}
	return c
}

// newHTTPClient 建一个独立连接池的客户端。
//
// 每账号一个 Transport 是刻意的：账号隔离不只是凭据隔离，还包括连接池与限速影响面
// —— 一个账号的上游抖动不应该把别人的连接占满。
func newHTTPClient(cfg Config) *http.Client {
	transport := &http.Transport{
		Proxy:                 nil, // trust_env=false 时必须真的不走代理（回环实测会被 HTTP_PROXY 拐走）
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if cfg.TrustEnv {
		transport.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{Transport: transport, Timeout: cfg.Timeout}
}

// probeClient 是**只读探测**用的短超时客户端（出口代理的 TLS 握手实测抖到 10s+，
// 一发卡住的探测会把重试循环占满一整个默认超时）。
func newProbeClient(cfg Config, parent *http.Client) *http.Client {
	transport, _ := parent.Transport.(*http.Transport)
	clone := transport.Clone()
	return &http.Client{Transport: clone, Timeout: cfg.ProbeTimeout}
}

// ParamError / UpstreamError 是错误边界的别名，便于本包内部直接构造。
type ParamError = apierr.ParamError

// UpstreamError 是上游错误。
type UpstreamError = apierr.UpstreamError

// safeResendTransports 是**能证明请求一个字节都没发出去**的传输错误 ⇒ 写请求唯一允许
// 重发的传输错误。
//
// 反例（刻意不在表里）：
//   - ReadTimeout       请求已送达，上游可能已经建好任务，只是响应慢了；
//   - WriteTimeout      请求体写到一半，上游可能已按完整请求处理；
//   - 对端已开始回应后断开。
//
// 这三种正是生产日志里最常见的，**只能靠加超时减少**，不能重试。
var safeResendTransports = map[string]bool{
	"ConnectError": true, "ConnectTimeout": true, "PoolTimeout": true,
	"ProxyError": true, "LocalProtocolError": true, "DialError": true,
}
