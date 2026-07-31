package agent

import (
	"context"
	"strings"
	"testing"
)

func TestCursorAgent_BuildArgsUsesManagedNativeContract(t *testing.T) {
	a := &cursorAgent{
		extraArgs: []string{
			"--model", "claude-sonnet-5",
			"--workspace", "/unsafe",
			"--add-dir=/other",
			"--force",
		},
		model: "claude-opus-5[context=1m,effort=high,fast=false]",
	}

	args := a.buildArgs("/tmp/clean", "/repo/worktree", "session-123")
	want := []string{
		"--force",
		"--model", "claude-opus-5[context=1m,effort=high,fast=false]",
		"-p",
		"--output-format", "stream-json",
		"--workspace", "/tmp/clean",
		"--add-dir", "/repo/worktree",
		"--resume", "session-123",
		"--trust",
	}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("buildArgs() = %q, want %q", args, want)
	}
}

func TestParseCursorEvents_UsesTerminalResultAndCamelCaseUsage(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"session-1","model":"GPT-5.6 Sol"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]},"session_id":"session-1"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"session-1","usage":{"inputTokens":120,"outputTokens":30,"cacheReadTokens":80,"cacheWriteTokens":5}}`,
		"",
	}, "\n")

	parsed, err := parseCursorEvents(context.Background(), strings.NewReader(events), nil, nil)
	if err != nil {
		t.Fatalf("parseCursorEvents() error = %v", err)
	}
	if parsed == nil || parsed.Text != "done" || parsed.SessionID != "session-1" || parsed.Model != "GPT-5.6 Sol" {
		t.Fatalf("parsed result = %+v", parsed)
	}
	wantUsage := TokenUsage{InputTokens: 120, OutputTokens: 30, CacheReadTokens: 80, CacheCreationTokens: 5, Reported: true, CacheCreationReported: true}
	if parsed.Usage != wantUsage {
		t.Fatalf("usage = %+v, want %+v", parsed.Usage, wantUsage)
	}
}

func TestParseCursorEvents_ExtractsBoundedActivityMetrics(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"checking"}]}}`,
		`{"type":"tool_call","subtype":"started","call_id":"read-1","tool_call":{"readToolCall":{"args":{"path":"/repo/file.go"}}}}`,
		`{"type":"tool_call","subtype":"completed","call_id":"read-1","tool_call":{"readToolCall":{"result":{"success":{"content":"ignored"}}}}}`,
		`{"type":"result","subtype":"success","result":"done"}`,
		"",
	}, "\n")
	metrics := newCursorMetricsAccumulator()

	if _, err := parseCursorEvents(context.Background(), strings.NewReader(events), nil, metrics.onEvent); err != nil {
		t.Fatalf("parseCursorEvents() error = %v", err)
	}
	got := metrics.metrics()
	if got.ModelRoundtrips != 2 || got.ToolCalls != 1 || got.ToolCategories.Read != 1 {
		t.Fatalf("metrics = %+v, want two roundtrips and one read", got)
	}
}

func TestCursorAgent_ReportsAgentAttempts(t *testing.T) {
	a := &cursorAgent{bin: "cursor-agent"}
	if !a.ReportsAgentAttempts() {
		t.Fatal("Cursor must report each retry attempt")
	}
}

func TestCursorAgent_ContainmentPromptTargetsAbsoluteRepo(t *testing.T) {
	prompt := cursorContainedPrompt("review the branch", "/repo/worktree")
	for _, want := range []string{"/repo/worktree", "git -C", "review the branch"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("contained prompt missing %q:\n%s", want, prompt)
		}
	}
}
