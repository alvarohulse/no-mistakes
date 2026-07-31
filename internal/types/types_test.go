package types

import (
	"encoding/json"
	"testing"
)

func TestParseRefreshStrategy(t *testing.T) {
	tests := []struct {
		value string
		want  RefreshStrategy
	}{
		{value: "", want: ""},
		{value: "rebase", want: RefreshStrategyRebase},
		{value: " MERGE ", want: RefreshStrategyMerge},
	}
	for _, tt := range tests {
		got, err := ParseRefreshStrategy(tt.value)
		if err != nil {
			t.Fatalf("ParseRefreshStrategy(%q) error = %v", tt.value, err)
		}
		if got != tt.want {
			t.Fatalf("ParseRefreshStrategy(%q) = %q, want %q", tt.value, got, tt.want)
		}
	}

	if _, err := ParseRefreshStrategy("reset"); err == nil {
		t.Fatal("ParseRefreshStrategy(reset) accepted an unsupported strategy")
	}
}

func TestRefreshStrategyDefaultsToRebase(t *testing.T) {
	if got := RefreshStrategy("").OrDefault(); got != RefreshStrategyRebase {
		t.Fatalf("empty strategy defaults to %q, want %q", got, RefreshStrategyRebase)
	}
}

func TestStepDisplayNameUsesRefreshStrategyWithoutChangingIdentity(t *testing.T) {
	tests := []struct {
		name     StepName
		strategy RefreshStrategy
		want     string
	}{
		{name: StepRefresh, strategy: RefreshStrategyRebase, want: "Rebase"},
		{name: StepRefresh, strategy: RefreshStrategyMerge, want: "Merge"},
		{name: StepRefresh, strategy: "", want: "Rebase"},
		{name: StepName("rebase"), strategy: RefreshStrategyMerge, want: "Merge"},
		{name: StepReview, strategy: RefreshStrategyMerge, want: "Review"},
		{name: StepBuild, strategy: RefreshStrategyMerge, want: "Build"},
		{name: StepPR, strategy: RefreshStrategyMerge, want: "PR"},
	}

	for _, tt := range tests {
		if got := tt.name.DisplayName(tt.strategy); got != tt.want {
			t.Errorf("%q.DisplayName(%q) = %q, want %q", tt.name, tt.strategy, got, tt.want)
		}
	}

	if got := StepName("rebase").Canonical(); got != StepRefresh {
		t.Fatalf("legacy rebase canonical identity = %q, want refresh", got)
	}
	if got := StepRefresh.Canonical(); got != StepRefresh {
		t.Fatalf("refresh canonical identity = %q, want refresh", got)
	}
}

func TestAllStepsOrder(t *testing.T) {
	steps := AllSteps()
	if len(steps) != 10 {
		t.Fatalf("expected 10 steps, got %d", len(steps))
	}

	expected := []StepName{StepIntent, StepRefresh, StepReview, StepBuild, StepTest, StepDocument, StepLint, StepPush, StepPR, StepCI}
	for i, s := range steps {
		if s != expected[i] {
			t.Errorf("step[%d] = %q, want %q", i, s, expected[i])
		}
	}
}

func TestStepNameOrder(t *testing.T) {
	tests := []struct {
		step StepName
		want int
	}{
		{StepIntent, 1},
		{StepRefresh, 2},
		{StepReview, 3},
		{StepBuild, 4},
		{StepTest, 5},
		{StepDocument, 6},
		{StepLint, 7},
		{StepPush, 8},
		{StepPR, 9},
		{StepCI, 10},
		{StepName("unknown"), 0},
	}

	for _, tt := range tests {
		if got := tt.step.Order(); got != tt.want {
			t.Errorf("%q.Order() = %d, want %d", tt.step, got, tt.want)
		}
	}
}

func TestStepNameUnmarshalJSON_LegacyBabysit(t *testing.T) {
	var step StepName
	if err := json.Unmarshal([]byte(`"babysit"`), &step); err != nil {
		t.Fatalf("unmarshal step name: %v", err)
	}
	if step != StepCI {
		t.Fatalf("step = %q, want %q", step, StepCI)
	}
}

func TestStepNameUnmarshalJSON_LegacyRebase(t *testing.T) {
	var step StepName
	if err := json.Unmarshal([]byte(`"rebase"`), &step); err != nil {
		t.Fatalf("unmarshal step name: %v", err)
	}
	if step != StepRefresh {
		t.Fatalf("step = %q, want refresh", step)
	}
}

func TestStepNameScan_LegacyRebase(t *testing.T) {
	var step StepName
	if err := step.Scan("rebase"); err != nil {
		t.Fatalf("scan step name: %v", err)
	}
	if step != StepRefresh {
		t.Fatalf("step = %q, want refresh", step)
	}
}

func TestStepNameValue_CanonicalizesLegacyRebase(t *testing.T) {
	value, err := StepName("rebase").Value()
	if err != nil {
		t.Fatalf("Value() error = %v", err)
	}
	if value != "refresh" {
		t.Fatalf("Value() = %q, want refresh", value)
	}
}

func TestRegisteredACPTargetFor(t *testing.T) {
	registered, ok := RegisteredACPTargetFor("cursor")
	if !ok {
		t.Fatal("cursor should be a registered ACP target")
	}
	if registered.LegacyArgsKey != AgentCursor {
		t.Fatalf("legacy args key = %q, want %q", registered.LegacyArgsKey, AgentCursor)
	}
	if registered.DefaultCommand != "cursor-agent acp" {
		t.Fatalf("default command = %q, want cursor-agent acp", registered.DefaultCommand)
	}
	if registered.DefaultCommandBinary() != "cursor-agent" {
		t.Fatalf("default command binary = %q, want cursor-agent", registered.DefaultCommandBinary())
	}

	targets := RegisteredACPTargets()
	if len(targets) != 1 {
		t.Fatalf("registered targets = %v, want only cursor", targets)
	}
	targets[0].Target = "mutated"
	registered, _ = RegisteredACPTargetFor("cursor")
	if registered.Target != "cursor" {
		t.Fatalf("RegisteredACPTargets should return a copy, target = %q", registered.Target)
	}
}

func TestACPTargetFor(t *testing.T) {
	tests := []struct {
		name       string
		agent      AgentName
		wantTarget string
		wantOK     bool
	}{
		{name: "native cursor", agent: AgentCursor, wantTarget: "", wantOK: false},
		{name: "explicit registered acp target", agent: "acp:cursor", wantTarget: "cursor", wantOK: true},
		{name: "explicit acp target", agent: "acp:gemini", wantTarget: "gemini", wantOK: true},
		{name: "native agent", agent: AgentClaude, wantTarget: "", wantOK: false},
		{name: "empty target", agent: "acp:", wantTarget: "", wantOK: false},
		{name: "whitespace in target", agent: "acp:foo bar", wantTarget: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, ok := ACPTargetFor(tt.agent)
			if target != tt.wantTarget || ok != tt.wantOK {
				t.Fatalf("ACPTargetFor(%q) = (%q, %v), want (%q, %v)", tt.agent, target, ok, tt.wantTarget, tt.wantOK)
			}
		})
	}
}

func TestACPRawCommand(t *testing.T) {
	tests := []struct {
		name      string
		target    string
		overrides map[string]string
		want      string
	}{
		{name: "registered default without overrides", target: "cursor", overrides: nil, want: "cursor-agent acp"},
		{name: "override wins over registered default", target: "cursor", overrides: map[string]string{"cursor": "/opt/cursor/cursor-agent acp"}, want: "/opt/cursor/cursor-agent acp"},
		{name: "override is trimmed", target: "cursor", overrides: map[string]string{"cursor": "  cursor-agent acp  "}, want: "cursor-agent acp"},
		{name: "blank override falls back to registered default", target: "cursor", overrides: map[string]string{"cursor": " \t"}, want: "cursor-agent acp"},
		{name: "unknown target without override", target: "gemini", overrides: nil, want: ""},
		{name: "blank override for unknown target", target: "gemini", overrides: map[string]string{"gemini": "   "}, want: ""},
		{name: "override for unknown target", target: "gemini", overrides: map[string]string{"gemini": "node /tmp/mock-acp.mjs"}, want: "node /tmp/mock-acp.mjs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ACPRawCommand(tt.target, tt.overrides); got != tt.want {
				t.Fatalf("ACPRawCommand(%q, %v) = %q, want %q", tt.target, tt.overrides, got, tt.want)
			}
		})
	}
}
