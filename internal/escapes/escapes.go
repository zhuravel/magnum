// Package escapes matches one review round's findings against another's by
// place: a finding of the other round escaped the local one when no local
// finding sits on its path within NearLines of its lines, at a head whose
// line numbers name the same code. It is the one near rule of the review
// retro (internal/learn: a comment near one of magnum's findings) and of the
// factory's local review against the rounds after it (DECISIONS "The review
// retro tags factory-origin misses and their stage").
//
// The package is pure: no registry, no I/O, nothing of a pull request
// beyond what a Finding holds. A caller that knows that two heads name the
// same code for a path (the file did not change between them) says so with
// SameCode.
package escapes

// NearLines is how far apart, in lines, two findings on the same path may
// be and still be about the same code.
const NearLines = 3

// Source says which round, or who, a finding is of.
type Source string

// Sources.
const (
	SourceLocal   Source = "local"   // the factory's local review round (factory.db review_findings)
	SourcePosted  Source = "posted"  // magnum's posted review round (magnum.db findings)
	SourceComment Source = "comment" // a person's comment on the pull request
)

// Finding is one finding as Match places it.
type Finding struct {
	Source Source
	// Ref is the caller's own key, opaque here, to find its row again.
	Ref  string
	Path string
	// StartLine and EndLine are the lines the finding is on, [StartLine,
	// EndLine], in the file at Head: equal for a one-line finding, and a
	// finding with only a line has StartLine = EndLine = that line.
	// StartLine 0 means EndLine alone; EndLine 0 means no line (a file or
	// review-body finding), which cannot be placed.
	StartLine, EndLine int
	// Priority is P0 to P3 as the registries write it ("" when unknown).
	// Match never filters by it: callers do.
	Priority string
	// Title may be empty: the factory keeps only TitleHash.
	Title, TitleHash string
	// Round is magnum's round or the factory's review cycle that raised
	// it (0 when unknown).
	Round int
	// Head is the commit the line numbers name ("" when unknown).
	Head string
}

// Lines is f's range as [from, to] with from <= to, false when f has no
// line.
func (f Finding) Lines() (from, to int, ok bool) {
	if f.EndLine <= 0 {
		return 0, 0, false
	}
	if f.StartLine <= 0 {
		return f.EndLine, f.EndLine, true
	}
	return min(f.StartLine, f.EndLine), max(f.StartLine, f.EndLine), true
}

// placed reports whether f has a path and a line.
func (f Finding) placed() bool {
	_, _, ok := f.Lines()
	return ok && f.Path != ""
}

// Reason says why no local finding caught a finding of the other round.
type Reason string

// Reasons.
const (
	// ReasonNoLocal: no local finding on its path at a comparable head.
	ReasonNoLocal Reason = "no_local"
	// ReasonFar: the nearest local finding on its path at a comparable
	// head is more than NearLines away (Escape.Nearest, Escape.Distance).
	ReasonFar Reason = "far"
	// ReasonUnplaced: it has no path or no line, so nothing can catch it.
	ReasonUnplaced Reason = "unplaced"
)

// Escape is a finding of the other round that no local finding caught, and
// why.
type Escape struct {
	Finding Finding
	Reason  Reason
	// Nearest is the local finding on the path nearest to it at a
	// comparable head, the first in local's order on a tie, and Distance
	// how many lines lie between their ranges; both are set for ReasonFar
	// only.
	Nearest  Finding
	Distance int
}

// Option changes how Match compares heads.
type Option func(*config)

type config struct {
	sameCode func(path, localHead, otherHead string) bool
}

// SameCode lets Match compare a local finding with one of the other round
// at two different heads: same(path, localHead, otherHead) reports whether
// path's line numbers name the same code at both (the file did not change
// between them). Match asks it only when both heads are set and differ.
// Without it, two findings are compared only at the same head (or both
// unknown).
func SameCode(same func(path, localHead, otherHead string) bool) Option {
	return func(c *config) { c.sameCode = same }
}

// Distance is how many lines lie between a's and b's ranges (0 when they
// overlap or touch the same line), whatever their paths; false when either
// has no line.
func Distance(a, b Finding) (int, bool) {
	aFrom, aTo, aOK := a.Lines()
	bFrom, bTo, bOK := b.Lines()
	if !aOK || !bOK {
		return 0, false
	}
	return max(0, bFrom-aTo, aFrom-bTo), true
}

// Match returns, in other's order, an Escape for every finding of other
// that no finding of local caught. A local finding catches one of other
// when both are placed (a path and a line), their paths are equal, their
// heads are comparable (equal, both unknown, or the same code by SameCode)
// and their ranges are at most NearLines apart (Distance). A finding of
// other that cannot be placed is an escape of its own (ReasonUnplaced),
// never matched; a local one that cannot be placed catches nothing. Match
// never filters by priority, and one local finding may catch several.
func Match(local, other []Finding, opts ...Option) []Escape {
	var c config
	for _, o := range opts {
		o(&c)
	}
	var out []Escape
	for _, o := range other {
		if !o.placed() {
			out = append(out, Escape{Finding: o, Reason: ReasonUnplaced})
			continue
		}
		e, caught := Escape{Finding: o, Reason: ReasonNoLocal}, false
		for _, l := range local {
			if !l.placed() || l.Path != o.Path || !c.comparable(o.Path, l.Head, o.Head) {
				continue
			}
			d, _ := Distance(l, o)
			if d <= NearLines {
				caught = true
				break
			}
			if e.Reason == ReasonNoLocal || d < e.Distance {
				e.Reason, e.Nearest, e.Distance = ReasonFar, l, d
			}
		}
		if !caught {
			out = append(out, e)
		}
	}
	return out
}

// comparable reports whether line numbers of path at localHead and at
// otherHead name the same code: the same head (or both unknown), or two
// known heads SameCode says so of.
func (c config) comparable(path, localHead, otherHead string) bool {
	switch {
	case localHead == otherHead:
		return true
	case localHead == "" || otherHead == "" || c.sameCode == nil:
		return false
	}
	return c.sameCode(path, localHead, otherHead)
}
