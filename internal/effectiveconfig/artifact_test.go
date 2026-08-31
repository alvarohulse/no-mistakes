package effectiveconfig_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/effectiveconfig"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"gopkg.in/yaml.v3"
)

func TestReadReturnsOnlyAnIntactSupportedArtifact(t *testing.T) {
	const runID = "01M1EFFECTIVECONFIGARTIFACT"
	const policyDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	yamlBytes := []byte("enabled: true # source=runtime; is_default=false\n")

	tests := []struct {
		name      string
		mutate    func(t *testing.T, p *paths.Paths)
		wantError string
	}{
		{name: "intact"},
		{
			name: "missing YAML",
			mutate: func(t *testing.T, p *paths.Paths) {
				t.Helper()
				if err := os.Remove(p.EffectiveConfigYAML(runID)); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "read effective config YAML",
		},
		{
			name: "missing sidecar",
			mutate: func(t *testing.T, p *paths.Paths) {
				t.Helper()
				if err := os.Remove(p.EffectiveConfigMeta(runID)); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "read effective config sidecar",
		},
		{
			name: "corrupt YAML",
			mutate: func(t *testing.T, p *paths.Paths) {
				t.Helper()
				if err := os.WriteFile(p.EffectiveConfigYAML(runID), append(yamlBytes, ' '), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "integrity",
		},
		{
			name: "unsupported sidecar schema",
			mutate: func(t *testing.T, p *paths.Paths) {
				t.Helper()
				writeArtifact(t, p, runID, policyDigest, yamlBytes, effectiveconfig.SchemaVersion+1)
			},
			wantError: "schema version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			writeArtifact(t, p, runID, policyDigest, yamlBytes, effectiveconfig.SchemaVersion)
			if tt.mutate != nil {
				tt.mutate(t, p)
			}

			artifact, err := effectiveconfig.Read(p, runID, policyDigest)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("Read() error = %v, want %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(artifact.YAML) != string(yamlBytes) || artifact.Metadata.RunID != runID || artifact.Metadata.PolicyDigest != policyDigest {
				t.Fatalf("Read() = %+v, want exact stored bytes and matching metadata", artifact)
			}
		})
	}
}

func TestReadRejectsOversizedArtifactFilesAtTheReadBoundary(t *testing.T) {
	const runID = "01M1EFFECTIVECONFIGBOUNDED"
	const policyDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	tests := []struct {
		name      string
		mutate    func(t *testing.T, p *paths.Paths)
		wantError string
	}{
		{
			name: "YAML",
			mutate: func(t *testing.T, p *paths.Paths) {
				t.Helper()
				if err := os.WriteFile(p.EffectiveConfigYAML(runID), bytes.Repeat([]byte{'x'}, effectiveconfig.YAMLMaxBytes+1), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "read effective config YAML: exceeds",
		},
		{
			name: "sidecar",
			mutate: func(t *testing.T, p *paths.Paths) {
				t.Helper()
				if err := os.WriteFile(p.EffectiveConfigMeta(runID), bytes.Repeat([]byte{' '}, 64*1024+1), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantError: "read effective config sidecar: exceeds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			writeArtifact(t, p, runID, policyDigest, []byte("enabled: true # source=runtime; is_default=false\n"), effectiveconfig.SchemaVersion)
			tt.mutate(t, p)
			if _, err := effectiveconfig.Read(p, runID, policyDigest); err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Read() error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestReadRejectsArtifactLinksBeforeOpeningThem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not reliably available on Windows CI")
	}
	const runID = "01M1EFFECTIVECONFIGLINK"
	const policyDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p := paths.WithRoot(t.TempDir())
	yamlBytes := []byte("enabled: true # source=runtime; is_default=false\n")
	writeArtifact(t, p, runID, policyDigest, yamlBytes, effectiveconfig.SchemaVersion)
	yamlPath := p.EffectiveConfigYAML(runID)
	targetPath := yamlPath + ".target"
	if err := os.Rename(yamlPath, targetPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetPath, yamlPath); err != nil {
		t.Fatal(err)
	}
	if _, err := effectiveconfig.Read(p, runID, policyDigest); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("Read() error = %v, want link rejection", err)
	}
}

func TestReadRejectsIdentityAndCompletenessMismatch(t *testing.T) {
	const runID = "01M1EFFECTIVECONFIGARTIFACT"
	const policyDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	tests := []struct {
		name       string
		yaml       []byte
		readRunID  string
		readPolicy string
		wantError  string
	}{
		{name: "run ID", yaml: []byte("enabled: true # source=runtime; is_default=false\n"), readRunID: "different-run", readPolicy: policyDigest, wantError: "run ID"},
		{name: "policy digest", yaml: []byte("enabled: true # source=runtime; is_default=false\n"), readRunID: runID, readPolicy: strings.Repeat("b", 64), wantError: "policy digest"},
		{name: "missing provenance", yaml: []byte("enabled: true\n"), readRunID: runID, readPolicy: policyDigest, wantError: "missing provenance"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			artifactRunID := runID
			if tt.name == "run ID" {
				artifactRunID = tt.readRunID
			}
			writeArtifactWithIdentity(t, p, artifactRunID, runID, policyDigest, tt.yaml, effectiveconfig.SchemaVersion)
			_, err := effectiveconfig.Read(p, tt.readRunID, tt.readPolicy)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Read() error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func TestCommentFreeYAMLPreservesValuesWhileRemovingProvenance(t *testing.T) {
	stored := []byte("command: 'echo # literal' # source=trusted; is_default=false\nscript: |- # source=global; is_default=true\n  echo \"# still data\"\n")

	rendered, err := effectiveconfig.CommentFreeYAML(stored)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]string
	if err := yaml.Unmarshal(rendered, &values); err != nil {
		t.Fatalf("rendered YAML is invalid: %v", err)
	}
	if values["command"] != "echo # literal" || values["script"] != "echo \"# still data\"" {
		t.Fatalf("rendered values changed: %#v\n%s", values, rendered)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(rendered, &root); err != nil {
		t.Fatal(err)
	}
	var assertNoComments func(*yaml.Node)
	assertNoComments = func(node *yaml.Node) {
		if node.HeadComment != "" || node.LineComment != "" || node.FootComment != "" {
			t.Fatalf("rendered YAML retained a comment at %q: %s", node.Value, rendered)
		}
		for _, child := range node.Content {
			assertNoComments(child)
		}
	}
	assertNoComments(&root)
}

func writeArtifact(t *testing.T, p *paths.Paths, runID, policyDigest string, yamlBytes []byte, schemaVersion int) {
	t.Helper()
	writeArtifactWithIdentity(t, p, runID, runID, policyDigest, yamlBytes, schemaVersion)
}

func writeArtifactWithIdentity(t *testing.T, p *paths.Paths, pathRunID, metadataRunID, policyDigest string, yamlBytes []byte, schemaVersion int) {
	t.Helper()
	if err := os.MkdirAll(p.RunDir(pathRunID), 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(yamlBytes)
	metadata := effectiveconfig.Metadata{
		SchemaVersion:   schemaVersion,
		RunID:           metadataRunID,
		PolicyDigest:    policyDigest,
		YAMLSHA256:      hex.EncodeToString(digest[:]),
		BinaryVersion:   "test",
		BinaryBuildSHA:  "build",
		Generator:       effectiveconfig.Generator,
		GeneratorSchema: schemaVersion,
	}
	metaBytes, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.RunDir(pathRunID), "effective-config.yaml"), yamlBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.RunDir(pathRunID), "effective-config.meta.json"), metaBytes, 0o600); err != nil {
		t.Fatal(err)
	}
}
