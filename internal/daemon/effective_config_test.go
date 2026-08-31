package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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
