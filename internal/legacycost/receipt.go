// Package legacycost decodes immutable cost receipts written by no-mistakes
// before pricing ownership moved to the external PR formatter.
//
// These types preserve the historical JSON shape only. This package contains
// no catalog, rates, profiles, or estimation logic.
package legacycost

import (
	"encoding/json"
	"fmt"
)

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

func DecodeReceipt(encoded string) (CostClasses, error) {
	var receipt CostClasses
	if err := json.Unmarshal([]byte(encoded), &receipt); err != nil {
		return CostClasses{}, fmt.Errorf("decode historical pricing receipt: %w", err)
	}
	return receipt, nil
}
