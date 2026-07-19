package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/bogdanfinn/websocket"

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

// egressBlame 表示一次失败该由谁负责。
type egressBlame int

const (
	// egressBlameNone:上游的应用层拒绝,或对端正常收尾。节点把活干完了,不该罚。
	egressBlameNone egressBlame = iota
	// egressBlameStatus:以上游状态码上报,交给 egress manager 自己的状态码分支处置。
	egressBlameStatus
	// egressBlameTransport:真链路故障,原样上报,该冷却就冷却。
	egressBlameTransport
)

// classifyEgressBlame 是"这次失败该不该算到出口节点头上"的唯一判据。
//
// 事故复盘:上游的应用层拒绝(限流)被当成传输故障上报 ——
// Feedback(nodeID, 0, err),status=0 且 transportErr 非空,在 egress manager
// 的 switch 里落进 default 分支:冷却 30 秒(翻倍至 10 分钟),LastError 写成
// "transport error"。grok_web 当时只有一个节点,于是 Grok 对 Imagine 的一次限流
// 把整个作用域打下线,连完全健康的会话流量一起挂掉;而运维侧看到的是"代理传输
// 错误",查了很久代理,代理始终是好的。
//
// 第一版修复只堵了 {"type":"error"} 这一种帧形状,漏了三条同机制的活路,其中
// 最要命的是 close 帧:Grok 限流时直接关闭 WebSocket,ReadMessage 返回
// *websocket.CloseError(bogdanfinn/websocket conn.go:983),照样进传输故障分支。
//
// 所以归因收敛到这里,每个上报点都问同一个问题。判定顺序即优先级:
//  1. 已被识别的上游语义(限流 / 反机器人)——最可靠,直接按状态码上报。
//  2. 错误自带上游状态码(webUpstreamError / videoUpstreamError)。
//  3. WebSocket close 帧:异常断连(1006 / 1015)是真链路故障;其余属对端主动
//     收尾,再用同一张词汇表看关闭理由里有没有限流/反机器人。
//  4. 其余一律按真传输故障处理 —— 宁可错罚,也不能让坏掉的代理逃过健康检测。
func classifyEgressBlame(err error) (egressBlame, int) {
	if err == nil {
		return egressBlameNone, 0
	}
	switch {
	case errors.Is(err, errWebUsageLimit):
		return egressBlameStatus, http.StatusTooManyRequests
	case errors.Is(err, errWebAntiBot):
		return egressBlameStatus, http.StatusForbidden
	}
	if status, ok := provider.ErrorHTTPStatus(err); ok {
		return egressBlameStatus, status
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		if closeErr.Code == websocket.CloseAbnormalClosure || closeErr.Code == websocket.CloseTLSHandshake {
			// 没收到 close 帧就断了 —— 那是链路问题,不是上游在说话。
			return egressBlameTransport, 0
		}
		switch reason := webResponseError(map[string]any{"message": closeErr.Text}); {
		case errors.Is(reason, errWebUsageLimit):
			return egressBlameStatus, http.StatusTooManyRequests
		case errors.Is(reason, errWebAntiBot):
			return egressBlameStatus, http.StatusForbidden
		}
		// 对端按协议正常收尾,理由我们不认识 —— 节点没做错任何事。
		return egressBlameNone, 0
	}
	return egressBlameTransport, 0
}

// feedbackUpstreamError 是 web provider 唯一的出口反馈入口。
//
// 直接调用 a.egress.Feedback(ctx, nodeID, 0, err) 是本次事故的根源:那等于断言
// "这是链路故障",而调用点往往拿到的是上游的应用层拒绝。归因交给
// classifyEgressBlame,调用点只管把错误递过来。
//
// 注意状态码分支传的 transportErr 是 nil —— 有了状态码就必须走 manager 的状态码
// 分支(401/429 豁免、403 只降健康度不冷却),再带上 err 会重新落回 default。
func (a *Adapter) feedbackUpstreamError(ctx context.Context, nodeID uint64, err error) {
	switch blame, status := classifyEgressBlame(err); blame {
	case egressBlameStatus:
		a.egress.Feedback(context.WithoutCancel(ctx), nodeID, status, nil)
	case egressBlameTransport:
		a.egress.Feedback(context.WithoutCancel(ctx), nodeID, 0, err)
	}
}
