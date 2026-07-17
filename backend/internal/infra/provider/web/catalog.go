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
// 反问 Grok,能力表完全由这张表 × 推断出的 tier 算出。上游放宽权限时这里不会
// 自己发现,只会把账号白白挡在门外:能力表里没有对应行,选号器就在发出任何上游
// 请求之前跳过该账号。**这是静默减产,不报错,没有测试就没人会知道。**
//
// 2026-07-17 用免费账号经本网关逐个实测(打真实 Grok,request_audits 有据可查)。
// Grok 的分档规律很干净:**按订阅锁的只有 chat 模式,Imagine 全家桶对免费号全开**。
//
//	grok-chat-fast              免费 200 ✓
//	grok-chat-auto              免费 403 "Model is not found" ✗
//	grok-chat-expert            免费 403 "Model is not found" ✗
//	grok-chat-heavy             免费 403 "Model is not found" ✗
//	grok-imagine-image          免费 200 ✓
//	grok-imagine-image-quality  免费 200 ✓  ← 此前误标 Super
//	grok-imagine-image-edit     免费 200 ✓  ← 此前误标 Super
//	grok-imagine-video          免费生成出真实 mp4 ✓  ← 此前误标 Super
//
// 误标的代价:495 个免费号在 Imagine 上全程零请求,流量全压在 5 个 Super 号上,
// 视频成功率只有 19/39。
//
// 判"不支持"要认准 Grok 的 **403 "Model is not found"**。429 "Too many requests"
// 只是限流、权限是通的,别当成不支持——实测视频首发就是 429,换个号重试即成功。
var catalog = []ModelSpec{
	{PublicID: "grok-chat-fast", UpstreamModel: "grok-chat-fast", Capability: modeldomain.CapabilityChat, Mode: "fast", MinimumTier: account.WebTierBasic},
	{PublicID: "grok-chat-auto", UpstreamModel: "grok-chat-auto", Capability: modeldomain.CapabilityChat, Mode: "auto", MinimumTier: account.WebTierSuper},
	{PublicID: "grok-chat-expert", UpstreamModel: "grok-chat-expert", Capability: modeldomain.CapabilityChat, Mode: "expert", MinimumTier: account.WebTierSuper},
	{PublicID: "grok-chat-heavy", UpstreamModel: "grok-chat-heavy", Capability: modeldomain.CapabilityChat, Mode: "heavy", MinimumTier: account.WebTierHeavy},
	{PublicID: "grok-imagine-image", UpstreamModel: "grok-imagine-image", ProtocolModel: "imagine-lite", Capability: modeldomain.CapabilityImage, Mode: "fast", MinimumTier: account.WebTierBasic},
	{PublicID: "grok-imagine-image-quality", UpstreamModel: "grok-imagine-image-quality", ProtocolModel: "imagine", Capability: modeldomain.CapabilityImage, MinimumTier: account.WebTierBasic},
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
