package account

import (
	"testing"
	"time"
)

// ExhaustQuota 在拿不到恢复时间时会把 reset_at 写成 NULL,而且**跳过恢复调度**:
//
//	if err := s.accounts.ExhaustQuotaWindow(ctx, id, mode, resetAt, s.now()); err != nil { ... }
//	if resetAt != nil && s.quotaQueue != nil {
//		return s.quotaQueue.ScheduleQuotaRecovery(...)
//	}
//	return nil
//
// 两头一起失守:恢复扫描过滤 reset_at IS NOT NULL,选号又排除 Remaining<=0 的
// 账号,于是这个账号对该 mode **永久出局** —— 没有任何东西会再把它捞回来。
//
// resetAt 为 nil 的路径不是罕见分支:该 mode 此前没有配额窗口(新模式、或从未
// 同步过的账号)、窗口有记录但 WindowSeconds<=0、GetQuotaWindows 报错 —— 任何
// 一种都会落到这里。
//
// 所以耗尽必须永远是**有界**的:拿不到真实恢复时间时给一个保守的兜底,让账号
// 一定会被重新探测。上游若仍在限流,下一次探测会再次耗尽,是自我纠正的;而
// 写 NULL 是不可逆的。
func TestResolveQuotaResetAt_NilFallsBackToBoundedRecovery(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	got := resolveQuotaResetAt(nil, now)
	if !got.After(now) {
		t.Fatalf("拿不到恢复时间时必须给一个未来的兜底,否则账号对该 mode 永久出局;得到 %v", got)
	}
	if got.Sub(now) > time.Hour {
		t.Errorf("兜底应当保守(便于尽快重新探测),得到 %v", got.Sub(now))
	}
}

// 上游给了明确的恢复时间就用它 —— 兜底只在缺失时接管,不能覆盖真实信号。
func TestResolveQuotaResetAt_ExplicitValueWins(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	explicit := now.Add(3 * time.Hour)
	if got := resolveQuotaResetAt(&explicit, now); !got.Equal(explicit) {
		t.Errorf("应沿用上游给出的恢复时间 %v,得到 %v", explicit, got)
	}
}

// 已经过期的恢复时间等同于没有:直接采用会写出一个立刻就该恢复、却可能被恢复
// 扫描当成陈旧数据的窗口。同样走兜底。
func TestResolveQuotaResetAt_PastValueFallsBack(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-time.Hour)
	got := resolveQuotaResetAt(&stale, now)
	if !got.After(now) {
		t.Errorf("过期的恢复时间应走兜底,得到 %v", got)
	}
}
