package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// videoDownloadTimeout 比图片下载更宽松,视频文件更大。
const videoDownloadTimeout = 5 * time.Minute

// downloadVideoToStore 用 grok_web_asset 出口 + SSO Cookie 下载 grok 生成的视频,
// 流式落盘为本地资源并返回不可猜测的资源 ID。仅供 Goal A 视频重服使用。
func (a *Adapter) downloadVideoToStore(ctx context.Context, credential account.Credential, rawURL string) (string, error) {
	if a.assets == nil {
		return "", fmt.Errorf("视频媒体存储未配置")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || !trustedImageAssetHost(parsed.Hostname()) || parsed.User != nil {
		return "", fmt.Errorf("视频内容 URL 不受信任")
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return "", err
	}
	downloadCtx, cancel := context.WithTimeout(ctx, videoDownloadTimeout)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < mediaOutputAttempts; attempt++ {
		id, retryable, attemptErr := a.downloadVideoAttempt(downloadCtx, credential, token, parsed.String())
		if attemptErr == nil {
			return id, nil
		}
		lastErr = attemptErr
		if !retryable || downloadCtx.Err() != nil || attempt+1 >= mediaOutputAttempts {
			break
		}
		if err := waitMediaOutputRetry(downloadCtx, attempt); err != nil {
			return "", err
		}
	}
	return "", lastErr
}

// downloadVideoAttempt 每次沿用同一账号,只允许出口管理器重新选择资源节点。
//
// 必须走 AcquireCredential 而非 Acquire:前者才会带上该账号的 Cloudflare cookie
// 与粘性代理绑定。上游把同构的 downloadImageAttempt 迁过去时不会碰这个文件
// ——它是 fork 独有的——而旧的 Acquire 仍然存在,漏迁**不会有任何编译错误**,
// 只会让视频下载悄悄失去 CF 绕过和出口粘性。
func (a *Adapter) downloadVideoAttempt(ctx context.Context, credential account.Credential, token, rawURL string) (string, bool, error) {
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWebAsset, credential)
	if err != nil {
		return "", true, err
	}
	defer lease.Release()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", false, err
	}
	request.Header = buildHeaders(token, lease, "")
	request.Header.Del("Content-Type")
	response, err := lease.Do(request)
	if err != nil {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, 0, err)
		return "", ctx.Err() == nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
		retryable := response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooEarly || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return "", retryable, fmt.Errorf("下载视频返回 %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType != "" && !strings.HasPrefix(contentType, "video/") {
		return "", false, fmt.Errorf("上游视频 Content-Type 无效: %s", contentType)
	}
	// 流式直灌落盘;SaveVideo 内部按上限截断并在失败时清理半成品。
	id, err := a.assets.SaveVideo(ctx, response.Body)
	if err != nil {
		// 下载/落盘融合错误:可能是上游中途断流(值得重试),也可能是本地落盘失败
		// (磁盘满/超限)。一律不惩罚出口节点健康(不一定是节点的问题),让 mediaOutputAttempts
		// 重试瞬时断流,避免直接降级到访问不了的裸 grok URL。
		return "", ctx.Err() == nil, fmt.Errorf("保存视频: %w", err)
	}
	a.egress.Feedback(context.WithoutCancel(ctx), lease.NodeID, response.StatusCode, nil)
	return id, false, nil
}
