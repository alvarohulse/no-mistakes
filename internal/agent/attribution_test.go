package agent

import (
	"fmt"
	"testing"
)

func TestAgentObservationCollectorCountsBeyondBoundedIdentities(t *testing.T) {
	collector := newAgentObservationCollector(true)
	for i := 0; i < maxAgentObservations+6; i++ {
		collector.observe(fmt.Sprintf("child-%d", i), fmt.Sprintf("agent-%d", i))
	}
	collector.observe("child-1", "duplicate")

	if len(collector.observations) != maxAgentObservations {
		t.Fatalf("stored observations = %d, want bounded %d", len(collector.observations), maxAgentObservations)
	}
	if collector.uniqueCount() != maxAgentObservations+6 {
		t.Fatalf("unique count = %d, want %d", collector.uniqueCount(), maxAgentObservations+6)
	}
}
