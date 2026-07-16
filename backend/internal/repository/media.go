package repository

import (
	"context"
	"io"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

// MediaAssetListQuery 表示管理端媒体资源列表的查询条件。
type MediaAssetListQuery struct {
	Page PageQuery
}

// MediaJobListFilter 表示视频任务列表允许使用的业务筛选条件。
type MediaJobListFilter struct {
	Status string
}

// MediaJobListQuery 表示管理端视频任务列表的查询条件。
type MediaJobListQuery struct {
	Page   PageQuery
	Filter MediaJobListFilter
}

// MediaAssetStats 表示媒体资源的聚合统计结果。
type MediaAssetStats struct {
	TotalImages int64
	TotalBytes  int64
}

// MediaJobStats 表示各状态视频任务的聚合统计结果。
type MediaJobStats struct {
	TotalJobs  int64
	Completed  int64
	Failed     int64
	InProgress int64
	Queued     int64
}

type MediaJobRepository interface {
	CreateMediaJob(ctx context.Context, value media.Job) error
	GetMediaJob(ctx context.Context, id string, clientKeyID uint64) (media.Job, error)
	GetMediaJobByID(ctx context.Context, id string) (media.Job, error)
	UpdateMediaJob(ctx context.Context, value media.Job) error
	ListMediaJobs(ctx context.Context, query MediaJobListQuery) ([]media.Job, int64, error)
	SummarizeMediaJobs(ctx context.Context) (MediaJobStats, error)
	ListRecoverableMediaJobs(ctx context.Context, limit int) ([]media.Job, error)
	ListUnrecordedTerminalMediaJobs(ctx context.Context, limit int) ([]media.Job, error)
	TryClaimMediaJob(ctx context.Context, id string, now, leaseUntil time.Time, claimToken string) (media.Job, bool, error)
	MarkMediaJobUsageRecorded(ctx context.Context, id string, recordedAt time.Time) error
}

// MediaAssetRepository 定义媒体资源元数据持久化能力。
type MediaAssetRepository interface {
	CreateMediaAsset(ctx context.Context, value media.Asset) error
	GetMediaAsset(ctx context.Context, id string) (media.Asset, error)
	ListMediaAssets(ctx context.Context, query MediaAssetListQuery) ([]media.Asset, int64, error)
	SummarizeMediaAssets(ctx context.Context) (MediaAssetStats, error)
	TotalMediaAssetBytes(ctx context.Context) (int64, error)
	ListOldestMediaAssets(ctx context.Context, limit int) ([]media.Asset, error)
	DeleteMediaAsset(ctx context.Context, id string) error
}

// MediaObjectStorage 定义媒体二进制对象的存取边界。
type MediaObjectStorage interface {
	SaveImage(ctx context.Context, id, mimeType string, data []byte) (string, error)
	// SaveVideo 以流式方式落盘视频,超过 maxBytes 视为失败;返回存储键与实际字节数。
	SaveVideo(ctx context.Context, id string, r io.Reader, maxBytes int64) (storageKey string, size int64, err error)
	Open(ctx context.Context, storageKey string) (io.ReadCloser, error)
	// OpenSeek 返回可随机寻址的读取器,供 HTTP Range 请求使用。
	OpenSeek(ctx context.Context, storageKey string) (io.ReadSeekCloser, error)
	Delete(ctx context.Context, storageKey string) error
	// PruneVideos 在重服视频目录总大小超过 maxBytes 时按最旧优先删除到上限内,返回删除数。
	// 视频重服无数据库行,靠此按容量兜底,防止磁盘无限增长拖垮整个媒体存储。
	PruneVideos(ctx context.Context, maxBytes int64) (int, error)
}
