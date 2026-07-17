package web

import (
	"net/http"
	"strings"
	"testing"
)

// statsig 签名是 Grok 的反爬门票:没有有效签名的请求会被判 403
// "Request rejected by anti-bot rules"。
//
// Warm 的价值在它自己的注释里:"使用一次 metaContent 请求预热多个常用签名键,
// **避免按账号或按路径重复抓取首页**"。也就是说——**没预热的路径不是不能用,
// 而是每次调用都要额外现抓一次 Grok 首页**:慢、多一次暴露面、更容易撞反爬。
//
// 上传是图片编辑与图生视频参考图的必经之路,却是唯一一个没进预热名单的热路径。
//
// ⚠️ 别把这条测试当成"图片编辑低成功率"的修复:实测加上 upload-file 后成功率
// 仍是 0/10。真正的根因是所有账号共用一个出口 IP、Grok 按 IP 限流(成功率与
// 请求量成反比),解法在粘性代理。这条只是少一个首页往返而已。
func TestStatsigWarmTargets_CoverEveryHotEndpoint(t *testing.T) {
	targets := statsigWarmTargetsFor("https://grok.com")

	paths := make(map[string]bool, len(targets))
	for _, target := range targets {
		if target.method != http.MethodPost {
			t.Errorf("预热目标应为 POST: %#v", target)
		}
		index := strings.Index(target.target, "/rest/")
		if index < 0 {
			t.Fatalf("预热目标不是 /rest/ 端点: %s", target.target)
		}
		paths[target.target[index:]] = true
	}

	// 每一条都是被真实热路径调用的端点,漏一个就是那条链路每次多抓一次首页。
	for endpoint, usedBy := range map[string]string{
		"/rest/app-chat/conversations/new": "聊天 / 图片生成 / 视频生成",
		"/rest/rate-limits":                "额度与档位探测",
		"/rest/media/post/create":          "高质量图",
		"/rest/app-chat/upload-file":       "图片编辑 / 图生视频参考图",
	} {
		if !paths[endpoint] {
			t.Errorf("%s 没进 statsig 预热名单,而它被 %s 依赖 —— 每次调用都会额外抓首页并更易撞反爬", endpoint, usedBy)
		}
	}
}

// 预热是一次 metaContent 请求签多个键,所以加目标近乎零成本;
// 但目标必须真的是被调用的端点,否则只是在给自己造无用签名。
func TestStatsigWarmTargets_NoDuplicates(t *testing.T) {
	targets := statsigWarmTargetsFor("https://grok.com")

	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		key := target.method + " " + target.target
		if seen[key] {
			t.Errorf("预热目标重复: %s", key)
		}
		seen[key] = true
	}
	if len(targets) < 4 {
		t.Errorf("预热目标只有 %d 个,四条热路径应各有其一", len(targets))
	}
}
