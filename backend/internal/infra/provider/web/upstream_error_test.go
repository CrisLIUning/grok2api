package web

import (
	"errors"
	"net/http"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// Imagine 的 WebSocket error 帧是**上游应用层的拒绝**(绝大多数是限流),
// 不是出口节点的传输故障。
//
// 修复前两处(generateWSImage 与 streamImagineImages)都是这样写的:
//
//	upstreamErr := fmt.Errorf("Imagine WebSocket 返回错误")
//	a.egress.Feedback(ctx, lease.NodeID, 0, upstreamErr)
//
// status=0 且 transportErr 非空,在 egress manager 的 switch 里既不满足成功分支
// (要求 transportErr == nil),也不满足 401/429 与 Build 403/400 那几个豁免分支
// (它们都要求有 status),于是**直落 default**:FailureCount++、Health*0.7、
// 冷却 30 秒(连续失败翻倍至 10 分钟),LastError 还被写成 "transport error"。
//
// 而 grok_web 作用域当时只有一个节点,冷却期内 Acquire 直接硬失败。于是 Grok
// 对 Imagine 的一次限流 = 整个作用域下线 30 秒,把本来健康的会话/视频流量一起
// 打挂。2026-07-19 线上就是这么塌的:
//
//	02:35  imagine-image-quality  502  1187ms  ln-grok_web   ← 限流,冷却了节点
//	02:35  imagine-image          502   253ms  (无出口)       ← 连带受害
//	02:35  imagine-video          502    31ms  (无出口)       ← 连带受害
//	02:35  chat-fast              502   286ms  (无出口)       ← 连会话都挂了
//
// 排查时最迷惑的一点:节点侧看到的 LastError 是 "transport error",于是所有人
// (包括我)都去查代理,而代理从头到尾都是好的。
//
// 第二宗罪是信息丢失:帧内容被 fmt.Errorf 整个丢掉,状态码没了,上层的换号重试
// 循环(image.go 的 executeImage)因此永远认不出这是 429,一次都不轮换。
//
// 所以这里要的是两件事:
//  1. 帧要被解析成带上游状态码的错误 —— 复用 webResponseError 的既有词汇表
//     (code==7 / "anti-bot" → 反机器人;"usage limit"/"usage quota" → 用量到顶)。
//  2. 出口反馈要按类别决定,且**绝不再以 transportErr 的形式上报** —— 节点把
//     消息正确送达了,它没做错任何事。
func TestImagineWSFrameError_UsageLimitCarriesRateLimitStatus(t *testing.T) {
	for name, frame := range map[string]map[string]any{
		"nested error object": {"type": "error", "error": map[string]any{"message": "You have reached your usage limit"}},
		"flat message":        {"type": "error", "message": "Usage quota exceeded for imagine"},
	} {
		t.Run(name, func(t *testing.T) {
			err := imagineWSFrameError(frame)
			if err == nil {
				t.Fatal("error 帧必须产出错误")
			}
			if !errors.Is(err, errWebUsageLimit) {
				t.Errorf("应识别为用量到顶(复用 webResponseError 的词汇表),得到: %v", err)
			}
			status, ok := provider.ErrorHTTPStatus(err)
			if !ok || status != http.StatusTooManyRequests {
				t.Errorf("限流必须带 429 出去,否则上层换号循环认不出来;得到 status=%d ok=%v", status, ok)
			}
		})
	}
}

func TestImagineWSFrameError_AntiBotCarriesForbidden(t *testing.T) {
	for name, frame := range map[string]map[string]any{
		"code 7":          {"type": "error", "error": map[string]any{"message": "Request rejected", "code": float64(7)}},
		"message keyword": {"type": "error", "message": "blocked by anti-bot rules"},
	} {
		t.Run(name, func(t *testing.T) {
			err := imagineWSFrameError(frame)
			if !errors.Is(err, errWebAntiBot) {
				t.Errorf("应识别为反机器人拒绝,得到: %v", err)
			}
			status, ok := provider.ErrorHTTPStatus(err)
			if !ok || status != http.StatusForbidden {
				t.Errorf("反机器人应带 403;得到 status=%d ok=%v", status, ok)
			}
		})
	}
}

// 未知错误不能假装知道状态码 —— 带上一个猜来的 429/5xx 会让上层拿一堆账号去
// 重试一个必然失败的请求(比如参数错误),把账号白白烧进冷却。
func TestImagineWSFrameError_UnknownCarriesNoStatus(t *testing.T) {
	err := imagineWSFrameError(map[string]any{"type": "error", "message": "prompt violates content policy"})
	if err == nil {
		t.Fatal("error 帧必须产出错误")
	}
	if status, ok := provider.ErrorHTTPStatus(err); ok {
		t.Errorf("未知错误不应携带状态码(会诱发无意义的换号重试);得到 %d", status)
	}
	if errors.Is(err, errWebUsageLimit) || errors.Is(err, errWebAntiBot) {
		t.Error("不应被误分类")
	}
}

// 帧里没有任何可读信息时也不能返回 nil —— 调用方靠非 nil 来终止循环。
func TestImagineWSFrameError_EmptyFrameStillErrors(t *testing.T) {
	if err := imagineWSFrameError(map[string]any{"type": "error"}); err == nil {
		t.Fatal("空 error 帧也必须产出非 nil 错误")
	}
}

// 出口归因(限流不得被算成传输故障)的断言已迁到 egress_blame_test.go 的
// classifyEgressBlame 上 —— 那里连 close 帧、异常断连、真传输故障一并覆盖,
// 而这里原先只测得到 error 帧这一种形状。

// createMediaPost 曾把四种完全不同的失败塌成一句无状态码的错误:
//
//	if response.StatusCode < 200 || response.StatusCode >= 300 ||
//		json.NewDecoder(...).Decode(&value) != nil || value.Post.ID == "" {
//		return "", fmt.Errorf("创建媒体 Post 失败")
//	}
//
// 它同时位于图片编辑(image.go:757)和**文生视频的种子 post**(video.go:84)路径上。
// 上游在这里回 429 时,状态码被丢掉,于是:
//   - sanitizeVideoFailure 拿不到 429 → 归类成 generation_failed 而不是 rate_limited;
//   - 视频/图片的换号重试循环看不出这值得换个账号 → 一次都不轮换,任务直接判死。
//
// 分开归类之后,只有真正的上游 HTTP 失败才带状态码;解析失败和"响应缺 id"是
// 我们这侧或上游契约的问题,在别的账号上会一模一样地失败,给它们状态码只会
// 诱发无意义的轮换,把账号白白烧进冷却。
func TestMediaPostFailure_PreservesUpstreamStatus(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		err := mediaPostFailure(status, nil, "")
		if err == nil {
			t.Fatalf("上游 %d 必须产出错误", status)
		}
		got, ok := provider.ErrorHTTPStatus(err)
		if !ok || got != status {
			t.Errorf("上游 %d 的状态码必须带出去(上层靠它决定是否换号重试);得到 status=%d ok=%v", status, got, ok)
		}
	}
}

func TestMediaPostFailure_LocalProblemsCarryNoStatus(t *testing.T) {
	cases := map[string]error{
		"响应无法解析":      mediaPostFailure(http.StatusOK, errors.New("unexpected EOF"), ""),
		"响应缺 post id": mediaPostFailure(http.StatusOK, nil, ""),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Fatal("必须产出错误")
			}
			if status, ok := provider.ErrorHTTPStatus(err); ok {
				t.Errorf("这不是可换号重试的上游失败,不应携带状态码;得到 %d", status)
			}
		})
	}
}

func TestMediaPostFailure_SuccessIsNil(t *testing.T) {
	if err := mediaPostFailure(http.StatusOK, nil, "post_123"); err != nil {
		t.Errorf("2xx + 可解析 + 有 id 应视为成功;得到 %v", err)
	}
}
