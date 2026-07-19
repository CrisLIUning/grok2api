package web

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	"github.com/bogdanfinn/websocket"
)

// classifyEgressBlame 是"这次失败该不该算出口节点头上"的唯一判据。
//
// 事故复盘:上游的应用层拒绝(限流)被当成传输故障上报,即
// Feedback(nodeID, 0, err) —— status=0 且 transportErr 非空,在 egress manager
// 里落进 default 分支,冷却节点 30 秒(翻倍至 10 分钟)。grok_web 当时只有一个
// 节点,于是 Grok 对 Imagine 的一次限流把整个作用域打下线,连健康的会话流量
// 一起挂掉,而运维侧看到的却是 "transport error"。
//
// 第一版修复只堵了 {"type":"error"} 这一种帧形状。复核发现同一机制还有三条活路:
//
//  1. **close 帧**。Grok 限流时会直接关闭 WebSocket,ReadMessage 返回
//     *websocket.CloseError(见 bogdanfinn/websocket conn.go:983),仍旧走
//     Feedback(nodeID, 0, readErr)。这是最要命的一条 —— 它就是原事故本身。
//  2. streamImageEdit 把流内的 usage-limit / anti-bot 当传输错误上报。
//  3. 非流式会话把 usage limit 当传输错误上报(anti-bot 有特判,限流没有)。
//
// 所以归因必须收敛到一处,让每个上报点都问同一个问题。
func TestClassifyEgressBlame_UpstreamRejectionsAreNotTransportFaults(t *testing.T) {
	cases := map[string]struct {
		err    error
		blame  egressBlame
		status int
	}{
		"用量到顶": {
			err: fmt.Errorf("%w: usage limit", errWebUsageLimit),
			// 429 在 manager 里是显式豁免的(直接 return,不罚节点),
			// 上报它既诚实又为将来按出口维度做限流调度留了信号。
			blame: egressBlameStatus, status: http.StatusTooManyRequests,
		},
		"反机器人": {
			err: fmt.Errorf("%w: anti-bot", errWebAntiBot),
			// 403 确实与出口身份(IP/UA/CF Cookie)绑定,该算节点账上;
			// manager 的 403 分支只降健康度并重建客户端,不设冷却。
			blame: egressBlameStatus, status: http.StatusForbidden,
		},
		"带上游状态码的错误": {
			err:   &webUpstreamError{status: http.StatusBadGateway, err: errors.New("upstream 502")},
			blame: egressBlameStatus, status: http.StatusBadGateway,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			blame, status := classifyEgressBlame(c.err)
			if blame != c.blame || status != c.status {
				t.Errorf("blame=%v status=%d,期望 %v/%d", blame, status, c.blame, c.status)
			}
			if blame == egressBlameTransport {
				t.Error("上游的应用层拒绝绝不能被算成传输故障 —— 那会冷却节点")
			}
		})
	}
}

// 事故的正主:限流以 close 帧送达。
func TestClassifyEgressBlame_RateLimitCloseFrameIsNotTransport(t *testing.T) {
	for _, text := range []string{"Too many requests", "usage limit reached", "you are being rate limited"} {
		err := &websocket.CloseError{Code: websocket.ClosePolicyViolation, Text: text}
		blame, status := classifyEgressBlame(err)
		if blame == egressBlameTransport {
			t.Errorf("close 帧里的限流(%q)被当成传输故障 —— 这正是把整个作用域打下线的那条路径", text)
		}
		if blame != egressBlameStatus || status != http.StatusTooManyRequests {
			t.Errorf("%q: blame=%v status=%d,期望以 429 上报", text, blame, status)
		}
	}
}

// 上游用 close 帧正常收尾(理由我们不认识)时,节点把协议走完了,它没做错事。
func TestClassifyEgressBlame_OrderlyCloseIsNobodysFault(t *testing.T) {
	err := &websocket.CloseError{Code: websocket.CloseNormalClosure, Text: "done"}
	if blame, _ := classifyEgressBlame(err); blame != egressBlameNone {
		t.Errorf("对端正常关闭不该记到节点账上;得到 %v", blame)
	}
}

// 1006 是"连接断了但没收到 close 帧",那确实是链路问题,必须继续罚。
// 这条守住"别矫枉过正":真出问题的代理仍要被冷却,否则健康检测就瞎了。
func TestClassifyEgressBlame_AbnormalClosureStaysTransport(t *testing.T) {
	err := &websocket.CloseError{Code: websocket.CloseAbnormalClosure, Text: ""}
	if blame, _ := classifyEgressBlame(err); blame != egressBlameTransport {
		t.Errorf("异常断连(1006)是真链路故障,必须仍算节点头上;得到 %v", blame)
	}
}

func TestClassifyEgressBlame_RealTransportFaultsStillBlameTheNode(t *testing.T) {
	cases := map[string]error{
		"网络错误": &net.OpError{Op: "read", Err: errors.New("connection reset by peer")},
		"裸错误":  errors.New("unexpected EOF"),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			if blame, _ := classifyEgressBlame(err); blame != egressBlameTransport {
				t.Errorf("真传输故障必须继续罚节点,否则出口健康检测就瞎了;得到 %v", blame)
			}
		})
	}
}

func TestClassifyEgressBlame_NilIsNotAFailure(t *testing.T) {
	if blame, _ := classifyEgressBlame(nil); blame != egressBlameNone {
		t.Errorf("nil 不是失败;得到 %v", blame)
	}
}
