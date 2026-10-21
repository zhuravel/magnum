package settings

import (
	"strings"
	"testing"
	"time"
)

// A setting saved twice keeps one "was" note on its line, the newest: the
// notes of the saves piled up in front of the line's own comment.
func TestASecondSaveKeepsTheNewestWasNoteOnly(t *testing.T) {
	first, err := newSnapshot(t, fixture(t)).Plan(Draft{g("pipeline.spend"): text("burn")}, true, testNow)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newSnapshot(t, string(first.New)).Plan(Draft{g("pipeline.spend"): text("low")}, true, testNow.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	line := lineWith(string(second.New), `spend = "low"`)
	if want := `# 2026-10-21: was "burn"; 2026-10-14: medium -> high`; !strings.Contains(line, want) {
		t.Errorf("the line is %q, want %q", line, want)
	}
	if strings.Contains(line, `was "high"`) {
		t.Errorf("the line keeps the earlier save's note: %q", line)
	}
}

// lineWith is the first line of text that holds sub.
func lineWith(text, sub string) string {
	for l := range strings.Lines(text) {
		if strings.Contains(l, sub) {
			return strings.TrimRight(l, "\n")
		}
	}
	return ""
}
