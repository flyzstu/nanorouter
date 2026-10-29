package agybridge

// EventKind 是 agy stream-json 事件的大类。
type EventKind int

const (
	EventGarbage EventKind = iota
	EventInit
	EventStep
	EventResult
)

// StepKind 是 step_update 的语义分类。
type StepKind string

const (
	StepText      StepKind = "text"
	StepThinking  StepKind = "thinking"
	StepTool      StepKind = "tool"
	StepTitle     StepKind = "title"
	StepSubagent  StepKind = "subagent"
	StepUserInput StepKind = "user-input"
	StepUnknown   StepKind = "unknown"
)

// Usage 汇总 agy 返回的 token 计数；缺省字段保持 0。
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	ThinkingTokens   int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TotalTokens      int64
}

// ToolInfo 是 agy 工具步骤的可见信息。
type ToolInfo struct {
	Name   string
	Args   any
	Output any
	Error  string
}

// Event 是解析后的 agy 事件。只有相关字段会被填充。
type Event struct {
	Kind           EventKind
	ConversationID string
	Model          string

	StepKey  string
	StepKind StepKind
	Text     string
	Fragment bool
	State    string
	Tool     *ToolInfo

	Usage *Usage

	OK       bool
	Response string
	Error    string

	Raw map[string]any
}
