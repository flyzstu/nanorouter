package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"
)

// 与官方 Vibe 客户端读取的目录端点保持一致：{api_base}/model-catalog?surface=vibe-code。
// 目录是 fail-open 的增强信息，拉取失败只影响自动发现的模型列表，绝不影响已有路由。
const (
	vibeCatalogPath            = "/model-catalog"
	vibeCatalogSurface         = "vibe-code"
	vibeCatalogTimeout         = 5 * time.Second
	vibeCatalogRefreshInterval = 15 * time.Minute
	// 失败后 1 分钟重试：目录短暂不可用（发布窗口、网络抖动）不该让自动发现停摆一刻钟。
	vibeCatalogRetryInterval = time.Minute
	vibeCatalogMaxBody       = 1 << 20
	vibeCatalogUserAgent     = "nanorouter"
)

// vibeModel 是目录里可路由的一个模型。contextLength 为 0 表示目录未声明。
type vibeModel struct {
	id              string
	contextLength   int
	reasoningLevels []string
	aliases         []string
	// vision 来自目录的 capabilities.vision：目录是逐模型的权威声明，
	// 而与它绑定的候选只表明"这个上游能收图"。两者都成立才发布图片能力。
	vision bool
}

func (m vibeModel) metadata() map[string]any {
	meta := make(map[string]any, 3)
	if m.contextLength > 0 {
		meta["context_length"] = m.contextLength
	}
	if len(m.reasoningLevels) > 0 {
		meta["supported_reasoning_levels"] = m.reasoningLevels
	}
	if m.vision {
		meta["input_modalities"] = []string{"text", "image"}
	}
	return meta
}

// vibeRouteEntry 把目录模型绑定到发现它的候选；同一模型被多个候选发布时取候选 id 较小者，
// 保证加权轮询表按配置稳定，不随拉取顺序抖动。
type vibeRouteEntry struct {
	model     vibeModel
	candidate Candidate
}

// vibeCatalogCandidates 按配置顺序返回启用了 vibe_catalog 的候选。
func (r *Router) vibeCatalogCandidates() []Candidate {
	candidates := make([]Candidate, 0, len(r.allCandidates))
	for _, candidate := range r.allCandidates {
		if candidate.VibeCatalog && candidate.IsEnabled() {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func (r *Router) hasVibeCatalogCandidates() bool {
	return len(r.vibeCatalogCandidates()) > 0
}

// runVibeCatalog 启动即拉一次，之后按成功/失败切换刷新间隔，直到 Close 取消。
func (r *Router) runVibeCatalog(ctx context.Context) {
	defer close(r.vibeDone)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			interval := vibeCatalogRetryInterval
			if r.refreshVibeCatalog(ctx) {
				interval = vibeCatalogRefreshInterval
			}
			if ctx.Err() != nil {
				return
			}
			timer.Reset(interval)
		}
	}
}

// refreshVibeCatalog 逐个候选拉取目录并整体替换索引。单个候选失败时沿用其上一次
// 成功的条目，避免一次网络抖动把可路由模型清空。
func (r *Router) refreshVibeCatalog(ctx context.Context) bool {
	previous := make(map[string][]vibeRouteEntry)
	// vibeRoutes 里 id 与别名都各占一个索引项，回退快照必须按 model.id 去重，
	// 否则别名多的模型会在多轮失败中不断累积重复条目。
	seenModels := make(map[string]bool)
	for _, entry := range r.vibeRoutesSnapshot() {
		if seenModels[entry.model.id] {
			continue
		}
		seenModels[entry.model.id] = true
		previous[entry.candidate.ID] = append(previous[entry.candidate.ID], entry)
	}
	merged := make([]vibeRouteEntry, 0)
	healthy := 0
	candidates := r.vibeCatalogCandidates()
	for _, u := range candidates {
		models, err := r.fetchVibeCatalog(ctx, u)
		if ctx.Err() != nil {
			return false
		}
		if err != nil {
			r.logger.Warn("vibe_catalog_refresh_failed", zap.String("candidate", u.ID), zap.Error(err))
			merged = append(merged, previous[u.ID]...)
			continue
		}
		healthy++
		for _, model := range models {
			bound := u
			bound.Model = model.id
			bound.ExposeAs = model.id
			merged = append(merged, vibeRouteEntry{model: model, candidate: bound})
		}
	}
	if healthy == 0 {
		return false
	}
	r.applyVibeCatalog(merged)
	r.logger.Info("vibe_catalog_refreshed", zap.Int("models", len(merged)), zap.Int("candidates", len(candidates)))
	return healthy == len(candidates)
}

// fetchVibeCatalog 用候选自己的凭据与 UA 拉一次目录；4xx/5xx 与非 JSON 都视为失败。
func (r *Router) fetchVibeCatalog(ctx context.Context, u Candidate) ([]vibeModel, error) {
	credential := r.resolveCandidateAPIKey(ctx, u)
	if credential == "" {
		return nil, fmt.Errorf("no credential for candidate %q", u.ID)
	}
	url := strings.TrimRight(u.BaseURL, "/") + vibeCatalogPath + "?surface=" + vibeCatalogSurface
	reqCtx, cancel := context.WithTimeout(ctx, vibeCatalogTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Accept", "application/json")
	userAgent := u.UserAgent
	if userAgent == "" {
		userAgent = vibeCatalogUserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, vibeCatalogMaxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// 上游 4xx/5xx 可能回显 Authorization；日志只留状态码与去敏感后的片段。
		detail := truncateLogMessage(string(body))
		if credential != "" {
			detail = strings.ReplaceAll(detail, credential, "***")
		}
		return nil, fmt.Errorf("model catalog rejected (%d): %s", resp.StatusCode, detail)
	}
	if len(body) >= vibeCatalogMaxBody {
		return nil, fmt.Errorf("model catalog exceeds %d bytes", vibeCatalogMaxBody)
	}
	return parseVibeCatalog(body)
}

// parseVibeCatalog 只保留可路由的条目：目录里的 publisher 是模型作者
// （zai / deepseek 等，仍由 Mistral 第一方转发），不能按它过滤；真正要挡的是
// 需要更高套餐的 requires 与非函数调用模型，与 upstream 的 _hide_reasons 一致。
func parseVibeCatalog(body []byte) ([]vibeModel, error) {
	var payload struct {
		Models []struct {
			ID               string   `json:"id"`
			Aliases          []string `json:"aliases"`
			Requires         any      `json:"requires"`
			MaxContextLength int      `json:"max_context_length"`
			Parameters       struct {
				Reasoning []string `json:"reasoning"`
			} `json:"parameters"`
			Capabilities struct {
				FunctionCalling *bool `json:"function_calling"`
				Vision          *bool `json:"vision"`
			} `json:"capabilities"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode model catalog: %w", err)
	}
	seen := make(map[string]bool, len(payload.Models))
	models := make([]vibeModel, 0, len(payload.Models))
	for _, entry := range payload.Models {
		id := strings.TrimSpace(entry.ID)
		if id == "" || seen[id] {
			continue
		}
		if entry.Requires != nil {
			continue
		}
		if entry.Capabilities.FunctionCalling != nil && !*entry.Capabilities.FunctionCalling {
			continue
		}
		seen[id] = true
		model := vibeModel{
			id:            id,
			contextLength: entry.MaxContextLength,
			aliases:       entry.Aliases,
			// 目录未声明 vision 时按不收图处理：这是安全方向，错误地声称能收图
			// 会让下游把图片发给一个会 400 的上游。
			vision: entry.Capabilities.Vision != nil && *entry.Capabilities.Vision,
		}
		for _, level := range entry.Parameters.Reasoning {
			if level = strings.TrimSpace(level); level != "" {
				model.reasoningLevels = append(model.reasoningLevels, level)
			}
		}
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].id < models[j].id })
	return models, nil
}

// applyVibeCatalog 原子替换索引；别名也进索引，让 catalog 的 aliases 能直接路由。
func (r *Router) applyVibeCatalog(entries []vibeRouteEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].model.id != entries[j].model.id {
			return entries[i].model.id < entries[j].model.id
		}
		return entries[i].candidate.ID < entries[j].candidate.ID
	})
	models := make([]vibeModel, 0, len(entries))
	index := make(map[string]vibeRouteEntry, len(entries))
	// 分两轮插入：先占满所有规范 id，再补别名。否则某个模型的别名可能抢走另一个
	// 模型中真正存在的 id，让后者按自己的 id 请求时被转发成前者的模型。
	for _, entry := range entries {
		if _, ok := index[entry.model.id]; ok {
			continue
		}
		index[entry.model.id] = entry
		models = append(models, entry.model)
	}
	for _, entry := range entries {
		for _, alias := range entry.model.aliases {
			if alias = strings.TrimSpace(alias); alias != "" {
				if _, ok := index[alias]; !ok {
					index[alias] = entry
				}
			}
		}
	}
	r.vibeMu.Lock()
	r.vibeModels = models
	r.vibeRoutes = index
	r.vibeMu.Unlock()
}

func (r *Router) vibeModelsSnapshot() []vibeModel {
	r.vibeMu.RLock()
	defer r.vibeMu.RUnlock()
	if len(r.vibeModels) == 0 {
		return nil
	}
	models := make([]vibeModel, len(r.vibeModels))
	copy(models, r.vibeModels)
	return models
}

func (r *Router) vibeRoutesSnapshot() []vibeRouteEntry {
	r.vibeMu.RLock()
	defer r.vibeMu.RUnlock()
	entries := make([]vibeRouteEntry, 0, len(r.vibeRoutes))
	for _, entry := range r.vibeRoutes {
		entries = append(entries, entry)
	}
	return entries
}

// vibeRouteForModel 把目录模型解析成单候选路由；配置里已声明的路由与 expose_as
// 在 routeForModel 里先被匹配，因此这里的兜底不会覆盖显式配置。
func (r *Router) vibeRouteForModel(name string) (Route, bool) {
	r.vibeMu.RLock()
	entry, ok := r.vibeRoutes[name]
	r.vibeMu.RUnlock()
	if !ok {
		return Route{}, false
	}
	// 目录逐模型声明 vision，候选只声明上游能收图；发布能力时必须两者都成立，
	// 因此这里按目录结论改写副本，而不是把候选配置当成逐模型的事实。
	candidate := entry.candidate
	candidate.Vision = candidate.Vision && entry.model.vision
	return Route{Candidates: []Candidate{candidate}}, true
}
