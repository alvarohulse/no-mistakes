package agent

import (
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

// ParseEffort validates the common effort vocabulary. Individual harnesses
// remain responsible for reporting a value their installed version rejects.
func ParseEffort(raw string) (Effort, error) {
	value := strings.TrimSpace(raw)
	if err := types.ValidateAgentEffort(value); err != nil {
		return "", err
	}
	return Effort(value), nil
}
