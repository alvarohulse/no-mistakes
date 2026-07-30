package config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestLoadRepo_StepAgentsAcceptScalarAndFallbackList(t *testing.T) {
	dir := t.TempDir()
	data := `agent: claude
intent:
  agent: claude
  enabled: false
rebase:
  agent: codex
review:
  agent: [codex, claude]
test:
  agent: pi
  evidence:
    store_in_repo: true
document:
  agent: opencode
  instructions: docs/ owns product guidance.
lint:
  agent: copilot
pr:
  agent: claude
ci:
  agent: [pi, codex]
`
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("LoadRepo() error = %v", err)
	}

	want := map[types.StepName][]types.AgentName{
		types.StepIntent:   {types.AgentClaude},
		types.StepRebase:   {types.AgentCodex},
		types.StepReview:   {types.AgentCodex, types.AgentClaude},
		types.StepTest:     {types.AgentPi},
		types.StepDocument: {types.AgentOpenCode},
		types.StepLint:     {types.AgentCopilot},
		types.StepPR:       {types.AgentClaude},
		types.StepCI:       {types.AgentPi, types.AgentCodex},
	}
	if got := cfg.ConfiguredStepAgents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("ConfiguredStepAgents() = %v, want %v", got, want)
	}
	if cfg.Test.Evidence.StoreInRepo == nil || !*cfg.Test.Evidence.StoreInRepo {
		t.Fatal("test.evidence.store_in_repo was not preserved")
	}
	if cfg.Document.Instructions != "docs/ owns product guidance." {
		t.Fatalf("document.instructions = %q", cfg.Document.Instructions)
	}
	if cfg.Intent.Enabled == nil || *cfg.Intent.Enabled {
		t.Fatal("intent.enabled was not preserved")
	}
}

func TestLoadGlobal_StepAgentsAcceptScalarAndFallbackList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `agent: claude
intent:
  agent: pi
review:
  agent: [codex, claude]
document:
  agent: pi
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal() error = %v", err)
	}
	want := map[types.StepName][]types.AgentName{
		types.StepIntent:   {types.AgentPi},
		types.StepReview:   {types.AgentCodex, types.AgentClaude},
		types.StepDocument: {types.AgentPi},
	}
	if got := cfg.configuredStepAgents(); !reflect.DeepEqual(got, want) {
		t.Fatalf("configuredStepAgents() = %v, want %v", got, want)
	}
}

func TestLoadGlobal_DocumentInstructionsRemainRepoOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `document:
  agent: codex
  instructions: do not accept this globally
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil || !strings.Contains(err.Error(), "document.instructions is repo-only") {
		t.Fatalf("LoadGlobal() error = %v, want repo-only refusal", err)
	}
}

func TestLoadGlobal_StepSectionsRejectUnknownFields(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "simple route",
			yaml: "review:\n  agnet: codex\n",
			want: "agnet",
		},
		{
			name: "intent settings",
			yaml: "intent:\n  agent: codex\n  enabeld: false\n",
			want: "enabeld",
		},
		{
			name: "test settings",
			yaml: "test:\n  agent: codex\n  evidnce: {}\n",
			want: "evidnce",
		},
		{
			name: "nested test evidence",
			yaml: "test:\n  evidence:\n    store_in_reop: true\n",
			want: "store_in_reop",
		},
		{
			name: "document settings",
			yaml: "document:\n  agent: codex\n  instructionz: docs own guidance\n",
			want: "instructionz",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := LoadGlobal(path)
			if err == nil {
				t.Fatalf("LoadGlobal() accepted unknown field %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "field") {
				t.Fatalf("LoadGlobal() error = %v, want unknown field %q", err, tt.want)
			}
		})
	}
}

func TestLoadGlobal_AliasCyclesReturnPromptly(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "self reference",
			yaml: "review: &review\n  agent: codex\n  <<: *review\n",
		},
		{
			name: "mutual reference",
			yaml: "review: &review\n  agent: codex\n  <<: &fallback\n    agent: claude\n    <<: *review\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLoadGlobal_AliasCycleHelper$")
			cmd.Env = append(os.Environ(), "NM_TEST_ALIAS_CYCLE_YAML="+tt.yaml)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("LoadGlobal() did not return for %s", tt.name)
			}
			if err != nil {
				t.Fatalf("alias-cycle helper crashed for %s: %v\n%s", tt.name, err, output)
			}
			text := string(output)
			if !strings.Contains(text, "CONFIG_ERROR:") || !strings.Contains(strings.ToLower(text), "cycle") {
				t.Fatalf("LoadGlobal() output = %q, want actionable cycle error", text)
			}
		})
	}
}

func TestLoadGlobal_AliasCycleHelper(t *testing.T) {
	data := os.Getenv("NM_TEST_ALIAS_CYCLE_YAML")
	if data == "" {
		t.Skip("subprocess helper")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("LoadGlobal() accepted a YAML alias cycle")
	}
	fmt.Printf("CONFIG_ERROR: %v\n", err)
}

func TestLoadGlobal_NonCyclicSharedAliasesRemainStrict(t *testing.T) {
	t.Run("valid shared route", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		data := `review: &route
  agent: [codex, claude]
test:
  <<: *route
document:
  <<: *route
`
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}

		cfg, err := LoadGlobal(path)
		if err != nil {
			t.Fatalf("LoadGlobal() error = %v", err)
		}
		want := []types.AgentName{types.AgentCodex, types.AgentClaude}
		if got := cfg.Test.Agents; !reflect.DeepEqual(got, want) {
			t.Fatalf("test route = %v, want %v", got, want)
		}
		if got := cfg.Document.Agents; !reflect.DeepEqual(got, want) {
			t.Fatalf("document route = %v, want %v", got, want)
		}
	})

	t.Run("shared alias revalidated under another schema", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		data := `intent: &intent
  agent: codex
  enabled: false
review:
  <<: *intent
`
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := LoadGlobal(path)
		if err == nil || !strings.Contains(err.Error(), "enabled") {
			t.Fatalf("LoadGlobal() error = %v, want aliased unknown field rejection", err)
		}
	})
}

func TestMerge_StepAgentsOverrideGlobalAndFallBackToRunAgent(t *testing.T) {
	global := DefaultGlobalConfig()
	global.Review = StepAgentRaw{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}}
	global.Test.Agent = types.AgentPi
	global.Test.Agents = []types.AgentName{types.AgentPi}
	repo := &RepoConfig{
		Review: StepAgentRaw{Agent: types.AgentClaude, Agents: []types.AgentName{types.AgentClaude, types.AgentCodex}},
	}

	cfg := Merge(global, repo)
	if got := cfg.ConfiguredAgentsForStep(types.StepReview); !reflect.DeepEqual(got, []types.AgentName{types.AgentClaude, types.AgentCodex}) {
		t.Fatalf("review agents = %v", got)
	}
	if got := cfg.ConfiguredAgentsForStep(types.StepTest); !reflect.DeepEqual(got, []types.AgentName{types.AgentPi}) {
		t.Fatalf("test agents = %v", got)
	}
	if got := cfg.ConfiguredAgentsForStep(types.StepLint); !reflect.DeepEqual(got, []types.AgentName{types.AgentAuto}) {
		t.Fatalf("unconfigured lint agents = %v, want run-wide fallback", got)
	}
}

func TestEffectiveRepoConfig_StepAgentsAreTrustedCodeExecutingSelectors(t *testing.T) {
	pushed := &RepoConfig{
		Review: StepAgentRaw{Agent: "acp:hostile", Agents: []types.AgentName{"acp:hostile"}},
		Test:   TestRaw{Agent: types.AgentOpenCode, Agents: []types.AgentName{types.AgentOpenCode}},
		Intent: IntentRaw{Agent: types.AgentOpenCode, Agents: []types.AgentName{types.AgentOpenCode}},
	}
	trusted := &RepoConfig{
		Review: StepAgentRaw{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}},
		Test:   TestRaw{Agent: types.AgentClaude, Agents: []types.AgentName{types.AgentClaude}},
		Intent: IntentRaw{Agent: types.AgentPi, Agents: []types.AgentName{types.AgentPi}},
	}

	got := EffectiveRepoConfig(pushed, trusted, false)
	if !reflect.DeepEqual(got.ConfiguredStepAgents(), trusted.ConfiguredStepAgents()) {
		t.Fatalf("effective step agents = %v, want trusted %v", got.ConfiguredStepAgents(), trusted.ConfiguredStepAgents())
	}
	if optIn := EffectiveRepoConfig(pushed, trusted, true); !reflect.DeepEqual(optIn.ConfiguredStepAgents(), pushed.ConfiguredStepAgents()) {
		t.Fatalf("opt-in step agents = %v, want pushed %v", optIn.ConfiguredStepAgents(), pushed.ConfiguredStepAgents())
	}
	if noTrusted := EffectiveRepoConfig(pushed, nil, false); len(noTrusted.ConfiguredStepAgents()) != 0 {
		t.Fatalf("step agents without trusted config = %v, want empty", noTrusted.ConfiguredStepAgents())
	}
}

func TestResolveAgent_ResolvesEachStepRouteIndependently(t *testing.T) {
	cfg := &Config{
		Agent:  types.AgentClaude,
		Agents: []types.AgentName{types.AgentClaude},
		StepAgents: map[types.StepName][]types.AgentName{
			types.StepReview: {types.AgentCodex, types.AgentPi},
			types.StepTest:   {types.AgentOpenCode},
		},
	}
	err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		return "/fake/bin/" + bin, nil
	})
	if err != nil {
		t.Fatalf("ResolveAgent() error = %v", err)
	}
	if got := cfg.ConfiguredAgentsForStep(types.StepReview); !reflect.DeepEqual(got, []types.AgentName{types.AgentCodex, types.AgentPi}) {
		t.Fatalf("review agents = %v", got)
	}
	if got := cfg.ConfiguredAgentsForStep(types.StepLint); !reflect.DeepEqual(got, []types.AgentName{types.AgentClaude}) {
		t.Fatalf("lint fallback agents = %v", got)
	}
}

func TestResolveAgent_AllowsACPRouteWithoutUnsupportedOverride(t *testing.T) {
	cfg := &Config{
		Agent:  types.AgentClaude,
		Agents: []types.AgentName{types.AgentClaude},
		StepAgents: map[types.StepName][]types.AgentName{
			types.StepReview: {types.AgentCursor},
		},
	}
	if err := cfg.ResolveAgent(context.Background(), func(bin string) (string, error) {
		return "/fake/bin/" + bin, nil
	}); err != nil {
		t.Fatalf("ResolveAgent() error = %v", err)
	}
	if got := cfg.ConfiguredAgentsForStep(types.StepReview); !reflect.DeepEqual(got, []types.AgentName{types.AgentCursor}) {
		t.Fatalf("review route = %v, want cursor", got)
	}
}

func TestLoadGlobal_AgentArgsOverrideRejectsACPTargetActionably(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `review:
  agent: cursor
agent_args_override:
  cursor:
    - --model
    - claude-opus-4-8
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected ACP agent_args_override to be rejected")
	}
	for _, want := range []string{"cursor", "ACP", "agent_args_override", "not supported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got %v", want, err)
		}
	}
}
