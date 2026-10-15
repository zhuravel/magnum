package engine

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/pipeline"
)

// evidenceGit names the files that differ between two heads
// (ChangedBetween) from changed, keyed "from..to"; it cannot read any
// other pair.
type evidenceGit struct {
	fakeGit
	mu      sync.Mutex
	changed map[string][]string
	calls   []string
}

func (g *evidenceGit) ChangedBetween(_ context.Context, _, from, to string) ([]string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, from+".."+to)
	if c, ok := g.changed[from+".."+to]; ok {
		return c, nil
	}
	return nil, fmt.Errorf("fake git: no diff of %s..%s", from, to)
}

// evidenceHarness is the harness whose rounds post as the pipeline does,
// each with the judge's evidence for its head from byHead (none when
// absent), over git.
func evidenceHarness(t *testing.T, git *evidenceGit, byHead map[string]*pipeline.Evidence) *harness {
	t.Helper()
	h := newHarness(t, func(h *harness) { h.d.Git = git })
	var mu sync.Mutex
	n := 0
	h.rd.mu.Lock()
	h.rd.judgeLogin = "talkable[bot]"
	h.rd.script = func(in pipeline.RoundInput) (pipeline.RoundResult, error) {
		mu.Lock()
		n++
		i := n
		mu.Unlock()
		res, err := h.rd.posted(h.ctx, in, i)
		res.Evidence = byHead[in.TargetSHA]
		return res, err
	}
	h.rd.mu.Unlock()
	return h
}

// evidenceOf reads PR n's evidence map.
func evidenceOf(t *testing.T, h *harness, n int) EvidenceMap {
	t.Helper()
	m, ok, err := ReadEvidenceMap(pipeline.EvidenceMapPath(h.layout, "talkable", "talkable", n))
	if err != nil || !ok {
		t.Fatalf("evidence map of #%d: ok %v, %v", n, ok, err)
	}
	return m
}

// entryOf is the map's entry of path and behaviour.
func entryOf(t *testing.T, m EvidenceMap, path, behaviour string) EvidenceMapEntry {
	t.Helper()
	for _, e := range m.Entries {
		if e.Path == path && e.Behaviour == behaviour {
			return e
		}
	}
	t.Fatalf("no entry %s %q in %+v", path, behaviour, m.Entries)
	return EvidenceMapEntry{}
}

func probed(path, behaviour string) pipeline.EvidenceEntry {
	return pipeline.EvidenceEntry{Path: path, Behaviour: behaviour, State: pipeline.EvidenceProbed,
		Command: "bin/rspec spec/" + strings.TrimSuffix(path, ".rb") + "_spec.rb", Control: true, Result: "passed"}
}

// What one round examined is kept across rounds in the PR's evidence map,
// next to its per-head report directories: a newer entry of a path and
// behaviour replaces the older, an unknown file a later round read is
// known, and when a round posts on a new head every entry whose file
// changed since the map's head goes stale unless that round wrote it
// again. Each entry keeps the head and the round that wrote it. When git
// cannot say what changed, nothing an older head examined stays fresh.
func TestEvidenceMergesAcrossRoundsAndGoesStaleWhenItsFileChanges(t *testing.T) {
	git := &evidenceGit{changed: map[string][]string{"b1..b2": {"app/b.rb", "app/d.rb", "app/new.rb"}}}
	h := evidenceHarness(t, git, map[string]*pipeline.Evidence{
		"b1": {HeadSHA: "b1", Entries: []pipeline.EvidenceEntry{
			probed("app/a.rb", "a kept reward keeps its variant"),
			{Path: "app/b.rb", Behaviour: "the guard refuses a closed campaign", State: pipeline.EvidenceRead},
			{Path: "app/c.rb", State: pipeline.EvidenceUnknown},
			probed("app/d.rb", "the restore writes no live action"),
		}},
		"b2": {HeadSHA: "b2", Entries: []pipeline.EvidenceEntry{
			probed("app/d.rb", "the restore writes no live action"),
			{Path: "app/c.rb", Behaviour: "an empty list is a no-op", State: pipeline.EvidenceRead},
		}},
	})
	h.reviewedPR(2, "b1")
	first := evidenceOf(t, h, 2)
	if first.HeadSHA != "b1" || len(first.Entries) != 4 || first.PR != "talkable/talkable#2" {
		t.Fatalf("map after the first review: %+v", first)
	}
	if e := entryOf(t, first, "app/a.rb", "a kept reward keeps its variant"); e.Head != "b1" || e.Round != 1 || e.Run == "" || !e.Control {
		t.Fatalf("a.rb after the first review: %+v", e)
	}

	pushAndRereview(t, h, "b2")
	m := evidenceOf(t, h, 2)
	if m.HeadSHA != "b2" || m.Round != 2 {
		t.Fatalf("map head %s round %d, want b2 and 2", m.HeadSHA, m.Round)
	}
	if e := entryOf(t, m, "app/a.rb", "a kept reward keeps its variant"); e.State != pipeline.EvidenceProbed || e.Head != "b1" || e.Round != 1 {
		t.Errorf("a.rb, unchanged since its probe: %+v", e)
	}
	if e := entryOf(t, m, "app/b.rb", "the guard refuses a closed campaign"); e.State != pipeline.EvidenceStale || e.Was != pipeline.EvidenceRead || e.Head != "b1" {
		t.Errorf("b.rb, changed since it was read: %+v", e)
	}
	if e := entryOf(t, m, "app/d.rb", "the restore writes no live action"); e.State != pipeline.EvidenceProbed || e.Head != "b2" || e.Round != 2 {
		t.Errorf("d.rb, changed and probed again: %+v", e)
	}
	if e := entryOf(t, m, "app/c.rb", "an empty list is a no-op"); e.State != pipeline.EvidenceRead || e.Round != 2 {
		t.Errorf("c.rb, read this round: %+v", e)
	}
	if slices.ContainsFunc(m.Entries, func(e EvidenceMapEntry) bool { return e.Path == "app/c.rb" && e.State == pipeline.EvidenceUnknown }) {
		t.Errorf("c.rb is still unknown after a round read it: %+v", m.Entries)
	}
	if got := m.Counts(); got != (EvidenceCounts{Probed: 2, Read: 1, Stale: 1}) {
		t.Errorf("counts %+v", got)
	}

	// No evidence from the judge, and git cannot tell what b2..b3 changed:
	// what an older head examined is stale.
	pushAndRereview(t, h, "b3")
	m = evidenceOf(t, h, 2)
	if m.HeadSHA != "b3" {
		t.Fatalf("map head %s, want b3", m.HeadSHA)
	}
	for _, e := range m.Entries {
		if e.State != pipeline.EvidenceStale {
			t.Errorf("%s %q is %s after a push git could not read", e.Path, e.Behaviour, e.State)
		}
	}
	git.mu.Lock()
	defer git.mu.Unlock()
	if !slices.Equal(git.calls, []string{"b1..b2", "b2..b3"}) {
		t.Errorf("ChangedBetween calls %q", git.calls)
	}
}
