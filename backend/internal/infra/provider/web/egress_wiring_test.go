package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 允许直接调用 egress.Feedback 的唯一文件 —— feedbackUpstreamError 就住在这里。
const egressFeedbackOwner = "upstream_error.go"

// 形如 Feedback(ctx, nodeID, 0, err):status=0 且带一个非 nil 的 transportErr,
// 等于断言"这是链路故障"。
//
// 中间用 `.*` 而不是 `[^)]*` —— 实参里就有括号(context.WithoutCancel(ctx)),
// 用后者会在第一个 `)` 处截断,让这条守卫变成永远通过的空壳。这不是假设:
// 初版就是这么写的,靠反向验证(临时塞回一处裸调用看它红不红)才发现。
var rawTransportFeedback = regexp.MustCompile(`\.egress\.Feedback\(.*,\s*0,\s*[A-Za-z]`)

// 这条不变量守的是**接线**,而不是某个函数的行为。
//
// 事故的形状是一行调用:
//
//	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, upstreamErr)
//
// status=0 且 transportErr 非空,在 egress manager 里直落 default 分支 —— 冷却
// 节点 30 秒(翻倍至 10 分钟)。而调用点拿到的往往是**上游的应用层拒绝**(限流、
// 反机器人、内容审核),不是链路故障。grok_web 当时只有一个节点,于是 Grok 对
// Imagine 的一次限流把整个作用域打下线,连健康的会话流量一起挂掉;运维侧看到的
// 却是 "transport error",查了很久代理,而代理始终是好的。
//
// 这个模式有两个特别难防的性质:
//
//  1. **它看起来完全正常。** 每一处单独看都像是"上报一次失败",没有任何刺眼之处。
//     全仓一度有 14 处,是同一个笔误被复制了 14 遍。
//  2. **纯函数测试挡不住它。** classifyEgressBlame 有完整的单元测试,但复核实测
//     确认:把调用点改回裸 Feedback、只保留辅助函数,整套测试依然全绿。真正会
//     打挂线上的那一行,没有任何东西守着。
//
// 所以这里直接盯源码:归因必须走 feedbackUpstreamError,它是唯一有资格断言
// "这是链路故障"的地方。成功上报(Feedback(..., StatusOK, nil))不受影响。
func TestEgressFeedbackGoesThroughTheBlameClassifier(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") || name == egressFeedbackOwner {
			continue
		}
		source, readErr := os.ReadFile(filepath.Clean(name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		checked++
		for index, line := range strings.Split(string(source), "\n") {
			if rawTransportFeedback.MatchString(line) {
				t.Errorf("%s:%d 直接把失败当成链路故障上报了:\n\t%s\n\n"+
					"请改用 a.feedbackUpstreamError(ctx, nodeID, err) —— 归因交给 classifyEgressBlame。\n"+
					"直接写 Feedback(..., 0, err) 等于断言\"这是链路故障\",而上游的限流/反机器人/内容审核\n"+
					"会因此冷却一个完全健康的出口节点(2026-07-19 线上事故的成因)。",
					name, index+1, strings.TrimSpace(line))
			}
		}
	}
	if checked == 0 {
		t.Fatal("一个源文件都没扫到 —— 这条不变量失效了(工作目录或文件名约定变了?)")
	}
}
