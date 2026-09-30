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
	"net/http"
	"time"

	json "github.com/goccy/go-json"
)

//go:embed stats.html
var statsPageHTML []byte

// StatsSummary 是一次聚合的完整结果，字段直接对齐页面的渲染需求。
type StatsSummary struct {
	Range    string         `json:"range"`
	Bucket   string         `json:"bucket"`
	Totals   StatsTotals    `json:"totals"`
	Series   []StatsPoint   `json:"series"`
	Models   []StatsModel   `json:"models"`
	Failures []StatsFailure `json:"failures"`
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
	summary := StatsSummary{Series: []StatsPoint{}, Models: []StatsModel{}, Failures: []StatsFailure{}}
	// RFC3339Nano 会裁掉尾随零，边界秒内不同小数位长度的字典序比较存在
	// 理论误判；对统计页而言只影响桶边界±1s 的归属，不值得为它换格式。
	sinceText := since.UTC().Format(time.RFC3339Nano)
	successExpr := "CASE WHEN status_code BETWEEN 200 AND 399 THEN 1 ELSE 0 END"

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
		`SELECT substr(created_at, 1, ?), COUNT(*), SUM(CASE WHEN `+successExpr+` THEN 0 ELSE 1 END)
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

	return summary, rows.Err()
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

// countTools 统计下游请求携带的 tools 数量；形状不符一律记 0，不做猜测。
func countTools(payload map[string]any) int {
	tools, ok := payload["tools"].([]any)
	if !ok {
		return 0
	}
	return len(tools)
}
