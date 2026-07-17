package inference

import (
	"testing"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
)

// 视频完成后的响应体里,request_id 和 post_id 是**前端发起拓展的唯一数据源**:
// 拓展要么按 source_request_id(我们自己的任务 ID)、要么按 source_post_id
// (grok 的 videoPostId)指回源视频,两者都从这里拿。
//
// 这两个字段是我们加的。删掉它们 `go build` 依然 exit 0、其余测试依然 PASS,
// 只有前端的"拓展"按钮会静默消失 —— 所以必须有断言钉住。
func TestVideoGenerationResponse_CompletedExposesExtensionHandles(t *testing.T) {
	const wantPostID = "80c3dcb2-da24-49df-9bbb-dfea4580172d"
	body := videoGenerationResponse(mediadomain.Job{
		ID:          "video_witness",
		Status:      mediadomain.StatusCompleted,
		Model:       "grok-imagine-video",
		PostID:      wantPostID,
		UpstreamURL: "https://aitoken.bigopen.cn:8443/v1/media/videos/vid_x",
		Seconds:     6,
	})

	if got := body["request_id"]; got != "video_witness" {
		t.Errorf("request_id = %v,拓展要用它作 source_request_id", got)
	}
	if got := body["post_id"]; got != wantPostID {
		t.Errorf("post_id = %v want %q,拓展要用它作 source_post_id", got, wantPostID)
	}
}

// 未完成的任务不该泄漏 post_id:它此刻要么为空、要么还不稳定,
// 前端据此渲染拓展入口会给出一个必然失败的按钮。
func TestVideoGenerationResponse_PendingHasNoExtensionHandles(t *testing.T) {
	body := videoGenerationResponse(mediadomain.Job{
		ID: "video_pending", Status: mediadomain.StatusQueued, Model: "grok-imagine-video", Progress: 37,
	})

	if _, exists := body["post_id"]; exists {
		t.Error("pending 任务不该带 post_id")
	}
	if body["status"] != "pending" {
		t.Errorf("status = %v want pending", body["status"])
	}
}
