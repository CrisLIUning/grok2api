package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// LocalStore 将媒体对象限制在单一根目录内，并使用临时文件与原子硬链接完成提交。
type LocalStore struct {
	root            string
	removeTemporary func(string) error
}

func NewLocalStore(root string) (*LocalStore, error) {
	absolute, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil || strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("媒体存储目录无效")
	}
	absolute = filepath.Clean(absolute)
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("创建媒体存储目录: %w", err)
	}
	return &LocalStore{root: absolute, removeTemporary: os.Remove}, nil
}

func (s *LocalStore) SaveImage(ctx context.Context, id, mimeType string, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	extension, ok := imageExtension(mimeType)
	if !ok || len(id) < 2 {
		return "", fmt.Errorf("图片存储参数无效")
	}
	storageKey := filepath.ToSlash(filepath.Join("images", id[:2], id+extension))
	path, err := s.resolve(storageKey)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("创建图片目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".image-*")
	if err != nil {
		return "", fmt.Errorf("创建图片临时文件: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanupPending := true
	defer func() {
		_ = temporary.Close()
		if cleanupPending {
			if cleanupErr := s.removeTemporary(temporaryPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				slog.Warn("media_temp_cleanup_failed", "path", temporaryPath, "error", cleanupErr)
			}
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		return "", fmt.Errorf("写入图片: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return "", fmt.Errorf("同步图片文件: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("关闭图片文件: %w", err)
	}
	// 硬链接提交具有 no-replace 语义，极端 ID 冲突时不会覆盖已有图片。
	if err := os.Link(temporaryPath, path); err != nil {
		return "", fmt.Errorf("提交图片文件: %w", err)
	}
	// 提交已经成功，清理失败不能回滚永久文件；defer 会再重试一次并记录持续失败。
	cleanupErr := s.removeTemporary(temporaryPath)
	cleanupPending = cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist)
	return storageKey, nil
}

// SaveVideo 以流式方式把视频写入 videos/{id[:2]}/{id}.mp4,使用临时文件 + 原子硬链接提交;
// 超过 maxBytes 立即失败并清理半成品。返回存储键与实际字节数。
func (s *LocalStore) SaveVideo(ctx context.Context, id string, r io.Reader, maxBytes int64) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	if len(id) < 2 {
		return "", 0, fmt.Errorf("视频存储参数无效")
	}
	storageKey := filepath.ToSlash(filepath.Join("videos", id[:2], id+".mp4"))
	path, err := s.resolve(storageKey)
	if err != nil {
		return "", 0, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", 0, fmt.Errorf("创建视频目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".video-*")
	if err != nil {
		return "", 0, fmt.Errorf("创建视频临时文件: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanupPending := true
	defer func() {
		_ = temporary.Close()
		if cleanupPending {
			if cleanupErr := s.removeTemporary(temporaryPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				slog.Warn("media_temp_cleanup_failed", "path", temporaryPath, "error", cleanupErr)
			}
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return "", 0, err
	}
	// 多读 1 字节以检测是否超限。
	written, err := io.Copy(temporary, io.LimitReader(r, maxBytes+1))
	if err != nil {
		return "", 0, fmt.Errorf("写入视频: %w", err)
	}
	if written > maxBytes {
		return "", 0, fmt.Errorf("视频超过 %d MiB 上限", maxBytes>>20)
	}
	if written == 0 {
		return "", 0, fmt.Errorf("视频内容为空")
	}
	if err := temporary.Sync(); err != nil {
		return "", 0, fmt.Errorf("同步视频文件: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", 0, fmt.Errorf("关闭视频文件: %w", err)
	}
	if err := os.Link(temporaryPath, path); err != nil {
		// 极端 ID 冲突时不覆盖已有文件;冲突即视为已存在,直接复用。
		if errors.Is(err, os.ErrExist) {
			cleanupErr := s.removeTemporary(temporaryPath)
			cleanupPending = cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist)
			return storageKey, written, nil
		}
		return "", 0, fmt.Errorf("提交视频文件: %w", err)
	}
	cleanupErr := s.removeTemporary(temporaryPath)
	cleanupPending = cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist)
	return storageKey, written, nil
}

func (s *LocalStore) Open(ctx context.Context, storageKey string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.resolve(storageKey)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("打开媒体文件: %w", err)
	}
	return file, nil
}

// OpenSeek 打开媒体文件并返回可寻址读取器(*os.File),供 HTTP Range 请求使用。
func (s *LocalStore) OpenSeek(ctx context.Context, storageKey string) (io.ReadSeekCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.resolve(storageKey)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("打开媒体文件: %w", err)
	}
	return file, nil
}

func (s *LocalStore) Delete(ctx context.Context, storageKey string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.resolve(storageKey)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return fmt.Errorf("删除媒体文件: %w", err)
	}
	return nil
}

// PruneVideos 在视频目录总大小超过 maxBytes 时,按修改时间从旧到新删除到上限内。
// 视频重服无数据库行(存储键由 ID 确定性推导),靠此按容量兜底,避免磁盘无限增长。
func (s *LocalStore) PruneVideos(ctx context.Context, maxBytes int64) (int, error) {
	if maxBytes <= 0 {
		return 0, nil
	}
	root := filepath.Join(s.root, "videos")
	type entry struct {
		path string
		size int64
		mod  time.Time
	}
	var entries []entry
	var total int64
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		entries = append(entries, entry{path: path, size: info.Size(), mod: info.ModTime()})
		total += info.Size()
		return nil
	})
	if walkErr != nil {
		if errors.Is(walkErr, os.ErrNotExist) {
			return 0, nil
		}
		return 0, walkErr
	}
	if total <= maxBytes {
		return 0, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod.Before(entries[j].mod) })
	deleted := 0
	for _, e := range entries {
		if total <= maxBytes {
			break
		}
		if err := os.Remove(e.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			continue
		}
		total -= e.size
		deleted++
	}
	return deleted, nil
}

func (s *LocalStore) resolve(storageKey string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(strings.TrimSpace(storageKey)))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("媒体存储路径无效")
	}
	full := filepath.Join(s.root, clean)
	relative, err := filepath.Rel(s.root, full)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("媒体存储路径越界")
	}
	return full, nil
}

func imageExtension(mimeType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/jpeg":
		return ".jpg", true
	case "image/png":
		return ".png", true
	case "image/webp":
		return ".webp", true
	case "image/gif":
		return ".gif", true
	default:
		return "", false
	}
}
