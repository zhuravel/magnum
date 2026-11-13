package factory

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory/registry"
)

// mysqlPing is a MySQL ping a test turns up and down: it records each call
// and the time its context allowed it (zero: none, the tick's own).
type mysqlPing struct {
	mu   sync.Mutex
	down bool
	// flapping answers the calls bounded by a deadline (the hold's probe)
	// and fails the others (the try's own ping, on the tick's context): a
	// MySQL that answers the probe and is gone again by the try.
	flapping  bool
	calls     int
	bounded   int           // the calls whose context had a deadline
	longest   time.Duration // the longest of those deadlines, from the call
	unbounded int
}

func (p *mysqlPing) set(down bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.down = down
}

func (p *mysqlPing) flap(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flapping = on
}

// count is how many pings ran, and how many of them were a try's own
// (unbounded).
func (p *mysqlPing) count() (calls, unbounded int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls, p.unbounded
}

func (p *mysqlPing) ping(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	dl, bounded := ctx.Deadline()
	if bounded {
		p.bounded++
		p.longest = max(p.longest, time.Until(dl))
	} else {
		p.unbounded++
	}
	if p.down && !(p.flapping && bounded) {
		return errors.New("mysqlx: ping: dial tcp 127.0.0.1:3306: connect: connection refused")
	}
	return nil
}

// mysqlHarness is a harness with one active task at a stage that always
// passes (it never ends the stage), counting its runs, and a MySQL ping a
// test controls, down to begin with.
func mysqlHarness(t *testing.T) (*harness, *mysqlPing, *calls) {
	t.Helper()
	p := &mysqlPing{down: true}
	h := newHarness(t, func(d *Deps) { d.MySQL = p.ping })
	h.task("PR-12340", registry.TaskActive, "investigate")
	var c calls
	h.stage(config.FactoryStageAgents, func(_ context.Context, t registry.Task, _ config.FactoryStage) (bool, error) {
		c.add(t.ID, h.clock.Now())
		return false, nil
	})
	return h, p, &c
}

// holdEvents are the daemon's hold events as text.
func holdEvents(h *harness) []string {
	var out []string
	for _, ev := range h.events(0, "hold") {
		out = append(out, ev.Phase+": "+ev.Text)
	}
	return out
}

// The infrastructure hold of a MySQL that does not answer lifted only at its
// next scheduled try, up to 30 minutes after the operator started MySQL (the
// 2026-11-13 outage: MySQL back at about 17:30, the hold lifted at 17:34:49).
// The question "does it answer again?" is cheap and the try's work is not, so
// each tick between the tries asks it alone, with a short bound; once MySQL
// answers, the hold runs its full try at once. The stage ran at the first
// tick after the answer, not at the hold's Next, the hold lifted, and the
// probe said so in one line.
func TestAnInfraHoldLiftsAtTheFirstTickAfterMySQLAnswersAgain(t *testing.T) {
	h, my, c := mysqlHarness(t)
	at := time.Duration(0) // the daemon's ticks come every poll, 30 seconds, or a gap would pass for a sleep
	tickTo := func(to time.Duration) {
		for ; at <= to; at += 30 * time.Second {
			h.clock.Set(t0.Add(at))
			h.tick()
		}
	}
	tickTo(6 * time.Minute) // the hold's tries, at 0, 2 and 6 minutes
	hd, held := h.e.holds[HoldInfra]
	if !held || hd.Reason != "MySQL does not answer" || hd.Tries != 2 || !hd.Next.Equal(t0.Add(14*time.Minute)) {
		t.Fatalf("hold %+v (%v), want MySQL's, two tries failed, the next at 14m", hd, held)
	}
	if n := len(c.list()); n != 0 {
		t.Fatalf("the stage ran %d times under the hold", n)
	}

	tickTo(6*time.Minute + 30*time.Second)
	my.set(false) // the operator started MySQL at 7m
	tickTo(7 * time.Minute)
	if _, held := h.e.holds[HoldInfra]; held {
		t.Fatalf("the hold outlived the first tick after MySQL answered: %+v", h.e.holds[HoldInfra])
	}
	if ran := c.at; len(ran) != 1 || !ran[0].Equal(t0.Add(7*time.Minute)) {
		t.Fatalf("the stage ran at %v, want once, at 7m (not at the hold's next try, 14m)", ran)
	}
	var probes []string
	for _, line := range holdEvents(h) {
		if strings.Contains(line, "answers again") {
			probes = append(probes, line)
		}
	}
	if len(probes) != 1 {
		t.Fatalf("hold events %q, want one line that says MySQL answers again", holdEvents(h))
	}
	if evs := holdEvents(h); !strings.HasPrefix(evs[len(evs)-1], "ok: infra: lifted after") {
		t.Fatalf("hold events %q, want the lift last", evs)
	}

	// The next outage is its own: its probe says so again.
	tickTo(20 * time.Minute)
	my.set(true)
	tickTo(20*time.Minute + 30*time.Second) // the hold begins, its first try due at 22m30s
	my.set(false)
	tickTo(21 * time.Minute)
	n := 0
	for _, line := range holdEvents(h) {
		if strings.Contains(line, "answers again") {
			n++
		}
	}
	if _, held := h.e.holds[HoldInfra]; held || n != 2 {
		t.Fatalf("second outage: held %v, %d probe lines in %q, want it lifted and one line each", held, n, holdEvents(h))
	}
}

// A probe that finds MySQL still down changes nothing: the hold's tries,
// its backoff (2, 4, 8, 16, then 30 minutes) and its time to NEEDS YOU
// (30 minutes) go on as they did, each tick between the tries asks MySQL
// once, bounded to 2 seconds, and a tick at a try asks it once, as the try's
// own ping; the probes write no hold event.
func TestAProbeThatFailsKeepsTheBackoffAndItsTries(t *testing.T) {
	h, my, c := mysqlHarness(t)
	ticks, tries := 0, 0
	for at := time.Duration(0); at <= 31*time.Minute; at += 30 * time.Second {
		h.clock.Set(t0.Add(at))
		before, held := h.e.holds[HoldInfra]
		var was Hold
		if held {
			was = *before
		}
		h.tick()
		ticks++
		hd, nowHeld := h.e.holds[HoldInfra]
		if !nowHeld {
			t.Fatalf("at %v the hold lifted with MySQL down", at)
		}
		if !held || !t0.Add(at).Before(was.Next) {
			tries++ // the hold's start, or a scheduled try
			continue
		}
		if *hd != was {
			t.Fatalf("at %v a failed probe changed the hold: %+v, was %+v", at, *hd, was)
		}
	}
	hd := h.e.holds[HoldInfra]
	if hd.Tries != 4 || !hd.Next.Equal(t0.Add(60*time.Minute)) {
		t.Fatalf("hold %+v, want four tries failed (2, 6, 14, 30 minutes) and the next at 60m", *hd)
	}
	if n := len(c.list()); n != 0 {
		t.Fatalf("the stage ran %d times while MySQL was down", n)
	}
	if my.calls != ticks || my.bounded != ticks-tries || my.longest > 2*time.Second {
		t.Fatalf("%d pings in %d ticks (%d tries), %d of them bounded, the longest to %v: want one a tick, the probes' bounded to 2s",
			my.calls, ticks, tries, my.bounded, my.longest)
	}
	if got := h.e.Blocking(); len(got) != 1 || !strings.HasPrefix(got[0], "infra: MySQL does not answer, held since ") {
		t.Fatalf("blockers %q, want the hold at NEEDS YOU after 30 minutes", got)
	}
	evs := holdEvents(h) // the begin and the four failed tries; no probe wrote one
	if len(evs) != 5 {
		t.Fatalf("hold events %q, want the begin and four failed tries", evs)
	}
	for _, line := range evs {
		if strings.Contains(line, "answers again") {
			t.Fatalf("a probe that failed wrote %q", line)
		}
	}
}

// A cause with no cheap probe keeps today's schedule: an SSH key refused
// holds every stage until its tries (2, 6, 14 minutes), and no tick between
// them asks MySQL or anything else, though MySQL answers; and the engine
// names a probe for MySQL's cause alone.
func TestAHoldWithNoCheapProbeKeepsItsSchedule(t *testing.T) {
	h, my, _ := mysqlHarness(t)
	my.set(false)
	var c calls
	h.stage(config.FactoryStageAgents, failingStage(&c, h, sshRefused, nil))
	for m := range 9 {
		h.clock.Set(t0.Add(time.Duration(m) * time.Minute))
		h.tick()
	}
	var minutes []int
	for _, at := range c.at {
		minutes = append(minutes, int(at.Sub(t0)/time.Minute))
	}
	if len(minutes) != 3 || minutes[0] != 0 || minutes[1] != 2 || minutes[2] != 6 {
		t.Fatalf("the stage ran at minutes %v, want 0, 2 and 6", minutes)
	}
	if my.calls != 3 {
		t.Fatalf("MySQL was pinged %d times, want 3 (the first pass and the two tries, none between them)", my.calls)
	}
	if hd, held := h.e.holds[HoldInfra]; !held || hd.Reason != "SSH key refused" || hd.Tries != 2 {
		t.Fatalf("hold %+v (%v), want the SSH hold, two tries failed", hd, held)
	}
	if evs := holdEvents(h); strings.Contains(strings.Join(evs, "\n"), "answers again") {
		t.Fatalf("hold events %q name a probe", evs)
	}

	crash := envCrash{key: "lint", cmd: "rubocop", line: "Bundler::GemNotFound", sig: envSignatures[1], copy: "/copies/1"}
	for _, reason := range []string{"SSH key refused", "SSH connection failed", "SSH host key refused", "DNS lookup failed", "TLS failure",
		"the same failure on 2 tasks: bin/setup: exit 1", crash.holdCause()} {
		if probe := h.e.holdProbe(Hold{Kind: HoldInfra, Reason: reason}); probe != nil {
			t.Errorf("the cause %q has a probe", reason)
		}
	}
	if probe := h.e.holdProbe(Hold{Kind: HoldInfra, Reason: causeMySQL}); probe == nil {
		t.Error("MySQL's cause has no probe")
	}
	if probe := h.e.holdProbe(Hold{Kind: HoldHerdr, Reason: causeMySQL}); probe != nil {
		t.Error("a herdr hold has a MySQL probe")
	}
	h.e.d.MySQL = nil // a build with no MySQL ping has nothing to ask
	if probe := h.e.holdProbe(Hold{Kind: HoldInfra, Reason: causeMySQL}); probe != nil {
		t.Error("a probe without a MySQL ping")
	}
}

// A MySQL that flaps answered the hold's probe and failed the try's own ping
// right after it: the try failed and counted, as a scheduled one does, and
// the next tick asked the probe again, which answered, so every tick ran a
// full try. After a try that a probe made due fails, the hold asks nothing
// before the Next that failure scheduled: no probe, no try, no change to the
// hold. At that Next the scheduled try runs as before, and after it the
// probe asks again: a MySQL that now answers lifts the hold at the next tick.
func TestAFailedTryAfterAProbeWaitsForTheScheduleBeforeTheNextProbe(t *testing.T) {
	h, my, c := mysqlHarness(t)
	at := time.Duration(0) // the daemon's ticks come every poll, 30 seconds, or a gap would pass for a sleep
	tickTo := func(to time.Duration) {
		for ; at <= to; at += 30 * time.Second {
			h.clock.Set(t0.Add(at))
			h.tick()
		}
	}
	tickTo(6 * time.Minute) // MySQL down: the hold's start and its tries at 2 and 6 minutes, the next due at 14m
	my.flap(true)           // the probe's ping answers from now on, the try's own fails
	tickTo(6*time.Minute + 30*time.Second)
	hd := h.e.holds[HoldInfra]
	failedAt := t0.Add(6*time.Minute + 30*time.Second)
	if hd == nil || hd.Tries != 3 || !hd.Next.Equal(failedAt.Add(16*time.Minute)) {
		t.Fatalf("hold %+v, want the try the probe made due at 6m30s failed and counted: three tries, the next at 22m30s", hd)
	}
	was := *hd
	pings, _ := my.count()
	for ; at < 22*time.Minute+30*time.Second; at += 30 * time.Second {
		h.clock.Set(t0.Add(at))
		h.tick()
		if n, _ := my.count(); n != pings {
			t.Fatalf("at %v the hold asked MySQL %d times before the Next its failed try scheduled (22m30s)", at, n-pings)
		}
		if hd := h.e.holds[HoldInfra]; hd == nil || *hd != was {
			t.Fatalf("at %v the hold changed before its Next: %+v, was %+v", at, hd, was)
		}
	}

	h.clock.Set(t0.Add(at)) // 22m30s, the scheduled try: the try's own ping alone, which fails and counts
	h.tick()
	at += 30 * time.Second
	if n, unbounded := my.count(); n != pings+1 || unbounded != 5 {
		t.Fatalf("at 22m30s %d pings, %d of all a try's own, want the scheduled try's ping alone (the fifth)", n-pings, unbounded)
	}
	if hd := h.e.holds[HoldInfra]; hd == nil || hd.Tries != 4 || !hd.Next.Equal(t0.Add(52*time.Minute+30*time.Second)) {
		t.Fatalf("hold %+v, want the scheduled try failed: four tries, the next at 52m30s", hd)
	}

	my.flap(false)
	my.set(false) // MySQL answers for good: the probe asks again after the scheduled try, and the hold lifts
	tickTo(23 * time.Minute)
	if _, held := h.e.holds[HoldInfra]; held {
		t.Fatalf("the probe asked nothing after the scheduled try: %+v", h.e.holds[HoldInfra])
	}
	if ran := c.at; len(ran) != 1 || !ran[0].Equal(t0.Add(23*time.Minute)) {
		t.Fatalf("the stage ran at %v, want once, at 23m", ran)
	}
	if n := strings.Count(strings.Join(holdEvents(h), "\n"), "answers again"); n != 1 {
		t.Fatalf("hold events %q, want one line that says MySQL answers again for the outage", holdEvents(h))
	}
}

// A MySQL that flaps for the whole outage (each probe answers, each try's own
// ping fails) made every tick a failed try: 62 tries in 31 minutes, a hold
// event each, the backoff at its 30-minute cap within 2 minutes. Each try
// a probe made due now waits for the Next its failure scheduled, so the tries
// keep the hold's schedule (infraBackoff after each): the probe's at 30
// seconds, then at its Next (4m30s) the scheduled one, the probe's again at
// the tick after it (5m), the scheduled one at 21m and the probe's at 21m30s.
// The hold reaches NEEDS YOU at 30 minutes, as one whose MySQL stays down
// does, with five tries, and no stage runs.
func TestAFlappingMySQLReachesNeedsYouNoFasterThanTheSchedule(t *testing.T) {
	h, my, c := mysqlHarness(t)
	my.flap(true)
	var tries []time.Duration // the ticks whose try failed and counted
	for at := time.Duration(0); at <= 31*time.Minute; at += 30 * time.Second {
		h.clock.Set(t0.Add(at))
		before := 0
		if hd := h.e.holds[HoldInfra]; hd != nil {
			before = hd.Tries
		}
		h.tick()
		hd := h.e.holds[HoldInfra]
		if hd == nil {
			t.Fatalf("at %v the hold lifted though every try's ping failed", at)
		}
		if hd.Tries > before {
			tries = append(tries, at)
		}
		if blockers := h.e.Blocking(); (len(blockers) > 0) != (at >= holdNeedsYou) {
			t.Fatalf("at %v blockers %q, want NEEDS YOU from 30 minutes on, not before", at, blockers)
		}
	}
	want := []time.Duration{30 * time.Second, 4*time.Minute + 30*time.Second, 5 * time.Minute, 21 * time.Minute,
		21*time.Minute + 30*time.Second}
	if !slices.Equal(tries, want) {
		t.Fatalf("failed tries at %v, want %v: after each try a probe made due, none before the Next it scheduled", tries, want)
	}
	if hd := h.e.holds[HoldInfra]; hd.Tries != 5 || !hd.Next.Equal(t0.Add(51*time.Minute+30*time.Second)) {
		t.Fatalf("hold %+v, want five tries, the next at 51m30s", *hd)
	}
	if evs := holdEvents(h); len(evs) != 7 {
		t.Fatalf("hold events %q, want the begin, one line that MySQL answers again and the five failed tries", evs)
	}
	if n := len(c.list()); n != 0 {
		t.Fatalf("the stage ran %d times while every try's ping failed", n)
	}
}
