package factory

// The waits of the factory's own work off the loop, as the screen shows
// them (ownWork): its checks (the proof and the precheck), a copy's
// preparation (a claim) and a QA server's boot. Each names its step once,
// who being the step's own word ("precheck: jest", "proof: runs … on the
// base", "copy: preparing factory3", never "precheck: precheck: jest"),
// with when the step began, for the painter's clock (tui.WaitsOn.Since),
// and the file the screen's o follows while no agent of the task works.
// The screen reads them from what the daemon writes as the step starts,
// never from its memory: the checks' step and its time from live.log's
// last begin line (the registry's wait is the tick's, up to a poll late),
// a claim's start from its begin event, a boot's from qa-run.json; the
// copy a first claim reserved before the task notes it from the agents
// store.

import (
	"cmp"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/runtime"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/tui"
)

// ownWork is the wait of task v when it waits on the factory's own work
// off the loop, as the screen shows it: its blocker and the file o follows
// ("" for none); false for any other wait.
func (s *screen) ownWork(v *taskView) (tui.WaitsOn, string, bool) {
	t := v.t
	switch {
	case t.WaitsWho == waitPrecheck:
		w, follow := s.checksWork(t)
		return w, follow, true
	case t.WaitsWho == waitCopy && strings.HasPrefix(t.WaitsWhat, preparingWhat+" "):
		w, follow := s.claimWork(t)
		return w, follow, true
	case t.WaitsWho == waitQA && strings.HasPrefix(t.WaitsWhat, qaBootWhat):
		w, follow := s.bootWork(t)
		return w, follow, true
	}
	return tui.WaitsOn{}, "", false
}

// checksWork is the wait of task t's checks: the command that runs, by
// live.log's last begin line, since it began; else (between commands: a
// lock's wait, the start) the registry's words, the step's own word first.
// o follows live.log.
func (s *screen) checksWork(t registry.Task) (tui.WaitsOn, string) {
	dir := s.l.FactoryTaskDir(t.ID)
	who, what := stepWords(t.WaitsWhat)
	var since time.Time
	if st, ok := readLiveStep(livePath(dir)); ok && st.running && !s.down {
		who, what = labelWords(st.label)
		since = st.at
	}
	follow := ""
	if liveExists(dir) {
		follow = livePath(dir)
	}
	return tui.WaitsOn{Who: who, What: what, Short: shortWhat(who, what), Since: since}, follow
}

// stepWords split a check's wait into its step's own word and the rest
// ("precheck: jest": precheck, jest; "proof: runs … on the base": proof,
// runs … on the base); any other wait of the checks stays the precheck's.
func stepWords(what string) (who, rest string) {
	for _, word := range []string{waitPrecheck, "proof"} {
		if rest, ok := strings.CutPrefix(what, word+": "); ok && rest != "" {
			return word, rest
		}
	}
	return waitPrecheck, what
}

// labelWords split a command's label (script's: "precheck jest", "proof
// runs … at the head") into its step's word and the rest.
func labelWords(label string) (who, rest string) {
	who, rest, ok := strings.Cut(label, " ")
	if !ok || rest == "" {
		return waitPrecheck, label
	}
	return who, rest
}

// claimWork is the wait of task t whose first claim of a copy runs:
// "preparing <copy>", the copy's name read from the agents store while the
// task has not noted it, since the claim's begin event, without the setup's
// minutes the daemon's words carry (the painter's clock moves on every
// frame). o follows the log of the copy's slot scripts the claim wrote last
// (its setup's, or its other scripts').
func (s *screen) claimWork(t registry.Task) (tui.WaitsOn, string) {
	name := t.Copy
	if name == "" && s.agents != nil {
		if sl, err := runtime.TaskSlot(s.ctx, s.agents, t.ID); err == nil {
			name = sl.Name
		}
	}
	what := t.WaitsWhat
	if i := strings.Index(what, " (setup "); i > 0 && strings.HasSuffix(what, ")") {
		what = what[:i]
	}
	if name != "" {
		what = preparingWhat + " " + name
	}
	var since time.Time
	if s.reg != nil && !s.down {
		if evs, err := s.reg.TaskEvents(s.ctx, t.ID, "claim", 1); err == nil && len(evs) == 1 && evs[0].Phase == registry.PhaseBegin {
			since = evs[0].At
		}
	}
	follow := ""
	if name != "" {
		follow = newestFile(filepath.Join(s.l.Logs(), slots.ProvisionLog(name)), filepath.Join(s.l.Logs(), slots.SlotLog(name)))
	}
	return tui.WaitsOn{Who: waitCopy, What: what, Short: shortWhat(waitCopy, what), Since: since}, follow
}

// bootWork is the wait of task t while its copy's server boots for QA:
// since the server's start (qa-run.json's), and o follows the server's log.
func (s *screen) bootWork(t registry.Task) (tui.WaitsOn, string) {
	w := tui.WaitsOn{Who: waitQA, What: t.WaitsWhat, Short: shortWhat(waitQA, t.WaitsWhat)}
	if !s.down {
		if run, ok := readQARun(s.l.FactoryTaskDir(t.ID)); ok {
			w.Since = run.Started
		}
	}
	follow := ""
	if p := serverLogIn(s.l.Logs(), t.Copy); t.Copy != "" && fsx.Exists(p) {
		follow = p
	}
	return w, follow
}

// newestFile is the one of paths changed last that exists, "" for none.
func newestFile(paths ...string) string {
	var best string
	var at time.Time
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && (best == "" || fi.ModTime().After(at)) {
			best, at = p, fi.ModTime()
		}
	}
	return best
}

// followWords are the words of o following file on the help line and the
// task card: how to come back to the screen.
func followWords(file string) string {
	return "o follows " + cmp.Or(filepath.Base(file), file) + " (ctrl-c, then q, comes back)"
}
