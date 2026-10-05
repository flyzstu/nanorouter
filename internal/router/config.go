package router

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen   string         `yaml:"listen"`
	Database DatabaseConfig `yaml:"database"`
	Logging  LoggingConfig  `yaml:"logging"`
	// AuthEnv 指向存放网关 API key 的环境变量名；配置后所有端点要求认证，
	// 接受 Authorization: Bearer <key> 与 X-Api-Key: <key> 两种传参并存
	// （OpenAI 系与 Anthropic 系客户端各自的习惯头，同一把 key 皆可）。
	AuthEnv string `yaml:"auth_env"`
	// StatsPublic 将统计面（/stats、/v1/stats/summary、/v1/zcode/usage）
	// 整体豁免出鉴权：内网或反代层已做访问控制、只想公开面板时开启。
	// 注意 /v1/zcode/usage 会匿名触发网关用所存凭据外呼智谱监控端点，
	// 并暴露配额与积分等账户级数据；生成端点不受影响，照常要求 key。
	StatsPublic bool `yaml:"stats_public"`
	// TrustForwardedFor 开启后统计里的调用方 IP 取 X-Forwarded-For 首值。
	// 仅在网关前面有可信反代时开启；直连部署开启会让调用方伪造来源。
	TrustForwardedFor bool               `yaml:"trust_forwarded_for"`
	RequestTimeout    time.Duration      `yaml:"request_timeout"`
	Retry             RetryConfig        `yaml:"retry"`
	Backpressure      BackpressureConfig `yaml:"backpressure"`
	Profiling         ProfilingConfig    `yaml:"profiling"`
	HealthCheck       HealthCheckConfig  `yaml:"health_check"`
	// UserAgent 全局覆盖转发到上游的 User-Agent。候选的 user_agent 优先于全局值；
	// 两者都未配置时使用 Go 默认的 Go-http-client/x.x。部分上游的 Cloudflare WAF
	// 会拦截该默认 UA，可在此配置常规客户端 UA（例如 opencode/1.18.25）统一绕过。
	UserAgent string           `yaml:"user_agent"`
	Routes    map[string]Route `yaml:"routes"`
}

// BackpressureConfig 对所有生成请求设置进程级准入上限，避免上游变慢时请求和请求体无限堆积。
type BackpressureConfig struct {
	MaxInflight  int           `yaml:"max_inflight"`
	MaxQueue     int           `yaml:"max_queue"`
	QueueTimeout time.Duration `yaml:"queue_timeout"`
}

func (c *BackpressureConfig) normalize() error {
	if c.MaxInflight < 0 || c.MaxQueue < 0 || c.QueueTimeout < 0 {
		return fmt.Errorf("backpressure settings must not be negative")
	}
	if c.MaxInflight == 0 {
		if c.MaxQueue > 0 || c.QueueTimeout > 0 {
			return fmt.Errorf("backpressure.max_inflight must be set when queueing is configured")
		}
		return nil
	}
	if c.MaxQueue == 0 && c.QueueTimeout > 0 {
		return fmt.Errorf("backpressure.queue_timeout requires max_queue")
	}
	if c.MaxQueue > 0 && c.QueueTimeout == 0 {
		c.QueueTimeout = time.Second
	}
	return nil
}

type ProfilingConfig struct {
	Enabled bool `yaml:"enabled"`
}

// HealthCheckConfig 控制熔断后的后台异步探活机制。
// 仅在候选因 503、网络错误或超时进入冷却熔断后触发，健康状态下不发请求，零额外额度消耗。
type HealthCheckConfig struct {
	Enabled      bool          `yaml:"enabled"`
	ProbeTimeout time.Duration `yaml:"probe_timeout"`
	Interval     time.Duration `yaml:"interval"`
	MaxBackoff   time.Duration `yaml:"max_backoff"`
}

func (c *HealthCheckConfig) normalize() error {
	if c.ProbeTimeout < 0 || c.Interval < 0 || c.MaxBackoff < 0 {
		return fmt.Errorf("health_check settings must not be negative")
	}
	if c.ProbeTimeout == 0 {
		c.ProbeTimeout = 5 * time.Second
	}
	if c.Interval == 0 {
		c.Interval = 2 * time.Second
	}
	if c.MaxBackoff == 0 {
		c.MaxBackoff = 10 * time.Minute
	}
	return nil
}

type RetryConfig struct {
	// MaxRateLimitRetries 使用指针区分“未配置”和显式禁用重试（0）。
	MaxRateLimitRetries *int          `yaml:"max_rate_limit_retries"`
	BaseDelay           time.Duration `yaml:"base_delay"`
	MaxDelay            time.Duration `yaml:"max_delay"`
}

// CandidateRetryConfig 覆盖单个候选的内部重试策略。未配置字段继承全局 retry；
// expose_as 入口只会按此策略重试当前候选，不会切换到父路由的其他候选。
type CandidateRetryConfig struct {
	Enabled    *bool         `yaml:"enabled"`
	MaxRetries *int          `yaml:"max_retries"`
	BaseDelay  time.Duration `yaml:"base_delay"`
	MaxDelay   time.Duration `yaml:"max_delay"`
}

type candidateRetryPolicy struct {
	MaxRetries int
	BaseDelay  time.Duration
	MaxDelay   time.Duration
	CustomWait bool
}

func (c CandidateRetryConfig) resolve(global RetryConfig) (candidateRetryPolicy, error) {
	policy := candidateRetryPolicy{
		MaxRetries: *global.MaxRateLimitRetries,
		BaseDelay:  global.BaseDelay,
		MaxDelay:   global.MaxDelay,
		CustomWait: c.BaseDelay != 0 || c.MaxDelay != 0,
	}
	if c.MaxRetries != nil {
		policy.MaxRetries = *c.MaxRetries
	}
	if c.BaseDelay != 0 {
		policy.BaseDelay = c.BaseDelay
	}
	if c.MaxDelay != 0 {
		policy.MaxDelay = c.MaxDelay
	}
	if c.Enabled != nil && !*c.Enabled {
		policy.MaxRetries = 0
	}
	if policy.MaxRetries < 0 {
		return policy, fmt.Errorf("max_retries must not be negative")
	}
	if c.BaseDelay < 0 || c.MaxDelay < 0 {
		return policy, fmt.Errorf("retry delays must not be negative")
	}
	if policy.MaxDelay < policy.BaseDelay {
		return policy, fmt.Errorf("max_delay must be at least base_delay")
	}
	return policy, nil
}

func (c *RetryConfig) normalize() error {
	if c.MaxRateLimitRetries == nil {
		defaultRetries := maxRateLimitRetries
		c.MaxRateLimitRetries = &defaultRetries
	}
	if *c.MaxRateLimitRetries < 0 {
		return fmt.Errorf("retry.max_rate_limit_retries must not be negative")
	}
	if c.BaseDelay == 0 {
		c.BaseDelay = time.Second
	}
	if c.MaxDelay == 0 {
		c.MaxDelay = 16 * time.Second
	}
	if c.BaseDelay < 0 || c.MaxDelay < 0 {
		return fmt.Errorf("retry delays must not be negative")
	}
	if c.MaxDelay < c.BaseDelay {
		return fmt.Errorf("retry.max_delay must be at least retry.base_delay")
	}
	return nil
}

type LoggingConfig struct {
	Language string `yaml:"language"`
	Level    string `yaml:"level"`
	Format   string `yaml:"format"`
	Color    *bool  `yaml:"color"`
}

func (l *LoggingConfig) normalize() error {
	switch strings.ToLower(strings.TrimSpace(l.Language)) {
	case "", "en":
		l.Language = "en"
	case "zh", "zh-cn":
		l.Language = "zh-CN"
	default:
		return fmt.Errorf("unsupported log language %q (supported: en, zh-CN)", l.Language)
	}
	l.Level = strings.ToUpper(strings.TrimSpace(l.Level))
	if l.Level == "" {
		l.Level = "INFO"
	}
	switch l.Level {
	case "DEBUG", "INFO", "WARN", "ERROR":
	default:
		return fmt.Errorf("unsupported log level %q (supported: DEBUG, INFO, WARN, ERROR)", l.Level)
	}
	l.Format = strings.ToLower(strings.TrimSpace(l.Format))
	if l.Format == "" {
		l.Format = "text"
	}
	if l.Format != "text" && l.Format != "json" {
		return fmt.Errorf("unsupported log format %q (supported: text, json)", l.Format)
	}
	if l.Color == nil {
		enabled := true
		l.Color = &enabled
	}
	return nil
}

func (l LoggingConfig) ColorEnabled() bool {
	return l.Color == nil || *l.Color
}

// DatabaseConfig 保持 SQLite 字符串配置的兼容性，也支持 PostgreSQL。
// PostgreSQL 的连接串应通过 DSNEnv 提供，避免凭据进入 YAML。
type DatabaseConfig struct {
	Driver        string        `yaml:"driver"`
	DSN           string        `yaml:"dsn"`
	DSNEnv        string        `yaml:"dsn_env"`
	AsyncWrite    bool          `yaml:"async_write"`
	BatchSize     int           `yaml:"batch_size"`
	FlushInterval time.Duration `yaml:"flush_interval"`
	QueueSize     int           `yaml:"queue_size"`
}

func (d *DatabaseConfig) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag != "!!str" {
			return fmt.Errorf("database must be a string or mapping")
		}
		d.Driver = "sqlite"
		d.DSN = value.Value
		d.DSNEnv = ""
		return nil
	case yaml.MappingNode:
		type rawDatabaseConfig DatabaseConfig
		var raw rawDatabaseConfig
		if err := value.Decode(&raw); err != nil {
			return err
		}
		*d = DatabaseConfig(raw)
		return nil
	default:
		return fmt.Errorf("database must be a string or mapping")
	}
}

func (d *DatabaseConfig) normalize() error {
	d.Driver = strings.ToLower(strings.TrimSpace(d.Driver))
	switch d.Driver {
	case "", "sqlite", "sqlite3":
		d.Driver = "sqlite"
		if d.DSN == "" && d.DSNEnv == "" {
			d.DSN = "nanorouter.db"
		}
	case "postgres", "postgresql", "pg":
		d.Driver = "postgres"
		if d.DSN == "" && d.DSNEnv == "" {
			return fmt.Errorf("postgres database requires dsn or dsn_env")
		}
	default:
		return fmt.Errorf("unsupported database driver %q", d.Driver)
	}
	if d.DSN != "" && d.DSNEnv != "" {
		return fmt.Errorf("database may set only one of dsn or dsn_env")
	}
	if d.BatchSize < 0 || d.FlushInterval < 0 || d.QueueSize < 0 {
		return fmt.Errorf("database async write settings must not be negative")
	}
	if d.AsyncWrite {
		if d.Driver != "sqlite" {
			return fmt.Errorf("database.async_write is currently supported only for sqlite")
		}
		if d.BatchSize == 0 {
			d.BatchSize = 100
		}
		if d.FlushInterval == 0 {
			d.FlushInterval = 100 * time.Millisecond
		}
		if d.QueueSize == 0 {
			d.QueueSize = 4096
		}
		if d.QueueSize < d.BatchSize {
			return fmt.Errorf("database.queue_size must be at least database.batch_size")
		}
	}
	return nil
}

// SQLDriver 是 database/sql 注册的驱动名称。
func (d DatabaseConfig) SQLDriver() string {
	if d.Driver == "postgres" {
		return "postgres"
	}
	return "sqlite3"
}

func (d DatabaseConfig) ResolveDSN() (string, error) {
	if d.DSNEnv == "" {
		return d.DSN, nil
	}
	if dsn := os.Getenv(d.DSNEnv); dsn != "" {
		return dsn, nil
	}
	return "", fmt.Errorf("database dsn environment variable %q is empty", d.DSNEnv)
}

// Route 的名称就是下游填写的 model，例如 free-fast；真实模型 ID 不暴露给下游。
type Route struct {
	Strategy    string            `yaml:"strategy"`
	RetryBudget RetryBudgetConfig `yaml:"retry_budget"`
	Candidates  []Candidate       `yaml:"candidates"`
}

// RetryBudgetConfig 用令牌预算限制故障切换带来的请求放大。每个初始请求补充 Ratio 个令牌，
// 每次真实的额外上游尝试消耗一个令牌，Burst 是可累积上限。
type RetryBudgetConfig struct {
	Enabled bool    `yaml:"enabled"`
	Ratio   float64 `yaml:"ratio"`
	Burst   int     `yaml:"burst"`
}

func (c *RetryBudgetConfig) normalize(route string) error {
	if c.Ratio < 0 || c.Ratio > 1 {
		return fmt.Errorf("route %q retry_budget.ratio must be between 0 and 1", route)
	}
	if c.Burst < 0 {
		return fmt.Errorf("route %q retry_budget.burst must not be negative", route)
	}
	if !c.Enabled {
		if c.Ratio != 0 || c.Burst != 0 {
			return fmt.Errorf("route %q retry_budget settings require enabled: true", route)
		}
		return nil
	}
	if c.Ratio == 0 {
		c.Ratio = 0.1
	}
	if c.Burst == 0 {
		c.Burst = 10
	}
	return nil
}

type Candidate struct {
	ID        string `yaml:"id"`
	BaseURL   string `yaml:"base_url"`
	APIKeyEnv string `yaml:"api_key_env"`
	Model     string `yaml:"model"`
	// UserAgent 覆盖转发到上游的 User-Agent。留空时使用 Go 默认 Go-http-client/x.x；
	// 部分上游（如 Nous Research 的 Cloudflare WAF）会拦截该默认 UA，可配置为
	// 常规客户端 UA（例如 opencode/1.18.25）绕过。
	UserAgent string `yaml:"user_agent"`
	// LlamaServer 表示上游是 llama.cpp 的 llama-server。此时 base_url 可填写服务根地址，
	// 网关会自动补上其 OpenAI 兼容 API 的 /v1 前缀。
	LlamaServer bool `yaml:"llama_server"`
	// ExposeAs 让候选仍属于父路由的故障切换池，同时以独立模型 ID 对下游暴露。
	// 例如 free-fast 的候选可设为 groq-qwen；下游指定该 ID 时只会调用此候选。
	ExposeAs  string `yaml:"expose_as"`
	Weight    int    `yaml:"weight"`
	Responses bool   `yaml:"responses"`
	// ResponsesViaChatCompletions 将下游 Responses 请求适配到仅支持 Chat Completions 的上游。
	// 仅应为已验证适配能力的供应商开启，例如 SenseNova。
	ResponsesViaChatCompletions bool `yaml:"responses_via_chat_completions"`
	// ChatMaxTokensField 用于 Responses 降级转换。OpenAI 兼容上游通常为 max_tokens（默认值）；
	// 仅在上游使用非标准字段名时配置，例如旧版 SenseNova 原生接口的 max_new_tokens。
	ChatMaxTokensField string `yaml:"chat_max_tokens_field"`
	// ChatCompletionsPath 覆盖默认 chat/completions 路径，用于少数非标准上游。
	ChatCompletionsPath string `yaml:"chat_completions_path"`
	// DropRequestFields / DropMessageFields 用于供应商兼容性清洗。
	// 仅删除已确认不支持且允许降级的字段，不应放入 model、messages 等必需字段。
	DropRequestFields []string `yaml:"drop_request_fields"`
	DropMessageFields []string `yaml:"drop_message_fields"`
	// MaxTokensLimit 将候选收到的最大输出 token 参数约束到 [1, limit]。
	// 该改写在选中候选后执行，不会改变故障切换时其他候选收到的原始值。
	MaxTokensLimit int `yaml:"max_tokens_limit"`
	// Enabled 未配置时默认为 true；设置为 false 会立即从路由候选中排除。
	Enabled *bool `yaml:"enabled"`
	// AggregateEnabled 控制候选是否参与父路由的轮转与故障切换。
	// 未配置时默认为 true；设为 false 后仍可通过 expose_as 直接访问。
	AggregateEnabled *bool `yaml:"aggregate_enabled"`
	// MaxConcurrency 限制此候选同时在途的请求数；0 表示不限制。
	MaxConcurrency int `yaml:"max_concurrency"`
	// QueueTimeout 是候选已满时等待空闲槽位的最长时间；0 表示不排队，立即拒绝。
	// 仅在 MaxConcurrency 大于 0 时生效。
	QueueTimeout time.Duration `yaml:"queue_timeout"`
	// AttemptTimeout 限制单次候选调用（包括完整 SSE）；0 表示仅受全局 request_timeout 限制。
	AttemptTimeout time.Duration `yaml:"attempt_timeout"`
	// AdaptiveConcurrency 在 [MinConcurrency, MaxConcurrency] 内按排队、TTFB 和 429 做 AIMD 调整。
	AdaptiveConcurrency bool          `yaml:"adaptive_concurrency"`
	MinConcurrency      int           `yaml:"min_concurrency"`
	TargetTTFB          time.Duration `yaml:"target_ttfb"`
	// Retry 覆盖全局内部重试参数；默认仅重试 429/502/503/504。
	Retry CandidateRetryConfig `yaml:"retry"`
	// SimulateOpenCode 开启对 OpenCode 客户端行为的全面仿真：
	// 1. 发送 User-Agent: opencode/1.18.30
	// 2. 注入 thinking: {"type": "enabled", "clear_thinking": false}
	// 3. 流式请求注入 stream_options: {"include_usage": true}
	// 4. 补全会话头 X-Session-Affinity 与 X-Session-Id
	SimulateOpenCode bool `yaml:"simulate_opencode"`
	// Anthropic 声明上游说 Anthropic Messages 协议（/v1/messages 形状），
	// 例如智谱 ZCode Coding Plan 网关。此类候选原生服务 /v1/messages 端点，
	// 并经 chat→Messages 适配服务 Chat Completions / Responses 下游。
	Anthropic bool `yaml:"anthropic"`
	// AnthropicMessagesPath 覆盖 Anthropic 候选的上游路径；默认 v1/messages。
	// simulate_zcode 且未配置本字段时，路径按 zcode_channel 取网关路径。
	AnthropicMessagesPath string `yaml:"anthropic_messages_path"`
	// SimulateZcode 开启对 ZCode 桌面客户端行为的全面仿真：
	// 1. 发送 User-Agent: ZCode/3.14.0 与完整 CLI 身份/平台指纹头
	// 2. anthropic-version: 2023-06-01 与 x-api-key + Authorization 双凭据头
	// 3. 会话 id 从对话首个 user turn 稳定派生，请求/追踪 id 每请求全新
	// 仅对 Anthropic 候选有意义。
	SimulateZcode bool `yaml:"simulate_zcode"`
	// ZcodeChannel 指定 ZCode Coding Plan 渠道：bigmodel（国内 ultra）或
	// zai（海外 ultra-zai）。路径与凭据文件的分组键都随渠道切换。
	ZcodeChannel string `yaml:"zcode_channel"`
	// ZcodeTokenFile 允许在 api_key_env 未命中时回退读取凭据文件：
	// 优先 ~/.nanorouter/zcode_token.json（login-zcode 写入），其次参考
	// 客户端的 ~/.zcode_coding_plan_token.json（只读）。凭据文件的分组
	// 键取 zcode_channel。
	ZcodeTokenFile bool `yaml:"zcode_token_file"`
	// Probe 显式控制该候选是否在熔断后开启后台异步探活；未配置时继承全局 health_check.enabled。
	Probe *bool `yaml:"probe"`
	// Headers 允许针对上游候选配置额外的自定义 HTTP 头（可覆盖默认头）。
	Headers map[string]string `yaml:"headers"`
}

func (c Candidate) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

func (c Candidate) IsAggregateEnabled() bool {
	return c.AggregateEnabled == nil || *c.AggregateEnabled
}

func (c Candidate) IsProbeEnabled(global HealthCheckConfig) bool {
	if c.Probe != nil {
		return *c.Probe
	}
	return global.Enabled
}

// validate 校验候选的独立配置项。LoadConfig 与 NewWithLogger 共用同一套规则，
// 避免两处维护导致规则漂移；调用方补充路由与候选 ID 的定位信息。
func (u Candidate) validate(global RetryConfig) error {
	if u.MaxConcurrency < 0 {
		return fmt.Errorf("negative max_concurrency")
	}
	if u.MaxTokensLimit < 0 {
		return fmt.Errorf("negative max_tokens_limit")
	}
	if _, err := u.Retry.resolve(global); err != nil {
		return fmt.Errorf("invalid retry settings: %w", err)
	}
	if u.QueueTimeout < 0 {
		return fmt.Errorf("negative queue_timeout")
	}
	if u.AttemptTimeout < 0 {
		return fmt.Errorf("negative attempt_timeout")
	}
	if u.AdaptiveConcurrency {
		if u.MaxConcurrency <= 0 {
			return fmt.Errorf("adaptive_concurrency requires max_concurrency")
		}
		minimum := u.MinConcurrency
		if minimum == 0 {
			minimum = 1
		}
		if minimum < 1 || minimum > u.MaxConcurrency {
			return fmt.Errorf("invalid min_concurrency")
		}
		if u.TargetTTFB < 0 {
			return fmt.Errorf("negative target_ttfb")
		}
	}
	if u.MaxConcurrency == 0 && u.QueueTimeout > 0 {
		return fmt.Errorf("queue_timeout requires max_concurrency")
	}
	if u.ZcodeChannel != "" && u.ZcodeChannel != ZcodeChannelBigmodel && u.ZcodeChannel != ZcodeChannelZai {
		return fmt.Errorf("zcode_channel must be %q or %q", ZcodeChannelBigmodel, ZcodeChannelZai)
	}
	if (u.SimulateZcode || u.ZcodeTokenFile || u.ZcodeChannel != "" || u.AnthropicMessagesPath != "") && !u.Anthropic {
		return fmt.Errorf("anthropic must be enabled for anthropic_messages_path / simulate_zcode / zcode_channel / zcode_token_file")
	}
	if u.Anthropic {
		if u.Responses {
			return fmt.Errorf("anthropic candidates serve Messages natively; responses: true does not apply")
		}
		if u.ResponsesViaChatCompletions {
			return fmt.Errorf("anthropic candidates adapt Responses through the chat chain; responses_via_chat_completions does not apply")
		}
		if u.SimulateOpenCode {
			return fmt.Errorf("simulate_opencode targets the OpenAI-dialect Zhipu gateway and conflicts with anthropic")
		}
		if u.ChatCompletionsPath != "" {
			return fmt.Errorf("chat_completions_path targets OpenAI Chat upstreams and is ignored by anthropic candidates; use anthropic_messages_path")
		}
		if u.ChatMaxTokensField != "" {
			return fmt.Errorf("chat_max_tokens_field targets OpenAI Chat upstreams and is ignored by anthropic candidates")
		}
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if err := c.Database.normalize(); err != nil {
		return c, err
	}
	if err := c.Logging.normalize(); err != nil {
		return c, err
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 90 * time.Second
	}
	if err := c.Retry.normalize(); err != nil {
		return c, err
	}
	if err := c.Backpressure.normalize(); err != nil {
		return c, err
	}
	if err := c.HealthCheck.normalize(); err != nil {
		return c, err
	}
	if len(c.Routes) == 0 {
		return c, fmt.Errorf("routes must not be empty")
	}
	modelNames := make(map[string]string, len(c.Routes))
	for name := range c.Routes {
		modelNames[name] = "route"
	}
	for name, r := range c.Routes {
		if err := r.RetryBudget.normalize(name); err != nil {
			return c, err
		}
		c.Routes[name] = r
		if len(r.Candidates) == 0 {
			return c, fmt.Errorf("route %q has no candidates", name)
		}
		for _, u := range r.Candidates {
			if u.ID == "" || u.BaseURL == "" || u.Model == "" {
				return c, fmt.Errorf("route %q has an incomplete candidate", name)
			}
			if err := u.validate(c.Retry); err != nil {
				return c, fmt.Errorf("candidate %q in route %q: %w", u.ID, name, err)
			}
			if u.ExposeAs == "" {
				continue
			}
			if owner, ok := modelNames[u.ExposeAs]; ok {
				return c, fmt.Errorf("exposed model %q in route %q conflicts with %s", u.ExposeAs, name, owner)
			}
			modelNames[u.ExposeAs] = "another exposed model"
		}
	}
	return c, nil
}
