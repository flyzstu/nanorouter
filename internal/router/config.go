package router

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen         string           `yaml:"listen"`
	Database       string           `yaml:"database"`
	AuthEnv        string           `yaml:"auth_env"`
	RequestTimeout time.Duration    `yaml:"request_timeout"`
	Routes         map[string]Route `yaml:"routes"`
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
	// ChatMaxTokensField 用于 Responses 降级转换。OpenAI 兼容上游通常为 max_tokens，
	// SenseNova 原生接口使用 max_new_tokens。
	ChatMaxTokensField string `yaml:"chat_max_tokens_field"`
	// ChatCompletionsPath 覆盖默认 chat/completions 路径，用于少数非标准上游。
	ChatCompletionsPath string `yaml:"chat_completions_path"`
	// DropRequestFields / DropMessageFields 用于供应商兼容性清洗。
	// 仅删除已确认不支持且允许降级的字段，不应放入 model、messages 等必需字段。
	DropRequestFields []string `yaml:"drop_request_fields"`
	DropMessageFields []string `yaml:"drop_message_fields"`
	// Enabled 未配置时默认为 true；设置为 false 会立即从路由候选中排除。
	Enabled *bool `yaml:"enabled"`
	// MaxConcurrency 限制此候选同时在途的请求数；0 表示不限制。
	MaxConcurrency int `yaml:"max_concurrency"`
	// QueueTimeout 是候选已满时等待空闲槽位的最长时间；0 表示不排队，立即拒绝。
	// 仅在 MaxConcurrency 大于 0 时生效。
	QueueTimeout time.Duration `yaml:"queue_timeout"`
}

func (c Candidate) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }

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
	if c.Database == "" {
		c.Database = "nanorouter.db"
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
