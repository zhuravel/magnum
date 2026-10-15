package config

import (
	"strings"
	"testing"
	"time"
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

// The churn and age rules of a full pass have their defaults (500 lines, 10
// files, 12h), 0 or "0" turns each one off alone, and a negative value is
// refused.
func TestFullPassChurnAndAgeKeysDefaultAndZeroTurnsEachOff(t *testing.T) {
	for _, p := range []Pipeline{Defaults().Pipeline, mustLoad(t, map[string]string{"config.toml": minimalConfig}).Pipeline} {
		if p.FullPassLines != 500 || p.FullPassFiles != 10 || p.FullPassAfter.Duration != 12*time.Hour {
			t.Fatalf("full_pass_lines %d, full_pass_files %d, full_pass_after %s: want 500, 10, 12h",
				p.FullPassLines, p.FullPassFiles, p.FullPassAfter.Duration)
		}
	}
	off := mustLoad(t, map[string]string{"config.toml": "[pipeline]\nfull_pass_lines = 0\nfull_pass_files = 0\nfull_pass_after = \"0\"\n" + minimalConfig}).Pipeline
	if off.FullPassLines != 0 || off.FullPassFiles != 0 || off.FullPassAfter.Duration != 0 || off.FullPassEvery != 10 {
		t.Fatalf("zeros load as %d, %d, %s (full_pass_every %d): want each rule off alone",
			off.FullPassLines, off.FullPassFiles, off.FullPassAfter.Duration, off.FullPassEvery)
	}
	for _, bad := range []string{"full_pass_lines = -1", "full_pass_files = -1", `full_pass_after = "-1h"`} {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": "[pipeline]\n" + bad + "\n" + minimalConfig})
		key, _, _ := strings.Cut(bad, " ")
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s: err = %v, want one naming %s", bad, err, key)
		}
	}
}
