package agybridge

import (
	"strings"
	"testing"
)

func TestStreamParserFullRun(t *testing.T) {
	input := strings.Join([]string{
		`{"event":"init","conversation_id":"c1","model":"gemini-3-6-flash"}`,
		`{"event":"step_update","idx":1,"step_type":"thinking","text":"Think"}`,
		`{"event":"step_update","idx":3,"step_type":"text","text":"Hi"}`,
		`{"event":"result","result":{"conversation_id":"c1","status":"DONE","response":"Hi","usage":{"input_tokens":3,"output_tokens":2}}}`,
	}, "\n") + "\n"

	parser := &StreamParser{}
	events := parser.Feed([]byte(input))
	if len(events) != 4 {
		t.Fatalf("events=%d want 4", len(events))
	}
	if events[0].Kind != EventInit || events[0].ConversationID != "c1" || events[0].Model != "gemini-3-6-flash" {
		t.Fatalf("init=%+v", events[0])
	}
	if events[1].StepKind != StepThinking || events[1].Text != "Think" {
		t.Fatalf("thinking=%+v", events[1])
	}
	if events[2].StepKind != StepText || events[2].Text != "Hi" {
		t.Fatalf("text=%+v", events[2])
	}
	if events[3].Kind != EventResult || !events[3].OK || events[3].Usage == nil {
		t.Fatalf("result=%+v", events[3])
	}
	if events[3].Usage.InputTokens != 3 || events[3].Usage.OutputTokens != 2 {
		t.Fatalf("usage=%+v", events[3].Usage)
	}
}

func TestStreamParserNestedStepUpdateAndDelta(t *testing.T) {
	input := strings.Join([]string{
		`{"event":"init","conversation_id":"c15","init":{"model":"gemini-3-7-flash"}}`,
		`{"event":"step_update","step_update":{"step_index":5,"state":"ACTIVE","step_type":"agent_response","text_delta":"There are "}}`,
		`{"event":"step_update","step_update":{"step_index":5,"state":"DONE","step_type":"agent_response","text_delta":"five"}}`,
		`{"event":"step_update","step_update":{"step_index":6,"state":"DONE","step_type":"tool","tool_info":{"name":"run_command","parameters":{"CommandLine":"ls"},"output":"ok"}}}`,
		`{"event":"result","result":{"conversation_id":"c15","status":"DONE","response":"There are five","usage":{"total_tokens":9}}}`,
	}, "\n") + "\n"

	parser := &StreamParser{}
	events := parser.Feed([]byte(input))
	if len(events) != 5 {
		t.Fatalf("events=%d want 5", len(events))
	}
	if events[0].Model != "gemini-3-7-flash" {
		t.Fatalf("model=%q", events[0].Model)
	}
	if events[1].StepKind != StepText || !events[1].Fragment || events[1].Text != "There are " {
		t.Fatalf("delta1=%+v", events[1])
	}
	if events[2].Text != "five" {
		t.Fatalf("delta2=%+v", events[2])
	}
	if events[3].StepKind != StepTool || events[3].Tool == nil || events[3].Tool.Name != "run_command" {
		t.Fatalf("tool=%+v", events[3])
	}
	if events[4].Usage == nil || events[4].Usage.TotalTokens != 9 {
		t.Fatalf("usage=%+v", events[4].Usage)
	}
}

func TestStreamParserTornChunksAndFlush(t *testing.T) {
	input := `{"event":"init","conversation_id":"c9"}`
	parser := &StreamParser{}
	if events := parser.Feed([]byte(input[:10])); len(events) != 0 {
		t.Fatalf("partial events=%d", len(events))
	}
	events := parser.Feed([]byte(input[10:]))
	events = append(events, parser.Flush()...)
	if len(events) != 1 || events[0].ConversationID != "c9" {
		t.Fatalf("events=%+v", events)
	}
	if tail := parser.Flush(); len(tail) != 0 {
		t.Fatalf("tail=%+v", tail)
	}
}

func TestStreamParserGarbageAndAuthFailure(t *testing.T) {
	parser := &StreamParser{}
	events := parser.Feed([]byte("⚠ noise line\nPlease sign in\n" +
		`{"event":"result","result":{"status":"ERROR","error":"authentication failed or timed out"}}` + "\n"))
	if len(events) != 3 {
		t.Fatalf("events=%d", len(events))
	}
	if !parser.AuthFailure || parser.AuthFailureText == "" {
		t.Fatalf("auth failure not captured: %+v", parser)
	}
	if events[2].Kind != EventResult || events[2].OK {
		t.Fatalf("result=%+v", events[2])
	}
}

func TestCollectorSnapshotSuffix(t *testing.T) {
	collector := newStreamCollector("annotate")
	var text strings.Builder
	for _, ev := range []Event{
		{Kind: EventStep, StepKey: "1", StepKind: StepText, Text: "Hello"},
		{Kind: EventStep, StepKey: "1", StepKind: StepText, Text: "Hello world"},
		{Kind: EventResult, OK: true, Response: "Hello world", Usage: &Usage{InputTokens: 2, OutputTokens: 3}},
	} {
		delta, _ := collector.consume(ev)
		text.WriteString(delta)
	}
	if got := text.String(); got != "Hello world" {
		t.Fatalf("text=%q", got)
	}
	if collector.textString() != "Hello world" {
		t.Fatalf("collected=%q", collector.textString())
	}
	usage := collector.usage()
	if usage.InputTokens != 2 || usage.OutputTokens != 3 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestCollectorPrefersStepUsageOverCumulativeResult(t *testing.T) {
	collector := newStreamCollector("annotate")
	collector.consume(Event{Kind: EventStep, StepKey: "1", StepKind: StepText, Text: "ok", Usage: &Usage{
		InputTokens: 100, OutputTokens: 5, TotalTokens: 105,
	}})
	collector.consume(Event{Kind: EventResult, OK: true, Response: "ok", Usage: &Usage{
		InputTokens: 500, OutputTokens: 10, TotalTokens: 510,
	}})
	usage := collector.usage()
	if usage.InputTokens != 100 || usage.OutputTokens != 5 || usage.TotalTokens != 105 {
		t.Fatalf("usage=%+v; want per-step 100/5/105", usage)
	}
}
