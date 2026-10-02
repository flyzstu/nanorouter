package router

// 启动时的凭据体检，以及运行期失效后的重新登录。
//
// 为什么需要自动登录：key 有有效期，而智谱的设备码流程不回传有效期（见
// storeZcodeToken 的说明），本地无法算。它只会在真实请求打到上游、收到 401
// 时才暴露——那时已经有一个用户请求失败了。启动时主动探一次，把失效提前到
// 进程刚起来时，而不是等第一个用户请求替我们发现。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"
)

// zcodeProbeStartupDelay 给上游一点启动余量。刚起来就立刻打探测，若网关自身
// 尚未就绪会得到连接错误，被误判成「凭据失效」而触发一次无谓登录。
const zcodeProbeStartupDelay = 3 * time.Second

// startZcodeCredentialCheck 异步体检凭据。没有 zcode 候选时直接返回。
//
// 判定分三种，与登录链的探测纪律一致：
//   - 无凭据 → 发起登录（这是首次部署或凭据被清空后的正常路径）
//   - 探测 401/403 → 凭据已失效，发起登录
//   - 探测通过或网关不可达 → 沿用，不触发（瞬时故障不该引发登录风暴）
func (r *Router) startZcodeCredentialCheck() {
	if !r.hasZcodeTokenFileCandidate() {
		return
	}
	go func() {
		time.Sleep(zcodeProbeStartupDelay)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		channel := r.primaryZcodeChannel()
		credential, haveCredential := r.currentZcodeCredential(ctx, channel)

		switch {
		case !haveCredential:
			r.logger.Info(r.logMessage("zcode_credential_missing"),
				eventFields("zcode_credential_missing", zap.String("channel", channel))...)
			r.triggerZcodeLogin(channel, "no credential at startup")
		case r.probeZcodeCredential(ctx, credential) == errZcodeCredentialRefused:
			r.logger.Warn(r.logMessage("zcode_credential_stale"),
				eventFields("zcode_credential_stale", zap.String("channel", channel))...)
			r.triggerZcodeLogin(channel, "credential rejected at startup")
		default:
			r.logger.Info(r.logMessage("zcode_credential_verified"),
				eventFields("zcode_credential_verified",
					zap.String("channel", channel),
					zap.String("last_verified", credential.LastVerified.Format(time.RFC3339)))...)
		}
	}()
}

// hasZcodeTokenFileCandidate 报告是否有候选会走凭据链。
func (r *Router) hasZcodeTokenFileCandidate() bool {
	for _, c := range r.allCandidates {
		if c.ZcodeTokenFile && c.Anthropic {
			return true
		}
	}
	return false
}

// primaryZcodeChannel 取首个 zcode 候选的渠道作为体检对象。多渠道时只查一个：
// 凭据按渠道独立，逐一体检会让启动日志随渠道数线性增长。
func (r *Router) primaryZcodeChannel() string {
	for _, c := range r.allCandidates {
		if c.ZcodeTokenFile && c.Anthropic {
			return c.ZcodeChannel
		}
	}
	return ZcodeChannelBigmodel
}

// currentZcodeCredential 解析该渠道当前可用的凭据，来源顺序与请求路径一致。
func (r *Router) currentZcodeCredential(ctx context.Context, channel string) (ZcodeCredential, bool) {
	if r.zcodeCredentials != nil {
		if c, ok := r.zcodeCredentials.get(ctx, channel); ok {
			return c, true
		}
	}
	// 回退到文件源（CLI 登录留下的文件、或测试环境）。
	if key := zcodeTokenFileKey(channel); key != "" {
		return ZcodeCredential{Channel: channel, APIKey: key}, true
	}
	return ZcodeCredential{}, false
}

// errZcodeCredentialRefused 表示上游明确拒绝了凭据。
var errZcodeCredentialRefused = &zcodeCredentialError{"upstream refused the credential"}

type zcodeCredentialError struct{ msg string }

func (e *zcodeCredentialError) Error() string { return e.msg }

// probeZcodeCredential 发一次 max_tokens:1 请求验证 key。只有 401/403 能证伪；
// 网关不可达等暂时性问题返回 nil（视为通过）——把瞬时故障误报成凭据失效，
// 会让一次网络抖动触发登录风暴。
func (r *Router) probeZcodeCredential(ctx context.Context, c ZcodeCredential) error {
	client := &http.Client{Timeout: 10 * time.Second}
	body, _ := json.Marshal(map[string]any{
		"model":      zcodeProbeModel,
		"max_tokens": 1,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	channel := c.Channel
	if channel != ZcodeChannelZai {
		channel = ZcodeChannelBigmodel
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ZcodeDefaultOrigin+"/"+zcodeGatewayPath(channel), bytes.NewReader(body))
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("x-api-key", c.APIKey)
	req.Header.Set("anthropic-version", ZcodeAnthropicVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ZCode/"+ZcodeClientVersion)
	resp, err := client.Do(req)
	if err != nil {
		return nil // 网关不可达不能证伪凭据。
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errZcodeCredentialRefused
	}
	return nil
}

// maybeTriggerZcodeRelogin 在凭据被上游拒绝后触发一次登录。
//
// 只对走凭据链的 anthropic 候选生效：其他上游的 401 多半是 key 配错或被
// 吊销，重新登录智谱解决不了，凭空打一轮设备码只是噪音。
//
// 触发是尽力而为：登录在后台跑，这个请求照常把 401 返回给下游。不阻塞
// 当前请求是刻意的——授权要等人点确认，挂在上面会让所有并发请求一起超时。
func (r *Router) maybeTriggerZcodeRelogin(u Candidate, reason string) {
	if !u.Anthropic || !u.ZcodeTokenFile {
		return
	}
	r.triggerZcodeLogin(u.ZcodeChannel, reason)
}

// triggerZcodeLogin 发起一次登录，reason 只进日志便于回溯触发来源。
func (r *Router) triggerZcodeLogin(channel, reason string) {
	if !r.beginZcodeLogin(channel, time.Now()) {
		return // 已有任务在跑，不重复发起。
	}
	r.logger.Info(r.logMessage("zcode_login_triggered"),
		eventFields("zcode_login_triggered", zap.String("channel", channel), zap.String("reason", reason))...)

	ctx, cancel := context.WithCancel(context.Background())
	linkReady := make(chan struct{})
	var once sync.Once
	notify := func(phase zcodeLoginPhase, link string, expiresAt time.Time) {
		r.updateZcodeLogin(func(t *zcodeLoginTask) {
			t.Phase = phase
			if link != "" {
				t.AuthURL = link
				t.ExpiresAt = expiresAt
			}
		})
		once.Do(func() { close(linkReady) })
	}

	go func() {
		defer cancel()
		storePath, err := RunZcodeLogin(ctx, ZcodeLoginOptions{
			GatewayOrigin: zcodeGatewayOrigin(),
			Channel:       channel,
			Output:        zapWriter{router: r},
			OnProgress:    notify,
			Store:         r.zcodeCredentials,
		})
		once.Do(func() { close(linkReady) })
		if err != nil {
			r.finishZcodeLogin(zcodeLoginFailed, err, "")
			r.logger.Warn(r.logMessage("zcode_login_failed"),
				eventFields("zcode_login_failed", zap.String("channel", channel), zap.String("reason", err.Error()))...)
			return
		}
		r.finishZcodeLogin(zcodeLoginSucceeded, nil, storePath)
		r.logger.Info(r.logMessage("zcode_login_succeeded"),
			eventFields("zcode_login_succeeded", zap.String("channel", channel), zap.String("store_path", storePath))...)
	}()

	go func() {
		select {
		case <-linkReady:
			if task := r.zcodeLoginSnapshot(); task.AuthURL != "" {
				r.logger.Warn(r.logMessage("zcode_login_authorization_required"),
					eventFields("zcode_login_authorization_required",
						zap.String("channel", channel), zap.String("auth_url", task.AuthURL))...)
			}
		case <-time.After(zcodeLoginStartTimeout):
		case <-ctx.Done():
		}
	}()
}
