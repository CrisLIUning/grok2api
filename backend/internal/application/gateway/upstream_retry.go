package gateway

import (
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// videoOperationExtension 是续拍操作的判别值。它此前以字面量 "extension" 散落在
// 四处(video.go:73/91、web/video.go:50 与这里),而"是不是续拍"现在决定了能否
// 换号重试 —— 拼错一个字母就是静默地允许续拍换号,然后 100% invalid-parent-post。
const videoOperationExtension = "extension"

// defaultUpstreamAttempts 是 maxAttempts 配置缺失/非法时的重试预算。
const defaultUpstreamAttempts = 3

// attemptBudget 返回本次请求的上游尝试次数。
//
// UpdateMaxAttempts 不校验入参,所以 0 / 负数是能落到运行时的(配置项没写、迁移
// 出错、后台填错)。图片与视频此前各自兜底:图片 <=0 → 3,视频 max(1, n) → 1。
// 后者意味着一个配置失误就静默关掉了视频的换号重试,而症状与修复前的 bug 完全
// 一样——视频 429 直接判死,同一时刻同一号池的图片却能出片。那正是这次排查里
// 最难辨认的信号,不能再留一个能重现它的开关。
func (s *Service) attemptBudget() int {
	if n := int(s.maxAttempts.Load()); n > 0 {
		return n
	}
	return defaultUpstreamAttempts
}

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
//  2. **错误是明确的 429。** 这是能否重试的**证明**,不只是分类。
//
// 第二条比图片严格,刻意如此。图片是同步的,重试至多多花一次调用;视频不是:
//
//   - 429:明确的拒绝,上游什么都没收下 —— 安全。而它正是我们要解决的那个失败
//     (catalog.go 的实测注释:「视频首发就是 429,换个号重试即成功」)。
//   - 5xx:无法区分"上游拒绝了"和"上游已经开始生成、只是响应丢了"。后者换号
//     会产生第二次生成:白烧一份上游配额,还在 Grok 那边留下孤儿 post。
//   - 无状态码:mid-stream 的失败返回裸 fmt.Errorf,那时 post 可能已经创建,
//     同上。
//
// 所以只认 429 —— 拿到了全部收益,完全避开重复生成。放宽它之前请先想清楚:
// 你能证明上游没收下这次请求吗?
func videoRotatableFailure(operation string, err error) bool {
	if operation == videoOperationExtension {
		return false
	}
	status, ok := provider.ErrorHTTPStatus(err)
	return ok && status == http.StatusTooManyRequests
}
