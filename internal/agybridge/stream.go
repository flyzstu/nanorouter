package agybridge

import (
	"strings"
)

// streamCollector 把 agy 事件归并成文本/思考/用量，并负责快照到增量的转换。
type streamCollector struct {
	text     strings.Builder
	thinking strings.Builder

	stepText     map[string]string
	stepThinking map[string]string
	toolSeen     map[string]bool

	stepUsage Usage
	final     *Usage
	model     string
	convID    string
	resultErr string

	toolMode string
	tools    []ToolEvent
}

// ToolEvent 是 completed agy 工具步骤的归一化结果。
type ToolEvent struct {
	Name   string
	Input  any
	Output any
	Error  string
}

func newStreamCollector(toolMode string) *streamCollector {
	return &streamCollector{
		stepText:     map[string]string{},
		stepThinking: map[string]string{},
		toolSeen:     map[string]bool{},
		toolMode:     toolMode,
	}
}

// consume 处理一个事件，返回需要立刻写出的文本增量和思考增量。
func (c *streamCollector) consume(ev Event) (string, string) {
	switch ev.Kind {
	case EventInit:
		if ev.Model != "" {
			c.model = ev.Model
		}
		if ev.ConversationID != "" {
			c.convID = ev.ConversationID
		}
	case EventStep:
		if ev.Usage != nil {
			c.mergeUsage(ev.Usage)
		}
		switch ev.StepKind {
		case StepText:
			delta := c.deltaFor(c.stepText, ev)
			if delta != "" {
				c.text.WriteString(delta)
			}
			return delta, ""
		case StepThinking, StepSubagent:
			delta := c.deltaFor(c.stepThinking, ev)
			if delta != "" {
				c.thinking.WriteString(delta)
			}
			return "", delta
		case StepTool:
			if ev.Tool == nil || c.toolSeen[ev.StepKey] {
				break
			}
			completed := ev.State == "DONE" || ev.State == "ERROR" ||
				(ev.State == "" && (ev.Tool.Output != nil || ev.Tool.Error != ""))
			if !completed {
				break
			}
			c.toolSeen[ev.StepKey] = true
			switch c.toolMode {
			case "none":
				// 不输出工具事件。
			case "passthrough":
				c.tools = append(c.tools, ToolEvent{
					Name: ev.Tool.Name, Input: ev.Tool.Args,
					Output: ev.Tool.Output, Error: ev.Tool.Error,
				})
			default:
				line := "[tool] " + ev.Tool.Name + "\n"
				if ev.Tool.Error != "" {
					line = "[tool] " + ev.Tool.Name + " failed: " + ev.Tool.Error + "\n"
				}
				c.thinking.WriteString(line)
				return "", line
			}
		}
	case EventResult:
		if ev.ConversationID != "" {
			c.convID = ev.ConversationID
		}
		if ev.Usage != nil {
			c.final = ev.Usage
		}
		if ev.Error != "" {
			c.resultErr = ev.Error
		}
		if ev.Response != "" && c.text.Len() == 0 {
			c.text.WriteString(ev.Response)
			return ev.Response, ""
		}
		if !ev.OK && ev.Error != "" {
			line := "[agy error] " + ev.Error + "\n"
			c.thinking.WriteString(line)
			return "", line
		}
	}
	return "", ""
}

func (c *streamCollector) deltaFor(seen map[string]string, ev Event) string {
	prev := seen[ev.StepKey]
	if ev.Fragment {
		seen[ev.StepKey] = prev + ev.Text
		return ev.Text
	}
	delta := suffixDelta(prev, ev.Text)
	seen[ev.StepKey] = ev.Text
	return delta
}

func (c *streamCollector) mergeUsage(u *Usage) {
	if u == nil {
		return
	}
	c.stepUsage.InputTokens = maxInt64(c.stepUsage.InputTokens, u.InputTokens)
	c.stepUsage.OutputTokens = maxInt64(c.stepUsage.OutputTokens, u.OutputTokens)
	c.stepUsage.ThinkingTokens = maxInt64(c.stepUsage.ThinkingTokens, u.ThinkingTokens)
	c.stepUsage.CacheReadTokens = maxInt64(c.stepUsage.CacheReadTokens, u.CacheReadTokens)
	c.stepUsage.CacheWriteTokens = maxInt64(c.stepUsage.CacheWriteTokens, u.CacheWriteTokens)
	c.stepUsage.TotalTokens = maxInt64(c.stepUsage.TotalTokens, u.TotalTokens)
}

func (c *streamCollector) usage() Usage {
	u := c.stepUsage
	if c.final != nil {
		// result 信封的 input/total 在续聊里是累计值，不能直接当本轮用量；
		// 只用它补齐 step 没给的字段。
		if u.InputTokens == 0 {
			u.InputTokens = c.final.InputTokens
		}
		if u.OutputTokens == 0 {
			u.OutputTokens = c.final.OutputTokens
		}
		if u.ThinkingTokens == 0 {
			u.ThinkingTokens = c.final.ThinkingTokens
		}
		if u.CacheReadTokens == 0 {
			u.CacheReadTokens = c.final.CacheReadTokens
		}
		if u.CacheWriteTokens == 0 {
			u.CacheWriteTokens = c.final.CacheWriteTokens
		}
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
	return u
}

func (c *streamCollector) textString() string      { return c.text.String() }
func (c *streamCollector) toolEvents() []ToolEvent { return c.tools }
func (c *streamCollector) thinkingString() string  { return c.thinking.String() }

func suffixDelta(prev, next string) string {
	if next == prev {
		return ""
	}
	if prev == "" {
		return next
	}
	if strings.HasPrefix(next, prev) {
		return next[len(prev):]
	}
	return "\n" + next
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
