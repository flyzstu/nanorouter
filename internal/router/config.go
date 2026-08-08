package router

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen         string             `yaml:"listen"`
	Database       DatabaseConfig     `yaml:"database"`
	Logging        LoggingConfig      `yaml:"logging"`
	AuthEnv        string             `yaml:"auth_env"`
	RequestTimeout time.Duration      `yaml:"request_timeout"`
	Retry          RetryConfig        `yaml:"retry"`
	Backpressure   BackpressureConfig `yaml:"backpressure"`
	Profiling      ProfilingConfig    `yaml:"profiling"`
	Routes         map[string]Route   `yaml:"routes"`
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
}

func (c Candidate) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

func (c Candidate) IsAggregateEnabled() bool {
	return c.AggregateEnabled == nil || *c.AggregateEnabled
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
