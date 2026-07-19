package web

import (
	"errors"
	"testing"
)

// webResponseError 是全仓唯一的上游错误词汇表:WS 帧、SSE 帧、以及新的出口归因
// 都从它取"这是限流还是反机器人"。词汇表漏一种说法,对应的整条路径就静默失效。
//
// 它原本只认 "usage limit" / "usage quota"。但仓库里躺着一份**实际抓到的**上游
// 响应(引用在 video.go 的脱敏说明里):
//
//	{"message":"视频上游返回 429: {\"error\":{\"code\":8,\"message\":\"Too many requests\"}}"}
//
// code 8 / "Too many requests" 这一形状此前完全不被识别,于是:
//   - 出口归因把它当成未知错误 → 落到传输故障分支 → 冷却节点(正是本次事故);
//   - 换号重试认不出这是限流 → 不轮换。
//
// code 的取值不是巧合:Grok 用的是 gRPC 标准状态码,7 = PERMISSION_DENIED
// (被 anti-bot 拦下),8 = RESOURCE_EXHAUSTED(配额/限流)。既有代码已经在认 7,
// 补上 8 是把同一套约定认全。
func TestWebResponseError_RecognizesGrokRateLimitWording(t *testing.T) {
	cases := map[string]map[string]any{
		"gRPC RESOURCE_EXHAUSTED": {"code": float64(8), "message": "Too many requests"},
		"too many requests":       {"message": "Too many requests"},
		"rate limit":              {"message": "You are being rate limited, slow down"},
		"usage limit(既有)":         {"message": "You have reached your usage limit"},
		"usage quota(既有)":         {"message": "Usage quota exceeded"},
	}
	for name, frame := range cases {
		t.Run(name, func(t *testing.T) {
			if err := webResponseError(frame); !errors.Is(err, errWebUsageLimit) {
				t.Errorf("应识别为用量到顶/限流,得到: %v", err)
			}
		})
	}
}

// 反机器人的既有识别不能被上面的扩充破坏。
func TestWebResponseError_StillRecognizesAntiBot(t *testing.T) {
	cases := []map[string]any{
		{"code": float64(7), "message": "Request rejected"},
		{"message": "blocked by anti-bot rules"},
	}
	for _, frame := range cases {
		if err := webResponseError(frame); !errors.Is(err, errWebAntiBot) {
			t.Errorf("应识别为反机器人,得到: %v", err)
		}
	}
}

// 词汇表不能过度捕捉:内容策略之类的拒绝在所有账号上都会一样地失败,误判成限流
// 会让上层拿一串账号去撞同一堵墙,并把它们挨个烧进冷却。
func TestWebResponseError_DoesNotOverMatch(t *testing.T) {
	cases := []map[string]any{
		{"message": "prompt violates content policy"},
		{"message": "invalid aspect ratio"},
		{"code": float64(3), "message": "invalid argument"},
	}
	for _, frame := range cases {
		err := webResponseError(frame)
		if errors.Is(err, errWebUsageLimit) || errors.Is(err, errWebAntiBot) {
			t.Errorf("%v 不该被归类为限流或反机器人", frame)
		}
	}
}
