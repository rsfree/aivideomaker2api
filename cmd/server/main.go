// 命令 server 是 aivideomaker2api 的入口：加载配置、建账号池、起 HTTP 服务。
//
// 启动顺序刻意是"配置 → 日志 → 存储 → 池 → 面板 → 服务"：每一层都只依赖前面已经
// 校验过的东西，任何一层失败都在**监听端口之前**退出 —— 不会有"起来了但半个功能没有"
// 的中间态。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rsfree/aivideomaker2api/internal/config"
	"github.com/rsfree/aivideomaker2api/internal/debughist"
	"github.com/rsfree/aivideomaker2api/internal/media"
	"github.com/rsfree/aivideomaker2api/internal/official"
	"github.com/rsfree/aivideomaker2api/internal/panel"
	"github.com/rsfree/aivideomaker2api/internal/pool"
	"github.com/rsfree/aivideomaker2api/internal/server"
	"github.com/rsfree/aivideomaker2api/internal/store"
	"github.com/rsfree/aivideomaker2api/internal/upstream"
)

// appVersion 透出到 /healthz 与面板。
const appVersion = "1.0.0"

const serviceName = "aivideomaker2api"

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（不存在时会自动生成一份推荐配置）")
	listen := flag.String("listen", "", "覆盖配置里的监听地址，如 :8808")
	healthcheck := flag.Bool("healthcheck", false, "只探活本机 /healthz 并退出（给容器 HEALTHCHECK 用）")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck(*listen))
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "config not found: %s\n", *cfgPath)
		} else {
			fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		}
		os.Exit(1)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.Logging.Level)
	slog.SetDefault(logger)

	// ---- 任务表 ----
	st, err := store.Open(cfg.TaskStore.Path, time.Duration(cfg.TaskStore.RetentionDays)*24*time.Hour)
	if err != nil {
		logger.Error("open task store failed", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	if removed, pruneErr := st.Prune(); pruneErr != nil {
		logger.Warn("prune expired tasks failed (service continues)", "err", pruneErr)
	} else if removed > 0 {
		logger.Info("pruned expired task records", "removed", removed)
	}

	// ---- 铸造服务（可选）----
	var minter *upstream.Minter
	if strings.TrimSpace(cfg.Minter.URL) != "" {
		minter = upstream.NewMinter(upstream.MinterConfig{
			URL:     cfg.Minter.URL,
			Key:     cfg.Minter.Key,
			Timeout: cfg.Minter.Timeout.Get(),
			Retries: cfg.Minter.Retries,
			Reserve: cfg.Minter.Reserve,
		})
	}

	// ---- 账号池 ----
	p, err := pool.New(pool.Options{
		AuthDir:        cfg.AuthDir,
		StateFile:      cfg.StateFile,
		MaxInFlight:    cfg.Queue.MaxInFlight,
		AcquireTimeout: cfg.Queue.AcquireTimeout.Get(),
		PollInterval:   cfg.Queue.PollInterval.Get(),
		WatchTimeout:   cfg.Queue.WatchTimeout.Get(),
		ProbeInterval:  cfg.Pool.ProbeInterval.Get(),
		Upstream: upstream.Config{
			BaseURL:            cfg.BaseURL,
			UserID:             cfg.Upstream.UserID,
			VisitorID:          cfg.Upstream.VisitorID,
			Timeout:            cfg.Upstream.Timeout.Get(),
			ProbeTimeout:       cfg.Upstream.ProbeTimeout.Get(),
			TrustEnv:           cfg.Upstream.TrustEnv,
			SubmitRetries:      cfg.Submit.Retries,
			SubmitRetryBackoff: cfg.Submit.RetryBackoff.Get(),
			MediaRetries:       cfg.Upstream.MediaRetries,
			MediaFetchTimeout:  cfg.Upstream.MediaFetchTimeout.Get(),
			CaptchaProbeTTL:    cfg.Upstream.CaptchaProbeTTL.Get(),
			Minter:             minter,
		},
	}, func(format string, args ...any) {
		logger.Info(fmt.Sprintf(format, args...))
	})
	if err != nil {
		logger.Error("build account pool failed", "err", err)
		os.Exit(1)
	}
	defer p.Close()
	p.StartBackground()

	// ---- 成片出口 ----
	proxy := media.New(cfg.PublicBase, cfg.MediaPath, cfg.Upstream.MediaReadTimeout.Get(), cfg.MediaPassthrough)

	// 面板只读展示官方通路（不把 key 传进去，只传"能不能读"和读取函数）。
	var officialAccount func(context.Context) (map[string]any, error)
	if cfg.Official.Enabled() {
		if oc, err := official.New(official.Config{
			BaseURL: cfg.Official.BaseURL, APIKey: cfg.Official.APIKey, Timeout: cfg.Official.Timeout.Get(),
		}); err == nil {
			officialAccount = func(ctx context.Context) (map[string]any, error) {
				a, err := oc.Account(ctx)
				if err != nil {
					return nil, err
				}
				return map[string]any{
					"key_name": a.KeyName, "balance": a.Balance,
					"models": a.Models, "rpm": a.RPM, "daily_limit": a.DailyLimit,
				}, nil
			}
		}
	}

	// 一份账本，面板与 server 共用：调试页记 dry-run/真跑，server 记所有真实提交。
	history := debughist.New(cfg.TaskStore.Path + ".debug")

	// 调试页的回调：PlanRequest 只翻译，SubmitRequest 复用公网创建链路。
	// srv 在下面才赋值 —— 闭包捕获的是变量本身，调用时已经就绪。
	var srv *server.Server

	// ---- 面板 ----
	var panelHandler http.Handler
	if cfg.Panel.Enabled {
		password := cfg.Panel.Password
		if password == "" {
			password = cfg.APIKey
		}
		panelHandler = panel.New(panel.Deps{
			Pool:            p,
			Store:           st,
			ServiceName:     serviceName,
			Version:         appVersion,
			AuthMode:        authMode(cfg.APIKey),
			BaseURL:         cfg.BaseURL,
			MediaEnabled:    proxy.Enabled(),
			Started:         time.Now(),
			ProbeInterval:   cfg.Pool.ProbeInterval.Get(),
			MinterEndpoints: upstream.CountMinterEndpoints(cfg.Minter.URL),
			OfficialEnabled: cfg.Official.Enabled(), OfficialAccount: officialAccount,
			PlanRequest: server.PlanDebug,
			// 调试历史落在任务库旁边（同一个 data 目录，一起备份）。
			DebugHistory: history,
			SubmitRequest: func(ctx context.Context, body map[string]any) (map[string]any, error) {
				if srv == nil {
					return nil, fmt.Errorf("server not ready")
				}
				return srv.DebugSubmit(ctx, body)
			},
		}, password)
	}

	// ---- HTTP 服务 ----
	srv = server.New(server.Config{
		ServiceName:        serviceName,
		Version:            appVersion,
		Listen:             cfg.Listen,
		APIKey:             cfg.APIKey,
		TaskCacheTTL:       cfg.TaskStore.CacheTTL.Get(),
		SubmitWorkers:      cfg.Submit.Workers,
		SubmitInline:       cfg.Submit.Inline,
		SubmitLease:        cfg.Submit.Lease.Get(),
		MediaRehostBudget:  cfg.Upstream.MediaRehostBudget.Get(),
		RehostWorkers:      cfg.Upstream.RehostWorkers,
		PanelHandler:       panelHandler,
		History:            history,
		MinterURL:          cfg.Minter.URL,
		TransportRetries:   cfg.Submit.TransportRetries,
		TimeoutRetries:     cfg.Submit.TimeoutRetries,
		RuntimeRetries:     cfg.Submit.RuntimeRetries,
		RuntimeBackoff:     cfg.Submit.RuntimeBackoff.Get(),
		SubmitRetryBackoff: cfg.Submit.RetryBackoff.Get(),
		Official: server.OfficialConfig{
			APIKey:  cfg.Official.APIKey,
			BaseURL: cfg.Official.BaseURL,
			Timeout: cfg.Official.Timeout.Get(),
		},
		ReadTimeout: cfg.Server.ReadTimeout.Get(),
	}, p, st, proxy, logger)

	httpSrv := &http.Server{
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       cfg.Server.ReadTimeout.Get(),
		// WriteTimeout 默认 0：成片下载是流式的，固定总时长会变成"文件越大越容易失败"。
		WriteTimeout: cfg.Server.WriteTimeout.Get(),
		IdleTimeout:  120 * time.Second,
	}

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		logger.Error("listen failed", "addr", cfg.Listen, "err", err)
		os.Exit(1)
	}

	// 上次进程留下的"进行中"任务先对一次上游：容器重启（部署就是一次重启）会让
	// 盯梢 goroutine 全部消失，而 store 里的记录还停在 queued —— 不补这次，
	// 面板会永远显示"排队"，调用方也以为任务还在跑。
	srv.ResumeInflight()
	// 再挂个低频回收器，兜住"只提交不轮询"的调用方。
	srv.StartInflightReaper(60 * time.Second)

	logStartup(cfg, logger, p.Count(), proxy.Enabled(), minter)

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		<-signals
		logger.Info("shutting down (draining in-flight submissions are abandoned; records become failed via lease)")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	}()

	if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server stopped", "err", err)
	}
	<-shutdownDone
	logger.Info("bye")
}

// runHealthcheck 是容器 HEALTHCHECK 的实现：探本机 /healthz，200 才算健康。
//
// 为什么不写 `curl http://127.0.0.1:8808/healthz`：端口会被部署改掉（compose override /
// -listen），写死端口的健康检查会在挪端口之后恒 unhealthy —— 服务好端端的，运维却被
// 引去查错方向。这里从 listen 地址推导探测地址，判据与服务自己的 /healthz 同源。
func runHealthcheck(listen string) int {
	if listen == "" {
		if raw := os.Getenv("AVM2_LISTEN"); raw != "" {
			listen = raw
		} else if cfg, err := config.Load("config.json"); err == nil {
			listen = cfg.Listen
		}
	}
	if listen == "" {
		listen = ":8808"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: bad listen address %q\n", listen)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

func authMode(apiKey string) string {
	if apiKey == "" {
		return "open"
	}
	return "key"
}

func logStartup(cfg config.Config, logger *slog.Logger, accounts int, mediaEnabled bool, minter *upstream.Minter) {
	logger.Info("aivideomaker2api starting",
		"version", appVersion,
		"listen", cfg.Listen,
		"base_url", cfg.BaseURL,
		"auth", authMode(cfg.APIKey),
		"accounts", accounts,
		"task_store", cfg.TaskStore.Path,
		"media_proxy", mediaEnabled,
		"minter", minter != nil && minter.Configured(),
	)
	if accounts == 0 {
		logger.Warn("no account loaded — put auths/<name>.json ({\"cookie\": \"auth_session=...\"}) or add one in the panel")
	}
	if enabled := authMode(cfg.APIKey); enabled == "open" {
		logger.Warn("api_key is empty: the API is open to anyone who can reach this port")
	}
	if warn := cfg.PanelOpenWarning(); warn != "" {
		logger.Warn(warn)
	}
}
