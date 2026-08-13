package agent

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordedClaudeStreamTelemetry(t *testing.T) {
	stream := recordedAgentFixture(t, "claude")
	var usage TokenUsage
	var result *claudeResult
	if err := parseClaudeEvents(context.Background(), strings.NewReader(stream), nil, &usage, &result); err != nil {
		t.Fatal(err)
	}
	if result == nil || result.model != "claude-opus-5" || result.nestedAgentCount != 0 || !result.agentObservationsReported {
		t.Fatalf("result = %+v", result)
	}
	if usage.InputTokens != 2 || usage.OutputTokens != 121 || usage.CacheReadTokens != 0 || usage.CacheCreationTokens != 35_241 {
		t.Fatalf("usage = %+v", usage)
	}
	if !usage.InputReported || !usage.OutputReported || !usage.CacheReadReported || !usage.CacheCreationReported {
		t.Fatalf("usage presence = %+v", usage)
	}
	if result.reportedCostUSD == nil || math.Abs(*result.reportedCostUSD-0.355445) > 0.0000001 {
		t.Fatalf("reported cost = %v", result.reportedCostUSD)
	}
}

func TestRecordedCodexStreamTelemetry(t *testing.T) {
	stream := recordedAgentFixture(t, "codex")
	var usage TokenUsage
	var lastMessage, threadID string
	metrics := newCodexMetricsAccumulator()
	if err := parseCodexEvents(context.Background(), strings.NewReader(stream), nil, &usage, &lastMessage, nil, &threadID, metrics); err != nil {
		t.Fatal(err)
	}
	if threadID == "" || lastMessage == "" || metrics.nestedAgentCount() != 0 {
		t.Fatalf("thread/message/nested = %q/%q/%d", threadID, lastMessage, metrics.nestedAgentCount())
	}
	if usage.InputTokens != 14_028 || usage.OutputTokens != 25 || usage.CacheReadTokens != 0 || usage.CacheCreationTokens != 14_025 {
		t.Fatalf("usage = %+v", usage)
	}
	if !usage.InputReported || !usage.OutputReported || !usage.CacheReadReported || !usage.CacheCreationReported {
		t.Fatalf("usage presence = %+v", usage)
	}
}

func TestRecordedCursorStreamTelemetry(t *testing.T) {
	stream := recordedAgentFixture(t, "cursor")
	metrics := newCursorMetricsAccumulator()
	parsed, err := parseCursorEvents(context.Background(), strings.NewReader(stream), nil, metrics.onEvent)
	if err != nil {
		t.Fatal(err)
	}
	if parsed == nil || !parsed.Terminal || parsed.Model != "Opus 4.8 300K Extra High" {
		t.Fatalf("result = %+v", parsed)
	}
	if parsed.Usage.InputTokens != 4 || parsed.Usage.OutputTokens != 237 || parsed.Usage.CacheReadTokens != 40_305 || parsed.Usage.CacheCreationTokens != 40_445 {
		t.Fatalf("usage = %+v", parsed.Usage)
	}
	if !parsed.Usage.InputReported || !parsed.Usage.OutputReported || !parsed.Usage.CacheReadReported || !parsed.Usage.CacheCreationReported {
		t.Fatalf("usage presence = %+v", parsed.Usage)
	}
	if metrics.metrics().ToolCalls != 1 {
		t.Fatalf("metrics = %+v", metrics.metrics())
	}
}

func recordedAgentFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "e2e", "fixtures", name, "structured.jsonl")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}
