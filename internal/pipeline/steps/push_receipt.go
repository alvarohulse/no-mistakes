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
)

const maxPushDiagnosticBytes = 32 * 1024

type pushReceiptRecorder struct {
	sctx              *pipeline.StepContext
	startedAt         time.Time
	targetKind        string
	targetFingerprint string
	targetIdentity    string
	destinationRef    string
	pushedSHA         string
	observedRemoteSHA *string
	leaseDecision     db.PushLeaseOrForceDecision
	decisionReason    string
	reviewApprovedSHA *string
	lastSeenSHA       *string
	remoteBeforeSHA   *string
	remoteAfterSHA    *string
	bindingUpdated    bool
}

func newPushReceiptRecorder(sctx *pipeline.StepContext, pushURL, destinationRef string) *pushReceiptRecorder {
	targetKind := "upstream"
	if sctx != nil && sctx.Repo != nil && strings.TrimSpace(sctx.Repo.ForkURL) != "" {
		targetKind = "fork"
	}
	identity := safeurl.Redact(pushURL)
	return &pushReceiptRecorder{
		sctx:              sctx,
		startedAt:         time.Now(),
		targetKind:        targetKind,
		targetFingerprint: branchsync.TargetFingerprint(pushURL),
		targetIdentity:    identity,
		destinationRef:    destinationRef,
		leaseDecision:     db.PushLeaseOrForceDecisionUnavailable,
	}
}

func (r *pushReceiptRecorder) enabled() bool {
	return r != nil && r.sctx != nil && r.sctx.DB != nil && r.sctx.Run != nil &&
		r.sctx.StepResultID != "" && r.sctx.RoundID != ""
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

func (r *pushReceiptRecorder) finish(runErr error) error {
	if !r.enabled() {
		return nil
	}
	if r.pushedSHA == "" {
		r.pushedSHA = strings.TrimSpace(r.sctx.Run.HeadSHA)
	}
	if r.pushedSHA == "" {
		r.pushedSHA = "unavailable"
	}
	if r.decisionReason == "" {
		if runErr != nil {
			r.decisionReason = safeurl.RedactText(runErr.Error())
		} else {
			r.decisionReason = "push completed"
		}
	}
	if r.reviewApprovedSHA == nil && r.sctx.Run.ReviewApprovedHeadSHA != nil {
		value := strings.TrimSpace(*r.sctx.Run.ReviewApprovedHeadSHA)
		if value != "" {
			r.reviewApprovedSHA = &value
		}
	}

	attemptIDs, err := pushReceiptAttemptIDs(r.sctx)
	if err != nil {
		return fmt.Errorf("load push receipt command attempts: %w", err)
	}
	var diagnosticID *string
	var diagnosticErr error
	if runErr != nil && len(attemptIDs) == 0 && r.sctx.Paths != nil {
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

	outcome := db.PushOperationOutcomeUpdated
	if runErr != nil {
		var refusal *forcePushWouldDiscardError
		if errors.As(runErr, &refusal) {
			outcome = db.PushOperationOutcomeRefused
			r.leaseDecision = db.PushLeaseOrForceDecisionRefused
		} else if r.bindingUpdated {
			outcome = db.PushOperationOutcomeFailed
		} else if errors.Is(runErr, context.Canceled) {
			outcome = db.PushOperationOutcomeProcessError
		} else {
			outcome = db.PushOperationOutcomeFailed
		}
	} else if r.leaseDecision == db.PushLeaseOrForceDecisionNewBranch {
		outcome = db.PushOperationOutcomeCreated
	} else if r.leaseDecision == db.PushLeaseOrForceDecisionAlreadyEqual {
		outcome = db.PushOperationOutcomeAlreadyEqual
	}
	completedAt := time.Now()
	operation := db.PushOperation{
		RunID: r.sctx.Run.ID, StepID: r.sctx.StepResultID, RoundID: r.sctx.RoundID,
		TargetKind: r.targetKind, TargetFingerprint: r.targetFingerprint, TargetIdentity: r.targetIdentity,
		DestinationRef: r.destinationRef, PushedSHA: r.pushedSHA, ObservedRemoteSHA: r.observedRemoteSHA,
		LeaseOrForceDecision: r.leaseDecision, DecisionReason: r.decisionReason, Outcome: outcome,
		ReviewApprovedHeadSHA: r.reviewApprovedSHA, LastSeenSHA: r.lastSeenSHA,
		RemoteBeforeSHA: r.remoteBeforeSHA, RemoteAfterSHA: r.remoteAfterSHA,
		BindingUpdated: r.bindingUpdated, CommandAttemptIDs: attemptIDs,
		StartedAt: r.startedAt.UnixMilli(), CompletedAt: completedAt.UnixMilli(),
		DurationMS: maxInt64(0, completedAt.Sub(r.startedAt).Milliseconds()), DiagnosticArtifactID: diagnosticID,
	}
	if r.bindingUpdated {
		if current, getErr := r.sctx.DB.GetRun(r.sctx.Run.ID); getErr == nil && current != nil {
			operation.ResultingGeneration = current.PushGeneration
		}
	}
	_, insertErr := r.sctx.DB.InsertPushOperation(operation)
	return errors.Join(diagnosticErr, insertErr)
}

func pushReceiptAttemptIDs(sctx *pipeline.StepContext) ([]string, error) {
	attempts, err := sctx.DB.GetCommandAttemptsByRun(sctx.Run.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.StepID == sctx.StepResultID && attempt.RoundID == sctx.RoundID {
			ids = append(ids, attempt.ID)
		}
	}
	return ids, nil
}

func boundedPushDiagnostic(value string) []byte {
	value = safeurl.RedactText(strings.TrimSpace(value))
	if len(value) <= maxPushDiagnosticBytes {
		return []byte(value)
	}
	marker := "\n… [push diagnostic truncated]"
	return []byte(value[:maxPushDiagnosticBytes-len(marker)] + marker)
}
