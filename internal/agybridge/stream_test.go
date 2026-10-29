package agybridge

import (
	"strings"
	"testing"
)

func TestCollectorThinkingSnapshotAndSubagent(t *testing.T) {
	collector := newStreamCollector("annotate")
	_, first := collector.consume(Event{Kind: EventStep, StepKey: "1", StepKind: StepThinking, Text: "Th"})
	_, second := collector.consume(Event{Kind: EventStep, StepKey: "1", StepKind: StepThinking, Text: "Think"})
	_, third := collector.consume(Event{Kind: EventStep, StepKey: "2", StepKind: StepSubagent, Text: "sub"})
	if first != "Th" || second != "ink" || third != "sub" {
		t.Fatalf("deltas=%q,%q,%q", first, second, third)
	}
	if collector.thinkingString() != "Thinksub" {
		t.Fatalf("thinking=%q", collector.thinkingString())
	}
}

func TestCollectorToolModes(t *testing.T) {
	active := Event{Kind: EventStep, StepKey: "1", StepKind: StepTool, State: "ACTIVE",
		Tool: &ToolInfo{Name: "run_command"}}
	done := Event{Kind: EventStep, StepKey: "1", StepKind: StepTool, State: "DONE",
		Tool: &ToolInfo{Name: "run_command", Args: map[string]any{"CommandLine": "ls"}, Output: "ok"}}

	annotate := newStreamCollector("annotate")
	annotate.consume(active)
	if text, thinking := annotate.consume(done); text != "" || !strings.Contains(thinking, "[tool] run_command") {
		t.Fatalf("annotate text=%q thinking=%q", text, thinking)
	}
	if len(annotate.toolEvents()) != 0 {
		t.Fatalf("annotate should not emit tool calls")
	}

	passthrough := newStreamCollector("passthrough")
	passthrough.consume(active)
	passthrough.consume(done)
	passthrough.consume(done)
	tools := passthrough.toolEvents()
	if len(tools) != 1 || tools[0].Name != "run_command" {
		t.Fatalf("passthrough tools=%+v", tools)
	}

	none := newStreamCollector("none")
	none.consume(done)
	if len(none.toolEvents()) != 0 || none.thinkingString() != "" {
		t.Fatalf("none should ignore tools")
	}

	annotateError := newStreamCollector("annotate")
	if _, thinking := annotateError.consume(Event{Kind: EventStep, StepKey: "2", StepKind: StepTool,
		State: "ERROR", Tool: &ToolInfo{Name: "run_command", Error: "boom"}}); !strings.Contains(thinking, "failed: boom") {
		t.Fatalf("error annotation=%q", thinking)
	}
}

func TestCollectorResultErrorAnnotation(t *testing.T) {
	collector := newStreamCollector("annotate")
	collector.consume(Event{Kind: EventStep, StepKey: "1", StepKind: StepText, Text: "ok"})
	_, thinking := collector.consume(Event{Kind: EventResult, OK: false, Response: "ok", Error: "tool failed"})
	if !strings.Contains(thinking, "[agy error] tool failed") {
		t.Fatalf("thinking=%q", thinking)
	}
	if collector.textString() != "ok" {
		t.Fatalf("text=%q", collector.textString())
	}
}

func TestCollectorResultResponseOnlyWhenNoText(t *testing.T) {
	collector := newStreamCollector("annotate")
	collector.consume(Event{Kind: EventStep, StepKey: "1", StepKind: StepText, Text: "hi"})
	if text, _ := collector.consume(Event{Kind: EventResult, OK: true, Response: "hello"}); text != "" {
		t.Fatalf("unexpected duplicate response delta %q", text)
	}
	if collector.textString() != "hi" {
		t.Fatalf("text=%q", collector.textString())
	}

	empty := newStreamCollector("annotate")
	if text, _ := empty.consume(Event{Kind: EventResult, OK: true, Response: "hello"}); text != "hello" {
		t.Fatalf("response fallback=%q", text)
	}
}

func TestCollectorUsageFillsMissingFields(t *testing.T) {
	collector := newStreamCollector("annotate")
	collector.consume(Event{Kind: EventStep, StepKey: "1", StepKind: StepText, Text: "ok",
		Usage: &Usage{InputTokens: 100}})
	collector.consume(Event{Kind: EventResult, OK: true, Usage: &Usage{InputTokens: 500, OutputTokens: 5, TotalTokens: 505}})
	usage := collector.usage()
	if usage.InputTokens != 100 || usage.OutputTokens != 5 || usage.TotalTokens != 105 {
		t.Fatalf("usage=%+v", usage)
	}
}
