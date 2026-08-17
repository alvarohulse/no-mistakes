package config

import (
	"strings"
	"testing"
)

func TestPricingProfilesAreGlobalOnlyAndSurviveMerge(t *testing.T) {
	global, err := LoadGlobalFromBytes([]byte("pricing:\n  profiles:\n    cursor: cursor-token-rate\n"))
	if err != nil {
		t.Fatalf("LoadGlobalFromBytes: %v", err)
	}
	if got := global.PricingProfiles["cursor"]; got != "cursor-token-rate" {
		t.Fatalf("global cursor profile = %q", got)
	}
	merged := Merge(global, &RepoConfig{})
	if got := merged.PricingProfiles["cursor"]; got != "cursor-token-rate" {
		t.Fatalf("merged cursor profile = %q", got)
	}

	if _, err := LoadRepoFromBytes([]byte("pricing:\n  profiles:\n    cursor: cursor-token-rate\n")); err == nil || !strings.Contains(err.Error(), "field pricing not found") {
		t.Fatalf("repo pricing error = %v, want global-only refusal", err)
	}
}

func TestGlobalPricingProfilesRejectUnknownProfile(t *testing.T) {
	_, err := LoadGlobalFromBytes([]byte("pricing:\n  profiles:\n    cursor: made-up-profile\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown pricing profile") {
		t.Fatalf("LoadGlobalFromBytes error = %v, want unknown-profile refusal", err)
	}
}
