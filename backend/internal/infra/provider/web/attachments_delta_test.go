package web

import (
	"context"
	"testing"
)

// 参考图/图生视频的素材 URL 由本网关自己重服(publicApiBaseURL),而它监听在
// **8443**。attachments 的校验一度写死"端口必须是 443",于是我们自己发出去的
// 素材 URL 被自己拒收 —— 图生视频的参考图链路整条断掉。
//
// 这条测试是那次修复的见证人。上游把校验挪进了新函数,**并原样保留了那句端口
// 限制**;合并时取上游侧会静默重新拒掉 8443,编译器一声不吭。正解是取上游的
// 结构 + 在新函数里重新删掉端口判断。
func TestValidateRemoteImageURL_AcceptsNonStandardHTTPSPort(t *testing.T) {
	resolver := &rebindingImageResolver{}

	target, err := validateRemoteImageURLWithResolver(
		context.Background(), "https://images.example.test:8443/photo.png", resolver)
	if err != nil {
		t.Fatalf("8443 的 https 素材 URL 被拒了: %v —— 检查校验里是否又出现了 Port() != \"443\" 这类判断", err)
	}
	if target.fetchURL.Host != "93.184.216.34:8443" {
		t.Errorf("端口没带到取回地址上: %s(应为 93.184.216.34:8443)", target.fetchURL.Host)
	}
	if target.hostHeader != "images.example.test:8443" {
		t.Errorf("Host 头丢了端口: %q", target.hostHeader)
	}
}

// 与上一条相对:放开非标端口不等于放开非 https。scheme 校验必须还在,
// 否则素材 URL 会变成一个能打内网 http 服务的 SSRF 面。
func TestValidateRemoteImageURL_StillRejectsNonHTTPS(t *testing.T) {
	resolver := &rebindingImageResolver{}

	for _, raw := range []string{
		"http://images.example.test:8443/photo.png",
		"file:///etc/passwd",
	} {
		if _, err := validateRemoteImageURLWithResolver(context.Background(), raw, resolver); err == nil {
			t.Errorf("%s 应当被拒", raw)
		}
	}
}
