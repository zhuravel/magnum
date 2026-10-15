package config

import (
	"strings"
	"testing"
)

// Every fifth automatic re-review is an audit pass unless the config says
// otherwise; 0 turns audit passes off and a negative count is refused.
func TestAuditEveryDefaultsToFiveAndZeroTurnsItOff(t *testing.T) {
	if n := Defaults().Pipeline.AuditEvery; n != 5 {
		t.Fatalf("Defaults(): audit_every = %d, want 5", n)
	}
	if n := mustLoad(t, map[string]string{"config.toml": minimalConfig}).Pipeline.AuditEvery; n != 5 {
		t.Fatalf("unset: audit_every = %d, want 5", n)
	}
	if n := mustLoad(t, map[string]string{"config.toml": "[pipeline]\naudit_every = 0\n" + minimalConfig}).Pipeline.AuditEvery; n != 0 {
		t.Fatalf("audit_every = 0 loads as %d, want 0 (off)", n)
	}
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": "[pipeline]\naudit_every = -1\n" + minimalConfig})
	if err == nil || !strings.Contains(err.Error(), "audit_every") {
		t.Fatalf("a negative audit_every: err = %v, want one naming it", err)
	}
}
