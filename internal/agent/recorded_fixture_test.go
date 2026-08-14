package agent

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var recordedFixtureUnscrubbedPatterns = map[string]*regexp.Regexp{
	"Linux home directory":     regexp.MustCompile(`/home/[^/\\\r\n"]+`),
	"macOS home directory":     regexp.MustCompile(`/Users/[^/\\\r\n"]+`),
	"Windows home directory":   regexp.MustCompile(`(?i)\b[A-Z]:[\\/]+Users[\\/]+[^/\\\r\n"]+`),
	"recording temporary path": regexp.MustCompile(`/tmp/record(?:claude|codex|cursor)-[^\s"/]+`),
	"UUID":                     regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`),
	"provider-generated ID":    regexp.MustCompile(`\b(?:msg|req|toolu)_[A-Za-z0-9_-]+`),
	"recording wall clock":     regexp.MustCompile(`20(?:2[4-9]|[3-9][0-9])-[0-9]{2}-[0-9]{2}T`),
	"millisecond wall clock":   regexp.MustCompile(`"(?:timestamp_ms|startedAtMs|completedAtMs)":"?1[0-9]{12}`),
	"second wall clock":        regexp.MustCompile(`"(?:resetsAt|overageResetsAt)":1[0-9]{9}`),
}

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

func TestRecordedAgentFixturesAreScrubbed(t *testing.T) {
	t.Parallel()
	for _, agentName := range []string{"claude", "codex", "cursor"} {
		paths, err := filepath.Glob(filepath.Join("..", "e2e", "fixtures", agentName, "*.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			contents, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for name, pattern := range recordedFixtureUnscrubbedPatterns {
				if match := pattern.Find(contents); match != nil {
					t.Errorf("%s contains %s %q", path, name, match)
				}
			}
		}
	}
}

func TestRecordedFixtureScrubPatternsRejectMacOSHomeDirectories(t *testing.T) {
	pattern := recordedFixtureUnscrubbedPatterns["macOS home directory"]
	if pattern == nil || !pattern.MatchString(`/Users/another-user/repos/project`) {
		t.Fatal("macOS home directory was not rejected")
	}
}

func TestRecordedFixtureScrubPatternsRejectLinuxHomeDirectories(t *testing.T) {
	pattern := recordedFixtureUnscrubbedPatterns["Linux home directory"]
	if pattern == nil || !pattern.MatchString(`/home/another-user/repos/project`) {
		t.Fatal("Linux home directory was not rejected")
	}
}

func TestRecordedFixtureScrubPatternsRejectWindowsHomeDirectories(t *testing.T) {
	pattern := recordedFixtureUnscrubbedPatterns["Windows home directory"]
	for _, path := range []string{
		`C:\Users\another-user\repos\project`,
		`D:/Users/another-user/repos/project`,
		`{"cwd":"C:\\Users\\another-user\\repos\\project"}`,
	} {
		if pattern == nil || !pattern.MatchString(path) {
			t.Errorf("Windows home directory %q was not rejected", path)
		}
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
