package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCompleteCommandAttemptWithOutputArtifactAtomicallyLinksExactlyOneArtifact(t *testing.T) {
	d := openTestDB(t)
	first, definition, step, round := newCommandArtifactAttemptFixture(t, d)

	exit := 0
	artifact, err := d.CompleteCommandAttemptWithOutputArtifact(
		first.ID,
		CommandOutcomePass,
		&exit,
		nil,
		stringPointer("git:head"),
		stringPointer("head"),
		commandOutputArtifact(filepath.ToSlash(filepath.Join(first.RunID, "command-output", first.ID+".log")), "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", 11),
	)
	if err != nil {
		t.Fatalf("complete command attempt with output artifact: %v", err)
	}
	if artifact.ID == "" || artifact.CommandAttemptID == nil || *artifact.CommandAttemptID != first.ID || artifact.StepID == nil || *artifact.StepID != step.ID || artifact.RoundID == nil || *artifact.RoundID != round.ID {
		t.Fatalf("stored artifact producer = %+v", artifact)
	}

	attempts, err := d.GetCommandAttemptsByRun(first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].OutputArtifactID == nil || *attempts[0].OutputArtifactID != artifact.ID || attempts[0].CompletedAt == nil {
		t.Fatalf("completed attempt = %+v", attempts)
	}
	stored, err := d.GetArtifact(artifact.ID)
	if err != nil {
		t.Fatalf("get linked artifact: %v", err)
	}
	if stored.RunID != first.RunID || stored.RelativePath != artifact.RelativePath || stored.SourceBytes != 11 || stored.SHA256 != artifact.SHA256 {
		t.Fatalf("stored artifact = %+v", stored)
	}

	if _, err := d.CompleteCommandAttemptWithOutputArtifact(
		first.ID,
		CommandOutcomePass,
		&exit,
		nil,
		stringPointer("git:head"),
		stringPointer("head"),
		commandOutputArtifact(filepath.ToSlash(filepath.Join(first.RunID, "command-output", "second.log")), "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", 11),
	); err == nil || !strings.Contains(err.Error(), "already complete") {
		t.Fatalf("second completion error = %v", err)
	}
	artifacts, err := d.GetArtifactsByRun(first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].ID != artifact.ID {
		t.Fatalf("artifacts after duplicate completion = %+v", artifacts)
	}

	second, err := d.StartCommandAttempt(CommandAttempt{
		RunID: first.RunID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
		Sequence: 2, Purpose: "test", Observer: CommandObserverController,
		Trigger: "initial", BeforeSHA: "head", InputStateID: stringPointer("git:head"),
		CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
	})
	if err != nil {
		t.Fatalf("start second attempt: %v", err)
	}
	if _, err := d.CompleteCommandAttemptWithOutputArtifact(
		second.ID,
		CommandOutcomePass,
		&exit,
		nil,
		stringPointer("git:head"),
		stringPointer("head"),
		commandOutputArtifact(artifact.RelativePath, "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", 11),
	); err == nil {
		t.Fatal("completion with a duplicate artifact path succeeded")
	}
	secondStored, err := d.getCommandAttempt(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondStored.CompletedAt != nil || secondStored.OutputArtifactID != nil {
		t.Fatalf("failed transaction terminalized second attempt: %+v", secondStored)
	}
	artifacts, err = d.GetArtifactsByRun(first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("failed transaction left artifacts = %+v", artifacts)
	}
}

func TestCompleteCommandAttemptWithOutputArtifactKeepsObservedPassAsHistory(t *testing.T) {
	for _, observer := range []string{CommandObserverController, CommandObserverProvider} {
		t.Run(observer, func(t *testing.T) {
			d := openTestDB(t)
			attempt, _, _, _ := newCommandArtifactAttemptFixture(t, d)
			if _, err := d.sql.Exec(`UPDATE command_attempts SET observer = ? WHERE id = ?`, observer, attempt.ID); err != nil {
				t.Fatal(err)
			}
			exit := 0
			if _, err := d.CompleteCommandAttemptWithOutputArtifact(
				attempt.ID,
				CommandOutcomePass,
				&exit,
				nil,
				stringPointer("git:head"),
				stringPointer("head"),
				commandOutputArtifact(filepath.ToSlash(filepath.Join(attempt.RunID, "command-output", attempt.ID+".log")), "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", 11),
			); err != nil {
				t.Fatalf("complete command attempt history: %v", err)
			}
			stored, err := d.getCommandAttempt(attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.AcceptedAsProof || stored.ProofReason != nil {
				t.Fatalf("generic completion accepted proof = %+v", stored)
			}
		})
	}
}

func TestCompleteControllerCommandAttemptWithOutputArtifactRejectsNonValidationProofPurpose(t *testing.T) {
	tests := []struct {
		name    string
		step    types.StepName
		purpose string
	}{
		{name: "review", step: types.StepReview, purpose: "review"},
		{name: "CI", step: types.StepCI, purpose: "ci"},
		{name: "other purpose", step: types.StepTest, purpose: "other"},
		{name: "mismatched validation purpose", step: types.StepBuild, purpose: "test"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openTestDB(t)
			repo, err := d.InsertRepo("/home/user/non-validation-proof", "git@github.com:user/non-validation-proof.git", "main")
			if err != nil {
				t.Fatal(err)
			}
			run, err := d.InsertRun(repo.ID, "feature", "head", "base")
			if err != nil {
				t.Fatal(err)
			}
			step, err := d.InsertStepResult(run.ID, tt.step)
			if err != nil {
				t.Fatal(err)
			}
			round, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			definition, err := d.EnsureCommandDefinition(run.ID, runner.Resolved{
				Script:        "echo validation",
				CommandSource: runner.SourceBase,
				Provenance:    runner.Provenance{SchemaVersion: runner.SchemaVersion, Platform: "linux", Source: runner.SourceDefault, Executable: "sh", Args: []string{"-c"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := d.StartCommandAttempt(CommandAttempt{
				RunID: run.ID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
				Sequence: 1, Purpose: tt.purpose, Observer: CommandObserverController, Trigger: "initial", BeforeSHA: "head",
				InputStateID: stringPointer("git:head"), CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
			})
			if err != nil {
				t.Fatal(err)
			}
			exit := 0
			if _, err := d.CompleteControllerCommandAttemptWithOutputArtifact(attempt.ID, CommandOutcomePass, &exit, nil, stringPointer("git:head"), stringPointer("head"), commandOutputArtifactForAttempt(attempt)); err != nil {
				t.Fatal(err)
			}
			stored, err := d.getCommandAttempt(attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.AcceptedAsProof || stored.ProofReason != nil {
				t.Fatalf("non-validation proof acceptance = %+v", stored)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateStepStatus(step.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
			if _, err := d.sql.Exec(`UPDATE command_attempts SET accepted_as_proof = 1, proof_reason = ? WHERE id = ?`, CommandProofReasonObservedPass, attempt.ID); err != nil {
				t.Fatal(err)
			}
			proofs, err := d.GetAcceptedCommandAttemptsByTestedSHA(run.ID, "head")
			if err != nil {
				t.Fatal(err)
			}
			if len(proofs) != 0 {
				t.Fatalf("non-validation proof lookup = %+v", proofs)
			}
		})
	}
}

func TestCompleteCommandAttemptWithOutputArtifactRejectsNonNormalizedPathWithoutTerminalizing(t *testing.T) {
	d := openTestDB(t)
	attempt, _, _, _ := newCommandArtifactAttemptFixture(t, d)
	exit := 0
	_, err := d.CompleteCommandAttemptWithOutputArtifact(
		attempt.ID,
		CommandOutcomePass,
		&exit,
		nil,
		stringPointer("git:head"),
		stringPointer("head"),
		commandOutputArtifact("../escaped.log", "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9", 11),
	)
	if err == nil || !strings.Contains(err.Error(), "relative path") {
		t.Fatalf("unsafe relative path error = %v", err)
	}
	stored, err := d.getCommandAttempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CompletedAt != nil || stored.OutputArtifactID != nil {
		t.Fatalf("unsafe artifact terminalized attempt: %+v", stored)
	}
}

func TestCompleteCommandAttemptWithOutputArtifactRejectsNonCommandOutputMetadataWithoutTerminalizing(t *testing.T) {
	tests := []struct {
		name     string
		artifact func(*CommandAttempt, *StepResult, *StepRound) Artifact
		wantErr  string
	}{
		{
			name: "test evidence",
			artifact: func(attempt *CommandAttempt, step *StepResult, round *StepRound) Artifact {
				return testEvidenceArtifact(
					filepath.ToSlash(filepath.Join(attempt.RunID, "screenshots", "checkout.html")),
					attempt.RunID,
					step.ID,
					round.ID,
				)
			},
			wantErr: "purpose must be \"command_output\"",
		},
		{
			name: "different kind",
			artifact: func(attempt *CommandAttempt, _ *StepResult, _ *StepRound) Artifact {
				artifact := commandOutputArtifactForAttempt(attempt)
				artifact.Kind = "log"
				return artifact
			},
			wantErr: "kind must be \"command-output\"",
		},
		{
			name: "evidence storage root",
			artifact: func(attempt *CommandAttempt, _ *StepResult, _ *StepRound) Artifact {
				artifact := commandOutputArtifactForAttempt(attempt)
				artifact.StorageRoot = ArtifactStorageRootEvidence
				return artifact
			},
			wantErr: "storage root must be \"run\"",
		},
		{
			name: "different output path",
			artifact: func(attempt *CommandAttempt, _ *StepResult, _ *StepRound) Artifact {
				artifact := commandOutputArtifactForAttempt(attempt)
				artifact.RelativePath = filepath.ToSlash(filepath.Join(attempt.RunID, "command-output", "other.log"))
				return artifact
			},
			wantErr: "relative path must be",
		},
		{
			name: "text media with binary encoding",
			artifact: func(attempt *CommandAttempt, _ *StepResult, _ *StepRound) Artifact {
				artifact := commandOutputArtifactForAttempt(attempt)
				artifact.Encoding = "binary"
				return artifact
			},
			wantErr: "media type and encoding must be",
		},
		{
			name: "binary media with utf-8 encoding",
			artifact: func(attempt *CommandAttempt, _ *StepResult, _ *StepRound) Artifact {
				artifact := commandOutputArtifactForAttempt(attempt)
				artifact.MediaType = "application/octet-stream"
				return artifact
			},
			wantErr: "media type and encoding must be",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := openTestDB(t)
			attempt, _, step, round := newCommandArtifactAttemptFixture(t, d)
			exit := 0
			_, err := d.CompleteCommandAttemptWithOutputArtifact(
				attempt.ID,
				CommandOutcomePass,
				&exit,
				nil,
				stringPointer("git:head"),
				stringPointer("head"),
				tt.artifact(attempt, step, round),
			)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("completion error = %v, want %q", err, tt.wantErr)
			}

			stored, err := d.getCommandAttempt(attempt.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.CompletedAt != nil || stored.OutputArtifactID != nil {
				t.Fatalf("rejected artifact terminalized attempt: %+v", stored)
			}
			artifacts, err := d.GetArtifactsByRun(attempt.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if len(artifacts) != 0 {
				t.Fatalf("rejected artifact created rows: %+v", artifacts)
			}
		})
	}
}

func TestCompleteCommandAttemptWithOutputArtifactAcceptsBinaryOutput(t *testing.T) {
	d := openTestDB(t)
	attempt, _, _, _ := newCommandArtifactAttemptFixture(t, d)
	output := commandOutputArtifactForAttempt(attempt)
	output.MediaType = "application/octet-stream"
	output.Encoding = "binary"
	exit := 0

	stored, err := d.CompleteCommandAttemptWithOutputArtifact(
		attempt.ID,
		CommandOutcomePass,
		&exit,
		nil,
		stringPointer("git:head"),
		stringPointer("head"),
		output,
	)
	if err != nil {
		t.Fatalf("complete binary command output: %v", err)
	}
	if stored.MediaType != output.MediaType || stored.Encoding != output.Encoding {
		t.Fatalf("stored binary output metadata = %+v", stored)
	}
}

func TestCompleteCommandAttemptWithOutputArtifactRejectsMissingOutputWithoutTerminalizing(t *testing.T) {
	d := openTestDB(t)
	attempt, _, _, _ := newCommandArtifactAttemptFixture(t, d)
	exit := 0
	_, err := d.CompleteCommandAttemptWithOutputArtifact(
		attempt.ID,
		CommandOutcomePass,
		&exit,
		nil,
		stringPointer("git:head"),
		stringPointer("head"),
		Artifact{},
	)
	if err == nil || !strings.Contains(err.Error(), "required metadata is incomplete") {
		t.Fatalf("missing output artifact error = %v", err)
	}
	stored, err := d.getCommandAttempt(attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CompletedAt != nil || stored.OutputArtifactID != nil {
		t.Fatalf("missing artifact terminalized attempt: %+v", stored)
	}
	artifacts, err := d.GetArtifactsByRun(attempt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("missing artifact created rows: %+v", artifacts)
	}
}

func TestRegisterArtifactKeepsOneStableRowPerPhysicalPath(t *testing.T) {
	d := openTestDB(t)
	attempt, _, step, round := newCommandArtifactAttemptFixture(t, d)
	candidate := testEvidenceArtifact(
		filepath.ToSlash(filepath.Join(attempt.RunID, "screenshots", "checkout.html")),
		attempt.RunID,
		step.ID,
		round.ID,
	)

	stored, err := d.RegisterArtifact(candidate)
	if err != nil {
		t.Fatalf("register evidence artifact: %v", err)
	}
	if stored.ID == "" || stored.StepID == nil || *stored.StepID != step.ID || stored.RoundID == nil || *stored.RoundID != round.ID {
		t.Fatalf("registered artifact = %+v", stored)
	}
	fetched, err := d.GetArtifact(stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fetched == nil || fetched.RelativePath != candidate.RelativePath || fetched.SHA256 != candidate.SHA256 || fetched.SourceBytes != candidate.SourceBytes {
		t.Fatalf("fetched artifact = %+v", fetched)
	}

	duplicate, err := d.RegisterArtifact(candidate)
	if err != nil {
		t.Fatalf("register duplicate evidence artifact: %v", err)
	}
	if duplicate.ID != stored.ID {
		t.Fatalf("duplicate artifact ID = %q, want %q", duplicate.ID, stored.ID)
	}
	artifacts, err := d.GetArtifactsByRun(attempt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].ID != stored.ID {
		t.Fatalf("artifacts = %+v, want one stable row", artifacts)
	}

	conflicting := candidate
	conflicting.Label = "Different evidence label"
	if _, err := d.RegisterArtifact(conflicting); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflicting duplicate error = %v", err)
	}
	wrongRunPath := candidate
	wrongRunPath.RelativePath = "other-run/screenshots/checkout.html"
	if _, err := d.RegisterArtifact(wrongRunPath); err == nil || !strings.Contains(err.Error(), "run ID") {
		t.Fatalf("wrong run path error = %v", err)
	}
}

func TestRegisterArtifactRejectsMissingAndForeignProducers(t *testing.T) {
	d := openTestDB(t)
	attempt, _, step, round := newCommandArtifactAttemptFixture(t, d)
	run, err := d.GetRun(attempt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	foreignRun, err := d.InsertRun(run.RepoID, "other-branch", "other-head", "other-base")
	if err != nil {
		t.Fatal(err)
	}
	foreignStep, err := d.InsertStepResult(foreignRun.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	foreignRound, err := d.InsertStepRound(foreignStep.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreignInvocation, err := d.InsertAgentInvocation(AgentInvocation{
		RunID: foreignRun.ID, StepName: string(types.StepTest), Round: 1, Purpose: "test-evidence", Agent: "test",
		SessionMode: InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 1, ExitStatus: "ok",
	})
	if err != nil {
		t.Fatal(err)
	}

	base := testEvidenceArtifact(
		filepath.ToSlash(filepath.Join(attempt.RunID, "evidence", "report.txt")),
		attempt.RunID,
		step.ID,
		round.ID,
	)
	tests := []struct {
		name    string
		mutate  func(*Artifact)
		wantErr string
	}{
		{
			name: "missing producer",
			mutate: func(artifact *Artifact) {
				artifact.StepID = nil
				artifact.RoundID = nil
			},
			wantErr: "producer metadata is required",
		},
		{
			name: "foreign step",
			mutate: func(artifact *Artifact) {
				artifact.StepID = &foreignStep.ID
			},
			wantErr: "step producer does not belong to run",
		},
		{
			name: "foreign round",
			mutate: func(artifact *Artifact) {
				artifact.RoundID = &foreignRound.ID
			},
			wantErr: "round producer does not belong to run",
		},
		{
			name: "foreign invocation",
			mutate: func(artifact *Artifact) {
				artifact.InvocationID = &foreignInvocation.ID
			},
			wantErr: "invocation producer does not belong to run",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := base
			tt.mutate(&candidate)
			if _, err := d.RegisterArtifact(candidate); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("register artifact error = %v, want %q", err, tt.wantErr)
			}
		})
	}

	artifacts, err := d.GetArtifactsByRun(attempt.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("invalid registrations created artifacts: %+v", artifacts)
	}
}

func newCommandArtifactAttemptFixture(t *testing.T, d *DB) (*CommandAttempt, *CommandDefinition, *StepResult, *StepRound) {
	t.Helper()
	repo, err := d.InsertRepo("/home/user/command-artifact", "git@github.com:user/command-artifact.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	round, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := d.EnsureCommandDefinition(run.ID, runner.Resolved{
		Script:        "go test ./...",
		CommandSource: runner.SourceBase,
		Provenance: runner.Provenance{
			SchemaVersion: runner.SchemaVersion,
			Platform:      "linux",
			Source:        runner.SourceDefault,
			Executable:    "sh",
			Args:          []string{"-c"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := d.StartCommandAttempt(CommandAttempt{
		RunID: run.ID, CommandID: definition.ID, StepID: step.ID, RoundID: round.ID,
		Sequence: 1, Purpose: "test", Observer: CommandObserverController,
		Trigger: "initial", BeforeSHA: "head", InputStateID: stringPointer("git:head"),
		CommandSource: runner.SourceBase, RunnerSchemaVersion: runner.SchemaVersion, RunnerSource: runner.SourceDefault,
	})
	if err != nil {
		t.Fatal(err)
	}
	return attempt, definition, step, round
}

func commandOutputArtifact(relativePath, digest string, size int64) Artifact {
	return Artifact{
		StorageRoot:  ArtifactStorageRootRun,
		RelativePath: relativePath,
		Purpose:      ArtifactPurposeCommandOutput,
		Label:        "Command output",
		Kind:         ArtifactKindCommandOutput,
		MediaType:    "text/plain",
		Encoding:     "utf-8",
		SHA256:       digest,
		SourceBytes:  size,
		State:        ArtifactStateAvailable,
	}
}

func testEvidenceArtifact(relativePath, runID, stepID, roundID string) Artifact {
	return Artifact{
		RunID:        runID,
		StepID:       stringPointer(stepID),
		RoundID:      stringPointer(roundID),
		Purpose:      "test_evidence",
		Label:        "Checkout screenshot",
		StorageRoot:  ArtifactStorageRootEvidence,
		RelativePath: relativePath,
		Kind:         "screenshot",
		MediaType:    "text/html",
		Encoding:     "utf-8",
		SHA256:       "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
		SourceBytes:  11,
		State:        ArtifactStateAvailable,
	}
}
