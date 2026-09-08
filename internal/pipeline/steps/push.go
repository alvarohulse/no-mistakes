package steps

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PushStep force-pushes the worktree state to the configured push remote.
type PushStep struct{}

func (s *PushStep) Name() types.StepName { return types.StepPush }

func (s *PushStep) Execute(sctx *pipeline.StepContext) (outcome *pipeline.StepOutcome, runErr error) {
	pushURL := resolvePushURL(sctx)
	ref := normalizedBranchRef(sctx.Run.Branch)
	receipt := newPushReceiptRecorder(sctx, pushURL, ref)
	if err := receipt.start(); err != nil {
		return nil, err
	}
	defer func() {
		if receiptErr := receipt.finish(runErr); receiptErr != nil {
			runErr = errors.Join(runErr, receiptErr)
		}
	}()
	return s.execute(sctx, receipt)
}

func (s *PushStep) execute(sctx *pipeline.StepContext, receipt *pushReceiptRecorder) (*pipeline.StepOutcome, error) {
	if err := assertPipelineHeadContinuity(sctx, s.Name()); err != nil {
		receipt.markRefused(err.Error())
		return nil, err
	}
	newHeadSHA := ""
	if err := sctx.DB.SetRunPushActive(sctx.Run.ID, true); err != nil {
		return nil, err
	}
	defer func() { _ = sctx.DB.SetRunPushActive(sctx.Run.ID, false) }()
	purpose := pushOperationPurpose(sctx)
	status, err := durablePushGitCommand(sctx, receipt, purpose, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("check for uncommitted changes before push: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		err := fmt.Errorf("refusing to push uncommitted changes without agent authorship metadata")
		receipt.markRefused(err.Error())
		return nil, err
	}

	// Run format command if configured (before committing, so changes are formatted)
	if formatCommand := sctx.Config.Commands.FormatCommand(); !formatCommand.IsZero() {
		sctx.Log(fmt.Sprintf("running formatter: %s", formatCommand.Run))
		output, exitCode, err := runStepRunnerCommand(sctx, formatCommand, "format")
		if err != nil {
			if (errors.Is(err, errCommandPreparation) || errors.Is(err, errCommandExecution)) &&
				!errors.Is(err, errCommandPersistence) {
				sctx.Log(fmt.Sprintf("warning: format command failed: %v", err))
				err = nil
			}
		}
		if err != nil {
			return nil, fmt.Errorf("run formatter: %w", err)
		} else if exitCode != 0 {
			sctx.Log(fmt.Sprintf("warning: format command exited with code %d: %s", exitCode, output))
		}
	}

	// Commit any changes made by the configured formatter. Test evidence is
	// deliberately not among them: it is retained outside the worktree in
	// owner-local storage, so no artifact enters repository history.
	status, err = durablePushGitCommand(sctx, receipt, purpose, "status", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("check formatter changes: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		sctx.Log("committing formatter changes...")
		if _, err := durablePushGitCommand(sctx, receipt, purpose, "add", "-A"); err != nil {
			return nil, fmt.Errorf("stage formatter changes: %w", err)
		}
		_, err := durablePushGitCommand(sctx, receipt, purpose, "commit", "-m", "chore(format): apply configured formatting")
		if err != nil {
			return nil, fmt.Errorf("commit formatter changes: %w", err)
		}
		headSHA, err := durablePushGitCommand(sctx, receipt, purpose, "rev-parse", "HEAD")
		if err != nil {
			return nil, fmt.Errorf("resolve head after commit: %w", err)
		}
		newHeadSHA = strings.TrimSpace(headSHA)
	}

	ref := normalizedBranchRef(sctx.Run.Branch)
	branch := strings.TrimPrefix(ref, "refs/heads/")

	pushURL := resolvePushURL(sctx)
	pushTarget := "upstream"
	usingFork := strings.TrimSpace(sctx.Repo.ForkURL) != ""
	if usingFork {
		pushTarget = "fork"
		sctx.Log(fmt.Sprintf("pushing to fork %s (%s)...", safeurl.Redact(pushURL), ref))
	} else {
		sctx.Log(fmt.Sprintf("pushing to %s (%s)...", safeurl.Redact(pushURL), ref))
	}

	headBeingPushed, err := durablePushGitCommand(sctx, receipt, purpose, "rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("resolve head before push: %w", err)
	}
	headBeingPushed = strings.TrimSpace(headBeingPushed)
	receipt.setPushedSHA(headBeingPushed)
	if sctx.Run.ReviewApprovedHeadSHA != nil {
		approved := strings.TrimSpace(*sctx.Run.ReviewApprovedHeadSHA)
		if approved != "" {
			receipt.reviewApprovedSHA = &approved
		}
	}
	if err := assertReviewApprovedPushHeadWithRunner(sctx, headBeingPushed, func(args ...string) (string, error) {
		return durablePushGitCommand(sctx, receipt, purpose, args...)
	}); err != nil {
		receipt.markRefused(err.Error())
		return nil, err
	}

	// Decide whether force-pushing would discard commits the pipeline never saw.
	// The lease is anchored to the remote-tracking ref the refresh step freshly
	// fetched (the exact commit this branch incorporated), so a push that
	// would clobber an out-of-band or stale-mirror commit fails loudly instead
	// of silently dropping it. A bare --force-with-lease offers no protection
	// when pushing to a URL (no remote-tracking refs), so the anchor is explicit.
	trackingRef := "refs/remotes/origin/" + branch
	if usingFork {
		trackingRef = forkBranchTrackingRef(branch)
	}
	lastSeen := ""
	if tracked, trackErr := durablePushGitCommand(sctx, receipt, purpose, "rev-parse", "--verify", "--quiet", trackingRef+"^{commit}"); trackErr == nil {
		lastSeen = strings.TrimSpace(tracked)
	}
	gitRun := func(args ...string) (string, error) {
		return durablePushGitCommand(sctx, receipt, purpose, args...)
	}
	decision, err := resolveForcePushDecision(gitRun, pushURL, ref, headBeingPushed, lastSeen, sctx.Run.BaseSHA)
	if err != nil {
		receipt.lastSeenSHA = pushReceiptStringPointer(lastSeen)
		receipt.setDecision(db.PushLeaseOrForceDecisionUnavailable, err.Error(), "")
		var refusal *forcePushWouldDiscardError
		if errors.As(err, &refusal) {
			receipt.markRefusedAtRemote(err.Error(), refusal.remoteSHA)
		}
		return nil, fmt.Errorf("push to %s: %w", pushTarget, err)
	}
	receipt.lastSeenSHA = pushReceiptStringPointer(lastSeen)
	switch {
	case decision.newBranch:
		receipt.setDecision(db.PushLeaseOrForceDecisionNewBranch, "remote branch did not exist", decision.remoteSHA)
	case decision.upToDate:
		receipt.setDecision(db.PushLeaseOrForceDecisionAlreadyEqual, "remote already pointed at pushed commit", decision.remoteSHA)
	case decision.incorporated:
		receipt.setDecision(db.PushLeaseOrForceDecisionForceWithLease, "remote movement passed incorporation safety checks", decision.remoteSHA)
	default:
		receipt.setDecision(db.PushLeaseOrForceDecisionForceWithLease, "remote head matched the verified lease anchor", decision.remoteSHA)
	}
	pushCommand := fmt.Sprintf("git push %s %s:%s", pushURL, headBeingPushed, ref)
	pushRan := false
	switch {
	case decision.newBranch:
		// New branch: regular push (no force needed).
		pushRan = true
		receipt.transportStarted = true
		if _, err := durablePushGitCommand(sctx, receipt, purpose, "push", pushURL, headBeingPushed+":"+ref); err != nil {
			sctx.RecordCommand(pushCommand, nil, err)
			return nil, fmt.Errorf("push to %s: %w", pushTarget, err)
		}
	case decision.upToDate:
		// Remote already at this exact head. This freshly verified equality is a
		// successful binding even though no objects needed to move.
	default:
		// Existing branch: force-with-lease anchored to the verified remote head.
		pushRan = true
		pushCommand = fmt.Sprintf("git push --force-with-lease=%s:%s %s %s:%s", ref, decision.remoteSHA, pushURL, headBeingPushed, ref)
		receipt.transportStarted = true
		if _, err := durablePushGitCommand(sctx, receipt, purpose, "push", pushURL, "--force-with-lease="+ref+":"+decision.remoteSHA, headBeingPushed+":"+ref); err != nil {
			sctx.RecordCommand(pushCommand, nil, err)
			return nil, fmt.Errorf("push to %s: %w", pushTarget, err)
		}
	}
	if pushRan {
		zero := 0
		sctx.RecordCommand(pushCommand, &zero, nil)
	}
	verifiedRemote, err := durablePushGitCommand(sctx, receipt, purpose, "ls-remote", pushURL, ref)
	if err != nil {
		return nil, fmt.Errorf("verify successful push to %s: %w", pushTarget, err)
	}
	if err := recordVerifiedPushRemoteSHA(receipt, verifiedRemote, headBeingPushed); err != nil {
		return nil, fmt.Errorf("verify successful push to %s: %w", pushTarget, err)
	}
	verifiedRemote = *receipt.verifiedRemoteSHA
	binding := db.PushBinding{
		HeadSHA:           headBeingPushed,
		TargetKind:        pushTarget,
		TargetFingerprint: branchsync.TargetFingerprint(pushURL),
		Ref:               ref,
	}
	var generation int64
	if receipt.enabled() {
		generation, err = sctx.DB.UpdateRunPushBindingWithGeneration(sctx.Run.ID, binding)
	} else {
		err = sctx.DB.UpdateRunPushBinding(sctx.Run.ID, binding)
	}
	if err != nil {
		return nil, err
	}
	if receipt.enabled() {
		receipt.recordBinding(generation)
	}

	if newHeadSHA != "" {
		if _, err := durablePushGitCommand(sctx, receipt, purpose, "update-ref", ref, newHeadSHA); err != nil {
			return nil, fmt.Errorf("update local branch ref: %w", err)
		}
	}

	// Persist the immutable source that was verified and delivered, never a
	// fresh read of mutable worktree HEAD after the push.
	if headBeingPushed != sctx.Run.HeadSHA {
		sctx.Run.HeadSHA = headBeingPushed
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headBeingPushed); err != nil {
			return nil, err
		}
	}

	sctx.Log("pushed successfully")
	return &pipeline.StepOutcome{}, nil
}

func pushReceiptStringPointer(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func recordVerifiedPushRemoteSHA(receipt *pushReceiptRecorder, output, expected string) error {
	fields := strings.Fields(output)
	if len(fields) == 0 || fields[0] != expected {
		if len(fields) > 0 {
			receipt.verifiedRemoteSHA = pushReceiptStringPointer(fields[0])
		}
		observed := "missing"
		if len(fields) > 0 {
			observed = fields[0]
		}
		return fmt.Errorf("remote head %s does not equal pushed head %s", observed, expected)
	}
	receipt.verifiedRemoteSHA = pushReceiptStringPointer(fields[0])
	return nil
}

func assertReviewApprovedPushHead(sctx *pipeline.StepContext, proposedHead string) error {
	return assertReviewApprovedPushHeadWithRunner(sctx, proposedHead, func(args ...string) (string, error) {
		return git.Run(sctx.Ctx, sctx.WorkDir, args...)
	})
}

func assertReviewApprovedPushHeadWithRunner(sctx *pipeline.StepContext, proposedHead string, gitRun gitRunner) error {
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return fmt.Errorf("load durable review approval before push: %w", err)
	}
	if run == nil || run.ReviewApprovedHeadSHA == nil || strings.TrimSpace(*run.ReviewApprovedHeadSHA) == "" {
		return fmt.Errorf("refusing to push: run has no durably recorded review-approved head")
	}
	approvedHead := strings.TrimSpace(*run.ReviewApprovedHeadSHA)
	if !isFullGitObjectID(approvedHead) {
		return fmt.Errorf("refusing to push: durable review-approved head is malformed")
	}
	resolved, err := gitRun("rev-parse", "--verify", approvedHead+"^{commit}")
	if err != nil || !strings.EqualFold(strings.TrimSpace(resolved), approvedHead) {
		return fmt.Errorf("refusing to push: durable review-approved head is unreachable")
	}
	if proposedHead != approvedHead {
		if _, err := gitRun("merge-base", "--is-ancestor", approvedHead, proposedHead); err != nil {
			return fmt.Errorf("refusing to push: proposed head %s violates continuity with review-approved head %s (it is not an equal or descendant commit)", shortObjectID(proposedHead), shortObjectID(approvedHead))
		}
	}
	return nil
}

func isFullGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func shortObjectID(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}
