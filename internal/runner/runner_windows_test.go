//go:build windows

package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPowerShellPrepareExecuteAndSyntaxOnWindows(t *testing.T) {
	options := ExecuteOptions{Timeout: 10 * time.Second}
	prepared, err := Prepare(context.Background(), Command{Run: `Write-Output "ready"`}, Spec{}, options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := prepared.Execute(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !strings.Contains(result.Output, "ready") {
		t.Fatalf("result = %+v", result)
	}

	_, err = Prepare(context.Background(), Command{Run: `if (`}, Spec{}, options)
	if err == nil || !errors.Is(err, ErrInvalidSyntax) {
		t.Fatalf("invalid syntax error = %v", err)
	}
}
