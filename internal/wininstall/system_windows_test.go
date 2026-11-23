//go:build windows

package wininstall

import (
	"strings"
	"testing"
)

// cmd.exe expands %VAR% even inside quotes, so a path with % is refused, not mangled.
func TestDeleteAfterExitRefusesPercent(t *testing.T) {
	err := osSystem{}.DeleteAfterExit(`C:\Users\a%PATH%b\pdb-print.exe.pdb-print.old.1`)
	if err == nil || !strings.Contains(err.Error(), "%") {
		t.Fatalf("err = %v", err)
	}
}
