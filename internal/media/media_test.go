package media

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestExtensionWhitelist(t *testing.T) {
	cases := map[string]string{
		"https://static2.img2video.ai/x_0_minimax_h3.mp4": ".mp4",
		"https://cdn/x.mov":  ".mov",
		"https://cdn/x.webm": ".webm",
		"https://cdn/x.php":  ".mp4", // 非白名单后缀一律归 .mp4
		"https://cdn/x":      ".mp4",
		"://bad":             ".mp4",
	}
	for in, want := range cases {
		if got := ExtensionOf(in); got != want {
			t.Errorf("ExtensionOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStripExt(t *testing.T) {
	if got := StripExt("cgt-20260916-a1b2c3d4.mp4"); got != "cgt-20260916-a1b2c3d4" {
		t.Errorf("StripExt = %q", got)
	}
	// 不认识的扩展名不该被剥掉（否则会误命中别的 id）。
	if got := StripExt("cgt-abc.exe"); got != "cgt-abc.exe" {
		t.Errorf("StripExt must not strip unknown extensions, got %q", got)
	}
}

func TestServeRewritesLeakyHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Disposition", `attachment; filename="1635002_0_minimax_h3_1635002.mp4"`)
		w.Header().Set("Server", "cloudflare")
		w.Header().Set("CF-Ray", "8c0ffee")
		w.Header().Set("Content-Length", "5")
		w.Header().Set("Accept-Ranges", "bytes")
		_, _ = w.Write([]byte("hello"))
	}))
	defer upstream.Close()

	p := New("https://avm2api.example", "/v", 5*time.Second, false)
	if !p.Enabled() {
		t.Fatal("proxy must be enabled when public_base is set")
	}
	req := httptest.NewRequest(http.MethodGet, "/v/cgt-1.mp4", nil)
	rec := httptest.NewRecorder()
	p.Serve(rec, req, upstream.URL, "cgt-1.mp4")

	body := rec.Body.String()
	if body != "hello" {
		t.Errorf("body = %q", body)
	}
	disposition := rec.Header().Get("Content-Disposition")
	if strings.Contains(disposition, "minimax_h3") {
		t.Errorf("upstream model name leaked through content-disposition: %q", disposition)
	}
	if !strings.Contains(disposition, "cgt-1.mp4") {
		t.Errorf("content-disposition must name our own file, got %q", disposition)
	}
	for _, leaked := range []string{"Server", "Cf-Ray"} {
		if v := rec.Header().Get(leaked); v != "" {
			t.Errorf("header %s must not be forwarded (it identifies the upstream): %q", leaked, v)
		}
	}
	if rec.Header().Get("Accept-Ranges") != "bytes" {
		t.Error("range support must be advertised (players rely on it)")
	}
}

func TestDownloadURLIsOurs(t *testing.T) {
	p := New("https://avm2api.example/", "v", time.Second, false)
	got := p.DownloadURL("cgt-1", "https://static2.img2video.ai/x_0_minimax_h3.mov")
	if want := "https://avm2api.example/v/cgt-1.mov"; got != want {
		t.Errorf("DownloadURL = %q, want %q", got, want)
	}
}

func TestDisabledProxyKeepsUpstreamLinks(t *testing.T) {
	p := New("", "/v", time.Second, false)
	if p.Enabled() {
		t.Fatal("empty public_base must disable the proxy (byte-for-byte old behavior)")
	}
	if got := p.Describe()["enabled"]; got != false {
		t.Errorf("describe.enabled = %v", got)
	}
}
