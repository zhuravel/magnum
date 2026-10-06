package config

import (
	"strings"
	"testing"
	"time"
)

// A judge resumed after its prompt cache went cold (about 1.5 hours)
// re-reads its whole conversation uncached, so by default a judge idle
// longer than 90 minutes starts fresh; "0" always resumes, and a negative
// value is refused.
func TestJudgeFreshAfterDefaultsTo90MinutesAndZeroAlwaysResumes(t *testing.T) {
	if d := Defaults().Pipeline.JudgeFreshAfter.Duration; d != 90*time.Minute {
		t.Fatalf("Defaults(): judge_fresh_after = %v, want 90m", d)
	}
	if d := mustLoad(t, map[string]string{"config.toml": minimalConfig}).Pipeline.JudgeFreshAfter.Duration; d != 90*time.Minute {
		t.Fatalf("unset: judge_fresh_after = %v, want 90m", d)
	}
	if d := mustLoad(t, map[string]string{"config.toml": "[pipeline]\njudge_fresh_after = \"0\"\n" + minimalConfig}).Pipeline.JudgeFreshAfter.Duration; d != 0 {
		t.Fatalf(`judge_fresh_after = "0" loads as %v, want 0 (always resume)`, d)
	}
	_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": "[pipeline]\njudge_fresh_after = \"-1m\"\n" + minimalConfig})
	if err == nil || !strings.Contains(err.Error(), "judge_fresh_after") {
		t.Fatalf("a negative judge_fresh_after: err = %v, want one naming it", err)
	}
}
