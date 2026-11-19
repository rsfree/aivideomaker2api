// Package store 是任务记录的持久化。
//
// **为什么必须持久化**：Ark 契约要求 `GET /tasks/{id}` 在保留窗口内始终可查（本服务沿用
// 7 天）。进程内 map 一重启就清空 —— 调用方手里正在轮询的 `cgt-*` 会突然 404，表现为
// "任务凭空消失"。这类缺陷只在重启时暴露，平时看不出来。
//
// 为什么是 JSONL 而不是 SQLite：本服务的任务量是每小时几条，7 天窗口最多几千条；
// 纯 Go 的 JSONL 追加写不引入 CGO、不引入依赖、不需要迁移，重启后按行重放即可
// 恢复到最后一个状态。真正需要 SQL 的那天（跨实例查询 / 复杂过滤）再换后端，
// 接口只有六个方法。
//
// 写入语义：每次变更追加一行 `{"op":"put","entry":{…}}`，读取时**后写覆盖先写**。
// 保留窗口到期时做一次**紧凑重写**（临时文件 + rename），文件不会无限长。
package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry 是一条任务记录。
//
// ⚠️ `Owner` 是**凭据指纹**（sha256 前 16 位），`Account` 是账号名 —— 两者都不含凭据原文。
// 透传模式下 owner 用于租户隔离；池模式下 account 用于把任务绑回某个账号查询。
type Entry struct {
	ID       string `json:"id"`
	TaskID   string `json:"task_id,omitempty"`
	Account  string `json:"account,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Model    string `json:"model"`
	Upstream string `json:"upstream"`
	// Shape 记录入口形态（ark / openai）：轮询阶段拿到的是记录、不是请求，
	// 入口信息**只能**在创建时落下，事后无从推断。
	Shape string `json:"shape,omitempty"`

	Requested   map[string]any `json:"requested,omitempty"`
	Effective   map[string]any `json:"effective,omitempty"`
	Warnings    []string       `json:"warnings,omitempty"`
	Unsupported []string       `json:"unsupported,omitempty"`

	Status          string `json:"status"`
	CreatedAtMs     int64  `json:"created_at_ms"`
	SubmittedAtMs   int64  `json:"submitted_at_ms,omitempty"`
	SubmitError     string `json:"submit_error,omitempty"`
	SubmitErrorCode string `json:"submit_error_code,omitempty"`
	// SubmitErrorQuote 是**上游原话**（仅内容审核等已确认可透传的措辞）。
	SubmitErrorQuote string `json:"submit_error_quote,omitempty"`
	// SubmitErrorHint 是**我们自己写的、已脱敏的**可操作说明（如"等闸门衰减"）。
	// 与 Quote 分开存：一个是上游措辞（证据），一个是我们的话（指引），混在一起
	// 就说不清"这句话到底是谁说的"。
	SubmitErrorHint string `json:"submit_error_hint,omitempty"`

	// Usage 是上游终态记录里的 `usage` 原样（`credits` + `paid`）。
	//
	// 🔴 **必须落盘**：`paid` 是「这条任务到底有没有花钱」的唯一权威判据，而它只在
	// 终态出现一次。不落盘的话，进程重启 / 缓存过期之后就再也回答不了这个问题 ——
	// 而本项目的第一号红线就是计费，答不出"花没花"等于红线失守。
	Usage map[string]any `json:"usage,omitempty"`

	// SourceURL 是成片的上游直链。**只在内部使用**，绝不进响应体
	// （对外给的是本服务自己的下载地址，见 media 包）。
	SourceURL     string `json:"source_url,omitempty"`
	UpstreamModel string `json:"upstream_model,omitempty"`
	VideoURL      string `json:"video_url,omitempty"`
}

// Clone 返回深拷贝（调用方拿到记录后不应该在锁外改原对象）。
func (e *Entry) Clone() *Entry {
	if e == nil {
		return nil
	}
	out := *e
	out.Requested = cloneMap(e.Requested)
	out.Effective = cloneMap(e.Effective)
	out.Warnings = append([]string(nil), e.Warnings...)
	out.Unsupported = append([]string(nil), e.Unsupported...)
	return &out
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Store 是任务表。path 为空 ⇒ 纯内存（**显式**的开发/测试开关，不是持久化失败后的隐式兜底）。
type Store struct {
	mu      sync.RWMutex
	entries map[string]*Entry
	path    string
	file    *os.File
	lines   int
	retain  time.Duration
}

type logLine struct {
	Op    string `json:"op"`
	Entry *Entry `json:"entry,omitempty"`
	ID    string `json:"id,omitempty"`
}

// Open 打开（或创建）任务表并重放历史。path 为空时是内存表。
func Open(path string, retention time.Duration) (*Store, error) {
	if retention <= 0 {
		retention = 7 * 24 * time.Hour
	}
	s := &Store{entries: map[string]*Entry{}, path: path, retain: retention}
	if path == "" {
		return s, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create task store dir: %w", err)
		}
	}
	if err := s.replay(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open task store: %w", err)
	}
	s.file = file
	return s, nil
}

func (s *Store) replay() error {
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		s.lines++
		var rec logLine
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			// 半行（进程被杀在写一半）不该让整个表读不出来 —— 跳过并保留其余记录。
			continue
		}
		switch rec.Op {
		case "put":
			if rec.Entry != nil && rec.Entry.ID != "" {
				s.entries[rec.Entry.ID] = rec.Entry
			}
		case "del":
			delete(s.entries, rec.ID)
		}
	}
	return scanner.Err()
}

// Put 写入（或整体覆盖）一条记录。
func (s *Store) Put(e *Entry) error {
	if e == nil || e.ID == "" {
		return fmt.Errorf("store: entry has no id")
	}
	if e.CreatedAtMs == 0 {
		e.CreatedAtMs = time.Now().UnixMilli()
	}
	s.mu.Lock()
	s.entries[e.ID] = e.Clone()
	s.mu.Unlock()
	return s.append(logLine{Op: "put", Entry: e.Clone()})
}

// Patch 在锁内就地修改一条记录并落盘。返回 false 表示记录不存在。
func (s *Store) Patch(id string, mutate func(*Entry)) (bool, error) {
	s.mu.Lock()
	entry, ok := s.entries[id]
	if !ok {
		s.mu.Unlock()
		return false, nil
	}
	mutate(entry)
	snapshot := entry.Clone()
	s.mu.Unlock()
	return true, s.append(logLine{Op: "put", Entry: snapshot})
}

// Get 读一条记录（返回副本）。
func (s *Store) Get(id string) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.entries[id]
	if !ok {
		return nil, false
	}
	return entry.Clone(), true
}

// Delete 删一条记录并落墓碑。站点没有取消端点 —— 这里删的只是**本地记录**，
// 所以对外接口面刻意不暴露它（只给面板的运维视角用）。
func (s *Store) Delete(id string) bool {
	s.mu.Lock()
	_, ok := s.entries[id]
	delete(s.entries, id)
	s.mu.Unlock()
	if ok {
		_ = s.append(logLine{Op: "del", ID: id})
	}
	return ok
}

// List 按创建时间倒序列出最近记录。owner 为空 = 不做归属过滤（池模式）。
func (s *Store) List(limit, offset int, owner string) []*Entry {
	s.mu.RLock()
	out := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		if owner != "" && e.Owner != "" && e.Owner != owner {
			continue
		}
		out = append(out, e)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAtMs > out[j].CreatedAtMs })
	if offset > 0 {
		if offset >= len(out) {
			return nil
		}
		out = out[offset:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	for i := range out {
		out[i] = out[i].Clone()
	}
	return out
}

// Count 统计记录数。owner 为空 = 全部。
// CountCharged 返回**已确认扣过费**的任务数（`usage.paid == true`）。
//
// 只数 true：`paid=false` 是"确认免费"，字段缺失是"还不知道"，两者都不该计进扣费数。
func (s *Store) CountCharged() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, e := range s.entries {
		if paid, ok := e.Usage["paid"].(bool); ok && paid {
			n++
		}
	}
	return n
}

// Stats 是面板总览用的**只读聚合**（锁内遍历，不克隆 Entry）。
//
// 单独成方法而不是让面板 List 全量再自己数：List 会 Clone 每一条，
// 总览每 5 秒刷一次，几千条克隆是纯浪费。
type Stats struct {
	Since        int64             `json:"since_ms"`
	Total        int               `json:"total"`
	Succeeded    int               `json:"succeeded"`
	Failed       int               `json:"failed"`
	Running      int               `json:"running"`
	ByAccount    map[string]int    `json:"by_account"` // 账号 → 成功条数
	FailedByAcct map[string]int    `json:"failed_by_account"`
	FailByCode   map[string]int    `json:"fail_by_code"`
	FailSample   map[string]string `json:"fail_sample"` // code → 一条内部归因（面板排障用）
}

// StatsSince 统计 startMs 之后创建的记录。
func (s *Store) StatsSince(startMs int64) Stats {
	out := Stats{
		Since: startMs, ByAccount: map[string]int{}, FailedByAcct: map[string]int{},
		FailByCode: map[string]int{}, FailSample: map[string]string{},
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.CreatedAtMs < startMs {
			continue
		}
		out.Total++
		switch e.Status {
		case "succeeded":
			out.Succeeded++
			if e.Account != "" {
				out.ByAccount[e.Account]++
			}
		case "failed", "cancelled", "expired":
			out.Failed++
			if e.Account != "" {
				out.FailedByAcct[e.Account]++
			}
			// 提交期失败带 SubmitErrorCode；已提交后失败（审核/超时）没有码 ——
			// 归成 RuntimeFailure 比混进 Unknown 更能说明"失败发生在哪个相位"。
			code := e.SubmitErrorCode
			if code == "" {
				code = "RuntimeFailure"
			}
			out.FailByCode[code]++
			if _, seen := out.FailSample[code]; !seen && e.SubmitError != "" {
				out.FailSample[code] = clipText(e.SubmitError, 180)
			}
		default:
			out.Running++
		}
	}
	return out
}

// clipText 截断到 n 个字符（按字节），用于面板展示的内部归因。
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (s *Store) Count(owner string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if owner == "" {
		return len(s.entries)
	}
	n := 0
	for _, e := range s.entries {
		if e.Owner == owner {
			n++
		}
	}
	return n
}

// Prune 清掉超过保留窗口的记录，并做一次**紧凑重写**。返回清理条数。
func (s *Store) Prune() (int, error) {
	cutoff := time.Now().Add(-s.retain).UnixMilli()
	s.mu.Lock()
	var dead []string
	for id, e := range s.entries {
		if e.CreatedAtMs < cutoff {
			dead = append(dead, id)
		}
	}
	for _, id := range dead {
		delete(s.entries, id)
	}
	snapshot := make([]*Entry, 0, len(s.entries))
	for _, e := range s.entries {
		snapshot = append(snapshot, e.Clone())
	}
	s.mu.Unlock()
	if len(dead) == 0 {
		return 0, nil
	}
	return len(dead), s.compact(snapshot)
}

// compact 把当前全量记录重写成新文件（临时文件 + rename，崩溃时旧文件仍完整）。
func (s *Store) compact(entries []*Entry) error {
	if s.path == "" {
		return nil
	}
	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for _, e := range entries {
		if err := enc.Encode(logLine{Op: "put", Entry: e}); err != nil {
			f.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()

	s.mu.Lock()
	if s.file != nil {
		s.file.Close()
	}
	s.mu.Unlock()
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	file, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.file = file
	s.lines = len(entries)
	s.mu.Unlock()
	return nil
}

// Describe 是 /healthz 的一眼可见口径：kind=jsonl 才跨重启可读。
func (s *Store) Describe() map[string]any {
	s.mu.RLock()
	n := len(s.entries)
	lines := s.lines
	s.mu.RUnlock()
	if s.path == "" {
		return map[string]any{"kind": "memory", "durable": false, "entries": n}
	}
	return map[string]any{
		"kind": "jsonl", "durable": true, "path": s.path,
		"entries": n, "lines": lines, "retention": s.retain.String(),
	}
}

// Close 关闭文件句柄。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}

func (s *Store) append(rec logLine) error {
	if s.path == "" {
		return nil
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return fmt.Errorf("task store is closed")
	}
	if _, err := s.file.Write(append(raw, '\n')); err != nil {
		return err
	}
	s.lines++
	return nil
}
