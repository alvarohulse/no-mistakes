package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const evalReplayConfigVersion = 1

// evalReplayConfig is owner-only local material for replaying one Review pass.
// It deliberately excludes machine commands, hooks, paths, agent arguments,
// routes, and non-Review prompts. Review prompt additions and path guidance are
// retained because omitting them would change the subject being evaluated.
type evalReplayConfig struct {
	Version                   int                         `json:"version"`
	IgnorePatterns            []string                    `json:"ignore_patterns,omitempty"`
	ProcessTerminationGraceNS int64                       `json:"process_termination_grace_ns,omitempty"`
	DisableProjectSettings    bool                        `json:"disable_project_settings,omitempty"`
	Prompts                   evalReplayPrompts           `json:"prompts,omitempty"`
	ReviewPathInstructions    []evalReplayPathInstruction `json:"review_path_instructions,omitempty"`
}

type evalReplayPrompts struct {
	Shared string `json:"shared,omitempty"`
	Review string `json:"review,omitempty"`
}

type evalReplayPathInstruction struct {
	Path         string `json:"path"`
	Instructions string `json:"instructions"`
}

// MarshalEvalReplayConfig returns the versioned candidate-independent Review
// inputs that can be stored in owner-only eval provenance.
func MarshalEvalReplayConfig(cfg *Config) ([]byte, error) {
	if cfg == nil {
		return nil, fmt.Errorf("eval replay config is nil")
	}
	snapshot := evalReplayConfig{
		Version:                   evalReplayConfigVersion,
		IgnorePatterns:            append([]string(nil), cfg.IgnorePatterns...),
		ProcessTerminationGraceNS: int64(cfg.ProcessTerminationGrace),
		DisableProjectSettings:    cfg.DisableProjectSettings,
		Prompts: evalReplayPrompts{
			Shared: cfg.Prompts.Shared,
			Review: cfg.Prompts.Review,
		},
		ReviewPathInstructions: marshalEvalReplayPathInstructions(cfg.Review.PathInstructions),
	}
	if err := snapshot.validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode eval replay config: %w", err)
	}
	return encoded, nil
}

// UnmarshalEvalReplayConfig validates owner-only Review replay material and
// returns the exact Config subset consumed by replay.
func UnmarshalEvalReplayConfig(data []byte) (*Config, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var snapshot evalReplayConfig
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("decode eval replay config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode eval replay config: trailing content")
	}
	if err := snapshot.validate(); err != nil {
		return nil, err
	}
	return &Config{
		IgnorePatterns:          append([]string(nil), snapshot.IgnorePatterns...),
		ProcessTerminationGrace: time.Duration(snapshot.ProcessTerminationGraceNS),
		DisableProjectSettings:  snapshot.DisableProjectSettings,
		Prompts: PromptConfig{
			Shared: snapshot.Prompts.Shared,
			Review: snapshot.Prompts.Review,
		},
		Review: Review{PathInstructions: unmarshalEvalReplayPathInstructions(snapshot.ReviewPathInstructions)},
	}, nil
}

func (c evalReplayConfig) validate() error {
	if c.Version != evalReplayConfigVersion {
		return fmt.Errorf("eval replay config version %d is unsupported", c.Version)
	}
	if c.ProcessTerminationGraceNS < 0 {
		return fmt.Errorf("eval replay process termination grace must not be negative")
	}
	if err := validateReviewRaw(ReviewRaw{PathInstructions: unmarshalEvalReplayPathInstructions(c.ReviewPathInstructions)}); err != nil {
		return fmt.Errorf("eval replay config: %w", err)
	}
	return nil
}

func marshalEvalReplayPathInstructions(instructions []PathInstruction) []evalReplayPathInstruction {
	out := make([]evalReplayPathInstruction, 0, len(instructions))
	for _, instruction := range instructions {
		out = append(out, evalReplayPathInstruction{Path: instruction.Path, Instructions: instruction.Instructions})
	}
	return out
}

func unmarshalEvalReplayPathInstructions(instructions []evalReplayPathInstruction) []PathInstruction {
	out := make([]PathInstruction, 0, len(instructions))
	for _, instruction := range instructions {
		out = append(out, PathInstruction{Path: instruction.Path, Instructions: instruction.Instructions})
	}
	return out
}
