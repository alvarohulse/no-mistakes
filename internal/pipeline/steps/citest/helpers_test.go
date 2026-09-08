package citest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

func setupStackedRefreshRepo(t *testing.T) (dir, upstream, featureHead string) {
	t.Helper()
	upstream = t.TempDir()
	stepstest.GitCmd(t, upstream, "init", "--bare")

	dir = t.TempDir()
	stepstest.GitCmd(t, dir, "init")
	stepstest.GitCmd(t, dir, "config", "user.name", "test")
	stepstest.GitCmd(t, dir, "config", "user.email", "test@test.com")
	stepstest.GitCmd(t, dir, "checkout", "-b", "main")
	stepstest.GitCmd(t, dir, "remote", "add", "origin", upstream)
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "base")
	stepstest.GitCmd(t, dir, "push", "origin", "main")

	stepstest.GitCmd(t, dir, "checkout", "-b", "dependency")
	if err := os.WriteFile(filepath.Join(dir, "dependency.txt"), []byte("dependency\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "dependency")
	stepstest.GitCmd(t, dir, "push", "origin", "dependency")

	stepstest.GitCmd(t, dir, "checkout", "main")
	stepstest.GitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stepstest.GitCmd(t, dir, "add", "-A")
	stepstest.GitCmd(t, dir, "commit", "-m", "feature")
	stepstest.GitCmd(t, dir, "push", "origin", "feature")
	featureHead = stepstest.GitCmd(t, dir, "rev-parse", "HEAD")
	return dir, upstream, featureHead
}
