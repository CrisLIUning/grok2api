package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// extensionSourceRepository 按 id / post_id 两种键返回同一条源任务,用来验证
// 两条续拍入口最终解析出**相同**的归属账号。
type extensionSourceRepository struct {
	byID     map[string]media.Job
	byPostID map[string]media.Job
}

func (r *extensionSourceRepository) CreateMediaJob(context.Context, media.Job) error { return nil }

func (r *extensionSourceRepository) GetMediaJob(context.Context, string, uint64) (media.Job, error) {
	return media.Job{}, errors.New("not implemented")
}

func (r *extensionSourceRepository) GetMediaJobByID(_ context.Context, id string) (media.Job, error) {
	if job, ok := r.byID[id]; ok {
		return job, nil
	}
	return media.Job{}, errors.New("not found")
}

func (r *extensionSourceRepository) GetMediaJobByPostID(_ context.Context, postID string) (media.Job, error) {
	if job, ok := r.byPostID[postID]; ok {
		return job, nil
	}
	return media.Job{}, errors.New("not found")
}

func (r *extensionSourceRepository) UpdateMediaJob(context.Context, media.Job) error { return nil }

func (r *extensionSourceRepository) ListMediaJobs(context.Context, repository.MediaJobListQuery) ([]media.Job, int64, error) {
	return nil, 0, nil
}

func (r *extensionSourceRepository) SummarizeMediaJobs(context.Context) (repository.MediaJobStats, error) {
	return repository.MediaJobStats{}, nil
}

func (r *extensionSourceRepository) ListRecoverableMediaJobs(context.Context, int) ([]media.Job, error) {
	return nil, nil
}

func (r *extensionSourceRepository) ListUnrecordedTerminalMediaJobs(context.Context, int) ([]media.Job, error) {
	return nil, nil
}

func (r *extensionSourceRepository) TryClaimMediaJob(context.Context, string, time.Time, time.Time, string) (media.Job, bool, error) {
	return media.Job{}, false, nil
}

func (r *extensionSourceRepository) MarkMediaJobUsageRecorded(context.Context, string, time.Time) error {
	return nil
}

func newExtensionSourceService() *Service {
	source := media.Job{
		ID: "video_src", RequestID: "req-src", ClientKeyID: 1, AccountID: 77, AccountName: "owner",
		Provider: "grok_web", Model: "grok-imagine-video", Status: media.StatusCompleted,
		PostID: "post_abc", InputJSON: `{"image_urls":["https://example.test/a.png"]}`,
	}
	return &Service{mediaJobs: &extensionSourceRepository{
		byID:     map[string]media.Job{source.ID: source},
		byPostID: map[string]media.Job{source.PostID: source},
	}}
}

// 已经跑通的那条路:给我们自己的 request_id,反查出 postId 与归属账号。
func TestResolveVideoExtensionSource_ByRequestID(t *testing.T) {
	got, err := newExtensionSourceService().resolveVideoExtensionSource(context.Background(), VideoInput{
		Operation: "extension", SourceRequestID: "video_src",
	})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.PostID != "post_abc" || got.AccountID != 77 {
		t.Fatalf("postID=%q accountID=%d,期望 post_abc / 77", got.PostID, got.AccountID)
	}
	if len(got.ReferenceURLs) != 1 {
		t.Errorf("源任务的参考图应一并带出,得到 %v", got.ReferenceURLs)
	}
}

// 这是本次要补的缺口。
//
// 此前 source_post_id 分支只设了 extendPostID,pinnedAccountID 留 0,于是任务
// 落到随机取号 —— 而 extendPostId 归创建它的账号所有,换个账号的会话解析不了,
// 结果是稳定的 invalid-parent-post(403)。代码注释自己写着「须属被选账号」,
// 但账号是我们随机选的,调用方根本无从保证。
//
// media_jobs 里本来就同时存着 PostID 与 AccountID,反查一下即可 —— 两条入口
// 应当解析出完全相同的归属账号。
func TestResolveVideoExtensionSource_ByBarePostIDResolvesSameAccount(t *testing.T) {
	service := newExtensionSourceService()
	byRequest, err := service.resolveVideoExtensionSource(context.Background(), VideoInput{
		Operation: "extension", SourceRequestID: "video_src",
	})
	if err != nil {
		t.Fatalf("按 request_id 解析失败: %v", err)
	}
	byPost, err := service.resolveVideoExtensionSource(context.Background(), VideoInput{
		Operation: "extension", SourcePostID: "post_abc",
	})
	if err != nil {
		t.Fatalf("按 post_id 解析失败: %v", err)
	}
	if byPost.AccountID != byRequest.AccountID {
		t.Errorf("两条入口必须解析出同一个归属账号;post_id=%d request_id=%d", byPost.AccountID, byRequest.AccountID)
	}
	if byPost.PostID != "post_abc" {
		t.Errorf("postID = %q", byPost.PostID)
	}
}

// 号池里查不到的 postId 无法确定归属账号,续拍必然失败。早点如实拒绝,好过
// 随机挑个账号再收一个语焉不详的 403。
func TestResolveVideoExtensionSource_UnknownPostIDIsRejected(t *testing.T) {
	_, err := newExtensionSourceService().resolveVideoExtensionSource(context.Background(), VideoInput{
		Operation: "extension", SourcePostID: "post_not_ours",
	})
	if !errors.Is(err, ErrExtensionSourceNotFound) {
		t.Errorf("未知 postId 应返回 ErrExtensionSourceNotFound,得到 %v", err)
	}
}

func TestResolveVideoExtensionSource_RequiresASource(t *testing.T) {
	if _, err := newExtensionSourceService().resolveVideoExtensionSource(context.Background(), VideoInput{
		Operation: "extension",
	}); err == nil {
		t.Error("既没给 source_request_id 也没给 source_post_id 时必须报错")
	}
}

// 非续拍不做任何来源解析,也不该因此报错。
func TestResolveVideoExtensionSource_NonExtensionIsNoop(t *testing.T) {
	got, err := newExtensionSourceService().resolveVideoExtensionSource(context.Background(), VideoInput{})
	if err != nil {
		t.Fatalf("首发生成不应解析来源: %v", err)
	}
	if got.PostID != "" || got.AccountID != 0 {
		t.Errorf("首发不应产生 pin;得到 %#v", got)
	}
}
