package debughist

import (
	"path/filepath"
	"testing"
)

// 追加与读取：新的在前，坏行不影响其它行。
func TestAppendAndList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "debug.jsonl")
	r := New(path)
	if !r.Enabled() {
		t.Fatal("recorder with a path must be enabled")
	}
	r.Append(Record{Kind: "plan", Model: "minimaxH3", OK: true})
	r.Append(Record{Kind: "submit", Model: "wan30", Channel: "official", Billed: true, OK: true, TaskID: "cgt-1"})

	got := r.List(10)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Model != "wan30" {
		t.Errorf("newest first, got %q", got[0].Model)
	}
	if got[0].At == "" {
		t.Error("At should be filled automatically")
	}
	if !got[0].Billed {
		t.Error("billed flag lost")
	}
}

// 没有配置路径时：不写、不报错、返回空。
func TestDisabledRecorder(t *testing.T) {
	r := New("")
	if r.Enabled() {
		t.Fatal("empty path must disable the recorder")
	}
	r.Append(Record{Kind: "plan"})
	if got := r.List(5); len(got) != 0 {
		t.Errorf("disabled recorder returned %d records", len(got))
	}
	var nilRec *Recorder
	nilRec.Append(Record{}) // 不能 panic
	if got := nilRec.List(5); got != nil {
		t.Error("nil recorder should return nil")
	}
}
