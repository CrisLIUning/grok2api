package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// webUpstreamError 让 Imagine WebSocket 的上游拒绝把状态码带出函数边界。
//
// WebSocket 没有 HTTP 状态码可言,但上层(executeImage 的换号重试循环、
// sanitizeVideoFailure 的错误分类)全靠 provider.ErrorHTTPStatus 判断一个失败
// 是否值得换个账号重试。不带状态码的错误在它们眼里等同于"未知失败",只能放弃。
type webUpstreamError struct {
	status int
	err    error
}

func (e *webUpstreamError) Error() string       { return e.err.Error() }
func (e *webUpstreamError) Unwrap() error       { return e.err }
func (e *webUpstreamError) HTTPStatusCode() int { return e.status }

var _ provider.HTTPStatusError = (*webUpstreamError)(nil)

// upstreamStatusFailure 把一次上游 HTTP 失败包成保留状态码的错误。
//
// 用它而不是 fmt.Errorf("... 返回 %d", status):把状态码写进消息字符串,只有人
// 读得懂,provider.ErrorHTTPStatus 读不懂,于是上层的换号重试循环形同虚设。
func upstreamStatusFailure(status int, message string) error {
	return &webUpstreamError{status: status, err: errors.New(message)}
}

// mediaPostFailure 归类 /rest/media/post/create 的失败。
//
// 只有真正的上游 HTTP 失败才带状态码。响应解析失败与"响应缺 post id"是我们这侧
// 或上游契约的问题 —— 换个账号重试会一模一样地失败,给它们状态码只会诱发无意义的
// 轮换,把账号白白烧进冷却。
func mediaPostFailure(status int, decodeErr error, postID string) error {
	if status < 200 || status >= 300 {
		return upstreamStatusFailure(status, "创建媒体 Post 失败: 上游返回 "+strconv.Itoa(status))
	}
	if decodeErr != nil {
		return fmt.Errorf("创建媒体 Post 失败: 响应无法解析: %w", decodeErr)
	}
	if postID == "" {
		return errors.New("创建媒体 Post 失败: 响应缺少 post id")
	}
	return nil
}

// imagineWSFrameError 把 Imagine WebSocket 的 error 帧解析成带上游语义的错误。
//
// 词汇表直接复用 webResponseError —— Imagine 的 WebSocket 与 Web 会话的 SSE 是
// 同一套后端在说话,错误表达方式一致(code==7 / "anti-bot" 表示反机器人,
// "usage limit"/"usage quota" 表示用量到顶)。在这里另起一套关键字只会随上游
// 措辞漂移而失效。
//
// 已知的三种帧形状都要吃下:
//
//	{"type":"error","error":{"message":"...","code":7}}
//	{"type":"error","message":"...","code":7}
//	{"type":"error","error":"..."}
func imagineWSFrameError(frame map[string]any) error {
	err := webResponseError(imagineWSFramePayload(frame))
	switch {
	case errors.Is(err, errWebUsageLimit):
		return &webUpstreamError{status: http.StatusTooManyRequests, err: err}
	case errors.Is(err, errWebAntiBot):
		return &webUpstreamError{status: http.StatusForbidden, err: err}
	default:
		// 刻意不猜状态码:未知拒绝(内容策略、参数非法)在所有账号上都会一样地
		// 失败,给它一个 429/5xx 只会让上层拿一串账号去撞同一堵墙。
		return err
	}
}

func imagineWSFramePayload(frame map[string]any) map[string]any {
	switch value := frame["error"].(type) {
	case map[string]any:
		return value
	case string:
		if strings.TrimSpace(value) != "" {
			return map[string]any{"message": value, "code": frame["code"]}
		}
	}
	return frame
}

// imagineWSEgressStatus 决定一个 Imagine WebSocket 上游错误要不要算到出口节点账上,
// 以及以什么状态码上报。返回 (status, report);report 为 false 表示完全不上报。
//
// 这个函数存在的理由是一次线上事故。原来的调用点是:
//
//	a.egress.Feedback(ctx, lease.NodeID, 0, upstreamErr)
//
// status=0 且 transportErr 非空,会直落 egress manager 的 default 分支 ——
// FailureCount++、Health*0.7、冷却 30 秒(翻倍至 10 分钟),LastError 写成
// "transport error"。而 grok_web 当时只有一个节点,冷却期内 Acquire 硬失败。
// 于是 Grok 对 Imagine 的一次限流,把整个作用域(含完全健康的会话流量)打下线
// 30 秒;运维侧看到的却是"代理传输错误",查了半天代理,而代理没有任何问题。
//
// 现在按类别区分:
//   - 用量到顶 → 429。manager 对 401/429 是显式豁免的(直接 return,不罚),
//     所以这既不冷却节点,又为将来"按出口维度做限流调度"留下了真实信号。
//   - 反机器人 → 403。它确实与出口身份(IP / User-Agent / CF Cookie)绑定,
//     该算节点账上;manager 的 403 分支只降健康度并重建客户端,不设冷却。
//   - 其它 → 不上报。节点把消息完整送达了,上游拒绝的是我们的请求内容。
//
// 无论哪一类都不再以 transportErr 形式上报,调用点因此只能写成
// Feedback(ctx, nodeID, status, nil)。
func imagineWSEgressStatus(err error) (int, bool) {
	switch {
	case errors.Is(err, errWebUsageLimit):
		return http.StatusTooManyRequests, true
	case errors.Is(err, errWebAntiBot):
		return http.StatusForbidden, true
	default:
		return 0, false
	}
}
