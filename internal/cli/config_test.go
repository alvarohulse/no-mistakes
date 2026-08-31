package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/effectiveconfig"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	runstats "github.com/kunchenguid/no-mistakes/internal/stats"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestConfigExplainRendersCanonicalGatePolicyAsTextAndJSON(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	repoDir := setupTestRepo(t)
	configYAML := "commands:\n  build: echo build\nignore_patterns: [vendor/**]\nauto_fix:\n  review: 0\n"
	if err := os.WriteFile(filepath.Join(repoDir, ".no-mistakes.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repoDir, "git", "add", ".no-mistakes.yaml")
	run(t, repoDir, "git", "commit", "-m", "configure policy")
	branch := commandOutput(t, repoDir, "git", "branch", "--show-current")
	run(t, repoDir, "git", "push", "origin", "HEAD:refs/heads/"+branch)

	p := paths.WithRoot(os.Getenv("NM_HOME"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, _, err := gate.Init(context.Background(), database, p, repoDir)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	run(t, "", "git", "--git-dir="+p.RepoDir(repo.ID), "fetch", repoDir, "+HEAD:refs/heads/"+branch)

	// The command must resolve the gate head, not the dirty or advanced local
	// checkout. This unpushed malformed config would fail if read from disk.
	if err := os.WriteFile(filepath.Join(repoDir, ".no-mistakes.yaml"), []byte("commands: [malformed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repoDir, "git", "add", ".no-mistakes.yaml")
	run(t, repoDir, "git", "commit", "-m", "unpublished local config")
	setSafeBareRepositoryExplicitForCLITest(t)

	jsonOutput, err := executeCmd("config", "explain", "--format", "json")
	if err != nil {
		t.Fatalf("config explain JSON: %v", err)
	}
	var envelope struct {
		PolicyDigest string          `json:"policy_digest"`
		Policy       json.RawMessage `json:"policy"`
	}
	if err := json.Unmarshal([]byte(jsonOutput), &envelope); err != nil {
		t.Fatalf("decode config explain JSON: %v\n%s", err, jsonOutput)
	}
	wantDigest := sha256.Sum256(envelope.Policy)
	if envelope.PolicyDigest != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("policy digest = %q, want digest of canonical policy", envelope.PolicyDigest)
	}
	var policy struct {
		Sources []db.ConfigSource `json:"sources"`
		Steps   []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(envelope.Policy, &policy); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	if got := cliConfigSourceKinds(policy.Sources); got != "branch,default" {
		t.Fatalf("source kinds = %q, want branch,default", got)
	}
	for _, source := range policy.Sources {
		if source.Ref == "" || source.Digest == "" {
			t.Fatalf("incomplete source: %+v", source)
		}
	}
	if len(policy.Steps) == 0 {
		t.Fatal("resolved policy has no steps")
	}

	jsonOutputAgain, err := executeCmd("config", "explain", "--format", "json")
	if err != nil {
		t.Fatalf("second config explain JSON: %v", err)
	}
	if jsonOutputAgain != jsonOutput {
		t.Fatalf("canonical JSON changed between identical resolutions:\n%s\n%s", jsonOutput, jsonOutputAgain)
	}

	textOutput, err := executeCmd("config", "explain")
	if err != nil {
		t.Fatalf("config explain text: %v", err)
	}
	for _, required := range []string{
		"policy digest: " + envelope.PolicyDigest,
		`"kind": "branch"`,
		`"kind": "default"`,
		`"digest":`,
		`"ref":`,
		`"commands":`,
		`"routing":`,
	} {
		if !strings.Contains(textOutput, required) {
			t.Errorf("text output omitted %q:\n%s", required, textOutput)
		}
	}
}

func TestConfigExplainRejectsUnknownFormat(t *testing.T) {
	if _, err := executeCmd("config", "explain", "--format", "yaml"); err == nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("error = %v, want format refusal", err)
	}
}

func TestConfigExplainRunReturnsStoredYAMLAndAxiStatusShowsAvailability(t *testing.T) {
	t.Setenv("NM_DEMO", "1")
	repoDir := setupTestRepo(t)
	configYAML := "commands:\n  build: echo launch-value\n"
	if err := os.WriteFile(filepath.Join(repoDir, ".no-mistakes.yaml"), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repoDir, "git", "add", ".no-mistakes.yaml")
	run(t, repoDir, "git", "commit", "-m", "configure launch policy")
	branch := commandOutput(t, repoDir, "git", "branch", "--show-current")
	run(t, repoDir, "git", "push", "origin", "HEAD:refs/heads/"+branch)

	p := paths.WithRoot(os.Getenv("NM_HOME"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, _, err := gate.Init(context.Background(), database, p, repoDir)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	run(t, "", "git", "--git-dir="+p.RepoDir(repo.ID), "fetch", repoDir, "+HEAD:refs/heads/"+branch)
	setSafeBareRepositoryExplicitForCLITest(t)

	policyOutput, err := executeCmd("config", "explain", "--format", "json")
	if err != nil {
		t.Fatalf("resolve launch policy: %v", err)
	}
	var envelope struct {
		PolicyDigest string          `json:"policy_digest"`
		Policy       json.RawMessage `json:"policy"`
	}
	if err := json.Unmarshal([]byte(policyOutput), &envelope); err != nil {
		t.Fatal(err)
	}

	database, err = db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	storedRun, err := database.InsertRun(repo.ID, branch, "head", "base")
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.UpdateRunResolvedPolicy(storedRun.ID, string(envelope.Policy), envelope.PolicyDigest); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	legacyRun, err := database.InsertRunWithOptions(repo.ID, "legacy", "legacy-head", "legacy-base", db.RunOptions{LegacyResolvedPolicy: true})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	archivedRun, err := database.InsertRunWithOptions(repo.ID, "archived", "archived-head", "archived-base", db.RunOptions{LegacyResolvedPolicy: true})
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(archivedRun.ID, types.RunCompleted); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	_, archivedReceipt, err := runstats.BuildMetricReceipt(database, archivedRun.ID, time.Now())
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if archived, err := database.ArchiveRunWithMetricReceipt(archivedReceipt, false, nil); err != nil || !archived {
		_ = database.Close()
		t.Fatalf("archive run = %t, %v", archived, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	storedYAML := []byte("build: echo launch-value # source=trusted; is_default=false\n")
	writeStoredEffectiveConfigForCLITest(t, p, storedRun.ID, envelope.PolicyDigest, storedYAML)
	if err := os.WriteFile(filepath.Join(repoDir, ".no-mistakes.yaml"), []byte("commands:\n  build: echo drifted-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, repoDir, "git", "add", ".no-mistakes.yaml")
	run(t, repoDir, "git", "commit", "-m", "change current policy")
	run(t, repoDir, "git", "push", "origin", "HEAD:refs/heads/"+branch)
	run(t, "", "git", "--git-dir="+p.RepoDir(repo.ID), "fetch", repoDir, "+HEAD:refs/heads/"+branch)

	output, err := executeCmd("config", "explain", "--run", storedRun.ID)
	if err != nil {
		t.Fatalf("config explain --run: %v", err)
	}
	if output != string(storedYAML) {
		t.Fatalf("stored explain output = %q, want exact stored YAML %q", output, storedYAML)
	}

	statusOutput, err := executeCmd("axi", "status", "--run", storedRun.ID)
	if err != nil {
		t.Fatalf("axi status stored run: %v\n%s", err, statusOutput)
	}
	if !strings.Contains(statusOutput, "effective_config: "+p.EffectiveConfigYAML(storedRun.ID)) {
		t.Fatalf("axi status omitted effective config path:\n%s", statusOutput)
	}
	if err := os.WriteFile(p.EffectiveConfigYAML(storedRun.ID), append(storedYAML, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := executeCmd("config", "explain", "--run", storedRun.ID); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("corrupt config explain error = %v", err)
	}
	corruptStatus, err := executeCmd("axi", "status", "--run", storedRun.ID)
	if err != nil {
		t.Fatalf("axi status corrupt stored run: %v\n%s", err, corruptStatus)
	}
	if !strings.Contains(corruptStatus, "effective_config: \"unavailable (required artifact invalid:") || !strings.Contains(corruptStatus, "integrity") {
		t.Fatalf("axi status omitted required corrupt state:\n%s", corruptStatus)
	}

	legacyStatus, err := executeCmd("axi", "status", "--run", legacyRun.ID)
	if err != nil {
		t.Fatalf("axi status legacy run: %v\n%s", err, legacyStatus)
	}
	if !strings.Contains(legacyStatus, "effective_config: unavailable (legacy run)") {
		t.Fatalf("axi status omitted legacy unavailability:\n%s", legacyStatus)
	}
	if _, err := executeCmd("config", "explain", "--run", legacyRun.ID); err == nil || !strings.Contains(err.Error(), "unavailable for legacy run") {
		t.Fatalf("legacy config explain error = %v", err)
	}
	if _, err := executeCmd("config", "explain", "--run", archivedRun.ID); err == nil || !strings.Contains(err.Error(), "unavailable for archived run") {
		t.Fatalf("archived config explain error = %v", err)
	}
	if _, err := executeCmd("config", "explain", "--run", storedRun.ID, "--format", "json"); err == nil || !strings.Contains(err.Error(), "cannot be used with --run") {
		t.Fatalf("config explain run format error = %v", err)
	}
}

func commandOutput(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return strings.TrimSpace(string(out))
}

func cliConfigSourceKinds(sources []db.ConfigSource) string {
	kinds := make([]string, 0, len(sources))
	for _, source := range sources {
		kinds = append(kinds, source.Kind)
	}
	return strings.Join(kinds, ",")
}

func setSafeBareRepositoryExplicitForCLITest(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
}

func writeStoredEffectiveConfigForCLITest(t *testing.T, p *paths.Paths, runID, policyDigest string, yamlBytes []byte) {
	t.Helper()
	if err := os.MkdirAll(p.RunDir(runID), 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(yamlBytes)
	metaBytes, err := json.Marshal(effectiveconfig.Metadata{
		SchemaVersion: effectiveconfig.SchemaVersion, RunID: runID, PolicyDigest: policyDigest,
		YAMLSHA256: hex.EncodeToString(digest[:]), BinaryVersion: "test", BinaryBuildSHA: "test",
		Generator: effectiveconfig.Generator, GeneratorSchema: effectiveconfig.SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.EffectiveConfigYAML(runID), yamlBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.EffectiveConfigMeta(runID), metaBytes, 0o600); err != nil {
		t.Fatal(err)
	}
}
