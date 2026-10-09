package agents

import (
	"bytes"
	"flag"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/learn"
)

// skillPins are the phrases the skill tests want SKILL.md to say (skillSays
// records them): the sentences the skill editor must keep
// (internal/engine/learn_edit.go reads them from skillPinsFile).
var skillPins = struct {
	sync.Mutex
	set map[string]bool
}{set: map[string]bool{}}

// recordSkillPins adds want to the pins, each trimmed (a pin is a
// substring the skill must hold, so trimming keeps it true).
func recordSkillPins(want []string) {
	skillPins.Lock()
	defer skillPins.Unlock()
	for _, w := range want {
		if p := strings.TrimSpace(w); p != "" {
			skillPins.set[p] = true
		}
	}
}

// skillPinsFile is the pins file the editor reads (learn.PinsSource).
const skillPinsFile = "testdata/skill_pins.txt"

// The pins file says what the skill tests want, with the skill's cap
// (skillMaxBytes), so the skill editor knows before it writes which
// sentences a change must keep and how large the skill may get; -update
// rewrites it. The pins come from the other tests of the package as they
// run (this file sorts after review_format_test.go and none of them runs
// in parallel), so the comparison needs the whole package: with -run, -skip
// or -shuffle it skips.
func TestSkillPinsFileIsCurrent(t *testing.T) {
	for _, name := range []string{"test.run", "test.skip"} {
		if f := flag.Lookup(name); f != nil && f.Value.String() != "" {
			t.Skipf("-%s is set: the pins come from every skill test (go test ./internal/agents [-update])", strings.TrimPrefix(name, "test."))
		}
	}
	if f := flag.Lookup("test.shuffle"); f != nil && f.Value.String() != "off" {
		t.Skip("-shuffle is set: the pins come from the skill tests that ran before this one")
	}
	skillPins.Lock()
	pins := slices.Sorted(maps.Keys(skillPins.set))
	skillPins.Unlock()
	if len(pins) == 0 {
		t.Fatal("no skill test recorded a pin (skillSays)")
	}
	for _, p := range pins {
		if strings.ContainsAny(p, "\r\n") || strings.HasPrefix(p, "#") {
			t.Errorf("pin %q holds a line break or starts with #: split it or reword the test's want", p)
		}
	}
	want := learn.FormatPins(pins, skillMaxBytes)
	if *update {
		if err := os.WriteFile(skillPinsFile, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(skillPinsFile)
	if err != nil {
		t.Fatalf("%v: run `go test ./internal/agents -update`", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is not current (%d pins, cap %d): run `go test ./internal/agents -update`", skillPinsFile, len(pins), skillMaxBytes)
	}
	read, limit, err := learn.ParsePins(got)
	if err != nil || limit != skillMaxBytes || !slices.Equal(read, pins) {
		t.Fatalf("ParsePins = %d pins, cap %d, %v; want %d, %d", len(read), limit, err, len(pins), skillMaxBytes)
	}
}
