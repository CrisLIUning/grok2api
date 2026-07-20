package gateway

import (
	"errors"
	"net/http"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type stubStatusError struct{ status int }

func (e *stubStatusError) Error() string       { return "stub upstream error" }
func (e *stubStatusError) HTTPStatusCode() int { return e.status }

var _ provider.HTTPStatusError = (*stubStatusError)(nil)

// videoRotatableFailure 只认 UnsubmittedVideoError:provider 必须能证明
// "生成尚未提交"。裸 HTTP 429 不够 —— 流中途 accepted 后的 429 仍带 429,
// 但已可能收下任务,换号会产生第二份视频。
func TestVideoRotatableFailure_InitialGenerationRotatesOnUnsubmittedRateLimit(t *testing.T) {
	err := provider.NewUnsubmittedVideoError(http.StatusTooManyRequests, &stubStatusError{status: http.StatusTooManyRequests})
	if !videoRotatableFailure("", err) {
		t.Error("首发遇到可证明未提交的 429 必须能换号")
	}
	if !videoRotatableFailure("generation", err) {
		t.Error("显式的 generation 操作同样应可换号")
	}
	// 流内 temporary unavailable(503 Unsubmitted)同样可换号。
	tmp := provider.NewUnsubmittedVideoError(http.StatusServiceUnavailable, errors.New("Service temporarily unavailable. Please try again later."))
	if !videoRotatableFailure("", tmp) {
		t.Error("首帧 temporary unavailable 应可换号")
	}
}

// 裸状态码不再构成换号证明:必须由 provider 包装 UnsubmittedVideoError。
func TestVideoRotatableFailure_BareStatusDoesNotRotate(t *testing.T) {
	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
	} {
		if videoRotatableFailure("", &stubStatusError{status: status}) {
			t.Errorf("裸 %d 不应换号:缺少 UnsubmittedVideoError 证明", status)
		}
	}
}

// 视频比图片更严格:普通 5xx **不**换号(除非被包装为 Unsubmitted 503)。
func TestVideoRotatableFailure_ServerErrorsDoNotRotate(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		if videoRotatableFailure("", &stubStatusError{status: status}) {
			t.Errorf("首发遇到 %d 不应换号:无法区分'上游拒绝'与'上游已开始生成但响应丢了'", status)
		}
	}
}

// 续拍绝不能换号:extendPostId 只有创建它的账号会话解析得了。
func TestVideoRotatableFailure_ExtensionNeverRotates(t *testing.T) {
	unsubmitted := provider.NewUnsubmittedVideoError(http.StatusTooManyRequests, &stubStatusError{status: http.StatusTooManyRequests})
	if videoRotatableFailure("extension", unsubmitted) {
		t.Error("续拍即使是 Unsubmitted 429 也不能换号")
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		if videoRotatableFailure("extension", &stubStatusError{status: status}) {
			t.Errorf("续拍在 %d 下也不能换号", status)
		}
	}
}

func TestVideoRotatableFailure_StatuslessErrorNeverRotates(t *testing.T) {
	if videoRotatableFailure("", errors.New("解析视频流失败")) {
		t.Error("拿不到上游状态码时不能换号")
	}
	if videoRotatableFailure("", nil) {
		t.Error("nil 错误不是失败")
	}
}

func TestVideoRotatableFailure_ClientErrorsNeverRotate(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		if videoRotatableFailure("", &stubStatusError{status: status}) {
			t.Errorf("%d 不应换号", status)
		}
	}
}

func TestSanitizeVideoFailure_UnsubmittedServiceUnavailableIsRateLimited(t *testing.T) {
	err := provider.NewUnsubmittedVideoError(http.StatusServiceUnavailable, errors.New("Service temporarily unavailable. Please try again later."))
	code, publicErr := sanitizeVideoFailure(err)
	if code != "rate_limited" {
		t.Fatalf("code = %q, want rate_limited", code)
	}
	if publicErr == nil || publicErr.Error() == "" {
		t.Fatal("public error must be non-empty")
	}
	if publicErr.Error() != "上游繁忙,请稍后重试" {
		t.Fatalf("public = %q", publicErr.Error())
	}
}
