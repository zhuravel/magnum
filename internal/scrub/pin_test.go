package scrub

// The scrub moved here from internal/factory/runtime (runtime.Scrub), and the move changed no behaviour. The pins
// below were recorded from runtime.Scrub at 7ae1822e, before the move: for each of the fuzz's seeds (scrubSeeds,
// in their order) the SHA-256 of its text and of the scrubbed text, and the scrubbed text itself where it is short.
// Seeds appended after the pinned ones are not pinned. A later change of what the scrub masks edits the pins it
// changes, by hand, in the commit that changes it, with its entry in docs/DECISIONS.md.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

type scrubPin struct {
	Seed      int    `json:"seed"`
	InSHA256  string `json:"in_sha256"`
	OutSHA256 string `json:"out_sha256"`
	Out       string `json:"out"`
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestTextMasksTheSeedsAsRuntimeScrubDidBeforeTheMove(t *testing.T) {
	raw, err := os.ReadFile("testdata/runtime_scrub_before_the_move.json")
	if err != nil {
		t.Fatal(err)
	}
	var pins []scrubPin
	if err := json.Unmarshal(raw, &pins); err != nil {
		t.Fatal(err)
	}
	if len(pins) == 0 || len(pins) > len(scrubSeeds) {
		t.Fatalf("%d pins for %d seeds: a seed is pinned once, and the pinned seeds stay first", len(pins), len(scrubSeeds))
	}
	for i, pin := range pins {
		if pin.Seed != i {
			t.Fatalf("pin %d is of seed %d", i, pin.Seed)
		}
		seed := scrubSeeds[i]
		in := scrubText(seed.head, seed.fill, seed.n, seed.tail)
		if got := sha256Hex(in); got != pin.InSHA256 {
			t.Fatalf("seed %d is not the text that was pinned (its SHA-256 is %s, the pin's %s): seeds are edited with their pins",
				i, got, pin.InSHA256)
		}
		out := Text(in)
		if pin.Out != "" && out != pin.Out {
			t.Errorf("seed %d:\n text: %q\n got:  %q\n want: %q", i, brief(in), brief(out), brief(pin.Out))
		} else if got := sha256Hex(out); got != pin.OutSHA256 {
			t.Errorf("seed %d: the scrubbed text's SHA-256 is %s, the pin's %s (text %q)", i, got, pin.OutSHA256, brief(in))
		}
	}
}
