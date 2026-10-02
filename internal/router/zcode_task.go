package router

// ZCode 登录的后台任务状态。
//
// 登录是一条需要人参与的链路：网关拿到设备码、把授权链接交给操作者、
// 等浏览器里点确认、再兑换 key 并落盘。HTTP 请求不该被这整条链路阻塞，
// 于是 POST 只负责发起并立刻返回链接，真正的轮询在后台 goroutine 里跑，
// 进度记在这里，由 GET 端点读回。
//
// 状态机刻意保持内存态、单实例：登录是一次性的人工动作，重启后重来
// 即可，持久化反而会留下过期的设备码与凭据路径。终态（Succeeded/Failed）
// 只保留到下次触发为止。

import (
	"strings"
	"time"
)

type zcodeLoginPhase string

const (
	zcodeLoginIdle       zcodeLoginPhase = "idle"
	zcodeLoginAwaiting   zcodeLoginPhase = "awaiting_authorization"
	zcodeLoginExchanging zcodeLoginPhase = "exchanging"
	zcodeLoginSucceeded  zcodeLoginPhase = "succeeded"
	zcodeLoginFailed     zcodeLoginPhase = "failed"
)

// zcodeLoginTask 是登录进度快照。字段直接进 JSON，故全部导出且带 tag。
type zcodeLoginTask struct {
	Phase     zcodeLoginPhase `json:"phase"`
	Channel   string          `json:"channel,omitempty"`
	AuthURL   string          `json:"auth_url,omitempty"`
	StartedAt time.Time       `json:"started_at,omitempty"`
	UpdatedAt time.Time       `json:"updated_at,omitempty"`
	ExpiresAt time.Time       `json:"expires_at,omitempty"`
	Error     string          `json:"error,omitempty"`
	StorePath string          `json:"store_path,omitempty"`
}

// active 报告任务是否仍在进行中。设备码有 expires_at，过期后即便
// goroutine 还卡在轮询里也算结束——否则一次忘点的授权会把后续触发
// 永久挡在 409 外面。
func (t *zcodeLoginTask) active(now time.Time) bool {
	if t == nil {
		return false
	}
	switch t.Phase {
	case zcodeLoginAwaiting, zcodeLoginExchanging:
		return t.ExpiresAt.IsZero() || now.Before(t.ExpiresAt)
	default:
		return false
	}
}

// zcodeLoginSnapshot 复制一份任务，避免把内部指针交给 HTTP 层后被后台
// goroutine 改写。零值任务返回 idle，让"从未触发"与"触发后已重置"同形。
func (r *Router) zcodeLoginSnapshot() zcodeLoginTask {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.zcodeLogin == nil {
		return zcodeLoginTask{Phase: zcodeLoginIdle}
	}
	return *r.zcodeLogin
}

// beginZcodeLogin 抢占登录槽位。已有任务在进行时返回 false，调用方据此
// 回 409——检查与占位必须在同一把锁内，否则并发 POST 会各起一条登录链。
func (r *Router) beginZcodeLogin(channel string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.zcodeLogin.active(now) {
		return false
	}
	r.zcodeLogin = &zcodeLoginTask{
		Phase:     zcodeLoginAwaiting,
		Channel:   channel,
		StartedAt: now,
		UpdatedAt: now,
	}
	return true
}

// updateZcodeLogin 就地更新任务字段。空值不覆盖已有内容：登录链接只在
// 第一次拿到时写入，后续阶段推进不应把它清掉。
func (r *Router) updateZcodeLogin(mutate func(*zcodeLoginTask)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.zcodeLogin == nil {
		return
	}
	mutate(r.zcodeLogin)
	r.zcodeLogin.UpdatedAt = time.Now()
}

func (r *Router) finishZcodeLogin(phase zcodeLoginPhase, err error, storePath string) {
	message := ""
	if err != nil {
		message = err.Error()
	}
	r.updateZcodeLogin(func(t *zcodeLoginTask) {
		t.Phase = phase
		t.Error = message
		t.StorePath = storePath
		// 成功即清掉授权链接：它已经用过，留着只会让人误以为还能再点。
		if phase == zcodeLoginSucceeded {
			t.AuthURL = ""
		}
	})
}

// normalizeZcodeChannel 校验渠道。空串取默认 bigmodel，与 login 子命令
// 的默认一致。
func normalizeZcodeChannel(value string) (string, bool) {
	channel := strings.TrimSpace(value)
	if channel == "" {
		return ZcodeChannelBigmodel, true
	}
	if channel == ZcodeChannelBigmodel || channel == ZcodeChannelZai {
		return channel, true
	}
	return "", false
}
