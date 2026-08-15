package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunStartGlobalOverrideOverridesCommittedCommandsAndAgent(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	globalYAML := `process_termination_grace: 11s
overrides:
  test/repo:
    agent: opencode
    commands:
      build: machine-build
      test: machine-test
    build:
      agent: codex
      model: {name: gpt-5.6-sol, vendor: openai}
    prompts:
      test: run the scaleapi testing commands
`
	if err := os.WriteFile(p.ConfigFile(), []byte(globalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, _ := setupTestGitRepo(t, p, database, "override-config-precedence")
	committed := `agent: claude
commands:
  build: committed-build
  test: committed-test
  lint: committed-lint
auto_fix:
  lint: 0
  test: 0
  review: 0
prompts:
  shared: committed shared guidance
  test: committed test guidance
`
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte(committed), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "configure committed commands")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")

	step := &captureRunConfigStep{captured: make(chan capturedRunConfig, 2)}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "override config precedence", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	select {
	case got := <-step.captured:
		if got.agent != types.AgentOpenCode {
			t.Fatalf("agent = %q, want opencode", got.agent)
		}
		if got.testCommand != "machine-test" {
			t.Fatalf("commands.test = %q, want machine-test", got.testCommand)
		}
		if got.buildCommand != "machine-build" || got.buildModel != "gpt-5.6-sol" {
			t.Fatalf("build config = %q/%q, want machine-build/gpt-5.6-sol", got.buildCommand, got.buildModel)
		}
		if got.lintCommand != "committed-lint" {
			t.Fatalf("commands.lint = %q, want inherited committed-lint", got.lintCommand)
		}
		// The override overlays the trusted committed prompts key by key:
		// prompts.test is replaced while prompts.shared is inherited, and both
		// reach the resolved per-step prompt additions.
		wantTestPrompt := "committed shared guidance\n\nrun the scaleapi testing commands"
		if got.testPrompt != wantTestPrompt {
			t.Fatalf("test prompt additions = %q, want %q", got.testPrompt, wantTestPrompt)
		}
	default:
		t.Fatal("pipeline step did not capture effective config")
	}
	firstRun, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if kinds := configSourceKinds(firstRun.ConfigSources); kinds != "global,branch,default,global-override" {
		t.Fatalf("config source kinds = %q, want global,branch,default,global-override", kinds)
	}
	firstOverride := configSourceByKind(t, firstRun.ConfigSources, db.ConfigSourceGlobalOverride)
	if firstOverride.Ref != "test/repo" {
		t.Fatalf("stored override key = %q, want test/repo", firstOverride.Ref)
	}
	wantDigest := sha256.Sum256([]byte(globalYAML))
	if firstOverride.Digest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("stored override digest = %q, want SHA-256 of the exact global config bytes", firstOverride.Digest)
	}

	updated := strings.Replace(globalYAML, "machine-test", "machine-test-v2", 1)
	if err := os.WriteFile(p.ConfigFile(), []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	rerunID, err := manager.HandleRerun(context.Background(), repo.ID, "main", nil, "override config rerun", "", "", "")
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if run := waitForRunTerminalState(t, database, rerunID); run.Status != types.RunCompleted {
		t.Fatalf("rerun status = %s, error = %v", run.Status, run.Error)
	}
	rerun, err := database.GetRun(rerunID)
	if err != nil {
		t.Fatal(err)
	}
	rerunOverride := configSourceByKind(t, rerun.ConfigSources, db.ConfigSourceGlobalOverride)
	if rerunOverride.Digest == firstOverride.Digest {
		t.Fatalf("rerun retained stale global config digest %q", rerunOverride.Digest)
	}
}

func TestRunStartRecordsMalformedOverridesAsFailedRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "override-config-malformed")
	if err := os.WriteFile(p.ConfigFile(), []byte("overrides:\n  not-owner-repo:\n    commands:\n      test: unsafe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewRunManager(database, p, func() []pipeline.Step { return nil })
	t.Cleanup(manager.Shutdown)

	_, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "override config malformed", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "overrides key") {
		t.Fatalf("error = %v, want malformed-key refusal", err)
	}
	// A malformed overrides section fails like any other malformed global
	// config: the run row carries the reason (the only feedback a
	// push-triggered pipeline has) and the setup worktree is cleaned up.
	runs, queryErr := database.GetRunsByRepo(repo.ID)
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want the failed run to be recorded", len(runs))
	}
	if runs[0].Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed", runs[0].Status)
	}
	if runs[0].Error == nil || !strings.Contains(*runs[0].Error, "overrides key") {
		t.Fatalf("run error = %v, want the malformed-key reason", runs[0].Error)
	}
	if _, statErr := os.Stat(p.WorktreeDir(repo.ID, runs[0].ID)); !os.IsNotExist(statErr) {
		t.Fatalf("setup-failure worktree still present, stat err = %v", statErr)
	}
}

type capturedRunConfig struct {
	agent        types.AgentName
	buildCommand string
	buildModel   string
	testCommand  string
	lintCommand  string
	testPrompt   string
}

type captureRunConfigStep struct {
	captured chan capturedRunConfig
}

func (s *captureRunConfigStep) Name() types.StepName { return types.StepReview }

func (s *captureRunConfigStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.captured <- capturedRunConfig{
		agent:        sctx.Config.Agent,
		buildCommand: sctx.Config.Commands.Build,
		buildModel:   sctx.Config.ConfiguredModelForStep(types.StepBuild).Name,
		testCommand:  sctx.Config.Commands.Test,
		lintCommand:  sctx.Config.Commands.Lint,
		testPrompt:   sctx.Config.Prompts.ForStep(types.StepTest),
	}
	return &pipeline.StepOutcome{}, nil
}

func TestResolveGlobalOverrideWithoutOverridesIsNil(t *testing.T) {
	repo := &db.Repo{WorkingPath: t.TempDir(), UpstreamURL: "https://github.com/owner/project.git"}
	global := &globalConfigInput{Config: config.DefaultGlobalConfig()}
	if got := resolveGlobalOverride(global, repo); got != nil {
		t.Fatalf("override = %+v, want nil when no overrides are configured", got)
	}
}

func TestResolveGlobalOverrideMatchesEquivalentRemoteFormsAndCarriesGlobalDigest(t *testing.T) {
	globalYAML := "overrides:\n  Owner/Project:\n    agent: opencode\n    commands:\n      test: machine-test\n"
	global := loadGlobalConfigInputFromBytes(t, globalYAML)
	for _, upstream := range []string{
		"git@github.com:owner/project.git",
		"https://github.com/Owner/Project",
		"ssh://git@github.com/owner/project.git",
	} {
		repo := &db.Repo{WorkingPath: t.TempDir(), UpstreamURL: upstream}
		loaded := resolveGlobalOverride(global, repo)
		if loaded == nil {
			t.Fatalf("upstream %q: no override matched", upstream)
		}
		if loaded.Key != "owner/project" {
			t.Fatalf("upstream %q: key = %q, want owner/project", upstream, loaded.Key)
		}
		if loaded.Config.Agent != types.AgentOpenCode || loaded.Config.Commands.Test != "machine-test" {
			t.Fatalf("upstream %q: config = %+v, want override agent and test command", upstream, loaded.Config)
		}
		wantDigest := sha256.Sum256([]byte(globalYAML))
		if loaded.Digest != hex.EncodeToString(wantDigest[:]) {
			t.Fatalf("digest = %q, want SHA-256 of the exact global config bytes", loaded.Digest)
		}
		if loaded.Path != global.Source.Path {
			t.Fatalf("path = %q, want the global config path %q", loaded.Path, global.Source.Path)
		}
	}
}

func TestResolveGlobalOverrideNonMatchingRepoIsUnaffected(t *testing.T) {
	global := loadGlobalConfigInputFromBytes(t, "overrides:\n  other/project:\n    commands:\n      test: machine-test\n")
	repo := &db.Repo{WorkingPath: t.TempDir(), UpstreamURL: "https://github.com/owner/project.git"}
	if got := resolveGlobalOverride(global, repo); got != nil {
		t.Fatalf("override = %+v, want nil for a repository no key matches", got)
	}
}

// TestResolveGlobalOverrideUnresolvableIdentityIsNoOverlay pins the decision
// that a registered remote with no normalizable identity (for example a
// local-path remote) resolves to "no overlay" rather than failing every run
// on a machine that configures overrides for other repositories.
func TestResolveGlobalOverrideUnresolvableIdentityIsNoOverlay(t *testing.T) {
	global := loadGlobalConfigInputFromBytes(t, "overrides:\n  other/project:\n    commands:\n      test: machine-test\n")
	repo := &db.Repo{WorkingPath: t.TempDir(), UpstreamURL: "/srv/git/upstream.git"}
	if got := resolveGlobalOverride(global, repo); got != nil {
		t.Fatalf("override = %+v, want nil for an unresolvable registered identity", got)
	}
}

func TestEffectiveRepoConfigSourcesIncludeOnlyContributingCommittedInputs(t *testing.T) {
	globalConfig := config.DefaultGlobalConfig()
	globalConfig.Agent = types.AgentClaude
	global := &globalConfigInput{
		Config: globalConfig,
		Source: &db.ConfigSource{Kind: db.ConfigSourceGlobal, Digest: "global", Path: "/private/global.yaml"},
	}
	pushedConfig, err := config.LoadRepoFromBytes([]byte("ignore_patterns: [vendor/**]\n"))
	if err != nil {
		t.Fatal(err)
	}
	trustedConfig, err := config.LoadRepoFromBytes([]byte("commands:\n  test: trusted-test\n"))
	if err != nil {
		t.Fatal(err)
	}
	overrideConfig, err := config.LoadRepoFromBytes([]byte("commands:\n  test: machine-test\n"))
	if err != nil {
		t.Fatal(err)
	}
	pushed := repoConfigInputFromBytes(pushedConfig, []byte("pushed"), db.ConfigSourceBranch, "head")
	trusted := repoConfigInputFromBytes(trustedConfig, []byte("trusted"), db.ConfigSourceDefault, "main")
	override := &globalOverrideInput{Config: overrideConfig, Key: "test/repo", Digest: "global", Path: "/private/global.yaml"}

	effective, sources := effectiveRepoConfigAndSources(global, pushed, trusted, override)
	if effective.Commands.Test != "machine-test" {
		t.Fatalf("commands.test = %q, want machine-test", effective.Commands.Test)
	}
	if kinds := configSourceKinds(sources); kinds != "global,branch,global-override" {
		t.Fatalf("config source kinds = %q, want global,branch,global-override; trusted command was fully displaced", kinds)
	}
}

func TestEffectiveRepoConfigSourcesPersistOrdinaryContributingInputs(t *testing.T) {
	globalConfig := config.DefaultGlobalConfig()
	globalConfig.Agent = types.AgentClaude
	global := &globalConfigInput{
		Config: globalConfig,
		Source: &db.ConfigSource{Kind: db.ConfigSourceGlobal, Digest: "global", Path: "/private/global.yaml"},
	}
	pushedConfig, err := config.LoadRepoFromBytes([]byte("ignore_patterns: [vendor/**]\n"))
	if err != nil {
		t.Fatal(err)
	}
	trustedConfig, err := config.LoadRepoFromBytes([]byte("commands:\n  test: trusted-test\n"))
	if err != nil {
		t.Fatal(err)
	}
	pushed := repoConfigInputFromBytes(pushedConfig, []byte("pushed"), db.ConfigSourceBranch, "head")
	trusted := repoConfigInputFromBytes(trustedConfig, []byte("trusted"), db.ConfigSourceDefault, "main")

	effective, sources := effectiveRepoConfigAndSources(global, pushed, trusted, nil)
	if effective.Commands.Test != "trusted-test" || !reflect.DeepEqual(effective.IgnorePatterns, []string{"vendor/**"}) {
		t.Fatalf("effective config = %#v", effective)
	}
	if kinds := configSourceKinds(sources); kinds != "global,branch,default" {
		t.Fatalf("config source kinds = %q, want global,branch,default", kinds)
	}
}

func TestEffectiveRepoConfigSourcesDoNotInventDigestForBuiltInGlobalDefaults(t *testing.T) {
	overrideConfig, err := config.LoadRepoFromBytes([]byte("commands:\n  test: machine-test\n"))
	if err != nil {
		t.Fatal(err)
	}
	override := &globalOverrideInput{Config: overrideConfig, Key: "test/repo", Digest: "global", Path: "/private/global.yaml"}
	pushed := &repoConfigInput{Config: &config.RepoConfig{}}
	withoutOverride, sources := effectiveRepoConfigAndSources(&globalConfigInput{Config: config.DefaultGlobalConfig()}, pushed, nil, nil)
	if withoutOverride.Refresh.Strategy.OrDefault() != types.RefreshStrategyRebase || len(sources) != 0 {
		t.Fatalf("absent override changed defaults or recorded sources: config=%+v sources=%#v", withoutOverride, sources)
	}

	_, sources = effectiveRepoConfigAndSources(&globalConfigInput{Config: config.DefaultGlobalConfig()}, pushed, nil, override)
	if kinds := configSourceKinds(sources); kinds != "global-override" {
		t.Fatalf("built-in defaults produced config source kinds %q, want only global-override", kinds)
	}

	_, sources = effectiveRepoConfigAndSources(&globalConfigInput{
		Config: config.DefaultGlobalConfig(),
		Source: &db.ConfigSource{Kind: db.ConfigSourceGlobal, Digest: "same-as-defaults", Path: "/private/global.yaml"},
	}, pushed, nil, override)
	if kinds := configSourceKinds(sources); kinds != "global-override" {
		t.Fatalf("semantically empty global file produced config source kinds %q, want only global-override", kinds)
	}
}

func TestRecoveredGlobalOverrideRefusesChangedMissingOrNewSource(t *testing.T) {
	expected := []db.ConfigSource{{Kind: db.ConfigSourceGlobalOverride, Digest: "launch-digest", Ref: "test/repo", Path: "/private/config.yaml"}}
	tests := []struct {
		name     string
		sources  []db.ConfigSource
		override *globalOverrideInput
	}{
		{name: "changed digest", sources: expected, override: &globalOverrideInput{Digest: "changed", Key: "test/repo", Path: "/private/config.yaml"}},
		{name: "changed path", sources: expected, override: &globalOverrideInput{Digest: "launch-digest", Key: "test/repo", Path: "/private/other.yaml"}},
		{name: "changed key", sources: expected, override: &globalOverrideInput{Digest: "launch-digest", Key: "test/other", Path: "/private/config.yaml"}},
		{name: "missing", sources: expected},
		{name: "new mid-run", override: &globalOverrideInput{Digest: "new", Key: "test/repo", Path: "/private/config.yaml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateRecoveredGlobalOverride(tt.sources, tt.override); err == nil {
				t.Fatal("expected recovery to fail closed")
			}
		})
	}
	if err := validateRecoveredGlobalOverride(expected, &globalOverrideInput{Digest: "launch-digest", Key: "test/repo", Path: "/private/config.yaml"}); err != nil {
		t.Fatalf("unchanged launch config rejected: %v", err)
	}
}

func TestLoadRecoveredConfigRefusesRetiredMachineLocalSource(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "machine-source-recovery")
	run, err := database.InsertRunWithOptions(repo.ID, "main", head, refreshTestZeroSHA, db.RunOptions{LegacyResolvedPolicy: true})
	if err != nil {
		t.Fatal(err)
	}
	sources := []db.ConfigSource{{Kind: db.ConfigSourceMachine, Digest: "legacy", Path: "/private/repo.yaml"}}
	if err := database.UpdateRunConfigSources(run.ID, sources); err != nil {
		t.Fatal(err)
	}
	run.ConfigSources = sources
	persistResolvedRoutingForTest(t, database, run)
	manager := NewRunManager(database, p, nil)
	t.Cleanup(manager.Shutdown)

	if _, err := manager.loadRecoveredConfig(context.Background(), run, repo, repo.WorkingPath); err == nil || !strings.Contains(err.Error(), "removed machine-local") {
		t.Fatalf("error = %v, want retired-mechanism refusal", err)
	}
}

func TestLoadRecoveredConfigRefusesGlobalOverrideDigestDrift(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "override-config-recovery")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", p.RepoDir(repo.ID))
	if err := os.WriteFile(p.ConfigFile(), []byte("overrides:\n  test/repo:\n    commands:\n      test: launch-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	globalInput, err := loadGlobalConfigInput(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	override := resolveGlobalOverride(globalInput, repo)
	if override == nil {
		t.Fatal("override did not resolve at launch")
	}
	run, err := database.InsertRunWithOptions(repo.ID, "main", head, refreshTestZeroSHA, db.RunOptions{LegacyResolvedPolicy: true})
	if err != nil {
		t.Fatal(err)
	}
	sources := []db.ConfigSource{{Kind: db.ConfigSourceGlobalOverride, Digest: override.Digest, Ref: override.Key, Path: override.Path}}
	if err := database.UpdateRunConfigSources(run.ID, sources); err != nil {
		t.Fatal(err)
	}
	run.ConfigSources = sources
	persistResolvedRoutingForTest(t, database, run)
	manager := NewRunManager(database, p, nil)
	t.Cleanup(manager.Shutdown)

	cfg, err := manager.loadRecoveredConfig(context.Background(), run, repo, repo.WorkingPath)
	if err != nil {
		t.Fatalf("load unchanged recovered config: %v", err)
	}
	if cfg.Commands.Test != "launch-test" {
		t.Fatalf("recovered commands.test = %q, want launch-test", cfg.Commands.Test)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("overrides:\n  test/repo:\n    commands:\n      test: changed-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.loadRecoveredConfig(context.Background(), run, repo, repo.WorkingPath); err == nil || !strings.Contains(err.Error(), "digest differs") {
		t.Fatalf("drift recovery error = %v, want digest mismatch refusal", err)
	}
}

func TestLoadRecoveredConfigRefusesGlobalConfigDigestDrift(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	globalYAML := "process_termination_grace: 11s\noverrides:\n  test/repo:\n    commands:\n      test: launch-test\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(globalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	repo, head := setupTestGitRepo(t, p, database, "global-config-recovery")
	globalInput, err := loadGlobalConfigInput(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	override := resolveGlobalOverride(globalInput, repo)
	if override == nil {
		t.Fatal("override did not resolve at launch")
	}
	run, err := database.InsertRunWithOptions(repo.ID, "main", head, refreshTestZeroSHA, db.RunOptions{LegacyResolvedPolicy: true})
	if err != nil {
		t.Fatal(err)
	}
	sources := []db.ConfigSource{*globalInput.Source, {Kind: db.ConfigSourceGlobalOverride, Digest: override.Digest, Ref: override.Key, Path: override.Path}}
	if err := database.UpdateRunConfigSources(run.ID, sources); err != nil {
		t.Fatal(err)
	}
	run.ConfigSources = sources
	persistResolvedRoutingForTest(t, database, run)
	manager := NewRunManager(database, p, nil)
	t.Cleanup(manager.Shutdown)

	cfg, err := manager.loadRecoveredConfig(context.Background(), run, repo, repo.WorkingPath)
	if err != nil {
		t.Fatalf("load unchanged recovered config: %v", err)
	}
	if cfg.ProcessTerminationGrace.String() != "11s" {
		t.Fatalf("recovered process grace = %s, want 11s", cfg.ProcessTerminationGrace)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte(strings.Replace(globalYAML, "11s", "12s", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.loadRecoveredConfig(context.Background(), run, repo, repo.WorkingPath); err == nil || !strings.Contains(err.Error(), "digest differs") {
		t.Fatalf("global drift recovery error = %v, want digest mismatch refusal", err)
	}
}

func TestLoadRecoveredConfigUsesLaunchRefsAfterCommittedConfigAdvances(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "committed-config-recovery")
	launchBytes := []byte("commands:\n  lint: launch-lint\nignore_patterns: [launch/**]\n")
	configPath := filepath.Join(repo.WorkingPath, ".no-mistakes.yaml")
	if err := os.WriteFile(configPath, launchBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "launch config")
	launchHead := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	if err := os.WriteFile(p.ConfigFile(), []byte("overrides:\n  test/repo:\n    commands:\n      test: launch-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	globalInput, err := loadGlobalConfigInput(p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	override := resolveGlobalOverride(globalInput, repo)
	if override == nil {
		t.Fatal("override did not resolve at launch")
	}
	parsed, err := config.LoadRepoFromBytes(launchBytes)
	if err != nil {
		t.Fatal(err)
	}
	pushed := repoConfigInputFromBytes(parsed, launchBytes, db.ConfigSourceBranch, launchHead)
	trusted := repoConfigInputFromBytes(parsed, launchBytes, db.ConfigSourceDefault, launchHead)
	_, sources := effectiveRepoConfigAndSources(globalInput, pushed, trusted, override)
	run, err := database.InsertRunWithOptions(repo.ID, "main", launchHead, refreshTestZeroSHA, db.RunOptions{LegacyResolvedPolicy: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunConfigSources(run.ID, sources); err != nil {
		t.Fatal(err)
	}
	run.ConfigSources = sources
	persistResolvedRoutingForTest(t, database, run)

	if err := os.WriteFile(configPath, []byte("commands:\n  lint: changed-lint\nignore_patterns: [changed/**]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "advance config")
	manager := NewRunManager(database, p, nil)
	t.Cleanup(manager.Shutdown)

	cfg, err := manager.loadRecoveredConfig(context.Background(), run, repo, repo.WorkingPath)
	if err != nil {
		t.Fatalf("load pinned recovered config: %v", err)
	}
	if cfg.Commands.Lint != "launch-lint" || !reflect.DeepEqual(cfg.IgnorePatterns, []string{"launch/**"}) {
		t.Fatalf("recovered committed config = lint %q ignore %v, want launch values", cfg.Commands.Lint, cfg.IgnorePatterns)
	}
}

func configSourceByKind(t *testing.T, sources []db.ConfigSource, kind string) db.ConfigSource {
	t.Helper()
	for _, source := range sources {
		if source.Kind == kind {
			return source
		}
	}
	t.Fatalf("config source %q missing from %#v", kind, sources)
	return db.ConfigSource{}
}

func configSourceKinds(sources []db.ConfigSource) string {
	kinds := make([]string, 0, len(sources))
	for _, source := range sources {
		kinds = append(kinds, source.Kind)
	}
	return strings.Join(kinds, ",")
}

// loadGlobalConfigInputFromBytes writes the YAML into a temp global config
// file and loads it through the production loader, so tests exercise the same
// digest and path canonicalization runs record.
func loadGlobalConfigInputFromBytes(t *testing.T, yaml string) *globalConfigInput {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := loadGlobalConfigInput(path)
	if err != nil {
		t.Fatal(err)
	}
	return input
}
