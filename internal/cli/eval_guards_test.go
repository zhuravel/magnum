package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/eval"
)

// `eval guards` prints, one per line, the cases the skill gate would guard
// now, with the gate's own selection: those whose newest stored replay at
// the corpus's head found a defect, the least recently replayed first, of
// a watched pull request nobody flagged; --n says how many (2 by default).
// It writes and replays nothing, so an improve run can pass its output to
// eval-at.sh.
func TestEvalGuardsPrintsTheCasesTheGateWouldGuard(t *testing.T) {
	f := newInspFixture(t)
	corpus := filepath.Join(t.TempDir(), "eval.toml")
	var toml string
	for _, c := range []struct{ name, pr string }{
		{"geo", "talkable/talkable#3"}, {"copy", "talkable/talkable#4"}, {"map", "talkable/talkable#5"}, {"old", "talkable/talkable#6"},
		{"unwatched", "example/web#7"}, {"flagged", "talkable/talkable#8"}, {"never", "talkable/talkable#9"},
	} {
		toml += "[[case]]\nname = \"" + c.name + "\"\npr = \"" + c.pr + "\"\nhead = \"" + evalTestHead + "\"\n" +
			"[[case.defect]]\nid = \"d\"\ntitle = \"t\"\nmatch = [\"x\"]\n\n"
	}
	writeEvalFile(t, corpus, toml)
	root := evalRunsRoot(f.Ctx.Layout)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 9, 0, 0, 0, time.UTC) }
	replay := func(id string, at time.Time, name string, found int) {
		r := eval.Run{ID: id, Inputs: "0123456789ab", Started: at, Cases: []eval.CaseRun{{Case: name, Head: evalTestHead, Outcome: "dry_run",
			Score: &eval.Score{Found: found, Total: 1}}}}
		if err := eval.SaveRun(filepath.Join(root, id), r); err != nil {
			t.Fatal(err)
		}
	}
	replay("20261001-090000", day(1), "copy", 1)
	replay("20261001-100000", day(1), "map", 1)
	replay("20261002-090000", day(2), "old", 1)
	replay("20261003-090000", day(3), "geo", 1)
	replay("20261004-090000", day(4), "map", 0) // its newest found nothing
	replay("20261004-100000", day(4), "unwatched", 1)
	replay("20261004-110000", day(4), "flagged", 1)
	if err := eval.MarkFlagged(root, eval.Flagged{Case: "flagged", PR: "talkable/talkable#8", Run: "20261004-110000", Reason: "refused"}); err != nil {
		t.Fatal(err)
	}
	entries := func() []string {
		es, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range es {
			out = append(out, e.Name())
		}
		return out
	}
	before := entries()

	if code := execute(f.Ctx, []string{"eval", "guards", "--corpus", corpus}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	if got := f.Out.String(); got != "copy\nold\n" {
		t.Fatalf("output %q, want the two least recently replayed guards: copy, old", got)
	}
	f.Out.Reset()
	if code := execute(f.Ctx, []string{"eval", "guards", "--corpus", corpus, "--n", "5"}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, f.Err)
	}
	if got := f.Out.String(); got != "copy\nold\ngeo\n" {
		t.Fatalf("output with --n 5 %q, want every guard: copy, old, geo", got)
	}
	if after := entries(); !slices.Equal(after, before) {
		t.Fatalf("the runs root holds %q after eval guards, %q before", after, before)
	}
	if code := execute(f.Ctx, []string{"eval", "guards", "--corpus", corpus, "--n", "-1"}); code != 2 {
		t.Fatalf("--n -1: exit %d, want a usage error", code)
	}
}
