package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRefreshStep_RebasesOntoStackedBranch(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupStackedRefreshRepo(t)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyRebase
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream

	if _, err := (&RefreshStep{}).Execute(sctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dependency.txt")); err != nil {
		t.Fatalf("stacked branch change missing after refresh: %v", err)
	}
	gitCmd(t, dir, "merge-base", "--is-ancestor", "origin/dependency", "HEAD")
	parents := strings.Fields(gitCmd(t, dir, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 2 {
		t.Fatalf("rebased HEAD parents = %v, want one parent", parents)
	}
}

func TestRefreshStepEvidenceKeepsPrimaryOperationAndOmitsFetchPlumbing(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupStackedRefreshRepo(t)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyRebase
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepRefresh)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	sctx.Round = 1

	if _, err := (&RefreshStep{}).Execute(sctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	stored, err := sctx.DB.GetStepResult(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := stored.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Commands) == 0 {
		t.Fatal("refresh evidence has no primary operation")
	}
	for _, command := range evidence.Commands {
		if strings.HasPrefix(command.Command, "git fetch ") {
			t.Fatalf("refresh evidence exposed support plumbing: %+v", evidence.Commands)
		}
	}
	if !strings.HasPrefix(evidence.Commands[len(evidence.Commands)-1].Command, "git rebase ") {
		t.Fatalf("refresh evidence = %+v, want a rebase operation", evidence.Commands)
	}
}

func TestRefreshStep_MergesStackedBranch(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupStackedRefreshRepo(t)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyMerge
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream

	if _, err := (&RefreshStep{}).Execute(sctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dependency.txt")); err != nil {
		t.Fatalf("stacked branch change missing after refresh: %v", err)
	}
	gitCmd(t, dir, "merge-base", "--is-ancestor", "origin/dependency", "HEAD")
	gitCmd(t, dir, "merge-base", "--is-ancestor", featureHead, "HEAD")
	parents := strings.Fields(gitCmd(t, dir, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 3 {
		t.Fatalf("merge HEAD parents = %v, want two parents", parents)
	}
}

func TestRefreshStep_FetchesLatestStackedBranchBeforeRefresh(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupStackedRefreshRepo(t)
	other := t.TempDir()
	gitCmd(t, other, "clone", upstream, ".")
	gitCmd(t, other, "config", "user.name", "test")
	gitCmd(t, other, "config", "user.email", "test@test.com")
	gitCmd(t, other, "checkout", "dependency")
	os.WriteFile(filepath.Join(other, "fresh.txt"), []byte("fresh\n"), 0o644)
	gitCmd(t, other, "add", "-A")
	gitCmd(t, other, "commit", "-m", "fresh dependency")
	gitCmd(t, other, "push", "origin", "dependency")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyRebase
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream
	if _, err := (&RefreshStep{}).Execute(sctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fresh.txt")); err != nil {
		t.Fatalf("latest stacked-branch commit missing: %v", err)
	}
}

func TestRefreshStep_MergeConflictRequiresApproval(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupConflictingStackedRefreshRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyMerge
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream

	outcome, err := (&RefreshStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want auto-fixable approval", outcome)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("decode findings: %v", err)
	}
	if len(findings.Items) != 1 || !strings.Contains(findings.Items[0].Description, "merge conflict merging origin/dependency") {
		t.Fatalf("findings = %+v, want dependency merge conflict", findings.Items)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != featureHead {
		t.Fatalf("HEAD = %s, want aborted merge head %s", got, featureHead)
	}
	if mergeInProgress(sctx.Ctx, dir) {
		t.Fatal("merge remained in progress after conflict finding")
	}
}

func TestRefreshStep_MergeConflictFixUsesAgent(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupConflictingStackedRefreshRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("resolved\n"), 0o644); err != nil {
				return nil, err
			}
			for _, args := range [][]string{{"add", "shared.txt"}, {"merge", "--continue"}} {
				cmd := exec.CommandContext(ctx, "git", args...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GIT_EDITOR=true")
				if out, err := cmd.CombinedOutput(); err != nil {
					return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), out, err)
				}
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolve stacked merge conflict"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyMerge
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepRefresh)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	sctx.Round = 1

	outcome, err := (&RefreshStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if outcome.NeedsApproval || len(ag.calls) != 1 {
		t.Fatalf("outcome = %+v, agent calls = %d", outcome, len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "git merge --continue") || !strings.Contains(ag.calls[0].Prompt, "shared.txt") {
		t.Fatalf("merge conflict prompt missing instructions: %s", ag.calls[0].Prompt)
	}
	gitCmd(t, dir, "merge-base", "--is-ancestor", "origin/dependency", "HEAD")
	parents := strings.Fields(gitCmd(t, dir, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 3 {
		t.Fatalf("resolved merge parents = %v, want two parents", parents)
	}
	stored, err := sctx.DB.GetStepResult(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := stored.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Commands) != 2 {
		t.Fatalf("merge command evidence = %+v, want failed merge and successful continuation", evidence.Commands)
	}
	failed := evidence.Commands[0]
	if failed.Command != "git merge --no-edit origin/dependency" || failed.Outcome != db.CommandOutcomeFailed || failed.ExitCode == nil || *failed.ExitCode != 1 {
		t.Fatalf("failed merge evidence = %+v", failed)
	}
	continued := evidence.Commands[1]
	if continued.Command != "git merge --continue" || continued.Outcome != db.CommandOutcomePassed || continued.ExitCode == nil || *continued.ExitCode != 0 {
		t.Fatalf("continued merge evidence = %+v", continued)
	}
}

func setupStackedRefreshRepo(t *testing.T) (dir, upstream, featureHead string) {
	t.Helper()
	upstream = t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "dependency")
	os.WriteFile(filepath.Join(dir, "dependency.txt"), []byte("dependency\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "dependency")
	gitCmd(t, dir, "push", "origin", "dependency")

	gitCmd(t, dir, "checkout", "main")
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	gitCmd(t, dir, "push", "origin", "feature")
	featureHead = gitCmd(t, dir, "rev-parse", "HEAD")
	return dir, upstream, featureHead
}

func setupConflictingStackedRefreshRepo(t *testing.T) (dir, upstream, featureHead string) {
	t.Helper()
	upstream = t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "dependency")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("dependency\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "dependency")
	gitCmd(t, dir, "push", "origin", "dependency")

	gitCmd(t, dir, "checkout", "main")
	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("feature\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	gitCmd(t, dir, "push", "origin", "feature")
	featureHead = gitCmd(t, dir, "rev-parse", "HEAD")
	return dir, upstream, featureHead
}
