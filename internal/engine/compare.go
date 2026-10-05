package engine

// One comparison per range per tick (DECISIONS "A push that merges the base
// branch is reviewed by the PR's own diff"): a push to a reviewed PR had its
// reviewed...head range fetched by the trivial-delta check (settlePush), the
// approval check (followApproval) and the since-review size
// (refreshSinceReview) in the same poll, and a review whose head moved
// during its round compared the range again just to count the commits the
// delta check had already counted. A comparison of two commits never
// changes and a branch moves slowly, so each tick keeps the comparisons it
// made (compareMemo) and every caller asks through it: Compare, CompareFiles
// and ComparePush hit the same REST endpoint, so a push comparison of a
// range also answers the other two of the same range.

import (
	"context"
	"slices"
	"sync"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// compareKind is the kind of a GitHub compare call.
type compareKind int

const (
	kindStats compareKind = iota // GitHub.Compare
	kindFiles                    // GitHub.CompareFiles
	kindPush                     // GitHub.ComparePush
)

// compareKey names one comparison: the repository, its range and the kind
// of call.
type compareKey struct {
	repo, base, head string
	kind             compareKind
}

// compareMemo keeps the tick's successful comparisons; Tick clears it at its
// start. Rounds ask from their goroutines, hence the lock. A failed call is
// never kept: the next caller asks GitHub again.
type compareMemo struct {
	mu sync.Mutex
	m  map[compareKey]any
}

func (c *compareMemo) reset() {
	c.mu.Lock()
	c.m = nil
	c.mu.Unlock()
}

func (c *compareMemo) get(k compareKey) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	return v, ok
}

func (c *compareMemo) put(k compareKey, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[compareKey]any{}
	}
	c.m[k] = v
}

// gh is identity's GitHub client; nil when the engine has none.
func (e *Engine) gh(identity string) GitHub {
	if e.d.GitHub == nil {
		return nil
	}
	return e.d.GitHub(identity)
}

// comparePush is gh.ComparePush of base...head, once per tick.
func (e *Engine) comparePush(ctx context.Context, gh GitHub, repo store.Repo, base, head string) (github.PushComparison, error) {
	k := compareKey{repo.FullName(), base, head, kindPush}
	if v, ok := e.compares.get(k); ok {
		pc := v.(github.PushComparison)
		pc.Files = slices.Clone(pc.Files)
		return pc, nil
	}
	pc, err := gh.ComparePush(ctx, repo.Owner, repo.Name, base, head)
	if err != nil {
		return pc, err
	}
	e.compares.put(k, pc)
	pc.Files = slices.Clone(pc.Files)
	return pc, nil
}

// compareFiles is gh.CompareFiles of base...head, once per tick; a push
// comparison of the range this tick answers it.
func (e *Engine) compareFiles(ctx context.Context, gh GitHub, repo store.Repo, base, head string) ([]github.FileDelta, error) {
	k := compareKey{repo.FullName(), base, head, kindFiles}
	if v, ok := e.compares.get(compareKey{k.repo, base, head, kindPush}); ok {
		return slices.Clone(v.(github.PushComparison).Files), nil
	}
	if v, ok := e.compares.get(k); ok {
		return slices.Clone(v.([]github.FileDelta)), nil
	}
	files, err := gh.CompareFiles(ctx, repo.Owner, repo.Name, base, head)
	if err != nil {
		return nil, err
	}
	e.compares.put(k, files)
	return slices.Clone(files), nil
}

// compareStats is gh.Compare of base...head, once per tick; a push
// comparison of the range this tick answers it.
func (e *Engine) compareStats(ctx context.Context, gh GitHub, repo store.Repo, base, head string) (github.CompareStats, error) {
	k := compareKey{repo.FullName(), base, head, kindStats}
	if v, ok := e.compares.get(compareKey{k.repo, base, head, kindPush}); ok {
		return v.(github.PushComparison).Stats, nil
	}
	if v, ok := e.compares.get(k); ok {
		return v.(github.CompareStats), nil
	}
	cs, err := gh.Compare(ctx, repo.Owner, repo.Name, base, head)
	if err != nil {
		return cs, err
	}
	e.compares.put(k, cs)
	return cs, nil
}

// rangeMeasure is a range from a reviewed commit (or a role's last run) to
// a newer head, measured with the PR's own diff in view (base_merge.go):
// from...to as GitHub compares it and, when that range has a merge commit
// or diverged (the base branch merged in, a rebase, a force push), the PR's
// own diff against its base before and after it.
type rangeMeasure struct {
	push github.PushComparison
	// own compares the PR's own diff before and after the range (ownOK:
	// in full); without it from...to is the measure.
	own   ownDiff
	ownOK bool
}

// viaBase reports whether a range needs the PR's own diff to be measured:
// it has a merge commit or GitHub says it diverged.
func viaBase(pc github.PushComparison) bool { return pc.Merge || pc.Status == "diverged" }

// measureRange is the one base-aware measure the re-review gate
// (checkDelta), triage and the simplify rerun share: from...to
// (comparePush) and, when that range merged or diverged and base is known,
// the PR's own diff before (base...from) and after (base...to) it
// (ownDiffDelta). An own comparison that fails or is incomplete leaves
// from...to as the measure; a failure of from...to is the error.
func (e *Engine) measureRange(ctx context.Context, gh GitHub, repo store.Repo, base, from, to string) (rangeMeasure, error) {
	pc, err := e.comparePush(ctx, gh, repo, from, to)
	if err != nil {
		return rangeMeasure{}, err
	}
	m := rangeMeasure{push: pc}
	if viaBase(pc) && base != "" {
		m.own, m.ownOK = e.ownDiffDelta(ctx, gh, repo, base, from, to)
	}
	return m, nil
}

// size is the range's size for a threshold: the change of the PR's own
// diff when it was compared, else from...to's (MeasureDelta).
func (m rangeMeasure) size() DeltaSize {
	if m.ownOK {
		return m.own.size
	}
	return MeasureDelta(m.push.Files)
}

// files is the range as a diff to read: the files whose own change differs
// (ownDiff.files) when the PR's own diff was compared, else from...to's.
func (m rangeMeasure) files() []github.FileDelta {
	if m.ownOK {
		return m.own.files
	}
	return m.push.Files
}
