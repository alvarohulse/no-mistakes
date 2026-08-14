package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const maxAgentObservations = 64

type agentObservationCollector struct {
	reported     bool
	observations []types.AgentObservation
	seen         map[string]struct{}
	count        int
}

func newAgentObservationCollector(reported bool) *agentObservationCollector {
	return &agentObservationCollector{
		reported: reported,
		seen:     make(map[string]struct{}),
	}
}

func (c *agentObservationCollector) observe(key, identity string) {
	if c == nil {
		return
	}
	identity = sanitizeAgentIdentity(identity)
	if identity == "" {
		return
	}
	if key != "" {
		if _, ok := c.seen[key]; ok {
			return
		}
		c.seen[key] = struct{}{}
	}
	c.count++
	if len(c.observations) >= maxAgentObservations {
		return
	}
	c.observations = append(c.observations, types.AgentObservation{
		Identity:       identity,
		InvocationMode: types.AgentInvocationModeSubagentTool,
	})
}

func (c *agentObservationCollector) uniqueCount() int {
	if c == nil {
		return 0
	}
	return c.count
}

func sanitizeAgentIdentity(identity string) string {
	identity = strings.TrimSpace(identity)
	if index := strings.IndexAny(identity, " \t\r\n"); index >= 0 {
		identity = identity[:index]
	}
	if len(identity) > 64 {
		identity = identity[:64]
	}
	var sanitized strings.Builder
	for _, char := range identity {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9',
			char == '-', char == '_', char == '.', char == ':', char == '/':
			sanitized.WriteRune(char)
		}
	}
	return sanitized.String()
}

func fingerprintAgentIdentity(prefix, identity string) string {
	if identity == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(identity))
	return prefix + ":" + hex.EncodeToString(sum[:8])
}
