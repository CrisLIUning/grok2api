package gateway

import (
	"net/http"
	"testing"
)

// retryableOnAnotherAccount 是图片与视频两条媒体路径共用的换号判据。
//
// 它必须同时避免两种错误:
//
//   - 太窄:限流不换号,请求直接判死。线上就是这样 —— grok-imagine-video 的
//     429 落到终态,而 catalog.go 自己的实测注释写着「视频首发就是 429,换个号
//     重试即成功」。
//   - 太宽:参数错误、内容策略、反机器人这些在**所有**账号上都会一样地失败。
//     拿 501 个账号去撞同一堵墙,只会把整个号池挨个烧进冷却(MarkFailure 是
//     30 秒起步、连续失败翻倍),最后收敛成"全员冷却"——那正是线上 03:19 那个
//     503 的成因。
//
// 所以判据是:只有 429 与 5xx。它们表示"上游此刻不接",换个身份/时机有意义。
func TestRetryableOnAnotherAccount_RateLimitAndServerErrors(t *testing.T) {
	for _, status := range []int{
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		if !retryableOnAnotherAccount(status) {
			t.Errorf("%d 应当换号重试:上游此刻不接,换个账号/出口有意义", status)
		}
	}
}

func TestRetryableOnAnotherAccount_ClientErrorsAreTerminal(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:   "参数非法,在任何账号上都会一样地失败",
		http.StatusUnauthorized: "凭据问题,有专门的重认证路径,不该靠换号掩盖",
		http.StatusForbidden:    "反机器人与出口身份绑定,换号只会拿更多账号去撞已经烧掉的出口",
		http.StatusNotFound:     "模型/资源不存在,换号无意义",
	}
	for status, why := range cases {
		if retryableOnAnotherAccount(status) {
			t.Errorf("%d 不应换号重试:%s", status, why)
		}
	}
}

// 0 表示"我们压根没拿到状态码"。这种错误(本地解析失败、契约不符)换号也是
// 同样的结果,不能当成可重试。
func TestRetryableOnAnotherAccount_UnknownStatusIsTerminal(t *testing.T) {
	if retryableOnAnotherAccount(0) {
		t.Error("拿不到状态码时不应换号重试")
	}
	if retryableOnAnotherAccount(http.StatusOK) {
		t.Error("2xx 不是失败")
	}
}
