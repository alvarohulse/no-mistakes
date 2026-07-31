package types

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
)

// RunStatus represents the lifecycle state of a pipeline run.
type RunStatus string

const (
	RunPending   RunStatus = "pending"
	RunRunning   RunStatus = "running"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

const (
	RunCancelReasonAbortedByUser = "cancelled: aborted by user"
	RunCancelReasonSuperseded    = "cancelled: superseded by new push"
)

// StepName identifies a pipeline step.
type StepName string

const (
	StepIntent   StepName = "intent"
	StepRefresh  StepName = "refresh"
	StepReview   StepName = "review"
	StepTest     StepName = "test"
	StepDocument StepName = "document"
	StepLint     StepName = "lint"
	StepPush     StepName = "push"
	StepPR       StepName = "pr"
	StepCI       StepName = "ci"
)

// RefreshStrategy controls how the refresh step incorporates its base branch.
type RefreshStrategy string

const (
	RefreshStrategyRebase RefreshStrategy = "rebase"
	RefreshStrategyMerge  RefreshStrategy = "merge"
)

// ParseRefreshStrategy validates a configured or command-line strategy. An
// empty value means no explicit selection and is resolved by OrDefault.
func ParseRefreshStrategy(value string) (RefreshStrategy, error) {
	strategy := RefreshStrategy(strings.ToLower(strings.TrimSpace(value)))
	switch strategy {
	case "", RefreshStrategyRebase, RefreshStrategyMerge:
		return strategy, nil
	default:
		return "", fmt.Errorf("unsupported refresh strategy %q (expected rebase or merge)", value)
	}
}

// OrDefault resolves an unset strategy to the safe historical behavior.
func (s RefreshStrategy) OrDefault() RefreshStrategy {
	if s == "" {
		return RefreshStrategyRebase
	}
	return s
}

func normalizeStepName(s StepName) StepName {
	switch s {
	case "babysit":
		return StepCI
	case "rebase":
		return StepRefresh
	default:
		return s
	}
}

// Canonical returns the persisted machine identity for a step, including
// historical aliases that older runs may still carry.
func (s StepName) Canonical() StepName {
	return normalizeStepName(s)
}

// DisplayName returns the human-facing step label. Refresh keeps one canonical
// identity while its label reflects the strategy selected for the run.
func (s StepName) DisplayName(strategy RefreshStrategy) string {
	switch s.Canonical() {
	case StepIntent:
		return "Intent"
	case StepRefresh:
		if strategy.OrDefault() == RefreshStrategyMerge {
			return "Merge"
		}
		return "Rebase"
	case StepReview:
		return "Review"
	case StepTest:
		return "Test"
	case StepDocument:
		return "Document"
	case StepLint:
		return "Lint"
	case StepPush:
		return "Push"
	case StepPR:
		return "PR"
	case StepCI:
		return "CI"
	default:
		return string(s)
	}
}

func (s *StepName) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = normalizeStepName(StepName(raw))
	return nil
}

func (s *StepName) Scan(src any) error {
	switch v := src.(type) {
	case string:
		*s = normalizeStepName(StepName(v))
		return nil
	case []byte:
		*s = normalizeStepName(StepName(v))
		return nil
	case nil:
		*s = ""
		return nil
	default:
		return fmt.Errorf("scan StepName from %T", src)
	}
}

func (s StepName) Value() (driver.Value, error) {
	return string(normalizeStepName(s)), nil
}

// StepOrder returns the fixed execution order for a step (1-indexed).
func (s StepName) Order() int {
	switch s {
	case StepIntent:
		return 1
	case StepRefresh:
		return 2
	case StepReview:
		return 3
	case StepTest:
		return 4
	case StepDocument:
		return 5
	case StepLint:
		return 6
	case StepPush:
		return 7
	case StepPR:
		return 8
	case StepCI:
		return 9
	default:
		return 0
	}
}

// AllSteps returns all pipeline steps in execution order.
func AllSteps() []StepName {
	return []StepName{StepIntent, StepRefresh, StepReview, StepTest, StepDocument, StepLint, StepPush, StepPR, StepCI}
}

// StepStatus represents the lifecycle state of a pipeline step.
type StepStatus string

const (
	StepStatusPending          StepStatus = "pending"
	StepStatusRunning          StepStatus = "running"
	StepStatusAwaitingApproval StepStatus = "awaiting_approval"
	StepStatusFixing           StepStatus = "fixing"
	StepStatusFixReview        StepStatus = "fix_review"
	StepStatusCompleted        StepStatus = "completed"
	StepStatusSkipped          StepStatus = "skipped"
	StepStatusFailed           StepStatus = "failed"
)

// ApprovalAction represents user responses at approval points.
type ApprovalAction string

const (
	ActionApprove ApprovalAction = "approve"
	ActionFix     ApprovalAction = "fix"
	ActionSkip    ApprovalAction = "skip"
	ActionAbort   ApprovalAction = "abort"
)

// AgentName identifies a supported agent backend. Explicit ACP targets use
// dynamic acp:<target> values; first-class ACP aliases have constants below.
type AgentName string

const (
	AgentAuto     AgentName = "auto"
	AgentClaude   AgentName = "claude"
	AgentCodex    AgentName = "codex"
	AgentRovoDev  AgentName = "rovodev"
	AgentOpenCode AgentName = "opencode"
	AgentPi       AgentName = "pi"
	AgentCopilot  AgentName = "copilot"
	AgentCursor   AgentName = "cursor"
)

// AgentInvocationMode identifies how an agent process or nested agent was
// invoked. The vocabulary intentionally stops at the harness boundary; SDK
// internals that adapters cannot observe are not inferred.
type AgentInvocationMode string

const (
	AgentInvocationModeHarnessCLI   AgentInvocationMode = "harness_cli"
	AgentInvocationModeSubagentTool AgentInvocationMode = "subagent_tool"
)

// AgentObservation is one nested agent invocation observed in an adapter's
// event stream. Identity is a bounded agent name or privacy-safe fingerprint,
// never a prompt or model response.
type AgentObservation struct {
	Identity       string              `json:"identity"`
	InvocationMode AgentInvocationMode `json:"invocation_mode"`
}

// ACPAlias describes a first-class agent name that resolves to an ACP target.
type ACPAlias struct {
	Name           AgentName
	Target         string
	DefaultCommand string
}

var acpAliases = []ACPAlias{
	{Name: AgentCursor, Target: "cursor", DefaultCommand: "cursor-agent acp"},
}

// ACPAliasFor returns the ACP alias metadata for a first-class agent name.
func ACPAliasFor(name AgentName) (ACPAlias, bool) {
	for _, alias := range acpAliases {
		if alias.Name == name {
			return alias, true
		}
	}
	return ACPAlias{}, false
}

// ACPAliasForTarget returns the ACP alias metadata for a raw ACP target.
func ACPAliasForTarget(target string) (ACPAlias, bool) {
	for _, alias := range acpAliases {
		if alias.Target == target {
			return alias, true
		}
	}
	return ACPAlias{}, false
}

// ACPAliases returns all first-class ACP aliases.
func ACPAliases() []ACPAlias {
	out := make([]ACPAlias, len(acpAliases))
	copy(out, acpAliases)
	return out
}

// DefaultCommandBinary returns the executable named by the alias default command.
func (a ACPAlias) DefaultCommandBinary() string {
	fields := strings.Fields(a.DefaultCommand)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// ACPTargetFor resolves the ACP target an agent name drives: the alias target
// for a first-class alias, or the parsed target of an explicit acp:<target>
// name. Returns false for non-ACP agent names.
func ACPTargetFor(name AgentName) (string, bool) {
	if alias, ok := ACPAliasFor(name); ok {
		return alias.Target, true
	}
	value := string(name)
	if !strings.HasPrefix(value, "acp:") {
		return "", false
	}
	target := strings.TrimPrefix(value, "acp:")
	if target == "" || strings.ContainsAny(target, " \t\r\n") {
		return "", false
	}
	return target, true
}

// IsBareACPModelName reports whether model can be selected faithfully through
// ACP target startup. Bracketed Cursor variants are normalized by its ACP
// server, so every bracket form fails closed before launch.
func IsBareACPModelName(model string) bool {
	return model != "" && !strings.ContainsAny(model, "[]")
}

// ACPRawCommand resolves the raw command acpx runs for an ACP target: a
// registry override is trimmed and wins when non-blank, otherwise the alias
// default command is used.
// Empty means acpx dispatches the target through its own registry.
func ACPRawCommand(target string, overrides map[string]string) string {
	if override := strings.TrimSpace(overrides[target]); override != "" {
		return override
	}
	if alias, ok := ACPAliasForTarget(target); ok {
		return alias.DefaultCommand
	}
	return ""
}
