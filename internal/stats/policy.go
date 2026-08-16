package stats

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type policyShape struct {
	Version int          `json:"version"`
	Managed bool         `json:"managed"`
	Steps   []policyStep `json:"steps"`
	Routing struct {
		Demo             bool                    `json:"demo"`
		ReviewCandidates []policyReviewCandidate `json:"review_candidates"`
	} `json:"routing"`
}

type policyStep struct {
	Name       types.StepName   `json:"name"`
	Status     string           `json:"status"`
	SkipSource types.SkipSource `json:"skip_source"`
}

type policyReviewCandidate struct {
	Agent string `json:"agent"`
	Model struct {
		Name   string `json:"name"`
		Vendor string `json:"vendor"`
	} `json:"model"`
	Optional bool `json:"optional"`
}

type policyFacts struct {
	Steps                 []policyStep
	StepOrder             []types.StepName
	SkipSources           map[types.StepName]types.SkipSource
	ManagedReviewReceipts bool
	ReviewCandidates      []ReviewCandidate
}

func resolvedPolicyFacts(encoded, digest *string) (policyFacts, bool, []string) {
	facts := policyFacts{SkipSources: make(map[types.StepName]types.SkipSource)}
	if encoded == nil && digest == nil {
		return facts, false, nil
	}
	if encoded == nil || digest == nil || strings.TrimSpace(*encoded) == "" || strings.TrimSpace(*digest) == "" {
		return facts, false, []string{"resolved policy snapshot is incomplete"}
	}
	expectedDigest := sha256.Sum256([]byte(*encoded))
	if hex.EncodeToString(expectedDigest[:]) != *digest {
		return facts, false, []string{"resolved policy digest does not match snapshot"}
	}
	var policy policyShape
	if err := json.Unmarshal([]byte(*encoded), &policy); err != nil {
		return facts, false, []string{fmt.Sprintf("resolved policy could not be decoded: %v", err)}
	}
	if policy.Version < 1 || policy.Version > 5 {
		return facts, false, []string{fmt.Sprintf("resolved policy version %d is unsupported", policy.Version)}
	}
	var integrityErrors []string
	seen := make(map[types.StepName]bool, len(policy.Steps))
	for _, step := range policy.Steps {
		name := step.Name.Canonical()
		if name.Order() == 0 {
			integrityErrors = append(integrityErrors, fmt.Sprintf("resolved policy contains unknown step %q", name))
			continue
		}
		if seen[name] {
			integrityErrors = append(integrityErrors, fmt.Sprintf("resolved policy contains duplicate step %s", name))
			continue
		}
		seen[name] = true
		step.Name = name
		facts.Steps = append(facts.Steps, step)
		facts.StepOrder = append(facts.StepOrder, name)
		switch step.Status {
		case "enabled":
			if step.SkipSource != "" {
				integrityErrors = append(integrityErrors, fmt.Sprintf("resolved policy enabled step %s has skip source %q", name, step.SkipSource))
			}
		case "skipped":
			source := step.SkipSource
			if source == "" && policy.Version <= 4 {
				source = types.SkipSourceRunRequest
			}
			if !source.Valid() {
				integrityErrors = append(integrityErrors, fmt.Sprintf("resolved policy step %s has unsupported skip source %q", name, source))
				continue
			}
			facts.SkipSources[name] = source
		default:
			integrityErrors = append(integrityErrors, fmt.Sprintf("resolved policy step %s has unsupported status %q", name, step.Status))
		}
	}
	facts.ManagedReviewReceipts = policy.Managed && !policy.Routing.Demo
	if facts.ManagedReviewReceipts && len(policy.Routing.ReviewCandidates) == 0 {
		integrityErrors = append(integrityErrors, "managed resolved policy has no Review candidate pool")
	}
	for _, candidate := range policy.Routing.ReviewCandidates {
		facts.ReviewCandidates = append(facts.ReviewCandidates, ReviewCandidate{
			Agent: candidate.Agent, Model: candidate.Model.Name, Provider: candidate.Model.Vendor, Optional: candidate.Optional,
		})
	}
	if len(policy.Steps) == 0 {
		integrityErrors = append(integrityErrors, "resolved policy has no pipeline steps")
	}
	return facts, true, integrityErrors
}

func reconcilePolicySteps(runStatus types.RunStatus, policySteps []policyStep, storedSteps []Step) []string {
	storedByName := make(map[types.StepName][]Step, len(storedSteps))
	for _, step := range storedSteps {
		name := step.Name.Canonical()
		storedByName[name] = append(storedByName[name], step)
	}
	policyNames := make(map[types.StepName]bool, len(policySteps))
	var integrityErrors []string
	for _, expected := range policySteps {
		name := expected.Name.Canonical()
		policyNames[name] = true
		matches := storedByName[name]
		if len(matches) == 0 {
			if runStatus == types.RunCompleted {
				integrityErrors = append(integrityErrors, fmt.Sprintf("completed run is missing stored result for policy step %s", name))
			}
			continue
		}
		if len(matches) > 1 {
			integrityErrors = append(integrityErrors, fmt.Sprintf("policy step %s has %d stored results", name, len(matches)))
			continue
		}
		if runStatus == types.RunCompleted && matches[0].Status != types.StepStatusCompleted && matches[0].Status != types.StepStatusSkipped {
			integrityErrors = append(integrityErrors, fmt.Sprintf("completed run has nonterminal policy step %s with status %s", name, matches[0].Status))
		}
	}
	reportedExtras := make(map[types.StepName]bool)
	for _, step := range storedSteps {
		name := step.Name.Canonical()
		if !policyNames[name] && !reportedExtras[name] {
			integrityErrors = append(integrityErrors, fmt.Sprintf("stored step %s is absent from resolved policy", name))
			reportedExtras[name] = true
		}
	}
	return integrityErrors
}

func orderedSkipReceipts(sources map[types.StepName]types.SkipSource, policyOrder []types.StepName) []SkipReceipt {
	result := make([]SkipReceipt, 0, len(sources))
	seen := make(map[types.StepName]bool, len(sources))
	for _, step := range policyOrder {
		if source, ok := sources[step]; ok {
			result = append(result, SkipReceipt{Step: step, Source: source})
			seen[step] = true
		}
	}
	for _, step := range types.AllSteps() {
		if source, ok := sources[step]; ok && !seen[step] {
			result = append(result, SkipReceipt{Step: step, Source: source})
			seen[step] = true
		}
	}
	var remaining []types.StepName
	for step := range sources {
		if !seen[step] {
			remaining = append(remaining, step)
		}
	}
	sort.Slice(remaining, func(i, j int) bool { return remaining[i] < remaining[j] })
	for _, step := range remaining {
		result = append(result, SkipReceipt{Step: step, Source: sources[step]})
	}
	return result
}
