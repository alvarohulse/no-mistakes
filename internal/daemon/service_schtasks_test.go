package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestInstallWindowsTaskUsesDirectActionWithoutProxy(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm O'Brien & home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	exe := `C:\Program Files\O'Brien & Sons\no-mistakes.exe`
	t.Setenv("HTTPS_PROXY", "http://user:secret@proxy.example")

	var taskCommand string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		if name == "schtasks" && len(args) > 0 && args[0] == "/Create" {
			taskCommand = args[len(args)-1]
		}
		return nil, nil
	}
	if err := installWindowsTask(p, exe); err != nil {
		t.Fatal(err)
	}

	want := strconv.Quote(exe) + " daemon run --root " + strconv.Quote(p.Root())
	if taskCommand != want {
		t.Fatalf("task command = %q, want direct action %q", taskCommand, want)
	}
	if length := len(utf16.Encode([]rune(taskCommand))); length > windowsTaskCommandMaxUTF16 {
		t.Fatalf("task command length = %d, maximum %d", length, windowsTaskCommandMaxUTF16)
	}
	launchers, err := filepath.Glob(filepath.Join(p.Root(), windowsDaemonLauncherPrefix+"*"+windowsDaemonLauncherSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if len(launchers) != 0 {
		t.Fatalf("direct action should write no launchers, got %v", launchers)
	}
	for _, forbidden := range []string{"HTTPS_PROXY", "user:secret"} {
		if strings.Contains(taskCommand, forbidden) {
			t.Fatalf("Windows task leaked proxy value %q: command=%q", forbidden, taskCommand)
		}
	}
}

func TestInstallWindowsTaskRemovesStaleLaunchersAfterReplacement(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	exe := `C:\Program Files\no-mistakes\no-mistakes.exe`

	// A launcher left behind by an older binary whose task action set the
	// retired machine-local repo-config opt-in.
	staleLauncher := filepath.Join(p.Root(), windowsDaemonLauncherPrefix+"stale"+windowsDaemonLauncherSuffix)
	if err := os.WriteFile(staleLauncher, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherLauncher := filepath.Join(t.TempDir(), windowsDaemonLauncherPrefix+"other"+windowsDaemonLauncherSuffix)
	if err := os.WriteFile(otherLauncher, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}

	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		return nil, nil
	}
	if err := installWindowsTask(p, exe); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(staleLauncher); !os.IsNotExist(err) {
		t.Fatalf("stale launcher survived reinstall: %v", err)
	}
	if data, err := os.ReadFile(otherLauncher); err != nil || string(data) != "other" {
		t.Fatalf("reinstall touched another NM_HOME launcher: data=%q err=%v", data, err)
	}
}

func TestInstallWindowsTaskKeepsLauncherWhenReplacementFails(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	staleLauncher := filepath.Join(p.Root(), windowsDaemonLauncherPrefix+"stale"+windowsDaemonLauncherSuffix)
	if err := os.WriteFile(staleLauncher, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		if name == "schtasks" && len(args) > 0 && args[0] == "/Create" {
			return nil, fmt.Errorf("replacement failed")
		}
		return nil, nil
	}

	err := installWindowsTask(p, `C:\no-mistakes.exe`)
	if err == nil || !strings.Contains(err.Error(), "replacement failed") {
		t.Fatalf("replacement error = %v", err)
	}
	if _, err := os.Stat(staleLauncher); err != nil {
		t.Fatalf("failed replacement removed the launcher the live task may still point at: %v", err)
	}
}

func TestInstallWindowsTaskRejectsActionOverLimit(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), strings.Repeat("long-root-", 24)))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	called := false
	serviceCommandRunner = func(string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}

	err := installWindowsTask(p, `C:\no-mistakes.exe`)
	if err == nil || !strings.Contains(err.Error(), "move NM_HOME to a shorter path") {
		t.Fatalf("install error = %v, want actionable action-length refusal", err)
	}
	if called {
		t.Fatal("schtasks ran with an over-limit action")
	}
}

func TestStartInstallsWindowsTaskAndStartsManagedDaemon(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	exe := `C:\Program Files\no-mistakes\no-mistakes.exe`
	serviceExecutablePath = func() (string, error) { return exe, nil }

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		// Simulate fresh-install: the legacy unsuffixed task is absent, so
		// the pre-install cleanup query fails and cleanupLegacyWindowsTask
		// returns without issuing End/Delete.
		if name == "schtasks" && len(args) >= 4 && args[0] == "/Query" && args[2] == legacyWindowsTaskName && args[3] == "/XML" {
			return nil, fmt.Errorf("task not found")
		}
		return nil, nil
	}
	checks := 0
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		checks++
		return checks >= 2, nil
	}

	if err := Start(p); err != nil {
		t.Fatal(err)
	}

	wantTaskCommand := strconv.Quote(exe) + " daemon run --root " + strconv.Quote(p.Root())
	wantQueryLegacy := "schtasks /Query /TN " + legacyWindowsTaskName + " /XML"
	wantCreate := "schtasks /Create /TN " + windowsTaskName(p) +
		" /SC ONLOGON /RL LIMITED /F /TR " + wantTaskCommand
	wantRun := "schtasks /Run /TN " + windowsTaskName(p)
	if len(commands) != 3 {
		t.Fatalf("expected schtasks create, legacy query, and run, got %v", commands)
	}
	if commands[0] != wantCreate {
		t.Fatalf("create command = %q, want %q", commands[0], wantCreate)
	}
	if commands[1] != wantQueryLegacy {
		t.Fatalf("legacy query command = %q, want %q", commands[1], wantQueryLegacy)
	}
	if commands[2] != wantRun {
		t.Fatalf("run command = %q, want %q", commands[2], wantRun)
	}
}

func TestInstallWindowsTaskDoesNotRemoveLegacyTaskForDifferentRoot(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		if name == "schtasks" && len(args) >= 4 && args[0] == "/Query" && args[2] == legacyWindowsTaskName && args[3] == "/XML" {
			otherRoot := filepath.Join(t.TempDir(), "other-nm-home")
			return []byte(`<Task><Exec><Command>C:\nm.exe</Command><Arguments>daemon run --root ` + otherRoot + `</Arguments></Exec></Task>`), nil
		}
		return nil, nil
	}

	if err := installWindowsTask(p, `C:\Program Files\no-mistakes\no-mistakes.exe`); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 {
		t.Fatalf("install should not end or delete unrelated legacy task, got commands %v", commands)
	}
	if commands[1] != "schtasks /Query /TN "+legacyWindowsTaskName+" /XML" {
		t.Fatalf("legacy query command = %q", commands[1])
	}
}

func TestInstallWindowsTaskKeepsLegacyTaskOnCreateFailure(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"

	var commands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		command := name + " " + strings.Join(args, " ")
		commands = append(commands, command)
		if name == "schtasks" && len(args) > 0 && args[0] == "/Create" {
			return nil, fmt.Errorf("create failed")
		}
		return nil, nil
	}

	err := installWindowsTask(p, `C:\Program Files\no-mistakes\no-mistakes.exe`)
	if err == nil {
		t.Fatal("installWindowsTask should fail when schtasks create fails")
	}
	for _, command := range commands {
		if strings.Contains(command, "/End /TN "+legacyWindowsTaskName) || strings.Contains(command, "/Delete /TN "+legacyWindowsTaskName+" /F") {
			t.Fatalf("legacy cleanup should not run before successful scoped install, got %q", command)
		}
	}
}

func TestWindowsManagedDaemonStateUsesRunGeneration(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"

	var command string
	output := "3|100|0\r\n"
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		command = name + " " + strings.Join(args, " ")
		return []byte(output), nil
	}

	launch, err := managedDaemonLaunch(p)
	if err != nil {
		t.Fatal(err)
	}
	if launch.windowsRunGeneration != "100" {
		t.Fatalf("launch generation = %q, want 100", launch.windowsRunGeneration)
	}
	state, err := managedDaemonServiceState(p, launch)
	if err != nil {
		t.Fatal(err)
	}
	if state != managedServiceUnknown {
		t.Fatalf("unchanged Ready generation state = %v, want unknown", state)
	}

	output = "3|101|0\r\n"
	state, err = managedDaemonServiceState(p, launch)
	if err != nil {
		t.Fatal(err)
	}
	if state != managedServiceExited {
		t.Fatalf("completed new generation state = %v, want exited", state)
	}
	if !strings.HasPrefix(command, "powershell.exe -NoLogo -NoProfile -NonInteractive -Command ") {
		t.Fatalf("unexpected task state command: %q", command)
	}
	if strings.Contains(command, "/FO LIST") {
		t.Fatalf("task state command uses localized schtasks output: %q", command)
	}
}
