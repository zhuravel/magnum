package config

import (
	"strings"
	"testing"
)

// A long-lived PR gets a scheduled full pass every full_pass_every automatic
// re-reviews: 10 by default, 0 turns it off, and a negative count is
// refused.
func TestFullPassEveryDefaultsToTenAndZeroTurnsItOff(t *testing.T) {
	if n := Defaults().Pipeline.FullPassEvery; n != 10 {
		t.Fatalf("Defaults(): full_pass_every = %d, want 10", n)
	}
	if n := mustLoad(t, map[string]string{"config.toml": minimalConfig}).Pipeline.FullPassEvery; n != 10 {
		t.Fatalf("unset: full_pass_every = %d, want 10", n)
	}
	if n := mustLoad(t, map[string]string{"config.toml": "[pipeline]\nfull_pass_every = 0\n" + minimalConfig}).Pipeline.FullPassEvery; n != 0 {
		t.Fatalf("full_pass_every = 0 loads as %d, want 0 (off)", n)
	}
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": "[pipeline]\nfull_pass_every = -1\n" + minimalConfig})
	if err == nil || !strings.Contains(err.Error(), "full_pass_every") {
		t.Fatalf("a negative full_pass_every: err = %v, want one naming it", err)
	}
}
