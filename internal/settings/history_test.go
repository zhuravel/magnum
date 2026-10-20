package settings

import (
	"os"
	"testing"
)

// A hand edit that breaks the file leaves the screen nothing to load:
// History lists the kept versions all the same, and RestoreFile puts one
// back through the same checks and Commit, keeping the broken file.
func TestRestoreFilePutsAKeptVersionBackOverAFileThatNoLongerLoads(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	p, err := s.Plan(Draft{g("pipeline.spend"): text("low")}, true, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(p); err != nil {
		t.Fatal(err)
	}
	broken := "[pipeline\nspend = \"low\"\n"
	if err := os.WriteFile(s.Path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(s.layout, ""); err == nil {
		t.Fatal("the broken file loads")
	}
	hist, err := History(s.layout)
	if err != nil || len(hist) != 1 {
		t.Fatalf("History = %v, %v; want the one kept version", hist, err)
	}
	r, err := RestoreFile(s.layout, "", hist[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Old) != broken || string(r.New) != fixture(t) || r.Count == 0 || len(r.Hunks) == 0 {
		t.Errorf("restore plan = %+v", r)
	}
	if _, err := Commit(r); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(s.Path); string(got) != fixture(t) {
		t.Error("RestoreFile did not put the kept version back")
	}
	if hist, _ := History(s.layout); len(hist) != 2 {
		t.Errorf("the restore kept %d versions, want 2 (the broken file too)", len(hist))
	}
	if _, err := RestoreFile(s.layout, "", HistoryEntry{Path: s.Path}); err == nil {
		t.Error("a file outside the history restored")
	}
}

// A kept version that no longer loads (the loader grew stricter, or the
// base changed) is refused, and nothing is planned.
func TestRestoreFileRefusesAVersionThatDoesNotLoad(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	p, err := s.Plan(Draft{g("pipeline.spend"): text("low")}, true, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(p); err != nil {
		t.Fatal(err)
	}
	hist, err := History(s.layout)
	if err != nil || len(hist) != 1 {
		t.Fatal(hist, err)
	}
	if err := os.WriteFile(hist[0].Path, []byte("[pipeline]\nspend = \"nowhere\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreFile(s.layout, "", hist[0]); err == nil {
		t.Error("a kept version magnum does not load was planned")
	}
}
