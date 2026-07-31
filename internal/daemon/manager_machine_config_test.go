package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunStartMachineRepoConfigOverridesCommittedCommandsAndAgent(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "machine-config-precedence")
	committed := `agent: claude
commands:
  test: committed-test
  lint: committed-lint
auto_fix:
  lint: 0
  test: 0
  review: 0
`
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte(committed), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "configure committed commands")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/main")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")

	path := writeMachineRepoConfig(t, t.TempDir(), `repo: git@github.com:test/repo.git
agent: opencode
commands:
  test: machine-test
`)
	t.Setenv(machineRepoConfigEnv, path)
	step := &captureRunConfigStep{captured: make(chan capturedRunConfig, 2)}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "machine config precedence", "", "", "")
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
		if got.lintCommand != "committed-lint" {
			t.Fatalf("commands.lint = %q, want inherited committed-lint", got.lintCommand)
		}
	default:
		t.Fatal("pipeline step did not capture effective config")
	}
	firstRun, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if kinds := configSourceKinds(firstRun.ConfigSources); kinds != "branch,default,machine-local" {
		t.Fatalf("config source kinds = %q, want branch,default,machine-local", kinds)
	}
	firstMachine := configSourceByKind(t, firstRun.ConfigSources, db.ConfigSourceMachine)
	if firstMachine.Path != path {
		t.Fatalf("stored machine path = %q, want %q", firstMachine.Path, path)
	}

	updated := `repo: git@github.com:test/repo.git
agent: opencode
commands:
  test: machine-test-v2
`
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	rerunID, err := manager.HandleRerun(context.Background(), repo.ID, "main", nil, "machine config rerun", "", "", "")
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
	rerunMachine := configSourceByKind(t, rerun.ConfigSources, db.ConfigSourceMachine)
	if rerunMachine.Digest == firstMachine.Digest {
		t.Fatalf("rerun retained stale machine digest %q", rerunMachine.Digest)
	}
}

func TestRunStartRejectsMachineRepoBindingBeforeCreatingRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "machine-config-mismatch")
	path := writeMachineRepoConfig(t, t.TempDir(), "repo: https://github.com/other/repo\ncommands:\n  test: unsafe\n")
	t.Setenv(machineRepoConfigEnv, path)
	manager := NewRunManager(database, p, func() []pipeline.Step { return nil })
	t.Cleanup(manager.Shutdown)

	_, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "machine config mismatch", "", "", "")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want binding mismatch", err)
	}
	runs, queryErr := database.GetRunsByRepo(repo.ID)
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %d, want none before binding validation", len(runs))
	}
}

type capturedRunConfig struct {
	agent       types.AgentName
	testCommand string
	lintCommand string
}

type captureRunConfigStep struct {
	captured chan capturedRunConfig
}

func (s *captureRunConfigStep) Name() types.StepName { return types.StepReview }

func (s *captureRunConfigStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	s.captured <- capturedRunConfig{
		agent:       sctx.Config.Agent,
		testCommand: sctx.Config.Commands.Test,
		lintCommand: sctx.Config.Commands.Lint,
	}
	return &pipeline.StepOutcome{}, nil
}

func TestLoadMachineRepoConfigUnsetPreservesCurrentBehavior(t *testing.T) {
	repo := &db.Repo{WorkingPath: t.TempDir(), UpstreamURL: "https://github.com/owner/project.git"}
	loaded, err := loadMachineRepoConfig(repo, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if loaded != nil {
		t.Fatalf("loaded config = %+v, want nil when NM_REPO_CONFIG is unset", loaded)
	}
}

func TestLoadMachineRepoConfigRequiresNonEmptyPath(t *testing.T) {
	repo := &db.Repo{WorkingPath: t.TempDir(), UpstreamURL: "https://github.com/owner/project.git"}
	_, err := loadMachineRepoConfig(repo, func(string) (string, bool) { return "  ", true })
	if err == nil || !strings.Contains(err.Error(), "is set but empty") {
		t.Fatalf("error = %v, want explicit empty-path refusal", err)
	}
}

func TestLoadMachineRepoConfigAcceptsEquivalentBoundRemoteAndDigestsBytes(t *testing.T) {
	repo := &db.Repo{WorkingPath: t.TempDir(), UpstreamURL: "https://github.com/Owner/Project.git"}
	configDir := t.TempDir()
	path := filepath.Join(configDir, "repo.yaml")
	data := []byte("repo: git@github.com:owner/project.git\nagent: opencode\ncommands:\n  test: machine-test\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadMachineRepoConfig(repo, fixedEnv(path))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Config.Agent != types.AgentOpenCode || loaded.Config.Commands.Test != "machine-test" {
		t.Fatalf("loaded config = %+v, want machine agent and test command", loaded.Config)
	}
	if loaded.Path != path {
		t.Fatalf("path = %q, want canonical %q", loaded.Path, path)
	}
	wantDigest := sha256.Sum256(data)
	if loaded.Digest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("digest = %q, want SHA-256 of exact bytes", loaded.Digest)
	}
}

func TestLoadMachineRepoConfigRejectsBindingMismatch(t *testing.T) {
	repo := &db.Repo{WorkingPath: t.TempDir(), UpstreamURL: "https://github.com/owner/project.git"}
	path := writeMachineRepoConfig(t, t.TempDir(), "repo: https://github.com/other/project\ncommands:\n  test: unsafe\n")

	_, err := loadMachineRepoConfig(repo, fixedEnv(path))
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %v, want repo-binding mismatch refusal", err)
	}
}

func TestLoadMachineRepoConfigRequiresBinding(t *testing.T) {
	repo := &db.Repo{WorkingPath: t.TempDir(), UpstreamURL: "https://github.com/owner/project.git"}
	path := writeMachineRepoConfig(t, t.TempDir(), "commands:\n  test: unsafe\n")

	_, err := loadMachineRepoConfig(repo, fixedEnv(path))
	if err == nil || !strings.Contains(err.Error(), "must declare repo") {
		t.Fatalf("error = %v, want missing-binding refusal", err)
	}
}

func TestLoadMachineRepoConfigRejectsPathsInsideRepository(t *testing.T) {
	repoRoot := t.TempDir()
	repo := &db.Repo{WorkingPath: repoRoot, UpstreamURL: "https://github.com/owner/project.git"}
	path := writeMachineRepoConfig(t, repoRoot, "repo: https://github.com/owner/project\ncommands:\n  test: unsafe\n")

	_, err := loadMachineRepoConfig(repo, fixedEnv(path))
	if err == nil || !strings.Contains(err.Error(), "must be outside") {
		t.Fatalf("error = %v, want inside-repository refusal", err)
	}
}

func TestLoadMachineRepoConfigRejectsSymlinksAcrossRepositoryBoundary(t *testing.T) {
	repoRoot := t.TempDir()
	repo := &db.Repo{WorkingPath: repoRoot, UpstreamURL: "https://github.com/owner/project.git"}
	outside := t.TempDir()
	insideTarget := writeMachineRepoConfig(t, repoRoot, "repo: https://github.com/owner/project\n")
	outsideTarget := writeMachineRepoConfig(t, outside, "repo: https://github.com/owner/project\n")

	tests := []struct {
		name   string
		link   string
		target string
	}{
		{name: "inside link to outside target", link: filepath.Join(repoRoot, "outside.yaml"), target: outsideTarget},
		{name: "outside link to inside target", link: filepath.Join(outside, "inside.yaml"), target: insideTarget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.Symlink(tt.target, tt.link); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			_, err := loadMachineRepoConfig(repo, fixedEnv(tt.link))
			if err == nil || !strings.Contains(err.Error(), "must be outside") {
				t.Fatalf("error = %v, want symlink boundary refusal", err)
			}
		})
	}
}

func TestEffectiveRepoConfigSourcesIncludeOnlyContributingCommittedInputs(t *testing.T) {
	global := &config.GlobalConfig{Agent: types.AgentClaude}
	pushedConfig, err := config.LoadRepoFromBytes([]byte("ignore_patterns: [vendor/**]\n"))
	if err != nil {
		t.Fatal(err)
	}
	trustedConfig, err := config.LoadRepoFromBytes([]byte("commands:\n  test: trusted-test\n"))
	if err != nil {
		t.Fatal(err)
	}
	machineConfig, err := config.LoadRepoFromBytes([]byte("repo: https://github.com/test/repo\ncommands:\n  test: machine-test\n"))
	if err != nil {
		t.Fatal(err)
	}
	pushed := repoConfigInputFromBytes(pushedConfig, []byte("pushed"), db.ConfigSourceBranch, "head")
	trusted := repoConfigInputFromBytes(trustedConfig, []byte("trusted"), db.ConfigSourceDefault, "main")
	machine := &machineRepoConfig{Config: machineConfig, Path: "/private/machine.yaml", Digest: "machine"}

	effective, sources := effectiveRepoConfigAndSources(global, pushed, trusted, machine)
	if effective.Commands.Test != "machine-test" {
		t.Fatalf("commands.test = %q, want machine-test", effective.Commands.Test)
	}
	if kinds := configSourceKinds(sources); kinds != "branch,machine-local" {
		t.Fatalf("config source kinds = %q, want branch,machine-local; trusted command was fully displaced", kinds)
	}
}

func TestRecoveredMachineRepoConfigRefusesChangedMissingOrNewSource(t *testing.T) {
	expected := []db.ConfigSource{{Kind: db.ConfigSourceMachine, Digest: "launch-digest", Path: "/private/repo.yaml"}}
	tests := []struct {
		name    string
		sources []db.ConfigSource
		machine *machineRepoConfig
	}{
		{name: "changed digest", sources: expected, machine: &machineRepoConfig{Digest: "changed", Path: "/private/repo.yaml"}},
		{name: "changed path", sources: expected, machine: &machineRepoConfig{Digest: "launch-digest", Path: "/private/other.yaml"}},
		{name: "missing", sources: expected},
		{name: "new mid-run", machine: &machineRepoConfig{Digest: "new", Path: "/private/repo.yaml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateRecoveredMachineRepoConfig(tt.sources, tt.machine); err == nil {
				t.Fatal("expected recovery to fail closed")
			}
		})
	}
	if err := validateRecoveredMachineRepoConfig(expected, &machineRepoConfig{Digest: "launch-digest", Path: "/private/repo.yaml"}); err != nil {
		t.Fatalf("unchanged launch config rejected: %v", err)
	}
}

func TestLoadRecoveredConfigRefusesMachineConfigDigestDrift(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, head := setupTestGitRepo(t, p, database, "machine-config-recovery")
	gitCmd(t, repo.WorkingPath, "remote", "add", "origin", p.RepoDir(repo.ID))
	path := writeMachineRepoConfig(t, t.TempDir(), "repo: https://github.com/test/repo\ncommands:\n  test: launch-test\n")
	t.Setenv(machineRepoConfigEnv, path)
	loaded, err := loadMachineRepoConfig(repo, fixedEnv(path))
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "main", head, refreshTestZeroSHA)
	if err != nil {
		t.Fatal(err)
	}
	sources := []db.ConfigSource{{Kind: db.ConfigSourceMachine, Digest: loaded.Digest, Path: loaded.Path}}
	if err := database.UpdateRunConfigSources(run.ID, sources); err != nil {
		t.Fatal(err)
	}
	run.ConfigSources = sources
	manager := NewRunManager(database, p, nil)
	t.Cleanup(manager.Shutdown)

	cfg, err := manager.loadRecoveredConfig(context.Background(), run, repo, repo.WorkingPath)
	if err != nil {
		t.Fatalf("load unchanged recovered config: %v", err)
	}
	if cfg.Commands.Test != "launch-test" {
		t.Fatalf("recovered commands.test = %q, want launch-test", cfg.Commands.Test)
	}
	if err := os.WriteFile(path, []byte("repo: https://github.com/test/repo\ncommands:\n  test: changed-test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.loadRecoveredConfig(context.Background(), run, repo, repo.WorkingPath); err == nil || !strings.Contains(err.Error(), "digest differs") {
		t.Fatalf("drift recovery error = %v, want digest mismatch refusal", err)
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

func fixedEnv(path string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if name != machineRepoConfigEnv {
			return "", false
		}
		return path, true
	}
}

func writeMachineRepoConfig(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "machine-repo.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
