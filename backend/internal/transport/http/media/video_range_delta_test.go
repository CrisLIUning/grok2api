package media

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func newVideoRangeTestRouter(t *testing.T) (*gin.Engine, string, []byte) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx := context.Background()

	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "video-range.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	service := mediaapp.NewService(
		relational.NewMediaAssetRepository(database), relational.NewMediaJobRepository(database), objects, nil,
		mediaapp.Config{
			PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30,
			CleanupThresholdPercent: 80, CleanupInterval: 10 * time.Minute,
		})

	// 内容不重要,只要够长、可按字节区间校验。
	raw := bytes.Repeat([]byte("0123456789abcdef"), 64) // 1024 bytes
	id, err := service.SaveVideo(ctx, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}

	router := gin.New()
	NewHandler(service).RegisterPublic(router)
	return router, "/v1/media/videos/" + id, raw
}

// 视频拓展的"选帧"要求前端能 seek:creative-console 读 <video>.currentTime
// 决定从第几秒续拓,gallery 预览也要拖进度条。浏览器的 seek 依赖服务端支持
// HTTP Range —— 没有 206 就只能从头下完整个文件,进度条直接失效。
//
// 我们用 http.ServeContent + 可寻址的 OpenVideo(io.ReadSeekCloser)实现。
// 上游的对应实现是 io.Copy + no-store、**无 Range 无 206**,而且它们根本没有
// 视频 GET 路由 —— 合并时若采纳上游的 OpenVideo(io.ReadCloser),这里会静默
// 退化成整段下载,选帧拓展报废。
func TestPublicVideo_SupportsRangeRequests(t *testing.T) {
	router, path, raw := newVideoRangeTestRouter(t)

	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Range", "bytes=100-199")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPartialContent {
		t.Fatalf("Range 请求返回 %d,want 206 —— 检查 getVideo 是否还在用 http.ServeContent + 可寻址 body", recorder.Code)
	}
	if got := recorder.Body.Bytes(); !bytes.Equal(got, raw[100:200]) {
		t.Errorf("206 返回了错误的字节区间(len=%d)", len(got))
	}
	if got := recorder.Header().Get("Content-Range"); !strings.HasPrefix(got, "bytes 100-199/") {
		t.Errorf("Content-Range = %q", got)
	}
}

// 完整 GET 仍要正常,且必须声明 Accept-Ranges —— 浏览器据此才认为可以 seek。
func TestPublicVideo_FullGetAdvertisesRangeSupport(t *testing.T) {
	router, path, raw := newVideoRangeTestRouter(t)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("GET status = %d", recorder.Code)
	}
	if recorder.Body.Len() != len(raw) {
		t.Errorf("body len = %d want %d", recorder.Body.Len(), len(raw))
	}
	if got := recorder.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges = %q want \"bytes\" —— 浏览器据此判断能否 seek", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "video/mp4" {
		t.Errorf("Content-Type = %q", got)
	}
}
