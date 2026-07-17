package conversation

import "strings"

func chatResponse(value parsedResponse) map[string]any {
	message := map[string]any{"role": "assistant", "content": value.Text}
	if value.Reasoning != "" {
		message["reasoning_content"] = value.Reasoning
	}
	finishReason := "stop"
	if len(value.Calls) > 0 {
		finishReason = "tool_calls"
		if value.Text == "" {
			message["content"] = nil
		}
		calls := make([]any, 0, len(value.Calls))
		for _, call := range value.Calls {
			calls = append(calls, map[string]any{
				"id": call.CallID, "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
			})
		}
		message["tool_calls"] = calls
	} else if value.Refusal != "" {
		finishReason = "content_filter"
	} else if value.Status == "incomplete" {
		finishReason = "length"
	}
	if value.Refusal != "" {
		message["refusal"] = value.Refusal
	}
	if len(value.Annotations) > 0 {
		message["annotations"] = value.Annotations
	}
	id := strings.Replace(value.ID, "resp_", "chatcmpl_", 1)
	return map[string]any{
		"id": id, "object": "chat.completion", "created": value.CreatedAt, "model": value.Model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
		"usage":   chatUsage(value.Usage),
	}
}

// chatUsage 只对外给 token 计数。
//
// FORK DELTA:上游在此透传 cost_in_usd_ticks / num_sources_used /
// num_server_side_tools_used / context_details。对上游合理——他们的用户就是
// Grok 账号主人,看自己的成本天经地义。对我们不成立:/v1 是转售面,调用方按
// client key 计费,他知道我们收多少,再看到上游成本多少,一减就是毛利。
//
// responseUsage 仍然解析这些字段,它们对我们自己的成本核算有用;只是不外露。
// 见 usage_privacy_delta_test.go。
func chatUsage(value responseUsage) map[string]any {
	total := value.TotalTokens
	if total == 0 {
		total = value.InputTokens + value.OutputTokens
	}
	return map[string]any{
		"prompt_tokens": value.InputTokens, "completion_tokens": value.OutputTokens,
		"total_tokens":              total,
		"prompt_tokens_details":     map[string]any{"cached_tokens": value.InputTokensDetails.CachedTokens},
		"completion_tokens_details": map[string]any{"reasoning_tokens": value.OutputTokensDetails.ReasoningTokens},
	}
}
