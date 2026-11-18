// Package debughist 记录面板「调试」页的每一次 dry-run 与真跑。
//
// 为什么值得单独存：官方通路的价差是**十倍级**（minimax 720p/5s=15 积分，
// seedance20 720p/5s=147），没有累计视图就没法判断"这条模型值不值得用"。
// 只追加、不修改（JSONL），坏了也只丢一行。
package debughist

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record 是一次调试动作。
type Record struct {
	At      string         `json:"at"`
	Kind    string         `json:"kind"` // plan | submit
	Model   string         `json:"model"`
	Channel string         `json:"channel,omitempty"`
	Billed  bool           `json:"billed"`
	OK      bool           `json:"ok"`
	TaskID  string         `json:"task_id,omitempty"`
	Error   string         `json:"error,omitempty"`
	Request map[string]any `json:"request,omitempty"`
}

// Recorder 串行化写入（面板可能被多标签页同时点）。
type Recorder struct {
	path string
	mu   sync.Mutex
}

// New 建一个 recorder；path 为空表示不记录。
func New(path string) *Recorder { return &Recorder{path: path} }

// Enabled 表示是否落盘。
func (r *Recorder) Enabled() bool { return r != nil && r.path != "" }

// Append 追加一条。
func (r *Recorder) Append(rec Record) {
	if !r.Enabled() {
		return
	}
	if rec.At == "" {
		rec.At = time.Now().Format(time.RFC3339)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil && filepath.Dir(r.path) != "." {
		return
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(raw, '\n'))
}

// List 返回最近 limit 条（新的在前）。文件不存在 = 空，不是错误。
func (r *Recorder) List(limit int) []Record {
	if !r.Enabled() {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	f, err := os.Open(r.path)
	if err != nil {
		return nil
	}
	defer f.Close()

	// 文件可能很大；调试页只关心最近若干条，从尾往前读。
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	out := make([]Record, 0, limit)
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		var rec Record
		if err := json.Unmarshal([]byte(lines[i]), &rec); err == nil {
			out = append(out, rec)
		}
	}
	return out
}
