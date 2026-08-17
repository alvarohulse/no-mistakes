package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestPrintRunLineIncludesCopyableIDAndPinState(t *testing.T) {
	pinnedAt := int64(1)
	run := &db.Run{ID: "01K2EXAMPLE000000000000000", Branch: "feature/pin", HeadSHA: "abcdef123456", PinnedAt: &pinnedAt}
	var output bytes.Buffer
	printRunLine(&output, run)
	if got := output.String(); !strings.Contains(got, run.ID) || !strings.Contains(got, "pinned") {
		t.Fatalf("run line does not expose pin command input and state: %q", got)
	}
}

func TestRunsPinAndUnpinMutateOnlyTheNamedRun(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepoWithID("repo-1", "/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/pin", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	other, err := database.InsertRun(repo.ID, "feature/other", "ghi", "def")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := executeCmd("runs", "pin", run.ID)
	if err != nil {
		t.Fatalf("runs pin: %v\n%s", err, out)
	}
	if !strings.Contains(out, "pinned run "+run.ID) {
		t.Fatalf("runs pin output = %q", out)
	}
	database, err = db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	untouched, err := database.GetRun(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.PinnedAt == nil || untouched.PinnedAt != nil {
		t.Fatalf("pin state = named %v, other %v", pinned.PinnedAt, untouched.PinnedAt)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	out, err = executeCmd("runs", "unpin", run.ID)
	if err != nil {
		t.Fatalf("runs unpin: %v\n%s", err, out)
	}
	if !strings.Contains(out, "unpinned run "+run.ID) {
		t.Fatalf("runs unpin output = %q", out)
	}
	database, err = db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	unpinned, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unpinned.PinnedAt != nil {
		t.Fatalf("unpin left pinned_at = %v", unpinned.PinnedAt)
	}
}

func TestRunsPinRejectsUnknownRun(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	out, err := executeCmd("runs", "pin", "missing-run")
	if err == nil {
		t.Fatalf("runs pin missing run succeeded: %s", out)
	}
	if !strings.Contains(err.Error(), "run \"missing-run\" not found") {
		t.Fatalf("runs pin error = %v", err)
	}
}
