// Package config 是 config.json + 环境变量覆盖的配置层。
//
// 为什么是 JSON 而不是纯 env：本服务的参数按**语义分组**（账号池 / 闸门 / 转存 / 铸造 /
// 面板），全部铺平成 env 之后没人能一眼看出"哪些是一组、哪两个必须成对"。JSON 里能写
// `_comment`，也能把"必须成对看的两个值"放在同一层。
//
// 敏感值（api_key / minter key / 面板口令）**建议只走环境变量** —— 配置文件会进版本库，
// 而环境变量不会。两者的优先级：环境变量 > config.json。
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

// Duration 是"能写 `300s` 也能写 `300`"的时长解析。
//
// 纯数字按**秒**处理：配置文件里写 `"timeout": 120` 的人想的一定是 120 秒，
// 不是 120 纳秒（Go 的裸整数时长是这个语义，踩一次就是一次线上事故）。
type Duration time.Duration

func (d *Duration) UnmarshalJSON(raw []byte) error {
	text := strings.TrimSpace(string(raw))
	if text == "null" {
		return nil
	}
	if strings.HasPrefix(text, `"`) {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil
		}
		if n, err := strconv.Atoi(s); err == nil {
			*d = Duration(time.Duration(n) * time.Second)
			return nil
		}
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil {
		return err
	}
	*d = Duration(time.Duration(n * float64(time.Second)))
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Get 取 time.Duration。
func (d Duration) Get() time.Duration { return time.Duration(d) }

// Config 是完整配置。
type Config struct {
	Listen  string `json:"listen"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"`
	// PublicBase 是**对外可访问的本服务地址**（如 https://avm2api.1task.cn）。
	// 留空 = 成片出口关闭，行为与从前逐字节一致（原样透传上游链接）。
	PublicBase string `json:"public_base"`
	MediaPath  string `json:"media_path"`
	// MediaPassthrough 为 true 时，对外 video_url **直接给上游地址**，不改写成本服务的 /v/。
	//
	// 与"关掉出口"不同：/v/ 端点仍然可用（历史链接不失效、回源能力还在），
	// 只是新任务不再让本服务替上游扛下载带宽。
	MediaPassthrough bool `json:"media_passthrough"`

	AuthDir   string `json:"auth_dir"`
	StateFile string `json:"state_file"`

	TaskStore TaskStoreConfig `json:"task_store"`
	Server    ServerConfig    `json:"server"`
	Submit    SubmitConfig    `json:"submit"`
	Queue     QueueConfig     `json:"queue"`
	Upstream  UpstreamConfig  `json:"upstream"`
	Minter    MinterConfig    `json:"minter"`
	// Official 是官方 REST API（`/api/v1/*`）的凭据；api_key 非空即启用。
	// 与网页会话并存：720p/1080p 走官方（无闸门），480p 仍走网页免费档。
	Official OfficialConfig `json:"official"`
	Pool     PoolConfig     `json:"pool"`
	Panel    PanelConfig    `json:"panel"`
	Logging  LoggingConfig  `json:"logging"`
}

// TaskStoreConfig 是任务表参数。path 留空 = 内存（**显式**开关，不是持久化失败后的兜底）。
type TaskStoreConfig struct {
	Path          string   `json:"path"`
	RetentionDays int      `json:"retention_days"`
	CacheTTL      Duration `json:"cache_ttl"`
}

// ServerConfig 是 HTTP 服务参数。
type ServerConfig struct {
	ReadTimeout  Duration `json:"read_timeout"`
	WriteTimeout Duration `json:"write_timeout"`
}

// SubmitConfig 是"受理/推进分离"的推进侧参数。
type SubmitConfig struct {
	Workers      int      `json:"workers"`
	Lease        Duration `json:"lease"`
	Retries      int      `json:"retries"`
	RetryBackoff Duration `json:"retry_backoff"`

	// TransportRetries 是"连接层失败"（拒绝连接 / DNS / 握手）的换号重试次数。
	// 这类失败**明确没送达**，重试零副作用，默认开 1 次。
	TransportRetries int `json:"transport_retries"`
	// TimeoutRetries 是"请求已发出但没等到响应"（ReadTimeout）的换号重试次数。
	//
	// 🔴 默认 **0（关闭）**：上游很可能已经建了任务，重试会造出第二条。
	// 免费档不花钱，但要吃一次额度 + 一次闸门计数；付费档就是实打实的重复扣费。
	// 站点没有任何幂等键可供去重（`model.listModel` 只返回已成功的记录，查不到在途），
	// 所以这个开关是**风险自担**的。
	TimeoutRetries int `json:"timeout_retries"`
	// RuntimeRetries 是任务在**上游运行期**失败（上游接了单、跑完回 failed）后的自动重提次数。
	//
	// 这类失败站点只给 `status=failed`，不给原因，其中混着运行时内容审核 ——
	// 那种重试必然再失败，还会多烧一次额度。所以上限默认只给 1 次：
	// 一次是"救抖动"，两次以上就是"放大失败"。
	RuntimeRetries int      `json:"runtime_retries"`
	RuntimeBackoff Duration `json:"runtime_backoff"`

	// Inline 让推进侧同步执行（只给测试/本地调试；生产误开等于关掉"受理即返"）。
	Inline bool `json:"inline"`
}

// QueueConfig 是提交闸门参数。
type QueueConfig struct {
	// MaxInFlight > 0 时用它（显式覆盖优先）；= 0 时按账号额度自动
	// （读 ai.queryUserPermission.maxQueueLength：premium 2 / pro 4）。
	MaxInFlight    int      `json:"max_in_flight"`
	AcquireTimeout Duration `json:"acquire_timeout"`
	PollInterval   Duration `json:"poll_interval"`
	WatchTimeout   Duration `json:"watch_timeout"`
}

// UpstreamConfig 是上游客户端参数。
type UpstreamConfig struct {
	Timeout      Duration `json:"timeout"`
	ProbeTimeout Duration `json:"probe_timeout"`
	TrustEnv     bool     `json:"trust_env"`
	UserID       string   `json:"user_id"`
	VisitorID    string   `json:"visitor_id"`

	MediaFetchTimeout Duration `json:"media_fetch_timeout"`
	MediaRehostBudget Duration `json:"media_rehost_budget"`
	MediaRetries      int      `json:"media_retries"`
	RehostWorkers     int      `json:"rehost_workers"`
	CaptchaProbeTTL   Duration `json:"captcha_probe_ttl"`
	MediaReadTimeout  Duration `json:"media_read_timeout"`
}

// OfficialConfig 是官方 REST API 的 key（在「设置 → API 密钥」里生成，形如 ak_…）。
type OfficialConfig struct {
	APIKey  string   `json:"api_key"`
	BaseURL string   `json:"base_url"`
	Timeout Duration `json:"timeout"`
}

// Enabled 表示配置了官方 key。
func (c OfficialConfig) Enabled() bool { return strings.TrimSpace(c.APIKey) != "" }

// MinterConfig 是 Turnstile 铸造服务（留空 = 不启用该能力）。
type MinterConfig struct {
	URL     string   `json:"url"`
	Key     string   `json:"key"`
	Timeout Duration `json:"timeout"`
	Retries int      `json:"retries"`
	Reserve int      `json:"reserve"`
}

// PoolConfig 是账号池参数。
type PoolConfig struct {
	ProbeInterval Duration `json:"probe_interval"`
}

// PanelConfig 是内置面板。password 留空时回落到 api_key；两者都空 = 不校验（仅本机自用）。
type PanelConfig struct {
	Enabled  bool   `json:"enabled"`
	Password string `json:"password"`
}

// LoggingConfig 是日志参数。
type LoggingConfig struct {
	Level string `json:"level"`
}

// Default 返回推荐配置（也是"配置文件不存在时自动生成"的那份）。
func Default() Config {
	return Config{
		Listen:    ":8808",
		BaseURL:   "https://aivideomaker.ai",
		MediaPath: "/v",
		AuthDir:   "./auths",
		StateFile: "./data/state.json",
		TaskStore: TaskStoreConfig{Path: "./data/tasks.jsonl", RetentionDays: 7, CacheTTL: Duration(15 * time.Second)},
		Server:    ServerConfig{ReadTimeout: Duration(300 * time.Second)},
		Submit: SubmitConfig{
			Workers: 32, Lease: Duration(1200 * time.Second), Retries: 2, RetryBackoff: Duration(3 * time.Second),
			// 连接层失败（拒绝连接/DNS/握手）可以放心重试：请求确实没送达。
			TransportRetries: 1,
			// 读超时**默认不重试**：上游可能已建任务，重试会造出第二条（详见字段注释）。
			TimeoutRetries: 0,
			// 运行期失败重提 1 次 + 10s 退避。
			RuntimeRetries: 1,
			RuntimeBackoff: Duration(10 * time.Second),
		},
		Queue:    QueueConfig{AcquireTimeout: Duration(600 * time.Second), PollInterval: Duration(10 * time.Second), WatchTimeout: Duration(3600 * time.Second)},
		Upstream: UpstreamConfig{Timeout: Duration(120 * time.Second), ProbeTimeout: Duration(8 * time.Second), TrustEnv: true, MediaFetchTimeout: Duration(30 * time.Second), MediaRehostBudget: Duration(120 * time.Second), MediaRetries: 2, RehostWorkers: 7, CaptchaProbeTTL: Duration(5 * time.Second), MediaReadTimeout: Duration(60 * time.Second)},
		Minter:   MinterConfig{Timeout: Duration(25 * time.Second), Retries: 1, Reserve: 8},
		Official: OfficialConfig{BaseURL: "https://aivideomaker.ai", Timeout: Duration(60 * time.Second)},
		Pool:     PoolConfig{ProbeInterval: Duration(300 * time.Second)},
		Panel:    PanelConfig{Enabled: true},
		Logging:  LoggingConfig{Level: "info"},
	}
}

// Load 读配置：文件不存在时写一份推荐配置并返回它（首次启动即可跑）。
func Load(path string) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if writeErr := Save(path, cfg); writeErr != nil {
				return cfg, fmt.Errorf("config not found and could not be created: %w", writeErr)
			}
			applyEnv(&cfg)
			return cfg, nil
		}
		return cfg, err
	}
	// 先铺默认值再覆盖：缺字段的配置文件不该让服务带着零值启动
	// （零值的 timeout = 无超时，那是最危险的一种"看起来起来了"）。
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	applyEnv(&cfg)
	return cfg, cfg.Validate()
}

// Save 原子写配置（临时文件 + rename）。
func Save(path string, cfg Config) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if dir := parentDir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// applyEnv 用环境变量覆盖**敏感项与部署相关项**。
//
// 只覆盖这些：其余参数属于"该进配置文件"的东西，给每个都开一个 env 会让
// "这两个值必须成对"的知识重新散开。
func applyEnv(cfg *Config) {
	setStr(&cfg.Listen, "AVM2_LISTEN")
	setStr(&cfg.APIKey, "AVM2_API_KEY")
	setStr(&cfg.BaseURL, "AVM2_BASE_URL")
	setStr(&cfg.PublicBase, "AVM2_PUBLIC_BASE")
	setBool(&cfg.MediaPassthrough, "AVM2_MEDIA_PASSTHROUGH")
	setStr(&cfg.AuthDir, "AVM2_AUTH_DIR")
	setStr(&cfg.StateFile, "AVM2_STATE_FILE")
	setStr(&cfg.TaskStore.Path, "AVM2_TASK_STORE")
	setStr(&cfg.Logging.Level, "AVM2_LOG_LEVEL")
	setStr(&cfg.Minter.URL, "AVM_MINTER_URL")
	setStr(&cfg.Official.APIKey, "AVM_OFFICIAL_API_KEY")
	setStr(&cfg.Minter.Key, "AVM_MINTER_KEY")
	setStr(&cfg.Panel.Password, "AVM2_PANEL_PASSWORD")

	if raw := strings.TrimSpace(os.Getenv("AVM2_MAX_CONCURRENT")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			cfg.Queue.MaxInFlight = n
		}
	}
	if raw := strings.TrimSpace(os.Getenv("AVM2_TRUST_ENV")); raw != "" {
		cfg.Upstream.TrustEnv = !(raw == "0" || strings.EqualFold(raw, "false") || strings.EqualFold(raw, "no"))
	}
}

func setStr(dst *string, env string) {
	if raw, ok := os.LookupEnv(env); ok && strings.TrimSpace(raw) != "" {
		*dst = strings.TrimSpace(raw)
	}
}

// setBool 用环境变量覆盖布尔项。语义与 setStr 一致：**空值不算设置**，
// 这样"没配这个变量"和"配了空"得到同一个结果（沿用配置文件/默认值）。
func setBool(dst *bool, env string) {
	raw, ok := os.LookupEnv(env)
	if !ok {
		return
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	*dst = !(raw == "0" || strings.EqualFold(raw, "false") || strings.EqualFold(raw, "no"))
}

// Validate 把配置错误挡在起服务之前，而不是等第一个请求打进来。
func (c Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.Listen) == "" {
		problems = append(problems, "listen must not be empty")
	}
	if strings.TrimSpace(c.BaseURL) == "" {
		problems = append(problems, "base_url must not be empty")
	}
	if c.TaskStore.RetentionDays <= 0 {
		problems = append(problems, "task_store.retention_days must be > 0")
	}
	if c.Submit.Workers <= 0 {
		problems = append(problems, "submit.workers must be > 0")
	}
	if c.Queue.AcquireTimeout.Get() <= 0 {
		problems = append(problems, "queue.acquire_timeout must be > 0")
	}
	if c.Queue.WatchTimeout.Get() <= 0 {
		problems = append(problems, "queue.watch_timeout must be > 0")
	}
	if c.Upstream.Timeout.Get() <= 0 {
		problems = append(problems, "upstream.timeout must be > 0")
	}
	if c.Panel.Enabled && c.Panel.Password == "" && c.APIKey == "" {
		// 不算错误，但要明确告诉运维：面板此刻是开放的。
		// （这条提示由 main 打印，不阻断启动 —— 本机自用是合法形态。）
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

// PanelOpenWarning 判断"面板开放且对公网可达"这个组合该不该告警。
func (c Config) PanelOpenWarning() string {
	if !c.Panel.Enabled {
		return ""
	}
	if c.Panel.Password != "" || c.APIKey != "" {
		return ""
	}
	return "panel has NO password and api_key is empty — anyone who can reach this port can manage the " +
		"account pool. Set panel.password or api_key, or bind to 127.0.0.1."
}

func parentDir(path string) string {
	i := strings.LastIndex(path, string(os.PathSeparator))
	if i <= 0 {
		return ""
	}
	return path[:i]
}
