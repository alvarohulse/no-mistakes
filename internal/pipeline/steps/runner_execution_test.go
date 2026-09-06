package steps

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/artifact"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunStepRunnerCommandUsesPlatformOverrideAndPersistsProvenance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	baseScript := "printf base"
	platformScript := "printf platform"
	bash := &runner.Spec{Executable: "bash", Args: []string{"-lc"}}
	override := &runner.Override{Run: &platformScript, Runner: bash}
	command := runner.Command{Run: baseScript}
	wantSource := runner.SourceLinux
	if runtime.GOOS == "darwin" {
		command.MacOS = override
		wantSource = runner.SourceMacOS
	} else {
		command.Linux = override
	}

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1
	round, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sctx.RoundID = round.ID
	sctx.RoundTrigger = "initial"

	output, exitCode, err := runStepRunnerCommand(sctx, command, "build")
	if err != nil || exitCode != 0 || output != "platform" {
		t.Fatalf("command result = output %q exit %d error %v", output, exitCode, err)
	}
	stored, err := sctx.DB.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := stored.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Commands) != 1 {
		t.Fatalf("commands = %+v", evidence.Commands)
	}
	receipt := evidence.Commands[0]
	if receipt.Command != platformScript || receipt.CommandSource != wantSource || receipt.Runner == nil || receipt.Runner.Source != wantSource || receipt.Runner.Executable != "bash" || receipt.Runner.Version == nil {
		t.Fatalf("runner receipt = %+v", receipt)
	}
	definitions, err := sctx.DB.GetCommandDefinitionsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || definitions[0].Script != platformScript || definitions[0].RunnerExecutable != "bash" {
		t.Fatalf("command definitions = %+v", definitions)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("command attempts = %+v", attempts)
	}
	attempt := attempts[0]
	if attempt.CommandID != definitions[0].ID || attempt.StepID != step.ID || attempt.RoundID != round.ID || attempt.Sequence != 1 || attempt.Purpose != "build" || attempt.Observer != "controller" || attempt.Trigger != "initial" || attempt.BeforeSHA != headSHA || attempt.TestedSHA == nil || *attempt.TestedSHA != headSHA || attempt.Outcome == nil || *attempt.Outcome != "pass" || attempt.ExitCode == nil || *attempt.ExitCode != 0 {
		t.Fatalf("command attempt = %+v", attempt)
	}
}

func TestRunStepRunnerCommandRejectsInvalidSyntaxBeforeExecution(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	marker := filepath.Join(dir, "must-not-run")
	command := runner.Command{Run: "printf ran > " + marker + "; if true; then"}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Runner = runner.Spec{Executable: "bash", Args: []string{"-lc"}}
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepBuild)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1
	round, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sctx.RoundID = round.ID
	sctx.RoundTrigger = "initial"

	_, _, err = runStepRunnerCommand(sctx, command, "test")
	if err == nil || !errors.Is(err, runner.ErrInvalidSyntax) {
		t.Fatalf("command error = %v, want invalid syntax", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("invalid command executed before refusal: %v", statErr)
	}
	stored, err := sctx.DB.GetStepResult(step.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := stored.Evidence()
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Commands) != 1 || evidence.Commands[0].Outcome != "error" || evidence.Commands[0].Runner == nil || evidence.Commands[0].Runner.Executable != "bash" || !strings.Contains(evidence.Commands[0].Command, "if true; then") {
		t.Fatalf("syntax-error receipt = %+v", evidence.Commands)
	}
	definitions, err := sctx.DB.GetCommandDefinitionsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(definitions) != 1 || definitions[0].Script != command.Run || definitions[0].RunnerExecutable != "bash" {
		t.Fatalf("syntax-error definitions = %+v", definitions)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 0 {
		t.Fatalf("syntax-error attempts = %+v, want none", attempts)
	}
}

func TestRunStepRunnerCommandPersistsPartialProvenanceOnPrepareErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	tests := []struct {
		name       string
		writeShell bool
	}{
		{name: "missing executable"},
		{name: "version probe launch failure", writeShell: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			binDir := t.TempDir()
			if tt.writeShell {
				if err := os.WriteFile(filepath.Join(binDir, "zsh"), []byte("not an executable image"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", binDir)

			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.Runner = runner.Spec{Executable: "zsh", Args: []string{"-lc"}}
			step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepBuild)
			if err != nil {
				t.Fatal(err)
			}
			sctx.StepResultID = step.ID
			sctx.Round = 1

			_, exitCode, err := runStepRunnerCommand(sctx, runner.Command{Run: "printf ready"}, "test")
			if err == nil || exitCode != -1 {
				t.Fatalf("prepare result = exit %d error %v, want -1/error", exitCode, err)
			}
			stored, err := sctx.DB.GetStepResult(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := stored.Evidence()
			if err != nil {
				t.Fatal(err)
			}
			if len(evidence.Commands) != 1 {
				t.Fatalf("commands = %+v", evidence.Commands)
			}
			receipt := evidence.Commands[0]
			if receipt.Command != "printf ready" || receipt.Outcome != "error" || receipt.ExitCode != nil || receipt.CommandSource != runner.SourceBase || receipt.Runner == nil {
				t.Fatalf("prepare-error receipt = %+v", receipt)
			}
			if receipt.Runner.SchemaVersion != runner.SchemaVersion || receipt.Runner.Platform != runtime.GOOS || receipt.Runner.Source != runner.SourceDefault || receipt.Runner.Executable != "zsh" || len(receipt.Runner.Args) != 1 || receipt.Runner.Args[0] != "-lc" || receipt.Runner.Version != nil {
				t.Fatalf("prepare-error runner provenance = %+v", receipt.Runner)
			}
		})
	}
}

func TestRunStepRunnerCommandRetainsCompleteOutputForStepLogging(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})

	output, exitCode, err := runStepRunnerCommand(sctx, runner.Command{Run: `head -c 131072 /dev/zero`}, "test")
	if err != nil || exitCode != 0 {
		t.Fatalf("command exit = %d, error %v", exitCode, err)
	}
	if len(output) != 131072 {
		t.Fatalf("output length = %d, want 131072", len(output))
	}
}

func TestRunStepRunnerCommandPersistsImmutableOutputArtifacts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	tests := []struct {
		name          string
		command       string
		wantOutput    string
		wantExitCode  int
		wantOutcome   string
		wantTestedSHA bool
	}{
		{
			name:          "non-empty success",
			command:       "printf output",
			wantOutput:    "output",
			wantExitCode:  0,
			wantOutcome:   db.CommandOutcomePass,
			wantTestedSHA: true,
		},
		{
			name:          "empty success",
			command:       ":",
			wantOutput:    "",
			wantExitCode:  0,
			wantOutcome:   db.CommandOutcomePass,
			wantTestedSHA: true,
		},
		{
			name:         "non-zero failure",
			command:      "printf failure; exit 7",
			wantOutput:   "failure",
			wantExitCode: 7,
			wantOutcome:  db.CommandOutcomeFail,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
			step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
			if err != nil {
				t.Fatal(err)
			}
			round, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			sctx.StepResultID = step.ID
			sctx.Round = 1
			sctx.RoundID = round.ID
			sctx.RoundTrigger = "initial"

			output, exitCode, err := runStepRunnerCommand(sctx, runner.Command{Run: tt.command}, "test")
			if err != nil || exitCode != tt.wantExitCode || output != tt.wantOutput {
				t.Fatalf("command result = output %q exit %d error %v, want output %q exit %d", output, exitCode, err, tt.wantOutput, tt.wantExitCode)
			}

			attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(attempts) != 1 {
				t.Fatalf("command attempts = %+v, want one", attempts)
			}
			attempt := attempts[0]
			if attempt.OutputArtifactID == nil || attempt.CompletedAt == nil || attempt.Outcome == nil || *attempt.Outcome != tt.wantOutcome {
				t.Fatalf("command attempt = %+v", attempt)
			}
			if (attempt.TestedSHA != nil) != tt.wantTestedSHA {
				t.Fatalf("tested SHA = %v, want present=%t", attempt.TestedSHA, tt.wantTestedSHA)
			}

			stored, err := sctx.DB.GetArtifact(*attempt.OutputArtifactID)
			if err != nil {
				t.Fatal(err)
			}
			if stored == nil {
				t.Fatal("output artifact was not registered")
			}
			if stored.RunID != sctx.Run.ID || stored.StepID == nil || *stored.StepID != step.ID || stored.RoundID == nil || *stored.RoundID != round.ID || stored.CommandAttemptID == nil || *stored.CommandAttemptID != attempt.ID || stored.Purpose != db.ArtifactPurposeCommandOutput || stored.Kind != db.ArtifactKindCommandOutput || stored.StorageRoot != db.ArtifactStorageRootRun || stored.RelativePath != path.Join(sctx.Run.ID, "command-output", attempt.ID+".log") || stored.SourceBytes != int64(len(tt.wantOutput)) {
				t.Fatalf("output artifact metadata = %+v", stored)
			}
			store, err := artifact.NewStore(sctx.Paths, "")
			if err != nil {
				t.Fatal(err)
			}
			contents, err := store.Read(stored)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != tt.wantOutput {
				t.Fatalf("output artifact contents = %q, want %q", contents, tt.wantOutput)
			}
			artifacts, err := sctx.DB.GetArtifactsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(artifacts) != 1 || artifacts[0].ID != stored.ID {
				t.Fatalf("run artifacts = %+v, want output artifact %q", artifacts, stored.ID)
			}
		})
	}
}

func TestRunStepRunnerCommandFailsClosedWhenOutputArtifactCannotBeCreated(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1
	sctx.RoundID = round.ID
	sctx.RoundTrigger = "initial"
	sctx.Paths = nil

	output, exitCode, err := runStepRunnerCommand(sctx, runner.Command{Run: "printf output"}, "test")
	if output != "output" || exitCode != 0 || err == nil || !errors.Is(err, errCommandPersistence) || !strings.Contains(err.Error(), "new artifact store: paths are nil") {
		t.Fatalf("command result = output %q exit %d error %v", output, exitCode, err)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].CompletedAt != nil || attempts[0].OutputArtifactID != nil {
		t.Fatalf("command attempts = %+v, want one incomplete unlinked attempt", attempts)
	}
	artifacts, err := sctx.DB.GetArtifactsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("run artifacts = %+v, want none after artifact creation failure", artifacts)
	}
}

func TestRunStepRunnerCommandPersistsSignalWithoutFabricatedExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX signal fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1
	sctx.RoundID = round.ID
	sctx.RoundTrigger = "initial"

	_, exitCode, err := runStepRunnerCommand(sctx, runner.Command{Run: "kill -TERM $$"}, "test")
	if err != nil || exitCode != -1 {
		t.Fatalf("signal command = exit %d error %v", exitCode, err)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != "fail" || attempts[0].ExitCode != nil || attempts[0].Signal == nil || attempts[0].OutputArtifactID == nil {
		t.Fatalf("signal attempt = %+v", attempts)
	}
	stored, err := sctx.DB.GetArtifact(*attempts[0].OutputArtifactID)
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.NewStore(sctx.Paths, "")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := store.Read(stored)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "" {
		t.Fatalf("signal output artifact contents = %q, want empty", contents)
	}
}

func TestRunStepRunnerCommandPersistsNonZeroExitAsFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepLint)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1
	sctx.RoundID = round.ID
	sctx.RoundTrigger = "initial"

	_, exitCode, err := runStepRunnerCommand(sctx, runner.Command{Run: "exit 7"}, "test")
	if err != nil || exitCode != 7 {
		t.Fatalf("failing command = exit %d error %v", exitCode, err)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Outcome == nil || *attempts[0].Outcome != "fail" || attempts[0].ExitCode == nil || *attempts[0].ExitCode != 7 || attempts[0].Signal != nil || attempts[0].TestedSHA != nil {
		t.Fatalf("failed attempt = %+v", attempts)
	}
}

func TestCommandAttemptOutcomeKeepsObservedExitAsProcessError(t *testing.T) {
	if got := commandAttemptOutcome(context.Background(), 0, errors.New("post-process wait failed")); got != db.CommandOutcomeProcessError {
		t.Fatalf("command attempt outcome = %q, want process_error", got)
	}
}

func TestRunStepRunnerCommandDoesNotRetryPastInterveningMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	command := runner.Command{Run: `if [ "$MUTATE" = 1 ]; then printf changed > intervening-mutation; fi; exit 1`}

	runRound := func(roundNumber int, trigger, mutate string) {
		t.Helper()
		round, err := sctx.DB.InsertStepRound(step.ID, roundNumber, trigger, nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		sctx.Round = roundNumber
		sctx.RoundID = round.ID
		sctx.RoundTrigger = trigger
		sctx.Env = []string{"MUTATE=" + mutate}
		if _, exitCode, err := runStepRunnerCommand(sctx, command, "test"); err != nil || exitCode != 1 {
			t.Fatalf("round %d result = exit %d error %v, want exit 1", roundNumber, exitCode, err)
		}
	}

	runRound(1, "initial", "0")
	runRound(2, "auto_fix", "1")
	if err := os.Remove(filepath.Join(dir, "intervening-mutation")); err != nil {
		t.Fatal(err)
	}
	runRound(3, "auto_fix", "0")

	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 3 {
		t.Fatalf("attempts = %+v, want three attempts", attempts)
	}
	if attempts[1].RetryOfAttemptID == nil || *attempts[1].RetryOfAttemptID != attempts[0].ID || attempts[1].ResultStateID != nil {
		t.Fatalf("mutating attempt = %+v, want retry of first with changed result state", attempts[1])
	}
	if attempts[2].RetryOfAttemptID != nil || attempts[2].RetryReason != nil {
		t.Fatalf("post-mutation attempt = %+v, must not link past the newer mutating attempt", attempts[2])
	}
}

func TestRunStepRunnerCommandDoesNotRetryPastInterveningInputChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	command := runner.Command{Run: `if [ "$RESTORE" = 1 ]; then rm intervening-input; fi; exit 1`}

	runRound := func(roundNumber int, trigger, restore string) {
		t.Helper()
		round, err := sctx.DB.InsertStepRound(step.ID, roundNumber, trigger, nil, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		sctx.Round = roundNumber
		sctx.RoundID = round.ID
		sctx.RoundTrigger = trigger
		sctx.Env = []string{"RESTORE=" + restore}
		if _, exitCode, err := runStepRunnerCommand(sctx, command, "test"); err != nil || exitCode != 1 {
			t.Fatalf("round %d result = exit %d error %v, want exit 1", roundNumber, exitCode, err)
		}
	}

	runRound(1, "initial", "0")
	if err := os.WriteFile(filepath.Join(dir, "intervening-input"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	runRound(2, "auto_fix", "1")
	runRound(3, "auto_fix", "0")

	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 3 {
		t.Fatalf("attempts = %+v, want three attempts", attempts)
	}
	if attempts[1].RetryOfAttemptID != nil || attempts[1].RetryReason != nil {
		t.Fatalf("changed-input attempt = %+v, must not retry the first attempt", attempts[1])
	}
	if attempts[2].RetryOfAttemptID != nil || attempts[2].RetryReason != nil {
		t.Fatalf("post-input-change attempt = %+v, must not link past the newer changed-input attempt", attempts[2])
	}
}

func TestRunStepRunnerCommandDoesNotClaimTestedSHAWhenCommandMutatesWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1
	sctx.RoundID = round.ID
	sctx.RoundTrigger = "initial"

	_, exitCode, err := runStepRunnerCommand(sctx, runner.Command{Run: "printf changed >> feature.txt"}, "test")
	if err != nil || exitCode != 0 {
		t.Fatalf("mutating command = exit %d error %v", exitCode, err)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].TestedSHA != nil || attempts[0].InputStateID == nil || attempts[0].ResultStateID != nil {
		t.Fatalf("mutating attempt subject = %+v", attempts)
	}
}

func TestRunStepRunnerCommandCompletesAttemptWhenResultStateCannotBeRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX runner fixture")
	}
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	sctx.Round = 1
	sctx.RoundID = round.ID
	sctx.RoundTrigger = "initial"

	_, exitCode, err := runStepRunnerCommand(sctx, runner.Command{Run: "rm -rf .git"}, "test")
	if err == nil || !strings.Contains(err.Error(), "resolve command result subject") || exitCode != 0 {
		t.Fatalf("command result = exit %d error %v", exitCode, err)
	}
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 {
		t.Fatalf("command attempts = %+v", attempts)
	}
	attempt := attempts[0]
	if attempt.CompletedAt == nil || attempt.DurationMS == nil || attempt.Outcome == nil || *attempt.Outcome != db.CommandOutcomePass || attempt.ExitCode == nil || *attempt.ExitCode != 0 || attempt.Signal != nil || attempt.ResultStateID != nil || attempt.TestedSHA != nil {
		t.Fatalf("completed attempt = %+v", attempt)
	}
}
