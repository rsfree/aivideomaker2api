// Package media 是成片对外出口：把上游直链换成本服务自己的下载地址，取用时**流式代理**转发。
//
// 为什么必须存在：上游（网页端）返回的成片是一个**公开直链**，实测形态：
//
//	https://static2.img2video.ai/1789479777409-…-1635002_0_minimax_h3_1635002.mp4
//	        └────── 域名：上游服务商 ──────┘                 └─ 上游实际模型名 ─┘
//
// 原样透传等于一次泄露三样东西：
//
//  1. **域名** —— 直接指向上游服务商；
//  2. **路径里的文件名** —— 内嵌 `minimax_h3`，即上游**实际执行**的模型；
//  3. 🔴 **响应头 `content-disposition`** —— 上游回的是
//     `attachment; filename="1635002_0_minimax_h3_1635002.mp4"`。这一条最隐蔽：
//     **即使只把 URL 换成自己的，只要还在转发上游响应头，模型名就跟着每一次下载暴露**。
//     所以本包对响应头做**白名单**：`server` / `cf-ray` / `nel` / `report-to` 这些同样能
//     指认上游的头一律不透传。
//
// 对外形态：`{public_base}/v/cgt-20260916-a1b2c3d4.mp4` —— 三段全是我们自己的。
// 链接**立即有效**，不依赖任何搬运动作是否完成（任务照实报 succeeded，不引入
// "成功了但拿不到地址"的自相矛盾中间态）；下载时才回源，没被取用的成片不占任何存储。
//
// ⚠️ **绝不提供"传任意 URL 给我换一个链接"的接口**：那等于开放代理 + SSRF，还白送别人
// 用你的带宽与出口 IP 做转存。下载地址只能由服务**自己**在观测到任务成功时给出。
package media

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// DefaultPath 是下载端点的默认前缀。写成 `/` 会让下载路由变成 `/{name}` —— 一条**通配**
// 路径，把每一条未知路径都吃掉，既遮蔽正常的 404 语义，也等于给调用方多开一个入口。
const DefaultPath = "/v"

const defaultExt = ".mp4"

var allowedExts = map[string]bool{".mp4": true, ".m4v": true, ".mov": true, ".webm": true, ".mkv": true}

// passThrough 是**只**透传的响应头。白名单而非黑名单：上游以后新增什么头都不会顺着
// 这条管子漏给调用方（cf-ray / nel / report-to / server 都曾在上游响应里出现过）。
var passThrough = []string{"content-length", "content-range", "accept-ranges", "etag", "last-modified"}

// Proxy 是成片下载出口。未配置 publicBase 时 Enabled() 为 false ⇒ 行为与从前一致
// （原样透传上游链接）。
type Proxy struct {
	publicBase string
	path       string
	client     *http.Client
	// passthrough 为 true 时**不改写**对外链接：video_url 直接用上游地址。
	//
	// 为什么要有这个开关：改写会让我们替上游扛全部下载带宽（成片几百 MB × 每条任务），
	// 而调用方多数只是把链接转手给终端用户 —— 直连上游更省，也更少一个故障点。
	// /v/ 端点仍然保留：历史链接不失效，回源能力也还在。
	passthrough bool
}

// New 构造出口。readTimeout 是**空闲**超时语义（连接没在动才算卡死），
// 不用"总超时"——成片可能几百 MB，任何固定总时长都会变成"文件越大越容易失败"。
func New(publicBase, mediaPath string, readTimeout time.Duration, passthrough bool) *Proxy {
	if readTimeout <= 0 {
		readTimeout = 60 * time.Second
	}
	p := strings.TrimRight(strings.TrimSpace(publicBase), "/")
	prefix := "/" + strings.Trim(strings.TrimSpace(mediaPath), "/")
	if prefix == "/" || prefix == "" {
		prefix = DefaultPath
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	return &Proxy{
		publicBase:  p,
		path:        prefix,
		passthrough: passthrough,
		client: &http.Client{
			Transport: transport,
			Timeout:   readTimeout,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

// Enabled 表示出口端点已配置（/v/ 可用）。
func (p *Proxy) Enabled() bool { return p != nil && p.publicBase != "" }

// Rewrites 表示**对外链接是否改写成 /v/**。
//
// Enabled 与 Rewrites 是两件事：passthrough 模式下端点仍然可用（历史链接、回源），
// 只是新任务的 video_url 直接给上游地址。
func (p *Proxy) Rewrites() bool { return p.Enabled() && !p.passthrough }

// Path 是下载端点前缀。
func (p *Proxy) Path() string { return p.path }

// Describe 是 /healthz 用的形态描述（不含任何凭据）。
func (p *Proxy) Describe() map[string]any {
	return map[string]any{
		"enabled": p.Enabled(), "rewrites": p.Rewrites(),
		"public_base": p.publicBase, "path": p.path,
	}
}

// DownloadURL 给出对外地址：`{public_base}{path}/{id}{ext}`。
func (p *Proxy) DownloadURL(id, sourceURL string) string {
	ext := ExtensionOf(sourceURL)
	return p.publicBase + p.path + "/" + id + ext
}

// ExtensionOf 从上游地址取扩展名 —— **只认白名单后缀**，其余归 .mp4。
//
// 绝不复用上游文件名：那里面就有模型名（`…_0_minimax_h3_….mp4`）。
func ExtensionOf(sourceURL string) string {
	u, err := url.Parse(sourceURL)
	if err != nil {
		return defaultExt
	}
	ext := strings.ToLower(path.Ext(u.Path))
	if allowedExts[ext] {
		return ext
	}
	return defaultExt
}

// StripExt 把 `cgt-…-a1b2c3d4.mp4` 还原成 id（用于按 id 查任务记录）。
func StripExt(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if allowedExts[ext] {
		return strings.TrimSuffix(name, path.Ext(name))
	}
	return name
}

// Serve 回源并流式转发。Range 原样透传（播放器会发它）。
//
// 回源失败时只回一句通用描述：出口那一跳不携带任何上游标识。
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, sourceURL, filename string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, sourceURL, nil)
	if err != nil {
		http.Error(w, "the video source is temporarily unavailable from this service", http.StatusBadGateway)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	req.Header.Set("user-agent", "aivideomaker2api-media/1")
	resp, err := p.client.Do(req)
	if err != nil {
		http.Error(w, "the video source is temporarily unavailable from this service", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		http.Error(w, "the video source is temporarily unavailable from this service", http.StatusBadGateway)
		return
	}

	out := w.Header()
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "video/mp4"
	}
	out.Set("Content-Type", contentType)
	// ★ 脱敏第三面：上游的 content-disposition 里带着模型名，我们自己写一份。
	out.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	for _, name := range passThrough {
		if v := resp.Header.Get(name); v != "" {
			out.Set(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
