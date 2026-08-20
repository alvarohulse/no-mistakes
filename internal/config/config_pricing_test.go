package config

import (
	"strings"
	"testing"
)

func TestLegacyGlobalPricingProfilesAreAcceptedAndIgnored(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("pricing:\n  profiles:\n    cursor: retired-profile\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	_ = Merge(global, &RepoConfig{})

	if _, err := LoadRepoFromBytes([]byte("pricing:\n  profiles:\n    cursor: cursor-token-rate\n")); err == nil || !strings.Contains(err.Error(), "field pricing not found") {
		t.Fatalf("repo pricing error = %v, want global-only refusal", err)
	}
}
