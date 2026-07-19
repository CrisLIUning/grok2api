package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/browserheaders"
)

func buildHeaders(token string, lease *infraegress.Lease, contentType string) http.Header {
	if contentType == "" {
		contentType = "application/json"
	}
	value := http.Header{}
	value.Set("Content-Type", contentType)
	value.Set("Accept", "*/*")
	value.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	value.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	value.Set("User-Agent", lease.UserAgent)
	value.Set("Cookie", infraegress.BuildSSOCookie(token, lease.CFCookies))
	value.Set("x-xai-request-id", newRequestUUID())
	return value
}

// applyAppHeaders 补齐真实浏览器同源 fetch 会携带的稳定请求头，不伪造 Sentry。
//
// Client Hints 现在补：不发它才是矛盾。我们的 TLS 握手用 Chrome_146 profile、
// User-Agent 也写着 Chrome/146，而真实 Chrome 在同源 fetch 上必定携带
// Sec-Ch-Ua —— 声称是 Chrome 却一个提示头都不发，本身就是个可被打分的信号。
//
// 2026-07-19 线上 grok.com 对所有出口下发 Cloudflare JS 挑战，与 IP 无关
// （6 个探测 IP、39 个新出口 IP 全部命中），而同期走 api.x.ai 的 build/console
// 完好 —— 那两条路本来就带 Client Hints。上游 f15b735 造了 browserheaders 包，
// 却只接在 console / sessionidentity / account_settings 上，漏了 grok_web。
//
// UA 从同一个 header 上读：调用方都是先 buildHeaders 再 applyAppHeaders，
// 所以此刻 User-Agent 已就位；非 Chromium 的 UA 不会产出任何提示头。
func applyAppHeaders(value http.Header, origin, referer string) {
	value.Set("Origin", origin)
	value.Set("Referer", referer)
	value.Set("Cache-Control", "no-cache")
	value.Set("Pragma", "no-cache")
	value.Set("Priority", "u=1, i")
	value.Set("Sec-Fetch-Dest", "empty")
	value.Set("Sec-Fetch-Mode", "cors")
	value.Set("Sec-Fetch-Site", "same-origin")
	browserheaders.ApplyChromiumClientHints(value, value.Get("User-Agent"))
}

func newRequestUUID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return newWebID("req")
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
