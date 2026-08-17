package pricing

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const million = 1_000_000

//go:embed catalog.json
var defaultCatalogJSON []byte

//go:embed profiles.json
var defaultProfilesJSON []byte

type Catalog struct {
	Version int          `json:"version"`
	Models  []ModelPrice `json:"models"`
}

type ModelPrice struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	SourceURL      string `json:"source_url"`
	EffectiveFrom  string `json:"effective_from"`
	EffectiveUntil string `json:"effective_until,omitempty"`
	USDPerMillion  Rates  `json:"usd_per_million"`
}

type Rates struct {
	UncachedInputTokens float64 `json:"uncached_input_tokens"`
	CacheReadTokens     float64 `json:"cache_read_tokens"`
	CacheWriteTokens    float64 `json:"cache_write_tokens"`
	OutputTokens        float64 `json:"output_tokens"`
}

type ProfileCatalog struct {
	Version  int              `json:"version"`
	Profiles []HarnessProfile `json:"profiles"`
}

type HarnessProfile struct {
	ID             string     `json:"id"`
	Version        int        `json:"version"`
	Harness        string     `json:"harness"`
	Status         string     `json:"status"`
	SourceURL      string     `json:"source_url,omitempty"`
	EffectiveFrom  string     `json:"effective_from,omitempty"`
	EffectiveUntil string     `json:"effective_until,omitempty"`
	Adjustment     Adjustment `json:"adjustment"`
	ExcludedModels []string   `json:"excluded_models,omitempty"`
}

type Adjustment struct {
	Kind          string  `json:"kind,omitempty"`
	USDPerMillion float64 `json:"usd_per_million,omitempty"`
}

type TokenMeters struct {
	UncachedInputTokens *int64 `json:"uncached_input_tokens"`
	CacheReadTokens     *int64 `json:"cache_read_tokens"`
	CacheWriteTokens    *int64 `json:"cache_write_tokens"`
	OutputTokens        *int64 `json:"output_tokens"`
}

type Observation struct {
	Harness         string
	Provider        string
	Model           string
	StartedAt       time.Time
	ReportedCostUSD *float64
	Meters          TokenMeters
}

type Coverage struct {
	Reported int `json:"reported"`
	Eligible int `json:"eligible"`
}

type Provenance struct {
	CatalogVersion        int    `json:"catalog_version,omitempty"`
	CatalogSHA256         string `json:"catalog_sha256,omitempty"`
	PriceSourceURL        string `json:"price_source_url,omitempty"`
	PriceEffectiveFrom    string `json:"price_effective_from,omitempty"`
	PriceEffectiveUntil   string `json:"price_effective_until,omitempty"`
	ProfileCatalogVersion int    `json:"profile_catalog_version,omitempty"`
	ProfileCatalogSHA256  string `json:"profile_catalog_sha256,omitempty"`
	ProfileID             string `json:"profile_id,omitempty"`
	ProfileVersion        int    `json:"profile_version,omitempty"`
	ProfileSourceURL      string `json:"profile_source_url,omitempty"`
	ProfileEffectiveFrom  string `json:"profile_effective_from,omitempty"`
	ProfileEffectiveUntil string `json:"profile_effective_until,omitempty"`
	ProfileStatus         string `json:"profile_status,omitempty"`
	AdjustmentKind        string `json:"adjustment_kind,omitempty"`
}

type CostEstimate struct {
	ValueUSD   *float64   `json:"value_usd"`
	Coverage   Coverage   `json:"coverage"`
	Complete   bool       `json:"complete"`
	Basis      string     `json:"basis"`
	Reason     string     `json:"reason,omitempty"`
	Provenance Provenance `json:"provenance"`
}

type CostClasses struct {
	HarnessReported         CostEstimate `json:"harness_reported"`
	APIListEstimate         CostEstimate `json:"api_list_estimate"`
	HarnessAdjustedEstimate CostEstimate `json:"harness_adjusted_estimate"`
}

type Estimator struct {
	catalog      Catalog
	profiles     ProfileCatalog
	catalogHash  string
	profilesHash string
}

func DefaultEstimator() (*Estimator, error) {
	var catalog Catalog
	if err := json.Unmarshal(defaultCatalogJSON, &catalog); err != nil {
		return nil, fmt.Errorf("decode embedded pricing catalog: %w", err)
	}
	var profiles ProfileCatalog
	if err := json.Unmarshal(defaultProfilesJSON, &profiles); err != nil {
		return nil, fmt.Errorf("decode embedded harness profiles: %w", err)
	}
	return NewEstimator(catalog, profiles)
}

func NewEstimator(catalog Catalog, profiles ProfileCatalog) (*Estimator, error) {
	if err := validateCatalog(catalog); err != nil {
		return nil, err
	}
	if err := validateProfiles(profiles); err != nil {
		return nil, err
	}
	catalogJSON, err := json.Marshal(catalog)
	if err != nil {
		return nil, fmt.Errorf("encode pricing catalog: %w", err)
	}
	profilesJSON, err := json.Marshal(profiles)
	if err != nil {
		return nil, fmt.Errorf("encode harness profiles: %w", err)
	}
	return &Estimator{
		catalog: catalog, profiles: profiles,
		catalogHash: contentHash(catalogJSON), profilesHash: contentHash(profilesJSON),
	}, nil
}

func (e *Estimator) Estimate(observation Observation) CostClasses {
	reported := reportedEstimate(observation.ReportedCostUSD)
	list, modelPrice := e.listEstimate(observation)
	effective := e.effectiveEstimate(observation, list, modelPrice)
	return CostClasses{HarnessReported: reported, APIListEstimate: list, HarnessAdjustedEstimate: effective}
}

func reportedEstimate(value *float64) CostEstimate {
	result := CostEstimate{Coverage: Coverage{Eligible: 1}, Basis: "agent_invocations.reported_cost_usd"}
	if value == nil {
		result.Reason = "not_reported"
		return result
	}
	if math.IsNaN(*value) || math.IsInf(*value, 0) || *value < 0 {
		result.Reason = "invalid_reported_cost"
		return result
	}
	result.ValueUSD = float64Ptr(*value)
	result.Coverage.Reported = 1
	result.Complete = true
	return result
}

func (e *Estimator) listEstimate(observation Observation) (CostEstimate, *ModelPrice) {
	coverage, known := meterCoverage(observation.Meters)
	result := CostEstimate{
		Coverage: coverage, Complete: coverage.Reported == coverage.Eligible,
		Basis:      "canonical_delta_token_meters_x_public_list_rate",
		Provenance: Provenance{CatalogVersion: e.catalog.Version, CatalogSHA256: e.catalogHash},
	}
	price := e.findModelPrice(observation.Provider, observation.Model, observation.StartedAt)
	if price == nil {
		result.Reason = "no_catalog_entry"
		return result, nil
	}
	result.Provenance.PriceSourceURL = price.SourceURL
	result.Provenance.PriceEffectiveFrom = price.EffectiveFrom
	result.Provenance.PriceEffectiveUntil = price.EffectiveUntil
	if known == 0 {
		result.Reason = "missing_required_meter"
		return result, price
	}
	amount := priceMeters(observation.Meters, price.USDPerMillion)
	result.ValueUSD = float64Ptr(amount)
	if !result.Complete {
		result.Reason = "missing_required_meter"
	}
	return result, price
}

func (e *Estimator) effectiveEstimate(observation Observation, list CostEstimate, price *ModelPrice) CostEstimate {
	result := CostEstimate{
		Coverage: list.Coverage, Complete: list.Complete,
		Basis:      "public_list_estimate_plus_harness_profile",
		Provenance: list.Provenance,
	}
	if list.ValueUSD == nil || price == nil {
		result.Reason = list.Reason
		return result
	}
	profile := e.findProfile(observation.Harness, observation.StartedAt)
	if profile == nil {
		result.Reason = "no_applicable_billing_profile"
		return result
	}
	result.Provenance.ProfileCatalogVersion = e.profiles.Version
	result.Provenance.ProfileCatalogSHA256 = e.profilesHash
	result.Provenance.ProfileID = profile.ID
	result.Provenance.ProfileVersion = profile.Version
	result.Provenance.ProfileSourceURL = profile.SourceURL
	result.Provenance.ProfileEffectiveFrom = profile.EffectiveFrom
	result.Provenance.ProfileEffectiveUntil = profile.EffectiveUntil
	result.Provenance.ProfileStatus = profile.Status
	result.Provenance.AdjustmentKind = profile.Adjustment.Kind
	if profile.Status != "active" {
		result.Reason = "inactive_profile"
		return result
	}
	if modelExcluded(observation.Model, profile.ExcludedModels) {
		result.ValueUSD = float64Ptr(*list.ValueUSD)
		result.Provenance.ProfileStatus = "excluded"
		result.Reason = list.Reason
		return result
	}
	if profile.Adjustment.Kind != "additive_total_tokens_usd_per_million" {
		result.Reason = "unsupported_profile_adjustment"
		return result
	}
	totalTokens := knownMeterTotal(observation.Meters)
	result.ValueUSD = float64Ptr(*list.ValueUSD + float64(totalTokens)*profile.Adjustment.USDPerMillion/million)
	result.Reason = list.Reason
	return result
}

func (e *Estimator) findModelPrice(provider, model string, at time.Time) *ModelPrice {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	for i := range e.catalog.Models {
		entry := &e.catalog.Models[i]
		if entry.Provider == provider && entry.Model == model && withinWindow(at, entry.EffectiveFrom, entry.EffectiveUntil) {
			return entry
		}
	}
	return nil
}

func (e *Estimator) findProfile(harness string, at time.Time) *HarnessProfile {
	harness = strings.TrimSpace(harness)
	for i := range e.profiles.Profiles {
		profile := &e.profiles.Profiles[i]
		if profile.Harness == harness && withinWindow(at, profile.EffectiveFrom, profile.EffectiveUntil) {
			return profile
		}
	}
	return nil
}

func validateCatalog(catalog Catalog) error {
	if catalog.Version != 1 {
		return fmt.Errorf("pricing catalog: unsupported version %d", catalog.Version)
	}
	if len(catalog.Models) == 0 {
		return fmt.Errorf("pricing catalog: models must not be empty")
	}
	seen := make(map[string]bool, len(catalog.Models))
	for _, model := range catalog.Models {
		if strings.TrimSpace(model.Provider) == "" || strings.TrimSpace(model.Model) == "" || strings.TrimSpace(model.SourceURL) == "" {
			return fmt.Errorf("pricing catalog: each model needs provider, model, and source_url")
		}
		if err := validateWindow(model.EffectiveFrom, model.EffectiveUntil); err != nil {
			return fmt.Errorf("pricing catalog %s/%s: %w", model.Provider, model.Model, err)
		}
		key := model.Provider + "\x00" + model.Model
		if seen[key] {
			return fmt.Errorf("pricing catalog: duplicate model %s/%s", model.Provider, model.Model)
		}
		seen[key] = true
		for name, value := range map[string]float64{
			"uncached_input_tokens": model.USDPerMillion.UncachedInputTokens,
			"cache_read_tokens":     model.USDPerMillion.CacheReadTokens,
			"cache_write_tokens":    model.USDPerMillion.CacheWriteTokens,
			"output_tokens":         model.USDPerMillion.OutputTokens,
		} {
			if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
				return fmt.Errorf("pricing catalog %s/%s: %s must be non-negative", model.Provider, model.Model, name)
			}
		}
	}
	return nil
}

func validateProfiles(catalog ProfileCatalog) error {
	if catalog.Version != 1 {
		return fmt.Errorf("harness profiles: unsupported version %d", catalog.Version)
	}
	seen := make(map[string]bool, len(catalog.Profiles))
	for _, profile := range catalog.Profiles {
		if profile.ID == "" || profile.Version <= 0 || profile.Harness == "" {
			return fmt.Errorf("harness profiles: each profile needs id, version, and harness")
		}
		if profile.Status != "active" && profile.Status != "inactive" {
			return fmt.Errorf("harness profile %s: status must be active or inactive", profile.ID)
		}
		if profile.Status == "active" {
			if profile.SourceURL == "" {
				return fmt.Errorf("harness profile %s: active profile needs source_url", profile.ID)
			}
			if profile.Adjustment.Kind != "additive_total_tokens_usd_per_million" || profile.Adjustment.USDPerMillion < 0 {
				return fmt.Errorf("harness profile %s: invalid adjustment", profile.ID)
			}
		}
		if err := validateWindow(profile.EffectiveFrom, profile.EffectiveUntil); err != nil {
			return fmt.Errorf("harness profile %s: %w", profile.ID, err)
		}
		key := profile.Harness + "\x00" + profile.ID
		if seen[key] {
			return fmt.Errorf("harness profiles: duplicate %s", profile.ID)
		}
		seen[key] = true
	}
	return nil
}

func validateWindow(from, until string) error {
	if from == "" {
		if until != "" {
			return fmt.Errorf("effective_until requires effective_from")
		}
		return nil
	}
	start, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return fmt.Errorf("invalid effective_from: %w", err)
	}
	if until == "" {
		return nil
	}
	end, err := time.Parse(time.DateOnly, until)
	if err != nil {
		return fmt.Errorf("invalid effective_until: %w", err)
	}
	if end.Before(start) {
		return fmt.Errorf("effective window is reversed")
	}
	return nil
}

func withinWindow(at time.Time, from, until string) bool {
	if from == "" {
		return until == ""
	}
	if at.IsZero() {
		return false
	}
	day := at.UTC().Format(time.DateOnly)
	return day >= from && (until == "" || day <= until)
}

func meterCoverage(meters TokenMeters) (Coverage, int) {
	values := []*int64{meters.UncachedInputTokens, meters.CacheReadTokens, meters.CacheWriteTokens, meters.OutputTokens}
	reported := 0
	for _, value := range values {
		if value != nil && *value >= 0 {
			reported++
		}
	}
	return Coverage{Reported: reported, Eligible: len(values)}, reported
}

func priceMeters(meters TokenMeters, rates Rates) float64 {
	var amount float64
	for _, meter := range []struct {
		value *int64
		rate  float64
	}{
		{meters.UncachedInputTokens, rates.UncachedInputTokens},
		{meters.CacheReadTokens, rates.CacheReadTokens},
		{meters.CacheWriteTokens, rates.CacheWriteTokens},
		{meters.OutputTokens, rates.OutputTokens},
	} {
		if meter.value != nil && *meter.value >= 0 {
			amount += float64(*meter.value) * meter.rate / million
		}
	}
	return amount
}

func knownMeterTotal(meters TokenMeters) int64 {
	var total int64
	for _, value := range []*int64{meters.UncachedInputTokens, meters.CacheReadTokens, meters.CacheWriteTokens, meters.OutputTokens} {
		if value != nil && *value >= 0 {
			total += *value
		}
	}
	return total
}

func modelExcluded(model string, excluded []string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	for _, candidate := range excluded {
		if model == strings.ToLower(strings.TrimSpace(candidate)) {
			return true
		}
	}
	return false
}

func contentHash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func float64Ptr(value float64) *float64 { return &value }
