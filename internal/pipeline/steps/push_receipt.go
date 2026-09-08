package steps

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/artifact"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const maxPushDiagnosticBytes = 32 * 1024

type pushReceiptRecorder struct {
	sctx              *pipeline.StepContext
	startedAt         time.Time
	operationID       string
	targetKind        string
	targetFingerprint string
	targetIdentity    string
	destinationRef    string
	pushedSHA         *string
	observedRemoteSHA *string
	leaseDecision     db.PushLeaseOrForceDecision
	decisionReason    string
	reviewApprovedSHA *string
	lastSeenSHA       *string
	remoteBeforeSHA   *string
	remoteAfterSHA    *string
	bindingUpdated    bool
	transportStarted  bool
	refused           bool
	attemptIDs        []string
}

func newPushReceiptRecorder(sctx *pipeline.StepContext, pushURL, destinationRef string) *pushReceiptRecorder {
	targetKind := "upstream"
	if sctx != nil && sctx.Repo != nil && strings.TrimSpace(sctx.Repo.ForkURL) != "" {
		targetKind = "fork"
	}
	return &pushReceiptRecorder{
		sctx:              sctx,
		startedAt:         time.Now(),
		targetKind:        targetKind,
		targetFingerprint: branchsync.TargetFingerprint(pushURL),
		targetIdentity:    safeurl.Redact(pushURL),
		destinationRef:    destinationRef,
		leaseDecision:     db.PushLeaseOrForceDecisionUnavailable,
	}
}

func (r *pushReceiptRecorder) enabled() bool {
	return r != nil && r.sctx != nil && r.sctx.DB != nil && r.sctx.Run != nil &&
		r.sctx.StepResultID != "" && r.sctx.RoundID != ""
}

func (r *pushReceiptRecorder) start() error {
	if !r.enabled() || r.operationID != "" {
		return nil
	}
	started, err := r.sctx.DB.StartPushOperation(db.PushOperation{
		RunID: r.sctx.Run.ID, StepID: r.sctx.StepResultID, RoundID: r.sctx.RoundID,
		TargetKind: r.targetKind, TargetFingerprint: r.targetFingerprint, TargetIdentity: r.targetIdentity,
		DestinationRef: r.destinationRef, StartedAt: r.startedAt.UnixMilli(),
	})
	if err != nil {
		return fmt.Errorf("start push receipt: %w", err)
	}
	r.operationID = started.ID
	if r.sctx.Run.ReviewApprovedHeadSHA != nil {
		value := strings.TrimSpace(*r.sctx.Run.ReviewApprovedHeadSHA)
		if value != "" {
			r.reviewApprovedSHA = &value
		}
	}
	return nil
}

func (r *pushReceiptRecorder) setPushedSHA(value string) {
	value = strings.TrimSpace(value)
	if value != "" {
		r.pushedSHA = &value
	}
}

func (r *pushReceiptRecorder) setDecision(decision db.PushLeaseOrForceDecision, reason string, observed string) {
	if r == nil {
		return
	}
	r.leaseDecision = decision
	r.decisionReason = safeurl.RedactText(strings.TrimSpace(reason))
	if strings.TrimSpace(observed) != "" {
		value := strings.TrimSpace(observed)
		r.observedRemoteSHA = &value
		r.remoteBeforeSHA = &value
	}
}

func (r *pushReceiptRecorder) markRefused(reason string) {
	r.refused = true
	r.setDecision(db.PushLeaseOrForceDecisionRefused, reason, "")
}

func (r *pushReceiptRecorder) recordAttempt(id string) {
	if id == "" {
		return
	}
	for _, existing := range r.attemptIDs {
		if existing == id {
			return
		}
	}
	r.attemptIDs = append(r.attemptIDs, id)
}

// runGit routes controller-owned git inspection and transport through the
// durable command-attempt/artifact seam and records only attempts created by
// this operation.
func (r *pushReceiptRecorder) runGit(purpose string, command string, args ...string) (string, error) {
	before, err := commandAttemptIDsForScope(r.sctx)
	if err != nil {
		return "", err
	}
	output, exitCode, runErr := runStepGitCommand(r.sctx, command, purpose, args...)
	after, afterErr := commandAttemptIDsForScope(r.sctx)
	if afterErr == nil {
		for id := range after {
			if _, existed := before[id]; !existed {
				r.recordAttempt(id)
			}
		}
	}
	if runErr != nil {
		return output, runErr
	}
	if exitCode != 0 {
		return output, &pushCommandExitError{command: command, code: exitCode, output: output}
	}
	return output, nil
}

type pushCommandExitError struct {
	command string
	code    int
	output  string
}

func (e *pushCommandExitError) Error() string {
	if strings.TrimSpace(e.output) == "" {
		return fmt.Sprintf("git %s exited with code %d", e.command, e.code)
	}
	return fmt.Sprintf("git %s exited with code %d: %s", e.command, e.code, safeurl.RedactText(strings.TrimSpace(e.output)))
}

func commandAttemptIDsForScope(sctx *pipeline.StepContext) (map[string]struct{}, error) {
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]struct{})
	for _, attempt := range attempts {
		if attempt.StepID == sctx.StepResultID && attempt.RoundID == sctx.RoundID {
			ids[attempt.ID] = struct{}{}
		}
	}
	return ids, nil
}

func (r *pushReceiptRecorder) finish(runErr error) error {
	if !r.enabled() || r.operationID == "" {
		return nil
	}
	if r.decisionReason == "" {
		r.decisionReason = safeurl.RedactText(errorReason(runErr, "push completed"))
	}
	outcome := db.PushOperationOutcomeUpdated
	if runErr != nil {
		switch {
		case r.refused:
			outcome = db.PushOperationOutcomeRefused
		case errors.Is(runErr, context.Canceled), errors.Is(runErr, context.DeadlineExceeded), errors.Is(runErr, errCommandExecution):
			outcome = db.PushOperationOutcomeProcessError
		default:
			var refusal *forcePushWouldDiscardError
			if errors.As(runErr, &refusal) {
				outcome = db.PushOperationOutcomeRefused
				r.leaseDecision = db.PushLeaseOrForceDecisionRefused
			} else {
				outcome = db.PushOperationOutcomeFailed
			}
		}
	} else if r.leaseDecision == db.PushLeaseOrForceDecisionNewBranch {
		outcome = db.PushOperationOutcomeCreated
	} else if r.leaseDecision == db.PushLeaseOrForceDecisionAlreadyEqual {
		outcome = db.PushOperationOutcomeAlreadyEqual
	}

	var diagnosticID *string
	var diagnosticErr error
	if runErr != nil && r.sctx.Paths != nil {
		store, storeErr := artifact.NewStore(r.sctx.Paths, "")
		if storeErr != nil {
			diagnosticErr = fmt.Errorf("create push diagnostic store: %w", storeErr)
		} else {
			digest := sha256.Sum256([]byte(r.destinationRef))
			metadata, createErr := store.CreateOperationDiagnostic(r.sctx.Run.ID, fmt.Sprintf("push-%s-%x", r.sctx.RoundID, digest[:8]), boundedPushDiagnostic(runErr.Error()))
			if createErr != nil {
				diagnosticErr = fmt.Errorf("create push diagnostic: %w", createErr)
			} else {
				metadata.RunID = r.sctx.Run.ID
				stepID, roundID := r.sctx.StepResultID, r.sctx.RoundID
				metadata.StepID = &stepID
				metadata.RoundID = &roundID
				registered, registerErr := r.sctx.DB.RegisterArtifact(metadata)
				if registerErr != nil {
					diagnosticErr = fmt.Errorf("register push diagnostic: %w", registerErr)
				} else {
					diagnosticID = &registered.ID
				}
			}
		}
	}

	completedAt := time.Now()
	operation := db.PushOperation{
		ID: r.operationID, RunID: r.sctx.Run.ID, StepID: r.sctx.StepResultID, RoundID: r.sctx.RoundID,
		TargetKind: r.targetKind, TargetFingerprint: r.targetFingerprint, TargetIdentity: r.targetIdentity,
		DestinationRef: r.destinationRef, PushedSHA: r.pushedSHA, ObservedRemoteSHA: r.observedRemoteSHA,
		LeaseOrForceDecision: r.leaseDecision, DecisionReason: boundedPushReason(r.decisionReason), Outcome: outcome,
		ReviewApprovedHeadSHA: r.reviewApprovedSHA, LastSeenSHA: r.lastSeenSHA,
		RemoteBeforeSHA: r.remoteBeforeSHA, RemoteAfterSHA: r.remoteAfterSHA,
		BindingUpdated: r.bindingUpdated, CommandAttemptIDs: append([]string(nil), r.attemptIDs...),
		StartedAt: r.startedAt.UnixMilli(), CompletedAt: completedAt.UnixMilli(),
		DurationMS: maxInt64(0, completedAt.Sub(r.startedAt).Milliseconds()), DiagnosticArtifactID: diagnosticID,
	}
	if r.bindingUpdated {
		current, getErr := r.sctx.DB.GetRun(r.sctx.Run.ID)
		if getErr != nil {
			diagnosticErr = errors.Join(diagnosticErr, fmt.Errorf("read resulting push generation: %w", getErr))
		} else if current == nil || current.PushGeneration == nil {
			diagnosticErr = errors.Join(diagnosticErr, fmt.Errorf("read resulting push generation: unavailable"))
		} else {
			operation.ResultingGeneration = current.PushGeneration
		}
	}
	_, completeErr := r.sctx.DB.CompletePushOperation(operation)
	return errors.Join(diagnosticErr, completeErr)
}

func errorReason(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	return err.Error()
}

func boundedPushReason(value string) string {
	value = safeurl.RedactText(strings.TrimSpace(value))
	if len(value) <= db.MaxPushReceiptReasonBytes() {
		return value
	}
	return value[:db.MaxPushReceiptReasonBytes()]
}

func boundedPushDiagnostic(value string) []byte {
	value = safeurl.RedactText(strings.TrimSpace(value))
	if len(value) <= maxPushDiagnosticBytes {
		return []byte(value)
	}
	marker := "\n… [push diagnostic truncated]"
	return []byte(value[:maxPushDiagnosticBytes-len(marker)] + marker)
}

func durablePushGitCommand(sctx *pipeline.StepContext, receipt *pushReceiptRecorder, purpose string, args ...string) (string, error) {
	command := safeurl.RedactText(strings.Join(append([]string{"git"}, args...), " "))
	return receipt.runGit(purpose, command, args...)
}

func pushOperationPurpose(sctx *pipeline.StepContext) string {
	if sctx != nil && sctx.StepResultID != "" && sctx.DB != nil {
		if step, err := sctx.DB.GetStepResult(sctx.StepResultID); err == nil && step != nil && step.StepName == types.StepCI {
			return string(types.StepCI)
		}
	}
	return string(types.StepPush)
}
