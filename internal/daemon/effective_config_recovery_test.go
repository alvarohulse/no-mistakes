package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/effectiveconfig"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPrepareRecoveredRunValidatesRequiredEffectiveConfigBeforeReadingCurrentConfig(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, headSHA := setupTestGitRepo(t, p, database, "effective-config-recovery-order")
	stepPlan := []pipeline.Step{policyTestStep{name: types.StepReview}}
	policy, err := resolvedPolicyFromConfig(resolvedRoutingTestConfig(), nil, stepPlan, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	policy.Version = 9
	encoded, digest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithOptions(repo.ID, "main", headSHA, headSHA, db.RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunResolvedPolicy(run.ID, encoded, digest); err != nil {
		t.Fatal(err)
	}
	run.ResolvedPolicy = &encoded
	run.ResolvedPolicyDigest = &digest
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	worktree := p.WorktreeDir(repo.ID, run.ID)
	if err := gitpkg.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), worktree, headSHA); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"needs approval"}`
	if err := database.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRound(step.ID, 1, "initial", &findings, nil, headSHA, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(step.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("commands: [malformed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := NewRunManager(database, p, func() []pipeline.Step { return stepPlan })
	t.Cleanup(manager.Shutdown)
	_, err = manager.prepareRecoveredRun(context.Background(), run)
	if err == nil || !strings.Contains(err.Error(), "read effective config YAML") {
		t.Fatalf("prepareRecoveredRun() error = %v, want missing-artifact failure before current config read", err)
	}
	if plans := manager.recoverableParkedRuns(context.Background()); len(plans) != 0 {
		t.Fatalf("recoverableParkedRuns() returned %d plans, want none", len(plans))
	}
	failed, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != types.RunFailed || failed.Error == nil || !strings.Contains(*failed.Error, "read effective config YAML") {
		t.Fatalf("rejected recovery = status %s error %v, want durable effective-config failure", failed.Status, failed.Error)
	}
	failedStep, err := database.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failedStep.Status != types.StepStatusFailed || failedStep.Error == nil || !strings.Contains(*failedStep.Error, "read effective config YAML") {
		t.Fatalf("rejected recovery step = status %s error %v, want matching durable failure", failedStep.Status, failedStep.Error)
	}
}

func TestRunEffectiveConfigTreatsPreRequirementPoliciesAsUnavailable(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "legacy-effective-config")
	policy, err := resolvedPolicyFromConfig(resolvedRoutingTestConfig(), nil, []pipeline.Step{policyTestStep{name: types.StepReview}}, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	policy.Version = 8
	encoded, digest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}
	run := &db.Run{ID: "legacy-run", RepoID: repo.ID, ResolvedPolicy: &encoded, ResolvedPolicyDigest: &digest}
	artifact, required, err := ReadEffectiveConfigForRun(p, run)
	if err != nil || required || artifact != nil {
		t.Fatalf("readRunEffectiveConfig() = artifact %#v required %t error %v, want legacy unavailable", artifact, required, err)
	}
}

func TestRunEffectiveConfigExposesAnIntactVersionEightArtifact(t *testing.T) {
	p, database := newRefreshRunFixture(t)
	repo, _ := setupTestGitRepo(t, p, database, "version-eight-effective-config")
	policy, err := resolvedPolicyFromConfig(resolvedRoutingTestConfig(), nil, []pipeline.Step{policyTestStep{name: types.StepReview}}, nil, types.RefreshStrategyRebase, false)
	if err != nil {
		t.Fatal(err)
	}
	policy.Version = 8
	encoded, digest, err := marshalResolvedPolicyDTO(policy)
	if err != nil {
		t.Fatal(err)
	}
	run := &db.Run{ID: "version-eight-run", RepoID: repo.ID, ResolvedPolicy: &encoded, ResolvedPolicyDigest: &digest}
	yamlBytes := []byte("enabled: true # source=runtime; is_default=false\n")
	writeEffectiveConfigRecoveryFixture(t, p.RunDir(run.ID), run.ID, digest, yamlBytes)

	artifact, required, err := ReadEffectiveConfigForRun(p, run)
	if err != nil || required || artifact == nil || string(artifact.YAML) != string(yamlBytes) {
		t.Fatalf("ReadEffectiveConfigForRun() = artifact %#v required %t error %v, want optional intact v8 artifact", artifact, required, err)
	}
}

func writeEffectiveConfigRecoveryFixture(t *testing.T, dir, runID, policyDigest string, yamlBytes []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(yamlBytes)
	metaBytes, err := json.Marshal(effectiveconfig.Metadata{
		SchemaVersion: effectiveconfig.SchemaVersion, RunID: runID, PolicyDigest: policyDigest,
		YAMLSHA256: hex.EncodeToString(digest[:]), BinaryVersion: "test", BinaryBuildSHA: "test",
		Generator: effectiveconfig.Generator, GeneratorSchema: effectiveconfig.SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "effective-config.yaml"), yamlBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "effective-config.meta.json"), metaBytes, 0o600); err != nil {
		t.Fatal(err)
	}
}
