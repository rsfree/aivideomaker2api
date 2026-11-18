package ratio

import "testing"

func TestResolveExactAndAbsent(t *testing.T) {
	got, err := Resolve("16:9", SiteUI, Options{Strict: true})
	if err != nil || got.Ratio != "16:9" || got.Mode != "exact" || !got.Silent() {
		t.Fatalf("exact 16:9: got %+v err=%v", got, err)
	}
	absent, err := Resolve("", SiteUI, Options{Strict: true})
	if err != nil || absent.Mode != "absent" || !absent.Silent() {
		t.Fatalf("absent: got %+v err=%v", absent, err)
	}
	adaptive, err := Resolve("adaptive", SiteUI, Options{Strict: true})
	if err != nil || adaptive.Ratio != "adaptive" || adaptive.Mode != "exact" {
		t.Fatalf("adaptive: got %+v err=%v", adaptive, err)
	}
}

func TestResolveDimensionAndProportionSnap(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantMod string
	}{
		{"1920x1080", "16:9", "dimension"},
		{"1024x1792", "9:16", "snapped"},
		{"5:4", "4:3", "proportion"},
		{"2.35:1", "21:9", "proportion"},
		// ⚠️ 21:9 本身是可约的（gcd 3 ⇒ 7:3），所以任何 WxH 都**不可能**约分出 21:9
		// —— 它只能靠吸附到达。这条与 Python 版行为逐字一致，别"修"成 dimension。
		{"2520x1080", "21:9", "snapped"},
		{"1024x768", "4:3", "dimension"},
	}
	for _, c := range cases {
		got, err := Resolve(c.in, SiteUI, Options{Strict: true, AllowProportion: true, WarnDrift: 15, Examples: map[string]string{}})
		if err != nil {
			t.Fatalf("%s: unexpected error %v", c.in, err)
		}
		if got.Ratio != c.want || got.Mode != c.wantMod {
			t.Errorf("%s: got ratio=%s mode=%s, want %s/%s", c.in, got.Ratio, got.Mode, c.want, c.wantMod)
		}
		if got.Note == "" {
			t.Errorf("%s: snapped result must carry a drift note", c.in)
		}
	}
}

func TestResolvePresetAndAlias(t *testing.T) {
	opt := Options{
		Strict: true, AllowProportion: true,
		Presets: map[string]string{"portrait": "9:16"},
		Aliases: map[string]string{"keep_ratio": "adaptive"},
	}
	if got, _ := Resolve("portrait", SiteUI, opt); got.Ratio != "9:16" || got.Mode != "preset" {
		t.Errorf("preset: got %+v", got)
	}
	if got, _ := Resolve("keep_ratio", SiteUI, opt); got.Ratio != "adaptive" || got.Mode != "alias" {
		t.Errorf("alias: got %+v", got)
	}
}

func TestResolveFallbackVersusStrict(t *testing.T) {
	if _, err := Resolve("banana", SiteUI, Options{Strict: true}); err == nil {
		t.Fatal("strict mode must reject an unrecognized value")
	}
	got, err := Resolve("banana", SiteUI.WithFallback("16:9"), Options{Strict: false})
	if err != nil || got.Ratio != "16:9" || got.Mode != "fallback" || got.Note == "" {
		t.Fatalf("fallback mode: got %+v err=%v", got, err)
	}
}

func TestProportionRejectsDegenerate(t *testing.T) {
	for _, in := range []string{"0:9", "16:0", "16", "16/9", "16:9:1", "abc"} {
		if _, ok := ParseProportion(in); ok {
			t.Errorf("%q must not parse as a proportion", in)
		}
	}
	if v, ok := ParseProportion("5:4"); !ok || v != 1.25 {
		t.Errorf("5:4 => %v %v", v, ok)
	}
}

func TestSimplifyWxH(t *testing.T) {
	if got := SimplifyWxH("1920X1080"); got != "16:9" {
		t.Errorf("1920X1080 => %q", got)
	}
	if got := SimplifyWxH("1024x1792"); got != "4:7" {
		t.Errorf("1024x1792 => %q", got)
	}
	if got := SimplifyWxH("16:9"); got != "" {
		t.Errorf("non-WxH must return empty, got %q", got)
	}
}

func TestMeasuredPixelsOnlyReturnsMeasured(t *testing.T) {
	// 两个"按比例推算会错"的实测点：本表存在的理由就是它们。
	if w, h := PixelsFor("720p", "4:3"); w != 928 || h != 704 {
		t.Errorf("720p/4:3 = %dx%d, want 928x704", w, h)
	}
	if w, h := PixelsFor("1080p", "16:9"); w != 1904 || h != 1080 {
		t.Errorf("1080p/16:9 = %dx%d, want 1904x1080", w, h)
	}
	// 未实测的组合必须返回 0 —— 绝不推算。
	if w, h := PixelsFor("480p", "1:1"); w != 0 || h != 0 {
		t.Errorf("480p/1:1 is unmeasured, want 0x0, got %dx%d", w, h)
	}
}

func TestRatioOf(t *testing.T) {
	if got := RatioOf(1920, 1080, SiteUI); got != "16:9" {
		t.Errorf("1920x1080 => %s", got)
	}
	if got := RatioOf(1918, 1078, SiteUI); got != "16:9" {
		t.Errorf("near-16:9 must snap => %s", got)
	}
	if got := RatioOf(0, 100, SiteUI); got != "16:9" {
		t.Errorf("degenerate => %s", got)
	}
}
