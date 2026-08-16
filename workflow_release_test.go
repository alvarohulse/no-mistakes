package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
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
	releaseBlock := extractJobBlock(t, content, "release-please")
	block := extractJobBlock(t, content, "release-pr-provenance")
	if !strings.Contains(content, "statuses: write") {
		t.Fatal("release workflow must be able to attest the exact generated head")
	}
	if !strings.Contains(releaseBlock, "node .github/release-please-reproducer/create-release.mjs") {
		t.Fatal("release-please must create releases through the exact-SHA runner")
	}
	if strings.Contains(releaseBlock, "googleapis/release-please-action@") {
		t.Fatal("release-please must not use the live-branch action for release mutation")
	}
	if !strings.Contains(block, "needs: release-please") {
		t.Fatal("release provenance must run after release creation")
	}
	for _, required := range []string{
		"steps.release_pr.outputs.prs_created == 'true'",
		"RELEASE_PR: ${{ steps.release_pr.outputs.pr }}",
		"HEAD_RETAINED: ${{ steps.release_pr.outputs.head_retained }}",
		"RETAINED_HEAD_SHA: ${{ steps.release_pr.outputs.retained_head_sha }}",
		"actions/checkout@d23441a48e516b6c34aea4fa41551a30e30af803",
		"actions/setup-node@249970729cb0ef3589644e2896645e5dc5ba9c38",
		"node-version: 20",
		"working-directory: .github/release-please-reproducer",
		"npm ci --ignore-scripts",
		"expected_head_ref=release-please--branches--main",
		`repos/${GITHUB_REPOSITORY}/git/ref/heads/main`,
		`node .github/release-please-reproducer/produce-release-pr.mjs \`,
		`repos/${GITHUB_REPOSITORY}/pulls/${pr_number}`,
		`refs/pull/${pr_number}/head`,
		`[ "$base_sha" != "$GITHUB_SHA" ]`,
		`[ "$fetched_head" != "$head_sha" ]`,
		`git show "${GITHUB_SHA}:scripts/guard-generated-files.sh"`,
		`git show "${GITHUB_SHA}:scripts/verify-release-please-output.sh"`,
		`sh "$trusted_output_verifier" "$head_sha" "$expected_output_dir"`,
		`[ "$RETAINED_HEAD_SHA" != "$head_sha" ]`,
		`"$GITHUB_SHA"`,
		`"$head_sha"`,
		"https://github.com/alvarohulse/no-mistakes.git",
		`repos/${GITHUB_REPOSITORY}/statuses/${head_sha}`,
		`refs/tags/no-mistakes/generated-file-provenance/${head_sha}`,
		`repos/${GITHUB_REPOSITORY}/git/refs`,
		`repos/${GITHUB_REPOSITORY}/git/ref/tags/no-mistakes/generated-file-provenance/${head_sha}`,
		"Generated files must not be hand-edited",
		"latest_head_sha",
	} {
		if !strings.Contains(block, required) {
			t.Errorf("release provenance job must contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"steps.release.outputs.prs_created",
		"steps.release.outputs.pr.sha",
		"github.event.pull_request",
		"reproduce.mjs",
	} {
		if strings.Contains(block, forbidden) {
			t.Errorf("release provenance job must not trust unavailable or unrelated metadata %q", forbidden)
		}
	}

	orderedOperations := []string{
		"node .github/release-please-reproducer/produce-release-pr.mjs",
		`require_live_main "before attestation verification"`,
		`sh "$trusted_script"`,
		`sh "$trusted_output_verifier" "$head_sha" "$expected_output_dir"`,
		"latest_head_sha=$(gh api",
		`require_live_main "before attestation publication"`,
		`attestation_ref="refs/tags/no-mistakes/generated-file-provenance/${head_sha}"`,
		`repos/${GITHUB_REPOSITORY}/statuses/${head_sha}`,
	}
	previous := -1
	for _, operation := range orderedOperations {
		position := strings.Index(block, operation)
		if position == -1 {
			t.Fatalf("release workflow must contain ordered operation %q", operation)
		}
		if position <= previous {
			t.Fatalf("release workflow operation %q is out of order", operation)
		}
		previous = position
	}
}

func TestReleasePleaseProducerCapturesPublishedOutput(t *testing.T) {
	producer := guardReadFile(t, ".github/release-please-reproducer/produce-release-pr.mjs")

	for _, required := range []string{
		"{ alwaysUpdate: true }",
		"github.buildChangeSet = async",
		"github.updatePullRequest = async",
		"capturedChangeSets.push(changes)",
		"retainableAttestedHead(number, changes)",
		`TRUSTED_ATTESTATION_REPOSITORY = "alvarohulse/no-mistakes"`,
		`setOutput("head_retained", retainedHeadSHA ? "true" : "false")`,
		"await manifest.createPullRequests()",
		"await github.getBranchSha(baseBranch)",
		"expectedBaseSHA",
		"await writeFile",
		"process.env.GITHUB_OUTPUT",
	} {
		if !strings.Contains(producer, required) {
			t.Errorf("release-please producer must contain %q", required)
		}
	}
	if strings.Contains(producer, "buildPullRequests()") {
		t.Fatal("release-please producer must capture the publishing invocation, not rebuild output afterward")
	}

	intercept := strings.Index(producer, "github.buildChangeSet = async")
	publish := strings.Index(producer, "await manifest.createPullRequests()")
	persist := strings.Index(producer, "await writeFile")
	if intercept == -1 || publish == -1 || persist == -1 || intercept >= publish || publish >= persist {
		t.Fatal("release-please producer must intercept, publish, then persist the exact published output")
	}
}

func TestReleasePleaseExactReleaseMutationBoundary(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("exact release runner tests require Node.js")
	}

	cmd := exec.Command("node", "--test", ".github/release-please-reproducer/exact-release.test.mjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exact release runner tests failed: %v\n%s", err, output)
	}
}

func TestReleaseWorkflowKeepsReleaseCreationIsolatedForRetries(t *testing.T) {
	wf := loadReleaseWorkflowDoc(t)
	releaseJob := wf.Jobs["release-please"]
	if releaseJob == nil {
		t.Fatal("release workflow must define release-please")
	}
	if len(releaseJob.Steps) != 8 {
		t.Fatalf("release-please must contain the release action and deterministic state recovery, found %d steps", len(releaseJob.Steps))
	}
	if releaseJob.Steps[0].Uses != "actions/checkout@d23441a48e516b6c34aea4fa41551a30e30af803" {
		t.Fatalf("release-please must first check out the triggering commit, got %q", releaseJob.Steps[0].Uses)
	}
	if !strings.Contains(releaseJob.Steps[1].Run, "scripts/resolve-release-state.sh") {
		t.Fatal("release-please must resolve exact state before any mutation")
	}
	if !strings.Contains(releaseJob.Steps[2].If, "github.run_attempt != 1") ||
		!strings.Contains(releaseJob.Steps[2].If, "release_state == 'none'") ||
		!strings.Contains(releaseJob.Steps[2].Run, "exit 1") {
		t.Fatal("release-please must fail closed when a rerun has no exact release to recover")
	}
	if releaseJob.Steps[3].Uses != "actions/setup-node@249970729cb0ef3589644e2896645e5dc5ba9c38" {
		t.Fatalf("release-please must use the pinned Node setup action, got %q", releaseJob.Steps[3].Uses)
	}
	if !strings.Contains(releaseJob.Steps[4].Run, "npm ci --ignore-scripts") {
		t.Fatal("release-please must install the pinned base-owned dependency graph")
	}
	if !strings.Contains(releaseJob.Steps[5].If, "github.run_attempt == 1") ||
		!strings.Contains(releaseJob.Steps[5].If, "release_state == 'none'") ||
		!strings.Contains(releaseJob.Steps[5].Run, "create-release.mjs") ||
		!strings.Contains(releaseJob.Steps[5].Run, `"$GITHUB_SHA"`) {
		t.Fatalf("release mutation must be bound to the triggering commit, got step %#v", releaseJob.Steps[5])
	}
	if !strings.Contains(releaseJob.Steps[6].Run, "git fetch --force --tags origin") {
		t.Fatal("release-please must refresh tags created after the triggering commit checkout")
	}
	if !strings.Contains(releaseJob.Steps[7].Run, "scripts/resolve-release-state.sh") {
		t.Fatal("release-please must resolve the exact final state after the release action")
	}

	provenanceJob := wf.Jobs["release-pr-provenance"]
	if provenanceJob == nil {
		t.Fatal("release workflow must isolate pull request provenance")
	}
	provenanceNeeds := provenanceJob.needs()
	if len(provenanceNeeds) != 1 || provenanceNeeds[0] != "release-please" {
		t.Fatalf("release provenance needs = %v, want [release-please]", provenanceNeeds)
	}
	if !strings.Contains(provenanceJob.If, "github.run_attempt == 1") || !strings.Contains(provenanceJob.If, "release_state == 'none'") {
		t.Fatalf("release provenance must skip reruns, draft recovery, and published releases, got if %q", provenanceJob.If)
	}

	for _, name := range []string{"build-darwin", "build-and-upload"} {
		job := wf.Jobs[name]
		if job == nil {
			t.Fatalf("release workflow must define %s", name)
		}
		needs := job.needs()
		if len(needs) != 1 || needs[0] != "release-please" {
			t.Errorf("%s needs = %v, want [release-please] so draft recovery is independent", name, needs)
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

func TestReleaseWorkflowRecoversExactDraftOnFullRerun(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	content := string(data)
	releaseBlock := extractJobBlock(t, content, "release-please")
	for _, required := range []string{
		"release_state: ${{ steps.release_state.outputs.release_state }}",
		"release_created: ${{ steps.release_state.outputs.release_created }}",
		"tag_name: ${{ steps.release_state.outputs.tag_name }}",
		"version: ${{ steps.release_state.outputs.version }}",
		"release_sha: ${{ steps.release_state.outputs.release_sha }}",
		"scripts/resolve-release-state.sh",
		`"${{ steps.release.outputs.release_created }}"`,
		`"${{ steps.release.outputs.tag_name }}"`,
		`"${{ steps.release.outputs.version }}"`,
		"github.run_attempt == 1",
		"steps.existing_release_state.outputs.release_state == 'none'",
	} {
		if !strings.Contains(releaseBlock, required) {
			t.Errorf("release-please job must contain %q", required)
		}
	}

	for _, job := range []string{"build-darwin", "build-and-upload"} {
		block := extractJobBlock(t, content, job)
		if !strings.Contains(block, "ref: ${{ needs.release-please.outputs.release_sha }}") {
			t.Errorf("%s must build the exact recovered release commit", job)
		}
	}
}

func TestReleaseWorkflowPinsEveryActionByCommit(t *testing.T) {
	data, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}

	immutableAction := regexp.MustCompile(`^[^@[:space:]]+@[0-9a-f]{40}$`)
	for lineNumber, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- uses:") {
			continue
		}
		uses := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(trimmed, " #", 2)[0], "- uses:"))
		if !immutableAction.MatchString(uses) {
			t.Errorf("release workflow line %d uses mutable action ref %q", lineNumber+1, uses)
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
	runner := guardReadFile(t, ".github/release-please-reproducer/create-release.mjs")
	if !strings.Contains(runner, `"release-please-config.json"`) {
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
