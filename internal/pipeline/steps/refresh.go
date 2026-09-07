package steps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/artifact"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runner"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/testguidance"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RefreshStep syncs the pushed branch with the configured push target and its
// freshly fetched authoritative base branch.
type RefreshStep struct{}

func (s *RefreshStep) Name() types.StepName { return types.StepRefresh }

const forkBranchRefPrefix = "refs/remotes/no-mistakes-push/"

const maxRefreshDiagnosticBytes = 32 * 1024

type refreshCommandExitError struct {
	command string
	code    int
	output  string
}

func (e *refreshCommandExitError) Error() string {
	if strings.TrimSpace(e.output) == "" {
		return fmt.Sprintf("git %s exited with code %d", e.command, e.code)
	}
	return fmt.Sprintf("git %s exited with code %d: %s", e.command, e.code, safeurl.RedactText(strings.TrimSpace(e.output)))
}

func (e *refreshCommandExitError) ExitCode() int { return e.code }

type refreshReceiptRecorder struct {
	sctx                 *pipeline.StepContext
	strategy             types.RefreshStrategy
	sourceRef            string
	authoritativeBaseRef string
	authoritativeBaseSHA *string
	enabled              bool
}

type refreshOperationBuilder struct {
	recorder             *refreshReceiptRecorder
	targetRef            string
	startingHeadSHA      string
	startedAt            time.Time
	commandAttemptIDs    []string
	diagnosticArtifactID *string
}

func newRefreshReceiptRecorder(sctx *pipeline.StepContext, strategy types.RefreshStrategy, sourceRef, authoritativeBaseRef string) *refreshReceiptRecorder {
	return &refreshReceiptRecorder{
		sctx:                 sctx,
		strategy:             strategy.OrDefault(),
		sourceRef:            sourceRef,
		authoritativeBaseRef: authoritativeBaseRef,
		enabled:              sctx != nil && sctx.DB != nil && sctx.Run != nil && sctx.StepResultID != "" && sctx.RoundID != "" && sctx.Paths != nil,
	}
}

func (r *refreshReceiptRecorder) begin(targetRef string) *refreshOperationBuilder {
	startingHead := ""
	if r != nil && r.sctx != nil {
		startingHead = strings.TrimSpace(r.sctx.Run.HeadSHA)
		if headSHA, err := git.HeadSHA(r.sctx.Ctx, r.sctx.WorkDir); err == nil && strings.TrimSpace(headSHA) != "" {
			startingHead = strings.TrimSpace(headSHA)
		}
	}
	if startingHead == "" {
		startingHead = git.EmptyTreeSHA
	}
	return &refreshOperationBuilder{
		recorder:        r,
		targetRef:       targetRef,
		startingHeadSHA: startingHead,
		startedAt:       time.Now(),
	}
}

func (r *refreshReceiptRecorder) recordRefusal(targetRef string, decision db.RefreshDecision, reason string) error {
	operation := r.begin(targetRef)
	return operation.finish(decision, db.RefreshConflictStateNone, db.RefreshRepairStateNotAttempted, reason)
}

func (o *refreshOperationBuilder) refreshScopeAttemptIDs() (map[string]struct{}, error) {
	ids := make(map[string]struct{})
	if o == nil || o.recorder == nil || !o.recorder.enabled {
		return ids, nil
	}
	attempts, err := o.recorder.sctx.DB.GetCommandAttemptsByRun(o.recorder.sctx.Run.ID)
	if err != nil {
		return nil, fmt.Errorf("load refresh command attempts: %w", err)
	}
	for _, attempt := range attempts {
		if attempt.StepID != o.recorder.sctx.StepResultID || attempt.RoundID != o.recorder.sctx.RoundID {
			continue
		}
		ids[attempt.ID] = struct{}{}
	}
	return ids, nil
}

func (o *refreshOperationBuilder) captureAttemptsStartedAfter(before map[string]struct{}) error {
	if o == nil || o.recorder == nil || !o.recorder.enabled {
		return nil
	}
	after, err := o.refreshScopeAttemptIDs()
	if err != nil {
		return err
	}
	for attemptID := range after {
		if _, alreadyPresent := before[attemptID]; alreadyPresent {
			continue
		}
		alreadyLinked := false
		for _, existingID := range o.commandAttemptIDs {
			if existingID == attemptID {
				alreadyLinked = true
				break
			}
		}
		if alreadyLinked {
			continue
		}
		o.commandAttemptIDs = append(o.commandAttemptIDs, attemptID)
	}
	return nil
}

func (o *refreshOperationBuilder) finish(decision db.RefreshDecision, conflictState db.RefreshConflictState, repairState db.RefreshRepairState, diagnostic string) error {
	if o == nil || o.recorder == nil || !o.recorder.enabled {
		return nil
	}
	resultingHead := o.startingHeadSHA
	if headSHA, err := git.HeadSHA(o.recorder.sctx.Ctx, o.recorder.sctx.WorkDir); err == nil && strings.TrimSpace(headSHA) != "" {
		resultingHead = strings.TrimSpace(headSHA)
	}
	if o.diagnosticArtifactID == nil && len(o.commandAttemptIDs) == 0 && strings.TrimSpace(diagnostic) != "" {
		store, err := artifact.NewStore(o.recorder.sctx.Paths, "")
		if err != nil {
			return fmt.Errorf("create refresh diagnostic store: %w", err)
		}
		targetDigest := sha256.Sum256([]byte(o.targetRef))
		name := fmt.Sprintf("refresh-%s-%x", o.recorder.sctx.RoundID, targetDigest[:8])
		metadata, err := store.CreateOperationDiagnostic(o.recorder.sctx.Run.ID, name, boundedRefreshDiagnostic(diagnostic))
		if err != nil {
			return fmt.Errorf("create refresh diagnostic: %w", err)
		}
		metadata.RunID = o.recorder.sctx.Run.ID
		metadata.StepID = refreshStringPointer(o.recorder.sctx.StepResultID)
		metadata.RoundID = refreshStringPointer(o.recorder.sctx.RoundID)
		registered, err := o.recorder.sctx.DB.RegisterArtifact(metadata)
		if err != nil {
			return fmt.Errorf("register refresh diagnostic: %w", err)
		}
		o.diagnosticArtifactID = &registered.ID
	}
	completedAt := time.Now()
	operation := db.RefreshOperation{
		RunID:                o.recorder.sctx.Run.ID,
		StepID:               o.recorder.sctx.StepResultID,
		RoundID:              o.recorder.sctx.RoundID,
		Strategy:             o.recorder.strategy,
		SourceRef:            o.recorder.sourceRef,
		DestinationRef:       o.targetRef,
		AuthoritativeBaseRef: o.recorder.authoritativeBaseRef,
		AuthoritativeBaseSHA: o.recorder.authoritativeBaseSHA,
		StartingHeadSHA:      o.startingHeadSHA,
		Decision:             decision,
		ResultingHeadSHA:     resultingHead,
		ConflictState:        conflictState,
		RepairState:          repairState,
		CommandAttemptIDs:    append([]string(nil), o.commandAttemptIDs...),
		StartedAt:            o.startedAt.UnixMilli(),
		CompletedAt:          completedAt.UnixMilli(),
		DurationMS:           maxInt64(0, completedAt.Sub(o.startedAt).Milliseconds()),
		DiagnosticArtifactID: o.diagnosticArtifactID,
	}
	if _, err := o.recorder.sctx.DB.InsertRefreshOperation(operation); err != nil {
		return fmt.Errorf("insert refresh receipt: %w", err)
	}
	return nil
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func refreshStringPointer(value string) *string { return &value }

func boundedRefreshDiagnostic(value string) []byte {
	value = safeurl.RedactText(strings.TrimSpace(value))
	if len(value) <= maxRefreshDiagnosticBytes {
		return []byte(value)
	}
	marker := "\n… [refresh diagnostic truncated]"
	limit := maxRefreshDiagnosticBytes - len(marker)
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return []byte(value[:limit] + marker)
}

func (s *RefreshStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	defaultBranch := strings.TrimSpace(sctx.Repo.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	baseBranch := refreshBaseBranch(sctx, defaultBranch)
	strategy := sctx.Run.RefreshStrategy.OrDefault()
	sourceRef := strings.TrimSpace(sctx.Run.Branch)
	if sourceRef == "" {
		sourceRef = "HEAD"
	}
	authoritativeBaseRef := "origin/" + baseBranch
	receipts := newRefreshReceiptRecorder(sctx, strategy, sourceRef, authoritativeBaseRef)
	branchTarget := ""
	pushRemote := resolveUpstreamURL(sctx)
	if branch != "" {
		branchTarget = "origin/" + branch
		if strings.TrimSpace(sctx.Repo.ForkURL) != "" {
			pushRemote = sctx.Repo.PushURL()
			branchTarget = forkBranchTrackingRef(branch)
		}
	}

	// Detect force push before fetching so we can skip pushed-branch sync.
	// A force push means the user explicitly rewrote the branch - the pushed
	// commit is authoritative and must not be overwritten by prior pipeline
	// state on the remote.
	forcePush := isForcePushAgainstRemote(ctx, sctx.WorkDir, pushRemote, branch, branchTarget, sctx.Run.BaseSHA)

	sctx.Log("fetching latest upstream state...")
	if err := fetchRunUpstreamBranch(ctx, sctx, baseBranch); err != nil {
		wrappedErr := fmt.Errorf("fetch authoritative base origin/%s: %w", baseBranch, err)
		if receiptErr := receipts.recordRefusal(authoritativeBaseRef, db.RefreshDecisionError, wrappedErr.Error()); receiptErr != nil {
			wrappedErr = errors.Join(wrappedErr, receiptErr)
		}
		return nil, wrappedErr
	}
	if baseSHA, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", authoritativeBaseRef+"^{commit}"); err == nil && strings.TrimSpace(baseSHA) != "" {
		receipts.authoritativeBaseSHA = refreshStringPointer(strings.TrimSpace(baseSHA))
	} else if receiptErr := receipts.recordRefusal(authoritativeBaseRef, db.RefreshDecisionError, fmt.Sprintf("resolve authoritative base %s: %v", authoritativeBaseRef, err)); receiptErr != nil {
		return nil, errors.Join(fmt.Errorf("resolve authoritative base %s: %w", authoritativeBaseRef, err), receiptErr)
	} else {
		return nil, fmt.Errorf("resolve authoritative base %s: %w", authoritativeBaseRef, err)
	}
	// Sync the push branch's remote-tracking ref only when we are about to rebase
	// onto it (a normal push). On a force push we deliberately skip both the fetch
	// and the rebase: the pushed commit is authoritative, and the remote-tracking
	// ref must keep pointing at the head we last *observed* rather than the live
	// tip. The push step uses that tracking ref as its force-with-lease anchor;
	// if we refreshed it here, the anchor would equal the live remote head and the
	// lease's "remote unchanged since we last saw it" fast path would pass even
	// when the remote carries an out-of-band commit - silently clobbering it
	// (the original #281/#305 hazard, in the force-push path). Leaving it stale is
	// what lets the push step's content check catch that case.
	if !forcePush && branch != "" && branch != defaultBranch {
		if strings.TrimSpace(sctx.Repo.ForkURL) == "" {
			if err := fetchRunUpstreamBranch(ctx, sctx, branch); err != nil {
				sctx.LogFile(fmt.Sprintf("warning: could not fetch origin/%s: %v", branch, err))
			}
		} else if err := git.FetchRemoteBranchToRef(ctx, sctx.WorkDir, pushRemote, branch, branchTarget); err != nil {
			sctx.LogFile(fmt.Sprintf("warning: could not fetch %s: %v", branchTarget, err))
		}
	}

	// Stop before refreshing when the gated branch carries commits that live on
	// the contributor's local default branch but were never pushed to
	// origin/<default>. The check also applies to stacked branches unless their
	// effective base already carries those commits.
	if outcome := detectBundledLocalDefaultCommits(ctx, sctx, branch, defaultBranch, baseBranch); outcome != nil {
		if receiptErr := receipts.recordRefusal(authoritativeBaseRef, db.RefreshDecisionRefused, outcome.Findings); receiptErr != nil {
			return nil, receiptErr
		}
		return outcome, nil
	}
	if forcePush && branch == defaultBranch && remoteDefaultBranchAdvanced(ctx, sctx.WorkDir, defaultBranch, sctx.Run.BaseSHA) {
		findingsJSON, _ := json.Marshal(Findings{
			Items: []Finding{{
				Severity:    "warning",
				File:        filepath.Join("internal", "pipeline", "steps", "refresh.go"),
				Description: fmt.Sprintf("origin/%s advanced after the force push; manual review required before updating the default branch", defaultBranch),
			}},
			Summary: fmt.Sprintf("remote %s advanced during force push", defaultBranch),
		})
		if receiptErr := receipts.recordRefusal(authoritativeBaseRef, db.RefreshDecisionRefused, string(findingsJSON)); receiptErr != nil {
			return nil, receiptErr
		}
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			Findings:      string(findingsJSON),
		}, nil
	}

	targets := refreshTargetsForBranch(branch, baseBranch, branchTarget)
	if forcePush {
		sctx.Log("force push detected, skipping " + branchTarget + " sync")
		if branchTarget != "" && branch != baseBranch {
			if receiptErr := receipts.begin(branchTarget).finish(db.RefreshDecisionSkipped, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, ""); receiptErr != nil {
				return nil, receiptErr
			}
		}
		targets = forcePushRefreshTargets(branch, baseBranch)
	}

	if sctx.Fixing {
		for _, target := range targets {
			if err := refreshWithAgent(ctx, sctx, strategy, target, receipts); err != nil {
				return nil, err
			}
		}
		return updateHeadSHA(ctx, sctx)
	}

	// Normal mode: try all refresh targets, tracking every conflict.
	var conflictTargets []string
	var conflictFindings []Finding
	for _, target := range targets {
		conflictFiles, err := tryRefresh(ctx, sctx, strategy, target, receipts)
		if err != nil {
			return nil, err
		}
		if len(conflictFiles) > 0 {
			conflictTargets = append(conflictTargets, target)
			for _, file := range conflictFiles {
				conflictFindings = append(conflictFindings, Finding{
					Severity:    "warning",
					File:        file,
					Description: refreshConflictDescription(strategy, target),
				})
			}
		}
	}

	if len(conflictTargets) > 0 {
		summary := fmt.Sprintf("conflict during %s refresh with %s", strategy, strings.Join(conflictTargets, ", "))
		findingsJSON, _ := json.Marshal(Findings{Items: dedupeRefreshFindings(conflictFindings), Summary: summary})
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      string(findingsJSON),
		}, nil
	}

	return updateHeadSHA(ctx, sctx)
}

// BaseBranchForRun is the branch a run refreshes onto and targets its PR at:
// its stacked-on parent when set, else the repository default branch. It is the
// single owner of that rule so a caller reconstructing a run outside a step
// context cannot drift from what the live step used.
func BaseBranchForRun(run *db.Run, defaultBranch string) string {
	if run != nil {
		if stackedOn := strings.TrimSpace(run.StackedOn); stackedOn != "" {
			return stackedOn
		}
	}
	return defaultBranch
}

func refreshBaseBranch(sctx *pipeline.StepContext, defaultBranch string) string {
	return BaseBranchForRun(sctx.Run, defaultBranch)
}

// refreshTargets returns the ordered list of refs to incorporate.
func refreshTargets(branch, baseBranch string) []string {
	return refreshTargetsForBranch(branch, baseBranch, "origin/"+branch)
}

func refreshTargetsForBranch(branch, baseBranch, branchTarget string) []string {
	var targets []string
	if branch != "" && branch != baseBranch {
		targets = append(targets, branchTarget)
	}
	if branch != baseBranch {
		targets = append(targets, "origin/"+baseBranch)
	}
	return targets
}

// forcePushRefreshTargets returns refresh targets for a force push. The pushed
// branch target is skipped because it may contain autofix commits from prior
// pipeline runs that the force push intended to discard.
func forcePushRefreshTargets(branch, baseBranch string) []string {
	if branch == baseBranch {
		return nil
	}
	return []string{"origin/" + baseBranch}
}

// detectBundledLocalDefaultCommits returns a blocking finding when the gated
// branch carries commits that exist on the contributor's local default branch
// but were never pushed to origin/<default>. In multi-session / monorepo setups
// the local default branch routinely carries another workstream's unpushed
// work; branching a fix off that local tip silently drags it into the PR when
// the branch is rebased onto the remote default. Returns nil when no such
// divergence is detected so the run proceeds normally.
//
// It only flags commits the branch actually carries: it reads the local default
// tip from the working repo, confirms that tip is ahead of origin/<default> and
// is an ancestor of the branch HEAD, then enumerates the unpushed commits.
// Detection is best-effort - if the local default tip advanced past the branch
// point, or the working repo cannot be read, it returns nil rather than guess.
func detectBundledLocalDefaultCommits(ctx context.Context, sctx *pipeline.StepContext, branch, defaultBranch, baseBranch string) *pipeline.StepOutcome {
	if branch == "" || branch == defaultBranch {
		return nil
	}
	workingPath := strings.TrimSpace(sctx.Repo.WorkingPath)
	if workingPath == "" {
		return nil
	}
	localTip, err := git.Run(ctx, workingPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+defaultBranch+"^{commit}")
	if err != nil {
		return nil
	}
	localTip = strings.TrimSpace(localTip)
	if localTip == "" {
		return nil
	}
	remoteRef := "origin/" + defaultBranch
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", "--quiet", remoteRef+"^{commit}"); err != nil {
		return nil
	}
	baseRef := "origin/" + baseBranch
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", "--quiet", baseRef+"^{commit}"); err != nil {
		return nil
	}
	// The local default tip must be present in the gate's object store (it is
	// when the branch carries it as an ancestor) for the reachability checks.
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", "--quiet", localTip+"^{commit}"); err != nil {
		return nil
	}
	// Already pushed (local default not ahead of remote) -> nothing bundled.
	if isAncestor(ctx, sctx.WorkDir, localTip, remoteRef) {
		return nil
	}
	// The branch must actually carry the local default tip's commits.
	if !isAncestor(ctx, sctx.WorkDir, localTip, "HEAD") {
		return nil
	}

	logArgs := []string{"log", "--oneline", "--no-decorate", remoteRef + ".." + localTip}
	if baseRef != remoteRef {
		logArgs = append(logArgs, "^"+baseRef)
	}
	subjects, err := git.Run(ctx, sctx.WorkDir, logArgs...)
	if err != nil || strings.TrimSpace(subjects) == "" {
		return nil
	}
	commits := strings.Split(strings.TrimSpace(subjects), "\n")
	files, _ := git.DiffNameOnly(ctx, sctx.WorkDir, remoteRef, localTip)
	if baseRef != remoteRef {
		if mergeBase, err := git.Run(ctx, sctx.WorkDir, "merge-base", baseRef, localTip); err == nil {
			files, _ = git.DiffNameOnly(ctx, sctx.WorkDir, strings.TrimSpace(mergeBase), localTip)
		}
	}
	firstFile := ""
	if len(files) > 0 {
		firstFile = files[0]
	}

	description := fmt.Sprintf(
		"branch carries %d commit(s) that exist on your local %s branch but were never pushed to origin/%s; rebasing would bundle this unrelated work (%d file(s)) into the PR:\n- %s\n\nPush %s to origin, or rebase your branch onto origin/%s, before gating.",
		len(commits), defaultBranch, defaultBranch, len(files), strings.Join(commits, "\n- "), defaultBranch, defaultBranch,
	)
	if baseBranch != defaultBranch {
		description = fmt.Sprintf(
			"branch carries %d commit(s) that exist on your local %s branch, were never pushed to origin/%s, and are absent from origin/%s; refreshing would bundle this unrelated work (%d file(s)) into the PR:\n- %s\n\nPush %s to origin, add the commits to %s, or rebase your branch onto origin/%s, before gating.",
			len(commits), defaultBranch, defaultBranch, baseBranch, len(files), strings.Join(commits, "\n- "), defaultBranch, baseBranch, baseBranch,
		)
	}
	findingsJSON, _ := json.Marshal(Findings{
		Items: []Finding{{
			Severity:    "warning",
			File:        firstFile,
			Description: description,
			// Bundling another workstream's unpushed commits is a workflow call
			// the contributor must make (push <default>, rebase, or proceed); the
			// pipeline cannot safely auto-resolve it. Mark it ask-user so the gate
			// classifies it correctly and the driving agent escalates.
			Action: types.ActionAskUser,
		}},
		Summary: fmt.Sprintf("branch bundles %d unpushed %s commit(s)", len(commits), defaultBranch),
	})
	return &pipeline.StepOutcome{
		NeedsApproval: true,
		AutoFixable:   false,
		Findings:      string(findingsJSON),
	}
}

func isAncestor(ctx context.Context, workDir, ancestor, descendant string) bool {
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

func remoteDefaultBranchAdvanced(ctx context.Context, workDir, defaultBranch, baseSHA string) bool {
	if baseSHA == "" || git.IsZeroSHA(baseSHA) {
		return false
	}
	remoteSHA, err := git.Run(ctx, workDir, "rev-parse", "--verify", "origin/"+defaultBranch)
	if err != nil {
		return false
	}
	return strings.TrimSpace(remoteSHA) != baseSHA
}

// isForcePush returns true when the current push is non-fast-forward relative
// to the previous push (baseSHA). This indicates the user explicitly rewrote
// history and the pipeline should treat the new HEAD as authoritative.
func isForcePush(ctx context.Context, workDir, branch, baseSHA string) bool {
	localRef := ""
	if branch != "" {
		localRef = "origin/" + branch
	}
	return isForcePushAgainstRemote(ctx, workDir, "origin", branch, localRef, baseSHA)
}

func isForcePushAgainstRemote(ctx context.Context, workDir, remote, branch, localRef, baseSHA string) bool {
	if git.IsZeroSHA(baseSHA) || baseSHA == "" {
		return false
	}
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", baseSHA, "HEAD")
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return false
	}
	if branch != "" {
		remoteSHA, err := git.LsRemote(ctx, workDir, remote, "refs/heads/"+branch)
		if err == nil && remoteSHA != "" {
			_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", remoteSHA, "HEAD")
			if err == nil {
				return false
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				return true
			}
		}
		if localRef != "" {
			if _, err := git.Run(ctx, workDir, "rev-parse", "--verify", localRef); err == nil {
				return isRemoteBranchRewritten(ctx, workDir, localRef)
			}
		}
	}
	return false
}

func forkBranchTrackingRef(branch string) string {
	return forkBranchRefPrefix + branch
}

func isRemoteBranchRewritten(ctx context.Context, workDir, remoteRef string) bool {
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", remoteRef, "HEAD")
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

func tryRefresh(ctx context.Context, sctx *pipeline.StepContext, strategy types.RefreshStrategy, targetRef string, receipts *refreshReceiptRecorder) ([]string, error) {
	switch strategy.OrDefault() {
	case types.RefreshStrategyRebase:
		return tryRebase(ctx, sctx, targetRef, receipts)
	case types.RefreshStrategyMerge:
		return tryMerge(ctx, sctx, targetRef, receipts)
	default:
		return nil, fmt.Errorf("unsupported refresh strategy %q", strategy)
	}
}

func refreshWithAgent(ctx context.Context, sctx *pipeline.StepContext, strategy types.RefreshStrategy, targetRef string, receipts *refreshReceiptRecorder) error {
	switch strategy.OrDefault() {
	case types.RefreshStrategyRebase:
		return rebaseWithAgent(ctx, sctx, targetRef, receipts)
	case types.RefreshStrategyMerge:
		return mergeWithAgent(ctx, sctx, targetRef, receipts)
	default:
		return fmt.Errorf("unsupported refresh strategy %q", strategy)
	}
}

func refreshConflictDescription(strategy types.RefreshStrategy, targetRef string) string {
	if strategy.OrDefault() == types.RefreshStrategyMerge {
		return fmt.Sprintf("merge conflict merging %s", targetRef)
	}
	return fmt.Sprintf("merge conflict rebasing onto %s", targetRef)
}

// tryRebase attempts a rebase onto targetRef. Returns conflicted files when the
// rebase stops on merge conflicts. The rebase is aborted before returning.
func tryRebase(ctx context.Context, sctx *pipeline.StepContext, targetRef string, receipts *refreshReceiptRecorder) ([]string, error) {
	operation := receipts.begin(targetRef)
	decision, err := prepareRefreshTarget(ctx, sctx, targetRef, operation)
	if err != nil {
		if receiptErr := operation.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateNotAttempted, err.Error()); receiptErr != nil {
			err = errors.Join(err, receiptErr)
		}
		return nil, err
	}
	if decision != "" {
		if receiptErr := operation.finish(decision, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, ""); receiptErr != nil {
			return nil, receiptErr
		}
		return nil, nil
	}

	sctx.Log(fmt.Sprintf("rebasing onto %s...", targetRef))
	if _, err := runRefreshPrimary(ctx, sctx, operation, "rebase", targetRef); err != nil {
		conflictFiles := rebaseConflictFiles(ctx, sctx.WorkDir)
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")

		if len(conflictFiles) == 0 {
			if receiptErr := operation.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateNotAttempted, err.Error()); receiptErr != nil {
				err = errors.Join(err, receiptErr)
			}
			return nil, fmt.Errorf("rebase onto %s: %w", targetRef, err)
		}
		if receiptErr := operation.finish(db.RefreshDecisionConflicted, db.RefreshConflictStateDetected, db.RefreshRepairStateNotAttempted, ""); receiptErr != nil {
			return nil, receiptErr
		}
		return conflictFiles, nil
	}
	if receiptErr := operation.finish(db.RefreshDecisionRebased, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, ""); receiptErr != nil {
		return nil, receiptErr
	}
	return nil, nil
}

// rebaseWithAgent performs a rebase and uses the agent to resolve any conflicts.
func rebaseWithAgent(ctx context.Context, sctx *pipeline.StepContext, targetRef string, receipts *refreshReceiptRecorder) error {
	operation := receipts.begin(targetRef)
	decision, err := prepareRefreshTarget(ctx, sctx, targetRef, operation)
	if err != nil {
		if receiptErr := operation.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateFailed, err.Error()); receiptErr != nil {
			err = errors.Join(err, receiptErr)
		}
		return err
	}
	if decision != "" {
		return operation.finish(decision, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, "")
	}

	sctx.Log(fmt.Sprintf("rebasing onto %s...", targetRef))
	_, err = runRefreshPrimary(ctx, sctx, operation, "rebase", targetRef)
	if err == nil {
		return operation.finish(db.RefreshDecisionRebased, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, "")
	}

	if len(rebaseConflictFiles(ctx, sctx.WorkDir)) == 0 {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
		operationErr := fmt.Errorf("rebase onto %s failed (no conflicts detected): %w", targetRef, err)
		if receiptErr := operation.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateFailed, operationErr.Error()); receiptErr != nil {
			operationErr = errors.Join(operationErr, receiptErr)
		}
		return operationErr
	}
	sctx.Log("conflicts detected, asking agent to resolve...")
	conflictFiles := rebaseConflictFiles(ctx, sctx.WorkDir)

	prompt := fmt.Sprintf(
		`Resolve git rebase conflicts. The rebase of the current branch onto %s has conflicts.

Current conflicted files:
- %s

Instructions:
- Find all conflicting files and resolve the conflict markers (<<<<<<< ======= >>>>>>>).
- After resolving each file, stage it with: git add <file>
- After all conflicts are resolved, run: git rebase --continue
- If additional conflicts arise during rebase --continue, resolve those too.
- Do not modify any files that don't have conflicts.
- Preserve the intent of both the current branch changes and the upstream changes.
- Return JSON with a single "summary" field describing what you resolved.
- Keep the summary under 10 words.`,
		targetRef,
		strings.Join(conflictFiles, "\n- "),
	)
	if sctx.PreviousFindings != "" {
		prompt += "\n\nPrevious findings:\n" + sctx.PreviousFindings
	}
	prompt += userIntentPromptSection(sctx)
	prompt += configuredPromptSection(sctx, types.StepRefresh)
	prompt = testguidance.LateRepairPrompt(string(types.StepRefresh), prompt)

	_, err = sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
		repairErr := fmt.Errorf("agent resolve conflicts: %w", err)
		if receiptErr := operation.finish(db.RefreshDecisionConflicted, db.RefreshConflictStateDetected, db.RefreshRepairStateFailed, repairErr.Error()); receiptErr != nil {
			repairErr = errors.Join(repairErr, receiptErr)
		}
		return repairErr
	}

	// Verify rebase completed (no rebase still in progress)
	if rebaseInProgress(ctx, sctx.WorkDir) {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
		repairErr := fmt.Errorf("agent did not complete the rebase")
		if receiptErr := operation.finish(db.RefreshDecisionConflicted, db.RefreshConflictStateDetected, db.RefreshRepairStateFailed, repairErr.Error()); receiptErr != nil {
			repairErr = errors.Join(repairErr, receiptErr)
		}
		return repairErr
	}

	sctx.RecordEvidence(fmt.Sprintf("Agent resolved the rebase conflicts onto %s; no rebase remained in progress.", targetRef))
	return operation.finish(db.RefreshDecisionRepaired, db.RefreshConflictStateResolved, db.RefreshRepairStateSucceeded, "")
}

func tryMerge(ctx context.Context, sctx *pipeline.StepContext, targetRef string, receipts *refreshReceiptRecorder) ([]string, error) {
	operation := receipts.begin(targetRef)
	decision, err := prepareRefreshTarget(ctx, sctx, targetRef, operation)
	if err != nil {
		if receiptErr := operation.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateNotAttempted, err.Error()); receiptErr != nil {
			err = errors.Join(err, receiptErr)
		}
		return nil, err
	}
	if decision != "" {
		if receiptErr := operation.finish(decision, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, ""); receiptErr != nil {
			return nil, receiptErr
		}
		return nil, nil
	}

	sctx.Log(fmt.Sprintf("merging %s...", targetRef))
	if _, err := runRefreshPrimary(ctx, sctx, operation, "merge", "--no-edit", targetRef); err != nil {
		conflictFiles := refreshConflictFiles(ctx, sctx.WorkDir)
		_, _ = git.Run(ctx, sctx.WorkDir, "merge", "--abort")
		if len(conflictFiles) == 0 {
			if receiptErr := operation.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateNotAttempted, err.Error()); receiptErr != nil {
				err = errors.Join(err, receiptErr)
			}
			return nil, fmt.Errorf("merge %s: %w", targetRef, err)
		}
		if receiptErr := operation.finish(db.RefreshDecisionConflicted, db.RefreshConflictStateDetected, db.RefreshRepairStateNotAttempted, ""); receiptErr != nil {
			return nil, receiptErr
		}
		return conflictFiles, nil
	}
	if receiptErr := operation.finish(db.RefreshDecisionMerged, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, ""); receiptErr != nil {
		return nil, receiptErr
	}
	return nil, nil
}

func mergeWithAgent(ctx context.Context, sctx *pipeline.StepContext, targetRef string, receipts *refreshReceiptRecorder) error {
	operation := receipts.begin(targetRef)
	decision, err := prepareRefreshTarget(ctx, sctx, targetRef, operation)
	if err != nil {
		if receiptErr := operation.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateFailed, err.Error()); receiptErr != nil {
			err = errors.Join(err, receiptErr)
		}
		return err
	}
	if decision != "" {
		return operation.finish(decision, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, "")
	}

	sctx.Log(fmt.Sprintf("merging %s...", targetRef))
	_, err = runRefreshPrimary(ctx, sctx, operation, "merge", "--no-edit", targetRef)
	if err == nil {
		return operation.finish(db.RefreshDecisionMerged, db.RefreshConflictStateNone, db.RefreshRepairStateNotNeeded, "")
	}

	conflictFiles := refreshConflictFiles(ctx, sctx.WorkDir)
	if len(conflictFiles) == 0 {
		_, _ = git.Run(ctx, sctx.WorkDir, "merge", "--abort")
		operationErr := fmt.Errorf("merge %s failed (no conflicts detected): %w", targetRef, err)
		if receiptErr := operation.finish(db.RefreshDecisionError, db.RefreshConflictStateNone, db.RefreshRepairStateFailed, operationErr.Error()); receiptErr != nil {
			operationErr = errors.Join(operationErr, receiptErr)
		}
		return operationErr
	}
	sctx.Log("conflicts detected, asking agent to resolve...")
	prompt := fmt.Sprintf(
		`Resolve git merge conflicts. Merging %s into the current branch has conflicts.

Current conflicted files:
- %s

Instructions:
- Find all conflicting files and resolve the conflict markers (<<<<<<< ======= >>>>>>>).
- After resolving each file, stage it with: git add <file>
- After all conflicts are resolved, run: git merge --continue
- Do not modify any files that don't have conflicts.
- Preserve the intent of both the current branch changes and the base branch changes.
- Return JSON with a single "summary" field describing what you resolved.
- Keep the summary under 10 words.`,
		targetRef,
		strings.Join(conflictFiles, "\n- "),
	)
	if sctx.PreviousFindings != "" {
		prompt += "\n\nPrevious findings:\n" + sctx.PreviousFindings
	}
	prompt += userIntentPromptSection(sctx)
	prompt += configuredPromptSection(sctx, types.StepRefresh)
	prompt = testguidance.LateRepairPrompt(string(types.StepRefresh), prompt)

	_, err = sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.LogChunk,
	})
	if err != nil {
		_, _ = git.Run(ctx, sctx.WorkDir, "merge", "--abort")
		repairErr := fmt.Errorf("agent resolve conflicts: %w", err)
		if receiptErr := operation.finish(db.RefreshDecisionConflicted, db.RefreshConflictStateDetected, db.RefreshRepairStateFailed, repairErr.Error()); receiptErr != nil {
			repairErr = errors.Join(repairErr, receiptErr)
		}
		return repairErr
	}
	if mergeInProgress(ctx, sctx.WorkDir) {
		_, _ = git.Run(ctx, sctx.WorkDir, "merge", "--abort")
		repairErr := fmt.Errorf("agent did not complete the merge")
		if receiptErr := operation.finish(db.RefreshDecisionConflicted, db.RefreshConflictStateDetected, db.RefreshRepairStateFailed, repairErr.Error()); receiptErr != nil {
			repairErr = errors.Join(repairErr, receiptErr)
		}
		return repairErr
	}
	sctx.RecordEvidence(fmt.Sprintf("Agent resolved the merge conflicts from %s; no merge remained in progress.", targetRef))
	return operation.finish(db.RefreshDecisionRepaired, db.RefreshConflictStateResolved, db.RefreshRepairStateSucceeded, "")
}

func recordRefreshCommand(sctx *pipeline.StepContext, command string, runErr error) {
	if runErr == nil {
		zero := 0
		sctx.RecordCommand(command, &zero, nil)
		return
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(runErr, &exitErr) {
		exitCode := exitErr.ExitCode()
		sctx.RecordCommand(command, &exitCode, nil)
		return
	}
	sctx.RecordCommand(command, nil, runErr)
}

func runRefreshPrimary(ctx context.Context, sctx *pipeline.StepContext, operation *refreshOperationBuilder, args ...string) (string, error) {
	command := refreshGitCommand(sctx, args...)
	if operation == nil || operation.recorder == nil || !operation.recorder.enabled {
		output, err := git.Run(ctx, sctx.WorkDir, args...)
		recordRefreshCommand(sctx, command, err)
		return output, err
	}

	before, err := operation.refreshScopeAttemptIDs()
	if err != nil {
		return "", err
	}
	env := git.NonInteractiveEnvFrom(sctx.Env, sctx.WorkDir)
	output, exitCode, err := runStepRunnerCommandWithEnv(sctx, runnerCommand(command), string(types.StepRefresh), env)
	if captureErr := operation.captureAttemptsStartedAfter(before); captureErr != nil {
		err = errors.Join(err, captureErr)
	}
	if err != nil {
		return output, err
	}
	if exitCode != 0 {
		return output, &refreshCommandExitError{command: strings.TrimPrefix(command, "git "), code: exitCode, output: output}
	}
	return output, nil
}

func runnerCommand(command string) runner.Command {
	return runner.Command{Run: command}
}

func refreshGitCommand(sctx *pipeline.StepContext, args ...string) string {
	parts := []string{"git"}
	powershell := runtime.GOOS == "windows"
	if sctx != nil && sctx.Config != nil {
		switch strings.ToLower(strings.TrimSpace(sctx.Config.Runner.Executable)) {
		case "pwsh", "powershell":
			powershell = true
		case "sh", "bash", "zsh":
			powershell = false
		}
	}
	gitArgs := args
	if sctx != nil && git.LooksLikeBareRepository(sctx.WorkDir) {
		gitArgs = append([]string{"--git-dir=" + sctx.WorkDir}, gitArgs...)
	}
	for _, arg := range gitArgs {
		if refreshShellWordSafe(arg) {
			parts = append(parts, arg)
			continue
		}
		if powershell {
			parts = append(parts, refreshPowerShellQuote(arg))
		} else {
			parts = append(parts, refreshPOSIXQuote(arg))
		}
	}
	return strings.Join(parts, " ")
}

func refreshShellWordSafe(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			continue
		}
		switch char {
		case '/', '.', '_', ':', '@', '+', ',', '-', '=', '%':
			continue
		default:
			return false
		}
	}
	return true
}

func refreshPOSIXQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func refreshPowerShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// prepareRefreshTarget checks whether incorporating targetRef can be skipped.
// Returns a semantic decision when no rebase or merge remains necessary.
func prepareRefreshTarget(ctx context.Context, sctx *pipeline.StepContext, targetRef string, operation *refreshOperationBuilder) (db.RefreshDecision, error) {
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", targetRef); err != nil {
		return db.RefreshDecisionSkipped, nil
	}
	localSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return "", fmt.Errorf("get local head: %w", err)
	}
	targetSHA, err := git.Run(ctx, sctx.WorkDir, "rev-parse", targetRef)
	if err != nil {
		return "", fmt.Errorf("get target head %s: %w", targetRef, err)
	}
	if localSHA == targetSHA {
		sctx.Log(fmt.Sprintf("already up-to-date with %s", targetRef))
		return db.RefreshDecisionSkipped, nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "merge-base", "--is-ancestor", targetRef, "HEAD"); err == nil {
		sctx.Log(fmt.Sprintf("already ahead of %s", targetRef))
		return db.RefreshDecisionSkipped, nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "merge-base", "--is-ancestor", "HEAD", targetRef); err == nil {
		sctx.Log(fmt.Sprintf("fast-forwarding to %s", targetRef))
		if _, err := runRefreshPrimary(ctx, sctx, operation, "reset", "--hard", targetRef); err != nil {
			return "", fmt.Errorf("fast-forward to %s: %w", targetRef, err)
		}
		return db.RefreshDecisionFastForwarded, nil
	}
	return "", nil
}

// rebaseInProgress returns true if a git rebase is currently in progress.
// Uses git rev-parse --git-path which works for both regular repos and worktrees.
func rebaseInProgress(ctx context.Context, workDir string) bool {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		p, err := git.Run(ctx, workDir, "rev-parse", "--git-path", dir)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(workDir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func mergeInProgress(ctx context.Context, workDir string) bool {
	_, err := git.Run(ctx, workDir, "rev-parse", "--verify", "--quiet", "MERGE_HEAD")
	return err == nil
}

func rebaseConflictFiles(ctx context.Context, workDir string) []string {
	return refreshConflictFiles(ctx, workDir)
}

func refreshConflictFiles(ctx context.Context, workDir string) []string {
	out, err := git.Run(ctx, workDir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		files = append(files, line)
	}
	return files
}

func dedupeRefreshFindings(findings []Finding) []Finding {
	if len(findings) < 2 {
		return findings
	}
	seen := make(map[string]bool, len(findings))
	filtered := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		key := finding.File + "\x00" + finding.Description
		if seen[key] {
			continue
		}
		seen[key] = true
		filtered = append(filtered, finding)
	}
	return filtered
}

// updateHeadSHA syncs the run's head SHA after refresh and checks for an empty
// diff against the effective base branch.
func updateHeadSHA(ctx context.Context, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve head after refresh: %w", err)
	}
	if headSHA != "" && headSHA != sctx.Run.HeadSHA {
		oldHead := sctx.Run.HeadSHA
		pipeline.RemapUncertifiedPipelineRangeAfterRebase(sctx, oldHead, headSHA)
		sctx.Run.HeadSHA = headSHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
			return nil, err
		}
		sctx.Log(fmt.Sprintf("updated head SHA to %s", shortSHA(headSHA)))
	}

	// Check if the branch has any diff against its effective base branch.
	// If the diff is empty (e.g. branch was already merged), skip remaining steps.
	defaultBranch := strings.TrimSpace(sctx.Repo.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	baseBranch := refreshBaseBranch(sctx, defaultBranch)
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, baseBranch)
	diff, err := git.Diff(ctx, sctx.WorkDir, baseSHA, "HEAD")
	if err == nil && strings.TrimSpace(diff) == "" {
		sctx.Log("empty diff after refresh, skipping remaining steps")
		return &pipeline.StepOutcome{SkipRemaining: true}, nil
	}

	return &pipeline.StepOutcome{}, nil
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
