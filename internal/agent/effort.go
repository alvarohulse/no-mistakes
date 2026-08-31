package agent

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Effort is the harness-neutral reasoning-depth selection used by typed agent
// routes. Adapters translate it to their verified native mechanism.
type Effort string

const (
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
	EffortXHigh   Effort = "xhigh"
	EffortMax     Effort = "max"
)

var supportedEfforts = []Effort{EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}

// ParseEffort validates the common effort vocabulary. Individual harnesses
// remain responsible for reporting a value their installed version rejects.
func ParseEffort(raw string) (Effort, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	for _, effort := range supportedEfforts {
		if value == string(effort) {
			return effort, nil
		}
	}
	values := make([]string, 0, len(supportedEfforts))
	for _, effort := range supportedEfforts {
		values = append(values, string(effort))
	}
	return "", fmt.Errorf("invalid effort %q (valid: %s)", raw, strings.Join(values, ", "))
}

// ValidateModelEffort is the shared fail-closed routing contract used by the
// pipeline agent constructor and eval candidates. It rejects a selection that
// no-mistakes cannot prove the named harness will receive.
func ValidateModelEffort(name types.AgentName, model string, effort Effort) error {
	if _, err := ParseEffort(string(effort)); err != nil {
		return err
	}
	if _, ok := types.ACPTargetFor(name); ok {
		if model != "" && !types.IsBareACPModelName(model) {
			return fmt.Errorf("parameterized or malformed bracketed model %q is not supported for ACP agent %q; configure a bare model family", model, name)
		}
		if effort != "" {
			return fmt.Errorf("agent %q cannot express effort; acpx exposes no verified effort mechanism", name)
		}
		return nil
	}

	switch name {
	case types.AgentClaude, types.AgentCodex, types.AgentCursor, types.AgentPi, types.AgentCopilot:
		return nil
	case types.AgentOpenCode:
		if model != "" {
			provider, id, ok := strings.Cut(model, "/")
			if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(id) == "" {
				return fmt.Errorf("opencode model %q must use provider/model form", model)
			}
		}
		return nil
	case types.AgentRovoDev:
		if model != "" {
			return fmt.Errorf("model %q is not supported for agent %q because Rovo Dev exposes no verified model-selection interface", model, name)
		}
		if effort != "" {
			return fmt.Errorf("effort %q is not supported for agent %q because Rovo Dev exposes no verified effort-selection interface", effort, name)
		}
		return nil
	default:
		return fmt.Errorf("unknown agent %q; valid options: auto, claude, codex, rovodev, opencode, pi, copilot, cursor, acp:<target> (set 'agent' in ~/.no-mistakes/config.yaml)", name)
	}
}
