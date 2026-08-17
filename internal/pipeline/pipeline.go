package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const maxCommandEvidenceBytes = 8 * 1024

const commandEvidenceTruncationMarker = "… [command truncated]"

var ErrFatalGateReconciliation = errors.New("fatal gate reconciliation")

// AgentRoutes is an immutable run-scoped routing table. Steps with no explicit
// entry use Default, preserving the run-wide agent behavior.
type AgentRoutes struct {
	Default          agent.Agent
	ByStep           map[types.StepName]agent.Agent
	ReviewCandidates []agent.Agent
}

// AgentForStep returns the configured step route or the run-wide fallback.
func (r AgentRoutes) AgentForStep(step types.StepName) agent.Agent {
	if routed := r.ByStep[step]; routed != nil {
		return routed
	}
	return r.Default
}

// StepContext provides shared resources to pipeline steps during execution.
type StepContext struct {
	Ctx                   context.Context
	Run                   *db.Run
	Repo                  *db.Repo
	WorkDir               string
	Agent                 agent.Agent
	Reviewer              agent.Agent
	Config                *config.Config
	DB                    *db.DB
	Log                   func(string) // discrete log line (newline-terminated, user-visible + file)
	LogChunk              func(string) // raw streaming chunk (user-visible + file)
	LogFile               func(string) // file-only log callback (not shown to user)
	Fixing                bool         // true when re-executing after a "fix" action
	SkipFixExecution      bool         // replay an already-completed fix round's review turn only
	ReviewStartingHeadSHA string
	PreviousFindings      string // JSON findings from the previous execution (set during fix loop)
	// StepResultID is the DB row ID of the current step's step_results record.
	// Steps use it to query their own round history for multi-round prompts.
	StepResultID string
	Round        int
	// PlannedCommand is retained across repair rounds so an unconfigured
	// command gate reruns the exact command selected before the failure.
	PlannedCommand  string
	commandSequence int
	EvidenceDir     string
	Env             []string // extra environment variables for subprocesses (used in tests)
	// UserIntent is a short, possibly-empty summary of what the change author
	// was trying to accomplish. It's surfaced in step prompts so agents have
	// context beyond the diff. Its authority depends on IntentSource: an
	// explicit `--intent` is the author's own goal statement, while an
	// inferred summary comes from a local agent transcript.
	UserIntent string
	// IntentSource records the provenance of UserIntent so steps can weigh
	// its authority. db.RunIntentSourceAgent ("agent") means the driving
	// agent supplied it explicitly via `axi run --intent`; db.RunIntentSourceRerun
	// ("rerun") means that authoritative intent was inherited. Both are
	// authoritative acceptance criteria; an agent name ("claude", "codex", ...)
	// means it was inferred from a transcript (a hint). Empty when no intent exists.
	IntentSource string
	// PRNote is optional author-supplied content that the PR step renders
	// verbatim and supplies to the summary prompt as trusted guidance.
	PRNote string
	// UncertifiedFromSHA/ToSHA/SourceRunID name a previous run's fixer commits
	// whose replacement full review did not complete.
	UncertifiedFromSHA     string
	UncertifiedToSHA       string
	UncertifiedSourceRunID string
	UncertifiedPriorRounds []*db.StepRound
	// Sessions manages the run's durable review-loop agent sessions
	// (reviewer and fixer roles). nil runs every invocation cold.
	Sessions           *RunSessions
	CommandPlanning    *CommandPlanningWorkspace
	CIReadinessChanged func(ready, declaredNoCI bool)
	OnPRMerged         func(ctx context.Context, runID string)
}

// RecordCommand appends one primary step command to the run's bounded
// evidence. It is observability, never a gate: the write is best-effort
// because it happens after the command already ran, so a busy database or an
// oversized evidence blob must not abort a step whose remote branch, worktree,
// or commit history has already moved.
func (sctx *StepContext) RecordCommand(command string, exitCode *int, runErr error) {
	if sctx == nil || sctx.DB == nil || sctx.StepResultID == "" {
		return
	}
	outcome := db.CommandOutcomePassed
	if runErr != nil {
		outcome = db.CommandOutcomeError
	} else if exitCode != nil && *exitCode != 0 {
		outcome = db.CommandOutcomeFailed
	}
	sctx.commandSequence++
	display := boundedCommandDisplay(safeurl.RedactText(intent.RedactSecrets(strings.TrimSpace(command))))
	if err := sctx.DB.AppendStepCommandEvidence(sctx.StepResultID, db.CommandEvidence{
		Round: sctx.Round, Sequence: sctx.commandSequence, Command: display, Outcome: outcome, ExitCode: exitCode,
	}); err != nil {
		slog.Warn("failed to record step command evidence", "step_result", sctx.StepResultID, "err", err)
	}
}

// RecordEvidence appends one non-shell observation the step verified itself.
// It shares RecordCommand's best-effort contract: evidence never gates a step.
func (sctx *StepContext) RecordEvidence(note string) {
	if sctx == nil || sctx.DB == nil || sctx.StepResultID == "" {
		return
	}
	note = boundedCommandDisplay(safeurl.RedactText(intent.RedactSecrets(strings.TrimSpace(note))))
	if note == "" {
		return
	}
	if err := sctx.DB.AppendStepEvidenceNote(sctx.StepResultID, note); err != nil {
		slog.Warn("failed to record step evidence", "step_result", sctx.StepResultID, "err", err)
	}
}

func boundedCommandDisplay(command string) string {
	if len(command) <= maxCommandEvidenceBytes {
		return command
	}
	end := maxCommandEvidenceBytes - len(commandEvidenceTruncationMarker)
	for end > 0 && !utf8.ValidString(command[:end]) {
		end--
	}
	return command[:end] + commandEvidenceTruncationMarker
}

// RunAgentSession executes one turn of a durable review-loop role session,
// running cold when sessions are unavailable. Only the review step's fixer
// turns use this; every other agent invocation - including every review turn,
// which must stay independent of the session that prescribed the fixes under
// review - goes through sctx.Agent.Run directly and stays session-isolated.
func (sctx *StepContext) RunAgentSession(role SessionRole, opts agent.RunOpts) (*agent.Result, error) {
	if sctx.Sessions == nil {
		return sctx.Agent.Run(sctx.Ctx, opts)
	}
	return sctx.Sessions.Run(sctx.Ctx, sctx.Agent, role, opts, sctx.Log)
}

// StepOutcome is the result of executing a pipeline step.
type StepOutcome struct {
	NeedsApproval bool // whether the step pauses for user action
	AutoFixable   bool
	Findings      string // JSON findings for TUI display (optional)
	ExitCode      int    // process exit code (0 = success)
	PRURL         string // PR/MR URL if this step created or found one
	Skipped       bool   // mark the step as skipped without failing the run
	// SkipReason explains a self-determined Skipped outcome in one sentence.
	// It is recorded as the step's evidence explanation so a rendered PR states
	// why the step did not run rather than guessing at its provenance.
	SkipReason    string
	SkipRemaining bool // skip all subsequent steps (e.g. empty diff after refresh)
	// FixSummary, when non-empty, is the agent's one-line commit summary for
	// the fix attempt performed during this round. Steps populate it in fix
	// mode so the executor can persist it on the round record and later
	// rounds can reference what was previously attempted.
	FixSummary string
	// ReviewApprovedHeadSHA is set only by a successfully executed full review
	// round. The executor durably records it only when the review step actually
	// completes, never while that outcome is parked or after a failed round.
	ReviewApprovedHeadSHA string

	// DurationOverrideMS, when positive, replaces the wall-clock duration
	// reported for this step. Used by demo mode to show realistic durations
	// without actually waiting.
	DurationOverrideMS int64
}

// Step is the interface that each pipeline step implements.
type Step interface {
	// Name returns the step's identity in the fixed pipeline sequence.
	Name() types.StepName

	// Execute runs the step logic and returns an outcome.
	// A step that returns NeedsApproval=true will pause the pipeline
	// until the user responds with an approval action.
	Execute(sctx *StepContext) (*StepOutcome, error)
}

// ApprovalGateReconciler is implemented by a step whose parked approval gate
// can become obsolete when an external source of truth changes. The executor
// invokes it with a bounded context while also waiting for an approval. A true
// result completes the step through the normal success path; false or an error
// leaves the gate parked. Implementations must be read-only and fail closed.
type ApprovalGateReconciler interface {
	ReconcileApprovalGate(sctx *StepContext) (resolved bool, err error)
}
