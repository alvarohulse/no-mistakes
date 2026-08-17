package pricing

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestEstimatorKeepsThreeCostClassesIndependent(t *testing.T) {
	estimator := testEstimator(t)
	reported := 9.25
	result := estimator.Estimate(Observation{
		Harness:         "cursor",
		ProfileID:       "cursor-token-rate",
		Provider:        "anthropic",
		Model:           "claude-opus-5",
		StartedAt:       time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		ReportedCostUSD: &reported,
		Meters: TokenMeters{
			UncachedInputTokens: int64Ptr(1_000_000),
			CacheReadTokens:     int64Ptr(1_000_000),
			CacheWriteTokens:    int64Ptr(1_000_000),
			OutputTokens:        int64Ptr(1_000_000),
		},
	})

	assertCost(t, result.HarnessReported, 9.25, true, 1, 1)
	assertCost(t, result.APIListEstimate, 36.75, true, 4, 4)
	assertCost(t, result.HarnessAdjustedEstimate, 37.75, true, 4, 4)
	if result.HarnessAdjustedEstimate.Provenance.ProfileID != "cursor-token-rate" {
		t.Fatalf("profile = %q, want cursor-token-rate", result.HarnessAdjustedEstimate.Provenance.ProfileID)
	}
}

func TestEstimatorReportsKnownMeterFloorWithoutInventingMissingUsage(t *testing.T) {
	estimator := testEstimator(t)
	result := estimator.Estimate(Observation{
		Harness:   "cursor",
		ProfileID: "cursor-token-rate",
		Provider:  "anthropic",
		Model:     "claude-opus-5",
		StartedAt: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		Meters: TokenMeters{
			OutputTokens: int64Ptr(1_000_000),
		},
	})

	assertUnknownCost(t, result.HarnessReported, "not_reported", 0, 1)
	assertCost(t, result.APIListEstimate, 25, false, 1, 4)
	assertCost(t, result.HarnessAdjustedEstimate, 25.25, false, 1, 4)
}

func TestEstimatorLeavesUnknownCatalogAndInactivePrivateProfilesNull(t *testing.T) {
	estimator := testEstimator(t)
	unknown := estimator.Estimate(Observation{
		Harness:   "cursor",
		Provider:  "openai",
		Model:     "gpt-5.6-sol",
		StartedAt: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		Meters:    TokenMeters{OutputTokens: int64Ptr(100)},
	})
	assertUnknownCost(t, unknown.APIListEstimate, "no_catalog_entry", 1, 4)
	assertUnknownCost(t, unknown.HarnessAdjustedEstimate, "no_catalog_entry", 1, 4)

	private := estimator.Estimate(Observation{
		Harness:   "claude",
		ProfileID: "claude-code-private",
		Provider:  "anthropic",
		Model:     "claude-opus-5",
		StartedAt: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		Meters: TokenMeters{
			UncachedInputTokens: int64Ptr(100),
			CacheReadTokens:     int64Ptr(0),
			CacheWriteTokens:    int64Ptr(0),
			OutputTokens:        int64Ptr(100),
		},
	})
	if private.APIListEstimate.ValueUSD == nil {
		t.Fatal("public list estimate = nil, want independent estimate")
	}
	assertUnknownCost(t, private.HarnessAdjustedEstimate, "inactive_profile", 4, 4)
}

func TestCursorAdjustmentRequiresAnExplicitProfileSelection(t *testing.T) {
	estimator := testEstimator(t)
	result := estimator.Estimate(Observation{
		Harness:   "cursor",
		Provider:  "anthropic",
		Model:     "claude-opus-5",
		StartedAt: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		Meters: TokenMeters{
			UncachedInputTokens: int64Ptr(100),
			CacheReadTokens:     int64Ptr(0),
			CacheWriteTokens:    int64Ptr(0),
			OutputTokens:        int64Ptr(100),
		},
	})

	if result.APIListEstimate.ValueUSD == nil {
		t.Fatal("public list estimate = nil, want independent estimate")
	}
	assertUnknownCost(t, result.HarnessAdjustedEstimate, "no_applicable_billing_profile", 4, 4)
}

func TestCursorFirstPartyExclusionUsesListEstimateWithoutSurcharge(t *testing.T) {
	estimator := testEstimator(t)
	result := estimator.Estimate(Observation{
		Harness:   "cursor",
		ProfileID: "cursor-token-rate",
		Provider:  "xai",
		Model:     "grok-4.6",
		StartedAt: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
		Meters: TokenMeters{
			UncachedInputTokens: int64Ptr(1_000_000),
			CacheReadTokens:     int64Ptr(0),
			CacheWriteTokens:    int64Ptr(0),
			OutputTokens:        int64Ptr(1_000_000),
		},
	})

	assertCost(t, result.APIListEstimate, 2, true, 4, 4)
	assertCost(t, result.HarnessAdjustedEstimate, 2, true, 4, 4)
	if result.HarnessAdjustedEstimate.Provenance.ProfileStatus != "excluded" {
		t.Fatalf("profile status = %q, want excluded", result.HarnessAdjustedEstimate.Provenance.ProfileStatus)
	}
}

func TestEstimatorSupportsNonOverlappingCatalogAndProfileWindows(t *testing.T) {
	estimator, err := NewEstimator(Catalog{
		Version: 2,
		Models: []ModelPrice{
			{
				Provider: "anthropic", Model: "claude-opus-5", SourceURL: "https://example.com/old", EffectiveFrom: "2026-08-01", EffectiveUntil: "2026-08-31",
				USDPerMillion: Rates{OutputTokens: 10},
			},
			{
				Provider: "anthropic", Model: "claude-opus-5", SourceURL: "https://example.com/new", EffectiveFrom: "2026-09-01",
				USDPerMillion: Rates{OutputTokens: 20},
			},
		},
	}, ProfileCatalog{
		Version: 2,
		Profiles: []HarnessProfile{
			{
				ID: "cursor-token-rate", Version: 1, Harness: "cursor", Status: "active", SourceURL: "https://example.com/profile-old", EffectiveFrom: "2026-08-01", EffectiveUntil: "2026-08-31",
				Adjustment: Adjustment{Kind: "additive_total_tokens_usd_per_million", USDPerMillion: 1},
			},
			{
				ID: "cursor-token-rate", Version: 2, Harness: "cursor", Status: "active", SourceURL: "https://example.com/profile-new", EffectiveFrom: "2026-09-01",
				Adjustment: Adjustment{Kind: "additive_total_tokens_usd_per_million", USDPerMillion: 2},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation := Observation{
		Harness: "cursor", ProfileID: "cursor-token-rate", Provider: "anthropic", Model: "claude-opus-5",
		Meters: TokenMeters{UncachedInputTokens: int64Ptr(0), CacheReadTokens: int64Ptr(0), CacheWriteTokens: int64Ptr(0), OutputTokens: int64Ptr(1_000_000)},
	}
	observation.StartedAt = time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	old := estimator.Estimate(observation)
	assertCost(t, old.APIListEstimate, 10, true, 4, 4)
	assertCost(t, old.HarnessAdjustedEstimate, 11, true, 4, 4)
	if old.HarnessAdjustedEstimate.Provenance.ProfileVersion != 1 {
		t.Fatalf("old profile version = %d", old.HarnessAdjustedEstimate.Provenance.ProfileVersion)
	}

	observation.StartedAt = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	current := estimator.Estimate(observation)
	assertCost(t, current.APIListEstimate, 20, true, 4, 4)
	assertCost(t, current.HarnessAdjustedEstimate, 22, true, 4, 4)
	if current.HarnessAdjustedEstimate.Provenance.ProfileVersion != 2 {
		t.Fatalf("current profile version = %d", current.HarnessAdjustedEstimate.Provenance.ProfileVersion)
	}
}

func TestEstimatorRejectsOverlappingEffectiveWindows(t *testing.T) {
	_, err := NewEstimator(Catalog{
		Version: 2,
		Models: []ModelPrice{
			{Provider: "anthropic", Model: "claude-opus-5", SourceURL: "https://example.com/old", EffectiveFrom: "2026-08-01", EffectiveUntil: "2026-09-01"},
			{Provider: "anthropic", Model: "claude-opus-5", SourceURL: "https://example.com/new", EffectiveFrom: "2026-09-01"},
		},
	}, ProfileCatalog{Version: 1})
	if err == nil || !strings.Contains(err.Error(), "overlapping effective windows") {
		t.Fatalf("NewEstimator() error = %v, want overlap refusal", err)
	}
}

func TestProfileSelectionPrefersDatedActivationOverInactiveFallback(t *testing.T) {
	estimator, err := NewEstimator(Catalog{
		Version: 2,
		Models: []ModelPrice{{
			Provider: "anthropic", Model: "claude-opus-5", SourceURL: "https://example.com/model", EffectiveFrom: "2026-01-01",
			USDPerMillion: Rates{OutputTokens: 10},
		}},
	}, ProfileCatalog{
		Version: 2,
		Profiles: []HarnessProfile{
			{ID: "claude-code-private", Version: 1, Harness: "claude", Status: "inactive"},
			{
				ID: "claude-code-private", Version: 2, Harness: "claude", Status: "active", SourceURL: "https://example.com/active", EffectiveFrom: "2027-01-01",
				Adjustment: Adjustment{Kind: "additive_total_tokens_usd_per_million", USDPerMillion: 2},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	observation := Observation{
		Harness: "claude", ProfileID: "claude-code-private", Provider: "anthropic", Model: "claude-opus-5",
		Meters: TokenMeters{UncachedInputTokens: int64Ptr(0), CacheReadTokens: int64Ptr(0), CacheWriteTokens: int64Ptr(0), OutputTokens: int64Ptr(1_000_000)},
	}
	observation.StartedAt = time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)
	before := estimator.Estimate(observation).HarnessAdjustedEstimate
	assertUnknownCost(t, before, "inactive_profile", 4, 4)
	if before.Provenance.ProfileVersion != 1 || before.Provenance.ProfileStatus != "inactive" {
		t.Fatalf("pre-window fallback provenance = %+v", before.Provenance)
	}

	observation.StartedAt = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	after := estimator.Estimate(observation).HarnessAdjustedEstimate
	assertCost(t, after, 12, true, 4, 4)
	if after.Provenance.ProfileVersion != 2 || after.Provenance.ProfileStatus != "active" {
		t.Fatalf("dated activation provenance = %+v", after.Provenance)
	}
}

func TestProfileValidationRejectsAmbiguousFallbacksAndDatedWindows(t *testing.T) {
	catalog := Catalog{Version: 1, Models: []ModelPrice{{Provider: "anthropic", Model: "claude-opus-5", SourceURL: "https://example.com/model", EffectiveFrom: "2026-01-01"}}}
	_, err := NewEstimator(catalog, ProfileCatalog{Version: 2, Profiles: []HarnessProfile{
		{ID: "claude-code-private", Version: 1, Harness: "claude", Status: "inactive"},
		{ID: "claude-code-private", Version: 2, Harness: "claude", Status: "inactive"},
	}})
	if err == nil || !strings.Contains(err.Error(), "multiple timeless fallbacks") {
		t.Fatalf("duplicate fallback error = %v", err)
	}

	_, err = NewEstimator(catalog, ProfileCatalog{Version: 2, Profiles: []HarnessProfile{
		{
			ID: "claude-code-private", Version: 1, Harness: "claude", Status: "active", SourceURL: "https://example.com/v1", EffectiveFrom: "2026-01-01", EffectiveUntil: "2027-01-01",
			Adjustment: Adjustment{Kind: "additive_total_tokens_usd_per_million"},
		},
		{
			ID: "claude-code-private", Version: 2, Harness: "claude", Status: "active", SourceURL: "https://example.com/v2", EffectiveFrom: "2027-01-01",
			Adjustment: Adjustment{Kind: "additive_total_tokens_usd_per_million"},
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "overlapping effective windows") {
		t.Fatalf("overlapping dated-window error = %v", err)
	}

	_, err = NewEstimator(catalog, ProfileCatalog{Version: 1, Profiles: []HarnessProfile{{
		ID: "claude-code-private", Version: 1, Harness: "claude", Status: "active", SourceURL: "https://example.com/timeless",
		Adjustment: Adjustment{Kind: "additive_total_tokens_usd_per_million"},
	}}})
	if err == nil || !strings.Contains(err.Error(), "timeless profile must be inactive") {
		t.Fatalf("active timeless error = %v", err)
	}
}

func testEstimator(t *testing.T) *Estimator {
	t.Helper()
	estimator, err := NewEstimator(Catalog{
		Version: 1,
		Models: []ModelPrice{
			{
				Provider: "anthropic", Model: "claude-opus-5", SourceURL: "https://www.anthropic.com/pricing",
				EffectiveFrom: "2026-08-15",
				USDPerMillion: Rates{UncachedInputTokens: 5, CacheReadTokens: 0.5, CacheWriteTokens: 6.25, OutputTokens: 25},
			},
			{
				Provider: "xai", Model: "grok-4.6", SourceURL: "https://example.invalid/test-only",
				EffectiveFrom: "2026-08-15",
				USDPerMillion: Rates{UncachedInputTokens: 1, CacheReadTokens: 0, CacheWriteTokens: 0, OutputTokens: 1},
			},
		},
	}, ProfileCatalog{
		Version: 1,
		Profiles: []HarnessProfile{
			{
				ID: "cursor-token-rate", Version: 1, Harness: "cursor", Status: "active",
				SourceURL: "https://cursor.com/docs/models-and-pricing", EffectiveFrom: "2026-08-15",
				Adjustment:     Adjustment{Kind: "additive_total_tokens_usd_per_million", USDPerMillion: 0.25},
				ExcludedModels: []string{"auto", "grok-4.6", "grok-4.5", "composer-2.5"},
			},
			{ID: "claude-code-private", Version: 1, Harness: "claude", Status: "inactive"},
			{ID: "codex-azure-private", Version: 1, Harness: "codex", Status: "inactive"},
		},
	})
	if err != nil {
		t.Fatalf("NewEstimator: %v", err)
	}
	return estimator
}

func assertCost(t *testing.T, got CostEstimate, want float64, complete bool, reported, eligible int) {
	t.Helper()
	if got.ValueUSD == nil || math.Abs(*got.ValueUSD-want) > 1e-9 {
		t.Fatalf("cost = %v, want %.6f", got.ValueUSD, want)
	}
	if got.Complete != complete || got.Coverage.Reported != reported || got.Coverage.Eligible != eligible {
		t.Fatalf("coverage = %+v complete=%v, want %d/%d complete=%v", got.Coverage, got.Complete, reported, eligible, complete)
	}
}

func assertUnknownCost(t *testing.T, got CostEstimate, reason string, reported, eligible int) {
	t.Helper()
	if got.ValueUSD != nil {
		t.Fatalf("cost = %v, want nil", *got.ValueUSD)
	}
	if got.Reason != reason || got.Coverage.Reported != reported || got.Coverage.Eligible != eligible {
		t.Fatalf("unknown = %+v, want reason %q coverage %d/%d", got, reason, reported, eligible)
	}
}

func int64Ptr(value int64) *int64 { return &value }
