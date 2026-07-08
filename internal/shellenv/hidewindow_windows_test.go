//go:build windows

package shellenv

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHideWindow_SetsCreateNoWindow(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "echo", "hi")
	HideWindow(cmd)
	if cmd.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil after HideWindow")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatal("CREATE_NO_WINDOW flag not set")
	}
}

func TestHideWindow_PreservesExistingFlags(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "echo", "hi")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	HideWindow(cmd)
	if cmd.SysProcAttr.CreationFlags&createNewProcessGroup == 0 {
		t.Fatal("existing CreationFlags were clobbered")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatal("CREATE_NO_WINDOW flag not set")
	}
}

func TestHideWindow_NilCmdDoesNotPanic(t *testing.T) {
	HideWindow(nil)
}
