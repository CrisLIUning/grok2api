package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type videoUpstreamError struct {
	status int
	body   string
}

func (e *videoUpstreamError) Error() string {
	return fmt.Sprintf("视频上游返回 %d: %s", e.status, e.body)
}

func (e *videoUpstreamError) HTTPStatusCode() int { return e.status }

func (a *Adapter) GenerateVideo(ctx context.Context, request provider.VideoRequest) (provider.VideoResult, error) {
	cfg := a.config()
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return provider.VideoResult{}, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, request.Credential)
	if err != nil {
		return provider.VideoResult{}, err
	}
	defer lease.Release()
	segments := videoSegments(request.Duration)
	if len(segments) == 0 {
		return provider.VideoResult{}, fmt.Errorf("duration 必须在 1 到 15 秒之间")
	}
	ratio := resolveAspectRatio(request.AspectRatio)
	resolution := request.Resolution
	if resolution == "" {
		resolution = "720p"
	}
	var payload map[string]any
	if request.Operation == "extension" {
		// 拓展 grok 已生成的视频:extendPostId=originalPostId=parentPostId=源 videoPostId。
		// 若源是图生视频,必须重新上传原参考图并在顶层带 fileAttachments=[fileId],否则 grok
		// 找不到根节点报 invalid-parent-post(与抓包一致)。文生视频拓展则无 fileAttachments。
		if strings.TrimSpace(request.ExtendPostID) == "" {
			return provider.VideoResult{}, fmt.Errorf("视频拓展缺少来源 postId")
		}
		videoLength := request.VideoLength
		if videoLength <= 0 {
			videoLength = segments[0]
		}
		fileAttachments := make([]string, 0, len(request.ReferenceURLs))
		for _, rawReference := range request.ReferenceURLs {
			uploaded, referenceErr := a.prepareVideoReference(ctx, cfg, lease, token, rawReference)
			if referenceErr != nil {
				return provider.VideoResult{}, wrapUnsubmittedVideoPreflight(referenceErr)
			}
			if uploaded.ID != "" {
				fileAttachments = append(fileAttachments, uploaded.ID)
			}
		}
		payload = videoExtensionPayload(request.Prompt, request.ExtendPostID, ratio, resolution, videoLength, request.VideoExtensionStartTime, fileAttachments)
	} else {
		uploads := make([]uploadedFile, 0, len(request.ReferenceURLs))
		for _, rawReference := range request.ReferenceURLs {
			uploaded, referenceErr := a.prepareVideoReference(ctx, cfg, lease, token, rawReference)
			if referenceErr != nil {
				return provider.VideoResult{}, wrapUnsubmittedVideoPreflight(referenceErr)
			}
			uploads = append(uploads, uploaded)
		}
		parentID := ""
		if len(uploads) == 0 {
			// 文生视频:建 video 种子 post 作 parentPostId。图生视频用 fileAttachments+rootPostId,不建 post。
			parentID, err = a.createMediaPost(ctx, cfg, lease, token, "MEDIA_POST_TYPE_VIDEO", "", request.Prompt)
			if err != nil {
				return provider.VideoResult{}, wrapUnsubmittedVideoPreflight(err)
			}
		}
		payload = videoCreatePayload(request.Prompt, parentID, ratio, resolution, segments[0], uploads)
	}
	response, err := a.postJSON(ctx, cfg, lease, token, cfg.BaseURL+"/rest/app-chat/conversations/new", payload, time.Duration(cfg.VideoTimeoutSeconds)*time.Second)
	if err != nil {
		// postJSON 已对传输错误/403 做过出口反馈;这里只原样上抛。
		return provider.VideoResult{}, err
	}
	result, postID, parseErr := parseVideoStream(response, request.Progress)
	_ = response.Body.Close()
	if parseErr != nil {
		// 只对已识别的上游语义(429/403)或真传输/解析故障回写出口。
		// 内容策略等无状态码业务错误绝不能 Feedback:classifyEgressBlame 会把裸
		// fmt.Errorf 默认成 transport,冷却节点 —— 与 Imagine 事故同一类误伤。
		// 可证明未提交的 429/503 是上游应用层拒绝,也不能罚出口。
		if shouldFeedbackVideoParseError(parseErr) {
			a.feedbackUpstreamError(ctx, lease.NodeID, parseErr)
		}
		return provider.VideoResult{}, parseErr
	}
	if result.URL == "" {
		return provider.VideoResult{}, fmt.Errorf("视频生成完成但没有返回内容 URL")
	}
	result.PostID = postID
	// Goal A:grok 返回的是需 SSO Cookie 才能访问的私有 CDN 地址(assets.grok.com),
	// 直接返回会让下游 403。用同一出口下载视频并重服为本地公开地址;下载失败则降级
	// 保留裸 URL(仍留下 PostID,便于续拓与排查)。
	if a.assets != nil {
		if assetID, downloadErr := a.downloadVideoToStore(ctx, request.Credential, result.URL); downloadErr == nil {
			result.URL = a.assets.PublicVideoURL(assetID)
		} else if a.logger != nil {
			a.logger.Warn("video_reserve_failed", "error", downloadErr, "upstream_url", result.URL)
		}
	}
	return result, nil
}

// prepareVideoReference 下载并上传一张参考图,返回上传结果(ID=fileId 供 fileAttachments,
// URI=fileUri 供 imageReferences)。
func (a *Adapter) prepareVideoReference(ctx context.Context, cfg Config, lease *egress.Lease, token, value string) (uploadedFile, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return uploadedFile{}, fmt.Errorf("视频参考图片 URL 不能为空")
	}
	image, err := a.loadChatImage(ctx, lease, value, 20<<20)
	if err != nil {
		return uploadedFile{}, err
	}
	uploaded, err := a.uploadFileLegacy(ctx, cfg, lease, token, image, cfg.BaseURL+"/imagine")
	if err != nil {
		return uploadedFile{}, err
	}
	if uploaded.URI == "" {
		return uploadedFile{}, fmt.Errorf("上传视频参考图片后未返回 fileUri")
	}
	return uploaded, nil
}

const videoServiceTemporarilyUnavailable = "Service temporarily unavailable. Please try again later."

func parseVideoStream(response *http.Response, progress func(int)) (provider.VideoResult, string, error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if response.StatusCode == http.StatusUnauthorized {
			return provider.VideoResult{}, "", provider.ErrUnauthorized
		}
		// 明确 429 是请求拒绝,conversations/new 尚未进入生成流 → 可证明未提交。
		// 普通 5xx 不能包装:无法排除已提交后响应链路失败。
		upstreamErr := &videoUpstreamError{status: response.StatusCode, body: strings.TrimSpace(string(body))}
		if response.StatusCode == http.StatusTooManyRequests {
			return provider.VideoResult{}, "", provider.NewUnsubmittedVideoError(http.StatusTooManyRequests, upstreamErr)
		}
		return provider.VideoResult{}, "", upstreamErr
	}
	var result provider.VideoResult
	var postID string
	// accepted 表示流里已经出现 progress/postId —— 上游可能已经收下任务。
	// 本地 job.Progress=1 是 gateway 写入的初始值,不参与这里的判定。
	// 分类仍保留 429/403 状态码;是否换号由 UnsubmittedVideoError 类型证明。
	var accepted bool
	handle := func(root map[string]any) (bool, error) {
		// 同帧若同时有 progress/postId 和 error,先标记 accepted 再分类,
		// 避免“已收下却被当成可换号”。
		stream := nestedMap(root, "result", "response", "streamingVideoGenerationResponse")
		if stream != nil {
			if value, ok := numberAsInt(stream["progress"]); ok && value > 0 {
				accepted = true
			}
			if value, _ := stream["videoPostId"].(string); value != "" {
				postID = value
				accepted = true
			} else if value, _ := stream["videoId"].(string); value != "" {
				postID = value
				accepted = true
			}
		}
		if errorValue, ok := root["error"].(map[string]any); ok {
			return false, classifyVideoStreamError(errorValue, accepted)
		}
		if errorValue := nestedMap(root, "result", "response", "error"); errorValue != nil {
			return false, classifyVideoStreamError(errorValue, accepted)
		}
		if stream != nil {
			if value, ok := numberAsInt(stream["progress"]); ok {
				if progress != nil {
					progress(value)
				}
			}
			moderated, _ := stream["moderated"].(bool)
			if moderated {
				return false, nil
			}
			if setVideoResultURL(&result, firstString(stream, "videoUrl", "contentUrl", "contentURL", "assetUrl", "assetURL", "fileUri", "fileURL")) {
				return true, nil
			}
		}
		for _, attachment := range videoFileAttachments(root) {
			if setVideoResultURL(&result, attachment) {
				return true, nil
			}
		}
		return false, nil
	}

	reader := bufio.NewReader(response.Body)
	prefix, _ := reader.Peek(64)
	trimmedPrefix := strings.TrimSpace(string(prefix))
	var err error
	if strings.HasPrefix(trimmedPrefix, "data:") || strings.HasPrefix(trimmedPrefix, "event:") {
		err = consumeVideoSSE(reader, handle)
	} else {
		err = consumeVideoJSON(reader, handle)
	}
	if err != nil {
		return provider.VideoResult{}, "", err
	}
	return result, postID, nil
}

// classifyVideoStreamError 把 HTTP 200 流内的 error 帧保留成带状态码的错误。
//
// 词汇表复用 webResponseError(code=7 anti-bot / code=8 too many requests)。
// 未知内容策略错误不猜状态码,避免误触发换号。
//
// accepted=false 时,明确的限流/临时不可用包装为 UnsubmittedVideoError,
// 证明上游尚未收下任务,网关可安全换号;accepted=true 时只保留状态码语义,
// 绝不带 Unsubmitted 类型,避免重复生成。
func classifyVideoStreamError(errorValue map[string]any, accepted bool) error {
	err := webResponseError(errorValue)
	switch {
	case errors.Is(err, errWebUsageLimit):
		wrapped := &webUpstreamError{status: http.StatusTooManyRequests, err: err}
		if !accepted {
			return provider.NewUnsubmittedVideoError(http.StatusTooManyRequests, wrapped)
		}
		return wrapped
	case errors.Is(err, errWebAntiBot):
		return &webUpstreamError{status: http.StatusForbidden, err: err}
	default:
		msg, _ := errorValue["message"].(string)
		if strings.TrimSpace(msg) == "" {
			msg = "视频上游错误"
		}
		// 只认线上实测的精确文案,不做 "temporarily"/"try again" 模糊匹配,
		// 防止把内容策略或参数错误误判成可重试。
		if !accepted && strings.TrimSpace(msg) == videoServiceTemporarilyUnavailable {
			return provider.NewUnsubmittedVideoError(http.StatusServiceUnavailable, fmt.Errorf("视频上游错误: %s", msg))
		}
		return fmt.Errorf("视频上游错误: %s", msg)
	}
}

// wrapUnsubmittedVideoPreflight 把生成端点之前的明确 429/503 包装成可证明未提交。
// createMediaPost / 参考图上传发生在 conversations/new 之前,不可能重复生成视频。
// 图片路径共享 createMediaPost,因此包装只在 GenerateVideo 边界做。
func wrapUnsubmittedVideoPreflight(err error) error {
	if err == nil || provider.IsUnsubmittedVideoError(err) {
		return err
	}
	status, ok := provider.ErrorHTTPStatus(err)
	if !ok {
		return err
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return provider.NewUnsubmittedVideoError(status, err)
	default:
		return err
	}
}

// shouldFeedbackVideoParseError 决定 parseVideoStream 的失败要不要写出口健康。
//
// 有 HTTP 状态(含 429/403)或限流/反机器人语义 → 反馈。
// 可证明未提交的 429/503 是上游应用层拒绝 → 不反馈,避免误冷出口。
// classifyVideoStreamError 默认分支的"视频上游错误: ..."是无状态业务拒绝 → 不反馈,
// 否则 classifyEgressBlame 会把裸 fmt.Errorf 当成 transport 冷却节点。
// 其余(流读失败、JSON 解析失败等) → 反馈。
func shouldFeedbackVideoParseError(err error) bool {
	if err == nil {
		return false
	}
	// 可证明未提交的限流/临时不可用:节点把消息送达了,不该罚。
	if provider.IsUnsubmittedVideoError(err) {
		return false
	}
	if status, ok := provider.ErrorHTTPStatus(err); ok {
		// 429 是上游应用层限流,不是出口故障。
		if status == http.StatusTooManyRequests {
			return false
		}
		return true
	}
	if errors.Is(err, errWebUsageLimit) || errors.Is(err, errWebAntiBot) {
		return true
	}
	// 业务侧包装的无状态拒绝(内容策略等):不反馈。
	if strings.HasPrefix(err.Error(), "视频上游错误") {
		return false
	}
	return true
}

func consumeVideoSSE(reader io.Reader, handle func(map[string]any) (bool, error)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
		if line == "" || line == "[DONE]" || !strings.HasPrefix(line, "{") {
			continue
		}
		var root map[string]any
		if json.Unmarshal([]byte(line), &root) != nil {
			continue
		}
		complete, err := handle(root)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
	return scanner.Err()
}

func consumeVideoJSON(reader io.Reader, handle func(map[string]any) (bool, error)) error {
	decoder := json.NewDecoder(io.LimitReader(reader, 64<<20))
	for {
		var root map[string]any
		if err := decoder.Decode(&root); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("解析视频上游流: %w", err)
		}
		complete, err := handle(root)
		if err != nil {
			return err
		}
		if complete {
			return nil
		}
	}
}

func nestedMap(value map[string]any, keys ...string) map[string]any {
	current := value
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func videoSegments(seconds int) []int {
	if seconds < 1 || seconds > 15 {
		return nil
	}
	return []int{seconds}
}

// videoExtensionPayload 构造"拓展 grok 已生成视频"的 conversations/new 载荷。
// 按抓包:extendPostId=originalPostId=parentPostId=源 videoPostId,mode=custom,
// videoExtensionStartTime 为起始帧秒数,不带 fileAttachments/rootPostId(grok 自行追溯根)。
func videoExtensionPayload(prompt, extendPostID, ratio, resolution string, videoLength int, startTime float64, fileAttachments []string) map[string]any {
	config := map[string]any{
		"isVideoExtension":        true,
		"isVideoEdit":             false,
		"videoExtensionStartTime": startTime,
		"extendPostId":            extendPostID,
		"originalPostId":          extendPostID,
		"parentPostId":            extendPostID,
		"stitchWithExtendPostId":  true,
		"originalPrompt":          prompt,
		"originalRefType":         "ORIGINAL_REF_TYPE_VIDEO_EXTENSION",
		"mode":                    "custom",
		"aspectRatio":             ratio,
		"videoLength":             videoLength,
		"resolutionName":          resolution,
	}
	payload := map[string]any{
		"temporary": true, "modelName": "imagine-video-gen", "message": prompt + " --mode=custom", "enableSideBySide": true,
		"responseMetadata": map[string]any{"experiments": []any{}, "modelConfigOverride": map[string]any{"modelMap": map[string]any{"videoGenModelConfig": config}}},
	}
	// 拓展图生视频:顶层带原参考图的 fileId,grok 据此还原 rootPostId 血缘。
	if len(fileAttachments) > 0 {
		payload["fileAttachments"] = fileAttachments
	}
	return payload
}

// videoCreatePayload 构造文生/图生视频的 conversations/new 载荷。图生视频按 grok-web 原生
// 结构建立可拓展的根血缘:顶层 fileAttachments=[fileId] + config 里 rootPostId=fileId、
// isRootUserUploaded=true、resolvedImageReferences=[fileUri](反推自拓展抓包的回显字段),
// 这样生成的视频后续可被拓展。文生视频仍用 createMediaPost 的 parentPostId。
func videoCreatePayload(prompt, parentID, ratio, resolution string, seconds int, uploads []uploadedFile) map[string]any {
	config := map[string]any{"aspectRatio": ratio, "videoLength": seconds, "resolutionName": resolution, "isVideoEdit": false}
	top := map[string]any{
		"temporary": true, "modelName": "imagine-video-gen", "message": prompt + " --mode=custom", "enableSideBySide": true,
	}
	if len(uploads) > 0 {
		fileIds := make([]string, 0, len(uploads))
		fileUris := make([]string, 0, len(uploads))
		for _, u := range uploads {
			if u.ID != "" {
				fileIds = append(fileIds, u.ID)
			}
			if u.URI != "" {
				fileUris = append(fileUris, u.URI)
			}
		}
		config["mode"] = "custom"
		config["isRootUserUploaded"] = true
		config["originalPrompt"] = prompt
		config["parentPostId"] = nil
		if len(fileIds) > 0 {
			config["rootPostId"] = fileIds[0]
			top["fileAttachments"] = fileIds
		}
		if len(fileUris) > 0 {
			config["resolvedImageReferences"] = fileUris
		}
	} else {
		config["parentPostId"] = parentID
	}
	top["responseMetadata"] = map[string]any{"experiments": []any{}, "modelConfigOverride": map[string]any{"modelMap": map[string]any{"videoGenModelConfig": config}}}
	return top
}

func videoFileAttachments(root map[string]any) []string {
	modelResponse := nestedMap(root, "result", "response", "modelResponse")
	if modelResponse == nil {
		return nil
	}
	values, _ := modelResponse["fileAttachments"].([]any)
	attachments := make([]string, 0, len(values))
	for _, value := range values {
		if attachment, _ := value.(string); attachment != "" {
			attachments = append(attachments, attachment)
		}
	}
	return attachments
}

func setVideoResultURL(result *provider.VideoResult, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	lower := strings.ToLower(value)
	if !strings.HasSuffix(strings.SplitN(lower, "?", 2)[0], ".mp4") && !strings.Contains(lower, "/content") {
		return false
	}
	result.URL = absoluteAssetURL(value)
	result.ContentType = "video/mp4"
	return true
}
