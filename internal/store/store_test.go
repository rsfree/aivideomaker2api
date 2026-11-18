package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	s, err := Open(path, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	entry := &Entry{ID: "cgt-1", Model: "minimaxH3", Upstream: "web", Status: "queued"}
	if err := s.Put(entry); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Patch("cgt-1", func(e *Entry) { e.Status = "running"; e.TaskID = "t-1" }); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	s.Close()

	// 重启：正在被轮询的 cgt-* 必须还在（进程内 map 一重启就让调用方 404，原项目踩过）。
	reopened, err := Open(path, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, ok := reopened.Get("cgt-1")
	if !ok {
		t.Fatal("entry lost across restart")
	}
	if got.Status != "running" || got.TaskID != "t-1" {
		t.Errorf("last write must win, got %+v", got)
	}
}

func TestStoreListOrderAndOwnerFilter(t *testing.T) {
	s, _ := Open("", 0)
	defer s.Close()
	now := time.Now().UnixMilli()
	_ = s.Put(&Entry{ID: "a", Owner: "o1", CreatedAtMs: now - 2000})
	_ = s.Put(&Entry{ID: "b", Owner: "o2", CreatedAtMs: now - 1000})
	_ = s.Put(&Entry{ID: "c", Owner: "o1", CreatedAtMs: now})
	list := s.List(10, 0, "")
	if len(list) != 3 || list[0].ID != "c" {
		t.Fatalf("list must be newest-first: %+v", list)
	}
	mine := s.List(10, 0, "o1")
	if len(mine) != 2 {
		t.Errorf("owner filter returned %d entries, want 2", len(mine))
	}
	if s.Count("") != 3 || s.Count("o1") != 2 {
		t.Errorf("counts wrong: all=%d o1=%d", s.Count(""), s.Count("o1"))
	}
}

func TestStorePruneCompacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	s, _ := Open(path, 7*24*time.Hour)
	defer s.Close()
	old := time.Now().Add(-8 * 24 * time.Hour).UnixMilli()
	_ = s.Put(&Entry{ID: "old", CreatedAtMs: old})
	_ = s.Put(&Entry{ID: "fresh"})
	removed, err := s.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 1 {
		t.Errorf("pruned %d, want 1", removed)
	}
	if _, ok := s.Get("old"); ok {
		t.Error("expired entry must be gone")
	}
	if _, ok := s.Get("fresh"); !ok {
		t.Error("fresh entry must survive")
	}
}

func TestStoreDescribeMarksDurability(t *testing.T) {
	mem, _ := Open("", 0)
	defer mem.Close()
	if mem.Describe()["durable"] != false {
		t.Error("memory store must not claim durability")
	}
	disk, _ := Open(filepath.Join(t.TempDir(), "t.jsonl"), 0)
	defer disk.Close()
	if disk.Describe()["durable"] != true {
		t.Error("jsonl store must claim durability (it is how we know restart-safe mode is on)")
	}
}

func TestStoreCloneIsolatesCallers(t *testing.T) {
	s, _ := Open("", 0)
	defer s.Close()
	_ = s.Put(&Entry{ID: "a", Warnings: []string{"w1"}})
	got, _ := s.Get("a")
	got.Warnings[0] = "mutated"
	again, _ := s.Get("a")
	if again.Warnings[0] != "w1" {
		t.Error("Get must return a copy — callers must not be able to mutate the store")
	}
}

// `paid` 是三态：true 才算扣费；false（确认免费）与缺失（还不知道）都不算。
func TestCountChargedOnlyCountsTrue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.jsonl")
	s, err := Open(path, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	_ = s.Put(&Entry{ID: "a", Status: "succeeded", Usage: map[string]any{"paid": true}})
	_ = s.Put(&Entry{ID: "b", Status: "succeeded", Usage: map[string]any{"paid": false}})
	_ = s.Put(&Entry{ID: "c", Status: "running"})
	_ = s.Put(&Entry{ID: "d", Status: "succeeded", Usage: map[string]any{"credits": 1}})
	if got := s.CountCharged(); got != 1 {
		t.Errorf("CountCharged = %d, want 1 (only paid=true counts)", got)
	}
}
