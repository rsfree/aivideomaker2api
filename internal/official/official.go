// Package official 是站点**官方 REST API**（`/api/v1/*`）的客户端。
//
// 为什么值得单独做一条通路（而不是只用网页 tRPC）：
//   - 官方 API 用 **API Key**（header `key: ak_…`）鉴权，**没有 cookie、没有 Turnstile 闸门**。
//     网页端那套"会话级闸门 + 10 分钟冷却 + 多号轮换"在这条路上根本不存在。
//   - 有文档（`/docs/aivideo-openapi.json`）、有 `quote`（先报价再下单）、有 `idempotencyKey`
//     （重复提交不会重复扣费）、有 `tasks` 列表 / `status` / `cancel`。
//   - 免费窗口一样在：实测 `720p/5s/turbo` → `credits=15, billable=false`（不扣费）。
//
// 代价是并发/日限由 key 自己带（`/api/v1/account` 里的 rpm / dailyCreditLimit）。
package official

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rsfree/aivideomaker2api/internal/apierr"
)

// Config 是官方通路的配置。
type Config struct {
	BaseURL string
	APIKey  string
	Timeout time.Duration
}

// Client 调官方 REST API。
type Client struct {
	cfg  Config
	http *http.Client
}

// New 建客户端。api_key 形如 `ak_…`（在「设置 → API 密钥」里生成）。
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("official: api key is required")
	}
	if !strings.HasPrefix(strings.TrimSpace(cfg.APIKey), "ak_") {
		return nil, fmt.Errorf("official: api key should start with ak_")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://aivideomaker.ai"
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	// 不读环境代理：沙箱代理会把带自定义 header 的 POST 吞掉（实测过）。
	return &Client{
		cfg: cfg,
		http: &http.Client{
			Timeout:   cfg.Timeout,
			Transport: &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 8},
		},
	}, nil
}

// Account 是 `GET /api/v1/account` 的结果（只取本服务用得到的字段）。
type Account struct {
	KeyName    string   `json:"key_name"`
	Balance    int      `json:"balance"`
	Models     []string `json:"models"`
	RPM        int      `json:"rpm"`
	DailyLimit int      `json:"daily_limit"`
}

type rawAccount struct {
	OK             bool `json:"ok"`
	CurrentBalance int  `json:"currentBalance"`
	Key            struct {
		Name                     string `json:"name"`
		Status                   string `json:"status"`
		DailyCreditLimit         int    `json:"dailyCreditLimit"`
		GenerationRequestsPerMin int    `json:"generationRequestsPerMinute"`
	} `json:"key"`
	SupportedModels []string `json:"supportedModels"`
}

// Account 读账号与 key 的限额（也用来做健康检查）。
func (c *Client) Account(ctx context.Context) (Account, error) {
	var raw rawAccount
	if err := c.call(ctx, http.MethodGet, "/api/v1/account", nil, &raw); err != nil {
		return Account{}, err
	}
	return Account{
		KeyName: raw.Key.Name, Balance: raw.CurrentBalance, Models: raw.SupportedModels,
		RPM: raw.Key.GenerationRequestsPerMin, DailyLimit: raw.Key.DailyCreditLimit,
	}, nil
}

// Quote 先报价（不下单）。billable=false 表示这次不扣积分。
func (c *Client) Quote(ctx context.Context, model string, body map[string]any) (map[string]any, error) {
	var out map[string]any
	if err := c.call(ctx, http.MethodPost, "/api/v1/quote/"+model, body, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Generate 下单。返回的 taskId 就是官方任务号（`/api/v1/tasks/{taskId}`）。
func (c *Client) Generate(ctx context.Context, model string, body map[string]any) (map[string]any, error) {
	var out map[string]any
	if err := c.call(ctx, http.MethodPost, "/api/v1/generate/"+model, body, &out); err != nil {
		return nil, err
	}
	if id := str(out["taskId"]); id == "" {
		return nil, apierr.NewUpstream("official.generate", "官方接口没有返回 taskId", "UPSTREAM_BAD_RESPONSE", 502)
	}
	return out, nil
}

// Task 查任务。官方会用 429 表达限速，这里保持原样透出。
func (c *Client) Task(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	if err := c.call(ctx, http.MethodGet, "/api/v1/tasks/"+id, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Cancel 取消任务。
func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPut, "/api/v1/tasks/"+id+"/cancel", nil, nil)
}

// ---------------------------------------------------------------- 内部 --

// call 发一次请求并按官方错误结构翻译错误：
// `{"status":"FAILED","errorCode":"INSUFFICIENT_CREDITS","message":"…"}`。
//
// 透传 message（调用方要看得懂原因），但**code 用官方 errorCode 原值**，
// 这样网关侧可以按 code 做重试/降级判断（例如 RATE_LIMITED 可重试、AUTH_FAILED 不可）。
func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return apierr.NewUpstream("official", "failed to encode body: "+err.Error(), "BAD_REQUEST", 0)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.BaseURL+path, reader)
	if err != nil {
		return apierr.NewUpstream("official", "failed to build request: "+err.Error(), "BAD_REQUEST", 0)
	}
	req.Header.Set("key", c.cfg.APIKey)
	req.Header.Set("accept", "application/json")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return apierr.NewUpstream("official", "transport error: "+err.Error(), "UPSTREAM_UNAVAILABLE", 0)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode/100 == 2 {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return apierr.NewUpstream("official", "bad json from upstream: "+err.Error(), "UPSTREAM_BAD_RESPONSE", resp.StatusCode)
		}
		return nil
	}

	// 官方错误体
	var e struct {
		Status    string `json:"status"`
		ErrorCode string `json:"errorCode"`
		Message   string `json:"message"`
		Error     string `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	code := e.ErrorCode
	if code == "" {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			code = "AUTH_FAILED"
		case http.StatusTooManyRequests:
			code = "RATE_LIMITED"
		case http.StatusPaymentRequired:
			code = "INSUFFICIENT_CREDITS"
		default:
			code = "UPSTREAM_ERROR"
		}
	}
	msg := firstNonEmpty(e.Message, e.Error, strings.TrimSpace(string(raw)))
	if len(msg) > 400 {
		msg = msg[:400]
	}
	return apierr.NewUpstream("official"+strings.ReplaceAll(path, "/", "."), msg, code, resp.StatusCode)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
