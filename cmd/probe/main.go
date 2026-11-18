// 命令 probe 是零成本的运维自检：会话是否有效、账号额度与闸门、以及一次请求**会被翻译成
// 什么**（dry-run）。
//
// 🔴 它**不创建任何任务**：默认只跑只读探测（credits / permission / needsCaptcha），
// 加 `-dry-run` 也只跑翻译层（纯函数）。这两件事都不计费。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jushenzhidao/aivideomaker2api/internal/config"
	"github.com/jushenzhidao/aivideomaker2api/internal/pool"
	"github.com/jushenzhidao/aivideomaker2api/internal/translate"
	"github.com/jushenzhidao/aivideomaker2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	account := flag.String("account", "", "只探测指定账号（默认全部）")
	dryRun := flag.Bool("dry-run", false, "只打印一次示例请求的翻译结果（不提交、不计费）")
	prompt := flag.String("prompt", "a red balloon floating over a lake", "dry-run 用的提示词")
	taskID := flag.String("task", "", "只打印某个**上游**任务的原始记录（账单核对用，只读）")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fail(err)
	}

	if *dryRun {
		printDryRun(*prompt)
		return
	}

	p, err := pool.New(pool.Options{
		AuthDir:        cfg.AuthDir,
		StateFile:      "", // 探测不写状态文件（只读操作不该改部署状态）
		MaxInFlight:    cfg.Queue.MaxInFlight,
		AcquireTimeout: cfg.Queue.AcquireTimeout.Get(),
		PollInterval:   cfg.Queue.PollInterval.Get(),
		WatchTimeout:   cfg.Queue.WatchTimeout.Get(),
		Upstream: upstream.Config{
			BaseURL:           cfg.BaseURL,
			Timeout:           cfg.Upstream.Timeout.Get(),
			ProbeTimeout:      cfg.Upstream.ProbeTimeout.Get(),
			TrustEnv:          cfg.Upstream.TrustEnv,
			MediaRetries:      cfg.Upstream.MediaRetries,
			MediaFetchTimeout: cfg.Upstream.MediaFetchTimeout.Get(),
			CaptchaProbeTTL:   cfg.Upstream.CaptchaProbeTTL.Get(),
		},
	}, func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) })
	if err != nil {
		fail(err)
	}
	defer p.Close()

	if p.Count() == 0 {
		fail(fmt.Errorf("no account found under %s (expected {\"cookie\":\"auth_session=...\"} json files)",
			cfg.AuthDir))
	}

	targets := []string{*account}
	if *account == "" {
		for _, view := range p.Snapshot() {
			targets = append(targets, view.Name)
		}
	}

	if *taskID != "" {
		dumpTask(p, *account, *taskID)
		return
	}

	failed := 0
	for _, name := range targets {
		if name == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := p.Probe(ctx, name)
		cancel()
		if err != nil {
			failed++
			fmt.Printf("✗ %-16s %v\n", name, err)
			continue
		}
		fmt.Printf("✓ %-16s %s\n", name, describe(p, name))
	}
	if failed > 0 {
		os.Exit(1)
	}
}

func describe(p *pool.Pool, name string) string {
	for _, view := range p.Snapshot() {
		if view.Name != name {
			continue
		}
		credits := "?"
		if view.Credits != nil {
			credits = fmt.Sprintf("%d", *view.Credits)
		}
		gate := "?"
		if view.Captcha != nil {
			if *view.Captcha {
				gate = "ON"
			} else {
				gate = "off"
			}
		}
		return fmt.Sprintf("credits=%s plan=%s limit=%d slots=%d/%d captcha=%s",
			credits, orDash(view.Plan), view.Limit, view.InUse, view.Capacity, gate)
	}
	return ""
}

func printDryRun(prompt string) {
	body := map[string]any{
		"model":      "minimaxH3",
		"content":    []any{map[string]any{"type": "text", "text": prompt}},
		"resolution": "480p",
		"duration":   5,
		"ratio":      "16:9",
	}
	plan, err := translate.Create(body, nil, "")
	if err != nil {
		fail(err)
	}
	effective, warnings := translate.BillingView(plan)
	out := map[string]any{
		"_note":       "dry-run：只跑了翻译层（纯函数），没有提交、没有计费",
		"requested":   plan.Requested,
		"effective":   effective,
		"warnings":    warnings,
		"unsupported": plan.Unsupported,
		"web_params":  plan.WebParams,
	}
	raw, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(raw))
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "probe: %v\n", err)
	os.Exit(1)
}

// dumpTask 打印某个上游任务的**原始**记录。
//
// 为什么需要：面板/接口里的字段是收窄过的（Ark 白名单会把 usage 之类挡掉），
// 而核对"这条到底扣了多少、按什么单价扣的"只能看原始记录。它是只读的。
func dumpTask(p *pool.Pool, account, taskID string) {
	names := []string{account}
	if account == "" {
		for _, view := range p.Snapshot() {
			names = append(names, view.Name)
		}
	}
	var lastErr error
	for _, name := range names {
		if name == "" {
			continue
		}
		acc, ok := p.Account(name)
		if !ok {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		raw, err := acc.Client.GetTask(ctx, taskID, 0)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		out, _ := json.MarshalIndent(raw, "", "  ")
		fmt.Println(string(out))
		return
	}
	fail(fmt.Errorf("could not read task %s from any account: %v", taskID, lastErr))
}
