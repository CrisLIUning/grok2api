package account

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// recordingQuotaQueue 只关心"恢复事件到底有没有被排上"。
type recordingQuotaQueue struct {
	scheduled []accountdomain.QuotaRecoveryEvent
}

func (q *recordingQuotaQueue) ScheduleQuotaRecovery(_ context.Context, value accountdomain.QuotaRecoveryEvent) error {
	q.scheduled = append(q.scheduled, value)
	return nil
}

func (q *recordingQuotaQueue) EnsureQuotaRecovery(_ context.Context, value accountdomain.QuotaRecoveryEvent) error {
	q.scheduled = append(q.scheduled, value)
	return nil
}

func (q *recordingQuotaQueue) ClaimDueQuotaRecoveries(context.Context, time.Time, int, time.Duration) ([]accountdomain.QuotaRecoveryEvent, error) {
	return nil, nil
}

func (q *recordingQuotaQueue) AckQuotaRecovery(context.Context, accountdomain.QuotaRecoveryEvent) error {
	return nil
}

func (q *recordingQuotaQueue) RescheduleQuotaRecovery(context.Context, accountdomain.QuotaRecoveryEvent) error {
	return nil
}

// 这个测试盯的是**接线**,不是辅助函数。
//
// resolveQuotaResetAt 自己有单元测试,但那种测试有个致命盲区:把 ExhaustQuota
// 里的调用改回原样(写 nil、并用 `if resetAt != nil` 守卫掉排程),整套测试依然
// 全绿 —— 复核实测确认过。也就是说,真正会导致账号永久出局的那行代码,是没有
// 任何东西守着的。
//
// 原代码:
//
//	s.accounts.ExhaustQuotaWindow(ctx, id, mode, resetAt /* 可能是 nil */, s.now())
//	if resetAt != nil && s.quotaQueue != nil {
//		return s.quotaQueue.ScheduleQuotaRecovery(...)
//	}
//
// 两头一起失守:窗口以 reset_at=NULL 落库,恢复调度被跳过。恢复扫描过滤
// reset_at IS NOT NULL,选号又排除 Remaining<=0 的账号 —— 该账号对这个 mode 永久
// 出局,没有任何东西会再把它捞回来。
//
// 所以这里断言的是可观测的外部行为:拿不到恢复时间时,**恢复事件仍然必须被排上**。
func TestExhaustQuotaAlwaysSchedulesRecoveryWithoutAnUpstreamResetTime(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encryptedToken, err := cipher.Encrypt("test-token")
	if err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(database)
	created, _, err := accounts.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		Name: "exhaustion-account", SourceKey: "exhaustion-source",
		EncryptedAccessToken: encryptedToken, Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}

	queue := &recordingQuotaQueue{}
	service := NewService(accounts, nil, nil, nil, provider.NewRegistry(), cipher, memory.NewLockStore())
	service.SetQuotaRecoveryQueue(queue)

	before := time.Now().UTC()
	// resetAt 为 nil:上游没给 Retry-After,该 mode 也没有可参考的既有窗口。
	if err := service.ExhaustQuota(ctx, created.ID, "fast", nil); err != nil {
		t.Fatalf("ExhaustQuota: %v", err)
	}

	if len(queue.scheduled) == 0 {
		t.Fatal("拿不到恢复时间时仍必须排上恢复事件 —— 否则该账号对这个 mode 永久出局:" +
			"窗口以 reset_at=NULL 落库,恢复扫描过滤 reset_at IS NOT NULL,选号又排除 Remaining<=0")
	}
	event := queue.scheduled[0]
	if event.AccountID != created.ID || event.Mode != "fast" {
		t.Errorf("排上的事件对不上:accountID=%d mode=%q", event.AccountID, event.Mode)
	}
	if !event.DueAt.After(before) {
		t.Errorf("恢复时间必须在未来,否则等于没排;得到 %v(now=%v)", event.DueAt, before)
	}
	if event.DueAt.Sub(before) > time.Hour {
		t.Errorf("兜底应当保守,便于尽快重新探测;得到 %v", event.DueAt.Sub(before))
	}
}
