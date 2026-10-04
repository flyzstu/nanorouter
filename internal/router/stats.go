package router

// 调用统计：从 llm_requests 台账做只读聚合，供 /v1/stats/summary 端点与
// 内置 /stats 页面消费。只做台账里已有字段的聚合，不做内存态快照
// （熔断/并发水位）也不做成本换算——那是后续扩展。
//
// 全部 SQL 按 SQLite 与 PostgreSQL 双方言可移植书写：时间分桶用
// substr(created_at, 1, N)（RFC3339 的前缀即时间桶），百分位用
// ORDER BY ... LIMIT 1 OFFSET k，失败分类用 COALESCE + || 拼接。

import (
	"context"
	_ "embed"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	json "github.com/goccy/go-json"
	"go.uber.org/zap"
)

//go:embed stats.html
var statsPageHTML []byte

// StatsSummary 是一次聚合的完整结果，字段直接对齐页面的渲染需求。
type StatsSummary struct {
	Range     string          `json:"range"`
	Bucket    string          `json:"bucket"`
	Totals    StatsTotals     `json:"totals"`
	Series    []StatsPoint    `json:"series"`
	Models    []StatsModel    `json:"models"`
	Failures  []StatsFailure  `json:"failures"`
	Upstreams []StatsUpstream `json:"upstreams"`
	Clients   []StatsClient   `json:"clients"`
	Agents    []StatsAgent    `json:"agents"`
	Tools     []StatsTool     `json:"tools"`
}

type StatsTotals struct {
	Requests         int64 `json:"requests"`
	Success          int64 `json:"success"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	WithTools        int64 `json:"with_tools"`
	LatencyP50MS     int64 `json:"latency_p50_ms"`
	LatencyP95MS     int64 `json:"latency_p95_ms"`
}

type StatsPoint struct {
	Bucket   string `json:"bucket"`
	Calls    int64  `json:"calls"`
	Failures int64  `json:"failures"`
}

type StatsModel struct {
	Route         string `json:"route"`
	UpstreamModel string `json:"upstream_model"`
	Calls         int64  `json:"calls"`
	Success       int64  `json:"success"`
	TotalTokens   int64  `json:"total_tokens"`
	AvgLatencyMS  int64  `json:"avg_latency_ms"`
}

type StatsFailure struct {
	Key   string `json:"key"`
	Count int64  `json:"count"`
}

type StatsUpstream struct {
	UpstreamID   string `json:"upstream_id"`
	Calls        int64  `json:"calls"`
	Success      int64  `json:"success"`
	TotalTokens  int64  `json:"total_tokens"`
	AvgLatencyMS int64  `json:"avg_latency_ms"`
}

type StatsClient struct {
	ClientIP    string `json:"client_ip"`
	Calls       int64  `json:"calls"`
	Success     int64  `json:"success"`
	TotalTokens int64  `json:"total_tokens"`
	LastSeen    string `json:"last_seen"`
}

type StatsAgent struct {
	Agent   string `json:"agent"`
	Calls   int64  `json:"calls"`
	Success int64  `json:"success"`
}

type StatsTool struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

// statsRanges 是页面允许的时间范围：范围 → 分桶前缀长度与桶标签。
// RFC3339 "2026-09-30T15:41:22Z"：取 16 位精确到分、13 位到时、10 位到天。
var statsRanges = map[string]struct {
	SubstrLen int
	Bucket    string
}{
	"1h":  {16, "1m"},
	"24h": {13, "1h"},
	"7d":  {10, "1d"},
	"30d": {10, "1d"},
}

// Summary 聚合 since 之后的台账记录。bucketSubstrLen 见 statsRanges。
func (s *Store) Summary(ctx context.Context, since time.Time, bucketSubstrLen int) (StatsSummary, error) {
	summary := StatsSummary{Series: []StatsPoint{}, Models: []StatsModel{}, Failures: []StatsFailure{},
		Upstreams: []StatsUpstream{}, Clients: []StatsClient{}, Agents: []StatsAgent{}, Tools: []StatsTool{}}
	// RFC3339Nano 会裁掉尾随零，边界秒内不同小数位长度的字典序比较存在
	// 理论误判；对统计页而言只影响桶边界±1s 的归属，不值得为它换格式。
	sinceText := since.UTC().Format(time.RFC3339Nano)
	successExpr := "CASE WHEN status_code BETWEEN 200 AND 399 THEN 1 ELSE 0 END"
	failureExpr := "CASE WHEN status_code BETWEEN 200 AND 399 THEN 0 ELSE 1 END"

	err := s.db.QueryRowContext(ctx, s.rebind(
		`SELECT COUNT(*), COALESCE(SUM(`+successExpr+`), 0),
		 COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(total_tokens), 0),
		 COALESCE(SUM(CASE WHEN tools_count > 0 THEN 1 ELSE 0 END), 0)
		 FROM llm_requests WHERE created_at >= ?`),
		sinceText,
	).Scan(&summary.Totals.Requests, &summary.Totals.Success,
		&summary.Totals.PromptTokens, &summary.Totals.CompletionTokens, &summary.Totals.TotalTokens,
		&summary.Totals.WithTools)
	if err != nil {
		return summary, err
	}

	// 百分位不依赖方言函数：按 latency_ms 排序后取第 k 行（k 从 0 计）。
	if summary.Totals.Requests > 0 {
		fetchPercentile := func(k int64) (int64, error) {
			var latency int64
			err := s.db.QueryRowContext(ctx, s.rebind(
				`SELECT latency_ms FROM llm_requests WHERE created_at >= ? ORDER BY latency_ms LIMIT 1 OFFSET ?`),
				sinceText, k).Scan(&latency)
			return latency, err
		}
		n := summary.Totals.Requests
		if summary.Totals.LatencyP50MS, err = fetchPercentile((n - 1) / 2); err != nil {
			return summary, err
		}
		if summary.Totals.LatencyP95MS, err = fetchPercentile((n - 1) * 95 / 100); err != nil {
			return summary, err
		}
	}

	rows, err := s.db.QueryContext(ctx, s.rebind(
		`SELECT substr(created_at, 1, ?), COUNT(*), COALESCE(SUM(`+failureExpr+`), 0)
		 FROM llm_requests WHERE created_at >= ? GROUP BY 1 ORDER BY 1`),
		bucketSubstrLen, sinceText)
	if err != nil {
		return summary, err
	}
	for rows.Next() {
		var point StatsPoint
		if err := rows.Scan(&point.Bucket, &point.Calls, &point.Failures); err != nil {
			rows.Close()
			return summary, err
		}
		summary.Series = append(summary.Series, point)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, s.rebind(
		`SELECT route, upstream_model, COUNT(*), COALESCE(SUM(`+successExpr+`), 0),
		 COALESCE(SUM(total_tokens), 0), COALESCE(AVG(latency_ms), 0)
		 FROM llm_requests WHERE created_at >= ?
		 GROUP BY route, upstream_model ORDER BY COUNT(*) DESC LIMIT 20`),
		sinceText)
	if err != nil {
		return summary, err
	}
	for rows.Next() {
		var m StatsModel
		// SQLite 的 AVG 返回 float64，PostgreSQL 对整数列返回 numeric——
		// 统一落进 float64 再取整，避免按驱动差异各自处理。
		var avgLatency float64
		if err := rows.Scan(&m.Route, &m.UpstreamModel, &m.Calls, &m.Success, &m.TotalTokens, &avgLatency); err != nil {
			rows.Close()
			return summary, err
		}
		m.AvgLatencyMS = int64(avgLatency + 0.5)
		summary.Models = append(summary.Models, m)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, s.rebind(
		// 成功记录 kind 为空串；失败记录缺 kind 时（历史数据）退回状态码标签。
		`SELECT COALESCE(NULLIF(error_kind, ''), 'http_' || CAST(status_code AS TEXT)), COUNT(*)
		 FROM llm_requests WHERE created_at >= ? AND status_code NOT BETWEEN 200 AND 399
		 GROUP BY 1 ORDER BY 2 DESC LIMIT 10`),
		sinceText)
	if err != nil {
		return summary, err
	}
	for rows.Next() {
		var f StatsFailure
		if err := rows.Scan(&f.Key, &f.Count); err != nil {
			rows.Close()
			return summary, err
		}
		summary.Failures = append(summary.Failures, f)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, s.rebind(
		`SELECT upstream_id, COUNT(*), COALESCE(SUM(`+successExpr+`), 0),
		 COALESCE(SUM(total_tokens), 0), COALESCE(AVG(latency_ms), 0)
		 FROM llm_requests WHERE created_at >= ?
		 GROUP BY upstream_id ORDER BY COUNT(*) DESC LIMIT 20`),
		sinceText)
	if err != nil {
		return summary, err
	}
	for rows.Next() {
		var u StatsUpstream
		var avgLatency float64
		if err := rows.Scan(&u.UpstreamID, &u.Calls, &u.Success, &u.TotalTokens, &avgLatency); err != nil {
			rows.Close()
			return summary, err
		}
		u.AvgLatencyMS = int64(avgLatency + 0.5)
		summary.Upstreams = append(summary.Upstreams, u)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, s.rebind(
		// 旧数据 client_ip 为空串：归入"未知"而不是丢掉调用量。
		`SELECT COALESCE(NULLIF(client_ip, ''), '(未知)'), COUNT(*), COALESCE(SUM(`+successExpr+`), 0),
		 COALESCE(SUM(total_tokens), 0), MAX(created_at)
		 FROM llm_requests WHERE created_at >= ?
		 GROUP BY 1 ORDER BY 2 DESC LIMIT 20`),
		sinceText)
	if err != nil {
		return summary, err
	}
	for rows.Next() {
		var c StatsClient
		if err := rows.Scan(&c.ClientIP, &c.Calls, &c.Success, &c.TotalTokens, &c.LastSeen); err != nil {
			rows.Close()
			return summary, err
		}
		summary.Clients = append(summary.Clients, c)
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, s.rebind(
		`SELECT COALESCE(NULLIF(client_agent, ''), '(未知)'), COUNT(*), COALESCE(SUM(`+successExpr+`), 0)
		 FROM llm_requests WHERE created_at >= ?
		 GROUP BY 1 ORDER BY 2 DESC LIMIT 10`),
		sinceText)
	if err != nil {
		return summary, err
	}
	for rows.Next() {
		var a StatsAgent
		if err := rows.Scan(&a.Agent, &a.Calls, &a.Success); err != nil {
			rows.Close()
			return summary, err
		}
		summary.Agents = append(summary.Agents, a)
	}
	rows.Close()

	// 工具榜没有跨方言的 JSON 展开函数（json_each vs jsonb_array_elements），
	// 把范围内的工具名拉回 Go 侧聚合；范围上限 30 天、每行至多 2KB，量级可控。
	rows, err = s.db.QueryContext(ctx, s.rebind(
		`SELECT tool_names FROM llm_requests WHERE created_at >= ? AND tool_names != ''`),
		sinceText)
	if err != nil {
		return summary, err
	}
	tally := map[string]int64{}
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			rows.Close()
			return summary, err
		}
		var names []string
		if json.Unmarshal([]byte(encoded), &names) != nil {
			continue
		}
		for _, name := range names {
			// 聚合侧与写入口同规：绕过网关直写库的超长名字不能进榜单。
			if name == "" || len(name) > maxToolName {
				continue
			}
			tally[name]++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return summary, err
	}
	for name, count := range tally {
		summary.Tools = append(summary.Tools, StatsTool{Name: name, Count: count})
	}
	sort.Slice(summary.Tools, func(i, j int) bool { return summary.Tools[i].Count > summary.Tools[j].Count })
	if len(summary.Tools) > 15 {
		summary.Tools = summary.Tools[:15]
	}

	return summary, nil
}

// writeJSONError 与网关其余错误响应保持同一信封形状。
func writeJSONError(w http.ResponseWriter, status int, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": message, "code": code}})
}

// statsSummary 服务 GET /v1/stats/summary?range=1h|24h|7d|30d。
func (r *Router) statsSummary(w http.ResponseWriter, q *http.Request) {
	rangeName := q.URL.Query().Get("range")
	spec, ok := statsRanges[rangeName]
	if !ok {
		rangeName = "24h"
		spec = statsRanges[rangeName]
	}
	summary, err := r.db.Summary(q.Context(), time.Now().Add(-statsRangeDuration(rangeName)), spec.SubstrLen)
	if err != nil {
		if r.logger != nil && r.logger.Core().Enabled(zap.WarnLevel) {
			r.logger.Warn(r.logMessage("stats_query_failed"), zap.Error(err), zap.String("range", rangeName))
		}
		writeJSONError(w, http.StatusInternalServerError, "stats query failed", "stats_unavailable")
		return
	}
	summary.Range = rangeName
	summary.Bucket = spec.Bucket
	summary.Series = fillSeriesBuckets(summary.Series, rangeName, spec)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(summary)
}

func statsRangeDuration(name string) time.Duration {
	switch name {
	case "1h":
		return time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	case "30d":
		return 30 * 24 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// fillSeriesBuckets 把缺失的空桶补零，时间轴才能和人类感知的刻度对齐；
// 窗口内完全没有请求时同样补出整条零轴，前端据此渲染空态而非空白。
// 桶以 UTC 文本前缀生成，与 SQL 侧 substr 的键一致。
func fillSeriesBuckets(measured []StatsPoint, rangeName string, spec struct {
	SubstrLen int
	Bucket    string
}) []StatsPoint {
	index := make(map[string]StatsPoint, len(measured))
	for _, point := range measured {
		index[point.Bucket] = point
	}
	now := time.Now().UTC()
	var buckets []string
	switch spec.Bucket {
	case "1m":
		start := now.Truncate(time.Minute).Add(-59 * time.Minute)
		for i := 0; i < 60; i++ {
			buckets = append(buckets, start.Add(time.Duration(i)*time.Minute).Format("2006-01-02T15:04"))
		}
	case "1h":
		start := now.Truncate(time.Hour).Add(-23 * time.Hour)
		for i := 0; i < 24; i++ {
			buckets = append(buckets, start.Add(time.Duration(i)*time.Hour).Format("2006-01-02T15"))
		}
	default:
		days := 7
		if rangeName == "30d" {
			days = 30
		}
		start := now.Truncate(24 * time.Hour).Add(time.Duration(-(days - 1)) * 24 * time.Hour)
		for i := 0; i < days; i++ {
			buckets = append(buckets, start.Add(time.Duration(i)*24*time.Hour).Format("2006-01-02"))
		}
	}
	filled := make([]StatsPoint, 0, len(buckets))
	for _, bucket := range buckets {
		if point, ok := index[bucket]; ok {
			filled = append(filled, point)
			continue
		}
		filled = append(filled, StatsPoint{Bucket: bucket})
	}
	return filled
}

// statsPage 服务 GET /stats 的内置单页。
func (r *Router) statsPage(w http.ResponseWriter, q *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(statsPageHTML)
}

// requestMeta 是每次代理请求采集的调用方元数据，一次生成、随 record 落盘。
// 全部是请求元数据：不含 prompt、回复内容或凭据。
type requestMeta struct {
	Tools       int
	ToolNames   string // JSON 数组文本；无工具时为空串
	ClientIP    string
	ClientAgent string
}

const (
	maxToolNamesJSON = 2048 // 防病态超大 tools 数组撑爆台账
	maxToolName      = 256  // 单个工具名上限，写入口与聚合侧同规
	maxClientAgent   = 256
)

// newRequestMeta 从请求与已解析 payload 提取元数据。形状不符一律记零值，
// 不做猜测；OpenAI（tools[].function.name）与 Anthropic（tools[].name）
// 两种工具形状都识别。
func newRequestMeta(q *http.Request, payload map[string]any, trustForwardedFor bool) requestMeta {
	var names []string
	if raw, ok := payload["tools"].([]any); ok {
		for _, rawTool := range raw {
			tool, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			name := ""
			if fn, ok := tool["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = tool["name"].(string)
			}
			if name != "" {
				names = append(names, name)
			}
		}
	}
	toolNames := ""
	if len(names) > 0 {
		if encoded, err := json.Marshal(names); err == nil && len(encoded) <= maxToolNamesJSON {
			toolNames = string(encoded)
		}
	}
	agent := strings.TrimSpace(q.Header.Get("User-Agent"))
	if len(agent) > maxClientAgent {
		agent = agent[:maxClientAgent]
	}
	return requestMeta{Tools: len(names), ToolNames: toolNames, ClientIP: clientIP(q, trustForwardedFor), ClientAgent: agent}
}

// clientIP 解析调用方 IP：开启 trust_forwarded_for 且头存在时取
// X-Forwarded-For 首值（链路最左侧的原始客户端），否则用 TCP 对端地址
// （去端口）。
func clientIP(q *http.Request, trustForwardedFor bool) string {
	if trustForwardedFor {
		if xff := q.Header.Get("X-Forwarded-For"); xff != "" {
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
				return first
			}
		}
	}
	host, _, err := net.SplitHostPort(q.RemoteAddr)
	if err != nil || host == "" {
		return q.RemoteAddr
	}
	return host
}
