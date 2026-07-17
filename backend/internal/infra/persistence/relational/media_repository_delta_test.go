package relational

import (
	"context"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// deltaWitnessFixture 建一个可用的账号 + 客户端密钥,供本文件的见证测试复用。
func deltaWitnessFixture(t *testing.T, database *Database, name string) (uint64, uint64) {
	t.Helper()
	ctx := context.Background()

	accountValue, _, err := NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider:             accountdomain.ProviderWeb,
		AuthType:             accountdomain.AuthTypeSSO,
		WebTier:              accountdomain.WebTierBasic,
		Name:                 name + "-account",
		SourceKey:            name + "-account",
		EncryptedAccessToken: testEncryptedToken,
		AuthStatus:           accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{
		Name: name + "-key", Prefix: name + "-key", SecretHash: testSecretHash,
		EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4,
	}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	return accountValue.ID, key.ID
}

// gorm 的 Select(...) 是**字符串白名单**:漏列一个字段不报编译错、不报运行时错,
// 只会静默丢掉那次更新。UpdateMediaJob 的白名单曾漏掉 post_id,导致视频任务的
// PostID 永远存不进去 —— 而 PostID 是视频拓展的全部前提(拓展要靠它把
// extendPostId/originalPostId/parentPostId 指回源视频)。
//
// 这条测试是那次修复的见证人。上游把白名单里的 post_id 换成了 result_asset_id,
// 合并时取任何一侧都会重新引入同一个 bug,而**编译器和其它测试全都不会响**。
// 正解是取并集。
func TestUpdateMediaJob_PersistsPostID(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accountID, keyID := deltaWitnessFixture(t, database, "post-id")
	repo := NewMediaJobRepository(database)

	job := testMediaJob("video_post_id_witness", accountID, keyID, mediadomain.StatusQueued, time.Now().UTC())
	if err := repo.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	// 模拟视频轮询完成后的回写。
	const wantPostID = "80c3dcb2-da24-49df-9bbb-dfea4580172d"
	const wantURL = "https://assets.grok.com/users/x/generated/y/content"
	job.Status = mediadomain.StatusCompleted
	job.PostID = wantPostID
	job.UpstreamURL = wantURL
	job.UpdatedAt = time.Now().UTC()
	if err := repo.UpdateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	stored, err := repo.GetMediaJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PostID != wantPostID {
		t.Fatalf("PostID 没落库: got %q want %q —— 检查 UpdateMediaJob 的 Select 白名单是否还列着 post_id", stored.PostID, wantPostID)
	}
	if stored.UpstreamURL != wantURL {
		t.Errorf("UpstreamURL 也丢了(同一个白名单): got %q", stored.UpstreamURL)
	}
}

// ListMediaJobs 有第二份独立的字段白名单(gorm 在查询侧同样按白名单取列)。
// 它曾漏掉 upstream_url 和 post_id,症状是视频库页面每个任务的链接和拓展入口
// 都空着,而任务本身明明是 completed。
func TestListMediaJobs_ReturnsPostIDAndUpstreamURL(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	accountID, keyID := deltaWitnessFixture(t, database, "list-post-id")
	repo := NewMediaJobRepository(database)

	const wantPostID = "e4a5835b-e82e-404e-9bf1-63cbe9db404e"
	const wantURL = "https://aitoken.bigopen.cn:8443/v1/media/videos/vid_witness"
	job := testMediaJob("video_list_witness", accountID, keyID, mediadomain.StatusCompleted, time.Now().UTC())
	job.PostID = wantPostID
	job.UpstreamURL = wantURL
	if err := repo.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	values, _, err := repo.ListMediaJobs(ctx, repository.MediaJobListQuery{
		Page: repository.PageQuery{Offset: 0, Limit: 50},
	})
	if err != nil {
		t.Fatal(err)
	}
	var found *mediadomain.Job
	for i := range values {
		if values[i].ID == "video_list_witness" {
			found = &values[i]
			break
		}
	}
	if found == nil {
		t.Fatal("列表里找不到刚建的任务")
	}
	if found.PostID != wantPostID {
		t.Errorf("列表丢了 PostID: got %q want %q —— 检查 ListMediaJobs 的 Select 白名单", found.PostID, wantPostID)
	}
	if found.UpstreamURL != wantURL {
		t.Errorf("列表丢了 UpstreamURL: got %q want %q", found.UpstreamURL, wantURL)
	}
}
