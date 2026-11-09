package factory

// The repairs of the broken copies (spec 7.1) and the infrastructure hold of
// a repository whose every copy broke the same way. A copy whose setup
// failed twice is broken, and no claim takes it. Each start of the daemon
// repairs each broken copy once, in the claim that finds no copy (the
// start's repair, copies.go: a restart is when a fix of the setup's cause
// takes effect). While the daemon runs, the backoff repairs it again: 10
// minutes after it broke (or its start's repair failed), 30 minutes after
// that try failed, then every hour, so a cause that clears by itself (a
// PATH, a volume that comes back, a network blip) needs no restart. A
// backoff repair runs the start's repair's steps, and only those, off the
// loop, one at a time across the factory, never while a claim or a task
// holds the copy, the daemon drains or a hold of the machine holds launches
// (the disk hold among them), and the daemon's stop cuts it: the copy is
// then broken as it was. When every copy a repository may have is broken
// with one cause (copySignature), that cause is an infrastructure hold of
// the repository, said once; it lifts itself once a copy is repaired.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// The backoff of a broken copy's repairs while the daemon runs.
const (
	repairFirst  = 10 * time.Minute // after it broke, or after its start's repair failed
	repairSecond = 30 * time.Minute // after the first backoff repair failed
	repairEvery  = time.Hour        // after each later one
)

// repairBackoff is the wait before a broken copy's next repair once tries
// of its backoff repairs failed since it last broke: 10 minutes, 30, then
// every hour.
func repairBackoff(tries int) time.Duration {
	switch {
	case tries <= 0:
		return repairFirst
	case tries == 1:
		return repairSecond
	}
	return repairEvery
}

// copyRepairs is a broken copy's repairs under this daemon start: whether
// its start's repair is spent (a claim repaired it, or it broke under this
// daemon), how many of its backoff repairs failed since it last broke
// (tries), when the backoff repairs it next, whether a repair works on it
// now, and the cause it last broke with (copySignature): a copy being
// repaired is down with that cause, and a failure with it is said once.
type copyRepairs struct {
	started bool
	tries   int
	next    time.Time
	running bool
	cause   string
}

// copyBroke is a repair whose setup failed again: the copy is broken again,
// with cause (its signature) in the words why gives, repaired again at
// next; same says it broke the way it last broke.
type copyBroke struct {
	name, cause, why string
	next             time.Time
	same             bool
}

func (b copyBroke) Error() string { return "the repair of " + b.name + " failed: " + b.cause }

// errNotDue is a backoff repair asked for a copy whose repair is not due:
// not broken, held by a task, pinned or held, worked on by a claim or a
// repair, or its try has not come.
var errNotDue = errors.New("factory: the copy's repair is not due")

// Outage is a repository whose every copy broke with one cause (Cause, the
// copies' signature, copySignature): each copy it may have (max_copies, 2
// at least) is broken, or being repaired, with the same one. No claim gets
// a copy of it until a repair works.
type Outage struct {
	Repo, Cause string
}

// setupLogWords start the end of a broken copy's reason that names its
// setup's log, which its signature leaves out.
const setupLogWords = "its setup's log: "

// broke records that copy sl broke with cause (its signature), its start's
// repair spent: a break, and a start's repair that failed, wait repairFirst
// for the next try; a backoff repair that failed counts a try. The caller
// holds no lock.
func (c *copySet) broke(sl store.Slot, cause string, backoff bool) copyBroke {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.repairs[sl.Name]
	if r == nil {
		r = &copyRepairs{}
		c.repairs[sl.Name] = r
	}
	r.started = true
	if backoff {
		r.tries++
	} else {
		r.tries = 0
	}
	r.next = c.now().Add(repairBackoff(r.tries))
	b := copyBroke{name: sl.Name, cause: cause, next: r.next, same: cause != "" && cause == r.cause}
	r.cause = cause
	return b
}

// startRepair notes that a repair of broken copy sl of repo starts: the
// start's is spent, no claim or other repair takes the copy (adding), and
// the copy is down with the cause it broke with while the repair runs
// (Outages). The caller holds mu.
func (c *copySet) startRepair(repo config.FactoryRepo, sl store.Slot) {
	r := c.repairs[sl.Name]
	if r == nil {
		r = &copyRepairs{}
		c.repairs[sl.Name] = r
	}
	r.started, r.running = true, true
	if cause := copySignature(repo, sl, store.Deref(sl.LastError)); cause != "" {
		r.cause = cause
	}
	c.adding[sl.Name] = true
}

// endRepair notes that the repair of copy name ended.
func (c *copySet) endRepair(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.adding, name)
	if r := c.repairs[name]; r != nil {
		r.running = false
	}
}

// repaired forgets copy name's repairs: it is back in the pool, and a break
// after this one starts afresh.
func (c *copySet) repaired(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.repairs, name)
}

// repairBeginWords are the words of the begin of copy name's repair.
func (c *copySet) repairBeginWords(name string, backoff bool) string {
	if !backoff {
		return "repairs the broken copy: each start of the factory daemon repairs it once, when a claim finds no copy"
	}
	c.mu.Lock()
	tries := 0
	if r := c.repairs[name]; r != nil {
		tries = r.tries
	}
	c.mu.Unlock()
	return fmt.Sprintf("repairs the broken copy again: try %d of its backoff (10 minutes, 30, then every hour)", tries+1)
}

// repairFailed ends the begin of copy name's repair whose setup failed
// again (b): a warning for a new cause, and the same at INFO for the cause
// it last broke with, which the copy said already.
func (c *copySet) repairFailed(ctx context.Context, name string, b copyBroke, text string) {
	level := "warn"
	if b.same {
		level, text = "info", "failed the same way again: "+text
	}
	c.phaseAt(ctx, name, "factory.copy_repair", store.PhaseFail, level, "its repair failed: "+text)
}

// unrepaired is the end of a backoff repair of copy sl that could not run
// to its end (err): the daemon's stop cut it, a fetch could not reach
// origin, the disk was short. Nothing of it stays: the copy is broken again
// with the reason it broke with, and its repair ends with a fail event. A
// stop counts no try (the next start repairs the copy); any other failure
// counts one, and the backoff tries it again.
func (c *copySet) unrepaired(ctx context.Context, sl store.Slot, err error) error {
	// A repair that stopped before the slot code revived the copy left it
	// broken: the compare-and-set finds nothing to put back.
	var put error
	if terr := c.st.TransitionSlot(context.WithoutCancel(ctx), sl.ID, []string{store.SlotProvisioning}, store.SlotBroken,
		func(u *store.SlotUpdate) { u.Set("last_error", store.Deref(sl.LastError)) }); terr != nil && !errors.Is(terr, store.ErrConflict) {
		put = fmt.Errorf("put %s back as broken: %w", sl.Name, terr)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		c.phase(ctx, sl.Name, "factory.copy_repair", store.PhaseFail,
			"the daemon stopped during its repair: it stays broken as it was, and the next start repairs it")
		return errors.Join(fmt.Errorf("the repair of %s: %w", sl.Name, ctx.Err()), put)
	}
	err = errors.Join(err, put)
	c.mu.Lock()
	next := c.now()
	if r := c.repairs[sl.Name]; r != nil {
		r.tries++
		r.next = next.Add(repairBackoff(r.tries))
		next = r.next
	}
	c.mu.Unlock()
	c.phase(ctx, sl.Name, "factory.copy_repair", store.PhaseFail, "its repair did not run to its end: "+
		textx.FirstLine(err.Error())+"; it stays broken as it was, "+repairAtWords(next, c.now()))
	return err
}

// repairAtWords say when a broken copy's next repair comes: "the next
// repair at 15:40", or "its repair is due" once it has come.
func repairAtWords(next, now time.Time) string {
	if next.After(now) {
		return "the next repair at " + clock(next)
	}
	return "its repair is due"
}

// nextRepairWords say, in a wait for a copy, when the first of the broken
// copies in list comes back, and that a restart repairs them now. The
// caller holds mu.
func (c *copySet) nextRepairWords(list []store.Slot, broken int) string {
	var next time.Time
	for _, sl := range list {
		if r := c.repairs[sl.Name]; sl.State == store.SlotBroken && r != nil && !r.next.IsZero() && (next.IsZero() || r.next.Before(next)) {
			next = r.next
		}
	}
	if next.IsZero() {
		them := "them"
		if broken == 1 {
			them = "it"
		}
		return "a restart repairs " + them + ": magnum factory install"
	}
	return repairAtWords(next, c.now()) + ", or a restart: magnum factory install"
}

// dueLocked reports whether broken copy sl's backoff repair is due at now:
// its try has come, no claim or repair works on it (adding, running), and
// no task holds it (its row names no owner row: a pinned break keeps its
// PR) nor a pin or a hold. The caller holds mu.
func (c *copySet) dueLocked(sl store.Slot, r *copyRepairs, now time.Time) bool {
	return sl.State == store.SlotBroken && r != nil && !r.running && !c.adding[sl.Name] && sl.PRID == nil && !sl.Pinned &&
		sl.HoldReason == nil && !r.next.IsZero() && !now.Before(r.next)
}

// RepairDue is the broken copy whose backoff repair is due now, the one
// due first ("" for none). A copy that broke before this start and that no
// claim repaired yet is scheduled when RepairDue first sees it: 10 minutes
// on, so its start's repair still comes first when a claim needs it. The
// repairs of copies no longer broken are forgotten.
func (c *copySet) RepairDue(ctx context.Context) (string, error) {
	now := c.now()
	due, at := "", time.Time{}
	for _, repo := range c.cfg.Factory.Repos {
		list, err := c.st.ListSlots(ctx, store.SlotFilter{RepoFullName: repo.Repo, Kind: store.SlotKindPool})
		if err != nil {
			return "", fmt.Errorf("the broken copies of %s: %w", repo.Repo, err)
		}
		c.mu.Lock()
		for _, sl := range list {
			if _, ok := copyNumber(repo, sl.Name); !ok {
				continue
			}
			r := c.repairs[sl.Name]
			if sl.State != store.SlotBroken {
				if r != nil && !r.running && sl.State != store.SlotProvisioning {
					delete(c.repairs, sl.Name)
				}
				continue
			}
			if r == nil {
				r = &copyRepairs{cause: copySignature(repo, sl, store.Deref(sl.LastError))}
				c.repairs[sl.Name] = r
			}
			if r.next.IsZero() && !r.running { // first seen, or a claim's repair that stopped before it began
				r.next = now.Add(repairFirst)
			}
			if c.dueLocked(sl, r, now) && (due == "" || r.next.Before(at)) {
				due, at = sl.Name, r.next
			}
		}
		c.mu.Unlock()
	}
	return due, nil
}

// Repair runs the backoff repair of broken copy name (RepairDue): the
// start's repair's steps (repair, slots.Repair), refused (errNotDue) unless
// it is still due, read again under mu. It returns nil once the copy is
// back in the pool, a copyBroke when its setup failed again, and the error
// of a repair that could not run to its end, after which the copy is
// broken as it was (unrepaired).
func (c *copySet) Repair(ctx context.Context, name string) error {
	repo, _, ok := c.repoOf(name)
	if !ok {
		return fmt.Errorf("repair %s: no [[factory.repo]] has that copy", name)
	}
	c.mu.Lock()
	sl, err := c.st.SlotByName(ctx, name)
	if err != nil || !c.dueLocked(sl, c.repairs[name], c.now()) {
		c.mu.Unlock()
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("repair %s: %w", name, err)
		}
		return errNotDue
	}
	c.startRepair(repo, sl)
	c.mu.Unlock()
	defer c.endRepair(name)
	return c.repair(ctx, repo, sl, true)
}

// Outages are the repositories whose every copy broke with one cause
// (Outage), in the order of their [[factory.repo]].
func (c *copySet) Outages(ctx context.Context) ([]Outage, error) {
	var out []Outage
	for _, repo := range c.cfg.Factory.Repos {
		list, err := c.st.ListSlots(ctx, store.SlotFilter{RepoFullName: repo.Repo, Kind: store.SlotKindPool})
		if err != nil {
			return nil, fmt.Errorf("the copies of %s: %w", repo.Repo, err)
		}
		if cause := c.outage(repo, list); cause != "" {
			out = append(out, Outage{Repo: repo.Repo, Cause: cause})
		}
	}
	return out, nil
}

// outage is the cause every copy of repo in list broke with ("" for none):
// each copy the repository may have (max_copies, 2 at least: one copy says
// nothing of a shared cause) is broken, or provisioning under a repair,
// which keeps the cause it broke with, and all read the same.
func (c *copySet) outage(repo config.FactoryRepo, list []store.Slot) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	cause, n := "", 0
	for _, sl := range list {
		if _, ok := copyNumber(repo, sl.Name); !ok || sl.State == store.SlotRemoved {
			continue
		}
		sig := ""
		switch r := c.repairs[sl.Name]; {
		case sl.State == store.SlotBroken:
			sig = copySignature(repo, sl, store.Deref(sl.LastError))
		case sl.State == store.SlotProvisioning && r != nil && r.running:
			sig = r.cause
		}
		if sig == "" || (cause != "" && sig != cause) {
			return ""
		}
		cause, n = sig, n+1
	}
	if n < max(2, repo.MaxCopies) {
		return ""
	}
	return cause
}

// The words of a cause that change from copy to copy or from run to run,
// which a signature leaves out: the times, dates and durations, and the
// paths of temporary files (the system's, signatureTemps, and those under
// the daemon's TMPDIR, tempDirRe).
var (
	signatureTimes = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}(?:[T ]\d{2}:\d{2}(?::\d{2})?(?:[.,]\d+)?(?:Z|[+-]\d{2}:?\d{2})?)?\b|` +
		`\b\d{1,2}:\d{2}(?::\d{2})?(?:[.,]\d+)?\b|\b(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+\b`)
	signatureTemps = regexp.MustCompile(`(?:/private)?/(?:tmp|var/folders)/[^\s'"():;,]*`)
	tempDirRe      = sync.OnceValue(func() *regexp.Regexp {
		tmp := filepath.Clean(os.TempDir())
		if tmp == "/" || tmp == "." {
			return nil
		}
		return regexp.MustCompile(regexp.QuoteMeta(tmp) + `/[^\s'"():;,]*`)
	})
)

// copySignature is the cause a broken copy's reason (text: its last
// error, as markBroken or the slot code wrote it) words: brokenWhy's line,
// past what failed (whatSetupTwice, whatRepairFailed) and without its
// setup's log, with the copy's own words (its path, its name wherever it
// stands, as in its databases, its database slug, and the env values only
// it has, its port among them) and what changes from run to run (times,
// durations, temporary files) put as placeholders. Two copies broken by one
// cause read the same, and two causes do not; "" for no reason.
func copySignature(repo config.FactoryRepo, sl store.Slot, text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if i := strings.LastIndex(s, " ("+setupLogWords); i >= 0 {
		s = s[:i]
	}
	for _, what := range []string{whatSetupTwice, whatRepairFailed} {
		s = strings.TrimPrefix(s, what+": ")
	}
	if s == "" {
		return ""
	}
	own := []string{sl.Path, fsx.Canon(sl.Path), store.Deref(sl.DBSlug), slots.DBSlug(sl.Name), sl.Name}
	slices.SortStableFunc(own, func(a, b string) int { return cmp.Compare(len(b), len(a)) }) // a path holds the name
	for _, w := range own {
		if len(w) > 1 {
			s = strings.ReplaceAll(s, w, "<copy>")
		}
	}
	if n, ok := copyNumber(repo, sl.Name); ok {
		env := copyEnv(repo, n)
		for k, tmpl := range repo.Env {
			if v := env[k]; (strings.Contains(tmpl, "{") || k == RedisOffsetEnv) && len(v) >= 3 {
				s = replaceWord(s, v, "<copy>")
			}
		}
	}
	if re := tempDirRe(); re != nil {
		s = re.ReplaceAllString(s, "<tmp>")
	}
	s = signatureTemps.ReplaceAllString(s, "<tmp>")
	return signatureTimes.ReplaceAllString(s, "<time>")
}

// replaceWord replaces w in s with by where it stands as a word of its own:
// no ASCII letter, digit or underscore next to it ("3010" in "port 3010",
// not in "13010").
func replaceWord(s, w, by string) string {
	word := func(i int) bool {
		c := s[i]
		return c == '_' || '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
	}
	var b strings.Builder
	last := 0
	for i := 0; i <= len(s)-len(w); {
		j := strings.Index(s[i:], w)
		if j < 0 {
			break
		}
		j += i
		end := j + len(w)
		if (j == 0 || !word(j-1)) && (end == len(s) || !word(end)) {
			b.WriteString(s[last:j])
			b.WriteString(by)
			last, i = end, end
			continue
		}
		i = j + 1
	}
	b.WriteString(s[last:])
	return b.String()
}

// copyRepair is a backoff repair that runs off the loop (repairCopies): its
// copy, when it started, done closed when it ended, and its result
// (written before done closes).
type copyRepair struct {
	name    string
	started time.Time
	done    chan struct{}
	err     error
}

// ended reports whether the repair's goroutine has ended.
func (r *copyRepair) ended() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// repairCopies takes the result of the backoff repair that ended
// (repairEnded) and starts the one Copies.RepairDue names off the loop, on
// a context of its own under the engine's, which the daemon's stop cancels:
// one at a time, never while one runs, and never while the daemon drains or
// a hold of the machine holds launches (mayLaunch: the disk hold among
// them), the guards of a claim, which runs the start's repair. It is named
// in the record of the work a restart would cut.
func (e *Engine) repairCopies(ctx context.Context) {
	if e.d.Copies == nil {
		return
	}
	if r := e.repairing; r != nil {
		if !r.ended() {
			return
		}
		e.repairing = nil
		e.repairEnded(r)
	}
	if _, _, ok := e.mayLaunch(); !ok {
		return
	}
	name, err := e.d.Copies.RepairDue(ctx)
	if err != nil {
		e.log.Warn("read the broken copies for their repairs", "err", err)
		return
	}
	if name == "" {
		return
	}
	rctx, cancel := context.WithCancel(e.bgCtx)
	run := &copyRepair{name: name, started: e.now(), done: make(chan struct{})}
	e.repairing = run
	endWork := e.beginWork(WorkRepair, 0, "repair of the broken copy "+name)
	e.bg.Go(func() {
		defer endWork()
		defer close(run.done)
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				e.log.Error("panic in a copy's repair; the daemon goes on", "copy", name, "panic", fmt.Sprint(r),
					"stack", execx.Redact(string(debug.Stack())))
				run.err = fmt.Errorf("the repair panicked: %v", r)
			}
		}()
		run.err = e.d.Copies.Repair(rctx, name)
	})
}

// repairEnded logs the end of a backoff repair: one that worked at INFO;
// one that failed at WARN once per cause of its copy (repairCauses), the
// copy's events having each try; one a claim took first not at all.
func (e *Engine) repairEnded(r *copyRepair) {
	b, broke := errors.AsType[copyBroke](r.err)
	switch {
	case r.err == nil:
		e.log.Info("a broken copy is repaired: it is back in the pool", "copy", r.name,
			"after", e.now().Sub(r.started).Round(time.Second))
		delete(e.repairCauses, r.name)
		return
	case errors.Is(r.err, errNotDue):
		return
	}
	cause, why := textx.FirstLine(r.err.Error()), r.err.Error()
	if broke {
		cause, why = b.cause, cmp.Or(b.why, b.cause)
	}
	if last, seen := e.repairCauses[r.name]; seen && sameWords(last, cause) {
		return
	}
	e.repairCauses[r.name] = cause
	attrs := []any{"copy", r.name, "why", why}
	if broke {
		attrs = append(attrs, "next", b.next)
	}
	e.log.Warn("a broken copy's repair failed; the factory repairs it again after 10 minutes, 30, then every hour", attrs...)
}

// repoHoldKey is the key of repository repo's infrastructure hold in
// Engine.holds (copyHolds).
func repoHoldKey(repo string) string { return HoldInfra + ":" + repo }

// copyHolds keeps an infrastructure hold of each repository whose every
// copy broke with one cause (Copies.Outages): raised once, with the cause
// in its reason, and lifted once the repository has a copy again (or its
// copies no longer share the cause). It is the machine's hold that says the
// cause once, for the screen (holds.json), the events and, past 30 minutes,
// a NEEDS YOU blocker with one alert (escalate). It holds no stage and no
// launch (mayLaunch leaves it out): the repository's tasks wait for a copy
// anyway, at no cost, and the repairs that lift it go on. A read of the
// copies that fails keeps the holds as they are.
func (e *Engine) copyHolds(ctx context.Context, now time.Time) {
	if e.d.Copies == nil {
		return
	}
	outs, err := e.d.Copies.Outages(ctx)
	if err != nil {
		e.log.Warn("read the copies for the holds of their repositories", "err", err)
		return
	}
	in := map[string]bool{}
	for _, o := range outs {
		key := repoHoldKey(o.Repo)
		in[key] = true
		reason := "every copy of " + o.Repo + " broke the same way: " + o.Cause
		if h := e.holds[key]; h != nil {
			h.Reason = reason
			continue
		}
		e.holds[key] = &Hold{Kind: HoldInfra, Repo: o.Repo, Reason: reason, Since: now}
		e.log.Warn("infrastructure hold of a repository: every copy broke the same way; its tasks wait for a copy, at no cost",
			"repo", o.Repo, "cause", o.Cause)
		e.event(ctx, 0, "hold", registry.PhaseBegin, "infra: "+reason+"; its tasks wait for a copy until a repair works")
	}
	for key, h := range e.holds {
		if h.Repo != "" && !in[key] {
			e.lift(ctx, key, now)
		}
	}
}

// repoHolds are the keys of the repositories' holds in force, sorted.
func (e *Engine) repoHolds() []string {
	var keys []string
	for key, h := range e.holds {
		if h.Repo != "" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}
