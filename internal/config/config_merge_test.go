package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestMerge_GlobalOnly(t *testing.T) {
	global := &GlobalConfig{
		Agent:                   types.AgentClaude,
		CITimeout:               4 * time.Hour,
		ProcessTerminationGrace: 750 * time.Millisecond,
		LogLevel:                "info",
	}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)
	if cfg.Agent != types.AgentClaude {
		t.Errorf("agent = %q, want %q", cfg.Agent, types.AgentClaude)
	}
	if cfg.CITimeout != 4*time.Hour {
		t.Errorf("ci_timeout = %v", cfg.CITimeout)
	}
	if cfg.ProcessTerminationGrace != 750*time.Millisecond {
		t.Errorf("process_termination_grace = %v", cfg.ProcessTerminationGrace)
	}
	if cfg.RefreshStrategy != types.RefreshStrategyRebase {
		t.Errorf("refresh strategy = %q, want rebase", cfg.RefreshStrategy)
	}
}

func TestMerge_PromptsCombineGlobalThenRepo(t *testing.T) {
	global := &GlobalConfig{
		Agent:     types.AgentClaude,
		CITimeout: 4 * time.Hour,
		LogLevel:  "info",
		Prompts: PromptConfig{
			Shared: "global shared",
			Review: "global review",
			Test:   "global test",
		},
	}
	repo := &RepoConfig{
		Prompts: PromptConfig{
			Shared: "repo shared",
			Review: "repo review",
		},
	}

	cfg := Merge(global, repo)

	if got, want := cfg.Prompts.ForStep(types.StepReview), "global shared\n\nrepo shared\n\nglobal review\n\nrepo review"; got != want {
		t.Errorf("review prompt = %q, want %q", got, want)
	}
	if got, want := cfg.Prompts.ForStep(types.StepTest), "global shared\n\nrepo shared\n\nglobal test"; got != want {
		t.Errorf("test prompt = %q, want %q", got, want)
	}
	if got, want := cfg.Prompts.ForStep(types.StepBuild), "global shared\n\nrepo shared"; got != want {
		t.Errorf("build prompt = %q, want %q", got, want)
	}
}

// TestPrompts_ForStepsEmitsSharedOnce covers an agent invocation owning several
// steps' duties.
func TestPrompts_ForStepsEmitsSharedOnce(t *testing.T) {
	p := PromptConfig{
		Shared:   "shared",
		Document: "document",
		Lint:     "lint",
	}

	if got, want := p.ForSteps(types.StepDocument, types.StepLint), "shared\n\ndocument\n\nlint"; got != want {
		t.Errorf("document+lint prompt = %q, want %q", got, want)
	}
	if got, want := p.ForSteps(types.StepLint, types.StepDocument), "shared\n\nlint\n\ndocument"; got != want {
		t.Errorf("step order must be preserved: got %q, want %q", got, want)
	}
	if got, want := p.ForSteps(types.StepDocument), p.ForStep(types.StepDocument); got != want {
		t.Errorf("single-step ForSteps = %q, want ForStep %q", got, want)
	}
	if got := (PromptConfig{}).SectionForSteps(types.StepDocument, types.StepLint); got != "" {
		t.Errorf("unconfigured prompts section = %q, want empty", got)
	}
}

func TestMerge_UsesRepoRefreshStrategy(t *testing.T) {
	cfg := Merge(DefaultGlobalConfig(), &RepoConfig{Refresh: RefreshRaw{Strategy: types.RefreshStrategyMerge}})
	if cfg.RefreshStrategy != types.RefreshStrategyMerge {
		t.Fatalf("refresh strategy = %q, want merge", cfg.RefreshStrategy)
	}
}

func TestMergeLeavesEvalProvenanceDisabledUntilExplicitlyEnabled(t *testing.T) {
	global := &GlobalConfig{SourceYAML: []byte("agent: claude\n"), Agent: types.AgentClaude}
	repo := &RepoConfig{IgnorePatterns: []string{"vendor/**"}}
	cfg := Merge(global, repo)
	if cfg.CaptureEvalProvenance || len(cfg.ReplayGlobalYAML) != 0 || len(cfg.ReplayRepoYAML) != 0 {
		t.Fatalf("ordinary merged config contains eval provenance: %#v", cfg)
	}
	if err := cfg.EnableEvalProvenance(global, repo); err != nil {
		t.Fatal(err)
	}
	if !cfg.CaptureEvalProvenance || string(cfg.ReplayGlobalYAML) != "agent: claude\n" || len(cfg.ReplayRepoYAML) == 0 {
		t.Fatalf("enabled eval provenance = %#v", cfg)
	}
}

func TestEnableEvalProvenanceSerializesCanonicalRepoConfig(t *testing.T) {
	repo, err := LoadRepoFromBytes([]byte(`agent: [claude, codex]
intent:
  agent: pi
  model: {name: claude-opus-5, vendor: anthropic}
refresh:
  agent: [codex, claude]
  model: {name: gpt-5.6-sol, vendor: openai}
  strategy: merge
review:
  agent: claude
  model: {name: claude-opus-5, vendor: anthropic}
  adversary_agent: [codex, pi]
  adversary_model: {name: gpt-5.6-sol, vendor: openai}
build:
  agent: codex
  model: {name: gpt-5.6-sol, vendor: openai}
test:
  agent: pi
  model: {name: claude-opus-5, vendor: anthropic}
document:
  agent: opencode
  model: {name: claude-opus-5, vendor: anthropic}
lint:
  agent: copilot
  model: {name: claude-opus-5, vendor: anthropic}
pr:
  agent: claude
  model: {name: claude-opus-5, vendor: anthropic}
ci:
  agent: [pi, codex]
  model: {name: gpt-5.6-sol, vendor: openai}
`))
	if err != nil {
		t.Fatal(err)
	}
	global := &GlobalConfig{SourceYAML: []byte("agent: claude\n"), Agent: types.AgentClaude}
	cfg := Merge(global, repo)
	if err := cfg.EnableEvalProvenance(global, repo); err != nil {
		t.Fatal(err)
	}

	replayed, err := LoadRepoFromBytes(cfg.ReplayRepoYAML)
	if err != nil {
		t.Fatalf("LoadRepoFromBytes(captured provenance) error = %v\n%s", err, cfg.ReplayRepoYAML)
	}
	if !reflect.DeepEqual(replayed.Agents, repo.Agents) {
		t.Fatalf("replayed agents = %v, want %v", replayed.Agents, repo.Agents)
	}
	if !reflect.DeepEqual(replayed.ConfiguredStepAgents(), repo.ConfiguredStepAgents()) {
		t.Fatalf("replayed step agents = %v, want %v", replayed.ConfiguredStepAgents(), repo.ConfiguredStepAgents())
	}
	if !reflect.DeepEqual(replayed.ConfiguredStepModels(), repo.ConfiguredStepModels()) {
		t.Fatalf("replayed step models = %v, want %v", replayed.ConfiguredStepModels(), repo.ConfiguredStepModels())
	}
	if replayed.Refresh.Strategy != repo.Refresh.Strategy {
		t.Fatalf("replayed refresh strategy = %q, want %q", replayed.Refresh.Strategy, repo.Refresh.Strategy)
	}
	if !reflect.DeepEqual(replayed.Review.AdversaryAgents, repo.Review.AdversaryAgents) || replayed.Review.AdversaryModel != repo.Review.AdversaryModel {
		t.Fatalf("replayed adversary route = %v/%#v, want %v/%#v", replayed.Review.AdversaryAgents, replayed.Review.AdversaryModel, repo.Review.AdversaryAgents, repo.Review.AdversaryModel)
	}
}

func TestEnableEvalProvenancePreservesOmittedRepoInheritances(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte(`agent: [claude, codex]
build:
  agent: [codex, claude]
  model: {name: gpt-5.6-sol, vendor: openai}
auto_fix:
  build: 2
  review: 1
`))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := LoadRepoFromBytes([]byte("ignore_patterns: ['vendor/**']\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := Merge(global, repo)
	captured := Merge(global, repo)
	if err := captured.EnableEvalProvenance(global, repo); err != nil {
		t.Fatal(err)
	}
	replayed, err := LoadRepoFromBytes(captured.ReplayRepoYAML)
	if err != nil {
		t.Fatalf("LoadRepoFromBytes(captured provenance) error = %v\n%s", err, captured.ReplayRepoYAML)
	}
	if replayed.Declares("agent") || replayed.Declares("build.agent") || replayed.Declares("auto_fix.build") {
		t.Fatalf("captured repo made inherited fields explicit:\n%s", captured.ReplayRepoYAML)
	}
	if got := Merge(global, replayed); !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed merge changed omitted repo inheritance:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestMerge_RepoOverridesAgent(t *testing.T) {
	global := &GlobalConfig{
		Agent:             types.AgentClaude,
		AgentPathOverride: map[string]string{"claude": "/usr/bin/claude"},
		CITimeout:         4 * time.Hour,
		LogLevel:          "info",
	}
	repo := &RepoConfig{
		Agent: types.AgentCodex,
		Commands: Commands{
			Test: "make test",
		},
		Hooks: Hooks{PostWorktree: "yarn install --immutable"},
	}

	cfg := Merge(global, repo)
	if cfg.Agent != types.AgentCodex {
		t.Errorf("agent = %q, want %q (repo override)", cfg.Agent, types.AgentCodex)
	}
	if cfg.AgentPathOverride["claude"] != "/usr/bin/claude" {
		t.Errorf("agent path override lost during merge")
	}
	if cfg.Commands.Test != "make test" {
		t.Errorf("test = %q", cfg.Commands.Test)
	}
	if cfg.Hooks.PostWorktree != "yarn install --immutable" {
		t.Errorf("hooks.post_worktree = %q", cfg.Hooks.PostWorktree)
	}
	if cfg.CITimeout != 4*time.Hour {
		t.Errorf("ci_timeout = %v", cfg.CITimeout)
	}
}

func TestMerge_RepoDoesNotOverrideWhenEmpty(t *testing.T) {
	global := &GlobalConfig{
		Agent:     types.AgentRovoDev,
		CITimeout: 2 * time.Hour,
		LogLevel:  "debug",
	}
	repo := &RepoConfig{
		// Agent is empty — should not override
		Commands: Commands{
			Lint: "eslint .",
		},
	}

	cfg := Merge(global, repo)
	if cfg.Agent != types.AgentRovoDev {
		t.Errorf("agent = %q, want %q (empty repo should not override)", cfg.Agent, types.AgentRovoDev)
	}
	if cfg.Commands.Lint != "eslint ." {
		t.Errorf("lint = %q", cfg.Commands.Lint)
	}
}

func TestMerge_AutoFixDefaults(t *testing.T) {
	global := &GlobalConfig{Agent: types.AgentClaude, CITimeout: 4 * time.Hour, LogLevel: "info"}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)
	if cfg.AutoFix.Lint != 3 {
		t.Errorf("lint = %d, want 3", cfg.AutoFix.Lint)
	}
	if cfg.AutoFix.Test != 3 {
		t.Errorf("test = %d, want 3", cfg.AutoFix.Test)
	}
	if cfg.AutoFix.Review != 0 {
		t.Errorf("review = %d, want 0", cfg.AutoFix.Review)
	}
	if cfg.AutoFix.Document != 3 {
		t.Errorf("document = %d, want 3", cfg.AutoFix.Document)
	}
	if cfg.AutoFix.CI != 3 {
		t.Errorf("ci = %d, want 3", cfg.AutoFix.CI)
	}
	if cfg.AutoFix.Refresh != 3 {
		t.Errorf("refresh = %d, want 3", cfg.AutoFix.Refresh)
	}
}

func TestMerge_AutoFixGlobalOverridesDefaults(t *testing.T) {
	five := 5
	zero := 0
	global := &GlobalConfig{
		Agent:     types.AgentClaude,
		CITimeout: 4 * time.Hour,
		LogLevel:  "info",
		AutoFix:   AutoFixRaw{Lint: &five, CI: &zero},
	}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)
	if cfg.AutoFix.Lint != 5 {
		t.Errorf("lint = %d, want 5 (global override)", cfg.AutoFix.Lint)
	}
	if cfg.AutoFix.Test != 3 {
		t.Errorf("test = %d, want 3 (default)", cfg.AutoFix.Test)
	}
	if cfg.AutoFix.CI != 0 {
		t.Errorf("ci =%d, want 0 (global override)", cfg.AutoFix.CI)
	}
	if cfg.AutoFix.Refresh != 3 {
		t.Errorf("refresh = %d, want 3 (default, no override)", cfg.AutoFix.Refresh)
	}
}

func TestMerge_AutoFixRepoOverridesGlobal(t *testing.T) {
	five := 5
	one := 1
	zero := 0
	global := &GlobalConfig{
		Agent:     types.AgentClaude,
		CITimeout: 4 * time.Hour,
		LogLevel:  "info",
		AutoFix:   AutoFixRaw{Lint: &five},
	}
	repo := &RepoConfig{
		AutoFix: AutoFixRaw{Lint: &one, Review: &zero},
	}

	cfg := Merge(global, repo)
	if cfg.AutoFix.Lint != 1 {
		t.Errorf("lint = %d, want 1 (repo override)", cfg.AutoFix.Lint)
	}
	if cfg.AutoFix.Review != 0 {
		t.Errorf("review = %d, want 0 (repo override)", cfg.AutoFix.Review)
	}
	if cfg.AutoFix.Test != 3 {
		t.Errorf("test = %d, want 3 (default, no override)", cfg.AutoFix.Test)
	}
}

func TestAutoFixLimit(t *testing.T) {
	cfg := &Config{
		AutoFix: AutoFix{Lint: 5, Test: 2, Review: 0, Document: 1, CI: 3, Refresh: 4},
	}
	tests := []struct {
		step types.StepName
		want int
	}{
		{types.StepLint, 5},
		{types.StepTest, 2},
		{types.StepReview, 0},
		{types.StepDocument, 1},
		{types.StepCI, 3},
		{types.StepRefresh, 4},
		{types.StepPush, 0},
		{types.StepPR, 0},
	}
	for _, tt := range tests {
		got := cfg.AutoFixLimit(tt.step)
		if got != tt.want {
			t.Errorf("AutoFixLimit(%q) = %d, want %d", tt.step, got, tt.want)
		}
	}
}
