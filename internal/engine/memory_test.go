package engine

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

// The swap line of 2026-10-19, when the machine was slow, and its used
// bytes.
const slowSwap = "total = 51200.00M  used = 50281.62M  free = 918.38M  (encrypted)\n"

var slowSwapUsed = int64(math.Round(50281.62 * (1 << 20)))

// fakeSysctl answers the memory probe's two sysctl commands: what each
// prints, or the error it fails with.
type fakeSysctl struct {
	run  *execx.Fake
	done <-chan struct{} // the test's end: a held read gives up

	mu                  sync.Mutex
	level, swap         string
	levelFail, swapFail string        // non-empty: the command exits 1 with this stderr
	hold                chan struct{} // non-nil: the level read waits until it is closed
	held                chan struct{} // closed when the first held level read started
	afterLevel          func()        // runs once the level read answered
}

func newFakeSysctl() *fakeSysctl {
	s := &fakeSysctl{level: "1\n", swap: slowSwap}
	s.run = &execx.Fake{Rules: []execx.Rule{{Prefix: []string{sysctlPath, "-n"}, Fn: s.answer}}}
	return s
}

// install is a harness mod: the memory probe runs through the fake.
func (s *fakeSysctl) install(h *harness) {
	h.d.MemoryRunner = s.run
	s.done = h.ctx.Done()
}

func (s *fakeSysctl) answer(c execx.Cmd) (execx.Result, error) {
	oid := c.Args[len(c.Args)-1]
	s.mu.Lock()
	hold, after := s.hold, s.afterLevel
	if oid != pressureOID {
		hold, after = nil, nil
	}
	held := s.held
	if hold != nil {
		s.held = nil // closed once
	}
	s.mu.Unlock()
	if hold != nil {
		if held != nil {
			close(held)
		}
		select {
		case <-hold:
		case <-s.done:
		}
	}
	if after != nil {
		defer after()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out, fail := "", ""
	switch oid {
	case pressureOID:
		out, fail = s.level, s.levelFail
	case swapOID:
		out, fail = s.swap, s.swapFail
	default:
		fail = "sysctl: unknown oid '" + oid + "'"
	}
	if fail != "" {
		return execx.Result{Code: 1, Stderr: []byte(fail + "\n")}, &execx.ExitError{Cmd: c, Code: 1, Stderr: fail + "\n"}
	}
	return execx.Result{Stdout: []byte(out)}, nil
}

// prints sets what the pressure level and the swap commands print.
func (s *fakeSysctl) prints(level, swap string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.level, s.swap = level, swap
	s.levelFail, s.swapFail = "", ""
}

// fails makes the pressure level's command, the swap's or both exit 1 with
// stderr ("" = it answers).
func (s *fakeSysctl) fails(level, swap string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.levelFail, s.swapFail = level, swap
}

// holdLevel makes the level reads wait until release is closed; started is
// closed when the first of them is in the probe.
func (s *fakeSysctl) holdLevel(release chan struct{}) (started <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold, s.held = release, make(chan struct{})
	return s.held
}

// whenLevelAnswered runs fn once each level read answered, before the swap's.
func (s *fakeSysctl) whenLevelAnswered(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afterLevel = fn
}

// reads counts the sysctl commands run.
func (s *fakeSysctl) reads() int { return len(s.run.CallsWithPrefix(sysctlPath)) }

// memorySample is what the registry holds of the machine's memory.
func memorySample(h *harness) MemorySample {
	h.t.Helper()
	s, ok, err := readKVJSON[MemorySample](h.ctx, h.e, KVMachineMemory, "memory sample")
	if err != nil || !ok {
		h.t.Fatalf("no memory sample recorded: %v", err)
	}
	return s
}

// memoryCheck is two ticks' health checks around a finished sample: the
// first starts one (when the last is a minute old), the sample runs on its
// own goroutine, and the second, at the same time, records it and starts
// none. The toasts it showed are waited for.
func memoryCheck(h *harness) {
	h.t.Helper()
	h.e.health(h.ctx)
	h.e.memWG.Wait()
	h.e.health(h.ctx)
	h.settle()
}

// The two sysctl answers parse on their own: the three pressure levels, a
// number the kernel does not define, the swap line and garbage, each with
// the cause of an unknown reading.
func TestMemoryPressureAndSwapParse(t *testing.T) {
	for _, tc := range []struct {
		out   string
		level int
		cause string
	}{
		{"1\n", MemoryNormal, ""},
		{"2\n", MemoryWarn, ""},
		{" 4 \n", MemoryCritical, ""},
		{"3\n", MemoryUnknown, "kern.memorystatus_vm_pressure_level is 3, not 1, 2 or 4"},
		{"0\n", MemoryUnknown, "kern.memorystatus_vm_pressure_level is 0, not 1, 2 or 4"},
		{"normal\n", MemoryUnknown, `kern.memorystatus_vm_pressure_level printed "normal", not a level`},
		{"", MemoryUnknown, `kern.memorystatus_vm_pressure_level printed "", not a level`},
	} {
		level, cause := parsePressure(tc.out)
		if level != tc.level || cause != tc.cause {
			t.Errorf("parsePressure(%q) = %d, %q; want %d, %q", tc.out, level, cause, tc.level, tc.cause)
		}
	}
	const mib = 1 << 20
	for _, tc := range []struct {
		out         string
		used, total int64
		cause       string
	}{
		{slowSwap, slowSwapUsed, 51200 * mib, ""},
		{"total = 0.00M  used = 0.00M  free = 0.00M  \n", 0, 0, ""},
		{"total = 2.00G  used = 512.00M  free = 1.50G  (encrypted)", 512 * mib, 2048 * mib, ""},
		{"vm.swapusage: no such thing", 0, 0, `vm.swapusage printed "vm.swapusage: no such thing", not a swap reading`},
		{"", 0, 0, `vm.swapusage printed "", not a swap reading`},
	} {
		swap, cause := parseSwap(tc.out)
		switch {
		case cause != tc.cause:
			t.Errorf("parseSwap(%q) cause = %q, want %q", tc.out, cause, tc.cause)
		case tc.cause != "" && swap != nil:
			t.Errorf("parseSwap(%q) = %+v with a cause", tc.out, *swap)
		case tc.cause == "" && (swap == nil || swap.UsedBytes != tc.used || swap.TotalBytes != tc.total):
			t.Errorf("parseSwap(%q) = %+v, want used %d total %d", tc.out, swap, tc.used, tc.total)
		}
	}
	s := MemorySample{Level: MemoryWarn, Pressure: "warn", Swap: &MemorySwap{UsedBytes: slowSwapUsed, TotalBytes: 51200 * mib}}
	if got := s.SwapText(); got != "swap 49.1 of 50.0 GB used" {
		t.Errorf("SwapText = %q", got)
	}
	if got := (MemorySample{}).SwapText(); got != "swap unknown" {
		t.Errorf("SwapText without a swap reading = %q", got)
	}
}

// The health check starts a sample of the machine's memory at most once a
// minute, with fixed commands under a short timeout that the runner logs
// quietly (the engine warns once per cause), and a later check records the
// finished sample in kv.
func TestTheMemoryIsSampledAtMostOnceAMinute(t *testing.T) {
	s := newFakeSysctl()
	h := newHarness(t, s.install)
	s.prints("2\n", slowSwap)

	memoryCheck(h)
	if n := s.reads(); n != 2 {
		t.Fatalf("sysctl ran %d times at the first check, want the level and the swap", n)
	}
	for _, c := range s.run.CallsWithPrefix(sysctlPath) {
		if len(c.Args) != 2 || c.Args[0] != "-n" || (c.Args[1] != pressureOID && c.Args[1] != swapOID) || c.Timeout != memoryTimeout ||
			c.Timeout <= 0 || c.Timeout > 5*time.Second || !c.Probe || !c.Quiet || c.Mutates {
			t.Errorf("probe %s (timeout %s, probe %v, quiet %v, mutates %v)", c.String(), c.Timeout, c.Probe, c.Quiet, c.Mutates)
		}
	}
	got := memorySample(h)
	const mib = 1 << 20
	if got.Level != MemoryWarn || got.Pressure != "warn" || got.Swap == nil || got.Swap.TotalBytes != 51200*mib ||
		got.Swap.UsedBytes != slowSwapUsed || !got.SampledAt.Equal(h.clock.Now()) || got.Cause != "" {
		t.Fatalf("sample = %+v", got)
	}

	h.advance(30 * time.Second)
	memoryCheck(h)
	if n := s.reads(); n != 2 {
		t.Fatalf("sysctl ran %d times after checks within a minute, want 2", n)
	}
	h.advance(30 * time.Second)
	s.prints("1\n", slowSwap)
	memoryCheck(h)
	if n := s.reads(); n != 4 {
		t.Fatalf("sysctl ran %d times a minute after the first read, want 4", n)
	}
	if got := memorySample(h); got.Level != MemoryNormal || !got.SampledAt.Equal(h.clock.Now()) {
		t.Fatalf("sample a minute later = %+v", got)
	}
}

// The tick never waits for a sample: a probe that hangs (a spawn on a
// thrashing machine) leaves the health check to return at once, no second
// sample starts while one is in flight, and the check after it finished
// records it.
func TestTheTickNeverWaitsForAMemorySample(t *testing.T) {
	s := newFakeSysctl()
	h := newHarness(t, s.install)
	release := make(chan struct{})
	held := s.holdLevel(release)
	returned := make(chan struct{})
	go func() {
		h.e.health(h.ctx)
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(30 * time.Second):
		close(release)
		t.Fatal("the health check waited for the memory sample")
	}
	select { // the sample's goroutine is in the probe, which holds it
	case <-held:
	case <-time.After(30 * time.Second):
		close(release)
		t.Fatal("the health check started no memory sample")
	}
	h.advance(2 * time.Minute)
	h.e.health(h.ctx)
	if n := s.reads(); n != 1 {
		t.Fatalf("sysctl ran %d times with a sample in flight, want the one that hangs", n)
	}
	if _, ok, err := h.st.GetKV(h.ctx, KVMachineMemory); ok || err != nil {
		t.Fatalf("a sample was recorded before it finished (%v)", err)
	}
	close(release)
	h.e.memWG.Wait()
	h.e.health(h.ctx) // records it, and starts the next: the last started two minutes ago
	if got := memorySample(h); got.Level != MemoryNormal || got.Swap == nil {
		t.Fatalf("sample = %+v", got)
	}
	h.e.memWG.Wait()
	if n := s.reads(); n != 4 {
		t.Fatalf("sysctl ran %d times, want the next sample started by the check that recorded the first", n)
	}
}

// One command that fails keeps the other's reading: a level without a swap
// reading, or a swap reading without a level, is still recorded, with the
// cause of the part that is unknown.
func TestOneFailingSysctlKeepsTheOthersReading(t *testing.T) {
	s := newFakeSysctl()
	h := newHarness(t, s.install)
	s.prints("2\n", slowSwap)
	s.fails("sysctl: unknown oid 'kern.memorystatus_vm_pressure_level'", "")
	memoryCheck(h)
	got := memorySample(h)
	if got.Level != MemoryUnknown || got.Pressure != "unknown" || got.Swap == nil || got.Swap.TotalBytes != 51200<<20 ||
		got.Cause != "sysctl kern.memorystatus_vm_pressure_level failed: sysctl: unknown oid 'kern.memorystatus_vm_pressure_level'" {
		t.Fatalf("sample with the level failing = %+v", got)
	}

	h.advance(time.Minute)
	s.prints("4\n", "garbage")
	memoryCheck(h)
	got = memorySample(h)
	if got.Level != MemoryCritical || got.Pressure != "critical" || got.Swap != nil || got.Cause != `vm.swapusage printed "garbage", not a swap reading` {
		t.Fatalf("sample with the swap unreadable = %+v", got)
	}

	h.advance(time.Minute)
	s.fails("", "sysctl: unknown oid 'vm.swapusage'")
	memoryCheck(h)
	if got := memorySample(h); got.Level != MemoryCritical || got.Swap != nil || got.Cause != "sysctl vm.swapusage failed: sysctl: unknown oid 'vm.swapusage'" {
		t.Fatalf("sample with the swap failing = %+v", got)
	}
}

// Each change of the pressure level is one machine.memory_pressure event:
// the first known level since the start is recorded once as info, an
// unchanged level records nothing, warn is a warning, critical an error with
// one urgent toast per run of critical (a run lasts until a normal sample,
// so a level that flaps between critical and warn toasts once), and the
// return to normal is info. An unknown sample is no return to normal.
func TestEachChangeOfTheMemoryPressureIsOneEvent(t *testing.T) {
	s := newFakeSysctl()
	h := newHarness(t, s.install)
	sample := func(level string) {
		t.Helper()
		s.prints(level, slowSwap)
		h.advance(time.Minute)
		memoryCheck(h)
	}
	levels := func() []string {
		var out []string
		for _, ev := range h.events(EventMemoryPressure) {
			out = append(out, ev.Level+": "+ev.Message)
		}
		return out
	}
	toasts := func() int { return len(h.nh.titles("memory pressure critical")) }

	sample("2\n")
	sample("2\n")
	want := []string{"info: memory pressure warn (the first reading since the daemon started): swap 49.1 of 50.0 GB used"}
	if got := levels(); !slices.Equal(got, want) {
		t.Fatalf("events after two warn samples = %q, want %q", got, want)
	}

	sample("4\n")
	sample("4\n")
	sample("2\n")
	sample("4\n") // the same run: no second toast
	s.fails("sysctl: unknown oid 'kern.memorystatus_vm_pressure_level'", "")
	h.advance(time.Minute)
	memoryCheck(h) // unknown: no event, no return to normal
	sample("4\n")  // still critical: nothing new
	want = append(want,
		"error: memory pressure critical (was warn): swap 49.1 of 50.0 GB used",
		"warn: memory pressure warn (was critical): swap 49.1 of 50.0 GB used",
		"error: memory pressure critical (was warn): swap 49.1 of 50.0 GB used")
	if got := levels(); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if n := toasts(); n != 1 {
		t.Fatalf("%d critical toasts in one run of critical, want 1", n)
	}

	sample("1\n")
	want = append(want, "info: memory pressure back to normal (was critical): swap 49.1 of 50.0 GB used")
	if got := levels(); !slices.Equal(got, want) {
		t.Fatalf("events after the return to normal = %q, want %q", got, want)
	}
	sample("4\n") // a new run within the toast's window: keyed, no second toast
	if n := toasts(); n != 1 {
		t.Fatalf("%d critical toasts after a flap through normal within the hour, want 1", n)
	}
	sample("1\n")
	h.advance(memoryToastWindow)
	sample("4\n") // a new run past the window toasts again
	if n := toasts(); n != 2 {
		t.Fatalf("%d critical toasts after a new run past the window, want 2", n)
	}
}

// A restarted daemon records its first known level once, as info, even when
// it is the level the last daemon recorded, and a normal one after the last
// daemon's critical is no return to normal (that sample is stale), also when
// unknown samples came first; a critical one starts a run of critical, whose
// toast the key keeps from repeating within the hour.
func TestTheFirstMemorySampleAfterAStartIsRecordedOnce(t *testing.T) {
	s := newFakeSysctl()
	h := newHarness(t, s.install)
	s.prints("4\n", slowSwap)
	memoryCheck(h)
	h.e = New(h.d) // a restart over the same registry
	h.advance(5 * time.Minute)
	memoryCheck(h)
	evs := h.events(EventMemoryPressure)
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want one per start", evs)
	}
	for _, ev := range evs {
		if ev.Level != "info" || ev.Message != "memory pressure critical (the first reading since the daemon started): swap 49.1 of 50.0 GB used" {
			t.Errorf("event = %+v", ev)
		}
	}
	if n := len(h.nh.titles("memory pressure critical")); n != 1 {
		t.Fatalf("%d critical toasts over a restart within the hour, want 1", n)
	}

	h.e = New(h.d)
	h.advance(5 * time.Minute)
	s.fails("sysctl: unknown oid 'kern.memorystatus_vm_pressure_level'", "")
	memoryCheck(h) // unknown first: nothing yet
	h.advance(time.Minute)
	s.prints("1\n", slowSwap)
	memoryCheck(h)
	evs = h.events(EventMemoryPressure)
	if len(evs) != 3 || evs[2].Level != "info" || evs[2].Message != "memory pressure normal (the first reading since the daemon started): swap 49.1 of 50.0 GB used" {
		t.Fatalf("events after a restart at normal = %+v", evs)
	}
}

// The critical toast says what the machine's memory is and how many of
// magnum's agents are live and working, from the registry.
func TestTheCriticalMemoryToastCountsTheLiveAgents(t *testing.T) {
	s := newFakeSysctl()
	h := newHarness(t, s.install)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // starts a normal sample; the PR the sessions belong to
	pr := h.pr(1)
	for i, status := range []herdr.Status{herdr.StatusWorking, herdr.StatusIdle, herdr.StatusIdle} {
		sess := store.Session{PRID: pr.ID, Role: "role" + itoa(int64(i)), State: store.SessionLive, AgentStatus: new(string(status))}
		if _, err := h.st.CreateSession(h.ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.st.CreateSession(h.ctx, store.Session{PRID: pr.ID, Role: "parked", State: store.SessionParked}); err != nil {
		t.Fatal(err)
	}
	s.prints("4\n", slowSwap)
	h.advance(time.Minute)
	memoryCheck(h)
	var body string
	for _, toast := range h.nh.all() {
		if title, b, ok := strings.Cut(toast, " | "); ok && title == "magnum: memory pressure critical" {
			body = b
		}
	}
	if !strings.Contains(body, "swap 49.1 of 50.0 GB used, 3 agents live (1 working)") {
		t.Fatalf("toast body = %q", body)
	}
}

// A probe that fails (Linux has no such oid) makes the sample unknown with
// its cause, warned about once per oid and kind of failure (exit, timeout,
// start, parse) since the daemon started, whatever sysctl printed; the
// repeats are debug lines.
func TestAnUnknownMemoryCauseIsWarnedOnce(t *testing.T) {
	s := newFakeSysctl()
	h, logs := quietHarness(t, s.install)
	s.fails("sysctl: unknown oid 'kern.memorystatus_vm_pressure_level'", "sysctl: unknown oid 'vm.swapusage'")
	for range 4 {
		memoryCheck(h)
		h.advance(time.Minute)
	}
	got := memorySample(h)
	if got.Level != MemoryUnknown || got.Swap != nil ||
		got.Cause != "sysctl kern.memorystatus_vm_pressure_level failed: sysctl: unknown oid 'kern.memorystatus_vm_pressure_level'; sysctl vm.swapusage failed: sysctl: unknown oid 'vm.swapusage'" {
		t.Fatalf("sample = %+v", got)
	}
	const msg = "cannot read the machine's memory"
	if n := logLines(logs, "WARN", msg); n != 2 {
		t.Fatalf("%d warnings over four failed samples, want one per oid:\n%s", n, logs.String())
	}
	if n := logLines(logs, "DEBUG", msg); n != 6 {
		t.Fatalf("%d debug lines, want the six repeats", n)
	}
	if evs := h.events(EventMemoryPressure); len(evs) != 0 {
		t.Fatalf("an unknown sample recorded %+v", evs)
	}
	s.fails("sysctl: cannot stat /proc/sys/kern/memorystatus_vm_pressure_level", "sysctl: unknown oid 'vm.swapusage'")
	memoryCheck(h)
	if n := logLines(logs, "WARN", msg); n != 2 {
		t.Fatalf("%d warnings after another text of the same failure, want still 2", n)
	}
	if got := memorySample(h); !strings.Contains(got.Cause, "cannot stat /proc/sys/kern/memorystatus_vm_pressure_level") {
		t.Fatalf("the sample's cause is not the newest text: %q", got.Cause)
	}
	h.advance(time.Minute)
	s.prints("garbage\n", slowSwap)
	memoryCheck(h)
	if n := logLines(logs, "WARN", msg); n != 3 {
		t.Fatalf("%d warnings after a level that does not parse, want 3", n)
	}
	if w := warnLines(logs); len(w) != 3 {
		t.Fatalf("warnings:\n%s", strings.Join(w, "\n"))
	}
}

// Through the real runner, which logs every command, the probe's failures
// are warned about once per oid and kind of failure and the runner's own
// lines are debug lines: a non-zero exit whose text differs each time, a
// timeout, and a sysctl missing at its path, which also stops the probing
// for the daemon's life and keeps the unknown sample with its cause.
func TestEveryWarningOfTheMemoryProbeIsOncePerCause(t *testing.T) {
	// Only the timeout case times out: the others' timeout outlasts a shell's
	// start on a loaded machine (a load of 230 made 200ms one).
	const ample = time.Minute
	for _, tc := range []struct {
		name    string
		script  string // "" = no file at the probe's path
		timeout time.Duration
		warns   []string
	}{
		{"exit", "#!/bin/sh\necho \"sysctl: unknown oid '$2' (pid $$)\" >&2\nexit 1\n", ample,
			[]string{"sysctl kern.memorystatus_vm_pressure_level failed: sysctl: unknown oid", "sysctl vm.swapusage failed: sysctl: unknown oid"}},
		{"timeout", "#!/bin/sh\nsleep 30\n", 200 * time.Millisecond,
			[]string{"sysctl kern.memorystatus_vm_pressure_level failed: timed out after 200ms", "sysctl vm.swapusage failed: timed out after 200ms"}},
		{"missing", "", ample, []string{"no such file or directory"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "sysctl")
			if tc.script != "" {
				if err := os.WriteFile(bin, []byte(tc.script), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			h, logs := quietHarness(t, func(h *harness) {
				h.d.MemoryRunner = &execx.Real{Log: app.Printf{Logger: h.d.Logger, Level: slog.LevelInfo, Src: "exec"}}
			})
			h.e.mem.probe = memoryProbe{bin: bin, timeout: tc.timeout}
			for range 3 {
				memoryCheck(h)
				h.advance(time.Minute)
			}
			w := warnLines(logs)
			if len(w) != len(tc.warns) {
				t.Fatalf("%d warnings, want %d:\n%s", len(w), len(tc.warns), strings.Join(w, "\n"))
			}
			for i, want := range tc.warns {
				if !strings.Contains(w[i], "cannot read the machine's memory") || !strings.Contains(w[i], want) {
					t.Errorf("warning %d = %s, want the engine's naming %q", i, w[i], want)
				}
			}
			ran := 0
			for line := range strings.SplitSeq(logs.String(), "\n") {
				if strings.Contains(line, `msg="exec `+bin) { // the runner's line of a run
					ran++
				}
			}
			got := memorySample(h)
			if tc.name != "missing" {
				if ran != 6 {
					t.Fatalf("the runner logged %d runs, want 6:\n%s", ran, logs.String())
				}
				return
			}
			if ran != 1 || !strings.Contains(w[0], "not sampled again until the daemon restarts") {
				t.Fatalf("a missing sysctl ran %d times (want the first level read only), warning %s", ran, w[0])
			}
			if got.Level != MemoryUnknown || got.Swap != nil || !strings.Contains(got.Cause, "no such file or directory") {
				t.Fatalf("sample = %+v", got)
			}
		})
	}
}

// A sample the daemon's stop cut short records nothing: the stop comes
// between the two reads, after a critical level, and the level stays
// unknown, with no kv, event, toast or warning, and no swap read.
func TestAMemorySampleCutShortByAStopRecordsNothing(t *testing.T) {
	s := newFakeSysctl()
	h, logs := quietHarness(t, s.install)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	s.prints("4\n", slowSwap)
	s.whenLevelAnswered(cancel)
	h.e.health(ctx)
	h.e.memWG.Wait()
	h.e.health(h.ctx)
	h.settle()
	if h.e.mem.level != MemoryUnknown || h.e.mem.critical {
		t.Fatalf("the engine took the level %d (critical run %v) from a sample cut short", h.e.mem.level, h.e.mem.critical)
	}
	if _, ok, err := h.st.GetKV(h.ctx, KVMachineMemory); ok || err != nil {
		t.Fatalf("a sample was recorded after a stop (%v)", err)
	}
	if evs := h.events(EventMemoryPressure); len(evs) != 0 {
		t.Fatalf("events %+v", evs)
	}
	if toasts := h.nh.all(); len(toasts) != 0 {
		t.Fatalf("toasts %v", toasts)
	}
	if w := warnLines(logs); len(w) != 0 {
		t.Fatalf("warnings:\n%s", strings.Join(w, "\n"))
	}
	if n := s.reads(); n != 1 {
		t.Fatalf("sysctl ran %d times, want the level read only", n)
	}
}

// heldRunner is a probe that answers only once the test releases it, its
// context ended or not, as a sysctl that outlives the daemon's stop for a
// while (the runner's kill grace): started is closed when the first command
// is in it.
type heldRunner struct {
	started, release chan struct{}
	once             sync.Once
}

func (r *heldRunner) Run(ctx context.Context, c execx.Cmd) (execx.Result, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	if err := ctx.Err(); err != nil {
		return execx.Result{Code: -1}, &execx.RunError{Cmd: c, Err: err}
	}
	return execx.Result{Stdout: []byte("1\n")}, nil
}

// Run waits for the sample in flight before it returns, so the sample's
// goroutine ends with it: a probe that goes on past the stop holds Run until
// it answers, and the sample, cut short, records nothing.
func TestRunWaitsForTheMemorySampleInFlight(t *testing.T) {
	probe := &heldRunner{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(probe.release) }) }
	defer release()
	h, logs := quietHarness(t, func(h *harness) { h.d.MemoryRunner = probe })
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	done := h.goRun(ctx, Options{NoSignals: true})
	select {
	case <-probe.started:
	case <-time.After(30 * time.Second):
		t.Fatal("Run started no memory sample")
	}
	cancel()
	// That Run does not return can only be seen over a window: a slow
	// machine can make this check miss a Run that does not wait, never fail
	// one that does.
	select {
	case <-done:
		t.Fatal("Run returned with the memory sample still in flight")
	case <-time.After(500 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return once the memory sample ended")
	}
	if _, ok, err := h.st.GetKV(h.ctx, KVMachineMemory); ok || err != nil {
		t.Fatalf("a sample was recorded at the stop (%v)", err)
	}
	if n := logLines(logs, "WARN", "memory"); n != 0 {
		t.Fatalf("%d warnings about the memory at the stop:\n%s", n, logs.String())
	}
}
