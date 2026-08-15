package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestResolvePolicyConcurrentRequestsUsePrivateTrustedRefs(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, _ := newPolicyResolutionFixture(t, "concurrent-explain")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepReview}} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	const requests = 20
	errors := make(chan error, requests)
	var group sync.WaitGroup
	for range requests {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := manager.ResolvePolicy(context.Background(), repo, head, nil, "")
			errors <- err
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("concurrent policy resolution: %v", err)
		}
	}
	refs, err := git.RunBare(context.Background(), p.RepoDir(repo.ID), "for-each-ref", "--format=%(refname)", "refs/no-mistakes/policy-resolution")
	if err != nil {
		t.Fatal(err)
	}
	if refs != "" {
		t.Fatalf("temporary policy refs leaked:\n%s", refs)
	}
}

func TestReapPolicyTrustedRefsPreservesOnlyActiveRunRefs(t *testing.T) {
	p, database, repo, _ := newPolicyResolutionFixture(t, "policy-ref-reap")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	active, err := database.InsertRun(repo.ID, "active", head, refreshTestZeroSHA)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := database.InsertRun(repo.ID, "terminal", head, refreshTestZeroSHA)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(terminal.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	refs := []string{
		policyTrustedRunRef(active.ID),
		policyTrustedRunRef(terminal.ID),
		policyTrustedRunRef("missing-run"),
		"refs/no-mistakes/policy-resolution/crashed",
	}
	for _, ref := range refs {
		if _, err := git.RunBare(context.Background(), gateDir, "update-ref", ref, head); err != nil {
			t.Fatal(err)
		}
	}
	setSafeBareRepositoryExplicitForDaemonTest(t)

	if got := reapPolicyTrustedRefs(context.Background(), database, p); got != 3 {
		t.Fatalf("reaped refs = %d, want 3", got)
	}
	out, err := git.RunBare(context.Background(), gateDir, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/policy-resolution", "refs/no-mistakes/policy-run")
	if err != nil {
		t.Fatal(err)
	}
	if out != policyTrustedRunRef(active.ID) {
		t.Fatalf("remaining refs = %q, want active run ref", out)
	}
}

func TestResolvedPolicyExplanationMatchesPersistedRunPolicy(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, _ := newPolicyResolutionFixture(t, "explain-matches-run")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	explanation, err := manager.ResolvePolicy(context.Background(), repo, head, nil, "")
	if err != nil {
		t.Fatalf("resolve policy explanation: %v", err)
	}
	encoded, err := explanation.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		PolicyDigest string          `json:"policy_digest"`
		Policy       json.RawMessage `json:"policy"`
	}
	if err := json.Unmarshal([]byte(encoded), &envelope); err != nil {
		t.Fatal(err)
	}
	runID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "matching policy", "", "", "")
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	if run := waitForRunTerminalState(t, database, runID); run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ResolvedPolicy == nil || *run.ResolvedPolicy != string(envelope.Policy) {
		t.Fatalf("persisted policy differs from explanation:\n%s\n%v", envelope.Policy, run.ResolvedPolicy)
	}
	if run.ResolvedPolicyDigest == nil || *run.ResolvedPolicyDigest != envelope.PolicyDigest {
		t.Fatalf("persisted digest = %v, want %s", run.ResolvedPolicyDigest, envelope.PolicyDigest)
	}
}

func TestStartRunRejectsMissingCandidateBeforeCreatingRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "missing-candidate")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "feature/missing", strings.Repeat("1", 40), refreshTestZeroSHA, "test", nil, "missing candidate", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "candidate head")
}

func TestStartRunRejectsMalformedGlobalConfigBeforeCreatingRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "malformed-global")
	if err := os.WriteFile(p.ConfigFile(), []byte("unknown_global_field: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "malformed global config", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "load global config")
}

func TestStartRunRejectsMissingTrustedCommitBeforeCreatingRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "missing-trusted")
	gateDir := p.RepoDir(repo.ID)
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/feature/missing-trusted")
	gitCmd(t, "", "--git-dir="+gateDir, "update-ref", "-d", "refs/heads/main")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "feature/missing-trusted", head, refreshTestZeroSHA, "test", nil, "missing trusted commit", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "trusted default branch")
}

func TestStartRunRejectsMalformedPushedConfigBeforeCreatingRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "malformed-pushed")
	writePolicyConfigCommit(t, repo, "commands:\n  build: [unterminated\n", "malformed pushed config", "refs/heads/feature/malformed-pushed")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "feature/malformed-pushed", head, refreshTestZeroSHA, "test", nil, "malformed pushed config", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "pushed .no-mistakes.yaml")
}

func TestStartRunRejectsMalformedTrustedConfigBeforeCreatingRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "malformed-trusted")
	candidate := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/feature/malformed-trusted")
	writePolicyConfigCommit(t, repo, "disable_project_settings: : malformed\n", "malformed trusted config", "refs/heads/main")
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "feature/malformed-trusted", candidate, refreshTestZeroSHA, "test", nil, "malformed trusted config", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "trusted .no-mistakes.yaml")
}

func TestStartRunRejectsPresentUnreadablePushedConfigBeforeCreatingRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, marker := newPolicyResolutionFixture(t, "unreadable-pushed")
	content := validPolicyResolutionConfig(marker) + "ignore_patterns: [unique-unreadable-pushed/**]\n"
	writePolicyConfigCommit(t, repo, content, "unreadable pushed config", "refs/heads/feature/unreadable-pushed")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	blob := gitOutput(t, repo.WorkingPath, "hash-object", ".no-mistakes.yaml")
	if err := os.Remove(filepath.Join(p.RepoDir(repo.ID), "objects", blob[:2], blob[2:])); err != nil {
		t.Fatalf("remove pushed config blob: %v", err)
	}
	step := &mockPassStep{name: types.StepReview}
	manager := NewRunManager(database, p, func() []pipeline.Step { return []pipeline.Step{step} })
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	_, err := manager.startRun(context.Background(), repo, "feature/unreadable-pushed", head, refreshTestZeroSHA, "test", nil, "unreadable pushed config", "", "", "")
	assertPolicyResolutionFailureHasNoSideEffects(t, p, database, repo, marker, step, err, "present but unreadable")
}

func TestLoadBareRepoConfigDistinguishesAbsentFromPresentUnreadable(t *testing.T) {
	p, _, repo, marker := newPolicyResolutionFixture(t, "bare-config-read")
	gateDir := p.RepoDir(repo.ID)
	if err := os.Remove(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml")); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", "remove config")
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:refs/heads/absent-config")
	absentHead := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	content := validPolicyResolutionConfig(marker) + "ignore_patterns: [unique-unreadable-trusted/**]\n"
	writePolicyConfigCommit(t, repo, content, "restore unique config", "refs/heads/unreadable-config")
	unreadableHead := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	blob := gitOutput(t, repo.WorkingPath, "hash-object", ".no-mistakes.yaml")
	if err := os.Remove(filepath.Join(gateDir, "objects", blob[:2], blob[2:])); err != nil {
		t.Fatalf("remove trusted config blob: %v", err)
	}
	setSafeBareRepositoryExplicitForDaemonTest(t)

	absent, err := loadBareRepoConfigInput(context.Background(), gateDir, absentHead, db.ConfigSourceDefault, false)
	if err != nil {
		t.Fatalf("read absent config: %v", err)
	}
	if absent != nil {
		t.Fatalf("absent trusted config = %+v, want nil", absent)
	}
	if _, err := loadBareRepoConfigInput(context.Background(), gateDir, unreadableHead, db.ConfigSourceDefault, false); err == nil || !strings.Contains(err.Error(), "trusted .no-mistakes.yaml") || !strings.Contains(err.Error(), "present but unreadable") {
		t.Fatalf("unreadable trusted config error = %v", err)
	}
}

func TestPolicyResolutionFailureDoesNotSupersedeActiveRun(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	p, database, repo, _ := newPolicyResolutionFixture(t, "preserve-active")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	started := make(chan struct{})
	manager := NewRunManager(database, p, func() []pipeline.Step {
		return []pipeline.Step{&mockSlowStep{name: types.StepReview, started: started}}
	})
	t.Cleanup(manager.Shutdown)
	setSafeBareRepositoryExplicitForDaemonTest(t)

	activeID, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "active run", "", "", "")
	if err != nil {
		t.Fatalf("start active run: %v", err)
	}
	<-started
	if err := os.WriteFile(p.ConfigFile(), []byte("overrides:\n  malformed-key: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.startRun(context.Background(), repo, "main", head, refreshTestZeroSHA, "test", nil, "invalid replacement", "", "", ""); err == nil {
		t.Fatal("invalid replacement run was accepted")
	}
	active, err := database.GetRun(activeID)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.Status != types.RunRunning {
		t.Fatalf("active run status = %v, want running", active)
	}
}

func newPolicyResolutionFixture(t *testing.T, repoID string) (*paths.Paths, *db.DB, *db.Repo, string) {
	t.Helper()
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, repoID)
	marker := filepath.Join(t.TempDir(), "post-worktree-ran")
	writePolicyConfigCommit(t, repo, validPolicyResolutionConfig(marker), "configure policy", "refs/heads/main")
	return p, database, repo, marker
}

func validPolicyResolutionConfig(marker string) string {
	return "auto_fix:\n  lint: 0\n  test: 0\n  review: 0\n" +
		"hooks:\n  post_worktree: " + yamlDoubleQuoted("echo ran > "+marker) + "\n" +
		"commands:\n  build: echo build\n" +
		"ignore_patterns: [vendor/**]\n"
}

func writePolicyConfigCommit(t *testing.T, repo *db.Repo, content, message, gateRef string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo.WorkingPath, ".no-mistakes.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "add", ".no-mistakes.yaml")
	gitCmd(t, repo.WorkingPath, "commit", "-m", message)
	gitCmd(t, repo.WorkingPath, "push", "gate", "HEAD:"+gateRef)
}

func assertPolicyResolutionFailureHasNoSideEffects(t *testing.T, p *paths.Paths, database *db.DB, repo *db.Repo, marker string, step *mockPassStep, runErr error, wantError string) {
	t.Helper()
	if runErr == nil || !strings.Contains(runErr.Error(), wantError) {
		t.Fatalf("error = %v, want %q", runErr, wantError)
	}
	runs, err := database.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %d, want none", len(runs))
	}
	entries, err := os.ReadDir(filepath.Join(p.WorktreesDir(), repo.ID))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("worktrees = %d, want none", len(entries))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("post-worktree hook marker exists or cannot be checked: %v", err)
	}
	if got := step.execCnt.Load(); got != 0 {
		t.Fatalf("pipeline executions = %d, want none", got)
	}
	invocations, err := database.AgentInvocationAggregates()
	if err != nil {
		t.Fatal(err)
	}
	if len(invocations) != 0 {
		t.Fatalf("agent invocation aggregates = %d, want none", len(invocations))
	}
}

func setSafeBareRepositoryExplicitForDaemonTest(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
}
