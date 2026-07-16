package web

import (
	"bufio"
	"context"
	"encoding/json"
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
	lease, err := a.egress.Acquire(ctx, domainegress.ScopeWeb, fmt.Sprintf("%d", request.Credential.ID))
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
		// 拓展 grok 已生成的视频:extendPostId=originalPostId=parentPostId=源 videoPostId,
		// 不上传参考图、不建 media post(与抓包一致)。
		if strings.TrimSpace(request.ExtendPostID) == "" {
			return provider.VideoResult{}, fmt.Errorf("视频拓展缺少来源 postId")
		}
		videoLength := request.VideoLength
		if videoLength <= 0 {
			videoLength = segments[0]
		}
		payload = videoExtensionPayload(request.Prompt, request.ExtendPostID, ratio, resolution, videoLength, request.VideoExtensionStartTime)
	} else {
		parentID := ""
		references := make([]string, 0, len(request.ReferenceURLs))
		for _, rawReference := range request.ReferenceURLs {
			reference, referenceErr := a.prepareVideoReference(ctx, cfg, lease, token, rawReference)
			if referenceErr != nil {
				return provider.VideoResult{}, referenceErr
			}
			references = append(references, reference)
		}
		if len(references) > 0 {
			parentID, err = a.createMediaPost(ctx, cfg, lease, token, "MEDIA_POST_TYPE_IMAGE", references[0], "")
		} else {
			parentID, err = a.createMediaPost(ctx, cfg, lease, token, "MEDIA_POST_TYPE_VIDEO", "", request.Prompt)
		}
		if err != nil {
			return provider.VideoResult{}, err
		}
		payload = videoCreatePayload(request.Prompt, parentID, ratio, resolution, segments[0], references)
	}
	response, err := a.postJSON(ctx, cfg, lease, token, cfg.BaseURL+"/rest/app-chat/conversations/new", payload, time.Duration(cfg.VideoTimeoutSeconds)*time.Second)
	if err != nil {
		return provider.VideoResult{}, err
	}
	result, postID, parseErr := parseVideoStream(response, request.Progress)
	_ = response.Body.Close()
	if parseErr != nil {
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

func (a *Adapter) prepareVideoReference(ctx context.Context, cfg Config, lease *egress.Lease, token, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("视频参考图片 URL 不能为空")
	}
	image, err := a.loadChatImage(ctx, lease, value, 20<<20)
	if err != nil {
		return "", err
	}
	uploaded, err := a.uploadImage(ctx, cfg, lease, token, image, cfg.BaseURL+"/imagine")
	if err != nil {
		return "", err
	}
	if uploaded.URI == "" {
		return "", fmt.Errorf("上传视频参考图片后未返回 fileUri")
	}
	return uploaded.URI, nil
}

func parseVideoStream(response *http.Response, progress func(int)) (provider.VideoResult, string, error) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if response.StatusCode == http.StatusUnauthorized {
			return provider.VideoResult{}, "", provider.ErrUnauthorized
		}
		return provider.VideoResult{}, "", &videoUpstreamError{status: response.StatusCode, body: strings.TrimSpace(string(body))}
	}
	var result provider.VideoResult
	var postID string
	handle := func(root map[string]any) (bool, error) {
		if errorValue, ok := root["error"].(map[string]any); ok {
			return false, fmt.Errorf("视频上游错误: %v", errorValue["message"])
		}
		stream := nestedMap(root, "result", "response", "streamingVideoGenerationResponse")
		if stream == nil {
			return false, nil
		}
		if value, ok := numberAsInt(stream["progress"]); ok && progress != nil {
			progress(value)
		}
		if value, _ := stream["videoPostId"].(string); value != "" {
			postID = value
		} else if value, _ := stream["videoId"].(string); value != "" {
			postID = value
		}
		moderated, _ := stream["moderated"].(bool)
		if moderated {
			return false, nil
		}
		if value, _ := stream["videoUrl"].(string); value != "" {
			result.URL = absoluteAssetURL(value)
			result.ContentType = "video/mp4"
			return true, nil
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
func videoExtensionPayload(prompt, extendPostID, ratio, resolution string, videoLength int, startTime float64) map[string]any {
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
	return map[string]any{
		"temporary": true, "modelName": "imagine-video-gen", "message": prompt + " --mode=custom", "enableSideBySide": true,
		"responseMetadata": map[string]any{"experiments": []any{}, "modelConfigOverride": map[string]any{"modelMap": map[string]any{"videoGenModelConfig": config}}},
	}
}

func videoCreatePayload(prompt, parentID, ratio, resolution string, seconds int, references []string) map[string]any {
	config := map[string]any{"parentPostId": parentID, "aspectRatio": ratio, "videoLength": seconds, "resolutionName": resolution}
	if len(references) > 0 {
		config["isVideoEdit"] = false
		config["isReferenceToVideo"] = true
		config["imageReferences"] = references
	}
	return map[string]any{
		"temporary": true, "modelName": "imagine-video-gen", "message": prompt + " --mode=custom", "enableSideBySide": true,
		"responseMetadata": map[string]any{"experiments": []any{}, "modelConfigOverride": map[string]any{"modelMap": map[string]any{"videoGenModelConfig": config}}},
	}
}
