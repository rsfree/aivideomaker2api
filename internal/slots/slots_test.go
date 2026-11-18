package slots

import "testing"

func TestResolveModelJudgmentOrder(t *testing.T) {
	options := map[string]any{"model_map": map[string]any{
		"doubao-seedance-2-5-260628": "seedance25",
		"*":                          "minimaxH3",
	}}
	// ① 精确键优先于 ② 兜底
	res, err := ResolveModel("doubao-seedance-2-5-260628", options, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Slot != "seedance25" || res.Source != SourceModelMap {
		t.Errorf("exact hit: got %+v", res)
	}
	// ② 兜底覆盖调用方写出的已知槽位名（这是渠道"强制一档"的唯一手段）
	res, err = ResolveModel("kling3", options, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Slot != "minimaxH3" || res.Source != SourceModelMap {
		t.Errorf("catch-all must override a known slot name: got %+v", res)
	}
	if len(res.Warnings) == 0 {
		t.Error("rewriting a caller's model must leave a trace")
	}
}

func TestResolveModelPassthroughAndRefusal(t *testing.T) {
	res, err := ResolveModel("minimaxH3", nil, "")
	if err != nil || res.Slot != "minimaxH3" || res.Source != SourcePassthrough || !res.Verified {
		t.Fatalf("passthrough: got %+v err=%v", res, err)
	}
	if _, err := ResolveModel("some-unknown-model", nil, ""); err == nil {
		t.Fatal("an unknown name with no model_map must be refused (never guessed)")
	}
}

func TestResolveModelStripsProviderAndResolutionSuffix(t *testing.T) {
	// `provider/name_1080p` 必须能匹配到 `name` 这一档，否则合法请求会因为名字对不上被拒。
	options := map[string]any{"model_map": map[string]any{"seedance25": "seedance25"}}
	res, err := ResolveModel("volcengine/seedance25_1080p", options, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Slot != "seedance25" {
		t.Errorf("slot = %q", res.Slot)
	}
}

func TestResolveModelFlagsUnverifiedSlot(t *testing.T) {
	res, err := ResolveModel("kling3", nil, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Verified {
		t.Error("kling3 has no verified procedure")
	}
	var warned bool
	for _, w := range res.Warnings {
		if len(w) > 0 {
			warned = true
		}
	}
	if !warned {
		t.Error("an unverified slot must be flagged in warnings (allow but never silently)")
	}
}

func TestRemovedPinnedKeyIsLoud(t *testing.T) {
	_, err := ResolveModel("minimaxH3", map[string]any{"model": "minimaxH3"}, "")
	if err == nil {
		t.Fatal("the removed `model` pin key must fail loudly, not be ignored")
	}
}

func TestModelMapValidation(t *testing.T) {
	cases := []struct {
		name string
		opts map[string]any
	}{
		{"wildcard pattern", map[string]any{"model_map": map[string]any{"doubao-*": "seedance20"}}},
		{"unknown slot", map[string]any{"model_map": map[string]any{"x": "not-a-slot"}}},
		{"empty value", map[string]any{"model_map": map[string]any{"x": ""}}},
		{"both aliases differ", map[string]any{
			"model_map":          map[string]any{"a": "seedance20"},
			"upstream_model_map": map[string]any{"a": "seedance25"},
		}},
	}
	for _, c := range cases {
		if _, err := ResolveModel("minimaxH3", c.opts, ""); err == nil {
			t.Errorf("%s: must be rejected as a channel config error", c.name)
		}
	}
}

func TestModelMapAliasIsAccepted(t *testing.T) {
	res, err := ResolveModel("alias-name", map[string]any{
		"upstream_model_map": map[string]any{"alias-name": "seedance25"},
	}, "")
	if err != nil || res.Slot != "seedance25" {
		t.Fatalf("alias table must work: %+v err=%v", res, err)
	}
}

func TestParseChannelOptions(t *testing.T) {
	if opts, err := ParseChannelOptions(""); err != nil || opts != nil {
		t.Errorf("empty header = not configured: %v %v", opts, err)
	}
	if _, err := ParseChannelOptions("{not json"); err == nil {
		t.Error("a broken header must not be treated as 'not configured'")
	}
	if _, err := ParseChannelOptions(`["a"]`); err == nil {
		t.Error("a non-object header must be rejected")
	}
}
