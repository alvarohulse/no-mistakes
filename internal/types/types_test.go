package types

import "testing"

func TestACPAliasFor(t *testing.T) {
	alias, ok := ACPAliasFor(AgentCursor)
	if !ok {
		t.Fatal("cursor should be registered as an ACP alias")
	}
	if alias.Target != "cursor" {
		t.Fatalf("target = %q, want cursor", alias.Target)
	}
	if alias.DefaultCommand != "cursor-agent acp" {
		t.Fatalf("default command = %q, want cursor-agent acp", alias.DefaultCommand)
	}
	if alias.DefaultCommandBinary() != "cursor-agent" {
		t.Fatalf("default command binary = %q, want cursor-agent", alias.DefaultCommandBinary())
	}

	aliases := ACPAliases()
	if len(aliases) != 1 {
		t.Fatalf("aliases = %v, want only cursor", aliases)
	}
	aliases[0].Target = "mutated"
	alias, _ = ACPAliasFor(AgentCursor)
	if alias.Target != "cursor" {
		t.Fatalf("ACPAliases should return a copy, target = %q", alias.Target)
	}
}
