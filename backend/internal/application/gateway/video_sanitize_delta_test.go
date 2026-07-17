package gateway

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

// upstreamErrStub 模拟 provider 层抛出的、携带上游原始 body 的错误。
// 真实形态见 infra/provider/web/video.go 的 videoUpstreamError:
//
//	fmt.Sprintf("视频上游返回 %d: %s", e.status, e.body)
type upstreamErrStub struct {
	status int
	body   string
}

func (e *upstreamErrStub) Error() string {
	return "视频上游返回 " + http.StatusText(e.status) + ": " + e.body
}
func (e *upstreamErrStub) HTTPStatusCode() int { return e.status }

// job.ErrorMessage 会原样出现在客户端的视频查询响应里
// (transport/http/inference 的 videoGenerationResponse → error.message)。
// 而 provider 层把 **Grok 的原始响应体**塞进了错误消息 —— 实测泄漏过:
//
//	{"message":"视频上游返回 429: {\"error\":{\"code\":8,\"message\":\"Too many requests\"}}"}
//
// 这里泄漏的是上游内部的错误码、字段名与实现细节。我们是转售面,客户端不该
// 看到我们的上游是谁、它长什么样。
//
// 上游(chenyme)只对 401/403 脱敏,429/5xx 等照样透传 —— 对他们合理(用户就是
// 账号主人),对我们不成立。故这里要求:凡是带上游 HTTP 状态的错误,一律换成
// 我们自己的公开措辞。
func TestSanitizeVideoFailure_NeverLeaksUpstreamBody(t *testing.T) {
	leaky := []struct {
		name   string
		status int
		body   string
	}{
		{"rate limited", http.StatusTooManyRequests, `{"error":{"code":8,"message":"Too many requests"}}`},
		{"forbidden", http.StatusForbidden, `{"error":{"code":7,"message":"Request rejected by anti-bot rules."}}`},
		{"unauthorized", http.StatusUnauthorized, `{"error":"sso token expired"}`},
		{"server error", http.StatusInternalServerError, `{"trace_id":"abc-123","host":"grok-internal-7"}`},
	}

	for _, test := range leaky {
		t.Run(test.name, func(t *testing.T) {
			_, publicErr := sanitizeVideoFailure(&upstreamErrStub{status: test.status, body: test.body})

			message := publicErr.Error()
			for _, secret := range []string{test.body, "视频上游返回", "grok-internal", "trace_id", "anti-bot"} {
				if strings.Contains(message, secret) {
					t.Errorf("公开错误消息泄漏了上游细节 %q:\n  %s", secret, message)
				}
			}
			if strings.TrimSpace(message) == "" {
				t.Error("脱敏后不能是空消息 —— 客户端需要知道发生了什么")
			}
		})
	}
}

// 脱敏不等于抹平:错误码要能区分"限流"和"生成失败",否则客户端无法决定
// 该重试还是该换参数,我们自己排障也失去分类。
func TestSanitizeVideoFailure_KeepsActionableCode(t *testing.T) {
	tests := []struct {
		status   int
		wantCode string
	}{
		{http.StatusTooManyRequests, "rate_limited"},
		{http.StatusUnauthorized, "provider_unavailable"},
		{http.StatusForbidden, "provider_unavailable"},
		{http.StatusInternalServerError, "provider_unavailable"},
	}
	for _, test := range tests {
		code, _ := sanitizeVideoFailure(&upstreamErrStub{status: test.status, body: "x"})
		if code != test.wantCode {
			t.Errorf("status %d → code %q, want %q", test.status, code, test.wantCode)
		}
	}
}

// 我们自己的错误(不带上游状态码)是可以照原样透出的 —— 它们本来就是写给
// 客户端看的,而且是拓展链路的主要诊断入口。
func TestSanitizeVideoFailure_PassesThroughOwnErrors(t *testing.T) {
	code, publicErr := sanitizeVideoFailure(ErrExtensionSourceNotFound)

	if code != "generation_failed" {
		t.Errorf("code = %q,自有错误应保持 generation_failed", code)
	}
	if !errors.Is(publicErr, ErrExtensionSourceNotFound) {
		t.Errorf("自有错误被改写了: %v", publicErr)
	}
}
