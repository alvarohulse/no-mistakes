package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"gopkg.in/yaml.v3"
)

func TestStartRunPersistsExactEffectiveConfigArtifactsBeforeWorktreeExecution(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, _ := newPolicyResolutionFixture(t, "effective-config")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "effective config", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}

	runDir := filepath.Join(p.Root(), "runs", runID)
	assertOwnerOnlyDirectory(t, p.RunsDir())
	assertOwnerOnlyDirectory(t, runDir)
	yamlBytes := readOwnerOnlyArtifact(t, filepath.Join(runDir, "effective-config.yaml"))
	metaBytes := readOwnerOnlyArtifact(t, filepath.Join(runDir, "effective-config.meta.json"))

	for _, want := range []string{
		"managed: false # source=global; is_default=true",
		"build: echo build # source=trusted; is_default=false",
		"- vendor/** # source=pushed; is_default=false",
		"source=runtime; is_default=false",
	} {
		if !strings.Contains(string(yamlBytes), want) {
			t.Errorf("effective config missing %q:\n%s", want, yamlBytes)
		}
	}

	var meta struct {
		SchemaVersion   int    `json:"schema_version"`
		RunID           string `json:"run_id"`
		PolicyDigest    string `json:"policy_digest"`
		YAMLSHA256      string `json:"yaml_sha256"`
		BinaryVersion   string `json:"binary_version"`
		BinaryBuildSHA  string `json:"binary_build_sha"`
		Generator       string `json:"generator"`
		GeneratorSchema int    `json:"generator_schema"`
	}
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("decode effective config sidecar: %v", err)
	}
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(yamlBytes)
	if meta.SchemaVersion != 1 || meta.RunID != runID || run.ResolvedPolicyDigest == nil || meta.PolicyDigest != *run.ResolvedPolicyDigest || meta.YAMLSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("sidecar integrity fields = %+v, run policy digest = %v", meta, run.ResolvedPolicyDigest)
	}
	if meta.BinaryVersion != buildinfo.CurrentVersion() || meta.BinaryBuildSHA != buildinfo.Commit || meta.Generator != "no-mistakes/effective-config" || meta.GeneratorSchema != 1 {
		t.Fatalf("sidecar generation identity = %+v", meta)
	}
	var document map[string]any
	if err := yaml.Unmarshal(yamlBytes, &document); err != nil {
		t.Fatal(err)
	}
	testConfig, ok := document["test"].(map[string]any)
	if !ok {
		t.Fatalf("test config = %#v, want mapping", document["test"])
	}
	evidence, ok := testConfig["evidence"].(map[string]any)
	if !ok {
		t.Fatalf("test.evidence = %#v, want mapping", testConfig["evidence"])
	}
	if got, want := sortedMapKeys(evidence), []string{"local_root", "max_runs", "retention"}; !slices.Equal(got, want) {
		t.Fatalf("test.evidence fields = %v, want %v", got, want)
	}
}

func TestStartRunEffectiveConfigProvenanceTracksAppliedValuesNotDeclarations(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "effective-config-applied-provenance")
	if err := os.WriteFile(p.ConfigFile(), []byte("hooks:\n  pr_body: global-formatter\nprompts:\n  review: global review guidance\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	trustedYAML := "auto_fix:\n  review: 0\n" +
		"hooks:\n  post_worktree: " + yamlDoubleQuoted("echo ran > "+marker) + "\n  pr_body: \"\"\n" +
		"prompts:\n  review: \"\"\n"
	writePolicyConfigCommit(t, repo, trustedYAML, "configure blank trusted values", "refs/heads/main")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "applied provenance", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	yamlBytes := readOwnerOnlyArtifact(t, p.EffectiveConfigYAML(runID))
	assertEffectiveConfigScalar(t, yamlBytes, []string{"hooks", "pr_body"}, "global-formatter", "source=global; is_default=false")
	assertEffectiveConfigScalar(t, yamlBytes, []string{"prompts", "review"}, "global review guidance", "source=global; is_default=false")
}

func TestStartRunEffectiveConfigPreservesNestedPlatformCommandLeafProvenance(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "effective-config-command-provenance")
	globalYAML := `overrides:
  test/repo:
    commands:
      build:
        windows:
          runner:
            executable: powershell
`
	if err := os.WriteFile(p.ConfigFile(), []byte(globalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	trustedYAML := "auto_fix:\n  review: 0\n" +
		"hooks:\n  post_worktree: " + yamlDoubleQuoted("echo ran > "+marker) + "\n" +
		`commands:
  build:
    run: trusted-build
    windows:
      run: trusted-windows-build
      runner:
        executable: pwsh
        args: [-NoLogo, -NoProfile, -NonInteractive, -Command]
`
	writePolicyConfigCommit(t, repo, trustedYAML, "configure nested command", "refs/heads/main")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "nested command provenance", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	yamlBytes := readOwnerOnlyArtifact(t, p.EffectiveConfigYAML(runID))
	assertEffectiveConfigScalar(t, yamlBytes, []string{"commands", "build", "windows", "run"}, "trusted-windows-build", "source=trusted; is_default=false")
	assertEffectiveConfigScalar(t, yamlBytes, []string{"commands", "build", "windows", "runner", "executable"}, "powershell", "source=global-override; is_default=false")
	args := effectiveConfigNode(t, yamlBytes, "commands", "build", "windows", "runner", "args")
	if args.Kind != yaml.SequenceNode || len(args.Content) != 4 {
		t.Fatalf("commands.build.windows.runner.args = %#v, want four-item sequence", args)
	}
	if got := effectiveConfigNodeComment(args.Content[0]); got != "source=trusted; is_default=false" {
		t.Fatalf("commands.build.windows.runner.args[0] provenance = %q, want trusted", got)
	}
}

func TestStartRunEffectiveConfigExplainsRoutingTrustDecision(t *testing.T) {
	p, database, repo, marker := newPolicyResolutionFixture(t, "effective-config-routing-provenance")
	agentDir := t.TempDir()
	mockCodex := writeRunnableMockAgent(t, agentDir, "codex")
	globalYAML := "agent_path_override:\n  codex: " + yamlDoubleQuoted(mockCodex) + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(globalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	trustedYAML := "allow_repo_commands: true\nauto_fix:\n  review: 0\n" +
		"hooks:\n  post_worktree: " + yamlDoubleQuoted("echo ran > "+marker) + "\n"
	writePolicyConfigCommit(t, repo, trustedYAML, "allow pushed routing", "refs/heads/main")
	pushedYAML := `agent: codex
auto_fix:
  review: 0
review:
  agent: codex
  model:
    name: gpt-5.6-sol
    vendor: openai
`
	writePolicyConfigCommit(t, repo, pushedYAML, "configure pushed routing", "refs/heads/feature/routing-provenance")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	runID, err := manager.startRun(context.Background(), repo, "feature/routing-provenance", head, refreshTestZeroSHA, "test", nil, "routing provenance", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	yamlBytes := readOwnerOnlyArtifact(t, p.EffectiveConfigYAML(runID))
	assertEffectiveConfigScalar(t, yamlBytes, []string{"allow_repo_commands"}, "true", "source=trusted; is_default=false")
	defaultAgents := effectiveConfigNode(t, yamlBytes, "agent", "default")
	if len(defaultAgents.Content) != 1 || defaultAgents.Content[0].Value != "codex" {
		t.Fatalf("agent.default = %#v, want [codex]", defaultAgents)
	}
	if got := effectiveConfigNodeComment(defaultAgents.Content[0]); got != "source=pushed; is_default=false" {
		t.Fatalf("agent.default[0] provenance = %q, want pushed", got)
	}
	reviewAgents := effectiveConfigNode(t, yamlBytes, "agent", "step_routes", "review", "agents")
	if len(reviewAgents.Content) != 1 || reviewAgents.Content[0].Value != "codex" {
		t.Fatalf("agent.step_routes.review.agents = %#v, want [codex]", reviewAgents)
	}
	if got := effectiveConfigNodeComment(reviewAgents.Content[0]); got != "source=pushed; is_default=false" {
		t.Fatalf("agent.step_routes.review.agents[0] provenance = %q, want pushed", got)
	}
	assertEffectiveConfigScalar(t, yamlBytes, []string{"agent", "step_routes", "review", "model", "name"}, "gpt-5.6-sol", "source=pushed; is_default=false")
	assertEffectiveConfigScalar(t, yamlBytes, []string{"agent", "step_routes", "review", "model", "vendor"}, "openai", "source=pushed; is_default=false")
}

func TestStartRunEffectiveConfigMarksAutoDerivedAgentAsRuntime(t *testing.T) {
	p, database, repo, _ := newPolicyResolutionFixture(t, "effective-config-runtime-routing")
	agentDir := t.TempDir()
	mockCodex := writeRunnableMockAgent(t, agentDir, "codex")
	missingClaude := filepath.Join(agentDir, "missing-claude")
	globalYAML := "agent: auto\nagent_path_override:\n  claude: " + yamlDoubleQuoted(missingClaude) + "\n  codex: " + yamlDoubleQuoted(mockCodex) + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(globalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "runtime routing provenance", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	yamlBytes := readOwnerOnlyArtifact(t, p.EffectiveConfigYAML(runID))
	defaultAgents := effectiveConfigNode(t, yamlBytes, "agent", "default")
	if len(defaultAgents.Content) != 1 || defaultAgents.Content[0].Value != "codex" {
		t.Fatalf("agent.default = %#v, want runtime-selected codex", defaultAgents)
	}
	if got := effectiveConfigNodeComment(defaultAgents.Content[0]); got != "source=runtime; is_default=false" {
		t.Fatalf("agent.default[0] provenance = %q, want runtime", got)
	}
}

func TestStartRunEffectiveConfigRecordsLayeredProvenance(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "effective-config-provenance")
	globalYAML := `managed: false
auto_fix:
  build: 3
intent:
  disabled_readers: [claude]
prompts:
  shared: |
    global prompt preserved exactly
overrides:
  test/repo:
    commands:
      test: ""
      build:
        runner:
          executable: bash
          args: [-c]
    prompts:
      shared: |
        override prompt preserved exactly
`
	if err := os.WriteFile(p.ConfigFile(), []byte(globalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	trustedYAML := "auto_fix:\n  lint: 0\n  test: 0\n  review: 0\n" +
		"hooks:\n  post_worktree: " + yamlDoubleQuoted("echo ran > "+marker) + "\n" +
		"commands:\n  build: trusted-build\n" +
		"refresh:\n  strategy: merge\n" +
		"document:\n  instructions: preserve trusted docs\n"
	writePolicyConfigCommit(t, repo, trustedYAML, "configure trusted policy", "refs/heads/main")
	pushedYAML := `auto_fix:
  lint: 0
  test: 0
  review: 0
ignore_patterns: [pushed/**]
intent:
  disabled_readers: [codex]
commands:
  build: ignored-pushed-build
`
	writePolicyConfigCommit(t, repo, pushedYAML, "configure pushed policy", "refs/heads/feature/effective-config")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	runID, err := manager.startRun(context.Background(), repo, "feature/effective-config", head, refreshTestZeroSHA, "test", []types.StepName{types.StepReview}, "effective config", "", types.RefreshStrategyRebase, "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	yamlBytes := readOwnerOnlyArtifact(t, p.EffectiveConfigYAML(runID))
	metaBytes := readOwnerOnlyArtifact(t, p.EffectiveConfigMeta(runID))
	yamlText := string(yamlBytes)
	for _, want := range []string{
		"managed: false # source=global; is_default=false",
		"build: 3 # source=global; is_default=false",
		"run: trusted-build # source=trusted; is_default=false",
		"executable: bash # source=global-override; is_default=false",
		"test: \"\" # source=global-override; is_default=false; qualifier=clear",
		"- pushed/** # source=pushed; is_default=false",
		"review: 0 # source=pushed; is_default=false",
		"source=global-override; is_default=false; qualifier=append",
		"- codex # source=pushed; is_default=false",
		"refresh_strategy: rebase # source=run-request; is_default=false",
		"source=run-request; is_default=false",
		"source=runtime; is_default=false",
		"global prompt preserved exactly",
		"override prompt preserved exactly",
	} {
		if !strings.Contains(yamlText, want) {
			t.Errorf("effective config missing %q:\n%s", want, yamlText)
		}
	}
	if strings.Contains(yamlText, "ignored-pushed-build") {
		t.Fatalf("effective config included an untrusted ignored command:\n%s", yamlText)
	}
	for _, privateValue := range []string{"test/repo", marker, "trusted-build", "global prompt preserved exactly"} {
		if strings.Contains(string(metaBytes), privateValue) {
			t.Fatalf("value-free sidecar contains private configuration value %q: %s", privateValue, metaBytes)
		}
	}
	var decoded struct {
		Prompts struct {
			Shared string `yaml:"shared"`
		} `yaml:"prompts"`
	}
	if err := yaml.Unmarshal(yamlBytes, &decoded); err != nil {
		t.Fatal(err)
	}
	if want := "global prompt preserved exactly\n\noverride prompt preserved exactly"; decoded.Prompts.Shared != want {
		t.Fatalf("effective prompt = %q, want exact %q", decoded.Prompts.Shared, want)
	}
}

func TestStartRunRejectsOversizedEffectiveConfigBeforeRunOrWorktreeCreation(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "effective-config-oversized")
	preflightMarker := filepath.Join(t.TempDir(), "preflight-ran")
	trustedYAML := validPolicyResolutionConfig(marker) + "preflight:\n  - " + yamlDoubleQuoted("echo ran > "+preflightMarker) + "\n"
	writePolicyConfigCommit(t, repo, trustedYAML, "configure preflight", "refs/heads/main")
	globalYAML := "prompts:\n  shared: " + yamlDoubleQuoted(strings.Repeat("x", effectiveConfigYAMLMaxBytes)) + "\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(globalYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "oversized effective config", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "must not exceed 262144 bytes")
	assertNoEffectiveConfigArtifacts(t, p)
	if _, err := os.Stat(preflightMarker); err != nil {
		t.Fatalf("preflight did not complete before effective config rendering: %v", err)
	}
}

func TestStartRunRejectsEffectiveConfigPersistenceFailureBeforeRunOrWorktreeCreation(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "effective-config-persist-failure")
	if err := os.RemoveAll(p.RunsDir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.RunsDir(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "persistence failure", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "create runs directory")
}

func TestValidateEffectiveConfigArtifactsRejectsIntegrityMismatch(t *testing.T) {
	yamlBytes := []byte("enabled: true # source=global; is_default=true\n")
	digest := sha256.Sum256(yamlBytes)
	meta, err := json.Marshal(effectiveConfigMetadata{
		SchemaVersion:   effectiveConfigSchemaVersion,
		RunID:           "run-1",
		PolicyDigest:    strings.Repeat("a", 64),
		YAMLSHA256:      hex.EncodeToString(digest[:]),
		BinaryVersion:   "test",
		BinaryBuildSHA:  "build",
		Generator:       effectiveConfigGenerator,
		GeneratorSchema: effectiveConfigSchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateEffectiveConfigArtifacts(append(yamlBytes, ' '), meta, "run-1", strings.Repeat("a", 64)); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("integrity error = %v", err)
	}
}

func TestPersistEffectiveConfigArtifactsRemovesPartialStagingOnIntegrityFailure(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	err := persistEffectiveConfigArtifacts(p, "run-1", &effectiveConfigArtifacts{
		YAML:         []byte("enabled: true # source=global; is_default=true\n"),
		Meta:         []byte("{not-json}\n"),
		PolicyDigest: strings.Repeat("a", 64),
	})
	if err == nil || !strings.Contains(err.Error(), "sidecar") {
		t.Fatalf("persist integrity error = %v", err)
	}
	assertNoEffectiveConfigArtifacts(t, p)
}

func TestReapEffectiveConfigArtifactDirsRemovesCrashOrphans(t *testing.T) {
	f := newEvidenceFixture(t)
	if err := os.MkdirAll(f.p.RunsDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	retained, err := f.db.InsertRun(f.repo.ID, "retained", "head-retained", "base-retained")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	pathsByState := map[string]string{
		"retained":      f.p.RunDir(retained.ID),
		"orphan-final":  f.p.RunDir("01M1ORPHANEDFINALARTIFACT00"),
		"stale-staging": filepath.Join(f.p.RunsDir(), ".01M1STALE-staging"),
		"new-staging":   filepath.Join(f.p.RunsDir(), ".01M1NEW-staging"),
	}
	for state, path := range pathsByState {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatalf("create %s directory: %v", state, err)
		}
	}
	stale := now.Add(-effectiveConfigStagingMaxAge - time.Minute)
	if err := os.Chtimes(pathsByState["stale-staging"], stale, stale); err != nil {
		t.Fatal(err)
	}

	reapEffectiveConfigArtifactDirs(f.db, f.p, now)

	for _, state := range []string{"retained", "new-staging"} {
		if _, err := os.Stat(pathsByState[state]); err != nil {
			t.Fatalf("%s directory was not preserved: %v", state, err)
		}
	}
	for _, state := range []string{"orphan-final", "stale-staging"} {
		if _, err := os.Stat(pathsByState[state]); !os.IsNotExist(err) {
			t.Fatalf("%s directory survived crash cleanup: %v", state, err)
		}
	}
}

func readOwnerOnlyArtifact(t *testing.T, path string) []byte {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("%s mode = %#o, want 0600", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func assertOwnerOnlyDirectory(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("%s mode = %#o, want 0700", path, info.Mode().Perm())
	}
}

func assertNoEffectiveConfigArtifacts(t *testing.T, p *paths.Paths) {
	t.Helper()
	entries, err := os.ReadDir(p.RunsDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("effective config artifact entries = %d, want none", len(entries))
	}
}

func sortedMapKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func assertEffectiveConfigScalar(t *testing.T, data []byte, path []string, wantValue, wantProvenance string) {
	t.Helper()
	node := effectiveConfigNode(t, data, path...)
	if node.Kind != yaml.ScalarNode || node.Value != wantValue {
		t.Fatalf("effective config %s = kind %d value %q, want scalar %q", strings.Join(path, "."), node.Kind, node.Value, wantValue)
	}
	if got := effectiveConfigNodeComment(node); got != wantProvenance {
		t.Fatalf("effective config %s provenance = %q, want %q", strings.Join(path, "."), got, wantProvenance)
	}
}

func effectiveConfigNode(t *testing.T, data []byte, path ...string) *yaml.Node {
	t.Helper()
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Content) != 1 {
		t.Fatalf("effective config has %d roots, want one", len(document.Content))
	}
	node := document.Content[0]
	for _, key := range path {
		if node.Kind != yaml.MappingNode {
			t.Fatalf("effective config path %s reaches non-mapping node", strings.Join(path, "."))
		}
		var next *yaml.Node
		for i := 0; i+1 < len(node.Content); i += 2 {
			if node.Content[i].Value == key {
				next = node.Content[i+1]
				break
			}
		}
		if next == nil {
			t.Fatalf("effective config path %s is missing key %q", strings.Join(path, "."), key)
		}
		node = next
	}
	return node
}

func effectiveConfigNodeComment(node *yaml.Node) string {
	if node.LineComment != "" {
		return strings.TrimPrefix(node.LineComment, "# ")
	}
	return strings.TrimPrefix(node.HeadComment, "# ")
}
