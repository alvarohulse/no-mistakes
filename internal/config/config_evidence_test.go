package config

import (
	"strings"
	"testing"
)

func TestRetiredEvidencePublicationKeysFailEveryConfigurationLayer(t *testing.T) {
	for layer, load := range evidenceConfigLoaders() {
		for key, value := range map[string]string{
			"store_in_repo": "true",
			"dir":           "artifacts/evidence",
			"branch":        "no-mistakes/evidence",
		} {
			t.Run(layer+"/"+key, func(t *testing.T) {
				assertRetiredEvidenceKeyError(t, load([]byte("test:\n  evidence:\n    "+key+": "+value+"\n")), key)
			})
		}
	}
}

func TestRetiredEvidencePublicationKeysInYAMLMergeFailWithMigrationGuidance(t *testing.T) {
	for layer, load := range evidenceConfigLoaders() {
		for shape, data := range map[string]string{
			"mapping":  "test:\n  evidence:\n    <<: &legacy\n      store_in_repo: true\n",
			"sequence": "test:\n  evidence:\n    <<:\n      - &legacy\n        store_in_repo: true\n",
		} {
			t.Run(layer+"/"+shape, func(t *testing.T) {
				assertRetiredEvidenceKeyError(t, load([]byte(data)), "store_in_repo")
			})
		}
	}
}

func evidenceConfigLoaders() map[string]func([]byte) error {
	return map[string]func([]byte) error{
		"global": func(data []byte) error {
			_, err := LoadGlobalFromBytes(data)
			return err
		},
		"repository": func(data []byte) error {
			_, err := LoadRepoFromBytes(data)
			return err
		},
		"machine override": func(data []byte) error {
			_, err := LoadGlobalFromBytes([]byte("overrides:\n  example/widgets:\n" + indentYAMLForEvidenceTest(string(data), 4)))
			return err
		},
	}
}

func assertRetiredEvidenceKeyError(t *testing.T, err error, key string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected test.evidence.%s to be rejected", key)
	}
	for _, want := range []string{"test.evidence." + key, "remove", "local"} {
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(want)) {
			t.Fatalf("error %q does not contain actionable guidance %q", err, want)
		}
	}
}

func indentYAMLForEvidenceTest(value string, spaces int) string {
	prefix := strings.Repeat(" ", spaces)
	return prefix + strings.ReplaceAll(strings.TrimSuffix(value, "\n"), "\n", "\n"+prefix) + "\n"
}
