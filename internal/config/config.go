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
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
	"unicode"

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
)

// GlobalConfig represents ~/.no-mistakes/config.yaml.
type GlobalConfig struct {
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
	// SessionReuse controls per-run, per-role agent session reuse in the
	// review loop (one durable reviewer session across full reviews, a
	// separate durable fixer session across fix turns). Default true; set
	// session_reuse: false to force every invocation cold.
	SessionReuse bool `yaml:"-"`
	AutoFix      AutoFixRaw
	Commit       CommitRaw
	Intent       IntentRaw
	Refresh      StepAgentRaw
	Review       ReviewRaw
	Test         TestRaw
	Document     DocumentRaw
	Lint         StepAgentRaw
	PR           StepAgentRaw
	CI           StepAgentRaw
}

// globalConfigRaw is the on-disk YAML representation with duration as string.
type globalConfigRaw struct {
	Agent                   agentList           `yaml:"agent"`
	ACPXPath                string              `yaml:"acpx_path"`
	ACPRegistryOverrides    map[string]string   `yaml:"acp_registry_overrides"`
	AgentPathOverride       map[string]string   `yaml:"agent_path_override"`
	AgentArgsOverride       map[string][]string `yaml:"agent_args_override"`
	CITimeout               string              `yaml:"ci_timeout"`
	DaemonConnectTimeout    string              `yaml:"daemon_connect_timeout"`
	ProcessTerminationGrace string              `yaml:"process_termination_grace"`
	BabysitTimeout          string              `yaml:"babysit_timeout"`
	StepQuietWarning        string              `yaml:"step_quiet_warning"`
	LogLevel                string              `yaml:"log_level"`
	SessionReuse            *bool               `yaml:"session_reuse"`
	AutoFix                 AutoFixRaw          `yaml:"auto_fix"`
	Commit                  CommitRaw           `yaml:"commit"`
	Intent                  IntentRaw           `yaml:"intent"`
	Refresh                 *StepAgentRaw       `yaml:"refresh"`
	LegacyRebase            *StepAgentRaw       `yaml:"rebase"`
	Review                  ReviewRaw           `yaml:"review"`
	Test                    TestRaw             `yaml:"test"`
	Document                DocumentRaw         `yaml:"document"`
	Lint                    StepAgentRaw        `yaml:"lint"`
	PR                      StepAgentRaw        `yaml:"pr"`
	CI                      StepAgentRaw        `yaml:"ci"`
}

// RepoConfig represents .no-mistakes.yaml in a repo root.
type RepoConfig struct {
	Repo           string            `yaml:"repo"`
	Agent          types.AgentName   `yaml:"agent"`
	Agents         []types.AgentName `yaml:"-"`
	Commands       Commands          `yaml:"commands"`
	Hooks          Hooks             `yaml:"hooks"`
	IgnorePatterns []string          `yaml:"ignore_patterns"`
	// AllowRepoCommands opts in to honoring the code-executing selection
	// fields (commands.{test,lint,format}, hooks.post_worktree, agent, and every step agent route) from a contributor's
	// pushed branch instead of the trusted default-branch copy. It is read
	// ONLY from the trusted default-branch copy of .no-mistakes.yaml (never
	// the pushed SHA), so a contributor cannot self-enable. Default false:
	// the pushed branch controls nothing that executes, including model selection.
	AllowRepoCommands bool       `yaml:"allow_repo_commands"`
	AutoFix           AutoFixRaw `yaml:"auto_fix"`
	Commit            CommitRaw  `yaml:"commit"`
	Intent            IntentRaw  `yaml:"intent"`
	Refresh           RefreshRaw `yaml:"refresh"`
	Review            ReviewRaw  `yaml:"review"`
	Test              TestRaw    `yaml:"test"`
	// Document carries the repository's documentation placement policy. It
	// steers the document step's gate prompt, so it is honored ONLY from the
	// trusted default-branch copy of .no-mistakes.yaml (see
	// EffectiveRepoConfig): a contributor's pushed branch must not be able to
	// weaken documentation rules for its own review.
	Document DocumentRaw  `yaml:"document"`
	Lint     StepAgentRaw `yaml:"lint"`
	PR       StepAgentRaw `yaml:"pr"`
	CI       StepAgentRaw `yaml:"ci"`
	// DisableProjectSettings opts the repository out of loading project-level
	// agent settings/instructions (AGENTS.md/CLAUDE.md and the equivalent
	// per-harness project settings) into gate agents. It exists for
	// agent-orchestration repos (e.g. firstmate) whose project instructions
	// would otherwise install a fleet-captain identity on a gate agent. It is a
	// SECURITY boundary honored ONLY from the trusted default-branch copy of
	// .no-mistakes.yaml (see EffectiveRepoConfig and the daemon's
	// assertGateTrustedConfigReadable): a contributor's pushed branch must not be
	// able to turn it off (or on). Default false; a plain bool so a missing key
	// or a YAML/JSON null is falsy and preserves current loading.
	DisableProjectSettings bool `yaml:"disable_project_settings"`
	present                map[string]bool
}

// StepAgentRaw is the YAML representation of one step's optional agent route.
// Agent is the primary entry and Agents preserves the ordered fallback list.
type StepAgentRaw struct {
	Agent  types.AgentName   `yaml:"-"`
	Agents []types.AgentName `yaml:"-"`
	Model  ModelRoute        `yaml:"model"`
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
	AdversaryAgent  types.AgentName   `yaml:"-"`
	AdversaryAgents []types.AgentName `yaml:"-"`
	AdversaryModel  ModelRoute        `yaml:"adversary_model"`
}

func (c *ReviewRaw) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Agent          agentList  `yaml:"agent"`
		Model          ModelRoute `yaml:"model"`
		AdversaryAgent agentList  `yaml:"adversary_agent"`
		AdversaryModel ModelRoute `yaml:"adversary_model"`
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

func (c *RepoConfig) UnmarshalYAML(value *yaml.Node) error {
	type repoConfigRaw struct {
		Repo                   string        `yaml:"repo"`
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
		Test                   TestRaw       `yaml:"test"`
		Document               DocumentRaw   `yaml:"document"`
		Lint                   StepAgentRaw  `yaml:"lint"`
		PR                     StepAgentRaw  `yaml:"pr"`
		CI                     StepAgentRaw  `yaml:"ci"`
		DisableProjectSettings bool          `yaml:"disable_project_settings"`
	}
	var raw repoConfigRaw
	if err := value.Decode(&raw); err != nil {
		return err
	}
	c.present = repoConfigPresence(value)
	c.Repo = strings.TrimSpace(raw.Repo)
	c.Agent = firstAgent(raw.Agent)
	c.Agents = copyAgents(raw.Agent)
	c.Commands = raw.Commands
	c.Hooks = raw.Hooks
	c.IgnorePatterns = raw.IgnorePatterns
	c.AllowRepoCommands = raw.AllowRepoCommands
	c.AutoFix = raw.AutoFix
	c.Commit = raw.Commit
	c.Intent = raw.Intent
	refresh, err := resolveLegacyRepoRefreshConfig(raw.Refresh, raw.LegacyRebase)
	if err != nil {
		return err
	}
	c.Refresh = refresh
	c.Review = raw.Review
	c.Test = raw.Test
	c.Document = raw.Document
	c.Lint = raw.Lint
	c.PR = raw.PR
	c.CI = raw.CI
	c.DisableProjectSettings = raw.DisableProjectSettings
	return nil
}

// OverlayRepoConfig applies only fields explicitly present in override. It is
// used for machine-local repo config, where omitted fields continue to inherit
// the already-resolved committed configuration while explicit empty values can
// deliberately clear commands and agent routes.
func OverlayRepoConfig(base, override *RepoConfig) *RepoConfig {
	if base == nil {
		base = &RepoConfig{}
	}
	if override == nil {
		return cloneRepoConfig(base)
	}
	out := cloneRepoConfig(base)
	if override.has("repo") {
		out.Repo = override.Repo
	}
	if override.has("agent") {
		out.Agent = override.Agent
		out.Agents = copyAgents(override.Agents)
	}
	if override.has("commands.lint") {
		out.Commands.Lint = override.Commands.Lint
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
	if override.has("ignore_patterns") {
		out.IgnorePatterns = copyStrings(override.IgnorePatterns)
	}
	if override.has("allow_repo_commands") {
		out.AllowRepoCommands = override.AllowRepoCommands
	}
	if override.has("auto_fix.lint") {
		out.AutoFix.Lint = override.AutoFix.Lint
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
	if override.has("disable_project_settings") {
		out.DisableProjectSettings = override.DisableProjectSettings
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

func cloneRepoConfig(src *RepoConfig) *RepoConfig {
	out := *src
	out.Agents = copyAgents(src.Agents)
	out.IgnorePatterns = copyStrings(src.IgnorePatterns)
	out.Intent.Agents = copyAgents(src.Intent.Agents)
	out.Intent.DisabledReaders = copyStrings(src.Intent.DisabledReaders)
	out.Refresh.Agents = copyAgents(src.Refresh.Agents)
	out.Review = copyReviewRaw(src.Review)
	out.Test.Agents = copyAgents(src.Test.Agents)
	out.Document.Agents = copyAgents(src.Document.Agents)
	out.Lint = copyStepAgentRaw(src.Lint)
	out.PR = copyStepAgentRaw(src.PR)
	out.CI = copyStepAgentRaw(src.CI)
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
	Test   string `yaml:"test"`
	Format string `yaml:"format"`
}

// Hooks holds deterministic controller commands that run outside pipeline
// step execution. Like Commands, hook values are trusted code-executing
// configuration and are never sourced from an untrusted pushed branch.
type Hooks struct {
	PostWorktree string `yaml:"post_worktree"`
}

// AutoFixRaw is the YAML representation of auto-fix config.
// Pointer fields distinguish "not set" (nil) from "set to 0" (disabled).
type AutoFixRaw struct {
	Lint     *int `yaml:"lint"`
	Test     *int `yaml:"test"`
	Review   *int `yaml:"review"`
	Document *int `yaml:"document"`
	CI       *int `yaml:"ci"`
	Babysit  *int `yaml:"babysit"`
	Refresh  *int `yaml:"refresh"`
}

func (c *AutoFixRaw) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Lint         *int `yaml:"lint"`
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

// AutoFix holds resolved per-step auto-fix attempt limits.
// A value of 0 means auto-fix is disabled (requires manual approval).
type AutoFix struct {
	Lint     int
	Test     int
	Review   int
	Document int
	CI       int
	Refresh  int
}

// Config is the merged result of global + per-repo configuration.
type Config struct {
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
	Commit                  Commit
	Intent                  Intent
	Test                    Test
	Document                Document
	RefreshStrategy         types.RefreshStrategy
	// DisableProjectSettings is the resolved, trusted-only opt-out (see the
	// RepoConfig field). When true, gate agents are launched with their
	// project-level settings/instructions suppressed; the daemon fails the run
	// closed if the resolved harness has no verified suppression knob.
	DisableProjectSettings bool
}

// Document is the resolved document-step config. Instructions come from the
// trusted default-branch repo config and augment the built-in placement
// policy in the document prompt.
type Document struct {
	Instructions string
}

// TestRaw is the YAML representation of test-step settings.
type TestRaw struct {
	Agent    types.AgentName   `yaml:"-"`
	Agents   []types.AgentName `yaml:"-"`
	Model    ModelRoute        `yaml:"model"`
	Evidence EvidenceRaw       `yaml:"evidence"`
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
}

// Test is the resolved test-step config.
type Test struct {
	Evidence Evidence
}

// Evidence is the resolved test-evidence config. When StoreInRepo is true, the
// test step writes evidence artifacts into Dir (relative to the repo worktree)
// so they are committed, pushed, and viewable directly on the PR. Otherwise
// evidence stays in a temporary directory referenced only by local path.
type Evidence struct {
	StoreInRepo bool
	Dir         string
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

// defaultConfigYAML is the template written when no global config file exists.
const defaultConfigYAML = `# no-mistakes global configuration

# Agent to use for code generation. This may also be an ordered fallback list,
# for example: agent: [codex, claude]
# Options: auto, claude, codex, rovodev, opencode, pi, copilot, cursor, acp:<target>
# "auto" detects the first available native agent or ACP alias on your system
# "cursor" is an ACP alias for acp:cursor using cursor-agent acp via acpx
# "acp:cursor" also uses that Cursor default command
# Use acp:<target> to run an optional user-installed acpx target, for example acp:gemini
agent: auto

# Optional per-step routes. Each agent accepts the same scalar or ordered
# fallback-list form as the run-wide agent. A model is a typed name plus an
# explicit lowercase vendor; adapters translate it through their verified
# native interface. OpenCode names use provider/model. Rovo Dev and ACP reject
# model routes because their managed integrations expose no verified model
# selection interface.
# Unconfigured steps inherit the run-wide agent and its default model.
# Supported sections: intent, refresh, review, test, document, lint, pr, ci.
# review:
#   agent: claude
#   model: {name: claude-opus-5, vendor: anthropic}
#   adversary_agent: codex
#   adversary_model: {name: gpt-5.6-sol, vendor: openai}

# Optional path to the user-installed acpx binary for acp:<target> agents and ACP aliases
# acpx_path: acpx

# Optional ACP target command overrides for acp:<target> agents and ACP aliases
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

# Reuse one durable agent session per run for the review loop: the reviewer
# keeps a single session across the initial review and every full rereview,
# and review fixes keep a separate fixer session. Roles never share a session.
# Supported for claude and codex; other agents run cold. Set false to force
# every agent invocation cold.
session_reuse: true

# Log level for daemon output
# Options: debug, info, warn, error
log_level: info

# Override native agent binary paths (optional)
# agent_path_override:
#   claude: /usr/local/bin/claude
#   codex: /opt/codex

# Extra native agent CLI flags (optional, global only). ACP targets and aliases
# do not support these flags; route a step to a native agent when it needs them.
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
  test: 3
  review: 0
  document: 3
  ci: 3

# Auto-fix commit subject template. Available variables: {{.Step}} and {{.Summary}}.
# Repo config may override this value.
# commit:
#   fix_message: "no-mistakes({{.Step}}): {{.Summary}}"

# User-intent extraction. When you push a branch, no-mistakes can read recent
# transcripts from your local agent (Claude Code, Codex, OpenCode, Rovo Dev, Pi,
# Copilot CLI), pick the session that produced the change, summarize the user
# intent, and feed it to review, test, document, lint, and PR agents so they
# understand what you were trying to do - not just the diff.
intent:
  enabled: true
  threshold: 0.2
  slack_days: 3
  # disabled_readers: [codex]

# Test-step evidence artifacts (screenshots, recordings, logs the test step
# gathers to demonstrate the change works). By default they are kept in a
# temporary directory and referenced by local path. Opt in to store_in_repo to
# commit them into the repo under a readable, branch-named directory so they are
# pushed and render directly on the PR.
# test:
#   evidence:
#     store_in_repo: true
#     dir: .no-mistakes/evidence
`

// defaultBinary maps agent names to their default binary names.
var defaultBinary = map[types.AgentName]string{
	types.AgentClaude:   "claude",
	types.AgentCodex:    "codex",
	types.AgentRovoDev:  "acli",
	types.AgentOpenCode: "opencode",
	types.AgentPi:       "pi",
	types.AgentCopilot:  "copilot",
}

// nativeAgentProbeOrder is the priority order for auto-detecting native agents.
var nativeAgentProbeOrder = []types.AgentName{
	types.AgentClaude,
	types.AgentCodex,
	types.AgentOpenCode,
	types.AgentRovoDev,
	types.AgentPi,
	types.AgentCopilot,
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
// explicit agent must be runnable; auto probes native agents, then ACP aliases;
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
	probed := make([]string, 0, len(nativeAgentProbeOrder)+len(types.ACPAliases())+1)
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
	if model.Name != "" {
		return "", fmt.Errorf("no runnable agent found for model %q (vendor %q; looked for: %s); auto only probes native backends capable of the declared vendor", model.Name, model.Vendor, strings.Join(probed, ", "))
	}
	for _, alias := range types.ACPAliases() {
		available, bins, err := c.acpAvailable(alias.Name, lookPath)
		probed = append(probed, bins...)
		if err != nil {
			return "", err
		}
		if available {
			return alias.Name, nil
		}
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
		return fmt.Errorf("model %q is not supported for ACP agent %q because configured model selection is not passed into ACP target startup", model.Name, name)
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
// ACP agents and ACP aliases use acpx_path if set, otherwise acpx.
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
	return c.AgentArgsOverride[string(name)]
}

// agentArgsOverrideAgents lists native agent names accepted as keys in
// agent_args_override.
var agentArgsOverrideAgents = map[string]bool{
	string(types.AgentClaude):   true,
	string(types.AgentCodex):    true,
	string(types.AgentRovoDev):  true,
	string(types.AgentOpenCode): true,
	string(types.AgentPi):       true,
	string(types.AgentCopilot):  true,
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
}

// validateAgentArgsOverride ensures each agent key is a known agent name and
// that no reserved flag appears. Empty args are rejected to catch trivially
// broken YAML.
func validateAgentArgsOverride(override map[string][]string) error {
	for name, args := range override {
		if !agentArgsOverrideAgents[name] {
			if _, ok := types.ACPTargetFor(types.AgentName(name)); ok {
				return fmt.Errorf("invalid agent_args_override.%s: ACP agent model/reasoning overrides are not supported; route the step to a native agent instead", name)
			}
			return fmt.Errorf("invalid agent name in agent_args_override: %q (valid: claude, codex, rovodev, opencode, pi, copilot)", name)
		}
		reserved := reservedAgentArgs[name]
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
	}
}

// LoadGlobal reads global config from path. Returns defaults if file doesn't exist.
func LoadGlobal(path string) (*GlobalConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return DefaultGlobalConfig(), nil
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
	if raw.AutoFix.CI == nil {
		raw.AutoFix.CI = raw.AutoFix.Babysit
	}
	cfg.AutoFix = raw.AutoFix
	cfg.Commit = raw.Commit
	cfg.Intent = raw.Intent
	refresh, err := resolveLegacyStepConfig(raw.Refresh, raw.LegacyRebase)
	if err != nil {
		return nil, fmt.Errorf("parse global config: %w", err)
	}
	cfg.Refresh = refresh
	cfg.Review = raw.Review
	cfg.Test = raw.Test
	cfg.Document = raw.Document
	cfg.Lint = raw.Lint
	cfg.PR = raw.PR
	cfg.CI = raw.CI

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
	if err := validateCommitRaw(cfg.Commit); err != nil {
		return nil, fmt.Errorf("parse repo config: %w", err)
	}
	if cfg.AutoFix.CI == nil {
		cfg.AutoFix.CI = cfg.AutoFix.Babysit
	}

	return cfg, nil
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
// pushed branch cannot inject shell or pick an agent. Document (the
// documentation placement policy injected into the document gate prompt) is
// trusted-only for the same reason: a pushed branch must not weaken the
// documentation rules that gate itself. DisableProjectSettings is also
// trusted-only so a pushed branch cannot enable or defeat the gate-agent
// project-instruction boundary. When allowRepoCommands is
// true the maintainer has explicitly opted in (via allow_repo_commands on the
// TRUSTED default-branch copy) to honoring the pushed branch's commands, hooks,
// and agent selection, including step routes.
// When there is no trusted copy and the maintainer has not opted in, all
// code-executing selectors are forced empty (Agent "" and nil Agents inherit
// the global agent; empty step routes inherit that run route; Commands{} and
// Hooks{} disable shell execution) rather than falling back to the pushed branch - this blocks
// the supply-chain vector for repos that ship .no-mistakes.yaml only on feature
// branches.
//
// Non-executing fields (ignore patterns, auto-fix, commit, intent settings
// other than its agent route, and test evidence) are always taken from the
// pushed copy, matching prior behavior, since they cannot run arbitrary shell
// or select a process.
func EffectiveRepoConfig(pushed, trusted *RepoConfig, allowRepoCommands bool) *RepoConfig {
	if pushed == nil {
		pushed = &RepoConfig{}
	}
	effective := *pushed
	if trusted != nil {
		effective.Refresh.Strategy = trusted.Refresh.Strategy
		effective.Document.Instructions = trusted.Document.Instructions
		// disable_project_settings is a security boundary: honor it ONLY from the
		// trusted default-branch copy so a pushed branch cannot turn the opt-out
		// off (and re-enable its own AGENTS.md) or on. A nil trusted copy here
		// means the trusted config was legitimately absent (the daemon aborts
		// separately when it could not be READ at all), so falsy is correct.
		effective.DisableProjectSettings = trusted.DisableProjectSettings
	} else {
		effective.Refresh.Strategy = ""
		effective.Document.Instructions = ""
		effective.DisableProjectSettings = false
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
		effective.Test.Agent = trusted.Test.Agent
		effective.Test.Agents = copyAgents(trusted.Test.Agents)
		effective.Test.Model = trusted.Test.Model
		effective.Document.Agent = trusted.Document.Agent
		effective.Document.Agents = copyAgents(trusted.Document.Agents)
		effective.Document.Model = trusted.Document.Model
		effective.Lint = copyStepAgentRaw(trusted.Lint)
		effective.PR = copyStepAgentRaw(trusted.PR)
		effective.CI = copyStepAgentRaw(trusted.CI)
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
		effective.Test.Agent = ""
		effective.Test.Agents = nil
		effective.Test.Model = ModelRoute{}
		effective.Document.Agent = ""
		effective.Document.Agents = nil
		effective.Document.Model = ModelRoute{}
		effective.Lint = StepAgentRaw{}
		effective.PR = StepAgentRaw{}
		effective.CI = StepAgentRaw{}
	}
	return &effective
}

func copyStepAgentRaw(src StepAgentRaw) StepAgentRaw {
	return StepAgentRaw{Agent: src.Agent, Agents: copyAgents(src.Agents), Model: src.Model}
}

func copyReviewRaw(src ReviewRaw) ReviewRaw {
	return ReviewRaw{
		StepAgentRaw:    copyStepAgentRaw(src.StepAgentRaw),
		AdversaryAgent:  src.AdversaryAgent,
		AdversaryAgents: copyAgents(src.AdversaryAgents),
		AdversaryModel:  src.AdversaryModel,
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

// testDefaults returns the default test-step settings. Evidence storage is
// opt-in (off by default); when enabled it lands under .no-mistakes/evidence.
func testDefaults() Test {
	return Test{
		Evidence: Evidence{
			StoreInRepo: false,
			Dir:         ".no-mistakes/evidence",
		},
	}
}

// applyTestOverrides applies non-nil raw values onto resolved defaults.
func applyTestOverrides(dst *Test, src *TestRaw) {
	if src.Evidence.StoreInRepo != nil {
		dst.Evidence.StoreInRepo = *src.Evidence.StoreInRepo
	}
	if src.Evidence.Dir != nil && strings.TrimSpace(*src.Evidence.Dir) != "" {
		dst.Evidence.Dir = strings.TrimSpace(*src.Evidence.Dir)
	}
}

// autoFixDefaults returns the default auto-fix configuration.
func autoFixDefaults() AutoFix {
	return AutoFix{
		Lint:     3,
		Test:     3,
		Review:   0,
		Document: 3,
		CI:       3,
		Refresh:  3,
	}
}

// applyAutoFixOverrides applies non-nil raw values onto resolved defaults.
func applyAutoFixOverrides(dst *AutoFix, src *AutoFixRaw) {
	if src.Lint != nil {
		dst.Lint = *src.Lint
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

	intent := intentDefaults()
	applyIntentOverrides(&intent, &global.Intent)
	applyIntentOverrides(&intent, &repo.Intent)

	test := testDefaults()
	applyTestOverrides(&test, &global.Test)
	applyTestOverrides(&test, &repo.Test)

	commit := Commit{FixMessage: DefaultFixMessageTemplate}
	if global.Commit.FixMessage != nil {
		commit.FixMessage = *global.Commit.FixMessage
	}
	if repo.Commit.FixMessage != nil {
		commit.FixMessage = *repo.Commit.FixMessage
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
		Hooks:                   repo.Hooks,
		IgnorePatterns:          repo.IgnorePatterns,
		AutoFix:                 af,
		Commit:                  commit,
		Intent:                  intent,
		Test:                    test,
		Document:                Document{Instructions: strings.TrimSpace(repo.Document.Instructions)},
		RefreshStrategy:         repo.Refresh.Strategy.OrDefault(),
		// repo is the EffectiveRepoConfig result, so this value is already
		// trusted-only (EffectiveRepoConfig sourced it from the trusted copy).
		DisableProjectSettings: repo.DisableProjectSettings,
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
