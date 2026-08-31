// Package effectiveconfig owns validation and reading of immutable per-run
// effective-configuration artifacts.
package effectiveconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"gopkg.in/yaml.v3"
)

const (
	SchemaVersion = 1
	YAMLMaxBytes  = 256 * 1024
	Generator     = "no-mistakes/effective-config"
)

var provenanceCommentPattern = regexp.MustCompile(`^#?\s*source=(global|global-override|trusted|pushed|run-request|runtime); is_default=(true|false)(; qualifier=(clear|append|merge)(,(clear|append|merge))*)?$`)

// Metadata is the value-free integrity sidecar stored with effective-config.yaml.
type Metadata struct {
	SchemaVersion   int    `json:"schema_version"`
	RunID           string `json:"run_id"`
	PolicyDigest    string `json:"policy_digest"`
	YAMLSHA256      string `json:"yaml_sha256"`
	BinaryVersion   string `json:"binary_version"`
	BinaryBuildSHA  string `json:"binary_build_sha"`
	Generator       string `json:"generator"`
	GeneratorSchema int    `json:"generator_schema"`
}

// Artifact is one validated immutable effective-configuration snapshot.
type Artifact struct {
	YAML     []byte
	Metadata Metadata
}

// Read loads and validates the artifact for runID against the authoritative
// launch-time policy digest. It never reconstructs data from current config.
func Read(p *paths.Paths, runID, policyDigest string) (*Artifact, error) {
	if p == nil {
		return nil, fmt.Errorf("read effective config: paths are missing")
	}
	if strings.TrimSpace(runID) == "" || filepath.Base(runID) != runID || runID == "." || runID == ".." {
		return nil, fmt.Errorf("read effective config: invalid run ID %q", runID)
	}
	yamlBytes, err := os.ReadFile(p.EffectiveConfigYAML(runID))
	if err != nil {
		return nil, fmt.Errorf("read effective config YAML: %w", err)
	}
	metaBytes, err := os.ReadFile(p.EffectiveConfigMeta(runID))
	if err != nil {
		return nil, fmt.Errorf("read effective config sidecar: %w", err)
	}
	return Validate(yamlBytes, metaBytes, runID, policyDigest)
}

// Validate checks completeness, schema support, and identity/integrity binding.
func Validate(yamlBytes, metaBytes []byte, runID, policyDigest string) (*Artifact, error) {
	if len(yamlBytes) == 0 || len(yamlBytes) > YAMLMaxBytes {
		return nil, fmt.Errorf("effective config YAML completeness or size validation failed")
	}
	var root yaml.Node
	if err := yaml.Unmarshal(yamlBytes, &root); err != nil {
		return nil, fmt.Errorf("validate effective config YAML: %w", err)
	}
	if err := validateAnnotations(&root, ""); err != nil {
		return nil, fmt.Errorf("effective config is incomplete: %w", err)
	}

	var metadata Metadata
	decoder := json.NewDecoder(bytes.NewReader(metaBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return nil, fmt.Errorf("validate effective config sidecar: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("validate effective config sidecar: trailing content")
	}
	if metadata.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("effective config sidecar schema version %d is unsupported", metadata.SchemaVersion)
	}
	if metadata.GeneratorSchema != SchemaVersion {
		return nil, fmt.Errorf("effective config generator schema version %d is unsupported", metadata.GeneratorSchema)
	}
	if metadata.Generator != Generator {
		return nil, fmt.Errorf("effective config sidecar generator is unsupported")
	}
	if metadata.RunID != runID {
		return nil, fmt.Errorf("effective config sidecar run ID does not match requested run")
	}
	if !validSHA256Hex(policyDigest) || !validSHA256Hex(metadata.PolicyDigest) || metadata.PolicyDigest != policyDigest {
		return nil, fmt.Errorf("effective config sidecar policy digest does not match resolved policy")
	}
	digest := sha256.Sum256(yamlBytes)
	if metadata.YAMLSHA256 != hex.EncodeToString(digest[:]) {
		return nil, fmt.Errorf("effective config sidecar integrity does not match stored YAML")
	}
	if strings.TrimSpace(metadata.BinaryVersion) == "" || strings.TrimSpace(metadata.BinaryBuildSHA) == "" {
		return nil, fmt.Errorf("effective config sidecar binary identity is incomplete")
	}
	return &Artifact{YAML: append([]byte(nil), yamlBytes...), Metadata: metadata}, nil
}

func validateAnnotations(node *yaml.Node, path string) error {
	if node == nil {
		return fmt.Errorf("missing YAML node at %s", path)
	}
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) != 1 {
			return fmt.Errorf("document has %d roots", len(node.Content))
		}
		return validateAnnotations(node.Content[0], path)
	}
	switch node.Kind {
	case yaml.MappingNode:
		if len(node.Content) == 0 {
			if err := validateAnnotation(node, path); err != nil {
				return err
			}
		}
		for i := 0; i+1 < len(node.Content); i += 2 {
			childPath := node.Content[i].Value
			if path != "" {
				childPath = path + "." + childPath
			}
			if err := validateAnnotations(node.Content[i+1], childPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		if len(node.Content) == 0 {
			if err := validateAnnotation(node, path); err != nil {
				return err
			}
		}
		for i, item := range node.Content {
			itemPath := fmt.Sprintf("%s[%d]", path, i)
			if err := validateAnnotation(item, itemPath); err != nil {
				return err
			}
			if err := validateAnnotations(item, itemPath); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if err := validateAnnotation(node, path); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported YAML node kind %d at %s", node.Kind, path)
	}
	return nil
}

func validateAnnotation(node *yaml.Node, path string) error {
	comment := node.LineComment
	if comment == "" {
		comment = node.HeadComment
	}
	if !provenanceCommentPattern.MatchString(comment) {
		return fmt.Errorf("value %s has invalid or missing provenance", path)
	}
	return nil
}

func validSHA256Hex(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}
