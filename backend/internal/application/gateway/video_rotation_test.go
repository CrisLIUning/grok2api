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

// videoRotatableFailure 决定一次视频生成失败能否换个账号重来。
//
// 背景:runVideoJob 此前是**单发且锁定账号**的 ——
//
//	// 视频任务创建时已持久化账号归属;恢复只能重新获取原账号,禁止因后续
//	// 轮询或结果处理失败切换到其他账号。
//	lease, err := s.selector.AcquirePinned(ctx, route.Provider, job.AccountID, ...)
//
// 那条注释对**续拍**是对的:extendPostId 归创建它的账号所有,换个账号的会话
// 解析不了那个 post,必然 invalid-parent-post。但它被过度套用到了首次生成上。
//
// 首发的 429 是在 parseVideoStream 读流之前的状态检查里抛出的
// (web/video.go:144 构造 videoUpstreamError),此时 postId 还不存在,**上游
// 什么都没收下**,换号重试完全安全。catalog.go 自己的实测注释也写着「视频首发
// 就是 429,换个号重试即成功」。
//
// 于是线上表现为:grok-imagine-video 的 429 直接落终态,而同一时刻同一账号池的
// 图片生成(有重试循环)是能出片的。
func TestVideoRotatableFailure_InitialGenerationRotatesOnRateLimit(t *testing.T) {
	err := &stubStatusError{status: http.StatusTooManyRequests}
	if !videoRotatableFailure("", err) {
		t.Error("首发遇到 429 必须能换号:此时 postId 尚不存在,上游什么都没收下")
	}
	if !videoRotatableFailure("generation", err) {
		t.Error("显式的 generation 操作同样应可换号")
	}
}

// 视频比图片更严格:5xx **不**换号。
//
// 图片是同步的,失败就是失败,重试至多多花一次调用。视频不是 —— conversations/new
// 回 5xx 时,我们无法区分"上游拒绝了"和"上游已经开始生成、只是响应丢了"。后者
// 换号重试会产生第二次生成:白烧一份上游配额,还在 Grok 那边留下一个孤儿 post。
//
// 429 没有这个歧义:它是明确的拒绝,上游什么都没收下。而 429 恰恰是我们要解决的
// 那个失败(catalog.go 的实测注释:「视频首发就是 429,换个号重试即成功」),
// 所以只认 429 既拿到了全部收益,又完全避开了重复生成。
func TestVideoRotatableFailure_ServerErrorsDoNotRotate(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		if videoRotatableFailure("", &stubStatusError{status: status}) {
			t.Errorf("首发遇到 %d 不应换号:无法区分'上游拒绝'与'上游已开始生成但响应丢了',"+
				"后者重试会产生第二次生成(白烧配额 + 孤儿 post)", status)
		}
	}
}

// 这是整个改动里最需要守住的一条:续拍绝不能换号。
//
// extendPostId 与 originalPostId / parentPostId 是同一个 post,只有创建它的
// 账号的会话解析得了。换号的结果是 100% 的 invalid-parent-post,既救不了这次
// 请求,还白白把另一个账号推进冷却。
func TestVideoRotatableFailure_ExtensionNeverRotates(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		if videoRotatableFailure("extension", &stubStatusError{status: status}) {
			t.Errorf("续拍在 %d 下也不能换号:postId 归源账号所有,换号必然 invalid-parent-post", status)
		}
	}
}

// 没有状态码 = 我们无法证明上游"什么都没收下"。
//
// 这一条是安全护栏:mid-stream 的失败(web/video.go:150/153 的裸 fmt.Errorf)
// 恰好不带状态码,而那时 post 可能已经创建。换号重试会产生第二个 post,既浪费
// 配额又可能把同一次请求算两遍钱。
func TestVideoRotatableFailure_StatuslessErrorNeverRotates(t *testing.T) {
	if videoRotatableFailure("", errors.New("解析视频流失败")) {
		t.Error("拿不到上游状态码时不能换号:无法证明上游未收下请求,重试可能产生重复的 post")
	}
	if videoRotatableFailure("", nil) {
		t.Error("nil 错误不是失败")
	}
}

func TestVideoRotatableFailure_ClientErrorsNeverRotate(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		if videoRotatableFailure("", &stubStatusError{status: status}) {
			t.Errorf("%d 不应换号:在所有账号上都会一样地失败,或与出口身份(而非账号)绑定", status)
		}
	}
}
