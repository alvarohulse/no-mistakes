package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const resolvedAgentRoutingVersion = 2

type resolvedAgentModel struct {
	Name   string `json:"name"`
	Vendor string `json:"vendor"`
}

type resolvedAgentRoute struct {
	Agents []types.AgentName  `json:"agents"`
	Model  resolvedAgentModel `json:"model"`
}

type resolvedReviewCandidate struct {
	Agent    types.AgentName    `json:"agent"`
	Model    resolvedAgentModel `json:"model"`
	Optional bool               `json:"optional,omitempty"`
}

type resolvedAgentRouting struct {
	Version          int                                   `json:"version"`
	Demo             bool                                  `json:"demo,omitempty"`
	DefaultAgents    []types.AgentName                     `json:"default_agents"`
	StepRoutes       map[types.StepName]resolvedAgentRoute `json:"step_routes"`
	ReviewCandidates []resolvedReviewCandidate             `json:"review_candidates,omitempty"`
	ReviewAdversary  *resolvedAgentRoute                   `json:"review_adversary,omitempty"`
}

func marshalResolvedAgentRouting(cfg *config.Config, demo bool) (string, error) {
	snapshot, err := resolvedAgentRoutingFromConfig(cfg, demo)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("encode resolved agent routing: %w", err)
	}
	return string(encoded), nil
}

// restoreResolvedAgentRouting applies the launch-time concrete routes to the
// freshly loaded non-routing config. A nil snapshot is the explicit legacy
// path for runs created before resolved routing was persisted.
func restoreResolvedAgentRouting(cfg *config.Config, persisted *string, demo bool) (bool, error) {
	snapshot, legacy, err := decodeResolvedAgentRouting(persisted)
	if err != nil || legacy {
		return legacy, err
	}
	if snapshot.Demo != demo {
		return false, fmt.Errorf("resolved agent routing mode changed since launch")
	}
	if snapshot.Demo {
		return false, nil
	}

	cfg.Agent = snapshot.DefaultAgents[0]
	cfg.Agents = append([]types.AgentName(nil), snapshot.DefaultAgents...)
	cfg.StepAgents = make(map[types.StepName][]types.AgentName, len(snapshot.StepRoutes))
	cfg.StepModels = make(map[types.StepName]config.ModelRoute, len(snapshot.StepRoutes))
	for step, route := range snapshot.StepRoutes {
		cfg.StepAgents[step] = append([]types.AgentName(nil), route.Agents...)
		if route.Model.Name != "" {
			cfg.StepModels[step] = config.ModelRoute{Name: route.Model.Name, Vendor: route.Model.Vendor}
		}
	}
	cfg.ReviewCandidates = make([]config.ReviewCandidate, len(snapshot.ReviewCandidates))
	for i, candidate := range snapshot.ReviewCandidates {
		cfg.ReviewCandidates[i] = config.ReviewCandidate{
			Agent:    candidate.Agent,
			Model:    config.ModelRoute{Name: candidate.Model.Name, Vendor: candidate.Model.Vendor},
			Optional: candidate.Optional,
		}
	}
	cfg.ReviewAdversaryAgents = nil
	cfg.ReviewAdversaryModel = config.ModelRoute{}
	if snapshot.ReviewAdversary != nil {
		cfg.ReviewAdversaryAgents = append([]types.AgentName(nil), snapshot.ReviewAdversary.Agents...)
		cfg.ReviewAdversaryModel = config.ModelRoute{
			Name:   snapshot.ReviewAdversary.Model.Name,
			Vendor: snapshot.ReviewAdversary.Model.Vendor,
		}
	}
	return false, nil
}

func validateResolvedAgentRouting(cfg *config.Config, persisted *string, demo bool) error {
	expected, legacy, err := decodeResolvedAgentRouting(persisted)
	if err != nil {
		return err
	}
	if legacy {
		return nil
	}
	actual, err := resolvedAgentRoutingFromConfig(cfg, demo)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(*actual, *expected) {
		return fmt.Errorf("resolved agent routing differs from launch")
	}
	return nil
}

func decodeResolvedAgentRouting(persisted *string) (*resolvedAgentRouting, bool, error) {
	if persisted == nil {
		return nil, true, nil
	}
	if strings.TrimSpace(*persisted) == "" {
		return nil, false, fmt.Errorf("resolved agent routing was not persisted at launch")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(*persisted))
	decoder.DisallowUnknownFields()
	var snapshot resolvedAgentRouting
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, false, fmt.Errorf("decode resolved agent routing: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, false, fmt.Errorf("decode resolved agent routing: trailing content")
	}
	if err := snapshot.validate(); err != nil {
		return nil, false, err
	}
	return &snapshot, false, nil
}

func resolvedAgentRoutingFromConfig(cfg *config.Config, demo bool) (*resolvedAgentRouting, error) {
	snapshot := &resolvedAgentRouting{
		Version:    resolvedAgentRoutingVersion,
		Demo:       demo,
		StepRoutes: make(map[types.StepName]resolvedAgentRoute),
	}
	if demo {
		return snapshot, nil
	}
	if cfg == nil {
		return nil, fmt.Errorf("resolved agent routing config is nil")
	}
	snapshot.DefaultAgents = append([]types.AgentName(nil), cfg.Agents...)
	for step, agents := range cfg.StepAgents {
		model := cfg.StepModels[step]
		snapshot.StepRoutes[step] = resolvedAgentRoute{
			Agents: append([]types.AgentName(nil), agents...),
			Model:  resolvedAgentModel{Name: model.Name, Vendor: model.Vendor},
		}
	}
	for step, model := range cfg.StepModels {
		if _, exists := snapshot.StepRoutes[step]; !exists {
			snapshot.StepRoutes[step] = resolvedAgentRoute{
				Model: resolvedAgentModel{Name: model.Name, Vendor: model.Vendor},
			}
		}
	}
	for _, candidate := range cfg.ReviewCandidates {
		snapshot.ReviewCandidates = append(snapshot.ReviewCandidates, resolvedReviewCandidate{
			Agent:    candidate.Agent,
			Model:    resolvedAgentModel{Name: candidate.Model.Name, Vendor: candidate.Model.Vendor},
			Optional: candidate.Optional,
		})
	}
	if len(cfg.ReviewAdversaryAgents) > 0 || cfg.ReviewAdversaryModel.Name != "" {
		snapshot.ReviewAdversary = &resolvedAgentRoute{
			Agents: append([]types.AgentName(nil), cfg.ReviewAdversaryAgents...),
			Model: resolvedAgentModel{
				Name:   cfg.ReviewAdversaryModel.Name,
				Vendor: cfg.ReviewAdversaryModel.Vendor,
			},
		}
	}
	if err := snapshot.validate(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (r *resolvedAgentRouting) validate() error {
	if r.Version != 1 && r.Version != resolvedAgentRoutingVersion {
		return fmt.Errorf("resolved agent routing version %d is unsupported", r.Version)
	}
	if r.StepRoutes == nil {
		return fmt.Errorf("resolved agent routing step routes are missing")
	}
	if r.Demo {
		if len(r.DefaultAgents) > 0 || len(r.StepRoutes) > 0 || len(r.ReviewCandidates) > 0 || r.ReviewAdversary != nil {
			return fmt.Errorf("demo resolved agent routing contains executable routes")
		}
		return nil
	}
	if err := validateResolvedAgents("default", r.DefaultAgents); err != nil {
		return err
	}
	for step, route := range r.StepRoutes {
		if !resolvedRoutingStep(step) {
			return fmt.Errorf("resolved agent routing contains unsupported step %q", step)
		}
		if err := validateResolvedAgents(string(step), route.Agents); err != nil {
			return err
		}
		if err := validateResolvedModel(string(step), route.Model, false); err != nil {
			return err
		}
	}
	seenCandidates := make(map[string]bool, len(r.ReviewCandidates))
	for i, candidate := range r.ReviewCandidates {
		if err := validateResolvedAgents(fmt.Sprintf("review candidate %d", i+1), []types.AgentName{candidate.Agent}); err != nil {
			return err
		}
		if err := validateResolvedModel(fmt.Sprintf("review candidate %d", i+1), candidate.Model, true); err != nil {
			return err
		}
		key := string(candidate.Agent) + "\x00" + candidate.Model.Name + "\x00" + candidate.Model.Vendor
		if seenCandidates[key] {
			return fmt.Errorf("resolved review candidate pool contains duplicate %s/%s route", candidate.Agent, candidate.Model.Name)
		}
		seenCandidates[key] = true
	}
	if r.ReviewAdversary != nil {
		if err := validateResolvedAgents("review adversary", r.ReviewAdversary.Agents); err != nil {
			return err
		}
		if err := validateResolvedModel("review adversary", r.ReviewAdversary.Model, true); err != nil {
			return err
		}
	}
	return nil
}

func validateResolvedAgents(route string, agents []types.AgentName) error {
	if len(agents) == 0 {
		return fmt.Errorf("resolved %s agent route is empty", route)
	}
	seen := make(map[types.AgentName]bool, len(agents))
	for _, name := range agents {
		if name == "" || name == types.AgentAuto {
			return fmt.Errorf("resolved %s agent route contains non-concrete agent %q", route, name)
		}
		if seen[name] {
			return fmt.Errorf("resolved %s agent route contains duplicate agent %q", route, name)
		}
		seen[name] = true
	}
	return nil
}

func validateResolvedModel(route string, model resolvedAgentModel, required bool) error {
	configured := config.ModelRoute{Name: model.Name, Vendor: model.Vendor}
	if configured == (config.ModelRoute{}) {
		if !required {
			return nil
		}
		return fmt.Errorf("resolved %s model identity is incomplete", route)
	}
	if err := configured.Validate(); err != nil {
		return fmt.Errorf("resolved %s model identity is invalid: %w", route, err)
	}
	return nil
}

func resolvedRoutingStep(step types.StepName) bool {
	switch step {
	case types.StepIntent, types.StepRefresh, types.StepReview, types.StepBuild, types.StepTest, types.StepDocument, types.StepLint, types.StepPR, types.StepCI:
		return true
	default:
		return false
	}
}
