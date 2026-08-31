//go:build windows

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"golang.org/x/sys/windows"
)

func TestPersistEffectiveConfigArtifactsUsesProtectedOwnerOnlyACL(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	const runID = "run-owner-only"
	yamlBytes := []byte("enabled: true # source=global; is_default=true\n")
	yamlDigest := sha256.Sum256(yamlBytes)
	policyDigest := strings.Repeat("a", 64)
	metaBytes, err := json.Marshal(effectiveConfigMetadata{
		SchemaVersion:   effectiveConfigSchemaVersion,
		RunID:           runID,
		PolicyDigest:    policyDigest,
		YAMLSHA256:      hex.EncodeToString(yamlDigest[:]),
		BinaryVersion:   "test",
		BinaryBuildSHA:  "test-build",
		Generator:       effectiveConfigGenerator,
		GeneratorSchema: effectiveConfigSchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := persistEffectiveConfigArtifacts(p, runID, &effectiveConfigArtifacts{
		YAML: yamlBytes, Meta: metaBytes, PolicyDigest: policyDigest,
	}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{p.RunsDir(), p.RunDir(runID), p.EffectiveConfigYAML(runID), p.EffectiveConfigMeta(runID)} {
		assertProtectedCurrentUserDACL(t, path)
	}
}

func assertProtectedCurrentUserDACL(t *testing.T, path string) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("read DACL for %s: %v", filepath.Base(path), err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatalf("read security descriptor control for %s: %v", filepath.Base(path), err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL for %s inherits access entries", filepath.Base(path))
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("read access entries for %s: %v", filepath.Base(path), err)
	}
	if dacl == nil {
		t.Fatalf("DACL for %s is missing", filepath.Base(path))
	}
	if dacl.AceCount != 1 {
		t.Fatalf("DACL for %s has %d entries, want one current-user entry", filepath.Base(path), dacl.AceCount)
	}
	var entry *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &entry); err != nil {
		t.Fatalf("read access entry for %s: %v", filepath.Base(path), err)
	}
	if entry.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || entry.Mask != windows.GENERIC_ALL {
		t.Fatalf("DACL for %s has type %d mask %#x, want current-user full access", filepath.Base(path), entry.Header.AceType, entry.Mask)
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	entrySID := (*windows.SID)(unsafe.Pointer(&entry.SidStart))
	if !user.User.Sid.Equals(entrySID) {
		t.Fatalf("DACL for %s grants access to a SID other than the current user", filepath.Base(path))
	}
}
