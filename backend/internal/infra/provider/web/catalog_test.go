package web

import (
	"context"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func listModelsFor(t *testing.T, tier account.WebTier) map[string]bool {
	t.Helper()
	adapter := &Adapter{}
	values, err := adapter.ListModels(context.Background(), account.Credential{WebTier: tier})
	if err != nil {
		t.Fatalf("ListModels(%s) 出错: %v", tier, err)
	}
	got := make(map[string]bool, len(values))
	for _, value := range values {
		got[value] = true
	}
	return got
}

// 2026-07-17 用免费账号经本网关实测 Grok:Imagine 全家桶全部返回 200
// (视频还生成出了真实 mp4)。早前把它们标成 Super,后果不是报错而是**静默
// 减产**——免费号拿不到能力,路由层直接跳过,495 个号在 Imagine 上全程零请求。
// 没有断言就没人会发现。
func TestListModels_BasicTierGetsAllImagineModels(t *testing.T) {
	got := listModelsFor(t, account.WebTierBasic)

	for _, model := range []string{
		"grok-imagine-image",
		"grok-imagine-image-quality",
		"imagine-image-edit",
		"grok-imagine-video",
	} {
		if !got[model] {
			t.Errorf("免费账号应能用 %s(实测 200),否则它会被路由层跳过", model)
		}
	}
}

// 与上一条相对:chat 模式是 Grok 真正按订阅锁的东西。实测免费号请求
// auto/expert/heavy 一律拿到 403 "Model is not found"。放开它们不会带来产能,
// 只会让请求白跑一趟上游才被拒——烧往返、烧账号信誉。
func TestListModels_BasicTierExcludesPaidChatModes(t *testing.T) {
	got := listModelsFor(t, account.WebTierBasic)

	for _, model := range []string{"grok-chat-auto", "grok-chat-expert", "grok-chat-heavy"} {
		if got[model] {
			t.Errorf("免费账号不该拿到 %s(实测 403 Model is not found)", model)
		}
	}
	if !got["grok-chat-fast"] {
		t.Error("grok-chat-fast 是免费号唯一可用的 chat 模式(实测 200),不能漏")
	}
}

// auto = 尚未探明档位。按最小权限当 basic 处理,不能因为"没测出来"就少给能力。
func TestListModels_AutoTierMatchesBasic(t *testing.T) {
	auto, basic := listModelsFor(t, account.WebTierAuto), listModelsFor(t, account.WebTierBasic)

	if len(auto) != len(basic) {
		t.Fatalf("auto 应与 basic 等价: auto=%d basic=%d", len(auto), len(basic))
	}
	for model := range basic {
		if !auto[model] {
			t.Errorf("auto 档缺少 %s", model)
		}
	}
}

// 空 WebTier(账号刚导入、尚未同步)同样按 basic 兜底,不能一个模型都不给。
func TestListModels_EmptyTierFallsBackToBasic(t *testing.T) {
	if got := listModelsFor(t, ""); !got["grok-chat-fast"] {
		t.Error("空档位应回退到 basic,至少要有 grok-chat-fast")
	}
}

// 分档仍然有意义:heavy 是 SuperGrok Heavy 专属,不能漏给低档账号——
// 放开会让请求打到上游才被拒,白烧一次往返和账号信誉。
func TestListModels_HeavyChatStaysGated(t *testing.T) {
	for _, tier := range []account.WebTier{account.WebTierBasic, account.WebTierSuper} {
		if listModelsFor(t, tier)["grok-chat-heavy"] {
			t.Errorf("%s 档不应拿到 grok-chat-heavy", tier)
		}
	}
	if !listModelsFor(t, account.WebTierHeavy)["grok-chat-heavy"] {
		t.Error("heavy 档应能用 grok-chat-heavy")
	}
}

// 高档账号必须涵盖低档的全部能力,否则升级订阅反而丢功能。
func TestListModels_HigherTierIsSuperset(t *testing.T) {
	basic, super, heavy := listModelsFor(t, account.WebTierBasic), listModelsFor(t, account.WebTierSuper), listModelsFor(t, account.WebTierHeavy)

	for model := range basic {
		if !super[model] {
			t.Errorf("super 档缺少 basic 档的 %s", model)
		}
	}
	for model := range super {
		if !heavy[model] {
			t.Errorf("heavy 档缺少 super 档的 %s", model)
		}
	}
}
