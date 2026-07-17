package web

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

type ModelSpec struct {
	PublicID      string
	UpstreamModel string
	ProtocolModel string
	Capability    modeldomain.Capability
	Mode          string
	MinimumTier   account.WebTier
}

// MinimumTier 是本地对"上游允许哪档账号用哪个模型"的假设——ListModels 从不
// 反问 Grok,能力表完全由这张表 × 推断出的 tier 算出。所以上游放宽权限时,
// 这里不会自己发现,只会把账号白白挡在门外。
//
// 2026-07-17 实测(grok.com 网页,免费账号):视频生成、视频拓展、图片生成、
// 图片编辑均可用。此前把 video/image-edit 标成 Super,导致 495 个免费号全程
// 闲置,全部流量压在 5 个 Super 号上(视频成功率仅 19/39)。
//
// image-quality 与 chat 的 auto/expert/heavy 未实测,保持原判定。
var catalog = []ModelSpec{
	{PublicID: "grok-chat-fast", UpstreamModel: "grok-chat-fast", Capability: modeldomain.CapabilityChat, Mode: "fast", MinimumTier: account.WebTierBasic},
	{PublicID: "grok-chat-auto", UpstreamModel: "grok-chat-auto", Capability: modeldomain.CapabilityChat, Mode: "auto", MinimumTier: account.WebTierSuper},
	{PublicID: "grok-chat-expert", UpstreamModel: "grok-chat-expert", Capability: modeldomain.CapabilityChat, Mode: "expert", MinimumTier: account.WebTierSuper},
	{PublicID: "grok-chat-heavy", UpstreamModel: "grok-chat-heavy", Capability: modeldomain.CapabilityChat, Mode: "heavy", MinimumTier: account.WebTierHeavy},
	{PublicID: "grok-imagine-image", UpstreamModel: "grok-imagine-image", ProtocolModel: "imagine-lite", Capability: modeldomain.CapabilityImage, Mode: "fast", MinimumTier: account.WebTierBasic},
	{PublicID: "grok-imagine-image-quality", UpstreamModel: "grok-imagine-image-quality", ProtocolModel: "imagine", Capability: modeldomain.CapabilityImage, MinimumTier: account.WebTierSuper},
	{PublicID: "grok-imagine-image-edit", UpstreamModel: "imagine-image-edit", Capability: modeldomain.CapabilityImageEdit, MinimumTier: account.WebTierBasic},
	{PublicID: "grok-imagine-video", UpstreamModel: "grok-imagine-video", ProtocolModel: "imagine-video-gen", Capability: modeldomain.CapabilityVideo, MinimumTier: account.WebTierBasic},
}

func Catalog() []ModelSpec { return append([]ModelSpec(nil), catalog...) }

func Routes() []modeldomain.Route {
	values := make([]modeldomain.Route, 0, len(catalog))
	for _, spec := range catalog {
		publicID, _ := modeldomain.NormalizePublicID(account.ProviderWeb, spec.PublicID)
		values = append(values, modeldomain.Route{PublicID: publicID, Provider: account.ProviderWeb, UpstreamModel: spec.UpstreamModel, Capability: spec.Capability, Enabled: true})
	}
	return values
}

func Resolve(upstreamModel string) (ModelSpec, bool) {
	for _, spec := range catalog {
		if spec.UpstreamModel == upstreamModel {
			return spec, true
		}
	}
	return ModelSpec{}, false
}

func TierSupports(actual, minimum account.WebTier) bool {
	rank := map[account.WebTier]int{account.WebTierBasic: 1, account.WebTierSuper: 2, account.WebTierHeavy: 3}
	return rank[actual] >= rank[minimum]
}
