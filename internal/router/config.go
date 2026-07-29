package router

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen         string           `yaml:"listen"`
	Database       DatabaseConfig   `yaml:"database"`
	Logging        LoggingConfig    `yaml:"logging"`
	AuthEnv        string           `yaml:"auth_env"`
	RequestTimeout time.Duration    `yaml:"request_timeout"`
	Routes         map[string]Route `yaml:"routes"`
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
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
	DSNEnv string `yaml:"dsn_env"`
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
	Strategy   string      `yaml:"strategy"`
	Candidates []Candidate `yaml:"candidates"`
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
}

func (c Candidate) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

func (c Candidate) IsAggregateEnabled() bool {
	return c.AggregateEnabled == nil || *c.AggregateEnabled
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
	if len(c.Routes) == 0 {
		return c, fmt.Errorf("routes must not be empty")
	}
	modelNames := make(map[string]string, len(c.Routes))
	for name := range c.Routes {
		modelNames[name] = "route"
	}
	for name, r := range c.Routes {
		if len(r.Candidates) == 0 {
			return c, fmt.Errorf("route %q has no candidates", name)
		}
		for _, u := range r.Candidates {
			if u.ID == "" || u.BaseURL == "" || u.Model == "" {
				return c, fmt.Errorf("route %q has an incomplete candidate", name)
			}
			if u.MaxConcurrency < 0 {
				return c, fmt.Errorf("candidate %q in route %q has negative max_concurrency", u.ID, name)
			}
			if u.MaxTokensLimit < 0 {
				return c, fmt.Errorf("candidate %q in route %q has negative max_tokens_limit", u.ID, name)
			}
			if u.QueueTimeout < 0 {
				return c, fmt.Errorf("candidate %q in route %q has negative queue_timeout", u.ID, name)
			}
			if u.MaxConcurrency == 0 && u.QueueTimeout > 0 {
				return c, fmt.Errorf("candidate %q in route %q sets queue_timeout without max_concurrency", u.ID, name)
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
