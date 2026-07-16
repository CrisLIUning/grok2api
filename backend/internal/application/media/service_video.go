package media

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// maxVideoAssetBytes 是单个重服视频的落盘上限;grok imagine 视频为短片,512 MiB 足够宽裕。
	maxVideoAssetBytes = int64(512) << 20
	// maxVideoStoreBytes 是重服视频目录的总容量上限;清理周期内超过则按最旧优先删除。
	// 视频无数据库行,靠此按容量兜底,避免磁盘无限增长拖垮整个媒体存储。
	maxVideoStoreBytes = int64(5) << 30 // 5 GiB
	videoAssetIDPrefix = "vid_"
)

func newVideoAssetID() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("生成视频资源 ID: %w", err)
	}
	return videoAssetIDPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// SaveVideo 以流式方式落盘视频并返回不可猜测的资源 ID。存储键由 ID 确定性推导
// (videos/{id[:2]}/{id}.mp4),因此公开重服无需数据库行即可按 ID 定位文件。
func (s *Service) SaveVideo(ctx context.Context, r io.Reader) (string, error) {
	id, err := newVideoAssetID()
	if err != nil {
		return "", err
	}
	if _, _, err := s.objects.SaveVideo(ctx, id, r, maxVideoAssetBytes); err != nil {
		return "", err
	}
	return id, nil
}

// PublicVideoURL 返回可直接播放的视频公开地址。
func (s *Service) PublicVideoURL(id string) string {
	return s.runtimeConfig().PublicBaseURL + "/v1/media/videos/" + id
}

// OpenVideo 按资源 ID 打开视频文件(可寻址,供 HTTP Range)。ID 非法或文件缺失返回 ErrAssetNotFound。
func (s *Service) OpenVideo(ctx context.Context, id string) (io.ReadSeekCloser, error) {
	id = strings.TrimSpace(id)
	if !validVideoAssetID(id) {
		return nil, ErrAssetNotFound
	}
	storageKey := "videos/" + id[:2] + "/" + id + ".mp4"
	body, err := s.objects.OpenSeek(ctx, storageKey)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	return body, nil
}

// validVideoAssetID 只接受 vid_ 前缀、限定字符集的 ID,杜绝路径穿越。
func validVideoAssetID(id string) bool {
	if !strings.HasPrefix(id, videoAssetIDPrefix) || len(id) < 6 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
