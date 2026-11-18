package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MinterConfig 是 Turnstile 铸造服务的连接参数。
//
// URL 支持**逗号分隔多个地址**（多实例轮询 + 故障转移）：长期均匀分摊负载，
// 某实例失败立即转移到下一个，**全失败才返回 false**（调用方据此退回慢路径）。
type MinterConfig struct {
	URL     string
	Key     string
	Timeout time.Duration
	Retries int
	Reserve int
}

// Minter 是铸造服务客户端。
//
// 设计三原则：
//
//  1. **绝不报错**：铸造失败只是"闸门仍然拦着"的一种情形 —— 调用方据此退回"等闸门衰减"
//     的慢路径。把失败伪装成成功会让上游更糟。
//  2. **短超时 + 只走回环/内网**：这是能产出"绕过风控凭证"的服务，不对外暴露；
//     trust_env=false 是必须的 —— 代理会把 127.0.0.1 也拐走（原项目踩过）。
//  3. **只做一件事**：取回 token 字符串；不缓存（token 单次有效，缓存就是 bug 源）。
type Minter struct {
	urls    []string
	key     string
	timeout time.Duration
	retries int
	reserve int

	client *http.Client

	mu         sync.Mutex
	next       int
	lastSource string
	lastErr    string
}

// NewMinter 构造铸造客户端。URL 为空 ⇒ Configured() == false（等价于"没接这个能力"）。
func NewMinter(cfg MinterConfig) *Minter {
	var urls []string
	for _, raw := range strings.Split(cfg.URL, ",") {
		if u := strings.TrimRight(strings.TrimSpace(raw), "/"); u != "" {
			urls = append(urls, u)
		}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	transport := &http.Transport{
		// trust_env=false 的等价物：回环绝不能被 HTTP_PROXY 拐走。
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns:        8,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	return &Minter{
		urls:    urls,
		key:     cfg.Key,
		timeout: timeout,
		retries: maxInt(0, cfg.Retries),
		reserve: maxInt(0, cfg.Reserve),
		client:  &http.Client{Transport: transport, Timeout: timeout},
	}
}

// Configured 表示接了铸造能力。
func (m *Minter) Configured() bool { return m != nil && len(m.urls) > 0 }

// LastError 是最近一次失败归因（成功后清空）。
//
//	unreachable: <url> (<原因>)  = 网络层不可达（多为宿主防火墙丢包）
//	<url>: HTTP 503 / empty token = 到得了但铸造失败
//
// 仅供调用方拼 429 报文用，不参与任何控制流。
func (m *Minter) LastError() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// LastSource 是最近一次拿到的 token 来源（pool / live / ""=没取过），只用于自适应预约。
func (m *Minter) LastSource() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastSource
}

// Mint 取一个 token；任何失败都返回 ("", false)，调用方负责降级。
func (m *Minter) Mint(ctx context.Context) (string, bool) {
	if !m.Configured() {
		return "", false
	}
	m.mu.Lock()
	m.lastErr = ""
	next := m.next
	lastSource := m.lastSource
	m.mu.Unlock()

	n := len(m.urls)
	// 重试**只在单地址部署时**开。多地址部署的初衷是"一个坏了立刻换下一个"；
	// 在同址重试只会把故障转移拖慢。
	retries := m.retries
	if n > 1 {
		retries = 0
	}
	lastErr := ""
	for k := 0; k < n; k++ {
		i := (next + k) % n
		u := m.urls[i]
		for attempt := 0; attempt < 1+retries; attempt++ {
			more := attempt+1 < 1+retries
			// 自适应预约：live（池子空了）⇒ 声明 N 个提前量；pool（还有存货）⇒ 不再追加。
			// ⚠️ 服务端的显式 reserve 是**累加**的 —— 每条都传 N 会把池深顶到上限，
			// 等于"只要有流量就永久满池铸造"，主动抬高站点侧风控分。
			wantReserve := m.reserve
			if lastSource == "pool" {
				wantReserve = 0
			}
			token, source, err := m.mintOnce(ctx, u, wantReserve)
			if err != nil {
				lastErr = err.Error()
				m.setLastError(lastErr)
				if more {
					if !sleepCtx(ctx, 500*time.Millisecond*time.Duration(1<<attempt)) {
						return "", false
					}
					continue
				}
				break
			}
			if token == "" {
				lastErr = u + ": empty token"
				m.setLastError(lastErr)
				if more {
					if !sleepCtx(ctx, 500*time.Millisecond*time.Duration(1<<attempt)) {
						return "", false
					}
					continue
				}
				break
			}
			m.mu.Lock()
			m.next = (i + 1) % n
			m.lastSource = source
			// 🔴 重试成功也要把**本次**的失败归因清掉：否则下一次 429 的报文会带上一次
			// 重试留下的 `HTTP 503`，指向错误的根因。
			m.lastErr = ""
			m.mu.Unlock()
			return token, true
		}
	}
	return "", false
}

func (m *Minter) mintOnce(ctx context.Context, base string, reserve int) (token, source string, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	url := fmt.Sprintf("%s/v1/turnstile/mint?reserve=%d", base, reserve)
	req, buildErr := http.NewRequestWithContext(reqCtx, http.MethodPost, url, nil)
	if buildErr != nil {
		return "", "", fmt.Errorf("unreachable: %s (bad request)", base)
	}
	if m.key != "" {
		req.Header.Set("X-Minter-Key", m.key)
	}
	resp, doErr := m.client.Do(req)
	if doErr != nil {
		return "", "", fmt.Errorf("unreachable: %s (%s)", base, classifyMinterErr(doErr))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("%s: HTTP %d", base, resp.StatusCode)
	}
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return "", "", fmt.Errorf("%s: empty token", base)
	}
	token, _ = parsed["token"].(string)
	source, _ = parsed["source"].(string)
	if token == "" {
		return "", source, fmt.Errorf("%s: empty token", base)
	}
	return token, source, nil
}

func (m *Minter) setLastError(msg string) {
	m.mu.Lock()
	m.lastErr = msg
	m.mu.Unlock()
}

func classifyMinterErr(err error) string {
	var netErr net.Error
	if ok := asNetError(err, &netErr); ok && netErr.Timeout() {
		return "timeout"
	}
	return fmt.Sprintf("%T", err)
}

func asNetError(err error, target *net.Error) bool {
	for err != nil {
		if ne, ok := err.(net.Error); ok {
			*target = ne
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
