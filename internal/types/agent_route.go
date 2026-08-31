package types

import (
	"fmt"
	"strings"
	"unicode"
)

var agentEffortValues = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// AgentEffortValues returns the common reasoning-effort vocabulary in
// ascending depth order.
func AgentEffortValues() []string {
	return append([]string(nil), agentEffortValues...)
}

// ValidateAgentEffort checks the common effort vocabulary independently of a
// harness. ValidateAgentRoute additionally checks whether that harness can
// express it.
func ValidateAgentEffort(effort string) error {
	if effort == "" {
		return nil
	}
	for _, supported := range agentEffortValues {
		if effort == supported {
			return nil
		}
	}
	return fmt.Errorf("invalid effort %q (valid: %s)", effort, strings.Join(agentEffortValues, ", "))
}

// ValidateModelIdentity checks the canonical model/vendor pair shared by
// configuration routes and eval candidates. The pair is optional, but never
// partial.
func ValidateModelIdentity(model, vendor string) error {
	if model == "" && vendor == "" {
		return nil
	}
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("model.name is required when model is configured")
	}
	if model != strings.TrimSpace(model) || strings.IndexFunc(model, unicode.IsControl) >= 0 {
		return fmt.Errorf("model.name must not contain surrounding whitespace or control characters")
	}
	if vendor == "" {
		return fmt.Errorf("model.vendor is required when model is configured")
	}
	if vendor != strings.ToLower(vendor) || !validModelVendor(vendor) {
		return fmt.Errorf("model.vendor %q must be a lowercase identifier containing only letters, digits, and hyphens", vendor)
	}
	return nil
}

func validModelVendor(vendor string) bool {
	for i, r := range vendor {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' && i > 0 && i < len(vendor)-1 {
			continue
		}
		return false
	}
	return vendor != ""
}

// ValidateAgentRoute is the single compatibility contract for pipeline and
// eval agent/model/vendor/effort routes. A zero model route remains valid for
// existing pipeline defaults.
func ValidateAgentRoute(name AgentName, model, vendor, effort string) error {
	if err := ValidateAgentEffort(effort); err != nil {
		return err
	}
	if err := ValidateModelIdentity(model, vendor); err != nil {
		return err
	}

	if _, ok := ACPTargetFor(name); ok {
		if model != "" && !IsBareACPModelName(model) {
			return fmt.Errorf("parameterized or malformed bracketed model %q is not supported for ACP agent %q; configure a bare model family", model, name)
		}
		if effort != "" {
			return fmt.Errorf("agent %q cannot express effort; acpx exposes no verified effort mechanism", name)
		}
		return nil
	}

	switch name {
	case AgentClaude:
		if model != "" && (vendor != "anthropic" || strings.Contains(model, "/")) {
			return incompatibleAgentModelError(name, model, vendor)
		}
	case AgentCodex:
		if model != "" && (vendor != "openai" || strings.Contains(model, "/")) {
			return incompatibleAgentModelError(name, model, vendor)
		}
	case AgentCursor:
		if effort != "" {
			if model == "" {
				return fmt.Errorf("agent %q requires an explicit model to encode effort", name)
			}
			if strings.ContainsAny(model, "[]") {
				return fmt.Errorf("agent %q model %q is already parameterized; specify effort only once in the model or the explicit effort field", name, model)
			}
		}
	case AgentPi, AgentCopilot:
		// These routing-capable harnesses can serve models from multiple vendors.
	case AgentOpenCode:
		if model != "" {
			provider, id, ok := strings.Cut(model, "/")
			if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(id) == "" {
				return fmt.Errorf("agent %q requires model %q to use provider/model form", name, model)
			}
		}
	case AgentRovoDev:
		if model != "" {
			return fmt.Errorf("model %q is not supported for agent %q because Rovo Dev exposes no verified model-selection interface", model, name)
		}
		if effort != "" {
			return fmt.Errorf("effort %q is not supported for agent %q because Rovo Dev exposes no verified effort-selection interface", effort, name)
		}
	default:
		return fmt.Errorf("unknown agent %q; valid options: auto, claude, codex, rovodev, opencode, pi, copilot, cursor, acp:<target> (set 'agent' in ~/.no-mistakes/config.yaml)", name)
	}
	return nil
}

func incompatibleAgentModelError(name AgentName, model, vendor string) error {
	return fmt.Errorf("agent %q cannot serve model %q from declared vendor %q", name, model, vendor)
}
