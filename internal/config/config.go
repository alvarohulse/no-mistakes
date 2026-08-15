package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"gopkg.in/yaml.v3"
)

// CI monitor timeout constants.
//
// CITimeout is interpreted by the CI step as the maximum time to babysit an
// open PR with no base-branch movement before giving up. The monitor re-arms
// this timer every time the base branch advances (see internal/pipeline/steps
// ci.go), so an actively-rebased PR keeps its monitor. The value is
// deliberately long because a green PR can legitimately wait days on a
// dependency PR or on review; a torn-down or abandoned run is reaped
// explicitly via `no-mistakes axi abort --run <id>` rather than by a short
// timeout.
const (
	// DefaultCITimeout is the monitor's idle timeout when ci_timeout is unset.
	DefaultCITimeout = 7 * 24 * time.Hour
	// DefaultStepQuietWarning is how long a running/fixing step can go without
	// a new log or lifecycle activity before AXI status marks it quiet.
	DefaultStepQuietWarning = 10 * time.Minute
	// DefaultDaemonConnectTimeout bounds client IPC connection attempts to a
	// daemon socket that exists but is not accepting connections.
	DefaultDaemonConnectTimeout = 3 * time.Second
	// DefaultProcessTerminationGrace is the maximum time a process group gets
	// to exit after SIGTERM before cleanup escalates to SIGKILL.
	DefaultProcessTerminationGrace = shellenv.DefaultProcessTerminationGrace
	// CITimeoutUnlimited is the sentinel meaning "monitor until the PR is
	// merged, closed, or the run is aborted - never self-terminate".
	// Any non-positive ci_timeout, or the keywords "unlimited", "none",
	// "off", and "never", resolves to this.
	CITimeoutUnlimited = time.Duration(-1)
	// DefaultCIRerunTransient is the per-check rerun budget the CI step uses
	// when ci.rerun_transient is unset. It is 0 because GitHub's CANCELLED
	// conclusion does not carry a cause: the same value covers a provider
	// aborting its own infrastructure, a maintainer stopping a runaway or
	// unsafe job, and repository concurrency with cancel-in-progress. Until a
	// reliable cause signal exists, restarting on that ambiguity risks
	// re-running work a person deliberately stopped, so rerunning cancelled
	// checks is an explicit opt-in rather than a default.
	DefaultCIRerunTransient = 0
	// MaxCIRerunTransient caps ci.rerun_transient. Reruns are cheap compared
	// with an agent round, but they are not free: each one keeps the monitor
	// polling the same commit, so the budget stays small by construction.
	MaxCIRerunTransient = 5
	// DefaultEvalMaxCases caps the auto-captured local eval corpus. Cases
	// share one object pool per repository, so the marginal cost of a case is
	// its JSON records plus the objects its commits actually introduced, not a
	// copy of the repository. The cap exists to bound that JSON and to keep
	// the corpus a recent, representative window rather than an archive.
	DefaultEvalMaxCases = 200
	// DefaultEvalDiversifiedSize caps the official gold-only eval set.
	// 0 means one gold case per stratum with no Hamilton bound.
	DefaultEvalDiversifiedSize = 32
	// DefaultEvidenceRetention is how long a run's on-disk evidence survives
	// before the daemon reaps it. It is comfortably longer than typical PR
	// review latency because a PR body references these artifacts by local path
	// whenever publishing is off or the provider has no derivable links. This
	// is no-mistakes' own budget: the point of owning it is that no OS temp
	// timer decides when a user's screenshots disappear.
	DefaultEvidenceRetention = 14 * 24 * time.Hour
	// DefaultEvidenceMaxRuns caps how many run directories survive regardless
	// of age, so a burst of parallel runs that all land inside the retention
	// window still cannot grow the directory without bound.
	DefaultEvidenceMaxRuns = 200
)

// GlobalConfig represents ~/.no-mistakes/config.yaml.
type GlobalConfig struct {
	SourceYAML              []byte              `yaml:"-"`
	Agent                   types.AgentName     `yaml:"agent"`
	Agents                  []types.AgentName   `yaml:"-"`
	ACPXPath                string              `yaml:"acpx_path"`
	ACPRegistryOverrides    map[string]string   `yaml:"acp_registry_overrides"`
	AgentPathOverride       map[string]string   `yaml:"agent_path_override"`
	AgentArgsOverride       map[string][]string `yaml:"agent_args_override"`
	CITimeout               time.Duration       `yaml:"-"`
	StepQuietWarning        time.Duration       `yaml:"-"`
	DaemonConnectTimeout    time.Duration       `yaml:"-"`
	ProcessTerminationGrace time.Duration       `yaml:"-"`
	LogLevel                string              `yaml:"log_level"`
	// SessionReuse controls per-run agent session reuse in the review loop:
	// one durable fixer session across review-fix turns. Review turns always
	// run session-free so the rereview never resumes the session whose
	// findings prescribed the fixes it certifies. Default true; set
	// session_reuse: false to force every invocation cold.
	SessionReuse bool `yaml:"-"`
	// Hooks carries the machine-wide hook defaults. Only pr_body is accepted
	// here: a PR body formatter is the same script for every repo on a
	// machine, while post_worktree is a repo's own install command and stays
	// repo-only.
	Hooks Hooks `yaml:"-"`
	// Overrides carries machine-local per-repository configuration, keyed by
	// the repository's `<owner>/<repo>` identity (for example
	// "scaleapi/scaleapi"). Each entry is a RepoConfig-shaped overlay applied
	// after the committed pushed/trusted resolution for a matching run, with
	// the same field-presence semantics as a committed overlay: only
	// explicitly present fields apply, and explicit empty values clear
	// committed values. Keys are normalized to lowercase; identity matching is
	// owned by OverrideForRepoIdentity.
	Overrides map[string]*RepoConfig `yaml:"-"`
	AutoFix   AutoFixRaw
	Commit    CommitRaw
	Intent    IntentRaw
	Refresh   StepAgentRaw
	Review    ReviewRaw
	Build     StepAgentRaw
	Test      TestRaw
	Document  DocumentRaw
	Lint      StepAgentRaw
	PR        StepAgentRaw
	CI        CIRaw
	Prompts   PromptConfig
	// Eval is resolved at load time because it is global-only: it describes
	// this machine's local eval corpus (disk, retention, whether review rounds
	// record replay provenance), never a repository policy. Keeping it out of
	// RepoConfig means no pushed branch can enable, disable, or resize it.
	Eval Eval
}

// globalConfigRaw is the on-disk YAML representation with duration as string.
type globalConfigRaw struct {
	Agent                   agentList              `yaml:"agent"`
	ACPXPath                string                 `yaml:"acpx_path"`
	ACPRegistryOverrides    map[string]string      `yaml:"acp_registry_overrides"`
	AgentPathOverride       map[string]string      `yaml:"agent_path_override"`
	AgentArgsOverride       map[string][]string    `yaml:"agent_args_override"`
	CITimeout               string                 `yaml:"ci_timeout"`
	DaemonConnectTimeout    string                 `yaml:"daemon_connect_timeout"`
	ProcessTerminationGrace string                 `yaml:"process_termination_grace"`
	BabysitTimeout          string                 `yaml:"babysit_timeout"`
	StepQuietWarning        string                 `yaml:"step_quiet_warning"`
	LogLevel                string                 `yaml:"log_level"`
	SessionReuse            *bool                  `yaml:"session_reuse"`
	Hooks                   Hooks                  `yaml:"hooks"`
	Overrides               map[string]*RepoConfig `yaml:"overrides"`
	AutoFix                 AutoFixRaw             `yaml:"auto_fix"`
	Commit                  CommitRaw              `yaml:"commit"`
	Intent                  IntentRaw              `yaml:"intent"`
	Refresh                 *StepAgentRaw          `yaml:"refresh"`
	LegacyRebase            *StepAgentRaw          `yaml:"rebase"`
	Review                  ReviewRaw              `yaml:"review"`
	Build                   StepAgentRaw           `yaml:"build"`
	Test                    TestRaw                `yaml:"test"`
	Document                DocumentRaw            `yaml:"document"`
	Lint                    StepAgentRaw           `yaml:"lint"`
	PR                      StepAgentRaw           `yaml:"pr"`
	CI                      CIRaw                  `yaml:"ci"`
	Prompts                 PromptConfig           `yaml:"prompts"`
	Eval                    EvalRaw                `yaml:"eval"`
}

// RepoConfig represents .no-mistakes.yaml in a repo root.
type RepoConfig struct {
	Agent          types.AgentName   `yaml:"agent"`
	Agents         []types.AgentName `yaml:"-"`
	Commands       Commands          `yaml:"commands"`
	Hooks          Hooks             `yaml:"hooks"`
	IgnorePatterns []string          `yaml:"ignore_patterns"`
	// AllowRepoCommands opts in to honoring the code-executing selection
	// fields (commands.{build,test,lint,format}, hooks.{post_worktree,pr_body}, agent, and every step agent route) from a contributor's
	// pushed branch instead of the trusted default-branch copy. It is read
	// ONLY from the trusted default-branch copy of .no-mistakes.yaml (never
	// the pushed SHA), so a contributor cannot self-enable. Default false:
	// the pushed branch controls nothing that executes, including model selection.
	AllowRepoCommands bool         `yaml:"allow_repo_commands"`
	AutoFix           AutoFixRaw   `yaml:"auto_fix"`
	Commit            CommitRaw    `yaml:"commit"`
	Intent            IntentRaw    `yaml:"intent"`
	Refresh           RefreshRaw   `yaml:"refresh"`
	Review            ReviewRaw    `yaml:"review"`
	Build             StepAgentRaw `yaml:"build"`
	Test              TestRaw      `yaml:"test"`
	// Document carries the repository's documentation placement policy. It
	// steers the document step's gate prompt, so it is honored ONLY from the
	// trusted default-branch copy of .no-mistakes.yaml (see
	// EffectiveRepoConfig): a contributor's pushed branch must not be able to
	// weaken documentation rules for its own review.
	Document DocumentRaw  `yaml:"document"`
	Lint     StepAgentRaw `yaml:"lint"`
	PR       StepAgentRaw `yaml:"pr"`
	CI       CIRaw        `yaml:"ci"`
	// Prompts appends extra guidance to the built-in pipeline agent prompts.
	// It steers the agents that launch with the maintainer's credentials, so
	// it is honored ONLY from the trusted default-branch copy of
	// .no-mistakes.yaml (see EffectiveRepoConfig) unless allow_repo_commands
	// opts in on that trusted copy.
	Prompts PromptConfig `yaml:"prompts"`
	// DisableProjectSettings opts the repository out of loading project-level
	// agent settings/instructions (AGENTS.md/CLAUDE.md and the equivalent
	// per-harness project settings) into gate agents. It exists for
	// agent-orchestration repos (e.g. firstmate) whose project instructions
	// would otherwise install a fleet-captain identity on a gate agent. It is a
	// SECURITY boundary honored ONLY from the trusted default-branch copy of
	// .no-mistakes.yaml (see EffectiveRepoConfig and the daemon's
	// resolveRunPolicyFromBareGate): a contributor's pushed branch must not be
	// able to turn it off (or on). Default false; a plain bool so a missing key
	// or a YAML/JSON null is falsy and preserves current loading.
	DisableProjectSettings bool `yaml:"disable_project_settings"`
	// NoCI is a trusted readiness boundary. It only permits an empty provider
	// check suite; it never waives a registered pending or failing check.
	NoCI    bool `yaml:"no_ci"`
	present map[string]bool
}

// MarshalYAML emits the same canonical shape accepted by LoadRepoFromBytes.
// RepoConfig keeps normalized fallback lists and presence metadata in derived
// fields that must never leak into captured eval provenance.
func (c RepoConfig) MarshalYAML() (any, error) {
	type repoConfigYAML struct {
		Agent                  agentList    `yaml:"agent,omitempty"`
		Commands               Commands     `yaml:"commands,omitempty"`
		Hooks                  Hooks        `yaml:"hooks,omitempty"`
		IgnorePatterns         []string     `yaml:"ignore_patterns,omitempty"`
		AllowRepoCommands      bool         `yaml:"allow_repo_commands,omitempty"`
		AutoFix                AutoFixRaw   `yaml:"auto_fix,omitempty"`
		Commit                 CommitRaw    `yaml:"commit,omitempty"`
		Intent                 IntentRaw    `yaml:"intent,omitempty"`
		Refresh                RefreshRaw   `yaml:"refresh,omitempty"`
		Review                 ReviewRaw    `yaml:"review,omitempty"`
		Build                  StepAgentRaw `yaml:"build,omitempty"`
		Test                   TestRaw      `yaml:"test,omitempty"`
		Document               DocumentRaw  `yaml:"document,omitempty"`
		Lint                   StepAgentRaw `yaml:"lint,omitempty"`
		PR                     StepAgentRaw `yaml:"pr,omitempty"`
		CI                     CIRaw        `yaml:"ci,omitempty"`
		Prompts                PromptConfig `yaml:"prompts,omitempty"`
		DisableProjectSettings bool         `yaml:"disable_project_settings,omitempty"`
		NoCI                   bool         `yaml:"no_ci,omitempty"`
	}
	return repoConfigYAML{
		Agent:                  agentList(stepAgentNames(c.Agent, c.Agents)),
		Commands:               c.Commands,
		Hooks:                  c.Hooks,
		IgnorePatterns:         c.IgnorePatterns,
		AllowRepoCommands:      c.AllowRepoCommands,
		AutoFix:                c.AutoFix,
		Commit:                 c.Commit,
		Intent:                 c.Intent,
		Refresh:                c.Refresh,
		Review:                 c.Review,
		Build:                  c.Build,
		Test:                   c.Test,
		Document:               c.Document,
		Lint:                   c.Lint,
		PR:                     c.PR,
		CI:                     c.CI,
		Prompts:                c.Prompts,
		DisableProjectSettings: c.DisableProjectSettings,
		NoCI:                   c.NoCI,
	}, nil
}

// StepAgentRaw is the YAML representation of one step's optional agent route.
// Agent is the primary entry and Agents preserves the ordered fallback list.
type StepAgentRaw struct {
	Agent  types.AgentName   `yaml:"-"`
	Agents []types.AgentName `yaml:"-"`
	Model  ModelRoute        `yaml:"model"`
}

func (c StepAgentRaw) MarshalYAML() (any, error) {
	return struct {
		Agent agentList  `yaml:"agent,omitempty"`
		Model ModelRoute `yaml:"model,omitempty"`
	}{
		Agent: agentList(stepAgentNames(c.Agent, c.Agents)),
		Model: c.Model,
	}, nil
}

func (c *StepAgentRaw) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Agent agentList  `yaml:"agent"`
		Model ModelRoute `yaml:"model"`
	}
	if err := decodeKnownFields(value, &raw); err != nil {
		return err
	}
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Model = raw.Model
	return nil
}

// ModelRoute is a controller-known model identity for one pipeline route.
// Vendor is explicit because deriving it from Name could silently classify a
// same-vendor adversarial pair as independent when model naming changes.
type ModelRoute struct {
	Name   string `yaml:"name"`
	Vendor string `yaml:"vendor"`
}

func (m *ModelRoute) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Tag == "!!null" {
		*m = ModelRoute{}
		return nil
	}
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("model must be a mapping with name and vendor")
	}
	var raw struct {
		Name   string `yaml:"name"`
		Vendor string `yaml:"vendor"`
	}
	if err := decodeKnownFields(value, &raw); err != nil {
		return err
	}
	raw.Name = strings.TrimSpace(raw.Name)
	raw.Vendor = strings.TrimSpace(raw.Vendor)
	model := ModelRoute{Name: raw.Name, Vendor: raw.Vendor}
	if err := model.Validate(); err != nil {
		return err
	}
	*m = model
	return nil
}

// Validate checks that a configured model has a complete, canonical identity.
// The zero value remains valid because model routing is optional.
func (m ModelRoute) Validate() error {
	if m.Name == "" && m.Vendor == "" {
		return nil
	}
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("model.name is required when model is configured")
	}
	if m.Name != strings.TrimSpace(m.Name) || strings.IndexFunc(m.Name, unicode.IsControl) >= 0 {
		return fmt.Errorf("model.name must not contain surrounding whitespace or control characters")
	}
	if m.Vendor == "" {
		return fmt.Errorf("model.vendor is required when model is configured")
	}
	if m.Vendor != strings.ToLower(m.Vendor) || !validVendorIdentity(m.Vendor) {
		return fmt.Errorf("model.vendor %q must be a lowercase identifier containing only letters, digits, and hyphens", m.Vendor)
	}
	return nil
}

func validVendorIdentity(vendor string) bool {
	for i, r := range vendor {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0 && i < len(vendor)-1 {
			continue
		}
		return false
	}
	return vendor != ""
}

// ReviewRaw adds the high-risk adversarial route to the ordinary Review
// route. AdversaryAgents is an ordered availability fallback list; it is never
// overloaded as the primary review route.
type ReviewRaw struct {
	StepAgentRaw
	AdversaryAgent   types.AgentName   `yaml:"-"`
	AdversaryAgents  []types.AgentName `yaml:"-"`
	AdversaryModel   ModelRoute        `yaml:"adversary_model"`
	PathInstructions []PathInstruction `yaml:"path_instructions"`
}

func (c ReviewRaw) MarshalYAML() (any, error) {
	return struct {
		Agent            agentList         `yaml:"agent,omitempty"`
		Model            ModelRoute        `yaml:"model,omitempty"`
		AdversaryAgent   agentList         `yaml:"adversary_agent,omitempty"`
		AdversaryModel   ModelRoute        `yaml:"adversary_model,omitempty"`
		PathInstructions []PathInstruction `yaml:"path_instructions,omitempty"`
	}{
		Agent:            agentList(stepAgentNames(c.Agent, c.Agents)),
		Model:            c.Model,
		AdversaryAgent:   agentList(stepAgentNames(c.AdversaryAgent, c.AdversaryAgents)),
		AdversaryModel:   c.AdversaryModel,
		PathInstructions: c.PathInstructions,
	}, nil
}

func (c *ReviewRaw) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Agent            agentList         `yaml:"agent"`
		Model            ModelRoute        `yaml:"model"`
		AdversaryAgent   agentList         `yaml:"adversary_agent"`
		AdversaryModel   ModelRoute        `yaml:"adversary_model"`
		PathInstructions []PathInstruction `yaml:"path_instructions"`
	}
	if err := decodeKnownFields(value, &raw); err != nil {
		return err
	}
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Model = raw.Model
	c.AdversaryAgent = firstAgent(raw.AdversaryAgent)
	c.AdversaryAgents = copyAgents(raw.AdversaryAgent)
	c.AdversaryModel = raw.AdversaryModel
	c.PathInstructions = raw.PathInstructions
	return nil
}

func resolveLegacyStepConfig(refresh, legacyRebase *StepAgentRaw) (StepAgentRaw, error) {
	if refresh != nil && legacyRebase != nil {
		return StepAgentRaw{}, fmt.Errorf("refresh and legacy rebase sections cannot both be set")
	}
	if refresh != nil {
		return *refresh, nil
	}
	if legacyRebase != nil {
		return *legacyRebase, nil
	}
	return StepAgentRaw{}, nil
}

// RefreshRaw is the repository refresh-step configuration. Strategy is
// repository-only because it changes how the branch history is incorporated;
// global refresh configuration remains limited to agent routing.
type RefreshRaw struct {
	Agent    types.AgentName
	Agents   []types.AgentName
	Model    ModelRoute
	Strategy types.RefreshStrategy
}

func (c RefreshRaw) MarshalYAML() (any, error) {
	return struct {
		Agent    agentList             `yaml:"agent,omitempty"`
		Model    ModelRoute            `yaml:"model,omitempty"`
		Strategy types.RefreshStrategy `yaml:"strategy,omitempty"`
	}{
		Agent:    agentList(stepAgentNames(c.Agent, c.Agents)),
		Model:    c.Model,
		Strategy: c.Strategy,
	}, nil
}

func (c *RefreshRaw) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Agent    agentList  `yaml:"agent"`
		Model    ModelRoute `yaml:"model"`
		Strategy string     `yaml:"strategy"`
	}
	if err := decodeKnownFields(value, &raw); err != nil {
		return err
	}
	strategy, err := types.ParseRefreshStrategy(raw.Strategy)
	if err != nil {
		return err
	}
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Model = raw.Model
	c.Strategy = strategy
	return nil
}

func resolveLegacyRepoRefreshConfig(refresh *RefreshRaw, legacyRebase *StepAgentRaw) (RefreshRaw, error) {
	if refresh != nil && legacyRebase != nil {
		return RefreshRaw{}, fmt.Errorf("refresh and legacy rebase sections cannot both be set")
	}
	if refresh != nil {
		return *refresh, nil
	}
	if legacyRebase != nil {
		return RefreshRaw{Agent: legacyRebase.Agent, Agents: copyAgents(legacyRebase.Agents), Model: legacyRebase.Model}, nil
	}
	return RefreshRaw{}, nil
}

// DocumentRaw is the YAML representation of document-step settings.
type DocumentRaw struct {
	Agent  types.AgentName   `yaml:"-"`
	Agents []types.AgentName `yaml:"-"`
	Model  ModelRoute        `yaml:"model"`
	// Instructions augment (never replace) the built-in documentation
	// placement policy with the repository's ownership map or extra
	// placement rules.
	Instructions string `yaml:"instructions"`
}

func (c DocumentRaw) MarshalYAML() (any, error) {
	return struct {
		Agent        agentList  `yaml:"agent,omitempty"`
		Model        ModelRoute `yaml:"model,omitempty"`
		Instructions string     `yaml:"instructions,omitempty"`
	}{
		Agent:        agentList(stepAgentNames(c.Agent, c.Agents)),
		Model:        c.Model,
		Instructions: c.Instructions,
	}, nil
}

func (c *DocumentRaw) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Agent        agentList  `yaml:"agent"`
		Model        ModelRoute `yaml:"model"`
		Instructions string     `yaml:"instructions"`
	}
	if err := decodeKnownFields(value, &raw); err != nil {
		return err
	}
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Model = raw.Model
	c.Instructions = raw.Instructions
	return nil
}

// PathInstruction is one glob-scoped block of review guidance. Path follows the
// same match rules as ignore_patterns: no slash matches by basename, a trailing
// "/**" matches an entire subtree, and anything else is a full-path glob.
type PathInstruction struct {
	Path         string `yaml:"path"`
	Instructions string `yaml:"instructions"`
}

// Review-prompt block frame for review.path_instructions.
//
// The review step renders every matched entry as
//
//	path: <path>
//	matched files: <files>
//	instructions:
//	<instructions>
//
// so each rule travels with the scope it was selected for and no block can read
// as a global instruction. The labels live here rather than in the review step
// because the byte accounting below has to measure the real assembled section,
// not an estimate of it; internal/pipeline/steps builds its blocks from these
// same constants and TestReviewPathInstructionsSectionStaysWithinAccountedBytes
// is the drift check.
const (
	ReviewPathInstructionsHeading    = "Repository review instructions for the changed paths (trusted, from the default branch). Each block below applies only to the files listed under its path, and adds to the requirements above:"
	ReviewPathInstructionsPathLabel  = "path: "
	ReviewPathInstructionsFilesLabel = "matched files: "
	ReviewPathInstructionsRulesLabel = "instructions:"
	// ReviewPathInstructionsMaxFilesBytes bounds the matched-file list a single
	// block may print. A broad glob can match hundreds of files, so the review
	// step truncates the list deterministically and states the remaining count;
	// the accounting charges every entry this full allowance so the cap holds
	// for any diff rather than only for small ones.
	ReviewPathInstructionsMaxFilesBytes = 192
)

// Bounds on review.path_instructions.
//
// The injected text lands in the review prompt, which is already the largest
// gate prompt no-mistakes builds, and an oversized prompt fails the agent
// invocation outright instead of degrading. The budget is therefore validated
// when the config is parsed - before a run starts - rather than truncated
// silently at review time.
const (
	// MaxReviewPathInstructions is the largest number of path_instructions
	// entries a repository may configure.
	MaxReviewPathInstructions = 32
	// MaxReviewPathInstructionsBytes is the largest review-prompt section
	// path_instructions may produce, measured by ReviewPathInstructionsBytes.
	// It leaves room for the entry cap to be reached with a rule of ordinary
	// length, so neither cap makes the other unusable.
	MaxReviewPathInstructionsBytes = 16384
)

// ReviewPathInstructionsBytes returns the largest review-prompt section these
// entries can produce: the leading blank line, the heading, and for every entry
// its labels, its path, its instructions, its full matched-file allowance, and
// the separator before it. Instruction text can only shrink on its way into the
// prompt (conflict markers are removed and whitespace is collapsed), and the
// matched-file list is truncated to its allowance, so the result is an upper
// bound on the real section for any diff.
func ReviewPathInstructionsBytes(entries []PathInstruction) int {
	if len(entries) == 0 {
		return 0
	}
	total := len("\n\n") + len(ReviewPathInstructionsHeading) + len("\n")
	for i, entry := range entries {
		if i > 0 {
			total += len("\n\n")
		}
		total += len(ReviewPathInstructionsPathLabel) + len(strings.TrimSpace(entry.Path)) + len("\n")
		total += len(ReviewPathInstructionsFilesLabel) + ReviewPathInstructionsMaxFilesBytes + len("\n")
		total += len(ReviewPathInstructionsRulesLabel) + len("\n")
		total += len(strings.TrimSpace(entry.Instructions))
	}
	return total
}

// promptConflictMarkers are the merge-conflict tokens the pipeline removes from
// maintainer-authored text before injecting it into an agent prompt
// (sanitizePromptMultilineText in internal/pipeline/steps owns the removal, and
// document.instructions goes through the same path). Validation applies the same
// removal so a value that would reach the reviewer as an empty block is rejected
// here instead of disappearing from the prompt without a word.
var promptConflictMarkers = strings.NewReplacer("<<<<<<<", " ", "=======", " ", ">>>>>>>", " ")

// RenderedInstructions is the emptiness-agreement helper for instruction text,
// not a second copy of the prompt renderer. The real renderer is
// sanitizePromptMultilineText in internal/pipeline/steps, which additionally
// normalizes CR and collapses each line's runs of whitespace; internal/config
// cannot import that package, which is why the conflict-marker replacer above is
// duplicated here at all. Two invariants tie the two together, and the rest of
// this feature silently depends on both:
//
//   - Emptiness agrees exactly. This returns "" for precisely the inputs the
//     prompt renderer reduces to "", so validation can reject a value that would
//     otherwise reach the reviewer as an empty block.
//   - The prompt renderer never lengthens text, so the rendered instructions are
//     no longer than strings.TrimSpace of the raw value and
//     ReviewPathInstructionsBytes stays an upper bound on the assembled section.
//
// A change to sanitizePromptMultilineText that can lengthen text (escaping,
// wrapping) or that strips a token this replacer keeps breaks one of them;
// TestPathInstructionRenderingAgreesWithConfigValidation is the drift check.
func RenderedInstructions(instructions string) string {
	return strings.TrimSpace(promptConflictMarkers.Replace(instructions))
}
func (c *RepoConfig) UnmarshalYAML(value *yaml.Node) error {
	type repoConfigRaw struct {
		Agent                  agentList     `yaml:"agent"`
		Commands               Commands      `yaml:"commands"`
		Hooks                  Hooks         `yaml:"hooks"`
		IgnorePatterns         []string      `yaml:"ignore_patterns"`
		AllowRepoCommands      bool          `yaml:"allow_repo_commands"`
		AutoFix                AutoFixRaw    `yaml:"auto_fix"`
		Commit                 CommitRaw     `yaml:"commit"`
		Intent                 IntentRaw     `yaml:"intent"`
		Refresh                *RefreshRaw   `yaml:"refresh"`
		LegacyRebase           *StepAgentRaw `yaml:"rebase"`
		Review                 ReviewRaw     `yaml:"review"`
		Build                  StepAgentRaw  `yaml:"build"`
		Test                   TestRaw       `yaml:"test"`
		Document               DocumentRaw   `yaml:"document"`
		Lint                   StepAgentRaw  `yaml:"lint"`
		PR                     StepAgentRaw  `yaml:"pr"`
		CI                     CIRaw         `yaml:"ci"`
		Prompts                PromptConfig  `yaml:"prompts"`
		DisableProjectSettings bool          `yaml:"disable_project_settings"`
		NoCI                   bool          `yaml:"no_ci"`
		LegacyEval             any           `yaml:"eval"`
		LegacyRepoBinding      any           `yaml:"repo"`
	}
	var raw repoConfigRaw
	if err := decodeKnownFieldsShallow(value, &raw); err != nil {
		return err
	}
	c.present = repoConfigPresence(value)
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Commands = raw.Commands
	c.Hooks = raw.Hooks
	c.IgnorePatterns = raw.IgnorePatterns
	c.AllowRepoCommands = raw.AllowRepoCommands
	c.AutoFix = raw.AutoFix
	c.CI = raw.CI
	c.Commit = raw.Commit
	c.Intent = raw.Intent
	refresh, err := resolveLegacyRepoRefreshConfig(raw.Refresh, raw.LegacyRebase)
	if err != nil {
		return err
	}
	c.Refresh = refresh
	c.Review = raw.Review
	c.Build = raw.Build
	c.Test = raw.Test
	c.Document = raw.Document
	c.Lint = raw.Lint
	c.PR = raw.PR
	c.Prompts = raw.Prompts
	c.DisableProjectSettings = raw.DisableProjectSettings
	c.NoCI = raw.NoCI
	return nil
}

// OverlayRepoConfig applies only fields explicitly present in override. It is
// used for the global config's machine-local per-repo overrides, where omitted
// fields continue to inherit the already-resolved committed configuration
// while explicit empty values can deliberately clear commands and agent
// routes.
func OverlayRepoConfig(base, override *RepoConfig) *RepoConfig {
	if base == nil {
		base = &RepoConfig{}
	}
	if override == nil {
		return cloneRepoConfig(base)
	}
	out := cloneRepoConfig(base)
	if override.has("agent") {
		out.Agent = override.Agent
		out.Agents = copyAgents(override.Agents)
	}
	if override.has("commands.lint") {
		out.Commands.Lint = override.Commands.Lint
	}
	if override.has("commands.build") {
		out.Commands.Build = override.Commands.Build
	}
	if override.has("commands.test") {
		out.Commands.Test = override.Commands.Test
	}
	if override.has("commands.format") {
		out.Commands.Format = override.Commands.Format
	}
	if override.has("hooks.post_worktree") {
		out.Hooks.PostWorktree = override.Hooks.PostWorktree
	}
	if override.has("hooks.pr_body") {
		out.Hooks.PRBody = override.Hooks.PRBody
	}
	if override.has("ignore_patterns") {
		out.IgnorePatterns = copyStrings(override.IgnorePatterns)
	}
	if override.has("allow_repo_commands") {
		out.AllowRepoCommands = override.AllowRepoCommands
	}
	if override.has("auto_fix.lint") {
		out.AutoFix.Lint = override.AutoFix.Lint
	}
	if override.has("auto_fix.build") {
		out.AutoFix.Build = override.AutoFix.Build
	}
	if override.has("auto_fix.test") {
		out.AutoFix.Test = override.AutoFix.Test
	}
	if override.has("auto_fix.review") {
		out.AutoFix.Review = override.AutoFix.Review
	}
	if override.has("auto_fix.document") {
		out.AutoFix.Document = override.AutoFix.Document
	}
	if override.has("auto_fix.ci", "auto_fix.babysit") {
		out.AutoFix.CI = override.AutoFix.CI
	}
	if override.has("auto_fix.refresh", "auto_fix.rebase") {
		out.AutoFix.Refresh = override.AutoFix.Refresh
	}
	if override.has("commit.fix_message") {
		out.Commit.FixMessage = override.Commit.FixMessage
	}
	if override.has("intent.agent") {
		out.Intent.Agent = override.Intent.Agent
		out.Intent.Agents = copyAgents(override.Intent.Agents)
	}
	if override.has("intent.model") {
		out.Intent.Model = override.Intent.Model
	}
	if override.has("intent.enabled") {
		out.Intent.Enabled = override.Intent.Enabled
	}
	if override.has("intent.threshold") {
		out.Intent.Threshold = override.Intent.Threshold
	}
	if override.has("intent.slack_days") {
		out.Intent.SlackDays = override.Intent.SlackDays
	}
	if override.has("intent.disabled_readers") {
		out.Intent.DisabledReaders = copyStrings(override.Intent.DisabledReaders)
	}
	if override.has("refresh.agent", "rebase.agent") {
		out.Refresh.Agent = override.Refresh.Agent
		out.Refresh.Agents = copyAgents(override.Refresh.Agents)
	}
	if override.has("refresh.model", "rebase.model") {
		out.Refresh.Model = override.Refresh.Model
	}
	if override.has("refresh.strategy") {
		out.Refresh.Strategy = override.Refresh.Strategy
	}
	if override.has("review.agent") {
		out.Review.Agent = override.Review.Agent
		out.Review.Agents = copyAgents(override.Review.Agents)
	}
	if override.has("review.model") {
		out.Review.Model = override.Review.Model
	}
	if override.has("review.adversary_agent") {
		out.Review.AdversaryAgent = override.Review.AdversaryAgent
		out.Review.AdversaryAgents = copyAgents(override.Review.AdversaryAgents)
	}
	if override.has("review.adversary_model") {
		out.Review.AdversaryModel = override.Review.AdversaryModel
	}
	if override.has("review.path_instructions") {
		out.Review.PathInstructions = append([]PathInstruction(nil), override.Review.PathInstructions...)
	}
	if override.has("build.agent") {
		out.Build.Agent = override.Build.Agent
		out.Build.Agents = copyAgents(override.Build.Agents)
	}
	if override.has("build.model") {
		out.Build.Model = override.Build.Model
	}
	if override.has("test.agent") {
		out.Test.Agent = override.Test.Agent
		out.Test.Agents = copyAgents(override.Test.Agents)
	}
	if override.has("test.model") {
		out.Test.Model = override.Test.Model
	}
	if override.has("test.evidence.store_in_repo") {
		out.Test.Evidence.StoreInRepo = override.Test.Evidence.StoreInRepo
	}
	if override.has("test.evidence.dir") {
		out.Test.Evidence.Dir = override.Test.Evidence.Dir
	}
	if override.has("test.evidence.branch") {
		out.Test.Evidence.Branch = override.Test.Evidence.Branch
	}
	if override.has("document.agent") {
		out.Document.Agent = override.Document.Agent
		out.Document.Agents = copyAgents(override.Document.Agents)
	}
	if override.has("document.model") {
		out.Document.Model = override.Document.Model
	}
	if override.has("document.instructions") {
		out.Document.Instructions = override.Document.Instructions
	}
	if override.has("lint.agent") {
		out.Lint.Agent = override.Lint.Agent
		out.Lint.Agents = copyAgents(override.Lint.Agents)
	}
	if override.has("lint.model") {
		out.Lint.Model = override.Lint.Model
	}
	if override.has("pr.agent") {
		out.PR.Agent = override.PR.Agent
		out.PR.Agents = copyAgents(override.PR.Agents)
	}
	if override.has("pr.model") {
		out.PR.Model = override.PR.Model
	}
	if override.has("ci.agent") {
		out.CI.Agent = override.CI.Agent
		out.CI.Agents = copyAgents(override.CI.Agents)
	}
	if override.has("ci.model") {
		out.CI.Model = override.CI.Model
	}
	if override.has("ci.rerun_transient") {
		out.CI.RerunTransient = override.CI.RerunTransient
	}
	if override.has("prompts.shared") {
		out.Prompts.Shared = override.Prompts.Shared
	}
	if override.has("prompts.intent") {
		out.Prompts.Intent = override.Prompts.Intent
	}
	if override.has("prompts.refresh") {
		out.Prompts.Refresh = override.Prompts.Refresh
	}
	if override.has("prompts.review") {
		out.Prompts.Review = override.Prompts.Review
	}
	if override.has("prompts.build") {
		out.Prompts.Build = override.Prompts.Build
	}
	if override.has("prompts.test") {
		out.Prompts.Test = override.Prompts.Test
	}
	if override.has("prompts.document") {
		out.Prompts.Document = override.Prompts.Document
	}
	if override.has("prompts.lint") {
		out.Prompts.Lint = override.Prompts.Lint
	}
	if override.has("prompts.pr") {
		out.Prompts.PR = override.Prompts.PR
	}
	if override.has("prompts.ci") {
		out.Prompts.CI = override.Prompts.CI
	}
	if override.has("disable_project_settings") {
		out.DisableProjectSettings = override.DisableProjectSettings
	}
	if override.has("no_ci") {
		out.NoCI = override.NoCI
	}
	return out
}

func (c *RepoConfig) has(paths ...string) bool {
	for _, path := range paths {
		if c.present[path] {
			return true
		}
	}
	return false
}

// Declares reports whether any of the dotted YAML paths was explicitly
// present in the parsed source, so callers outside this package can mirror
// OverlayRepoConfig's explicit-empty-clears semantics (for example the
// pr-body preview resolving hooks.pr_body from a global override).
func (c *RepoConfig) Declares(paths ...string) bool {
	return c.has(paths...)
}

// NormalizeOverrideKey validates one global-config overrides key and returns
// its normalized (lowercase) form. A key must be exactly `<owner>/<repo>`:
// two non-empty path segments and nothing else - no scheme, host, userinfo,
// whitespace, extra segments, or the clone URL's trailing `.git`, which the
// remote identities this key is matched against always have stripped - so a
// malformed binding fails config load loudly instead of silently never
// matching.
func NormalizeOverrideKey(key string) (string, error) {
	if key != strings.TrimSpace(key) {
		return "", fmt.Errorf("overrides key %q must not have surrounding whitespace", key)
	}
	owner, repo, ok := strings.Cut(key, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", fmt.Errorf("overrides key %q must be exactly <owner>/<repo>", key)
	}
	if strings.HasSuffix(key, ".git") {
		return "", fmt.Errorf("overrides key %q must not end in .git; use the plain <owner>/<repo> identity", key)
	}
	for _, segment := range []string{owner, repo} {
		if segment == "." || segment == ".." {
			return "", fmt.Errorf("overrides key %q must be exactly <owner>/<repo>", key)
		}
		if strings.ContainsAny(segment, ":@\\?#") ||
			strings.IndexFunc(segment, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
			return "", fmt.Errorf("overrides key %q must be a plain <owner>/<repo> identity without URL syntax or whitespace", key)
		}
	}
	return strings.ToLower(key), nil
}

// normalizeGlobalOverrides validates the parsed overrides map: every key must
// normalize via NormalizeOverrideKey without colliding, and every entry must
// be a well-formed RepoConfig overlay. The key is the repository binding, so
// an entry may not also declare the legacy `repo` field.
func normalizeGlobalOverrides(raw map[string]*RepoConfig) (map[string]*RepoConfig, error) {
	overrides := make(map[string]*RepoConfig, len(raw))
	for key, override := range raw {
		normalized, err := NormalizeOverrideKey(key)
		if err != nil {
			return nil, err
		}
		if _, exists := overrides[normalized]; exists {
			return nil, fmt.Errorf("overrides declares %q more than once after case normalization", normalized)
		}
		if override == nil {
			return nil, fmt.Errorf("overrides.%s must be a repo-config mapping", key)
		}
		if override.has("repo") {
			return nil, fmt.Errorf("overrides.%s must not declare repo; the overrides key is the repository binding", key)
		}
		if err := finalizeRepoConfig(override); err != nil {
			return nil, fmt.Errorf("overrides.%s: %w", key, err)
		}
		overrides[normalized] = override
	}
	return overrides, nil
}

// OverrideForRepoIdentity returns the overrides entry matching a normalized
// remote identity of the form `host/owner/repo` (the gate package's
// RemoteIdentity/RegisteredRemoteIdentity output), along with the matched
// key. Overrides keys are host-agnostic `<owner>/<repo>` identities, so the
// match compares the identity's path portion; a nested path (for example a
// GitLab subgroup) can never match a two-segment key.
func (c *GlobalConfig) OverrideForRepoIdentity(identity string) (*RepoConfig, string, bool) {
	if len(c.Overrides) == 0 {
		return nil, "", false
	}
	_, path, ok := strings.Cut(identity, "/")
	if !ok || path == "" {
		return nil, "", false
	}
	override, ok := c.Overrides[strings.ToLower(path)]
	if !ok {
		return nil, "", false
	}
	return override, strings.ToLower(path), true
}

func cloneRepoConfig(src *RepoConfig) *RepoConfig {
	out := *src
	out.Agents = copyAgents(src.Agents)
	out.IgnorePatterns = copyStrings(src.IgnorePatterns)
	out.Intent.Agents = copyAgents(src.Intent.Agents)
	out.Intent.DisabledReaders = copyStrings(src.Intent.DisabledReaders)
	out.Refresh.Agents = copyAgents(src.Refresh.Agents)
	out.Review = copyReviewRaw(src.Review)
	out.Build = copyStepAgentRaw(src.Build)
	out.Test.Agents = copyAgents(src.Test.Agents)
	out.Document.Agents = copyAgents(src.Document.Agents)
	out.Lint = copyStepAgentRaw(src.Lint)
	out.PR = copyStepAgentRaw(src.PR)
	out.CI = CIRaw{
		StepAgentRaw:   copyStepAgentRaw(src.CI.StepAgentRaw),
		RerunTransient: src.CI.RerunTransient,
	}
	if src.present != nil {
		out.present = make(map[string]bool, len(src.present))
		for path, present := range src.present {
			out.present[path] = present
		}
	}
	return &out
}

func copyStrings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

func repoConfigPresence(value *yaml.Node) map[string]bool {
	present := make(map[string]bool)
	collectRepoConfigPresence(value, "", present)
	return present
}

func collectRepoConfigPresence(value *yaml.Node, prefix string, present map[string]bool) {
	if value == nil {
		return
	}
	if value.Kind == yaml.AliasNode {
		collectRepoConfigPresence(value.Alias, prefix, present)
		return
	}
	if value.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		key := value.Content[i].Value
		if key == "<<" {
			merged := value.Content[i+1]
			if merged.Kind == yaml.SequenceNode {
				for _, item := range merged.Content {
					collectRepoConfigPresence(item, prefix, present)
				}
			} else {
				collectRepoConfigPresence(merged, prefix, present)
			}
			continue
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		present[path] = true
		collectRepoConfigPresence(value.Content[i+1], path, present)
	}
}

// Commands holds optional per-repo command overrides.
type Commands struct {
	Lint   string `yaml:"lint"`
	Build  string `yaml:"build"`
	Test   string `yaml:"test"`
	Format string `yaml:"format"`
}

// Hooks holds deterministic controller commands that run outside pipeline
// step execution. Like Commands, hook values are trusted code-executing
// configuration and are never sourced from an untrusted pushed branch.
type Hooks struct {
	PostWorktree string `yaml:"post_worktree"`
	// PRBody is an external PR body formatter. It receives the prbody
	// contract on stdin and returns the finished body on stdout. A non-zero
	// exit falls back to the built-in body and is reported, so a broken
	// formatter never blocks shipping.
	PRBody string `yaml:"pr_body"`
}

// AutoFixRaw is the YAML representation of auto-fix config.
// Pointer fields distinguish "not set" (nil) from "set to 0" (disabled).
type AutoFixRaw struct {
	Lint     *int `yaml:"lint"`
	Build    *int `yaml:"build"`
	Test     *int `yaml:"test"`
	Review   *int `yaml:"review"`
	Document *int `yaml:"document"`
	CI       *int `yaml:"ci"`
	Babysit  *int `yaml:"babysit"`
	Refresh  *int `yaml:"refresh"`
}

func (c AutoFixRaw) MarshalYAML() (any, error) {
	return struct {
		Lint     *int `yaml:"lint,omitempty"`
		Build    *int `yaml:"build,omitempty"`
		Test     *int `yaml:"test,omitempty"`
		Review   *int `yaml:"review,omitempty"`
		Document *int `yaml:"document,omitempty"`
		CI       *int `yaml:"ci,omitempty"`
		Refresh  *int `yaml:"refresh,omitempty"`
	}{
		Lint:     c.Lint,
		Build:    c.Build,
		Test:     c.Test,
		Review:   c.Review,
		Document: c.Document,
		CI:       c.CI,
		Refresh:  c.Refresh,
	}, nil
}

func (c *AutoFixRaw) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Lint         *int `yaml:"lint"`
		Build        *int `yaml:"build"`
		Test         *int `yaml:"test"`
		Review       *int `yaml:"review"`
		Document     *int `yaml:"document"`
		CI           *int `yaml:"ci"`
		Babysit      *int `yaml:"babysit"`
		Refresh      *int `yaml:"refresh"`
		LegacyRebase *int `yaml:"rebase"`
	}
	if err := decodeKnownFields(value, &raw); err != nil {
		return err
	}
	if raw.Refresh != nil && raw.LegacyRebase != nil {
		return fmt.Errorf("auto_fix.refresh and legacy auto_fix.rebase cannot both be set")
	}
	c.Lint = raw.Lint
	c.Build = raw.Build
	c.Test = raw.Test
	c.Review = raw.Review
	c.Document = raw.Document
	c.CI = raw.CI
	c.Babysit = raw.Babysit
	c.Refresh = raw.Refresh
	if c.Refresh == nil {
		c.Refresh = raw.LegacyRebase
	}
	return nil
}

// CIRaw is the YAML representation of CI-step settings.
// Pointer fields distinguish "not set" (nil) from "set to 0" (disabled).
type CIRaw struct {
	StepAgentRaw
	RerunTransient *int `yaml:"rerun_transient"`
}

func (c CIRaw) MarshalYAML() (any, error) {
	return struct {
		Agent          agentList  `yaml:"agent,omitempty"`
		Model          ModelRoute `yaml:"model,omitempty"`
		RerunTransient *int       `yaml:"rerun_transient,omitempty"`
	}{
		Agent:          agentList(stepAgentNames(c.Agent, c.Agents)),
		Model:          c.Model,
		RerunTransient: c.RerunTransient,
	}, nil
}

func (c *CIRaw) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Agent          agentList  `yaml:"agent"`
		Model          ModelRoute `yaml:"model"`
		RerunTransient *int       `yaml:"rerun_transient"`
	}
	if err := decodeKnownFields(value, &raw); err != nil {
		return err
	}
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Model = raw.Model
	c.RerunTransient = raw.RerunTransient
	return nil
}

// CI holds the resolved CI-step settings.
type CI struct {
	// RerunTransient is how many times the CI step may re-run a single check
	// the provider reported as cancelled - the one terminal outcome it
	// attributes to itself rather than to the job - before that check reaches
	// an approval gate. 0 disables reruns and restores the behavior of
	// escalating every failure on sight.
	RerunTransient int
}

// AutoFix holds resolved per-step auto-fix attempt limits.
// A value of 0 means auto-fix is disabled (requires manual approval).
type AutoFix struct {
	Lint     int
	Build    int
	Test     int
	Review   int
	Document int
	CI       int
	Refresh  int
}

// Config is the merged result of global + per-repo configuration.
type Config struct {
	ReplayConfigJSON        []byte
	TrustedConfigSHA        string
	CaptureEvalProvenance   bool
	Agent                   types.AgentName
	Agents                  []types.AgentName
	StepAgents              map[types.StepName][]types.AgentName
	StepModels              map[types.StepName]ModelRoute
	ReviewAdversaryAgents   []types.AgentName
	ReviewAdversaryModel    ModelRoute
	ACPXPath                string
	ACPRegistryOverrides    map[string]string
	AgentPathOverride       map[string]string
	AgentArgsOverride       map[string][]string
	CITimeout               time.Duration
	StepQuietWarning        time.Duration
	ProcessTerminationGrace time.Duration
	LogLevel                string
	SessionReuse            bool
	Commands                Commands
	Hooks                   Hooks
	IgnorePatterns          []string
	AutoFix                 AutoFix
	CI                      CI
	Commit                  Commit
	Intent                  Intent
	Test                    Test
	Document                Document
	Review                  Review
	Eval                    Eval
	Prompts                 PromptConfig
	RefreshStrategy         types.RefreshStrategy
	// DisableProjectSettings is the resolved, trusted-only opt-out (see the
	// RepoConfig field). When true, gate agents are launched with their
	// project-level settings/instructions suppressed; the daemon fails the run
	// closed if the resolved harness has no verified suppression knob.
	DisableProjectSettings bool
	// NoCI is the resolved, trusted-only declaration that this repository
	// intentionally has no CI (see the RepoConfig field). When true and the
	// forge reports zero checks, the CI monitor treats that as all-checks-passed.
	NoCI bool
}

// Document is the resolved document-step config. Instructions come from the
// trusted default-branch repo config and augment the built-in placement
// policy in the document prompt.
type Document struct {
	Instructions string
}

// PromptConfig holds optional prompt additions. Built-in prompts remain the
// source of structure, safety rules, and schemas; these values are appended as
// extra steering only. Push never prompts an agent, so it has no key.
type PromptConfig struct {
	Shared   string `yaml:"shared"`
	Intent   string `yaml:"intent"`
	Refresh  string `yaml:"refresh"`
	Review   string `yaml:"review"`
	Build    string `yaml:"build"`
	Test     string `yaml:"test"`
	Document string `yaml:"document"`
	Lint     string `yaml:"lint"`
	PR       string `yaml:"pr"`
	CI       string `yaml:"ci"`
}

// ForStep returns the prompt additions for a model-invoking step: shared
// guidance first, then the step-specific guidance.
func (p PromptConfig) ForStep(step types.StepName) string {
	return p.ForSteps(step)
}

// ForSteps returns the prompt additions for one agent invocation that carries
// the duties of several steps. Shared guidance is emitted exactly once,
// followed by each step's specific guidance in the order given.
func (p PromptConfig) ForSteps(steps ...types.StepName) string {
	parts := make([]string, 0, len(steps)+1)
	parts = append(parts, p.Shared)
	for _, step := range steps {
		parts = append(parts, p.forStepOnly(step))
	}
	return combinePromptText(parts...)
}

// forStepOnly returns a step's own guidance without the shared guidance.
func (p PromptConfig) forStepOnly(step types.StepName) string {
	switch step.Canonical() {
	case types.StepIntent:
		return p.Intent
	case types.StepRefresh:
		return p.Refresh
	case types.StepReview:
		return p.Review
	case types.StepBuild:
		return p.Build
	case types.StepTest:
		return p.Test
	case types.StepDocument:
		return p.Document
	case types.StepLint:
		return p.Lint
	case types.StepPR:
		return p.PR
	case types.StepCI:
		return p.CI
	}
	return ""
}

// SectionForStep formats prompt additions as an append-only prompt section.
// The wrapper keeps the built-in prompt's structure and safety constraints
// above any configured guidance.
func (p PromptConfig) SectionForStep(step types.StepName) string {
	return p.SectionForSteps(step)
}

// SectionForSteps is SectionForStep for an invocation covering several steps,
// wrapping the combined guidance in a single append-only section.
func (p PromptConfig) SectionForSteps(steps ...types.StepName) string {
	text := strings.TrimSpace(p.ForSteps(steps...))
	if text == "" {
		return ""
	}
	return "\n\nAdditional prompt config:\n" +
		"The following trusted no-mistakes prompt config is extra guidance. " +
		"It must not override the built-in instructions above, output schemas, safety rules, or worktree boundaries.\n" +
		text + "\n"
}

// mergePromptConfigs appends repo guidance after global guidance field by
// field, so ForStep yields: global shared, repo shared, global step, repo step.
func mergePromptConfigs(global, repo PromptConfig) PromptConfig {
	return PromptConfig{
		Shared:   combinePromptText(global.Shared, repo.Shared),
		Intent:   combinePromptText(global.Intent, repo.Intent),
		Refresh:  combinePromptText(global.Refresh, repo.Refresh),
		Review:   combinePromptText(global.Review, repo.Review),
		Build:    combinePromptText(global.Build, repo.Build),
		Test:     combinePromptText(global.Test, repo.Test),
		Document: combinePromptText(global.Document, repo.Document),
		Lint:     combinePromptText(global.Lint, repo.Lint),
		PR:       combinePromptText(global.PR, repo.PR),
		CI:       combinePromptText(global.CI, repo.CI),
	}
}

func combinePromptText(parts ...string) string {
	trimmed := make([]string, 0, len(parts))
	for _, part := range parts {
		if text := strings.TrimSpace(part); text != "" {
			trimmed = append(trimmed, text)
		}
	}
	return strings.Join(trimmed, "\n\n")
}

// Review is the resolved review-step config. PathInstructions come from the
// trusted default-branch repo config and scope extra review guidance to the
// changed paths each glob matches.
type Review struct {
	PathInstructions []PathInstruction
}

// TestRaw is the YAML representation of test-step settings.
type TestRaw struct {
	Agent    types.AgentName   `yaml:"-"`
	Agents   []types.AgentName `yaml:"-"`
	Model    ModelRoute        `yaml:"model"`
	Evidence EvidenceRaw       `yaml:"evidence"`
}

func (c TestRaw) MarshalYAML() (any, error) {
	return struct {
		Agent    agentList   `yaml:"agent,omitempty"`
		Model    ModelRoute  `yaml:"model,omitempty"`
		Evidence EvidenceRaw `yaml:"evidence,omitempty"`
	}{
		Agent:    agentList(stepAgentNames(c.Agent, c.Agents)),
		Model:    c.Model,
		Evidence: c.Evidence,
	}, nil
}

func (c *TestRaw) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Agent    agentList   `yaml:"agent"`
		Model    ModelRoute  `yaml:"model"`
		Evidence EvidenceRaw `yaml:"evidence"`
	}
	if err := decodeKnownFields(value, &raw); err != nil {
		return err
	}
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Model = raw.Model
	c.Evidence = raw.Evidence
	return nil
}

// EvidenceRaw is the YAML representation of test-evidence settings.
// Pointer fields distinguish "not set" (nil) from explicit zero/false values.
type EvidenceRaw struct {
	StoreInRepo *bool   `yaml:"store_in_repo"`
	Dir         *string `yaml:"dir"`
	// Branch selects the orphan evidence branch. It names a git ref the
	// daemon pushes to with the maintainer's credentials, so it is honored
	// ONLY from the trusted default-branch copy of .no-mistakes.yaml (see
	// EffectiveRepoConfig): a contributor's pushed branch must not be able to
	// aim evidence commits at another branch of the repository.
	Branch *string `yaml:"branch"`
	// LocalRoot, Retention, and MaxRuns describe this MACHINE's evidence
	// storage: where the daemon writes artifacts on local disk and how long it
	// keeps them. They are global-only - Merge resolves them straight from
	// GlobalConfig and never from a repository, trusted copy included. A
	// repository does not get to name a filesystem path the daemon writes to,
	// nor to set the retention budget for a resource every other repository on
	// the machine shares. (Contrast Branch, which is trusted-repo-settable
	// because a branch genuinely is per-repository state.)
	//
	// LocalRoot must be absolute; see validateTestRaw.
	LocalRoot *string `yaml:"local_root"`
	Retention *string `yaml:"retention"`
	MaxRuns   *int    `yaml:"max_runs"`
}

// Test is the resolved test-step config.
type Test struct {
	Evidence Evidence
}

// Evidence is the resolved test-evidence config. When StoreInRepo is true, the
// run publishes its evidence artifacts to the orphan Branch of the same
// repository, under Dir, and links them from the pull request body. Evidence
// never enters the pushed code branch, so it never reaches the default
// branch's history. Otherwise evidence stays on local disk under LocalRoot,
// referenced only by local path.
type Evidence struct {
	StoreInRepo bool
	Dir         string
	Branch      string
	// LocalRoot overrides the app-root default for on-disk evidence; empty
	// means paths.EvidenceDir(). Retention and MaxRuns bound how much of it
	// survives: no-mistakes reaps its own evidence rather than leaving that to
	// an OS temp-directory timer. Zero disables the corresponding bound.
	LocalRoot string
	Retention time.Duration
	MaxRuns   int
}

// EvalRaw is the YAML representation of local evaluation-corpus settings.
// Pointer fields distinguish "not set" (nil) from explicit zero/false values.
type EvalRaw struct {
	CaptureProvenance *bool `yaml:"capture_provenance"`
	AutoCapture       *bool `yaml:"auto_capture"`
	MaxCases          *int  `yaml:"max_cases"`
	DiversifiedSize   *int  `yaml:"diversified_size"`
}

// Eval is the resolved local evaluation-corpus config. It is deliberately a
// first-class configuration key rather than an environment variable: the
// daemon is a long-lived launchd/systemd service whose unit file is re-rendered
// on install and update, and only proxy variables survive that re-render, so an
// environment-gated corpus would silently stop collecting after an update.
//
// CaptureProvenance is the upstream half: it makes every review round record
// the exact commit and configuration inputs a replay needs. A round written
// with it off can never be captured afterwards, because the pinned global
// configuration is a point-in-time snapshot that no longer exists anywhere.
//
// AutoCapture is the downstream half: it freezes each finished run's review
// passes into the local corpus without anyone running a command. It has no
// effect while CaptureProvenance is off, since there is nothing to freeze.
type Eval struct {
	CaptureProvenance bool
	AutoCapture       bool
	// MaxCases caps the auto-captured corpus. 0 keeps every case. Pruning is
	// oldest-first and never removes a case that already has recorded
	// candidate replays, so a corpus you have spent tokens on is never
	// silently reclaimed underneath a comparison.
	MaxCases int
	// DiversifiedSize caps the official gold-only eval set. 0 means one gold
	// case per stratum (no Hamilton bound). Unlabeled cases never fill it.
	DiversifiedSize int
}

// IntentRaw is the YAML representation of user-intent extraction settings.
// Pointer fields distinguish "not set" (nil) from explicit zero/false values.
type IntentRaw struct {
	Agent           types.AgentName   `yaml:"-"`
	Agents          []types.AgentName `yaml:"-"`
	Model           ModelRoute        `yaml:"model"`
	Enabled         *bool             `yaml:"enabled"`
	Threshold       *float64          `yaml:"threshold"`
	SlackDays       *int              `yaml:"slack_days"`
	DisabledReaders []string          `yaml:"disabled_readers"`
}

func (c IntentRaw) MarshalYAML() (any, error) {
	return struct {
		Agent           agentList  `yaml:"agent,omitempty"`
		Model           ModelRoute `yaml:"model,omitempty"`
		Enabled         *bool      `yaml:"enabled,omitempty"`
		Threshold       *float64   `yaml:"threshold,omitempty"`
		SlackDays       *int       `yaml:"slack_days,omitempty"`
		DisabledReaders []string   `yaml:"disabled_readers,omitempty"`
	}{
		Agent:           agentList(stepAgentNames(c.Agent, c.Agents)),
		Model:           c.Model,
		Enabled:         c.Enabled,
		Threshold:       c.Threshold,
		SlackDays:       c.SlackDays,
		DisabledReaders: c.DisabledReaders,
	}, nil
}

func (c *IntentRaw) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Agent           agentList  `yaml:"agent"`
		Model           ModelRoute `yaml:"model"`
		Enabled         *bool      `yaml:"enabled"`
		Threshold       *float64   `yaml:"threshold"`
		SlackDays       *int       `yaml:"slack_days"`
		DisabledReaders []string   `yaml:"disabled_readers"`
	}
	if err := decodeKnownFields(value, &raw); err != nil {
		return err
	}
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Model = raw.Model
	c.Enabled = raw.Enabled
	c.Threshold = raw.Threshold
	c.SlackDays = raw.SlackDays
	c.DisabledReaders = raw.DisabledReaders
	return nil
}

// Intent is the resolved user-intent extraction config.
type Intent struct {
	Enabled         bool
	Threshold       float64
	SlackDays       int
	DisabledReaders map[string]bool
}

type agentList []types.AgentName

// decodeKnownFields preserves yaml.Decoder.KnownFields semantics inside
// custom UnmarshalYAML methods. yaml.Node.Decode does not inherit the parent
// decoder's strictness, so validate the node recursively against the concrete
// decode target before decoding it.
func decodeKnownFields(value *yaml.Node, out any) error {
	if err := validateKnownFields(value, reflect.TypeOf(out)); err != nil {
		return err
	}
	return value.Decode(out)
}

// decodeKnownFieldsShallow validates one custom-unmarshal boundary while
// leaving nested custom types to validate their own accepted shapes. A parent
// decoder cannot infer the private raw structs used by nested UnmarshalYAML
// methods, so recursively reflecting over the public normalized structs would
// reject valid keys such as step.agent and legacy aliases.
func decodeKnownFieldsShallow(value *yaml.Node, out any) error {
	if err := validateKnownFieldsShallow(value, reflect.TypeOf(out), make(map[*yaml.Node]bool)); err != nil {
		return err
	}
	return value.Decode(out)
}

func validateKnownFieldsShallow(value *yaml.Node, target reflect.Type, stack map[*yaml.Node]bool) error {
	if value == nil || target == nil {
		return nil
	}
	if stack[value] {
		return fmt.Errorf("YAML alias cycle while validating %s", target)
	}
	stack[value] = true
	defer delete(stack, value)
	if value.Kind == yaml.AliasNode {
		return validateKnownFieldsShallow(value.Alias, target, stack)
	}
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target.Kind() != reflect.Struct || value.Kind != yaml.MappingNode {
		return nil
	}

	fields := make(map[string]bool, target.NumField())
	for i := 0; i < target.NumField(); i++ {
		field := target.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		fields[name] = true
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		key := value.Content[i].Value
		if key == "<<" {
			merged := value.Content[i+1]
			if merged.Kind == yaml.SequenceNode {
				for _, item := range merged.Content {
					if err := validateKnownFieldsShallow(item, target, stack); err != nil {
						return err
					}
				}
			} else if err := validateKnownFieldsShallow(merged, target, stack); err != nil {
				return err
			}
			continue
		}
		if !fields[key] {
			return fmt.Errorf("field %s not found in type %s", key, target)
		}
	}
	return nil
}

func validateKnownFields(value *yaml.Node, target reflect.Type) error {
	return validateKnownFieldsStack(value, target, make(map[*yaml.Node]bool))
}

func validateKnownFieldsStack(value *yaml.Node, target reflect.Type, stack map[*yaml.Node]bool) error {
	if value == nil || target == nil {
		return nil
	}
	if stack[value] {
		return fmt.Errorf("YAML alias cycle while validating %s", target)
	}
	stack[value] = true
	defer delete(stack, value)
	if value.Kind == yaml.AliasNode {
		return validateKnownFieldsStack(value.Alias, target, stack)
	}
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target.Kind() == reflect.Slice && value.Kind == yaml.SequenceNode {
		for _, item := range value.Content {
			if err := validateKnownFieldsStack(item, target.Elem(), stack); err != nil {
				return err
			}
		}
		return nil
	}
	if target.Kind() != reflect.Struct || value.Kind != yaml.MappingNode {
		return nil
	}

	fields := make(map[string]reflect.Type, target.NumField())
	for i := 0; i < target.NumField(); i++ {
		field := target.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = strings.ToLower(field.Name)
		}
		fields[name] = field.Type
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		key := value.Content[i].Value
		if key == "<<" {
			merged := value.Content[i+1]
			if merged.Kind == yaml.SequenceNode {
				for _, item := range merged.Content {
					if err := validateKnownFieldsStack(item, target, stack); err != nil {
						return err
					}
				}
			} else if err := validateKnownFieldsStack(merged, target, stack); err != nil {
				return err
			}
			continue
		}
		fieldType, ok := fields[key]
		if !ok {
			return fmt.Errorf("field %s not found in type %s", key, target)
		}
		if err := validateKnownFieldsStack(value.Content[i+1], fieldType, stack); err != nil {
			return err
		}
	}
	return nil
}

func (a *agentList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		name := strings.TrimSpace(value.Value)
		if name == "" {
			*a = nil
			return nil
		}
		*a = []types.AgentName{types.AgentName(name)}
		return nil
	case yaml.SequenceNode:
		names := make([]types.AgentName, 0, len(value.Content))
		for i, item := range value.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("agent[%d] must be a string", i)
			}
			name := strings.TrimSpace(item.Value)
			if name == "" {
				return fmt.Errorf("agent[%d] must not be empty", i)
			}
			names = append(names, types.AgentName(name))
		}
		*a = names
		return nil
	default:
		return fmt.Errorf("agent must be a string or a list of strings")
	}
}

func firstAgent(names []types.AgentName) types.AgentName {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func copyAgents(names []types.AgentName) []types.AgentName {
	if len(names) == 0 {
		return nil
	}
	out := make([]types.AgentName, len(names))
	copy(out, names)
	return out
}

func stepAgentNames(agentName types.AgentName, agents []types.AgentName) []types.AgentName {
	if len(agents) > 0 {
		return copyAgents(agents)
	}
	if agentName != "" {
		return []types.AgentName{agentName}
	}
	return nil
}

func addStepAgentRoute(routes map[types.StepName][]types.AgentName, step types.StepName, agentName types.AgentName, agents []types.AgentName) {
	if names := stepAgentNames(agentName, agents); len(names) > 0 {
		routes[step] = names
	}
}

func addStepModelRoute(routes map[types.StepName]ModelRoute, step types.StepName, model ModelRoute) {
	if model.Name != "" {
		routes[step] = model
	}
}

// ConfiguredStepAgents returns the repo's explicitly configured per-step
// routes. Unconfigured steps are omitted and inherit the run-wide route.
func (c *RepoConfig) ConfiguredStepAgents() map[types.StepName][]types.AgentName {
	routes := make(map[types.StepName][]types.AgentName)
	addStepAgentRoute(routes, types.StepIntent, c.Intent.Agent, c.Intent.Agents)
	addStepAgentRoute(routes, types.StepRefresh, c.Refresh.Agent, c.Refresh.Agents)
	addStepAgentRoute(routes, types.StepReview, c.Review.Agent, c.Review.Agents)
	addStepAgentRoute(routes, types.StepBuild, c.Build.Agent, c.Build.Agents)
	addStepAgentRoute(routes, types.StepTest, c.Test.Agent, c.Test.Agents)
	addStepAgentRoute(routes, types.StepDocument, c.Document.Agent, c.Document.Agents)
	addStepAgentRoute(routes, types.StepLint, c.Lint.Agent, c.Lint.Agents)
	addStepAgentRoute(routes, types.StepPR, c.PR.Agent, c.PR.Agents)
	addStepAgentRoute(routes, types.StepCI, c.CI.Agent, c.CI.Agents)
	return routes
}

// ConfiguredStepModels returns the repo's explicitly configured per-step
// model identities. Unconfigured steps inherit the selected agent's default
// model and are omitted.
func (c *RepoConfig) ConfiguredStepModels() map[types.StepName]ModelRoute {
	routes := make(map[types.StepName]ModelRoute)
	addStepModelRoute(routes, types.StepIntent, c.Intent.Model)
	addStepModelRoute(routes, types.StepRefresh, c.Refresh.Model)
	addStepModelRoute(routes, types.StepReview, c.Review.Model)
	addStepModelRoute(routes, types.StepBuild, c.Build.Model)
	addStepModelRoute(routes, types.StepTest, c.Test.Model)
	addStepModelRoute(routes, types.StepDocument, c.Document.Model)
	addStepModelRoute(routes, types.StepLint, c.Lint.Model)
	addStepModelRoute(routes, types.StepPR, c.PR.Model)
	addStepModelRoute(routes, types.StepCI, c.CI.Model)
	return routes
}

func (c *GlobalConfig) configuredStepAgents() map[types.StepName][]types.AgentName {
	routes := make(map[types.StepName][]types.AgentName)
	addStepAgentRoute(routes, types.StepIntent, c.Intent.Agent, c.Intent.Agents)
	addStepAgentRoute(routes, types.StepRefresh, c.Refresh.Agent, c.Refresh.Agents)
	addStepAgentRoute(routes, types.StepReview, c.Review.Agent, c.Review.Agents)
	addStepAgentRoute(routes, types.StepBuild, c.Build.Agent, c.Build.Agents)
	addStepAgentRoute(routes, types.StepTest, c.Test.Agent, c.Test.Agents)
	addStepAgentRoute(routes, types.StepDocument, c.Document.Agent, c.Document.Agents)
	addStepAgentRoute(routes, types.StepLint, c.Lint.Agent, c.Lint.Agents)
	addStepAgentRoute(routes, types.StepPR, c.PR.Agent, c.PR.Agents)
	addStepAgentRoute(routes, types.StepCI, c.CI.Agent, c.CI.Agents)
	return routes
}

func (c *GlobalConfig) configuredStepModels() map[types.StepName]ModelRoute {
	routes := make(map[types.StepName]ModelRoute)
	addStepModelRoute(routes, types.StepIntent, c.Intent.Model)
	addStepModelRoute(routes, types.StepRefresh, c.Refresh.Model)
	addStepModelRoute(routes, types.StepReview, c.Review.Model)
	addStepModelRoute(routes, types.StepBuild, c.Build.Model)
	addStepModelRoute(routes, types.StepTest, c.Test.Model)
	addStepModelRoute(routes, types.StepDocument, c.Document.Model)
	addStepModelRoute(routes, types.StepLint, c.Lint.Model)
	addStepModelRoute(routes, types.StepPR, c.PR.Model)
	addStepModelRoute(routes, types.StepCI, c.CI.Model)
	return routes
}

// ConfiguredModelForStep returns a step's explicit model identity. The empty
// route means the selected backend should use its own default model.
func (c *Config) ConfiguredModelForStep(step types.StepName) ModelRoute {
	return c.StepModels[step]
}

// ConfiguredAgentsForStep returns a step's explicit route or the run-wide
// route when the step is unconfigured.
func (c *Config) ConfiguredAgentsForStep(step types.StepName) []types.AgentName {
	if names := c.StepAgents[step]; len(names) > 0 {
		return copyAgents(names)
	}
	return c.configuredAgents()
}

// resolvePathInstructions trims every entry and drops the ones left without a
// path or without instruction text that survives prompt rendering, so the
// resolved config never carries an entry the review step would have to skip.
// Parsing already rejects those, but Merge also runs on configs built in code.
func resolvePathInstructions(entries []PathInstruction) []PathInstruction {
	if len(entries) == 0 {
		return nil
	}
	out := make([]PathInstruction, 0, len(entries))
	for _, entry := range entries {
		trimmed := PathInstruction{
			Path:         strings.TrimSpace(entry.Path),
			Instructions: strings.TrimSpace(entry.Instructions),
		}
		if trimmed.Path == "" || RenderedInstructions(trimmed.Instructions) == "" {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// defaultConfigYAML is the template written when no global config file exists.
const defaultConfigYAML = `# no-mistakes global configuration

# Agent to use for code generation. This may also be an ordered fallback list,
# for example: agent: [codex, claude]
# Options: auto, claude, codex, rovodev, opencode, pi, copilot, cursor, acp:<target>
# "auto" detects the first available native agent, then registered ACP fallbacks
# "cursor" is the native cursor-agent print-mode backend
# "acp:cursor" keeps the registered cursor-agent acp fallback through acpx
# Use acp:<target> to run an optional user-installed acpx target, for example acp:gemini
agent: auto

# Optional per-step routes. Each agent accepts the same scalar or ordered
# fallback-list form as the run-wide agent. A model is a typed name plus an
# explicit lowercase vendor; adapters translate it through their verified
# interface. OpenCode names use provider/model. Rovo Dev rejects model routes.
# ACP accepts bare model families but rejects bracketed parameter variants,
# which ACP servers may silently normalize.
# Unconfigured steps inherit the run-wide agent and its default model.
# Supported sections: intent, refresh, review, build, test, document, lint, pr, ci.
# review:
#   agent: claude
#   model: {name: claude-opus-5, vendor: anthropic}
#   adversary_agent: codex
#   adversary_model: {name: gpt-5.6-sol, vendor: openai}

# Optional path to the user-installed acpx binary for acp:<target> agents
# acpx_path: acpx

# Optional ACP target command overrides for acp:<target> agents
# acp_registry_overrides:
#   local-gemini: node /opt/mock-acp-agent.mjs
#   cursor: cursor-agent acp

# Maximum time the CI monitor babysits an open PR with no base-branch movement
# before giving up. The monitor watches CI and auto-rebases when the base branch
# advances; each base advance re-arms this timer, so an actively-updated green PR
# keeps its monitor. Set to "unlimited", "none", "off", "never", or any
# non-positive duration to monitor until the PR is merged, closed, or the run is
# aborted with: no-mistakes axi abort --run <id>
ci_timeout: "168h"

# AXI status marks a running/fixing step as quiet when no step log or native
# agent lifecycle activity has appeared for this long. This is observability
# only; it never cancels work.
step_quiet_warning: "10m"

# Maximum time a CLI client waits for an existing daemon socket to accept a
# connection before failing instead of hanging.
daemon_connect_timeout: "3s"

# Maximum time a Unix process group gets to exit after SIGTERM before cleanup
# escalates to SIGKILL. Processes that exit promptly do not wait out the window.
process_termination_grace: "10s"

# Reuse one durable fixer session per run across review-fix turns. Review turns
# always run session-free so a rereview never resumes the session that prescribed
# its fixes. Supported for claude, codex, and cursor; other agents run cold. Set false to
# force every agent invocation cold.
session_reuse: true

# Log level for daemon output
# Options: debug, info, warn, error
log_level: info

# Override native agent binary paths (optional)
# agent_path_override:
#   claude: /usr/local/bin/claude
#   codex: /opt/codex

# Extra agent CLI flags (optional, global only). ACP targets pass
# these flags into a composable raw target command; arbitrary registry targets
# need an acp_registry_overrides entry so no arguments are silently discarded.
# Codex service_tier controls speed/priority; model_reasoning_effort controls reasoning depth.
# agent_args_override:
#   codex:
#     - -m
#     - gpt-5.4
#     - -c
#     - service_tier="priority"
#     - -c
#     - model_reasoning_effort="low"
#
# Maximum follow-up auto-fix attempts per step (0 = disabled after the initial pass)
# Document fixes are attempted during the initial document pass.
auto_fix:
  refresh: 3
  lint: 3
  build: 3
  test: 3
  review: 0
  document: 3
  ci: 3

# How many times the CI step may re-run a single check the provider reported as
# cancelled before that check reaches an approval gate instead of the fix agent.
# Defaults to 0: a cancelled conclusion does not identify who cancelled, so a
# rerun can restart a job a maintainer or a concurrency rule stopped on purpose.
# Raise this only for repositories whose cancellations are known to be
# provider-side. Each rerun is another workflow run billed to the repository
# being contributed to. A repository that sets ci.rerun_transient on its own
# default branch overrides this value.
ci:
  rerun_transient: 0

# Auto-fix commit subject template. Available variables: {{.Step}} and {{.Summary}}.
# Repo config may override this value.
# commit:
#   fix_message: "no-mistakes({{.Step}}): {{.Summary}}"

# User-intent extraction. When you push a branch, no-mistakes can read recent
# transcripts from your local agent (Claude Code, Codex, OpenCode, Rovo Dev, Pi,
# Copilot CLI), pick the session that produced the change, summarize the user
# intent, and feed it to review, build, test, document, lint, and PR agents so they
# understand what you were trying to do - not just the diff.
intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  # disabled_readers: [codex]

# Test-step evidence artifacts (screenshots, recordings, logs the test step
# gathers to demonstrate the change works). By default they are kept on local
# disk under <NM_HOME>/evidence and referenced by local path. Opt in to
# store_in_repo to publish them to an orphan evidence branch in the same
# repository and link them from the PR body. The evidence branch shares no
# history with your code branches, so artifacts never enter the pushed branch or
# the default branch.
#
# no-mistakes reaps its own evidence rather than leaving that to an OS temp
# directory timer: retention ages run directories out (default 14 days) and
# max_runs caps how many survive regardless of age (default 200). Set retention
# to "unlimited", or either to 0, to disable that bound. local_root moves the
# directory to another disk and must be an absolute path. These three are
# global-only - a repository's .no-mistakes.yaml cannot change where this
# machine writes evidence or how long it keeps it.
# test:
#   evidence:
#     store_in_repo: true
#     dir: .no-mistakes/evidence
#     branch: no-mistakes/evidence
#     local_root: /var/lib/no-mistakes/evidence
#     retention: 720h
#     max_runs: 50

# Optional prompt additions. Built-in prompts remain authoritative; these are
# appended as extra guidance. Shared guidance is included in every pipeline
# model prompt, then the step-specific guidance is appended after it.
# Supported keys: shared, intent, refresh, review, build, test, document,
# lint, pr, ci.
# prompts:
#   shared: |
#     Always included in model prompts.
#   review: |
#     Review-specific additions.

# Machine-local per-repository overrides, keyed by the repository's
# <owner>/<repo> identity. Each entry uses the repo-config shape and overlays
# the effective committed config after the default-branch trust rules,
# including code-executing fields; explicitly present empty values clear
# committed values. Repositories without a matching key are unaffected.
# overrides:
#   example/project:
#     commands:
#       build: "make build"
#     prompts:
#       test: |
#         Repo-specific testing guidance.

# Local review evaluation corpus, used by "no-mistakes eval" to compare
# agent+model candidates against review passes your own pipeline already made.
# capture_provenance records, on every review round, the exact commits and
# configuration a replay needs; it cannot be added afterwards, so a round
# recorded without it is never replayable. auto_capture freezes each finished
# run's review passes into the corpus so it fills without anyone remembering to
# collect it. Cases of the same repository share one local object pool, so a
# case costs its own records plus the objects its commits introduced - not a
# copy of the repository. max_cases bounds the corpus: the oldest cases are
# dropped first, and a case that already has recorded replays is never dropped.
# Set max_cases to 0 to keep every case. diversified_size caps the official
# gold-only eval set (default 32); 0 means one gold case per stratum. Unlabeled
# cases never fill it. Everything stays under <NM_HOME>/eval and is never
# uploaded anywhere.
eval:
  capture_provenance: true
  auto_capture: true
  max_cases: 200
  diversified_size: 32
`

// defaultBinary maps agent names to their default binary names.
var defaultBinary = map[types.AgentName]string{
	types.AgentClaude:   "claude",
	types.AgentCodex:    "codex",
	types.AgentRovoDev:  "acli",
	types.AgentOpenCode: "opencode",
	types.AgentPi:       "pi",
	types.AgentCopilot:  "copilot",
	types.AgentCursor:   "cursor-agent",
}

// nativeAgentProbeOrder is the priority order for auto-detecting native agents.
var nativeAgentProbeOrder = []types.AgentName{
	types.AgentClaude,
	types.AgentCodex,
	types.AgentOpenCode,
	types.AgentRovoDev,
	types.AgentPi,
	types.AgentCopilot,
	types.AgentCursor,
}

func isACPAgent(name types.AgentName) bool {
	_, ok := types.ACPTargetFor(name)
	return ok
}

var probeRovoDevSupport = func(ctx context.Context, bin string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "rovodev", "--help")
	winproc.Harden(cmd)
	output, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false, fmt.Errorf("probe rovodev support via %q timed out", bin)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		text := strings.ToLower(string(output))
		if strings.Contains(text, "unknown command") ||
			strings.Contains(text, "unknown subcommand") ||
			strings.Contains(text, "unrecognized command") ||
			strings.Contains(text, "no help topic for") {
			return false, nil
		}
		return false, fmt.Errorf("probe rovodev support via %q: %w", bin, err)
	}
	return false, fmt.Errorf("probe rovodev support via %q: %w", bin, err)
}

// ResolveAgent resolves configured agent names to available agents. A single
// explicit agent must be runnable; auto probes native agents, then registered ACP fallbacks;
// an ordered list is filtered to available agents, deduplicated by resolved
// identity, and kept as fallbacks. The lookPath function should behave like
// exec.LookPath.
func (c *Config) ResolveAgent(ctx context.Context, lookPath func(string) (string, error)) error {
	if c.StepAgents == nil {
		c.StepAgents = make(map[types.StepName][]types.AgentName)
	}
	defaultCandidates := c.configuredAgents()
	resolved, err := c.resolveAgents(ctx, defaultCandidates, ModelRoute{}, lookPath)
	if err != nil {
		return err
	}
	c.Agent = resolved[0]
	c.Agents = resolved
	for _, step := range []types.StepName{
		types.StepIntent,
		types.StepRefresh,
		types.StepReview,
		types.StepBuild,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
		types.StepPR,
		types.StepCI,
	} {
		candidates := c.StepAgents[step]
		model := c.StepModels[step]
		if len(candidates) == 0 && model.Name == "" {
			continue
		}
		if len(candidates) == 0 {
			candidates = defaultCandidates
		}
		resolved, err := c.resolveAgents(ctx, candidates, model, lookPath)
		if err != nil {
			return fmt.Errorf("resolve %s agent route: %w", step, err)
		}
		c.StepAgents[step] = resolved
	}
	if err := c.resolveReviewAdversary(ctx, lookPath); err != nil {
		return err
	}
	return nil
}

func (c *Config) resolveReviewAdversary(ctx context.Context, lookPath func(string) (string, error)) error {
	hasAgents := len(c.ReviewAdversaryAgents) > 0
	hasModel := c.ReviewAdversaryModel.Name != ""
	if !hasAgents && !hasModel {
		return nil
	}
	if !hasAgents || !hasModel {
		return fmt.Errorf("resolve review adversary route: review.adversary_agent and review.adversary_model must be configured together")
	}
	primaryModel := c.StepModels[types.StepReview]
	if primaryModel.Name == "" {
		return fmt.Errorf("resolve review adversary route: review.model is required so the controller can verify the adversarial pair")
	}
	if primaryModel.Vendor == c.ReviewAdversaryModel.Vendor {
		return fmt.Errorf("resolve review adversary route: adversarial review must be cross-vendor, but both models declare vendor %q", primaryModel.Vendor)
	}
	resolved, err := c.resolveAgents(ctx, c.ReviewAdversaryAgents, c.ReviewAdversaryModel, lookPath)
	if err != nil {
		return fmt.Errorf("resolve review adversary route: %w", err)
	}
	c.ReviewAdversaryAgents = resolved
	return nil
}

func (c *Config) resolveAgents(ctx context.Context, candidates []types.AgentName, model ModelRoute, lookPath func(string) (string, error)) ([]types.AgentName, error) {
	if len(candidates) <= 1 {
		name := firstAgent(candidates)
		if name == types.AgentAuto {
			name, err := c.resolveAutoAgentForModel(ctx, model, lookPath)
			if err != nil {
				return nil, err
			}
			return []types.AgentName{name}, nil
		}
		if err := validateAgentModelCompatibility(name, model); err != nil {
			return nil, err
		}
		resolved, ok, probe, err := c.resolveConfiguredAgent(ctx, name, lookPath)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, noRunnableAgentError([]types.AgentName{name}, []string{probe})
		}
		return []types.AgentName{resolved}, nil
	}

	resolved, err := c.resolveAgentList(ctx, candidates, model, lookPath)
	if err != nil {
		return nil, err
	}
	return resolved, nil
}

func (c *Config) configuredAgents() []types.AgentName {
	if len(c.Agents) > 0 {
		return copyAgents(c.Agents)
	}
	if c.Agent != "" {
		return []types.AgentName{c.Agent}
	}
	return []types.AgentName{types.AgentAuto}
}

func (c *Config) resolveAutoAgent(ctx context.Context, lookPath func(string) (string, error)) (types.AgentName, error) {
	return c.resolveAutoAgentForModel(ctx, ModelRoute{}, lookPath)
}

func (c *Config) resolveAutoAgentForModel(ctx context.Context, model ModelRoute, lookPath func(string) (string, error)) (types.AgentName, error) {
	probed := make([]string, 0, len(nativeAgentProbeOrder)+len(types.RegisteredACPTargets())+1)
	for _, name := range nativeAgentProbeOrder {
		if !agentCanServeModel(name, model) {
			continue
		}
		bin := string(name)
		if b, ok := defaultBinary[name]; ok {
			bin = b
		}
		if c.AgentPathOverride != nil {
			if p, ok := c.AgentPathOverride[string(name)]; ok {
				bin = p
			}
		}
		probed = append(probed, bin)
		resolvedBin, err := lookPath(bin)
		if err == nil {
			if name == types.AgentRovoDev {
				ok, probeErr := probeRovoDevSupport(ctx, resolvedBin)
				if probeErr != nil {
					return "", probeErr
				}
				if !ok {
					continue
				}
			}
			return name, nil
		} else if !errors.Is(err, exec.ErrNotFound) && !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("resolve %s agent from %q: %w", name, bin, err)
		}
	}
	if model.Name != "" && !types.IsBareACPModelName(model.Name) {
		return "", fmt.Errorf("no runnable agent found for model %q (vendor %q; looked for: %s); registered ACP fallbacks require a bare model family", model.Name, model.Vendor, strings.Join(probed, ", "))
	}
	for _, registered := range types.RegisteredACPTargets() {
		name := types.AgentName("acp:" + registered.Target)
		available, bins, err := c.acpAvailable(name, lookPath)
		probed = append(probed, bins...)
		if err != nil {
			return "", err
		}
		if available {
			return name, nil
		}
	}
	if model.Name != "" {
		return "", fmt.Errorf("no runnable agent found for model %q (vendor %q; looked for: %s); auto probed compatible native backends and registered ACP fallbacks", model.Name, model.Vendor, strings.Join(probed, ", "))
	}
	return "", noRunnableAgentError([]types.AgentName{types.AgentAuto}, probed)
}

func (c *Config) resolveAgentList(ctx context.Context, candidates []types.AgentName, model ModelRoute, lookPath func(string) (string, error)) ([]types.AgentName, error) {
	resolved := make([]types.AgentName, 0, len(candidates))
	seen := map[string]bool{}
	probed := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate == types.AgentAuto {
			name, err := c.resolveAutoAgentForModel(ctx, model, lookPath)
			if err != nil && strings.HasPrefix(err.Error(), "no runnable agent found") {
				probed = append(probed, "auto")
				continue
			}
			if err != nil {
				return nil, err
			}
			candidate = name
		}
		if err := validateAgentModelCompatibility(candidate, model); err != nil {
			if isACPAgent(candidate) {
				return nil, err
			}
			probed = append(probed, err.Error())
			continue
		}
		name, ok, probe, err := c.resolveConfiguredAgent(ctx, candidate, lookPath)
		if probe != "" {
			probed = append(probed, probe)
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		identity := resolvedAgentIdentity(name)
		if seen[identity] {
			continue
		}
		seen[identity] = true
		resolved = append(resolved, name)
	}
	if len(resolved) == 0 {
		return nil, noRunnableAgentError(candidates, probed)
	}
	return resolved, nil
}

func validateAgentModelCompatibility(name types.AgentName, model ModelRoute) error {
	if model.Name == "" {
		return nil
	}
	if isACPAgent(name) {
		if !types.IsBareACPModelName(model.Name) {
			return fmt.Errorf("parameterized or malformed bracketed model %q is not supported for ACP agent %q; configure a bare model family", model.Name, name)
		}
		return nil
	}
	if name == types.AgentOpenCode && !validOpenCodeModelName(model.Name) {
		return fmt.Errorf("agent %q requires model %q to use provider/model form", name, model.Name)
	}
	if name == types.AgentRovoDev {
		return fmt.Errorf("model %q is not supported for agent %q because Rovo Dev exposes no verified model-selection interface", model.Name, name)
	}
	if !agentCanServeModel(name, model) {
		return fmt.Errorf("agent %q cannot serve model %q from declared vendor %q", name, model.Name, model.Vendor)
	}
	return nil
}

func agentCanServeModel(name types.AgentName, model ModelRoute) bool {
	if model.Name == "" {
		return true
	}
	switch name {
	case types.AgentClaude:
		return model.Vendor == "anthropic" && !strings.Contains(model.Name, "/")
	case types.AgentCodex:
		return model.Vendor == "openai" && !strings.Contains(model.Name, "/")
	case types.AgentRovoDev:
		return false
	case types.AgentOpenCode:
		return validOpenCodeModelName(model.Name)
	case types.AgentPi, types.AgentCopilot:
		return true
	case types.AgentCursor:
		return true
	default:
		return false
	}
}

func validOpenCodeModelName(name string) bool {
	providerID, modelID, ok := strings.Cut(name, "/")
	return ok && providerID != "" && modelID != ""
}

func resolvedAgentIdentity(name types.AgentName) string {
	if target, ok := types.ACPTargetFor(name); ok {
		return "acp:" + target
	}
	return "native:" + string(name)
}

func noRunnableAgentError(configured []types.AgentName, probed []string) error {
	names := make([]string, 0, len(configured))
	for _, name := range configured {
		names = append(names, string(name))
	}
	return fmt.Errorf(
		"no runnable agent found for configured agent %s (looked for: %s); the gate cannot validate without an agent; install a supported native agent, choose an available agent in ~/.no-mistakes/config.yaml, or configure agent: acp:<target> with acpx installed",
		strings.Join(names, ", "),
		strings.Join(probed, ", "),
	)
}

func (c *Config) resolveConfiguredAgent(ctx context.Context, name types.AgentName, lookPath func(string) (string, error)) (types.AgentName, bool, string, error) {
	if name == types.AgentAuto {
		resolved, err := c.resolveAutoAgent(ctx, lookPath)
		if err != nil && strings.HasPrefix(err.Error(), "no runnable agent found") {
			return "", false, "auto", nil
		}
		return resolved, err == nil, "auto", err
	}
	if _, ok := defaultBinary[name]; !ok && !isACPAgent(name) {
		return "", false, string(name), fmt.Errorf("unknown agent %q; valid options: auto, claude, codex, rovodev, opencode, pi, copilot, cursor, acp:<target> (set 'agent' in ~/.no-mistakes/config.yaml)", name)
	}
	if isACPAgent(name) {
		available, bins, err := c.acpAvailable(name, lookPath)
		probe := strings.Join(bins, ", ")
		if err != nil {
			return "", false, probe, err
		}
		return name, available, probe, nil
	}
	bin := c.AgentPathFor(name)
	resolvedBin, err := lookPath(bin)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return "", false, bin, nil
		}
		return "", false, bin, fmt.Errorf("resolve %s agent from %q: %w", name, bin, err)
	}
	if name == types.AgentRovoDev {
		ok, probeErr := probeRovoDevSupport(ctx, resolvedBin)
		if probeErr != nil {
			return "", false, bin, probeErr
		}
		if !ok {
			return "", false, bin, nil
		}
	}
	return name, true, bin, nil
}

// AgentPath returns the binary path for the configured agent.
// ACP agents use acpx_path if set, otherwise acpx.
// Native agents use agent_path_override if set, otherwise the default binary name.
func (c *Config) AgentPath() string {
	return c.AgentPathFor(c.Agent)
}

func (c *Config) AgentPathFor(name types.AgentName) string {
	if isACPAgent(name) {
		if c.ACPXPath != "" {
			return c.ACPXPath
		}
		return "acpx"
	}
	if c.AgentPathOverride != nil {
		if p, ok := c.AgentPathOverride[string(name)]; ok {
			return p
		}
	}
	if b, ok := defaultBinary[name]; ok {
		return b
	}
	return string(name)
}

// acpAvailable reports whether the acpx shim and any probeable raw-command
// executable can be resolved. Only bare command names and clean absolute paths
// are probeable; relative, quoted, or escaped raw commands are left for acpx to
// execute from the worktree. It returns the binaries it considered for diagnostics.
func (c *Config) acpAvailable(name types.AgentName, lookPath func(string) (string, error)) (bool, []string, error) {
	bins := c.acpBinaries(name)
	for _, bin := range bins {
		if _, err := lookPath(bin); err != nil {
			if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
				return false, bins, nil
			}
			return false, bins, fmt.Errorf("resolve %s agent from %q: %w", name, bin, err)
		}
	}
	return true, bins, nil
}

func (c *Config) acpBinaries(name types.AgentName) []string {
	bins := make([]string, 0, 2)
	if target, ok := types.ACPTargetFor(name); ok {
		if bin, probeable := acpCommandBinaryForProbe(types.ACPRawCommand(target, c.ACPRegistryOverrides)); probeable {
			bins = append(bins, bin)
		}
	}
	return append(bins, c.AgentPathFor(name))
}

func acpCommandBinaryForProbe(command string) (string, bool) {
	return acpCommandBinaryForProbeForOS(command, runtime.GOOS)
}

func acpCommandBinaryForProbeForOS(command, goos string) (string, bool) {
	if strings.ContainsAny(command, `"'`) {
		return "", false
	}
	if goos != "windows" && strings.ContainsRune(command, '\\') {
		return "", false
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", false
	}
	bin := fields[0]
	if isAbsolutePathForProbe(bin, goos) {
		return bin, true
	}
	if containsPathSeparatorForProbe(bin, goos) {
		return "", false
	}
	return bin, true
}

func isAbsolutePathForProbe(path, goos string) bool {
	if goos == runtime.GOOS {
		return filepath.IsAbs(path)
	}
	if goos == "windows" {
		return isWindowsAbsolutePath(path)
	}
	return strings.HasPrefix(path, "/")
}

func isWindowsAbsolutePath(path string) bool {
	if len(path) >= 3 && isASCIILetter(path[0]) && path[1] == ':' && isWindowsPathSeparator(path[2]) {
		return true
	}
	return len(path) >= 3 && isWindowsPathSeparator(path[0]) && isWindowsPathSeparator(path[1])
}

func isASCIILetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isWindowsPathSeparator(value byte) bool {
	return value == '\\' || value == '/'
}

func containsPathSeparatorForProbe(path, goos string) bool {
	if goos == "windows" {
		return strings.ContainsAny(path, `/\`)
	}
	return strings.ContainsRune(path, '/')
}

// AgentArgs returns extra CLI args for the configured native agent, as declared in
// agent_args_override. Returns nil when no override is set for this agent.
func (c *Config) AgentArgs() []string {
	return c.AgentArgsFor(c.Agent)
}

func (c *Config) AgentArgsFor(name types.AgentName) []string {
	if c.AgentArgsOverride == nil {
		return nil
	}
	if args, ok := c.AgentArgsOverride[string(name)]; ok {
		return args
	}
	target, ok := types.ACPTargetFor(name)
	if !ok {
		return nil
	}
	if registered, ok := types.RegisteredACPTargetFor(target); ok {
		if args, exists := c.AgentArgsOverride[string(registered.LegacyArgsKey)]; exists {
			return args
		}
	}
	return c.AgentArgsOverride["acp:"+target]
}

// agentArgsOverrideAgents lists native agent names accepted as keys in
// agent_args_override. Explicit acp:<target> names are
// accepted dynamically by validateAgentArgsOverride.
var agentArgsOverrideAgents = map[string]bool{
	string(types.AgentClaude):   true,
	string(types.AgentCodex):    true,
	string(types.AgentRovoDev):  true,
	string(types.AgentOpenCode): true,
	string(types.AgentPi):       true,
	string(types.AgentCopilot):  true,
	string(types.AgentCursor):   true,
}

// reservedAgentArgs lists flags that no-mistakes manages internally and that
// users cannot override through agent_args_override. A flag is matched by its
// bare form (e.g. "--color") as well as the "--color=value" form.
var reservedAgentArgs = map[string]map[string]bool{
	string(types.AgentClaude): {
		"-p":              true,
		"--print":         true,
		"--verbose":       true,
		"--output-format": true,
		"--json-schema":   true,
		"-r":              true,
		"--resume":        true,
		"--session-id":    true,
		"-c":              true,
		"--continue":      true,
		"--fork-session":  true,
	},
	string(types.AgentCodex): {
		"exec":         true,
		"resume":       true,
		"--resume":     true,
		"--session":    true,
		"--session-id": true,
		"--thread":     true,
		"--thread-id":  true,
		"--last":       true,
		"--json":       true,
		"--color":      true,
	},
	string(types.AgentRovoDev): {
		"rovodev":                 true,
		"serve":                   true,
		"--disable-session-token": true,
	},
	string(types.AgentOpenCode): {
		"serve":        true,
		"--hostname":   true,
		"--port":       true,
		"--print-logs": true,
		"--model":      true,
	},
	string(types.AgentPi): {
		"--mode":       true,
		"--no-session": true,
	},
	string(types.AgentCopilot): {
		"-p":              true,
		"--prompt":        true,
		"--output-format": true,
		"--no-color":      true,
	},
	string(types.AgentCursor): {
		"-p":              true,
		"--print":         true,
		"--output-format": true,
		"--resume":        true,
		"resume":          true,
		"--continue":      true,
		"--workspace":     true,
		"--add-dir":       true,
		"--trust":         true,
	},
}

// validateAgentArgsOverride ensures each agent key is a known agent name and
// that no reserved flag appears. Empty args are rejected to catch trivially
// broken YAML.
func validateAgentArgsOverride(override map[string][]string) error {
	for name, args := range override {
		if !agentArgsOverrideAgents[name] {
			if _, ok := types.ACPTargetFor(types.AgentName(name)); !ok {
				return fmt.Errorf("invalid agent name in agent_args_override: %q (valid: claude, codex, rovodev, opencode, pi, copilot, cursor, acp:<target>)", name)
			}
		}
		reserved := reservedAgentArgs[name]
		if reserved == nil {
			if target, ok := types.ACPTargetFor(types.AgentName(name)); ok {
				if registered, found := types.RegisteredACPTargetFor(target); found {
					reserved = reservedAgentArgs[string(registered.LegacyArgsKey)]
				}
			}
		}
		for i, arg := range args {
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("invalid agent_args_override.%s[%d]: empty arg", name, i)
			}
			base := arg
			if idx := strings.Index(arg, "="); idx > 0 {
				base = arg[:idx]
			}
			if reserved[base] {
				return fmt.Errorf("invalid agent_args_override.%s[%d]: %q is managed by no-mistakes and cannot be overridden", name, i, arg)
			}
		}
	}
	return nil
}

// EnsureDefaultGlobalConfig writes the default config file at path if it does
// not already exist. Failures are logged at debug level and silently ignored.
func EnsureDefaultGlobalConfig(path string) {
	if _, err := os.Stat(path); err == nil {
		return
	} else if !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("failed to stat config path", "path", path, "error", err)
		return
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
		slog.Debug("failed to create config directory", "path", filepath.Dir(path), "error", mkErr)
		return
	}
	if wErr := os.WriteFile(path, []byte(defaultConfigYAML), 0o644); wErr != nil {
		slog.Debug("failed to write default config", "path", path, "error", wErr)
	}
}

// DefaultGlobalConfig returns the built-in global defaults.
func DefaultGlobalConfig() *GlobalConfig {
	return &GlobalConfig{
		Agent:                   types.AgentAuto,
		Agents:                  []types.AgentName{types.AgentAuto},
		CITimeout:               DefaultCITimeout,
		StepQuietWarning:        DefaultStepQuietWarning,
		DaemonConnectTimeout:    DefaultDaemonConnectTimeout,
		ProcessTerminationGrace: DefaultProcessTerminationGrace,
		LogLevel:                "info",
		SessionReuse:            true,
		Eval:                    evalDefaults(),
	}
}

// LoadGlobal reads global config from path. Returns defaults if file doesn't exist.
func LoadGlobal(path string) (*GlobalConfig, error) {
	cfg := DefaultGlobalConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			cfg.SourceYAML = []byte("{}\n")
			return cfg, nil
		}
		return nil, fmt.Errorf("read global config: %w", err)
	}
	return LoadGlobalFromBytes(data)
}

// LoadGlobalFromBytes parses global configuration from exact source bytes.
// It is used by recovery to revalidate a launch-recorded global config digest
// without silently switching to a newer file.
func LoadGlobalFromBytes(data []byte) (*GlobalConfig, error) {
	cfg := DefaultGlobalConfig()
	cfg.SourceYAML = append([]byte(nil), data...)
	var raw globalConfigRaw
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}
	if err := validateCommitRaw(raw.Commit); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}
	if strings.TrimSpace(raw.Document.Instructions) != "" {
		return nil, fmt.Errorf("parse global config: document.instructions is repo-only")
	}
	if strings.TrimSpace(raw.Hooks.PostWorktree) != "" {
		return nil, fmt.Errorf("parse global config: hooks.post_worktree is repo-only")
	}
	if err := validateTestRaw(raw.Test); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}
	if err := validateEvalRaw(raw.Eval); err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}

	if len(raw.Agent) > 0 {
		cfg.Agents = copyAgents(raw.Agent)
		cfg.Agent = firstAgent(cfg.Agents)
	}
	if raw.ACPXPath != "" {
		cfg.ACPXPath = raw.ACPXPath
	}
	if raw.ACPRegistryOverrides != nil {
		cfg.ACPRegistryOverrides = raw.ACPRegistryOverrides
	}
	if raw.AgentPathOverride != nil {
		cfg.AgentPathOverride = raw.AgentPathOverride
	}
	if raw.AgentArgsOverride != nil {
		if err := validateAgentArgsOverride(raw.AgentArgsOverride); err != nil {
			return nil, err
		}
		cfg.AgentArgsOverride = raw.AgentArgsOverride
	}
	timeoutValue := raw.CITimeout
	if timeoutValue == "" {
		timeoutValue = raw.BabysitTimeout
	}
	if timeoutValue != "" {
		d, err := parseCITimeout(timeoutValue)
		if err != nil {
			return nil, err
		}
		cfg.CITimeout = d
	}
	if raw.StepQuietWarning != "" {
		d, err := time.ParseDuration(raw.StepQuietWarning)
		if err != nil {
			return nil, fmt.Errorf("parse step_quiet_warning %q: %w", raw.StepQuietWarning, err)
		}
		if d > 0 {
			cfg.StepQuietWarning = d
		}
	}
	if raw.DaemonConnectTimeout != "" {
		d, err := parsePositiveDuration("daemon_connect_timeout", raw.DaemonConnectTimeout)
		if err != nil {
			return nil, err
		}
		cfg.DaemonConnectTimeout = d
	}
	if raw.ProcessTerminationGrace != "" {
		d, err := parsePositiveDuration("process_termination_grace", raw.ProcessTerminationGrace)
		if err != nil {
			return nil, err
		}
		cfg.ProcessTerminationGrace = d
	}
	if raw.LogLevel != "" {
		cfg.LogLevel = raw.LogLevel
	}
	if raw.SessionReuse != nil {
		cfg.SessionReuse = *raw.SessionReuse
	}
	cfg.Hooks.PRBody = strings.TrimSpace(raw.Hooks.PRBody)
	if len(raw.Overrides) > 0 {
		overrides, err := normalizeGlobalOverrides(raw.Overrides)
		if err != nil {
			return nil, fmt.Errorf("parse global config: %w", err)
		}
		cfg.Overrides = overrides
	}
	if raw.AutoFix.CI == nil {
		raw.AutoFix.CI = raw.AutoFix.Babysit
	}
	cfg.AutoFix = raw.AutoFix
	cfg.CI = raw.CI
	cfg.Commit = raw.Commit
	cfg.Intent = raw.Intent
	refresh, err := resolveLegacyStepConfig(raw.Refresh, raw.LegacyRebase)
	if err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}
	cfg.Refresh = refresh
	cfg.Review = raw.Review
	cfg.Build = raw.Build
	cfg.Test = raw.Test
	cfg.Document = raw.Document
	cfg.Lint = raw.Lint
	cfg.PR = raw.PR
	cfg.Prompts = raw.Prompts
	applyEvalOverrides(&cfg.Eval, &raw.Eval)

	return cfg, nil
}

// parseCITimeout interprets the ci_timeout config value. The keyword
// "unlimited" (also "none"/"off"/"never"), or any non-positive duration,
// resolves to CITimeoutUnlimited so the monitor never self-terminates;
// otherwise the value is parsed as a Go duration.
func parseCITimeout(value string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "unlimited", "none", "off", "never":
		return CITimeoutUnlimited, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse ci_timeout %q: %w", value, err)
	}
	if d <= 0 {
		return CITimeoutUnlimited, nil
	}
	return d, nil
}

func parsePositiveDuration(name, value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", name, value, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("parse %s %q: duration must be positive", name, value)
	}
	return d, nil
}

// LoadRepo reads per-repo config from dir/.no-mistakes.yaml.
// Returns zero-value config if file doesn't exist.
func LoadRepo(dir string) (*RepoConfig, error) {
	cfg := &RepoConfig{}

	path := filepath.Join(dir, ".no-mistakes.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read repo config: %w", err)
	}

	return parseRepoConfig(data)
}

// LoadRepoFromBytes parses per-repo config from raw YAML bytes. It is the
// trusted-config entry point: callers that read .no-mistakes.yaml from a
// specific git ref (e.g. the default branch) use this to avoid honoring a
// contributor's checked-out copy.
func LoadRepoFromBytes(data []byte) (*RepoConfig, error) {
	return parseRepoConfig(data)
}

func parseRepoConfig(data []byte) (*RepoConfig, error) {
	cfg := &RepoConfig{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse repo config: %w", err)
	}
	if err := finalizeRepoConfig(cfg); err != nil {
		return nil, fmt.Errorf("parse repo config: %w", err)
	}

	return cfg, nil
}

// finalizeRepoConfig applies the post-unmarshal normalization every repo-config
// source shares: commit-template validation, and the auto_fix.ci fallback to
// the legacy auto_fix.babysit spelling. A committed .no-mistakes.yaml and a
// global-config overrides entry are the same shape and must be read the same
// way, so both run through here rather than keeping separate copies that can
// drift.
func finalizeRepoConfig(cfg *RepoConfig) error {
	if err := validateCommitRaw(cfg.Commit); err != nil {
		return err
	}
	if err := validateReviewRaw(cfg.Review); err != nil {
		return err
	}
	if err := validateTestRaw(cfg.Test); err != nil {
		return err
	}
	if cfg.AutoFix.CI == nil {
		cfg.AutoFix.CI = cfg.AutoFix.Babysit
	}
	return nil
}

// validateReviewRaw fails the config closed on a review.path_instructions list
// the review step could not honor deterministically: a missing path or
// instructions value, a glob the matcher cannot compile, or a list that would
// overrun the review prompt budget. Rejecting the config aborts the run before
// an agent starts, which is preferable to silently dropping guidance the
// maintainer expects the reviewer to apply.
//
// This deliberately also runs on the PUSHED copy, even though EffectiveRepoConfig
// discards a pushed review block: the strict trusted-copy read
// (loadBareRepoConfigInput in internal/daemon) aborts EVERY run whose
// default-branch .no-mistakes.yaml fails these checks, so a branch carrying an
// invalid block has to fail here, before it merges, rather than brick the
// repository's pipeline afterwards. Do not scope this to the trusted copy.
func validateReviewRaw(review ReviewRaw) error {
	if len(review.PathInstructions) > MaxReviewPathInstructions {
		return fmt.Errorf("review.path_instructions has %d entries, at most %d are allowed", len(review.PathInstructions), MaxReviewPathInstructions)
	}
	for i, entry := range review.PathInstructions {
		path := strings.TrimSpace(entry.Path)
		if path == "" {
			return fmt.Errorf("review.path_instructions[%d].path must not be empty", i)
		}
		if strings.TrimSpace(entry.Instructions) == "" {
			return fmt.Errorf("review.path_instructions[%d].instructions must not be empty (path %q)", i, path)
		}
		if RenderedInstructions(entry.Instructions) == "" {
			return fmt.Errorf("review.path_instructions[%d].instructions for path %q is left empty once merge-conflict markers are removed; write the rule without <<<<<<<, =======, or >>>>>>>", i, path)
		}
		if err := validatePathInstructionGlob(path); err != nil {
			return fmt.Errorf("review.path_instructions[%d].path %q is not a valid glob: %w", i, path, err)
		}
	}
	if total := ReviewPathInstructionsBytes(review.PathInstructions); total > MaxReviewPathInstructionsBytes {
		return fmt.Errorf("review.path_instructions would add up to %d bytes to the review prompt, at most %d are allowed so the prompt stays within budget", total, MaxReviewPathInstructionsBytes)
	}
	return nil
}

// validatePathInstructionGlob mirrors how ignore_patterns are matched: a
// trailing "/**" is a literal subtree prefix rather than a glob, and everything
// else goes through path.Match, so only patterns Match can compile are accepted.
// It must stay path.Match rather than filepath.Match for the same reason the
// matcher does (matchIgnorePattern in internal/pipeline/steps): filepath.Match
// is separator-dependent, so on Windows the validator would accept patterns the
// matcher rejects and read a "\" as a path separator instead of an escape.
func validatePathInstructionGlob(pattern string) error {
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		if prefix == "" {
			return errors.New("subtree pattern needs a directory before /**")
		}
		return nil
	}
	if _, err := path.Match(pattern, "a"); err != nil {
		return err
	}
	return nil
}

// EffectiveRepoConfig returns the repo config that should drive the pipeline
// given a pushed-branch copy and the trusted default-branch copy.
//
// The code-executing selection fields - Commands and Hooks (run verbatim via
// sh -c on the daemon host), Agent/Agents, every per-step agent/model route,
// and the Review adversary route (select which
// processes launch with the maintainer's credentials, including fallback lists
// and acp: targets) - are
// taken only from the trusted copy when it is present, so a contributor's
// pushed branch cannot inject shell or pick an agent. Prompts follow the same
// opt-in boundary. Refresh strategy, document instructions, review path
// instructions, project-settings suppression, no-CI readiness, the CI rerun
// budget, and the evidence branch are always trusted-only.
// true the maintainer has explicitly opted in (via allow_repo_commands on the
// TRUSTED default-branch copy) to honoring the pushed branch's commands, hooks,
// prompt additions, and agent selection, including step routes.
// When there is no trusted copy and the maintainer has not opted in, all
// code-executing selectors are forced empty (Agent "" and nil Agents inherit
// the global agent; empty step routes inherit that run route; Commands{} and
// Hooks{} disable shell execution) rather than falling back to the pushed branch - this blocks
// the supply-chain vector for repos that ship .no-mistakes.yaml only on feature
// branches.
//
// Non-executing fields (ignore patterns, auto-fix, commit, intent, test) are
// always taken from the pushed copy, matching prior behavior, since they cannot
// run arbitrary shell, select a process, or spend the maintainer's CI minutes.
// The single exception inside test is evidence.branch, which names a git ref
// the daemon pushes to and is therefore trusted-only.
func EffectiveRepoConfig(pushed, trusted *RepoConfig, allowRepoCommands bool) *RepoConfig {
	if pushed == nil {
		pushed = &RepoConfig{}
	}
	effective := *pushed
	if trusted != nil {
		effective.Refresh.Strategy = trusted.Refresh.Strategy
		effective.Document.Instructions = trusted.Document.Instructions
		effective.Review.PathInstructions = append([]PathInstruction(nil), trusted.Review.PathInstructions...)
		// disable_project_settings is a security boundary: honor it ONLY from the
		// trusted default-branch copy so a pushed branch cannot turn the opt-out
		// off (and re-enable its own AGENTS.md) or on. A nil trusted copy here
		// means the trusted config was legitimately absent (the daemon aborts
		// separately when it could not be READ at all), so falsy is correct.
		effective.DisableProjectSettings = trusted.DisableProjectSettings
		// no_ci is a readiness boundary: honor it ONLY from the trusted
		// default-branch copy so a pushed branch cannot self-declare no-CI and
		// bypass checks that the default branch still expects.
		effective.NoCI = trusted.NoCI
		// ci.rerun_transient spends the maintainer's resources rather than the
		// contributor's: every rerun is another provider-side workflow run
		// billed to the repository. It is trusted-only for that reason, so a
		// pushed branch cannot raise its own rerun budget to the cap.
		effective.CI.RerunTransient = trusted.CI.RerunTransient
		// test.evidence.branch names the git ref evidence commits are pushed
		// to with the maintainer's credentials. It is trusted-only so a pushed
		// branch cannot aim them at another branch of the repository; the rest
		// of test.evidence stays pushed-readable because it only picks where
		// artifacts are collected. The publisher independently refuses any
		// branch without its marker file, so this is defense in depth.
		effective.Test.Evidence.Branch = trusted.Test.Evidence.Branch
	} else {
		effective.Refresh.Strategy = ""
		effective.Document.Instructions = ""
		effective.Review.PathInstructions = nil
		effective.DisableProjectSettings = false
		effective.NoCI = false
		effective.CI = CIRaw{}
		effective.Test.Evidence.Branch = nil
	}
	if allowRepoCommands {
		return &effective
	}
	if trusted != nil {
		effective.Commands = trusted.Commands
		effective.Hooks = trusted.Hooks
		effective.Agent = trusted.Agent
		effective.Agents = copyAgents(trusted.Agents)
		effective.Intent.Agent = trusted.Intent.Agent
		effective.Intent.Agents = copyAgents(trusted.Intent.Agents)
		effective.Intent.Model = trusted.Intent.Model
		effective.Refresh.Agent = trusted.Refresh.Agent
		effective.Refresh.Agents = copyAgents(trusted.Refresh.Agents)
		effective.Refresh.Model = trusted.Refresh.Model
		effective.Review = copyReviewRaw(trusted.Review)
		effective.Build = copyStepAgentRaw(trusted.Build)
		effective.Test.Agent = trusted.Test.Agent
		effective.Test.Agents = copyAgents(trusted.Test.Agents)
		effective.Test.Model = trusted.Test.Model
		effective.Document.Agent = trusted.Document.Agent
		effective.Document.Agents = copyAgents(trusted.Document.Agents)
		effective.Document.Model = trusted.Document.Model
		effective.Lint = copyStepAgentRaw(trusted.Lint)
		effective.PR = copyStepAgentRaw(trusted.PR)
		effective.CI.StepAgentRaw = copyStepAgentRaw(trusted.CI.StepAgentRaw)
		effective.Prompts = trusted.Prompts
	} else {
		effective.Commands = Commands{}
		effective.Hooks = Hooks{}
		effective.Agent = ""
		effective.Agents = nil
		effective.Intent.Agent = ""
		effective.Intent.Agents = nil
		effective.Intent.Model = ModelRoute{}
		effective.Refresh = RefreshRaw{}
		effective.Review = ReviewRaw{}
		effective.Build = StepAgentRaw{}
		effective.Test.Agent = ""
		effective.Test.Agents = nil
		effective.Test.Model = ModelRoute{}
		effective.Document.Agent = ""
		effective.Document.Agents = nil
		effective.Document.Model = ModelRoute{}
		effective.Lint = StepAgentRaw{}
		effective.PR = StepAgentRaw{}
		effective.CI.StepAgentRaw = StepAgentRaw{}
		effective.Prompts = PromptConfig{}
	}
	return &effective
}

func copyStepAgentRaw(src StepAgentRaw) StepAgentRaw {
	return StepAgentRaw{Agent: src.Agent, Agents: copyAgents(src.Agents), Model: src.Model}
}

func copyReviewRaw(src ReviewRaw) ReviewRaw {
	return ReviewRaw{
		StepAgentRaw:     copyStepAgentRaw(src.StepAgentRaw),
		AdversaryAgent:   src.AdversaryAgent,
		AdversaryAgents:  copyAgents(src.AdversaryAgents),
		AdversaryModel:   src.AdversaryModel,
		PathInstructions: append([]PathInstruction(nil), src.PathInstructions...),
	}
}

// ParseLogLevel converts a log level string to slog.Level.
// Accepted values: "debug", "info", "warn", "error". Defaults to slog.LevelInfo.
func ParseLogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// intentDefaults returns the default user-intent extraction settings.
// Default-on with a moderate file-overlap threshold and a 3-day slack window
// to handle "agent generated change Monday, user pushed Wednesday" cases.
func intentDefaults() Intent {
	return Intent{
		Enabled:         true,
		Threshold:       0.2,
		SlackDays:       3,
		DisabledReaders: map[string]bool{},
	}
}

// applyIntentOverrides applies non-nil raw values onto resolved defaults.
func applyIntentOverrides(dst *Intent, src *IntentRaw) {
	if src.Enabled != nil {
		dst.Enabled = *src.Enabled
	}
	if src.Threshold != nil {
		dst.Threshold = *src.Threshold
	}
	if src.SlackDays != nil {
		dst.SlackDays = *src.SlackDays
	}
	if len(src.DisabledReaders) > 0 {
		if dst.DisabledReaders == nil {
			dst.DisabledReaders = map[string]bool{}
		}
		for _, name := range src.DisabledReaders {
			dst.DisabledReaders[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
}

// testDefaults returns the default test-step settings. Evidence publication is
// opt-in (off by default); when enabled it lands under .no-mistakes/evidence on
// the default orphan evidence branch.
func testDefaults() Test {
	return Test{
		Evidence: Evidence{
			StoreInRepo: false,
			Dir:         ".no-mistakes/evidence",
			Branch:      evidence.DefaultBranch,
			LocalRoot:   "",
			Retention:   DefaultEvidenceRetention,
			MaxRuns:     DefaultEvidenceMaxRuns,
		},
	}
}

// applyTestOverrides applies non-nil raw values onto resolved defaults.
// The branch name is validated at config parse time (validateTestRaw), so an
// unusable value never reaches here.
//
// It deliberately covers only the repository-relevant half of test.evidence.
// The local-storage half is applied separately by applyEvidenceStorageOverrides
// so a repository config can never reach it (see EvidenceRaw.LocalRoot).
func applyTestOverrides(dst *Test, src *TestRaw) {
	if src.Evidence.StoreInRepo != nil {
		dst.Evidence.StoreInRepo = *src.Evidence.StoreInRepo
	}
	if src.Evidence.Dir != nil && strings.TrimSpace(*src.Evidence.Dir) != "" {
		dst.Evidence.Dir = strings.TrimSpace(*src.Evidence.Dir)
	}
	if src.Evidence.Branch != nil && strings.TrimSpace(*src.Evidence.Branch) != "" {
		if branch, err := evidence.NormalizeBranch(*src.Evidence.Branch); err == nil {
			dst.Evidence.Branch = branch
		}
	}
}

// applyEvidenceStorageOverrides applies the global-only local-storage half of
// test.evidence. Merge calls it with the GlobalConfig copy and nothing else, so
// neither a pushed nor a trusted repository config can move the daemon's
// evidence directory or change its retention budget. Values are validated at
// config parse time (validateTestRaw), so an unusable value never reaches here.
func applyEvidenceStorageOverrides(dst *Evidence, src *EvidenceRaw) {
	if src.LocalRoot != nil && strings.TrimSpace(*src.LocalRoot) != "" {
		dst.LocalRoot = strings.TrimSpace(*src.LocalRoot)
	}
	if src.Retention != nil {
		if d, err := parseEvidenceRetention(*src.Retention); err == nil {
			dst.Retention = d
		}
	}
	if src.MaxRuns != nil && *src.MaxRuns >= 0 {
		dst.MaxRuns = *src.MaxRuns
	}
}

// parseEvidenceRetention interprets test.evidence.retention. The keyword
// "unlimited" (also "none"/"off"/"never"), or any non-positive duration,
// disables age-based reaping and resolves to 0, which keeps every run's
// evidence until the max_runs ceiling removes it.
func parseEvidenceRetention(value string) (time.Duration, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	switch trimmed {
	case "":
		return DefaultEvidenceRetention, nil
	case "unlimited", "none", "off", "never":
		return 0, nil
	}
	d, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("test.evidence.retention: parse %q: %w", value, err)
	}
	if d <= 0 {
		return 0, nil
	}
	return d, nil
}

// evalDefaults returns the default local evaluation-corpus settings. Both
// halves are on by default: provenance is unrecoverable if it was not recorded
// at review time, and a corpus nobody has to remember to collect is the only
// kind that exists when a comparison is finally needed. The default cap keeps
// the corpus a rolling window rather than an unbounded archive.
func evalDefaults() Eval {
	return Eval{CaptureProvenance: true, AutoCapture: true, MaxCases: DefaultEvalMaxCases, DiversifiedSize: DefaultEvalDiversifiedSize}
}

// applyEvalOverrides applies non-nil raw values onto resolved defaults. The
// max_cases value is validated at config parse time (validateEvalRaw).
func applyEvalOverrides(dst *Eval, src *EvalRaw) {
	if src.CaptureProvenance != nil {
		dst.CaptureProvenance = *src.CaptureProvenance
	}
	if src.AutoCapture != nil {
		dst.AutoCapture = *src.AutoCapture
	}
	if src.MaxCases != nil && *src.MaxCases >= 0 {
		dst.MaxCases = *src.MaxCases
	}
	if src.DiversifiedSize != nil && *src.DiversifiedSize >= 0 {
		dst.DiversifiedSize = *src.DiversifiedSize
	}
}

// validateEvalRaw fails the config closed on a negative eval.max_cases. A
// negative cap has no defensible meaning here - it is neither "keep everything"
// (0) nor a bound - so surfacing the typo beats guessing which one was meant.
func validateEvalRaw(raw EvalRaw) error {
	if raw.MaxCases != nil && *raw.MaxCases < 0 {
		return fmt.Errorf("eval.max_cases must be 0 (keep every case) or greater, got %d", *raw.MaxCases)
	}
	if raw.DiversifiedSize != nil && *raw.DiversifiedSize < 0 {
		return fmt.Errorf("eval.diversified_size must be 0 (one gold case per stratum) or greater, got %d", *raw.DiversifiedSize)
	}
	return nil
}

// validateTestRaw fails the config closed on a test.evidence.branch value Git
// would reject as a branch name. Rejecting the config surfaces the typo where
// the user can fix it, rather than letting a run reach the push and fail there.
//
// Like validateReviewRaw this deliberately also runs on the PUSHED copy even
// though EffectiveRepoConfig only honors the trusted branch name: a branch
// carrying an invalid value has to fail before it merges.
func validateTestRaw(test TestRaw) error {
	if test.Evidence.Branch != nil {
		if _, err := evidence.NormalizeBranch(*test.Evidence.Branch); err != nil {
			return fmt.Errorf("test.evidence.branch: %w", err)
		}
	}
	// local_root must be absolute. The daemon's working directory is a bare
	// gate repository, so a relative path would resolve somewhere the operator
	// never named - and evidence would silently scatter instead of landing
	// where they asked. Surface the mistake in the config rather than at run
	// time. Like branch, this also validates the PUSHED copy even though the
	// value is honored only from the global config: a branch carrying an
	// invalid value has to fail before it merges.
	if test.Evidence.LocalRoot != nil {
		root := strings.TrimSpace(*test.Evidence.LocalRoot)
		if root != "" && !filepath.IsAbs(root) {
			return fmt.Errorf("test.evidence.local_root must be an absolute path, got %q", root)
		}
	}
	if test.Evidence.Retention != nil {
		if _, err := parseEvidenceRetention(*test.Evidence.Retention); err != nil {
			return err
		}
	}
	if test.Evidence.MaxRuns != nil && *test.Evidence.MaxRuns < 0 {
		return fmt.Errorf("test.evidence.max_runs must be 0 (keep every run) or greater, got %d", *test.Evidence.MaxRuns)
	}
	return nil
}

// autoFixDefaults returns the default auto-fix configuration.
func autoFixDefaults() AutoFix {
	return AutoFix{
		Lint:     3,
		Build:    3,
		Test:     3,
		Review:   0,
		Document: 3,
		CI:       3,
		Refresh:  3,
	}
}

// ciDefaults returns the default CI-step settings. Rerunning cancelled checks
// is off by default: a CANCELLED conclusion does not say who cancelled, so the
// safe baseline is to escalate rather than risk restarting a job a maintainer
// or a concurrency rule deliberately stopped. Repositories that know their
// cancellations are provider-side opt in via ci.rerun_transient.
func ciDefaults() CI {
	return CI{RerunTransient: DefaultCIRerunTransient}
}

// applyCIOverrides applies non-nil raw values onto resolved defaults, clamping
// the rerun budget into range: a negative value disables reruns rather than
// inverting the bound, and anything above MaxCIRerunTransient is capped so a
// typo cannot keep a run polling one commit indefinitely.
func applyCIOverrides(dst *CI, src *CIRaw) {
	if src.RerunTransient == nil {
		return
	}
	dst.RerunTransient = min(max(*src.RerunTransient, 0), MaxCIRerunTransient)
}

// applyAutoFixOverrides applies non-nil raw values onto resolved defaults.
func applyAutoFixOverrides(dst *AutoFix, src *AutoFixRaw) {
	if src.Lint != nil {
		dst.Lint = *src.Lint
	}
	if src.Build != nil {
		dst.Build = *src.Build
	}
	if src.Test != nil {
		dst.Test = *src.Test
	}
	if src.Review != nil {
		dst.Review = *src.Review
	}
	if src.Document != nil {
		dst.Document = *src.Document
	}
	if src.CI != nil {
		dst.CI = *src.CI
	}
	if src.Refresh != nil {
		dst.Refresh = *src.Refresh
	}
}

// AutoFixLimit returns the max auto-fix attempts for a given step.
// Steps without auto-fix support return 0.
func (c *Config) AutoFixLimit(step types.StepName) int {
	switch step {
	case types.StepLint:
		return c.AutoFix.Lint
	case types.StepBuild:
		return c.AutoFix.Build
	case types.StepTest:
		return c.AutoFix.Test
	case types.StepReview:
		return c.AutoFix.Review
	case types.StepDocument:
		return c.AutoFix.Document
	case types.StepCI:
		return c.AutoFix.CI
	case types.StepRefresh:
		return c.AutoFix.Refresh
	default:
		return 0
	}
}

// Merge combines global and per-repo config. Per-repo agent values, including
// ordered fallback lists, override global agent values when non-empty. Commands
// and ignore patterns come from repo config only.
func Merge(global *GlobalConfig, repo *RepoConfig) *Config {
	af := autoFixDefaults()
	applyAutoFixOverrides(&af, &global.AutoFix)
	applyAutoFixOverrides(&af, &repo.AutoFix)

	ci := ciDefaults()
	// The operator's global value is a machine-wide floor they can always set;
	// the repo value is trusted-only (EffectiveRepoConfig sourced it from the
	// default branch), so the maintainer of the repository still has the last
	// word on how many workflow runs their project is billed for.
	applyCIOverrides(&ci, &global.CI)
	applyCIOverrides(&ci, &repo.CI)

	intent := intentDefaults()
	applyIntentOverrides(&intent, &global.Intent)
	applyIntentOverrides(&intent, &repo.Intent)

	test := testDefaults()
	applyTestOverrides(&test, &global.Test)
	applyTestOverrides(&test, &repo.Test)
	// Applied last and from the global config only: where the daemon writes
	// evidence on this machine, and how long it keeps it, is never a
	// repository's decision (see EvidenceRaw.LocalRoot).
	applyEvidenceStorageOverrides(&test.Evidence, &global.Test.Evidence)

	commit := Commit{FixMessage: DefaultFixMessageTemplate}
	if global.Commit.FixMessage != nil {
		commit.FixMessage = *global.Commit.FixMessage
	}
	if repo.Commit.FixMessage != nil {
		commit.FixMessage = *repo.Commit.FixMessage
	}

	// post_worktree is repo-only, so it comes straight from the repo layer.
	// pr_body takes a machine-wide default that a repo config can override.
	hooks := Hooks{PostWorktree: repo.Hooks.PostWorktree, PRBody: global.Hooks.PRBody}
	if strings.TrimSpace(repo.Hooks.PRBody) != "" {
		hooks.PRBody = repo.Hooks.PRBody
	}

	cfg := &Config{
		Agent:                   global.Agent,
		Agents:                  copyAgents(global.Agents),
		StepAgents:              global.configuredStepAgents(),
		StepModels:              global.configuredStepModels(),
		ReviewAdversaryAgents:   copyAgents(global.Review.AdversaryAgents),
		ReviewAdversaryModel:    global.Review.AdversaryModel,
		ACPXPath:                global.ACPXPath,
		ACPRegistryOverrides:    global.ACPRegistryOverrides,
		AgentPathOverride:       global.AgentPathOverride,
		AgentArgsOverride:       global.AgentArgsOverride,
		CITimeout:               global.CITimeout,
		StepQuietWarning:        global.StepQuietWarning,
		ProcessTerminationGrace: global.ProcessTerminationGrace,
		LogLevel:                global.LogLevel,
		SessionReuse:            global.SessionReuse,
		Commands:                repo.Commands,
		Hooks:                   hooks,
		IgnorePatterns:          repo.IgnorePatterns,
		AutoFix:                 af,
		CI:                      ci,
		Commit:                  commit,
		Intent:                  intent,
		Test:                    test,
		Document:                Document{Instructions: strings.TrimSpace(repo.Document.Instructions)},
		Review:                  Review{PathInstructions: resolvePathInstructions(repo.Review.PathInstructions)},
		Eval:                    global.Eval,
		Prompts:                 mergePromptConfigs(global.Prompts, repo.Prompts),
		RefreshStrategy:         repo.Refresh.Strategy.OrDefault(),
		// repo is the EffectiveRepoConfig result, so this value is already
		// trusted-only (EffectiveRepoConfig sourced it from the trusted copy).
		DisableProjectSettings: repo.DisableProjectSettings,
		NoCI:                   repo.NoCI,
	}

	if repo.Agent != "" {
		cfg.Agent = repo.Agent
		cfg.Agents = copyAgents(repo.Agents)
		if len(cfg.Agents) == 0 {
			cfg.Agents = []types.AgentName{repo.Agent}
		}
	}
	for step, agents := range repo.ConfiguredStepAgents() {
		cfg.StepAgents[step] = copyAgents(agents)
	}
	for step, model := range repo.ConfiguredStepModels() {
		cfg.StepModels[step] = model
	}
	if len(repo.Review.AdversaryAgents) > 0 {
		cfg.ReviewAdversaryAgents = copyAgents(repo.Review.AdversaryAgents)
	}
	if repo.Review.AdversaryModel.Name != "" {
		cfg.ReviewAdversaryModel = repo.Review.AdversaryModel
	}

	return cfg
}

// EnableEvalProvenance pins the exact configuration this run reviews under so
// a later replay grades a candidate against identical conditions. The caller
// decides whether to call it (see Eval.CaptureProvenance); this is the single
// owner of what "exact provenance" contains.
func (c *Config) EnableEvalProvenance(global *GlobalConfig, repo *RepoConfig) error {
	if c == nil || global == nil || repo == nil {
		return fmt.Errorf("eval provenance requires merged, global, and repository configuration")
	}
	replayConfig, err := MarshalEvalReplayConfig(c)
	if err != nil {
		return fmt.Errorf("serialize eval replay configuration: %w", err)
	}
	c.ReplayConfigJSON = replayConfig
	c.CaptureEvalProvenance = true
	return nil
}
