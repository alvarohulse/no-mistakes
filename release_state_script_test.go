package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestResolveReleaseState(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("release-state resolver requires a POSIX shell")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("release-state resolver requires jq")
	}

	t.Run("accepts a newly created exact draft", func(t *testing.T) {
		fixture := newReleaseStateFixture(t, true)
		outputs, output, err := fixture.run("true", "v1.2.3", "1.2.3", releaseStateDraftJSON())
		if err != nil {
			t.Fatalf("resolve newly created draft: %v\n%s", err, output)
		}
		fixture.assertRelease(t, outputs)
	})

	t.Run("recovers an exact draft on a full rerun", func(t *testing.T) {
		fixture := newReleaseStateFixture(t, true)
		outputs, output, err := fixture.run("", "", "", releaseStateDraftJSON())
		if err != nil {
			t.Fatalf("recover exact draft: %v\n%s", err, output)
		}
		fixture.assertRelease(t, outputs)
		if calls := fixture.ghCalls(t); calls != "api --paginate --slurp repos/owner/repo/releases?per_page=100" {
			t.Fatalf("hosted release lookup = %q, want paginated release enumeration", calls)
		}
	})

	t.Run("does not recover a tag from an older commit", func(t *testing.T) {
		fixture := newReleaseStateFixture(t, false)
		outputs, output, err := fixture.run("", "", "", releaseStateDraftJSON())
		if err != nil {
			t.Fatalf("ignore stale tag: %v\n%s", err, output)
		}
		if outputs["release_created"] != "false" {
			t.Fatalf("release_created = %q, want false", outputs["release_created"])
		}
		if outputs["release_state"] != "none" {
			t.Fatalf("release_state = %q, want none", outputs["release_state"])
		}
		if calls := fixture.ghCalls(t); calls != "" {
			t.Fatalf("stale tag must not query a hosted release, got %q", calls)
		}
	})

	t.Run("does not recover when the expected tag is absent", func(t *testing.T) {
		fixture := newReleaseStateFixture(t, true)
		guardGit(t, fixture.repo, "tag", "-d", "v1.2.3")
		outputs, output, err := fixture.run("", "", "", releaseStateDraftJSON())
		if err != nil {
			t.Fatalf("ignore absent tag: %v\n%s", err, output)
		}
		if outputs["release_created"] != "false" {
			t.Fatalf("release_created = %q, want false", outputs["release_created"])
		}
		if outputs["release_state"] != "none" {
			t.Fatalf("release_state = %q, want none", outputs["release_state"])
		}
		if calls := fixture.ghCalls(t); calls != "" {
			t.Fatalf("absent tag must not query a hosted release, got %q", calls)
		}
	})

	t.Run("does not rebuild an already published release", func(t *testing.T) {
		fixture := newReleaseStateFixture(t, true)
		release := releaseStateDraftJSON()
		release["draft"] = false
		outputs, output, err := fixture.run("", "", "", release)
		if err != nil {
			t.Fatalf("ignore published release: %v\n%s", err, output)
		}
		if outputs["release_created"] != "false" {
			t.Fatalf("release_created = %q, want false", outputs["release_created"])
		}
		if outputs["release_state"] != "published" {
			t.Fatalf("release_state = %q, want published", outputs["release_state"])
		}
	})

	t.Run("rejects ambiguous hosted state for the exact tag", func(t *testing.T) {
		fixture := newReleaseStateFixture(t, true)
		first := releaseStateDraftJSON()
		second := releaseStateDraftJSON()
		second["id"] = 456
		_, output, err := fixture.runReleases("", "", "", first, second)
		if err == nil {
			t.Fatalf("resolver should reject ambiguous exact-tag releases\n%s", output)
		}
		if !strings.Contains(output, "hosted release state is ambiguous") {
			t.Fatalf("resolver failure = %q, want ambiguous-state error", output)
		}
	})

	t.Run("rejects an action release whose tag is missing", func(t *testing.T) {
		fixture := newReleaseStateFixture(t, true)
		guardGit(t, fixture.repo, "tag", "-d", "v1.2.3")
		_, output, err := fixture.run("true", "v1.2.3", "1.2.3", releaseStateDraftJSON())
		if err == nil {
			t.Fatalf("resolver should reject a newly created release without its tag\n%s", output)
		}
		if !strings.Contains(output, "release-please reported a release without the expected exact tag") {
			t.Fatalf("resolver failure = %q, want missing-tag error", output)
		}
	})

	t.Run("rejects inconsistent release-please outputs", func(t *testing.T) {
		cases := []struct {
			name    string
			created string
			tag     string
			version string
		}{
			{name: "invalid created flag", created: "yes", tag: "v1.2.3", version: "1.2.3"},
			{name: "wrong tag", created: "true", tag: "v9.9.9", version: "1.2.3"},
			{name: "wrong version", created: "true", tag: "v1.2.3", version: "9.9.9"},
			{name: "outputs on false", created: "false", tag: "v1.2.3", version: "1.2.3"},
			{name: "outputs without release", created: "", tag: "v1.2.3", version: "1.2.3"},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				fixture := newReleaseStateFixture(t, true)
				_, output, err := fixture.run(test.created, test.tag, test.version, releaseStateDraftJSON())
				if err == nil {
					t.Fatalf("resolver should reject inconsistent outputs\n%s", output)
				}
				if !strings.Contains(output, "release-please outputs are inconsistent") {
					t.Fatalf("resolver failure = %q, want inconsistent-output error", output)
				}
			})
		}
	})

	t.Run("rejects malformed manifest version", func(t *testing.T) {
		fixture := newReleaseStateFixture(t, true)
		guardWriteFile(t, fixture.repo, ".release-please-manifest.json", "{\".\":\"not a version\"}\n")
		_, output, err := fixture.run("", "", "", releaseStateDraftJSON())
		if err == nil {
			t.Fatalf("resolver should reject malformed manifest\n%s", output)
		}
		if !strings.Contains(output, "release manifest has an invalid root version") {
			t.Fatalf("resolver failure = %q, want manifest error", output)
		}
	})

	t.Run("rejects an untrusted draft release", func(t *testing.T) {
		cases := []struct {
			name   string
			mutate func(map[string]any)
		}{
			{name: "prerelease", mutate: func(release map[string]any) { release["prerelease"] = true }},
			{name: "wrong author", mutate: func(release map[string]any) {
				release["author"] = map[string]any{"login": "maintainer", "type": "User"}
			}},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				fixture := newReleaseStateFixture(t, true)
				release := releaseStateDraftJSON()
				test.mutate(release)
				_, output, err := fixture.run("", "", "", release)
				if err == nil {
					t.Fatalf("resolver should reject untrusted draft\n%s", output)
				}
				if !strings.Contains(output, "exact draft release does not match the trusted release-please contract") {
					t.Fatalf("resolver failure = %q, want trusted-draft error", output)
				}
			})
		}
	})

	t.Run("fails when an exact tag has no hosted release", func(t *testing.T) {
		fixture := newReleaseStateFixture(t, true)
		unrelated := releaseStateDraftJSON()
		unrelated["tag_name"] = "v9.9.9"
		_, output, err := fixture.run("", "", "", unrelated)
		if err == nil {
			t.Fatalf("resolver should reject an exact tag without a release\n%s", output)
		}
		if !strings.Contains(output, "exact release tag exists without a readable hosted release") {
			t.Fatalf("resolver failure = %q, want partial-release error", output)
		}
	})

	t.Run("fails when hosted releases cannot be enumerated", func(t *testing.T) {
		fixture := newReleaseStateFixture(t, true)
		_, output, err := fixture.runWithGHExit("", "", "", 1)
		if err == nil {
			t.Fatalf("resolver should reject an unreadable release inventory\n%s", output)
		}
		if !strings.Contains(output, "could not enumerate hosted releases") {
			t.Fatalf("resolver failure = %q, want release-enumeration error", output)
		}
	})
}

type releaseStateFixture struct {
	repo       string
	triggerSHA string
	fakeGH     string
	ghCallLog  string
}

func newReleaseStateFixture(t *testing.T, tagTrigger bool) *releaseStateFixture {
	t.Helper()
	repo := t.TempDir()
	guardGit(t, repo, "init", "-q")
	guardGit(t, repo, "config", "user.email", "release-state@example.com")
	guardGit(t, repo, "config", "user.name", "Release State Test")
	guardWriteFile(t, repo, ".release-please-manifest.json", "{\".\":\"1.2.3\"}\n")
	guardWriteFile(t, repo, "source.txt", "base\n")
	guardGit(t, repo, "add", ".release-please-manifest.json", "source.txt")
	guardGit(t, repo, "commit", "-q", "-m", "chore: base")
	if !tagTrigger {
		guardGit(t, repo, "tag", "v1.2.3")
	}
	guardWriteFile(t, repo, "source.txt", "release\n")
	guardGit(t, repo, "add", "source.txt")
	guardGit(t, repo, "commit", "-q", "-m", "chore(main): release 1.2.3")
	triggerSHA := strings.TrimSpace(guardGit(t, repo, "rev-parse", "HEAD"))
	if tagTrigger {
		guardGit(t, repo, "tag", "v1.2.3")
	}

	fakeBin := t.TempDir()
	fakeGH := filepath.Join(fakeBin, "gh")
	ghCallLog := filepath.Join(fakeBin, "gh-calls")
	guardWriteFile(t, fakeBin, "gh", "#!/bin/sh\ncat \"$FAKE_RELEASE_JSON\"\n")
	if err := os.Chmod(fakeGH, 0o755); err != nil {
		t.Fatalf("make fake gh executable: %v", err)
	}
	return &releaseStateFixture{repo: repo, triggerSHA: triggerSHA, fakeGH: fakeGH, ghCallLog: ghCallLog}
}

func (f *releaseStateFixture) run(actionCreated, actionTag, actionVersion string, release map[string]any) (map[string]string, string, error) {
	return f.runReleases(actionCreated, actionTag, actionVersion, release)
}

func (f *releaseStateFixture) runReleases(actionCreated, actionTag, actionVersion string, releases ...map[string]any) (map[string]string, string, error) {
	data, err := json.Marshal([][]map[string]any{releases})
	if err != nil {
		return nil, "", err
	}
	releaseFile := filepath.Join(filepath.Dir(f.fakeGH), "release.json")
	if err := os.WriteFile(releaseFile, data, 0o644); err != nil {
		return nil, "", err
	}
	return f.runCommand(actionCreated, actionTag, actionVersion, releaseFile, 0)
}

func (f *releaseStateFixture) runWithGHExit(actionCreated, actionTag, actionVersion string, exitCode int) (map[string]string, string, error) {
	return f.runCommand(actionCreated, actionTag, actionVersion, "", exitCode)
}

func (f *releaseStateFixture) runCommand(actionCreated, actionTag, actionVersion, releaseFile string, exitCode int) (map[string]string, string, error) {
	outputFile := filepath.Join(f.repo, "github-output")
	if err := os.Remove(outputFile); err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	if err := os.Remove(f.ghCallLog); err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	script, err := filepath.Abs(filepath.Join("scripts", "resolve-release-state.sh"))
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", script, "owner/repo", f.triggerSHA, actionCreated, actionTag, actionVersion, outputFile)
	cmd.Dir = f.repo
	cmd.Env = append(guardCommandEnv(),
		"PATH="+filepath.Dir(f.fakeGH)+":"+os.Getenv("PATH"),
		"FAKE_RELEASE_JSON="+releaseFile,
		"FAKE_GH_EXIT="+strconv.Itoa(exitCode),
		"FAKE_GH_CALLS="+f.ghCallLog,
	)
	guardWriteFileForCommand := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$FAKE_GH_CALLS\"\nif [ \"${FAKE_GH_EXIT:-0}\" -ne 0 ]; then exit \"$FAKE_GH_EXIT\"; fi\ncat \"$FAKE_RELEASE_JSON\"\n"
	if err := os.WriteFile(f.fakeGH, []byte(guardWriteFileForCommand), 0o755); err != nil {
		return nil, "", err
	}
	output, runErr := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return nil, string(output), ctx.Err()
	}
	outputs := map[string]string{}
	if data, readErr := os.ReadFile(outputFile); readErr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				outputs[key] = value
			}
		}
	}
	return outputs, string(output), runErr
}

func (f *releaseStateFixture) ghCalls(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.ghCallLog)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read fake gh calls: %v", err)
	}
	return strings.TrimSpace(string(data))
}

func (f *releaseStateFixture) assertRelease(t *testing.T, outputs map[string]string) {
	t.Helper()
	want := map[string]string{
		"release_state":   "draft",
		"release_created": "true",
		"tag_name":        "v1.2.3",
		"version":         "1.2.3",
		"release_sha":     f.triggerSHA,
	}
	for key, value := range want {
		if outputs[key] != value {
			t.Errorf("%s = %q, want %q", key, outputs[key], value)
		}
	}
}

func releaseStateDraftJSON() map[string]any {
	return map[string]any{
		"id":         123,
		"tag_name":   "v1.2.3",
		"draft":      true,
		"prerelease": false,
		"author": map[string]any{
			"login": "github-actions[bot]",
			"type":  "Bot",
		},
	}
}
