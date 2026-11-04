package factory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/evalrun"
	"github.com/zhuravel/magnum/internal/pipeline"
)

// storedResult is a local round of outcome whose judge wrote the stored
// result testdata/review/<name>.json, as evalrun.RunLocal reads it.
func storedResult(t *testing.T, name, outcome string) evalrun.LocalResult {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "review", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	r := evalrun.ParseLocalResult(b)
	r.Outcome, r.ReportDir, r.Round = outcome, filepath.Join("testdata", "review"), 1
	return r
}

// A round blocks on a P0, P1 or P2, new or still open (magnum's own rule,
// roundBlocks); a round with P3s only, or nothing, blocks nothing.
func TestBlocksOnP0ToP2NewOrOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		blocks bool
	}{{"p1-new", true}, {"p2-new", true}, {"p2-open", true}, {"p3-only", false}, {"clean", false}} {
		blocks, unavailable, why := Blocks(storedResult(t, tc.name, pipeline.OutcomeDryRun))
		if blocks != tc.blocks || unavailable {
			t.Errorf("%s: blocks %v, unavailable %v (%q); want blocks %v", tc.name, blocks, unavailable, why, tc.blocks)
		}
		if blocks && why == "" {
			t.Errorf("%s blocks without saying why", tc.name)
		}
	}
}

// The judge's verdict "blocking" blocks even with no P0 to P2 among the
// findings.
func TestBlocksOnABlockingVerdict(t *testing.T) {
	blocks, unavailable, why := Blocks(storedResult(t, "blocking-verdict", pipeline.OutcomeDryRun))
	if !blocks || unavailable || !strings.Contains(why, "blocking") {
		t.Fatalf("blocks %v, unavailable %v (%q); want a block on the verdict", blocks, unavailable, why)
	}
}

// The planned event is never read: an APPROVE with a P1 blocks, and a
// result whose event is anything else decides the same.
func TestBlocksNeverReadsTheEvent(t *testing.T) {
	r := storedResult(t, "approve-with-p1", pipeline.OutcomeDryRun)
	if blocks, unavailable, _ := Blocks(r); !blocks || unavailable {
		t.Fatalf("an APPROVE with a P1: blocks %v, unavailable %v", blocks, unavailable)
	}
	clean := storedResult(t, "clean", pipeline.OutcomeDryRun)
	if blocks, _, _ := Blocks(clean); blocks {
		t.Fatal("a clean result blocks")
	}
}

// Review Focus 1: a round that ended needs_attention, blocked, error,
// timeout or refused, or whose result does not parse, is review
// unavailable, never clean, whatever its result says; why names the
// outcome.
func TestUnavailableOutcomesAreNeverClean(t *testing.T) {
	for _, outcome := range []string{pipeline.OutcomeNeedsAttention, pipeline.OutcomeBlocked, pipeline.OutcomeError, pipeline.OutcomeTimeout,
		pipeline.OutcomeRefused, pipeline.OutcomeStopped, ""} {
		blocks, unavailable, why := Blocks(storedResult(t, "clean", outcome))
		if !unavailable || blocks || !strings.Contains(why, "review unavailable") {
			t.Errorf("a clean result of a round that ended %q: blocks %v, unavailable %v (%q)", outcome, blocks, unavailable, why)
		}
		if outcome != "" && !strings.Contains(why, outcome) {
			t.Errorf("%q: why %q does not name the outcome", outcome, why)
		}
	}
	blocks, unavailable, why := Blocks(storedResult(t, "unparsable", pipeline.OutcomeDryRun))
	if !unavailable || blocks || !strings.HasPrefix(why, "review unavailable (") {
		t.Fatalf("an unparsable result: blocks %v, unavailable %v (%q)", blocks, unavailable, why)
	}
}

// A finding's fingerprint follows it across rounds: the path, its line in
// a window of 10 and a hash of the title without its priority, so a
// finding re-ranked or moved a few lines is the same, another title or
// file is not.
func TestAFindingsFingerprintIgnoresItsPriorityAndNearbyLines(t *testing.T) {
	f := evalrun.LocalFinding{Severity: "P1", Title: "Points skip the refunded activities", Path: "app/credit.rb", Line: 42}
	same := []evalrun.LocalFinding{
		{Severity: "P2", Title: "[P2] Points skip the refunded activities", Path: "app/credit.rb", Line: 47},
		{Severity: "P1", Title: "points skip the  refunded activities (missed earlier)", Path: "app/credit.rb", Line: 40},
	}
	for _, s := range same {
		if Fingerprint(s) != Fingerprint(f) {
			t.Errorf("%+v: %s, want %s", s, Fingerprint(s), Fingerprint(f))
		}
	}
	other := []evalrun.LocalFinding{
		{Severity: "P1", Title: "Points skip the refunded activities", Path: "app/credit.rb", Line: 52},
		{Severity: "P1", Title: "Points skip the cancelled activities", Path: "app/credit.rb", Line: 42},
		{Severity: "P1", Title: "Points skip the refunded activities", Path: "app/debit.rb", Line: 42},
	}
	for _, o := range other {
		if Fingerprint(o) == Fingerprint(f) {
			t.Errorf("%+v shares %s's fingerprint", o, Fingerprint(f))
		}
	}
	if fp := Fingerprint(f); !strings.HasPrefix(fp, "app/credit.rb:40:") || strings.Contains(fp, "Points") {
		t.Errorf("fingerprint %q, want app/credit.rb:40:<hash>", fp)
	}
}
