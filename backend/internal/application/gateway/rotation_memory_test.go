package gateway

import (
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// 账号轮转的记忆必须跨得过进程重启,否则号池再大也只有前几十个在干活。
//
// 线上实测(1803 个 grok_web 账号):
//
//	启用中 1803   冷却中 0   被 429 罚过 20   —— 那 20 个的 id 是 17,18,19,21,22,23…
//	其余 1783 个账号,一次都没有被选中过。
//
// 成因是两件事叠加:
//
//  1. lastSelectedAt 是内存 map(selector.go:117 make(...)),进程一重启就清空。
//  2. 打分的最终裁决是 `return left.ID < right.ID`。basic 账号没有配额窗口,
//     remaining/billingFresh/inFlight/tier/priority 全部打平,于是一路平到最后
//     一条,由 **id 最小者胜出**。
//
// 合起来:每次重启都从最小 id 重新挑起,把同一批账号反复烧穿。2026-07-19 为发版
// 重启了六次容器,等于把 id 17-29 那一段烧了六轮,而 id 100 以后的从未被碰过。
//
// 持久化的 provider_accounts.last_used_at 记录的正是同一件事,所以内存没有记录时
// 回落到它 —— 轮转的记忆就跨过了重启。
func TestRotationLastSelected_FallsBackToPersistedLastUsed(t *testing.T) {
	persisted := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	credential := account.Credential{ID: 17, LastUsedAt: &persisted}

	got := rotationLastSelected(map[uint64]time.Time{}, credential)
	if !got.Equal(persisted) {
		t.Fatalf("内存无记录时应回落到持久化的 last_used_at;期望 %v,得到 %v", persisted, got)
	}
}

// 内存里的记录更新鲜,优先于持久值 —— 它记的是"本进程刚把它交出去",
// 而持久值可能还停在上一次落库时。
func TestRotationLastSelected_InMemoryWins(t *testing.T) {
	persisted := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 7, 19, 9, 30, 0, 0, time.UTC)
	credential := account.Credential{ID: 17, LastUsedAt: &persisted}

	got := rotationLastSelected(map[uint64]time.Time{17: recent}, credential)
	if !got.Equal(recent) {
		t.Errorf("内存记录应优先;期望 %v,得到 %v", recent, got)
	}
}

// 真正从未被选中过的账号返回零值,好让它排在所有用过的账号前面 —— 这正是
// 我们想要的:先用没碰过的。
func TestRotationLastSelected_NeverUsedSortsFirst(t *testing.T) {
	fresh := account.Credential{ID: 1500}
	used := account.Credential{ID: 17}
	usedAt := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	used.LastUsedAt = &usedAt

	freshTime := rotationLastSelected(map[uint64]time.Time{}, fresh)
	usedTime := rotationLastSelected(map[uint64]time.Time{}, used)
	if !freshTime.Before(usedTime) {
		t.Errorf("没被碰过的账号应排在用过的之前;fresh=%v used=%v", freshTime, usedTime)
	}
	if !freshTime.IsZero() {
		t.Errorf("从未使用应为零值,得到 %v", freshTime)
	}
}

// 打分链路上的验证:两个各方面都打平的账号,只有轮转时间不同时,
// 更久没用的那个必须胜出 —— 而不是 id 小的那个。
func TestCandidateScoreBetter_RotationBeatsLowestID(t *testing.T) {
	older := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)

	values := []account.RoutingCandidate{
		{Credential: account.Credential{ID: 17}, SupportsModel: true, ModelCapabilityKnown: true},
		{Credential: account.Credential{ID: 1500}, SupportsModel: true, ModelCapabilityKnown: true},
	}
	// id 小的那个刚用过,id 大的那个更久没用。
	low := candidateScore{index: 0, lastSelected: newer}
	high := candidateScore{index: 1, lastSelected: older}

	if candidateScoreBetter(values, low, high) {
		t.Error("id 小但刚用过的账号不该胜出 —— 否则号池永远只轮到最前面那几十个")
	}
	if !candidateScoreBetter(values, high, low) {
		t.Error("更久没用的账号应当胜出")
	}
}
