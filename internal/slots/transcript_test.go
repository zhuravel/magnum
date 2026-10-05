package slots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A slot, provision or per-PR log never grows past SlotLogMax: the write
// that would push it past moves it to <name>.1 (replacing the one before),
// as daemon.log rotates, so a seed that prints its SQL on every reset no
// longer grows the file without end.
func TestTranscriptRotatesALogPastItsCap(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.layout.Logs(), "slot-review1.log")
	chunk := strings.Repeat("x", 1<<20-2) + "\n" // two fit under the cap, with one byte to spare
	h.m.transcript("slot-review1.log", chunk)
	h.m.transcript("slot-review1.log", chunk)
	if fi, err := os.Stat(path); err != nil || fi.Size() != int64(2*len(chunk)) {
		t.Fatalf("below the cap: %v, %v", fi, err)
	}
	h.m.transcript("slot-review1.log", "== the next reset\n")
	if b, err := os.ReadFile(path); err != nil || string(b) != "== the next reset\n" {
		t.Fatalf("log after the cap = %d bytes, %v; want only the new text", len(b), err)
	}
	if fi, err := os.Stat(path + ".1"); err != nil || fi.Size() != int64(2*len(chunk)) || fi.Size() > SlotLogMax {
		t.Fatalf("rotated log = %v, %v", fi, err)
	}
	// The next rotation replaces .1: one old log is kept. A single write
	// larger than the cap still lands whole in a fresh file.
	h.m.transcript("slot-review1.log", strings.Repeat("y", SlotLogMax))
	if b, err := os.ReadFile(path + ".1"); err != nil || string(b) != "== the next reset\n" {
		t.Fatalf(".1 after a second rotation = %d bytes, %v; want the log before it", len(b), err)
	}
	h.m.transcript("slot-review1.log", "z\n")
	if b, err := os.ReadFile(path); err != nil || string(b) != "z\n" {
		t.Fatalf("log after a third rotation = %d bytes, %v", len(b), err)
	}
	if matches, _ := filepath.Glob(path + ".*"); len(matches) != 1 {
		t.Fatalf("rotated logs = %v, want one", matches)
	}
}
