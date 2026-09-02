package agent

import (
	"fmt"
	"testing"
)

func TestAgentObservationCollectorUpgradesIdentityForOneKey(t *testing.T) {
	collector := newAgentObservationCollector(true)
	collector.observe("child-1", "")
	collector.observe("child-1", "worker")
	collector.observe("child-1", "worker-v2")

	if collector.uniqueCount() != 1 {
		t.Fatalf("unique count = %d, want 1", collector.uniqueCount())
	}
	if len(collector.observations) != 1 {
		t.Fatalf("stored observations = %d, want 1", len(collector.observations))
	}
	if collector.observations[0].Identity != "worker-v2" {
		t.Fatalf("identity = %q, want upgraded worker-v2", collector.observations[0].Identity)
	}
}

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
