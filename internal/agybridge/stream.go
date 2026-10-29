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

	emitTools bool
}

func newStreamCollector(emitTools bool) *streamCollector {
	return &streamCollector{
		stepText:     map[string]string{},
		stepThinking: map[string]string{},
		toolSeen:     map[string]bool{},
		emitTools:    emitTools,
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
			if c.emitTools && ev.Tool != nil && !c.toolSeen[ev.StepKey] {
				c.toolSeen[ev.StepKey] = true
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
	if c.final != nil {
		return *c.final
	}
	return c.stepUsage
}

func (c *streamCollector) textString() string     { return c.text.String() }
func (c *streamCollector) thinkingString() string { return c.thinking.String() }

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
