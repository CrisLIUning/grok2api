package gateway

import (
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// videoOperationExtension 是续拍操作的判别值。它此前以字面量 "extension" 散落在
// 四处(video.go:73/91、web/video.go:50 与这里),而"是不是续拍"现在决定了能否
// 换号重试 —— 拼错一个字母就是静默地允许续拍换号,然后 100% invalid-parent-post。
const videoOperationExtension = "extension"

// retryableOnAnotherAccount 判断一次上游失败是否值得换一个账号重试。
//
// 判据只有 429 与 5xx —— 它们表示"上游此刻不接",换个身份或时机有意义。
//
// 刻意排除的:
//   - 400/404:参数或资源问题,在所有账号上会一模一样地失败。
//   - 401:凭据问题,有专门的重认证路径,不该靠换号掩盖。
//   - 403:反机器人与**出口身份**(IP / UA / CF Cookie)绑定,不是账号问题。
//     拿更多账号去撞一个已经被上游标记的出口,只会把它们一起烧掉。
//
// 放宽这个判据前请先想清楚号池代价:MarkFailure 是 30 秒起步、连续失败翻倍的
// 冷却,一次盲目的全池重试就能把整个号池推进"全员冷却"。
func retryableOnAnotherAccount(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// videoRotatableFailure 判断一次视频生成失败能否换个账号重来。
//
// 两个条件缺一不可:
//
//  1. **不是续拍。** extendPostId 与 originalPostId / parentPostId 是同一个
//     post,只有创建它的账号的会话解析得了;换号是 100% 的
//     invalid-parent-post,救不了这次请求,还白白把另一个账号推进冷却。
//     runVideoJob 原来那条"禁止切换账号"的注释对续拍是对的,错在被套用到了
//     首次生成上。
//
//  2. **错误带得出上游状态码。** 这是能否重试的**证明**,不只是分类:携带状态码
//     的视频错误只有 videoUpstreamError 一种,而它只在 parseVideoStream 读流
//     之前的状态检查处构造(web/video.go:144)—— 那一刻 postId 还不存在,上游
//     什么都没收下。反过来,mid-stream 的失败返回的是裸 fmt.Errorf,不带状态码;
//     那时 post 可能已经创建,换号重试会产生第二个 post,既浪费配额又可能把
//     同一次请求算两遍钱。
//
// 状态码本身的取舍与图片一致,见 retryableOnAnotherAccount。
func videoRotatableFailure(operation string, err error) bool {
	if operation == videoOperationExtension {
		return false
	}
	status, ok := provider.ErrorHTTPStatus(err)
	return ok && retryableOnAnotherAccount(status)
}
