package steps

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/testguidance"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// autoFixCI runs the agent to fix CI failures and/or merge conflicts, then
// commits and pushes to the configured push remote.
// Returns (true, summary, nil) when changes were committed and pushed,
// (false, "", nil) when the agent produced no changes, or (false, "", err)
// on failure.
func (s *CIStep) autoFixCI(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, failingNames []string, mergeConflict bool, repairRound *ciFixRepairRound) (bool, string, error) {
	ctx := sctx.Ctx
	if err := sctx.DB.SetRunPushActive(sctx.Run.ID, true); err != nil {
		return false, "", err
	}
	defer func() { _ = sctx.DB.SetRunPushActive(sctx.Run.ID, false) }()
	baseBranch := effectiveBaseBranch(sctx)
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, baseBranch)
	rebaseBaseSHA := resolveRunDefaultBranchTipSHA(ctx, sctx, sctx.Run.BaseSHA, baseBranch)
	promptBaseSHA := baseSHA
	if mergeConflict {
		promptBaseSHA = rebaseBaseSHA
	}

	const maxLogBytes = 32 * 1024
	var logOutput string
	if host.Capabilities().FailedCheckLogs {
		raw, err := host.FetchFailedCheckLogs(ctx, pr, sctx.Run.Branch, sctx.Run.HeadSHA, failingNames)
		if err != nil && err != scm.ErrUnsupported {
			slog.Warn("failed to fetch CI logs", "err", err)
		}
		if raw != "" {
			logOutput = trimLogOutput(strings.TrimSpace(raw), maxLogBytes)
		}
	}

	// Build prompt based on what issues are present
	var promptIntro string
	var promptRules string
	switch {
	case len(failingNames) > 0 && mergeConflict:
		promptIntro = "The following CI checks have failed and the PR has merge conflicts with the base branch. Diagnose and fix the CI issues, then rebase onto the base branch and resolve the merge conflicts."
		promptRules = `- You MUST produce file changes that fix the failing checks. Do not conclude that nothing needs to change.
		- If a test fails only on a specific OS (e.g. Windows CRLF, path separators), fix the test to be cross-platform.
		- If a test is flaky, make it deterministic.
		- Make the smallest correct root-cause fix.
		- Do not refactor beyond what is needed for that root-cause fix.
		- Verify the fix by running the most relevant commands locally before finishing.`
	case mergeConflict:
		promptIntro = "The PR has merge conflicts with the base branch. Rebase onto the base branch and resolve the merge conflicts."
		promptRules = `- Resolve the merge conflicts by applying the minimal necessary changes.
		- Do not make unrelated file edits.
		- Verify the rebase completes cleanly before finishing.`
	default:
		promptIntro = "The following CI checks have failed on this PR. Diagnose and fix the issues."
		promptRules = `- You MUST produce file changes that fix the failing checks. Do not conclude that nothing needs to change.
		- If a test fails only on a specific OS (e.g. Windows CRLF, path separators), fix the test to be cross-platform.
		- If a test is flaky, make it deterministic.
		- Make the smallest correct root-cause fix.
		- Do not refactor beyond what is needed for that root-cause fix.
		- Verify the fix by running the most relevant commands locally before finishing.`
	}

	prompt := fmt.Sprintf(
		`%s

Context:
- branch: %s
- base commit: %s
- target commit: %s
- PR number: %s
- failing checks: %s
- merge conflict: %v

		Rules:
		%s`,
		promptIntro,
		sctx.Run.Branch,
		promptBaseSHA,
		sctx.Run.HeadSHA,
		pr.Number,
		strings.Join(failingNames, ", "),
		mergeConflict,
		promptRules,
	)
	if mergeConflict {
		prompt += fmt.Sprintf("\n- rebase target commit: %s", rebaseBaseSHA)
	}
	if logOutput != "" {
		prompt += fmt.Sprintf(`

CI logs:
%s`, logOutput)
	}
	prompt += userIntentPromptSection(sctx)
	prompt += configuredPromptSection(sctx, s.Name())
	prompt = testguidance.LateRepairPrompt(string(s.Name()), prompt)
	prompt += `
- Return JSON with a single "summary" field when you are done.
- The summary must be one concise sentence fragment suitable for a git commit subject.
- Keep the summary under 10 words.`

	agentStartingHeadSHA, err := stepGitHeadSHA(sctx)
	if err != nil {
		return false, "", fmt.Errorf("resolve head before CI repair: %w", err)
	}
	sctx.Log("running agent to fix CI issues...")
	result, err := sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		return false, "", fmt.Errorf("agent CI fix: %w", err)
	}

	var persistRepairPush func(string, string, db.PushBinding) (int64, error)
	if repairRound != nil {
		persistRepairPush = func(headSHA, summary string, binding db.PushBinding) (int64, error) {
			repairRound.verifiedPush = true
			generation, err := sctx.DB.PersistCIFixRepairPush(sctx.Run.ID, repairRound.id, headSHA, summary, binding)
			if err != nil {
				return 0, pipeline.NewCIFixRepairDurabilityError(fmt.Errorf("persist pushed CI repair: %w", err))
			}
			return generation, nil
		}
	}
	return s.commitAndPushAttributed(sctx, result, agentStartingHeadSHA, persistRepairPush)
}

// commitAndPush commits any uncommitted changes and force-pushes to the
// configured push remote.
// Returns (true, nil) when changes were pushed, (false, nil) when there was
// nothing to commit, or (false, err) on failure.
func (s *CIStep) commitAndPush(sctx *pipeline.StepContext, summary string) (bool, error) {
	if strings.TrimSpace(summary) == "" {
		return false, fmt.Errorf("read CI repair commit summary: summary is empty")
	}
	pushed, _, err := s.commitAndPushResolved(sctx, nil, summary, "", nil)
	return pushed, err
}

func (s *CIStep) commitAndPushAttributed(sctx *pipeline.StepContext, result *agent.Result, agentStartingHeadSHA string, persistRepairPush func(string, string, db.PushBinding) (int64, error)) (bool, string, error) {
	recordedHeadSHA := sctx.Run.HeadSHA
	_, summary, err := s.commitAndPushResolved(sctx, result, "", agentStartingHeadSHA, persistRepairPush)
	if err != nil {
		return false, summary, err
	}
	if sctx.Run.HeadSHA == recordedHeadSHA || summary == "" {
		return false, "", nil
	}
	return true, summary, nil
}

func (s *CIStep) commitAndPushResolved(sctx *pipeline.StepContext, result *agent.Result, summary, agentStartingHeadSHA string, persistRepairPush func(string, string, db.PushBinding) (int64, error)) (bool, string, error) {
	status, err := stepGitRun(sctx, "status", "--porcelain")
	if err != nil {
		return false, "", fmt.Errorf("check CI changes: %w", err)
	}
	if strings.TrimSpace(status) == "" {
		sctx.Log("no changes to commit")
		headSHA, err := stepGitHeadSHA(sctx)
		if err == nil && headSHA != sctx.Run.HeadSHA {
			if summary == "" && result != nil && headSHA != agentStartingHeadSHA {
				summary, err = extractCommitSummary(result)
				if err != nil {
					return false, "", fmt.Errorf("read CI repair commit summary: %w", err)
				}
				if summary == "" {
					return false, "", fmt.Errorf("read CI repair commit summary: summary is empty")
				}
			}
			if persistRepairPush != nil && strings.TrimSpace(summary) == "" {
				return false, "", fmt.Errorf("read CI repair commit summary: summary is empty")
			}
			pushed, err := s.pushCIFixHeadSHA(sctx, strings.TrimSpace(headSHA), summary, persistRepairPush)
			if err != nil || summary == "" {
				return pushed, summary, err
			}
			return pushed, summary, nil
		}
		return false, "", nil
	}
	if summary == "" {
		summary, err = extractCommitSummary(result)
		if err != nil {
			return false, "", fmt.Errorf("read CI repair commit summary: %w", err)
		}
		if summary == "" {
			return false, "", fmt.Errorf("read CI repair commit summary: summary is empty")
		}
	}

	if _, err := stepGitRun(sctx, "add", "-A"); err != nil {
		return false, "", fmt.Errorf("stage CI changes: %w", err)
	}
	message, err := attributedAgentFixCommitMessage(sctx, types.StepCI, summary, result)
	if err != nil {
		return false, "", err
	}
	if _, err := stepGitRun(sctx, "commit", "-m", message); err != nil {
		return false, "", fmt.Errorf("commit: %w", err)
	}
	headSHA, err := stepGitHeadSHA(sctx)
	if err != nil {
		return false, "", fmt.Errorf("resolve head after commit: %w", err)
	}

	pushed, err := s.pushCIFixHeadSHA(sctx, strings.TrimSpace(headSHA), summary, persistRepairPush)
	return pushed, summary, err
}

func (s *CIStep) pushCIFixHeadSHA(sctx *pipeline.StepContext, headSHA, summary string, persistRepairPush func(string, string, db.PushBinding) (int64, error)) (bool, error) {
	if persistRepairPush == nil {
		return s.pushUpdatedHeadSHA(sctx, headSHA, nil)
	}
	return s.pushUpdatedHeadSHA(sctx, headSHA, func(binding db.PushBinding) (int64, error) {
		return persistRepairPush(headSHA, summary, binding)
	})
}

func (s *CIStep) pushUpdatedHeadSHA(sctx *pipeline.StepContext, newHeadSHA string, persistVerifiedPush func(db.PushBinding) (int64, error)) (pushed bool, runErr error) {
	ref := normalizedBranchRef(sctx.Run.Branch)
	pushURL := resolvePushURL(sctx)
	receipt := newPushReceiptRecorder(sctx, pushURL, ref)
	receipt.setPushedSHA(newHeadSHA)
	if err := receipt.start(); err != nil {
		return false, err
	}
	purpose := pushOperationPurpose(sctx)
	defer func() {
		if receiptErr := receipt.finish(runErr); receiptErr != nil {
			runErr = errors.Join(runErr, receiptErr)
		}
	}()

	// Anchor the force-with-lease to the head the run last recorded for this
	// branch (what the pipeline last pushed/observed), NOT to a SHA freshly read
	// from the remote a moment before pushing - that self-defeating anchor always
	// passes and lets an auto-fix rebased from stale local state overwrite a
	// commit that reached origin out of band. resolveForcePushDecision refuses
	// the push when the remote carries commits this run never incorporated.
	gitRun := func(args ...string) (string, error) {
		return durablePushGitCommand(sctx, receipt, purpose, args...)
	}
	receipt.lastSeenSHA = pushReceiptStringPointer(sctx.Run.HeadSHA)
	receipt.persistProgress()
	decision, err := resolveForcePushDecision(gitRun, pushURL, ref, newHeadSHA, sctx.Run.HeadSHA, sctx.Run.BaseSHA)
	if err != nil {
		receipt.setDecision(db.PushLeaseOrForceDecisionUnavailable, err.Error(), decision.remoteSHA)
		var refusal *forcePushWouldDiscardError
		if errors.As(err, &refusal) {
			receipt.markRefusedAtRemote(err.Error(), refusal.remoteSHA)
		}
		return false, err
	}
	receipt.lastSeenSHA = pushReceiptStringPointer(sctx.Run.HeadSHA)
	receipt.persistProgress()
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
	targetKind := "upstream"
	if strings.TrimSpace(sctx.Repo.ForkURL) != "" {
		targetKind = "fork"
	}
	persistBinding := func() error {
		remoteOut, err := durablePushGitCommand(sctx, receipt, purpose, "ls-remote", pushURL, ref)
		if err != nil {
			return fmt.Errorf("verify successful push: %w", err)
		}
		if err := recordVerifiedPushRemoteSHA(receipt, remoteOut, newHeadSHA); err != nil {
			return fmt.Errorf("verify successful push: %w", err)
		}
		binding := db.PushBinding{
			HeadSHA:           newHeadSHA,
			TargetKind:        targetKind,
			TargetFingerprint: branchsync.TargetFingerprint(pushURL),
			Ref:               ref,
		}
		var generation int64
		if persistVerifiedPush != nil {
			generation, err = persistVerifiedPush(binding)
		} else if receipt.enabled() {
			generation, err = sctx.DB.UpdateRunPushBindingWithGenerationForOperation(sctx.Run.ID, binding, receipt.operationID)
		} else {
			err = sctx.DB.UpdateRunPushBinding(sctx.Run.ID, binding)
		}
		if err != nil {
			return err
		}
		if receipt.enabled() {
			receipt.recordBinding(generation)
		}
		return nil
	}
	if decision.upToDate {
		if err := persistBinding(); err != nil {
			return false, err
		}
		if _, err := durablePushGitCommand(sctx, receipt, purpose, "update-ref", ref, newHeadSHA); err != nil {
			updateErr := fmt.Errorf("update local branch ref: %w", err)
			if persistVerifiedPush != nil {
				return false, pipeline.NewCIFixRepairDurabilityError(updateErr)
			}
			return false, updateErr
		}
		sctx.Run.HeadSHA = newHeadSHA
		if persistVerifiedPush == nil {
			if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHeadSHA); err != nil {
				return false, err
			}
		}
		return false, nil
	}
	receipt.transportStarted = true
	pushArgs := []string{"push", pushURL}
	if !decision.newBranch {
		pushArgs = append(pushArgs, "--force-with-lease="+ref+":"+decision.remoteSHA)
	}
	pushArgs = append(pushArgs, newHeadSHA+":"+ref)
	if _, err := durablePushGitCommand(sctx, receipt, purpose, pushArgs...); err != nil {
		return false, fmt.Errorf("push: %w", err)
	}
	if err := persistBinding(); err != nil {
		return false, err
	}

	if _, err := durablePushGitCommand(sctx, receipt, purpose, "update-ref", ref, newHeadSHA); err != nil {
		updateErr := fmt.Errorf("update local branch ref: %w", err)
		if persistVerifiedPush != nil {
			return false, pipeline.NewCIFixRepairDurabilityError(updateErr)
		}
		return false, updateErr
	}
	sctx.Run.HeadSHA = newHeadSHA
	if persistVerifiedPush == nil {
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHeadSHA); err != nil {
			return false, err
		}
	}

	sctx.Log("committed and pushed fixes")
	return true, nil
}
