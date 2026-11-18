package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jushenzhidao/aivideomaker2api/internal/apierr"
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
