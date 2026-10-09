package tui

// What the screens' actions did. An action's result carries the daemon
// requests it queued; the screens keep the outcomes of their last actions in
// one log (ActionLog; ! shows it) and follow the requests the daemon has not
// answered yet, re-reading them on each refresh, so the answer flashes when
// it comes instead of "queued" passing for a success.

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
)

// ActionResult is what a finished action reports.
type ActionResult struct {
	// Text is everything the action printed: the footer shows its last line,
	// the action log all of it.
	Text string
	// Requests are the daemon requests it queued, as last read.
	Requests []Request
}

// Request is a daemon request as the screens follow it.
type Request struct {
	ID     int64
	Kind   string // review, pin, release, …
	State  string // RequestPending, RequestDone or RequestFailed
	Result string // the daemon's answer; "" while pending
}

// The states of a Request.
const (
	RequestPending = "pending"
	RequestDone    = "done"
	RequestFailed  = "failed"
)

func (q Request) pending() bool { return q.State == RequestPending }

// label names q in a flash or the log: "request 42 (review)".
func (q Request) label() string { return fmt.Sprintf("request %d (%s)", q.ID, q.Kind) }

// actionLogSize is how many outcomes the log keeps.
const actionLogSize = 20

// ActionLog is the outcomes of the screens' last actions, newest last, with
// the requests among them the daemon has not answered yet. The board and the
// dashboard share one (tab keeps it), so a request queued on one screen
// flashes on the other when the answer comes. Safe for concurrent use; the
// zero value is ready.
type ActionLog struct {
	mu      sync.Mutex
	entries []logEntry
	gen     int64 // counts the changes: the frame caches key on it
}

// NewActionLog returns an empty log.
func NewActionLog() *ActionLog { return &ActionLog{} }

// logEntry is one action's outcome.
type logEntry struct {
	at     time.Time
	what   string // "review talkable#7"
	target string // the PR (or slot) acted on: its row carries the pending mark
	text   string // everything the action printed
	err    string // why it failed; "" when it did not
	reqs   []Request
	// answered is when the daemon answered the last of reqs that the
	// action left pending; zero while one is pending, or none was.
	answered time.Time
}

func (e logEntry) pending() bool { return slices.ContainsFunc(e.reqs, Request.pending) }

// add records an outcome, dropping the oldest beyond actionLogSize; one
// with a request still pending goes only when every other has.
func (l *ActionLog) add(e logEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.reqs = slices.Clone(e.reqs)
	l.entries = append(l.entries, e)
	for len(l.entries) > actionLogSize {
		i := max(slices.IndexFunc(l.entries, func(e logEntry) bool { return !e.pending() }), 0)
		l.entries = slices.Delete(l.entries, i, i+1)
	}
	l.gen++
}

// pendingIDs are the requests still pending, oldest first.
func (l *ActionLog) pendingIDs() []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ids []int64
	for _, e := range l.entries {
		for _, q := range e.reqs {
			if q.pending() {
				ids = append(ids, q.ID)
			}
		}
	}
	return ids
}

// pendingTargets are the targets of the actions with a request still
// pending, sorted: the rows they name carry the pending mark (queuedFor).
func (l *ActionLog) pendingTargets() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.entries {
		if t := strings.TrimSpace(e.target); t != "" && e.pending() && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	slices.Sort(out)
	return out
}

// sameTarget reports whether an action's target names ref: the same text,
// or the same PR however a screen spells it (repo#N on the dashboard,
// owner/repo#N on the board).
func sameTarget(target, ref string) bool {
	target, ref = strings.TrimSpace(target), strings.TrimSpace(ref)
	if target == "" || ref == "" {
		return false
	}
	if strings.EqualFold(target, ref) {
		return true
	}
	a, okA := parsePickRef(target)
	b, okB := parsePickRef(ref)
	if !okA || !okB || a.repo == "" || b.repo == "" || a.number != b.number || !strings.EqualFold(a.repo, b.repo) {
		return false
	}
	return a.owner == "" || b.owner == "" || strings.EqualFold(a.owner, b.owner)
}

// queuedFor reports whether one of targets (ActionLog.pendingTargets) names
// ref.
func queuedFor(ref string, targets []string) bool {
	return slices.ContainsFunc(targets, func(t string) bool { return sameTarget(t, ref) })
}

// answer is a request the daemon answered since the screen last asked,
// with the action that queued it.
type answer struct {
	what string
	req  Request
}

// settle records the requests asked for as read (got): the ones answered
// leave the pending set and are returned, oldest first. An id asked for
// that got lacks is gone from the registry: it is settled as failed.
func (l *ActionLog) settle(asked []int64, got []Request, now time.Time) []answer {
	l.mu.Lock()
	defer l.mu.Unlock()
	byID := make(map[int64]Request, len(got))
	for _, q := range got {
		byID[q.ID] = q
	}
	var out []answer
	for i := range l.entries {
		e := &l.entries[i]
		was := e.pending()
		for j, q := range e.reqs {
			if !q.pending() || !slices.Contains(asked, q.ID) {
				continue
			}
			read, ok := byID[q.ID]
			switch {
			case !ok:
				read = Request{ID: q.ID, Kind: q.Kind, State: RequestFailed, Result: "the request is gone from the registry"}
			case read.pending():
				continue
			}
			read.Kind = cmp.Or(read.Kind, q.Kind)
			e.reqs[j] = read
			out = append(out, answer{what: e.what, req: read})
		}
		if was && !e.pending() {
			e.answered = now
		}
	}
	if len(out) > 0 {
		l.gen++
	}
	return out
}

// snapshot is a copy of the entries, newest first, and the log's generation.
func (l *ActionLog) snapshot() ([]logEntry, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]logEntry, len(l.entries))
	for i, e := range l.entries {
		e.reqs = slices.Clone(e.reqs)
		out[len(l.entries)-1-i] = e
	}
	return out, l.gen
}

// generation counts the log's changes, for the frame caches.
func (l *ActionLog) generation() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gen
}

// box is content in a bordered box centered in width x height, scrolled by
// scroll lines; below counts the lines out of view (the help, the log).
func (p *prbPainter) box(content []string, width, height, scroll int) ([]string, int) {
	room := max(height-2, 1)
	scroll = min(max(scroll, 0), max(len(content)-room, 0))
	end := min(scroll+room, len(content))
	box := p.st.Box.BorderForeground(p.pal.ruleColor).Padding(0, 2)
	out := strings.Split(lipgloss.PlaceHorizontal(width, lipgloss.Center, box.Render(strings.Join(content[scroll:end], "\n"))), "\n")
	for i := range out {
		out[i] = truncate(strings.TrimRight(out[i], " "), width)
	}
	return out, len(content) - end
}

// logContent is the board's action log at screen width w (the box takes
// six cells).
func (m prBoardModel) logContent(w int) []string {
	entries, _ := m.log.snapshot()
	return logContent(m.st, m.g, entries, max(w-6, 20))
}

// logContent is the action log's lines at width cells: one block per
// outcome, newest first: when, the action and its requests' states, then
// all it printed and the daemon's answers, wrapped.
func logContent(st styles, g glyphs, entries []logEntry, width int) []string {
	width = max(width, 20)
	lines := []string{st.Title.Render(fmt.Sprintf("Action log (the last %d, newest first)", actionLogSize))}
	if len(entries) == 0 {
		return append(lines, st.Dim.Render("no actions yet"))
	}
	wrap := func(s string, style lipgloss.Style) {
		for raw := range strings.SplitSeq(strings.TrimSpace(s), "\n") {
			l := cleanText(raw)
			if l == "" {
				continue
			}
			for w := range strings.SplitSeq(lipgloss.NewStyle().Width(max(width-2, 10)).Render(l), "\n") {
				lines = append(lines, "  "+style.Render(strings.TrimRight(w, " ")))
			}
		}
	}
	for _, e := range entries {
		head := st.Dim.Render(e.at.Local().Format("15:04:05")) + " " + st.Key.Render(cleanText(e.what))
		mark, style := g.ok, st.OK
		switch {
		case e.err != "" || slices.ContainsFunc(e.reqs, func(q Request) bool { return q.State == RequestFailed }):
			mark, style = g.fail, st.Err
		case e.pending():
			mark, style = g.queued, st.Warn
		}
		head = style.Render(mark) + " " + head
		for _, q := range e.reqs {
			state := q.State
			if q.pending() {
				state = "no answer yet"
			}
			head += st.Dim.Render(" · " + q.label() + " " + state)
		}
		if !e.answered.IsZero() {
			head += st.Dim.Render(" · answered " + e.answered.Local().Format("15:04:05"))
		}
		lines = append(lines, "", truncate(head, width))
		wrap(e.text, st.Dim)
		if e.err != "" && !strings.Contains(e.text, e.err) {
			wrap(e.err, st.Err)
		}
		for _, q := range e.reqs {
			if q.pending() || q.Result == "" {
				continue
			}
			lines = append(lines, "  "+st.Label.Render("the daemon answered "+q.label()+":"))
			s := st.Dim
			if q.State == RequestFailed {
				s = st.Err
			}
			wrap(q.Result, s)
		}
	}
	return lines
}
