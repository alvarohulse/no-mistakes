package daemon

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
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

	const prefix = "powershell.exe -NoLogo -NoProfile -NonInteractive -EncodedCommand "
	if !strings.HasPrefix(taskCommand, prefix) {
		t.Fatalf("task command = %q, want encoded PowerShell action", taskCommand)
	}
	script := decodePowerShellTaskCommand(t, strings.TrimPrefix(taskCommand, prefix))
	want := "$env:NM_REPO_CONFIG='" + strings.ReplaceAll(machinePath, "'", "''") + "'; & '" + strings.ReplaceAll(exe, "'", "''") + "' 'daemon' 'run' '--root' '" + strings.ReplaceAll(p.Root(), "'", "''") + "'; exit $LASTEXITCODE"
	if script != want {
		t.Fatalf("decoded task script = %q, want %q", script, want)
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
	if err := os.Unsetenv(machineRepoConfigEnv); err != nil {
		t.Fatal(err)
	}
	if err := installWindowsTask(p, exe); err != nil {
		t.Fatal(err)
	}

	if len(taskCommands) != 2 {
		t.Fatalf("task create commands = %v, want set and unset refreshes", taskCommands)
	}
	if !strings.Contains(taskCommands[0], "-EncodedCommand") {
		t.Fatalf("set refresh did not carry machine config: %q", taskCommands[0])
	}
	wantUnset := strconv.Quote(exe) + " daemon run --root " + strconv.Quote(p.Root())
	if taskCommands[1] != wantUnset {
		t.Fatalf("unset refresh command = %q, want direct action %q", taskCommands[1], wantUnset)
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

func decodePowerShellTaskCommand(t *testing.T, encoded string) string {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(data)%2 != 0 {
		t.Fatalf("encoded PowerShell command has odd byte length %d", len(data))
	}
	codeUnits := make([]uint16, len(data)/2)
	for i := range codeUnits {
		codeUnits[i] = binary.LittleEndian.Uint16(data[i*2:])
	}
	return string(utf16.Decode(codeUnits))
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
