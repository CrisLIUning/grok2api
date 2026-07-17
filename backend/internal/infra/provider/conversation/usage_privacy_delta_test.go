package conversation

import "testing"

// 我们是**转售面**:/v1 由 client key 鉴权、按 key 计费,调用方是我们的客户,
// 不是 Grok 账号的主人。而 responseUsage 里带着 Grok 报的单次真实成本 ——
// 客户知道我们收他多少,再看到上游成本多少,一减就是我们的毛利。
//
// 上游(chenyme)把这些字段直接透传给调用方是合理的:他们的用户就是账号主人,
// 看自己的成本天经地义。**但对我们不成立。**
//
// 这条测试是那个边界的见证人:合并上游 conversation/ 时,它们的 chatUsage /
// anthropicUsage 会带进 cost_in_usd_ticks 等字段,这里会立刻变红。
// 别把它删了了事 —— 要么剥字段,要么先想清楚为什么可以外露。
var resaleForbiddenUsageKeys = []string{
	"cost_in_usd_ticks",          // Grok 报的单次成本 —— 直接反推毛利
	"num_sources_used",           // 检索条数,同样是成本侧信息
	"num_server_side_tools_used", // 同上
	"context_details",            // 上游的上下文成本明细
}

func TestChatUsage_DoesNotLeakUpstreamCost(t *testing.T) {
	usage := chatUsage(responseUsage{InputTokens: 100, OutputTokens: 50})

	for _, key := range resaleForbiddenUsageKeys {
		if _, exists := usage[key]; exists {
			t.Errorf("chatUsage 泄漏了 %q —— 我们是转售面,调用方据此可反推毛利", key)
		}
	}
	// 正常的 token 计数必须还在:客户按 token 计费,这是他该看到的。
	if usage["prompt_tokens"] == nil || usage["completion_tokens"] == nil || usage["total_tokens"] == nil {
		t.Fatalf("token 计数丢了: %#v", usage)
	}
}

func TestAnthropicUsage_DoesNotLeakUpstreamCost(t *testing.T) {
	usage := anthropicUsage(responseUsage{InputTokens: 100, OutputTokens: 50}, 0)

	for _, key := range resaleForbiddenUsageKeys {
		if _, exists := usage[key]; exists {
			t.Errorf("anthropicUsage 泄漏了 %q —— 我们是转售面,调用方据此可反推毛利", key)
		}
	}
	if usage["input_tokens"] == nil || usage["output_tokens"] == nil {
		t.Fatalf("token 计数丢了: %#v", usage)
	}
}
