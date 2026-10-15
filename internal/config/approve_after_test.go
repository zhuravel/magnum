package config

import (
	"strings"
	"testing"
)

// approve_after_full_pass defaults to 2, 0 turns the approval hold off, and
// a negative value is refused.
func TestApproveAfterFullPassDefaultsToTwoAndZeroTurnsItOff(t *testing.T) {
	if n := Defaults().Pipeline.ApproveAfterFullPass; n != 2 {
		t.Fatalf("Defaults(): approve_after_full_pass = %d, want 2", n)
	}
	if n := mustLoad(t, map[string]string{"config.toml": minimalConfig}).Pipeline.ApproveAfterFullPass; n != 2 {
		t.Fatalf("unset: approve_after_full_pass = %d, want 2", n)
	}
	if n := mustLoad(t, map[string]string{"config.toml": "[pipeline]\napprove_after_full_pass = 0\n" + minimalConfig}).Pipeline.ApproveAfterFullPass; n != 0 {
		t.Fatalf("approve_after_full_pass = 0 loads as %d, want 0 (off)", n)
	}
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": "[pipeline]\napprove_after_full_pass = -1\n" + minimalConfig})
	if err == nil || !strings.Contains(err.Error(), "approve_after_full_pass") {
		t.Fatalf("a negative approve_after_full_pass: err = %v, want one naming it", err)
	}
}
