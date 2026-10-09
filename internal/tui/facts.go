package tui

// What the titles of the PR board and the status dashboard say about the
// daemon, beyond their rows: an older build running ("daemon on v1 since
// 17:40 · v2 built: daemon-restart"), a drain, a pause and what it holds,
// a watch whose polls fail ("talkable polls failing 47m (HTTP 502)"), and
// the Codex budget's pace when it reaches a cap before the window resets.
// They go on the existing title line, the rows' left alone, and give
// way to its other parts on a narrow screen: short forms first, then the
// least pressing fact.

import (
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/textx"
)

// DaemonFacts are what the titles say of the daemon; the zero value says
// nothing.
type DaemonFacts struct {
	// SkewOld is the build the daemon runs when it is older than the CLI's
	// or the one on disk ("v1.4.0", "dev (8ad5bb2)"), SkewSince when that
	// daemon started and SkewNew what is built, as a phrase ("v1.5.0 built",
	// "new build 18:48"); SkewOld is "" when the daemon runs the newest
	// build.
	SkewOld, SkewNew string
	SkewSince        time.Time
	// Paused: `magnum pause` holds automation since PausedSince (zero when
	// unknown), holding Held review requests people made.
	Paused      bool
	PausedSince time.Time
	Held        int
	// Draining: `magnum daemon-restart --drain` holds new rounds; DrainerPID
	// is the draining command (0 when unknown).
	Draining   bool
	DrainerPID int
	// PollsFailing are the watches whose radar calls have failed for
	// engine.PollFailingShown or longer, the oldest failure first: magnum
	// sees no new PRs or pushes of them.
	PollsFailing []WatchFailing
	// NeedsMe counts the open PRs magnum approved that GitHub still blocks
	// on the operator's approval (PRBoardRow.NeedsMe).
	NeedsMe int
	// AutoApproved counts the open PRs magnum approved as the operator
	// whose approval stands (PRBoardRow.AutoApproved).
	AutoApproved int
	// Codex is the Codex budget's pace when it reaches a cap before the
	// window resets; nil otherwise.
	Codex *CodexPace
	// NotesProposals counts the curation proposals for repository notes
	// that wait for the operator (`magnum notes <repo> --review`).
	NotesProposals int
}

// CodexPace is the Codex budget used now and when, at the pace it has been
// spent since its window began, it reaches Cap ([usage] codex_soft, or
// codex_hard once past it), before the window resets.
type CodexPace struct {
	Used int // percent
	Cap  float64
	At   time.Time
}

// WatchFailing is a watch whose radar calls fail since Since, the last
// with Error ("HTTP 502"; "" when unknown).
type WatchFailing struct {
	Watch string
	Since time.Time
	Error string
}

// Text is w as the screens and `magnum status` say it at now: "talkable
// polls failing 47m (HTTP 502)", without the parenthesis when the cause is
// unknown.
func (w WatchFailing) Text(now time.Time) string {
	s := cleanText(w.Watch) + " polls failing " + factAge(now.Sub(w.Since))
	if e := cleanText(w.Error); e != "" {
		s += " (" + e + ")"
	}
	return s
}

// fact is w as a title says it, short "talkable ✗ 47m".
func (w WatchFailing) fact(now time.Time) fact {
	return fact{w.Text(now), cleanText(w.Watch) + " ✗ " + factAge(now.Sub(w.Since))}
}

// cleanWatchFailing makes w's text safe to draw.
func cleanWatchFailing(w *WatchFailing) { w.Watch, w.Error = cleanText(w.Watch), cleanText(w.Error) }

// cleanFacts is f with its text safe to draw (f's slices are copied, not
// changed); the titles clean it again as they draw it.
func cleanFacts(f DaemonFacts) DaemonFacts {
	f.SkewOld, f.SkewNew = cleanText(f.SkewOld), cleanText(f.SkewNew)
	f.PollsFailing = cleanEach(f.PollsFailing, cleanWatchFailing)
	return f
}

// fact is one thing a title says, whole and short.
type fact struct{ full, short string }

// list is f's facts, most pressing first: an older build (the daemon may
// refuse what this build offers), a drain and a pause (no round starts),
// the watches whose polls fail (magnum sees none of their pushes), the PRs
// that wait for the operator's approval, those magnum approved as them, the
// Codex pace, then the notes proposals waiting for review.
func (f DaemonFacts) list(now time.Time) []fact {
	var out []fact
	if f.SkewOld != "" {
		since := ""
		if !f.SkewSince.IsZero() {
			since = " since " + factClock(now, f.SkewSince)
		}
		built := cleanText(f.SkewNew) + ": daemon-restart"
		out = append(out, fact{"daemon on " + cleanText(f.SkewOld) + since + " · " + built, built})
	}
	if f.Draining {
		full := "draining"
		if f.DrainerPID > 0 {
			full += fmt.Sprintf(" (pid %d)", f.DrainerPID)
		}
		out = append(out, fact{full, "draining"})
	}
	if f.Paused {
		s := "paused"
		if !f.PausedSince.IsZero() {
			s += " " + factAge(now.Sub(f.PausedSince))
		}
		full := s
		if f.Held > 0 {
			full += " · " + textx.Count(f.Held, "request held", "requests held")
		}
		out = append(out, fact{full, s})
	}
	for _, w := range f.PollsFailing {
		out = append(out, w.fact(now))
	}
	if n := f.NeedsMe; n > 0 {
		out = append(out, fact{textx.Count(n, "needs your ✓", "need your ✓"), fmt.Sprintf("your ✓ ×%d", n)})
	}
	if n := f.AutoApproved; n > 0 {
		out = append(out, fact{fmt.Sprintf("%d auto-approved", n), fmt.Sprintf("auto ×%d", n)})
	}
	if c := f.Codex; c != nil {
		at := c.At.Local().Format("Mon 15:04")
		out = append(out, fact{fmt.Sprintf("codex %d%% · at this pace %g%% %s", c.Used, c.Cap, at), fmt.Sprintf("codex %g%% %s", c.Cap, at)})
	}
	if n := f.NotesProposals; n > 0 {
		out = append(out, fact{textx.Count(n, "notes proposal to review", "notes proposals to review"), fmt.Sprintf("notes ×%d", n)})
	}
	return out
}

// key is f as drawn at now, for the frame caches.
func (f DaemonFacts) key(now time.Time) string {
	var b strings.Builder
	for _, x := range f.list(now) {
		b.WriteString(x.full)
		b.WriteByte('\n')
	}
	return b.String()
}

// factVariants are the ways to draw facts, from the fullest (every fact
// whole) to the barest (none): the short forms, then fewer facts, the least
// pressing left out first. Each is styled; sep goes between facts.
func (st styles) factVariants(facts []fact, sep string) []string {
	if len(facts) == 0 {
		return []string{""}
	}
	join := func(fs []fact, short bool) string {
		parts := make([]string, len(fs))
		for i, f := range fs {
			s := f.full
			if short {
				s = f.short
			}
			parts[i] = st.Warn.Render(s)
		}
		return strings.Join(parts, sep)
	}
	out := []string{join(facts, false)}
	for n := len(facts); n > 0; n-- {
		out = append(out, join(facts[:n], true))
	}
	return append(out, "")
}

// factClock is when t was, for a title: "17:40" today, else "Mon 17:40".
func factClock(now, t time.Time) string {
	now, t = now.Local(), t.Local()
	if y, m, d := now.Date(); t.Year() == y && t.Month() == m && t.Day() == d {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

// factAge is how long a pause (or a watch's failing polls) has lasted, as
// herdr's tab bar says it: "7m", "19h", "3d".
func factAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", max(int(d/time.Minute), 0))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}
