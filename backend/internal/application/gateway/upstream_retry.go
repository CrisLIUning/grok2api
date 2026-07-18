package gateway

import "net/http"

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
