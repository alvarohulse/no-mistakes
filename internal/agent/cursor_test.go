package agent

import (
	"context"
	"os"
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
	wantUsage := TokenUsage{
		InputTokens: 120, OutputTokens: 30, CacheReadTokens: 80, CacheCreationTokens: 5,
		Reported: true, MeterPresenceReported: true, InputReported: true, OutputReported: true,
		CacheReadReported: true, CacheCreationReported: true,
	}
	if parsed.Usage != wantUsage {
		t.Fatalf("usage = %+v, want %+v", parsed.Usage, wantUsage)
	}
}

func TestParseCursorEvents_PreservesMissingCacheMeters(t *testing.T) {
	events := `{"type":"result","subtype":"success","result":"done","usage":{"inputTokens":120,"outputTokens":30}}
`
	parsed, err := parseCursorEvents(context.Background(), strings.NewReader(events), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Usage.InputReported || !parsed.Usage.OutputReported || parsed.Usage.CacheReadReported || parsed.Usage.CacheCreationReported {
		t.Fatalf("usage presence = %+v", parsed.Usage)
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

func TestCursorAgent_ExitFailureRetainsParsedUsage(t *testing.T) {
	t.Setenv("NM_CURSOR_FAILURE_HELPER", "1")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	agent := &cursorAgent{bin: executable, extraArgs: []string{"-test.run=^TestCursorFailureHelper$", "--"}}

	var result *Result
	_, err = agent.Run(context.Background(), RunOpts{
		Prompt: "review",
		CWD:    t.TempDir(),
		OnAttempt: func(attempt Attempt) {
			result = attempt.Result
		},
	})
	if err == nil {
		t.Fatal("expected cursor failure")
	}
	if result == nil || !result.UsageReported || result.Usage.InputTokens != 120 || result.Usage.OutputTokens != 30 || result.Usage.CacheReadTokens != 80 || result.Usage.CacheCreationTokens != 5 {
		t.Fatalf("partial result = %+v, want parsed usage from failed invocation", result)
	}
	if result.UsageCoverage != UsageCoverageUnknown {
		t.Fatalf("usage coverage = %q, want unknown", result.UsageCoverage)
	}
}

func TestCursorFailureHelper(t *testing.T) {
	if os.Getenv("NM_CURSOR_FAILURE_HELPER") == "" {
		return
	}
	_, _ = os.Stdout.WriteString(`{"type":"system","subtype":"init","session_id":"session-1","model":"cursor-model"}` + "\n")
	_, _ = os.Stdout.WriteString(`{"type":"result","subtype":"success","is_error":false,"result":"partial","session_id":"session-1","usage":{"inputTokens":120,"outputTokens":30,"cacheReadTokens":80,"cacheWriteTokens":5}}` + "\n")
	os.Exit(1)
}

func TestCursorAgent_ContainmentPromptTargetsAbsoluteRepo(t *testing.T) {
	prompt := cursorContainedPrompt("review the branch", "/repo/worktree")
	for _, want := range []string{"/repo/worktree", "git -C", "review the branch"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("contained prompt missing %q:\n%s", want, prompt)
		}
	}
}
