package web

import (
	"net/http"
	"strings"
	"testing"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

// grok_web 的请求指纹此前是**自相矛盾**的:
//
//   - TLS 握手用 profiles.Chrome_146(egress/tlsclient.go:49)
//   - User-Agent 写着 Chrome/146.0.0.0
//   - Sec-Fetch-Dest / Mode / Site 都在
//   - 但 **一个 Sec-Ch-Ua 都不发**
//
// applyAppHeaders 的原注释写着"不伪造 Sentry 或 Client Hints"。不伪造 Sentry 是
// 对的(那是站点自己的埋点,伪造反而露馅);但 Client Hints 不是伪造 —— 真实
// Chrome 在同源 fetch 上**必定**发送 Sec-Ch-Ua,声称自己是 Chrome 却不发,才是
// 矛盾。
//
// 2026-07-19 线上:grok.com 对所有出口下发 Cloudflare JS 挑战
// (响应体 <title>Just a moment...</title>),6 个探测 IP 与 39 个新出口 IP
// 全部命中,与 IP 无关;同期走 api.x.ai 的 build/console 完全正常 —— 那两条路
// 恰好本来就带 Client Hints(console/headers.go)。
//
// 上游 f15b735 造了 browserheaders 包来补这些头,却只接在 console /
// sessionidentity / account_settings 上,**没接到 grok_web 这条路**。这里补上。
//
// 说明:这是基于指纹一致性的假设,不是已验证的解药。即使 Cloudflare 这次不是
// 因为它下发挑战,"声称是 Chrome 就该发 Chrome 会发的头"本身也是对的。
func TestApplyAppHeaders_EmitsClientHintsConsistentWithUserAgent(t *testing.T) {
	lease := &infraegress.Lease{
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
	}
	header := buildHeaders("token", lease, "")
	applyAppHeaders(header, "https://grok.com", "https://grok.com/")

	ua := header.Get("Sec-Ch-Ua")
	if ua == "" {
		t.Fatal("声称 Chrome 却不发 Sec-Ch-Ua —— 真实 Chrome 同源 fetch 必带,这是自相矛盾的指纹")
	}
	if !strings.Contains(ua, `"146"`) {
		t.Errorf("Sec-Ch-Ua 的版本必须与 User-Agent 的 Chrome/146 一致;得到 %q", ua)
	}
	if got := header.Get("Sec-Ch-Ua-Platform"); got != `"macOS"` {
		t.Errorf("平台应与 UA 里的 Mac OS X 一致;得到 %q", got)
	}
	if got := header.Get("Sec-Ch-Ua-Mobile"); got != "?0" {
		t.Errorf("桌面 UA 的 Sec-Ch-Ua-Mobile 应为 ?0;得到 %q", got)
	}
	// 既有头不能被破坏。
	if header.Get("Sec-Fetch-Site") != "same-origin" || header.Get("Origin") != "https://grok.com" {
		t.Error("原有的同源 fetch 头被改坏了")
	}
}

// 非 Chromium 的 UA 不能凭空造 Chromium 提示头 —— 那才是真的伪造,而且是更明显的
// 矛盾。上游 browserheaders 的实现本身就守着这条,这里把它钉在我们的调用点上。
func TestApplyAppHeaders_NoHintsForNonChromiumUserAgent(t *testing.T) {
	lease := &infraegress.Lease{
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
			"AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15",
	}
	header := buildHeaders("token", lease, "")
	applyAppHeaders(header, "https://grok.com", "https://grok.com/")

	for _, k := range []string{"Sec-Ch-Ua", "Sec-Ch-Ua-Platform", "Sec-Ch-Ua-Mobile"} {
		if v := header.Get(k); v != "" {
			t.Errorf("Safari 的 UA 不应产生 %s(得到 %q)—— 那是自相矛盾的指纹", k, v)
		}
	}
}

// UA 缺失时不能崩,也不能瞎猜。
func TestApplyAppHeaders_ToleratesMissingUserAgent(t *testing.T) {
	header := http.Header{}
	applyAppHeaders(header, "https://grok.com", "https://grok.com/")
	if v := header.Get("Sec-Ch-Ua"); v != "" {
		t.Errorf("没有 UA 就不该产出 Sec-Ch-Ua;得到 %q", v)
	}
}
