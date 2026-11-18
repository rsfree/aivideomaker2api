package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDurationAcceptsStringAndNumber(t *testing.T) {
	var payload struct {
		A Duration `json:"a"`
		B Duration `json:"b"`
		C Duration `json:"c"`
	}
	raw := []byte(`{"a":"90s","b":120,"c":"2m"}`)
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.A.Get() != 90*time.Second {
		t.Errorf("a = %s", payload.A.Get())
	}
	// 裸数字按**秒**：写 120 的人想的是 120 秒，不是 120 纳秒。
	if payload.B.Get() != 120*time.Second {
		t.Errorf("b = %s, want 120s", payload.B.Get())
	}
	if payload.C.Get() != 2*time.Minute {
		t.Errorf("c = %s", payload.C.Get())
	}
}

func TestDurationRejectsGarbage(t *testing.T) {
	var payload struct {
		A Duration `json:"a"`
	}
	if err := json.Unmarshal([]byte(`{"a":"soon"}`), &payload); err == nil {
		t.Fatal("an unparseable duration must fail loudly, not silently become zero")
	}
}

func TestLoadCreatesConfigWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen == "" || cfg.BaseURL == "" {
		t.Errorf("defaults are incomplete: %+v", cfg)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a recommended config must be written on first start: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the generated default must be valid: %v", err)
	}
}

func TestLoadFillsDefaultsForMissingFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:9999"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	// 零值 timeout = 无超时，是最危险的一种"看起来起来了"⇒ 必须由默认值兜住。
	if cfg.Upstream.Timeout.Get() <= 0 {
		t.Error("upstream.timeout must fall back to the default, not to zero")
	}
	if cfg.TaskStore.RetentionDays <= 0 {
		t.Error("retention must fall back to the default")
	}
}

func TestEnvOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, Default()); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AVM2_LISTEN", "127.0.0.1:1")
	t.Setenv("AVM_MINTER_URL", "http://127.0.0.1:8899")
	t.Setenv("AVM2_MAX_CONCURRENT", "4")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != "127.0.0.1:1" {
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.Minter.URL != "http://127.0.0.1:8899" {
		t.Errorf("minter url = %q", cfg.Minter.URL)
	}
	if cfg.Queue.MaxInFlight != 4 {
		t.Errorf("max_in_flight = %d", cfg.Queue.MaxInFlight)
	}
}

func TestValidateCatchesZeroTimeouts(t *testing.T) {
	cfg := Default()
	cfg.Queue.AcquireTimeout = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("a zero acquire timeout must be rejected before the service starts")
	}
}

func TestPanelOpenWarning(t *testing.T) {
	cfg := Default()
	if cfg.PanelOpenWarning() == "" {
		t.Error("an open panel with no api_key must warn")
	}
	cfg.APIKey = "sk-x"
	if cfg.PanelOpenWarning() != "" {
		t.Error("setting api_key closes the warning")
	}
}
