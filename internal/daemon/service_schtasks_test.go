package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestInstallWindowsTaskForwardsMachineRepoConfigWithoutProxy(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm O'Brien & home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	exe := `C:\Program Files\O'Brien & Sons\no-mistakes.exe`
	machinePath := filepath.Join(t.TempDir(), "Config O'Brien & 100%.yaml")
	t.Setenv(machineRepoConfigEnv, machinePath)
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

	const prefix = "powershell.exe -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "
	if !strings.HasPrefix(taskCommand, prefix) {
		t.Fatalf("task command = %q, want generated-launcher action", taskCommand)
	}
	launchers, err := filepath.Glob(filepath.Join(p.Root(), "daemon-task-launcher-*.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(launchers) != 1 {
		t.Fatalf("generated launchers = %v, want exactly one", launchers)
	}
	if taskCommand != prefix+quoteWindowsTaskArg(launchers[0]) {
		t.Fatalf("task command = %q, want launcher %q", taskCommand, launchers[0])
	}
	if length := len(utf16.Encode([]rune(taskCommand))); length > windowsTaskCommandMaxUTF16 {
		t.Fatalf("task command length = %d, maximum %d", length, windowsTaskCommandMaxUTF16)
	}
	data, err := os.ReadFile(launchers[0])
	if err != nil {
		t.Fatal(err)
	}
	script := strings.TrimPrefix(string(data), "\xEF\xBB\xBF")
	want := "$env:NM_REPO_CONFIG='" + strings.ReplaceAll(machinePath, "'", "''") + "'; & '" + strings.ReplaceAll(exe, "'", "''") + "' 'daemon' 'run' '--root' '" + strings.ReplaceAll(p.Root(), "'", "''") + "'; exit $LASTEXITCODE"
	if script != want {
		t.Fatalf("launcher script = %q, want %q", script, want)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(launchers[0])
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("launcher mode = %o, want 0600", info.Mode().Perm())
		}
	}
	for _, forbidden := range []string{"HTTPS_PROXY", "user:secret"} {
		if strings.Contains(taskCommand, forbidden) || strings.Contains(script, forbidden) {
			t.Fatalf("Windows task leaked proxy value %q: command=%q script=%q", forbidden, taskCommand, script)
		}
	}
}

func TestInstallWindowsTaskRemovesMachineRepoConfigWhenUnset(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	exe := `C:\Program Files\no-mistakes\no-mistakes.exe`
	t.Setenv(machineRepoConfigEnv, filepath.Join(t.TempDir(), "repo.yaml"))

	var taskCommands []string
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		if name == "schtasks" && len(args) > 0 && args[0] == "/Create" {
			taskCommands = append(taskCommands, args[len(args)-1])
		}
		return nil, nil
	}
	if err := installWindowsTask(p, exe); err != nil {
		t.Fatal(err)
	}
	launchers, err := filepath.Glob(filepath.Join(p.Root(), windowsDaemonLauncherPrefix+"*"+windowsDaemonLauncherSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if len(launchers) != 1 {
		t.Fatalf("set refresh launchers = %v, want one", launchers)
	}
	otherLauncher := filepath.Join(t.TempDir(), windowsDaemonLauncherPrefix+"other"+windowsDaemonLauncherSuffix)
	if err := os.WriteFile(otherLauncher, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv(machineRepoConfigEnv); err != nil {
		t.Fatal(err)
	}
	if err := installWindowsTask(p, exe); err != nil {
		t.Fatal(err)
	}

	if len(taskCommands) != 2 {
		t.Fatalf("task create commands = %v, want set and unset refreshes", taskCommands)
	}
	if !strings.Contains(taskCommands[0], "-File") {
		t.Fatalf("set refresh did not carry machine config: %q", taskCommands[0])
	}
	wantUnset := strconv.Quote(exe) + " daemon run --root " + strconv.Quote(p.Root())
	if taskCommands[1] != wantUnset {
		t.Fatalf("unset refresh command = %q, want direct action %q", taskCommands[1], wantUnset)
	}
	launchers, err = filepath.Glob(filepath.Join(p.Root(), windowsDaemonLauncherPrefix+"*"+windowsDaemonLauncherSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if len(launchers) != 0 {
		t.Fatalf("unset refresh left stale launchers: %v", launchers)
	}
	if data, err := os.ReadFile(otherLauncher); err != nil || string(data) != "other" {
		t.Fatalf("unset refresh touched another NM_HOME launcher: data=%q err=%v", data, err)
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
	t.Setenv(machineRepoConfigEnv, filepath.Join(t.TempDir(), "repo.yaml"))
	createCalls := 0
	serviceCommandRunner = func(name string, args ...string) ([]byte, error) {
		if name == "schtasks" && len(args) > 0 && args[0] == "/Create" {
			createCalls++
			if createCalls == 2 {
				return nil, fmt.Errorf("replacement failed")
			}
		}
		return nil, nil
	}
	if err := installWindowsTask(p, `C:\no-mistakes.exe`); err != nil {
		t.Fatal(err)
	}
	launchers, err := filepath.Glob(filepath.Join(p.Root(), windowsDaemonLauncherPrefix+"*"+windowsDaemonLauncherSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if len(launchers) != 1 {
		t.Fatalf("initial launchers = %v, want one", launchers)
	}

	if err := os.Unsetenv(machineRepoConfigEnv); err != nil {
		t.Fatal(err)
	}
	err = installWindowsTask(p, `C:\no-mistakes.exe`)
	if err == nil || !strings.Contains(err.Error(), "replacement failed") {
		t.Fatalf("replacement error = %v", err)
	}
	if _, err := os.Stat(launchers[0]); err != nil {
		t.Fatalf("failed replacement removed live launcher: %v", err)
	}
}

func TestInstallWindowsTaskRejectsMachineConfigControlCharacters(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), "nm-home"))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	t.Setenv(machineRepoConfigEnv, filepath.Join(t.TempDir(), "repo.yaml")+"\nWrite-Output injected")
	called := false
	serviceCommandRunner = func(string, ...string) ([]byte, error) {
		called = true
		return nil, nil
	}

	err := installWindowsTask(p, `C:\Program Files\no-mistakes\no-mistakes.exe`)
	if err == nil || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("install error = %v, want control-character refusal", err)
	}
	if called {
		t.Fatal("schtasks ran after unsafe machine config was rejected")
	}
}

func TestInstallWindowsTaskRejectsLauncherActionOverLimit(t *testing.T) {
	p := paths.WithRoot(filepath.Join(t.TempDir(), strings.Repeat("long-root-", 15)))
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	cleanup := stubServiceRuntime(t)
	defer cleanup()
	runtimeGOOS = "windows"
	t.Setenv(machineRepoConfigEnv, filepath.Join(t.TempDir(), "repo.yaml"))
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
