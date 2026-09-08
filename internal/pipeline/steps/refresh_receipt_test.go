package steps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/artifact"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func refreshGitScriptEnv(t *testing.T, body string) ([]string, string) {
	t.Helper()
	binDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "git-started")
	script := "#!/bin/sh\nprintf started > \"$REFRESH_MARKER\"\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + binDir, "REFRESH_MARKER=" + marker}, marker
}

func beginRefreshReceiptRound(t *testing.T, sctx *pipeline.StepContext) *db.StepRound {
	t.Helper()
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepRefresh)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.BeginStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1
	sctx.RoundID = round.ID
	sctx.RoundTrigger = "initial"
	t.Cleanup(func() {
		if err := sctx.DB.CompleteStepRound(round.ID, nil, nil, 0); err != nil {
			t.Errorf("complete refresh receipt test round: %v", err)
		}
	})
	return round
}

func TestRefreshReceiptCapturesMultipleAttemptsInDurableOrder(t *testing.T) {
	dir, _, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, headSHA, headSHA, config.Commands{})
	beginRefreshReceiptRound(t, sctx)
	definition, err := sctx.DB.EnsureCommandDefinition(sctx.Run.ID, runner.Resolved{
		Script:        "git merge --no-edit origin/main",
		CommandSource: runner.SourceDirectGit,
		Provenance: runner.Provenance{
			SchemaVersion: runner.SchemaVersion,
			Platform:      runtime.GOOS,
			Source:        runner.SourceDirectGit,
			Executable:    "git",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	startAttempt := func(sequence int) *db.CommandAttempt {
		t.Helper()
		attempt, err := sctx.DB.StartCommandAttempt(db.CommandAttempt{
			RunID: sctx.Run.ID, CommandID: definition.ID, StepID: sctx.StepResultID, RoundID: sctx.RoundID,
			Sequence: sequence, Purpose: string(types.StepRefresh), Observer: db.CommandObserverController,
			Trigger: sctx.RoundTrigger, BeforeSHA: headSHA, CommandSource: runner.SourceDirectGit,
			RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDirectGit,
		})
		if err != nil {
			t.Fatal(err)
		}
		return attempt
	}
	startAttempt(1)
	recorder := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main")
	operation := recorder.begin("origin/main")
	before, err := operation.refreshScopeAttemptIDs()
	if err != nil {
		t.Fatal(err)
	}
	firstNew := startAttempt(2)
	secondNew := startAttempt(3)
	if err := operation.captureAttemptsStartedAfter(before); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(operation.commandAttemptIDs, ","), firstNew.ID+","+secondNew.ID; got != want {
		t.Fatalf("captured attempt order = %q, want %q", got, want)
	}
}

func TestRefreshStepRecordsTargetDecisionsAndPrimaryArtifacts(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupStackedRefreshRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	// Push notifications may retain only the parsed branch name. Receipts still
	// need to expose the canonical Git ref rather than a transport-specific form.
	sctx.Run.Branch = "feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyRebase
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream
	beginRefreshReceiptRound(t, sctx)

	if _, err := (&RefreshStep{}).Execute(sctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 2 {
		t.Fatalf("refresh operations = %+v, want one decision per target", operations)
	}

	var skipped, rebased *db.RefreshOperation
	for _, operation := range operations {
		switch operation.DestinationRef {
		case "origin/feature":
			skipped = operation
		case "origin/dependency":
			rebased = operation
		}
		if operation.SourceRef != "refs/heads/feature" || operation.AuthoritativeBaseRef != "origin/dependency" {
			t.Fatalf("operation identity = %+v", operation)
		}
	}
	if skipped == nil || skipped.Decision != db.RefreshDecisionSkipped || len(skipped.CommandAttemptIDs) != 0 {
		t.Fatalf("pushed-branch receipt = %+v", skipped)
	}
	if rebased == nil || rebased.Decision != db.RefreshDecisionRebased || rebased.ConflictState != db.RefreshConflictStateNone || len(rebased.CommandAttemptIDs) != 1 {
		t.Fatalf("base refresh receipt = %+v", rebased)
	}
	if rebased.StartingHeadSHA == nil || *rebased.StartingHeadSHA != featureHead || rebased.ResultingHeadSHA == nil || *rebased.ResultingHeadSHA == featureHead || rebased.AuthoritativeBaseSHA == nil || *rebased.AuthoritativeBaseSHA == "" {
		t.Fatalf("base refresh identities = %+v", rebased)
	}
	if rebased.ResolvedTargetHeadSHA == nil || *rebased.ResolvedTargetHeadSHA != *rebased.AuthoritativeBaseSHA {
		t.Fatalf("base refresh resolved target = %+v", rebased)
	}

	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].ID != rebased.CommandAttemptIDs[0] || attempts[0].OutputArtifactID == nil || attempts[0].RunnerSource != runner.SourceDirectGit {
		t.Fatalf("refresh command attempts = %+v", attempts)
	}
	definitions, err := sctx.DB.GetCommandDefinitionsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || definitions[0].RunnerExecutable != "git" || len(definitions[0].RunnerArgs) != 0 {
		t.Fatalf("refresh command definitions = %+v", definitions)
	}
	registered, err := sctx.DB.GetArtifact(*attempts[0].OutputArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if registered == nil || registered.Purpose != db.ArtifactPurposeCommandOutput {
		t.Fatalf("refresh output artifact = %+v", registered)
	}
	store, err := artifact.NewStore(sctx.Paths, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(registered); err != nil {
		t.Fatalf("read refresh output artifact: %v", err)
	}
}

func TestRefreshStepRunsPrimaryGitNoninteractively(t *testing.T) {
	dir, upstream, featureHead := setupStackedRefreshRepo(t)

	t.Setenv("GIT_EDITOR", "vim")
	t.Setenv("GIT_SEQUENCE_EDITOR", "vim")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[credential \"https://github.com\"]\n\thelper = !gh auth git-credential\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte("export GIT_EDITOR=profile\nexport GIT_SEQUENCE_EDITOR=profile\nexport GIT_TERMINAL_PROMPT=1\nexport GIT_OPTIONAL_LOCKS=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "git")
	logFile := filepath.Join(t.TempDir(), "git.log")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyRebase
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Runner = runner.Spec{Executable: "bash", Args: []string{"-lc"}}
	sctx.Env = fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":     "git-require-noninteractive-env",
		"FAKE_CLI_REAL_GIT": realGit,
		"FAKE_CLI_LOG":      logFile,
	})
	beginRefreshReceiptRound(t, sctx)
	dependencySHA := gitCmd(t, dir, "rev-parse", "origin/dependency")

	if _, err := (&RefreshStep{}).Execute(sctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	log, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "rebase "+dependencySHA) {
		t.Fatalf("primary rebase did not use step-scoped git: %q", log)
	}
}

func TestRefreshPrimaryPersistsContextTermination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process termination fixture")
	}
	tests := []struct {
		name      string
		wantError error
		wantState string
		deadline  bool
	}{
		{name: "cancelled", wantError: context.Canceled, wantState: db.CommandOutcomeCancelled},
		{name: "deadline", wantError: context.DeadlineExceeded, wantState: db.CommandOutcomeTimeout, deadline: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, _, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, headSHA, headSHA, config.Commands{})
			sctx.Config.ProcessTerminationGrace = 25 * time.Millisecond
			env, marker := refreshGitScriptEnv(t, "while :; do /bin/sleep 1; done")
			sctx.Env = env
			beginRefreshReceiptRound(t, sctx)
			recorder := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main")
			operation := recorder.begin("origin/main")

			var cancel context.CancelFunc
			if tt.deadline {
				sctx.Ctx, cancel = context.WithTimeout(context.Background(), 500*time.Millisecond)
			} else {
				sctx.Ctx, cancel = context.WithCancel(context.Background())
				go func() {
					deadline := time.Now().Add(2 * time.Second)
					for {
						if _, err := os.Stat(marker); err == nil || time.Now().After(deadline) {
							cancel()
							return
						}
						time.Sleep(5 * time.Millisecond)
					}
				}()
			}
			defer cancel()

			output, err := runRefreshPrimary(sctx.Ctx, sctx, operation, "rebase", "origin/main")
			if !errors.Is(err, tt.wantError) {
				t.Fatalf("refresh error = %v, want %v; output = %q", err, tt.wantError, output)
			}
			var exitErr *refreshCommandExitError
			if errors.As(err, &exitErr) {
				t.Fatalf("refresh error = %v, must not be a refresh command exit", err)
			}
			if err := operation.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateNotAttempted, "refresh command terminated"); err != nil {
				t.Fatal(err)
			}

			attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(attempts) != 1 {
				t.Fatalf("command attempts = %+v, want one", attempts)
			}
			attempt := attempts[0]
			if attempt.CompletedAt == nil || attempt.Outcome == nil || *attempt.Outcome != tt.wantState || attempt.OutputArtifactID == nil {
				t.Fatalf("terminated command attempt = %+v", attempt)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("git fixture did not start: %v", err)
			}
		})
	}
}

func TestRefreshReceiptCapturesResultingHeadAfterRunContextCancellation(t *testing.T) {
	dir, baseSHA, startingHeadSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, startingHeadSHA, config.Commands{})
	beginRefreshReceiptRound(t, sctx)
	receipts := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main")
	receipts.authoritativeBaseSHA = refreshStringPointer(baseSHA)
	operation := receipts.begin("origin/main")

	if err := os.WriteFile(filepath.Join(dir, "after.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "after.txt")
	gitCmd(t, dir, "commit", "-m", "advance head")
	resultingHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	ctx, cancel := context.WithCancel(context.Background())
	sctx.Ctx = ctx
	cancel()
	if err := operation.finish(db.RefreshDecisionFastForwarded, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, ""); err != nil {
		t.Fatal(err)
	}

	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ResultingHeadSHA == nil || *operations[0].ResultingHeadSHA != resultingHeadSHA {
		t.Fatalf("resulting head after cancellation = %+v, want %s", operations, resultingHeadSHA)
	}
}

func TestRefreshReceiptPreservesSuccessfulDecisionWhenOutputArtifactPersistenceFails(t *testing.T) {
	tests := []struct {
		name     string
		decision db.RefreshDecision
		run      func(t *testing.T, sctx *pipeline.StepContext, receipts *refreshReceiptRecorder) error
	}{
		{
			name:     "rebase",
			decision: db.RefreshDecisionRebased,
			run: func(t *testing.T, sctx *pipeline.StepContext, receipts *refreshReceiptRecorder) error {
				_, err := tryRebase(context.Background(), sctx, "origin/dependency", receipts)
				return err
			},
		},
		{
			name:     "merge",
			decision: db.RefreshDecisionMerged,
			run: func(t *testing.T, sctx *pipeline.StepContext, receipts *refreshReceiptRecorder) error {
				_, err := tryMerge(context.Background(), sctx, "origin/dependency", receipts)
				return err
			},
		},
		{
			name:     "fast forward",
			decision: db.RefreshDecisionFastForwarded,
			run: func(t *testing.T, sctx *pipeline.StepContext, receipts *refreshReceiptRecorder) error {
				_, err := tryRebase(context.Background(), sctx, "origin/target", receipts)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dir, featureHead string
			if tt.decision == db.RefreshDecisionFastForwarded {
				dir, _, featureHead = setupGitRepo(t)
				gitCmd(t, dir, "checkout", "-b", "target")
				if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("target\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, dir, "add", "target.txt")
				gitCmd(t, dir, "commit", "-m", "target")
				targetSHA := gitCmd(t, dir, "rev-parse", "HEAD")
				gitCmd(t, dir, "update-ref", "refs/remotes/origin/target", targetSHA)
				gitCmd(t, dir, "checkout", "feature")
			} else {
				dir, _, featureHead = setupStackedRefreshRepo(t)
			}

			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
			beginRefreshReceiptRound(t, sctx)
			sctx.Paths = nil // Simulate command-output artifact-store creation failure.
			receipts := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main")
			receipts.authoritativeBaseSHA = refreshStringPointer(featureHead)

			err := tt.run(t, sctx, receipts)
			if !errors.Is(err, errCommandPersistence) {
				t.Fatalf("refresh error = %v, want command persistence failure", err)
			}

			operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			resultingHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
			if len(operations) != 1 || operations[0].Decision != tt.decision || operations[0].ResultingHeadSHA == nil || *operations[0].ResultingHeadSHA != resultingHeadSHA || resultingHeadSHA == featureHead {
				t.Fatalf("refresh operation = %+v, want successful %s at %s", operations, tt.decision, resultingHeadSHA)
			}
		})
	}
}

func TestRefreshReceiptPreservesSuccessfulDecisionWhenAttemptCompletionFails(t *testing.T) {
	dir, _, featureHead := setupStackedRefreshRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	beginRefreshReceiptRound(t, sctx)
	receipts := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main")
	receipts.authoritativeBaseSHA = refreshStringPointer(featureHead)

	originalComplete := completeControllerCommandAttemptWithOutputArtifact
	completeControllerCommandAttemptWithOutputArtifact = func(*db.DB, string, string, *int, *string, *string, *string, db.Artifact) (*db.Artifact, error) {
		return nil, errors.New("injected attempt completion failure")
	}
	t.Cleanup(func() {
		completeControllerCommandAttemptWithOutputArtifact = originalComplete
	})

	dependencySHA := gitCmd(t, dir, "rev-parse", "origin/dependency")
	_, err := tryRebase(context.Background(), sctx, "origin/dependency", receipts)
	if !errors.Is(err, errCommandPersistence) || !strings.Contains(err.Error(), "injected attempt completion failure") {
		t.Fatalf("refresh error = %v, want attempt completion persistence failure", err)
	}

	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	resultingHeadSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	if len(operations) != 1 || operations[0].Decision != db.RefreshDecisionRebased || operations[0].ResultingHeadSHA == nil || *operations[0].ResultingHeadSHA != resultingHeadSHA || resultingHeadSHA == featureHead {
		t.Fatalf("refresh operation = %+v, want successful rebase at %s", operations, resultingHeadSHA)
	}
	if operations[0].Decision == db.RefreshDecisionError {
		t.Fatalf("refresh operation misclassified receipt persistence failure: %+v", operations[0])
	}
	step, err := sctx.DB.GetStepResult(sctx.StepResultID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := step.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Commands) != 1 || evidence.Commands[0].Command != "git rebase "+dependencySHA || evidence.Commands[0].Outcome != db.CommandOutcomePassed || evidence.Commands[0].ExitCode == nil || *evidence.Commands[0].ExitCode != 0 {
		t.Fatalf("successful refresh command evidence = %+v", evidence.Commands)
	}
}

func TestRefreshConflictPreservesReceiptFailures(t *testing.T) {
	tests := []struct {
		name                string
		strategy            types.RefreshStrategy
		repair              bool
		breakReceipt        func(t *testing.T, sctx *pipeline.StepContext)
		wantErrorSubstrings []string
		wantNoDiagnostic    bool
	}{
		{
			name:     "approval returns output artifact failure",
			strategy: types.RefreshStrategyRebase,
			breakReceipt: func(t *testing.T, sctx *pipeline.StepContext) {
				t.Helper()
				sctx.Paths = nil
			},
		},
		{
			name:     "approval joins output and diagnostic artifact failures",
			strategy: types.RefreshStrategyRebase,
			breakReceipt: func(t *testing.T, sctx *pipeline.StepContext) {
				rejectRefreshArtifactWrites(t, sctx)
			},
			wantErrorSubstrings: []string{"create command output artifact", "create refresh diagnostic"},
			wantNoDiagnostic:    true,
		},
		{
			name:     "repair joins output and diagnostic artifact failures",
			strategy: types.RefreshStrategyRebase,
			repair:   true,
			breakReceipt: func(t *testing.T, sctx *pipeline.StepContext) {
				rejectRefreshArtifactWrites(t, sctx)
			},
			wantErrorSubstrings: []string{"create command output artifact", "create refresh diagnostic"},
			wantNoDiagnostic:    true,
		},
		{
			name:     "repair returns output artifact failure",
			strategy: types.RefreshStrategyRebase,
			repair:   true,
			breakReceipt: func(t *testing.T, sctx *pipeline.StepContext) {
				t.Helper()
				sctx.Paths = nil
			},
		},
		{
			name:     "approval returns attempt completion failure",
			strategy: types.RefreshStrategyMerge,
			breakReceipt: func(t *testing.T, _ *pipeline.StepContext) {
				t.Helper()
				originalComplete := completeControllerCommandAttemptWithOutputArtifact
				completeControllerCommandAttemptWithOutputArtifact = func(*db.DB, string, string, *int, *string, *string, *string, db.Artifact) (*db.Artifact, error) {
					return nil, errors.New("injected attempt completion failure")
				}
				t.Cleanup(func() {
					completeControllerCommandAttemptWithOutputArtifact = originalComplete
				})
			},
			wantErrorSubstrings: []string{"injected attempt completion failure"},
		},
		{
			name:     "repair returns attempt completion failure",
			strategy: types.RefreshStrategyMerge,
			repair:   true,
			breakReceipt: func(t *testing.T, _ *pipeline.StepContext) {
				t.Helper()
				originalComplete := completeControllerCommandAttemptWithOutputArtifact
				completeControllerCommandAttemptWithOutputArtifact = func(*db.DB, string, string, *int, *string, *string, *string, db.Artifact) (*db.Artifact, error) {
					return nil, errors.New("injected attempt completion failure")
				}
				t.Cleanup(func() {
					completeControllerCommandAttemptWithOutputArtifact = originalComplete
				})
			},
			wantErrorSubstrings: []string{"injected attempt completion failure"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, _, featureHead := setupConflictingStackedRefreshRepo(t)
			ag := &mockAgent{name: "test"}
			if tt.repair {
				ag = resolvingRefreshConflictAgent(t, dir, tt.strategy)
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, featureHead, featureHead, config.Commands{})
			beginRefreshReceiptRound(t, sctx)
			tt.breakReceipt(t, sctx)
			receipts := newRefreshReceiptRecorder(sctx, tt.strategy, "refs/heads/feature", "origin/main")
			receipts.authoritativeBaseSHA = refreshStringPointer(featureHead)

			var err error
			if tt.repair {
				err = refreshWithAgent(context.Background(), sctx, tt.strategy, "origin/dependency", receipts)
			} else {
				conflictFiles, refreshErr := tryRefresh(context.Background(), sctx, tt.strategy, "origin/dependency", receipts)
				if len(conflictFiles) == 0 {
					t.Fatal("refresh did not report the detected conflict")
				}
				err = refreshErr
			}
			if !errors.Is(err, errCommandPersistence) {
				t.Fatalf("refresh error = %v, want command persistence failure", err)
			}
			for _, want := range tt.wantErrorSubstrings {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refresh error = %v, want %q", err, want)
				}
			}

			operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(operations) != 1 {
				t.Fatalf("refresh operations = %+v, want one", operations)
			}
			operation := operations[0]
			if len(operation.CommandAttemptIDs) != 1 || operation.ResultingHeadSHA == nil {
				t.Fatalf("refresh operation = %+v", operation)
			}
			if tt.wantNoDiagnostic && operation.DiagnosticArtifactID != nil {
				t.Fatalf("refresh operation diagnostic = %q, want unavailable", *operation.DiagnosticArtifactID)
			}
			if tt.repair {
				if operation.Decision != db.RefreshDecisionRepaired || operation.ConflictState != db.RefreshConflictStateResolved || operation.RepairState != db.RefreshRepairStateSucceeded || *operation.ResultingHeadSHA == featureHead {
					t.Fatalf("repaired refresh operation = %+v, want a changed terminal head", operation)
				}
			} else if operation.Decision != db.RefreshDecisionConflicted || operation.ConflictState != db.RefreshConflictStateDetected || operation.RepairState != db.RefreshRepairStateNotAttempted || *operation.ResultingHeadSHA != featureHead {
				t.Fatalf("conflicted refresh operation = %+v, want restored head %s", operation, featureHead)
			}
		})
	}
}

func rejectRefreshArtifactWrites(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "runs"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	sctx.Paths = paths.WithRoot(root)
}

func resolvingRefreshConflictAgent(t *testing.T, dir string, strategy types.RefreshStrategy) *mockAgent {
	t.Helper()
	return &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("resolved\n"), 0o644); err != nil {
				return nil, err
			}
			gitCmd(t, dir, "add", "shared.txt")
			if strategy.OrDefault() == types.RefreshStrategyRebase {
				gitCmd(t, dir, "-c", "core.editor=true", "rebase", "--continue")
			} else {
				gitCmd(t, dir, "-c", "core.editor=true", "merge", "--continue")
			}
			return &agent.Result{}, nil
		},
	}
}

func TestRefreshReceiptUsesLiveStartingHeadForNextTargetAfterCancellation(t *testing.T) {
	dir, baseSHA, startingHeadSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, startingHeadSHA, config.Commands{})
	beginRefreshReceiptRound(t, sctx)
	receipts := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main")
	receipts.authoritativeBaseSHA = refreshStringPointer(baseSHA)

	first := receipts.begin("origin/first")
	if err := os.WriteFile(filepath.Join(dir, "after-first-target.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "after-first-target.txt")
	gitCmd(t, dir, "commit", "-m", "advance after first target")
	afterFirstTargetSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := first.finish(db.RefreshDecisionRebased, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, ""); err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	sctx.Ctx = cancelled
	second := receipts.begin("origin/second")
	if err := second.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateNotAttempted, "cancelled before second target"); err != nil {
		t.Fatal(err)
	}

	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range operations {
		if operation.DestinationRef != "origin/second" {
			continue
		}
		if operation.StartingHeadSHA == nil || *operation.StartingHeadSHA != afterFirstTargetSHA || operation.ResultingHeadSHA == nil || *operation.ResultingHeadSHA != afterFirstTargetSHA {
			t.Fatalf("second target receipt = %+v, want live head %s", operation, afterFirstTargetSHA)
		}
		return
	}
	t.Fatalf("missing second target receipt: %+v", operations)
}

func TestRefreshReceiptMarksUnreadableHeadsUnavailable(t *testing.T) {
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, t.TempDir(), "base", "head", config.Commands{})
	beginRefreshReceiptRound(t, sctx)
	operation := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main").begin("origin/main")

	if err := operation.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateNotAttempted, "unable to read HEAD"); err != nil {
		t.Fatal(err)
	}

	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].StartingHeadSHA != nil || operations[0].ResultingHeadSHA != nil {
		t.Fatalf("unreadable receipt heads = %+v, want unavailable", operations)
	}
}

func TestTryRebasePinsResolvedTargetCommit(t *testing.T) {
	dir, baseSHA, startingHeadSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "target-first.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "target-first.txt")
	gitCmd(t, dir, "commit", "-m", "target first")
	firstTargetSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "target-second.txt"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "target-second.txt")
	gitCmd(t, dir, "commit", "-m", "target second")
	movedTargetSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/target", firstTargetSHA)
	gitCmd(t, dir, "checkout", "feature")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, startingHeadSHA, config.Commands{})
	beginRefreshReceiptRound(t, sctx)
	receipts := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main")
	receipts.authoritativeBaseSHA = refreshStringPointer(baseSHA)
	resolutions := 0
	receipts.resolveTargetRef = func(context.Context, string, string) (string, error) {
		resolutions++
		gitCmd(t, dir, "update-ref", "refs/remotes/origin/target", movedTargetSHA)
		return firstTargetSHA, nil
	}

	conflictFiles, err := tryRebase(context.Background(), sctx, "origin/target", receipts)
	if err != nil {
		t.Fatal(err)
	}
	if len(conflictFiles) != 0 || resolutions != 1 {
		t.Fatalf("refresh result = conflicts %v, resolutions %d", conflictFiles, resolutions)
	}
	if !isAncestor(context.Background(), dir, firstTargetSHA, "HEAD") || isAncestor(context.Background(), dir, movedTargetSHA, "HEAD") {
		t.Fatalf("rebase did not preserve resolved target %s after ref moved to %s", firstTargetSHA, movedTargetSHA)
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].ResolvedTargetHeadSHA == nil || *operations[0].ResolvedTargetHeadSHA != firstTargetSHA {
		t.Fatalf("resolved target receipt = %+v, want %s", operations, firstTargetSHA)
	}
}

func TestTryRebaseClassifiesOnlyVerifiedMissingTargetAsSkipped(t *testing.T) {
	tests := []struct {
		name         string
		targetRef    string
		resolve      func(context.Context, string, string) (string, error)
		wantErr      error
		wantError    bool
		wantDecision db.RefreshDecision
	}{
		{
			name:         "missing target",
			targetRef:    "refs/remotes/origin/missing",
			wantDecision: db.RefreshDecisionSkipped,
		},
		{
			name:      "resolution cancelled",
			targetRef: "HEAD",
			resolve: func(context.Context, string, string) (string, error) {
				return "", context.Canceled
			},
			wantErr:      context.Canceled,
			wantDecision: db.RefreshDecisionCancelled,
		},
		{
			name:      "malformed resolved commit",
			targetRef: "HEAD",
			resolve: func(context.Context, string, string) (string, error) {
				return "not-a-commit", nil
			},
			wantError:    true,
			wantDecision: db.RefreshDecisionError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
			beginRefreshReceiptRound(t, sctx)
			receipts := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main")
			receipts.authoritativeBaseSHA = refreshStringPointer(baseSHA)
			if tt.resolve != nil {
				receipts.resolveTargetRef = tt.resolve
			}

			_, err := tryRebase(context.Background(), sctx, tt.targetRef, receipts)
			if tt.wantErr == nil && !tt.wantError && err != nil {
				t.Fatalf("try rebase error = %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("try rebase error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantError && err == nil {
				t.Fatal("try rebase succeeded for malformed target resolution")
			}

			operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(operations) != 1 || operations[0].Decision != tt.wantDecision || len(operations[0].CommandAttemptIDs) != 0 {
				t.Fatalf("target resolution receipt = %+v, want %s without command attempts", operations, tt.wantDecision)
			}
		})
	}
}

func TestRunStepGitCommandPersistsSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process signal fixture")
	}
	dir, _, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, headSHA, headSHA, config.Commands{})
	sctx.Env, _ = refreshGitScriptEnv(t, "kill -TERM $$")
	beginRefreshReceiptRound(t, sctx)

	output, exitCode, err := runStepGitCommand(sctx, "git signal", string(types.StepRefresh), "signal")
	if err != nil || output != "" || exitCode != -1 {
		t.Fatalf("signal command = output %q exit %d error %v", output, exitCode, err)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != db.CommandOutcomeFail || attempts[0].ExitCode != nil || attempts[0].Signal == nil || *attempts[0].Signal != "terminated" || attempts[0].OutputArtifactID == nil {
		t.Fatalf("signal command attempt = %+v", attempts)
	}
}

func TestRunStepGitCommandDoesNotFabricateSignalForExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process signal fixture")
	}
	dir, _, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, headSHA, headSHA, config.Commands{})
	sctx.Env, _ = refreshGitScriptEnv(t, "exit 7")
	beginRefreshReceiptRound(t, sctx)

	output, exitCode, err := runStepGitCommand(sctx, "git exit", string(types.StepRefresh), "exit")
	if err != nil || output != "" || exitCode != 7 {
		t.Fatalf("exit command = output %q exit %d error %v", output, exitCode, err)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != db.CommandOutcomeFail || attempts[0].ExitCode == nil || *attempts[0].ExitCode != 7 || attempts[0].Signal != nil || attempts[0].OutputArtifactID == nil {
		t.Fatalf("exit command attempt = %+v", attempts)
	}
}

func TestRefreshReceiptAddsDiagnosticWhenCommandOutputIsMissing(t *testing.T) {
	t.Parallel()
	dir, _, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, headSHA, headSHA, config.Commands{})
	beginRefreshReceiptRound(t, sctx)

	definition, err := sctx.DB.EnsureCommandDefinition(sctx.Run.ID, runner.Resolved{
		Script:        "git rebase origin/main",
		CommandSource: runner.SourceBase,
		Provenance: runner.Provenance{
			SchemaVersion: runner.SchemaVersion,
			Platform:      "test",
			Source:        runner.SourceDefault,
			Executable:    "git",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	inputState := "git:" + headSHA
	attempt, err := sctx.DB.StartCommandAttempt(db.CommandAttempt{
		RunID: sctx.Run.ID, CommandID: definition.ID, StepID: sctx.StepResultID, RoundID: sctx.RoundID,
		Sequence: 1, Purpose: string(types.StepRefresh), Observer: db.CommandObserverController,
		Trigger: sctx.RoundTrigger, BeforeSHA: headSHA, InputStateID: &inputState,
		CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
	})
	if err != nil {
		t.Fatal(err)
	}

	recorder := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main")
	recorder.authoritativeBaseSHA = refreshStringPointer(headSHA)
	operation := recorder.begin("origin/main")
	operation.commandAttemptIDs = []string{attempt.ID}
	if err := operation.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateNotAttempted, ""); err != nil {
		t.Fatal(err)
	}

	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].DiagnosticArtifactID == nil {
		t.Fatalf("receipt missing output diagnostic = %+v", operations)
	}
	registered, err := sctx.DB.GetArtifact(*operations[0].DiagnosticArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.NewStore(sctx.Paths, "")
	if err != nil {
		t.Fatal(err)
	}
	diagnostic, err := store.Read(registered)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(diagnostic), "no output artifact") {
		t.Fatalf("missing-output diagnostic = %q", diagnostic)
	}
}

func TestRefreshStepRecordsConflictedDecisionWithFailedPrimaryArtifact(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupConflictingStackedRefreshRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyMerge
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream
	beginRefreshReceiptRound(t, sctx)

	outcome, err := (&RefreshStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("refresh outcome = %+v, want conflict approval", outcome)
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var conflicted *db.RefreshOperation
	for _, operation := range operations {
		if operation.DestinationRef == "origin/dependency" {
			conflicted = operation
		}
	}
	if conflicted == nil || conflicted.Decision != db.RefreshDecisionConflicted || conflicted.ConflictState != db.RefreshConflictStateDetected || conflicted.RepairState != db.RefreshRepairStateNotAttempted || len(conflicted.CommandAttemptIDs) != 1 {
		t.Fatalf("conflicted receipt = %+v", conflicted)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].OutputArtifactID == nil {
		t.Fatalf("conflicted command attempts = %+v", attempts)
	}
}

func TestRefreshReceiptRefusalStoresDiagnosticArtifact(t *testing.T) {
	t.Parallel()
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, t.TempDir(), "base", "head", config.Commands{})
	beginRefreshReceiptRound(t, sctx)
	recorder := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/feature", "origin/main")
	reason := strings.Repeat("credentialed refusal details ", 2000)
	startedAt := time.UnixMilli(1_725_000_000_000)
	if err := recorder.recordRefusal(startedAt, "origin/main", db.RefreshDecisionRefused, reason); err != nil {
		t.Fatal(err)
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].Decision != db.RefreshDecisionRefused || operations[0].StartedAt != startedAt.UnixMilli() || operations[0].DiagnosticArtifactID == nil {
		t.Fatalf("refusal receipt = %+v", operations)
	}
	registered, err := sctx.DB.GetArtifact(*operations[0].DiagnosticArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	if registered == nil || registered.Purpose != db.ArtifactPurposeOperationDiagnostic || registered.SourceBytes > maxRefreshDiagnosticBytes {
		t.Fatalf("refusal diagnostic = %+v", registered)
	}
	store, err := artifact.NewStore(sctx.Paths, "")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := store.Read(registered)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "refresh diagnostic truncated") {
		t.Fatalf("diagnostic contents missing truncation marker: %q", contents)
	}
}

func TestRefreshStep_FailedFeatureFetchLeavesAuthoritativeBaseSHAUnavailable(t *testing.T) {
	t.Parallel()
	dir, _, featureHead := setupStackedRefreshRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = filepath.Join(t.TempDir(), "unreachable-upstream.git")
	sctx.Repo.URLsVerified = true
	beginRefreshReceiptRound(t, sctx)

	if _, err := (&RefreshStep{}).Execute(sctx); err == nil {
		t.Fatal("refresh succeeded despite an unreachable authoritative upstream")
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("fetch-failure receipts = %+v, want one authoritative-base error", operations)
	}
	receipt := operations[0]
	if receipt.Decision != db.RefreshDecisionError || receipt.DestinationRef != "origin/main" {
		t.Fatalf("fetch-failure receipt = %+v", receipt)
	}
	if receipt.AuthoritativeBaseSHA != nil {
		t.Fatalf("fetch-failure receipt claimed authoritative base SHA %q before fetch resolution", *receipt.AuthoritativeBaseSHA)
	}
}

func TestRefreshStepBypassesUnrelatedRunnerConfiguration(t *testing.T) {
	t.Parallel()
	dir, upstream, featureHead := setupStackedRefreshRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, featureHead, featureHead, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Run.RefreshStrategy = types.RefreshStrategyRebase
	sctx.Run.StackedOn = "dependency"
	sctx.Repo.UpstreamURL = upstream
	beginRefreshReceiptRound(t, sctx)

	if _, _, err := runStepRunnerCommand(sctx, runner.Command{Run: "printf prior"}, string(types.StepRefresh)); err != nil {
		t.Fatalf("record prior command: %v", err)
	}
	// Refresh Git commands are controller-owned direct invocations. An invalid
	// configured shell must neither block them nor lend their receipts an
	// unrelated prior shell attempt.
	sctx.Config.Runner = runner.Spec{Executable: "not-a-supported-shell", Args: []string{"-c"}}

	if _, err := (&RefreshStep{}).Execute(sctx); err != nil {
		t.Fatalf("refresh with an unrelated invalid runner: %v", err)
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range operations {
		if operation.DestinationRef != "origin/dependency" {
			continue
		}
		if operation.Decision != db.RefreshDecisionRebased || len(operation.CommandAttemptIDs) != 1 || operation.CommandAttemptIDs[0] == "" {
			t.Fatalf("direct refresh receipt = %+v", operation)
		}
		attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, attempt := range attempts {
			if attempt.ID == operation.CommandAttemptIDs[0] && attempt.RunnerSource == runner.SourceDirectGit {
				return
			}
		}
		t.Fatalf("refresh receipt did not link a direct Git attempt: %+v", attempts)
	}
	t.Fatalf("missing direct refresh receipt: %+v", operations)
}

func TestRefreshPrimaryRunsBareRepositoryCommandAndRecordsReceipt(t *testing.T) {
	t.Parallel()
	bare := t.TempDir()
	gitCmd(t, bare, "init", "--bare")
	source := t.TempDir()
	gitCmd(t, source, "init")
	gitCmd(t, source, "config", "user.name", "test")
	gitCmd(t, source, "config", "user.email", "test@test.com")
	gitCmd(t, source, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(source, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, source, "add", "base.txt")
	gitCmd(t, source, "commit", "-m", "base")
	headSHA := gitCmd(t, source, "rev-parse", "HEAD")
	gitCmd(t, source, "push", bare, "main")
	gitCmd(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, bare, headSHA, headSHA, config.Commands{})
	sctx.Env = []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.bareRepository",
		"GIT_CONFIG_VALUE_0=explicit",
	}
	beginRefreshReceiptRound(t, sctx)
	receipts := newRefreshReceiptRecorder(sctx, types.RefreshStrategyRebase, "refs/heads/main", "origin/main")
	receipts.authoritativeBaseSHA = refreshStringPointer(headSHA)
	operation := receipts.begin("HEAD")

	output, err := runRefreshPrimary(sctx.Ctx, sctx, operation, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("run refresh primary: %v", err)
	}
	if strings.TrimSpace(output) != headSHA {
		t.Fatalf("bare refresh output = %q, want %q", output, headSHA)
	}
	if err := operation.finish(db.RefreshDecisionSkipped, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, ""); err != nil {
		t.Fatal(err)
	}
	operations, err := sctx.DB.GetRefreshOperationsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].Decision != db.RefreshDecisionSkipped || operations[0].ResultingHeadSHA == nil || len(operations[0].CommandAttemptIDs) != 1 {
		t.Fatalf("bare refresh receipt = %+v", operations)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].ID != operations[0].CommandAttemptIDs[0] || attempts[0].OutputArtifactID == nil {
		t.Fatalf("bare refresh command attempt = %+v", attempts)
	}
}
