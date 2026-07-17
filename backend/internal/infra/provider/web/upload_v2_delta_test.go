package web

import (
	"net/http"
	"testing"
)

// Grok 的 V2 直传端点在 **imagine 上传**(图片编辑 / 图生视频素材)上对我们的免费号池不可用:
// 2026-07-17 线上实测,图片编辑经 V2 连续 6 次拿到 429 "Too many requests",
// 而同一时刻同一批号的普通图片生成 200、legacy 上传也 200(合并前一直靠它)。
// 两条上传路的限流是各自独立的。
//
// 上游的回退只认"端点根本不存在"(404/405/410/501),429 属于"端点在、这次失败了",
// 不回退——**这个设计对上游是自洽的**:模糊状态下回退会掩盖真错误、双倍打上游。
// 所以不该去掰它的回退语义,而应该干脆不 opt-in V2:对我们它就是不通。
//
// 只在有证据的地方分叉:聊天附件(attachments.go)走的是另一条上传,我们没有实测过
// 它失败,故保持上游的 V2 行为不动。
//
// 这条测试钉住这个决定。哪天 imagine 的 V2 对我们的号池可用了,把 useDirectFileUploadV2
// 翻成 true 并改这里——但要先有实测,别再凭"上游有了所以我们也要"。
func TestDirectFileUploadV2_StaysOptedOut(t *testing.T) {
	if useDirectFileUploadV2 {
		t.Error("imagine 的 V2 直传在我们的免费号池上返回 429(2026-07-17 实测),开启会让图片编辑失效")
	}
}

// 回退语义本身保持与上游一致——我们没有理由让它和上游分叉,
// 分叉了将来每次跟随都要重新判一次。
func TestDirectFileUploadFallback_MatchesUpstreamSemantics(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusGone, http.StatusNotImplemented} {
		if !directFileUploadFallbackStatus(status) {
			t.Errorf("status %d 表示端点不存在,应允许回退 legacy", status)
		}
	}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden, http.StatusInternalServerError} {
		if directFileUploadFallbackStatus(status) {
			t.Errorf("status %d 是模糊状态(端点在、这次失败),不应回退——会掩盖真错误", status)
		}
	}
}
