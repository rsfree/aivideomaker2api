package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rsfree/aivideomaker2api/internal/apierr"
)

// 读超时**不能**建议"重试一下"：上游可能已经把任务建起来了。
// 实测（2026-11-08）：一次 720p/base/5s 提交读超时，我们记 failed + paid=null，
// 但余额随后少了 5 积分 —— 上游确实建了任务并出了片。盲目重试 = 再扣一次钱。
func TestTimeoutMessageNeverInvitesABlindRetry(t *testing.T) {
	msg := clientMessage(&apierr.UpstreamError{
		Procedure:  "ai.minimaxH3",
		Transport:  "Timeout",
		HTTPStatus: http.StatusGatewayTimeout,
		Message:    "context deadline exceeded",
	})
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "a retry may succeed") {
		t.Errorf("message must not invite a blind retry: %q", msg)
	}
	if !strings.Contains(lower, "may or may not have created") {
		t.Errorf("message must warn that the upstream may already have created the task: %q", msg)
	}
	if !strings.Contains(lower, "bill") {
		t.Errorf("message must warn about billing: %q", msg)
	}
	if strings.Contains(msg, "ai.minimaxH3") {
		t.Errorf("message leaks the internal procedure name: %q", msg)
	}
}

// 上游 401 是**我们的** cookie 失效，不是调用方的 key 错了。
// 原样透传 401 会让调用方去换 key，而真正要修的是池里的会话（video-adapter §2.5）。
func TestUpstream401IsAServerFault(t *testing.T) {
	up := apierr.NewUpstream("ai.minimaxH3", "unauthorized", "UNAUTHORIZED", http.StatusUnauthorized)
	if got := statusToHTTP(up); got != http.StatusBadGateway {
		t.Errorf("upstream 401 -> %d, want 502 (server-side credential expiry)", got)
	}
	// 而 404 仍然透传：那是"任务不存在"，调用方需要看到真实原因。
	nf := apierr.NewUpstream("model.getModel", "task not found", "NOT_FOUND", http.StatusNotFound)
	if got := statusToHTTP(nf); got != http.StatusNotFound {
		t.Errorf("upstream 404 -> %d, want 404", got)
	}
}
