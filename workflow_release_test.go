package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestReleaseWorkflowUsesScopedConcurrencyGroup(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "group: release-${{ github.ref }}") {
		t.Fatalf("release workflow must scope concurrency by ref")
	}
	if strings.Contains(content, "group: release\n") {
		t.Fatalf("release workflow must not use a global concurrency group")
	}
}

func TestReleaseWorkflowDoesNotDefineValidationJobs(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	content := string(data)
	for _, job := range []string{"check", "test"} {
		if strings.Contains(content, "\n  "+job+":\n") {
			t.Fatalf("release workflow must not define %q; CI owns validation now", job)
		}
	}
}

func TestReleaseWorkflowRunsReleasePleaseWithoutValidationGuards(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	block := extractJobBlock(t, string(data), "release-please")
	if strings.Contains(block, "needs:") {
		t.Fatalf("release-please must not depend on in-workflow validation jobs")
	}
	guard := "!startsWith(github.event.head_commit.message, 'chore(main): release')"
	if strings.Contains(block, guard) {
		t.Fatalf("release-please must not carry the old release-commit skip guard")
	}
}

func TestReleaseWorkflowAttestsExactReleasePleaseHead(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	content := string(data)
	block := extractJobBlock(t, content, "release-please")
	if !strings.Contains(content, "statuses: write") {
		t.Fatal("release workflow must be able to attest the exact generated head")
	}
	for _, required := range []string{
		"steps.release.outputs.prs_created == 'true'",
		"RELEASE_PR: ${{ steps.release.outputs.pr }}",
		"expected_head_ref=release-please--branches--main",
		`repos/${GITHUB_REPOSITORY}/pulls/${pr_number}`,
		`refs/pull/${pr_number}/head`,
		`[ "$base_sha" != "$GITHUB_SHA" ]`,
		`[ "$fetched_head" != "$head_sha" ]`,
		`git show "${GITHUB_SHA}:scripts/guard-generated-files.sh"`,
		`"$GITHUB_SHA"`,
		`"$head_sha"`,
		`repos/${GITHUB_REPOSITORY}/statuses/${head_sha}`,
		"Generated files must not be hand-edited",
		"latest_head_sha",
	} {
		if !strings.Contains(block, required) {
			t.Errorf("release-please job must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"steps.release.outputs.pr.sha",
		"github.event.pull_request",
	} {
		if strings.Contains(block, forbidden) {
			t.Errorf("release-please job must not trust unavailable or unrelated metadata %q", forbidden)
		}
	}
}

func TestReleaseWorkflowBuildStartsOnlyWhenReleaseIsCreated(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	block := extractJobBlock(t, string(data), "build-and-upload")
	if !strings.Contains(block, "if: needs.release-please.outputs.release_created == 'true'") {
		t.Fatalf("build-and-upload must run only when release-please created a release")
	}
	for _, unexpected := range []string{"!cancelled()", "needs.release-please.result == 'success'"} {
		if strings.Contains(block, unexpected) {
			t.Fatalf("build-and-upload must not keep the old skipped-validation guard %q", unexpected)
		}
	}
}

func TestReleaseWorkflowEmbedsSelfHostedTelemetryConfig(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	block := extractJobBlock(t, string(data), "build-and-upload")
	for _, want := range []string{
		"UMAMI_HOST: https://a.kunchenguid.com",
		"UMAMI_WEBSITE_ID: f959e889-92f5-4121-8a1f-571b10861198",
		"TelemetryHost=${UMAMI_HOST}",
		"TelemetryWebsiteID=${UMAMI_WEBSITE_ID}",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("build-and-upload must contain %q", want)
		}
	}
}

// Partial-release protection: release-please must create drafts so that a
// release is never marked "latest" until all binaries and checksums are
// uploaded. A separate finalize job gates the promotion on every asset job
// succeeding.
func TestReleasePleaseConfigCreatesDrafts(t *testing.T) {
	data, err := os.ReadFile("release-please-config.json")
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		Packages map[string]struct {
			Draft bool `json:"draft"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	pkg, ok := cfg.Packages["."]
	if !ok {
		t.Fatalf("release-please config missing '.' package")
	}
	if !pkg.Draft {
		t.Fatalf("release-please must create releases as drafts; partial releases would otherwise be marked latest before binaries are uploaded")
	}
}

func TestReleasePleaseConfigForcesTagCreation(t *testing.T) {
	data, err := os.ReadFile("release-please-config.json")
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg struct {
		Packages map[string]struct {
			ForceTagCreation bool `json:"force-tag-creation"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	pkg, ok := cfg.Packages["."]
	if !ok {
		t.Fatalf("release-please config missing '.' package")
	}
	if !pkg.ForceTagCreation {
		t.Fatalf("release-please config must force tag creation so an existing GitHub release cannot silently prevent the tag from being recreated")
	}
}

func TestReleaseWorkflowDoesNotOverrideReleaseType(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	block := extractJobBlock(t, content, "release-please")
	if strings.Contains(block, "release-type:") {
		t.Fatalf("release workflow must not override release-type; release-please should read it from release-please-config.json")
	}
	if !strings.Contains(block, "config-file: release-please-config.json") {
		t.Fatalf("release workflow must point release-please at release-please-config.json")
	}
}

func TestReleaseWorkflowPublishesPrereleaseOnlyAfterAssetsComplete(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	content := string(data)

	block := extractJobBlock(t, content, "finalize")

	required := []string{
		"!cancelled()",
		"needs.release-please.result == 'success'",
		"needs.build-and-upload.result == 'success'",
		"needs.checksums.result == 'success'",
		"needs.release-please.outputs.release_created == 'true'",
		"gh release edit",
		"--draft=false",
		"--prerelease=true",
	}
	for _, req := range required {
		if !strings.Contains(block, req) {
			t.Fatalf("finalize job must contain %q so a draft is only published as prerelease after every asset job succeeds", req)
		}
	}
	if strings.Contains(block, "--latest=true") {
		t.Fatalf("finalize job must not auto-promote to latest; latest is set manually")
	}

	for _, dep := range []string{"release-please", "build-and-upload", "checksums"} {
		if !strings.Contains(block, "- "+dep) {
			t.Fatalf("finalize job must declare %q in needs so its gate sees all upstream results", dep)
		}
	}
}

func TestExtractJobBlockHandlesCRLF(t *testing.T) {
	lf := "jobs:\n  foo:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo foo\n  bar:\n    runs-on: ubuntu-latest\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")

	block := extractJobBlock(t, crlf, "foo")
	if !strings.Contains(block, "echo foo") {
		t.Fatalf("CRLF block missing foo body: %q", block)
	}
	if strings.Contains(block, "bar:") {
		t.Fatalf("CRLF block must stop before next job: %q", block)
	}
}

func extractJobBlock(t *testing.T, content, name string) string {
	t.Helper()
	content = strings.ReplaceAll(content, "\r\n", "\n")
	header := "\n  " + name + ":\n"
	start := strings.Index(content, header)
	if start < 0 {
		t.Fatalf("could not locate %s job in workflow", name)
	}
	rest := content[start+len(header):]
	idx := 0
	for {
		next := strings.Index(rest[idx:], "\n  ")
		if next < 0 {
			return rest
		}
		pos := idx + next + 1
		if pos+2 >= len(rest) {
			return rest
		}
		ch := rest[pos+2]
		if ch != ' ' && ch != '#' && ch != '\n' {
			return rest[:pos]
		}
		idx = pos + 1
	}
}
