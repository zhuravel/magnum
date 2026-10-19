package engine

// The machine's memory (DECISIONS "The daemon records the machine's memory
// pressure and swap"): on 2026-10-19 the review machine had used 50.3 of
// 51.2 GB of swap at pressure level warn and compressed and decompressed
// about 6 GB every five seconds, and magnum, which guards the disk, knew
// nothing of memory. The health check starts a sample of the kernel's
// pressure level and the swap at most once a minute, on a goroutine of its
// own, and a later check records it; nothing is held on them.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// KVMachineMemory holds the daemon's newest memory sample (MemorySample,
// JSON), which `magnum status` shows on its memory line.
const KVMachineMemory = "machine.memory"

// EventMemoryPressure is recorded at each change of the machine's memory
// pressure level: the first known level since the daemon started (info), a
// return to normal (info), warn (warn) and critical (error, with one urgent
// toast per run of critical). A sample whose level is unknown changes
// nothing.
const EventMemoryPressure = "machine.memory_pressure"

// The kernel's memory pressure levels (kern.memorystatus_vm_pressure_level);
// MemoryUnknown is a sample without one.
const (
	MemoryUnknown  = 0
	MemoryNormal   = 1
	MemoryWarn     = 2
	MemoryCritical = 4
)

const (
	// memoryEvery bounds how often a sample starts, memoryTimeout each of
	// its two commands (sysctl answers in milliseconds; the tick never
	// waits for either).
	memoryEvery   = time.Minute
	memoryTimeout = 3 * time.Second
	// memoryToastKey and memoryToastWindow dedupe the critical toast: one
	// per run of critical, and none within the window of the last, so a
	// level that flaps through normal does not toast again at once.
	memoryToastKey    = "memory:critical"
	memoryToastWindow = time.Hour
	// The probe's fixed command: macOS's sysctl, at its absolute path as
	// notify's osascript and reveal's commands are, and the two values it
	// reads. Another system (Linux has neither value) answers with a
	// failure.
	sysctlPath  = "/usr/sbin/sysctl"
	pressureOID = "kern.memorystatus_vm_pressure_level"
	swapOID     = "vm.swapusage"
)

// The kinds of failure of a part of a sample (memoryFailure.kind).
const (
	memoryFailExit    = "exit"    // sysctl exited non-zero
	memoryFailTimeout = "timeout" // sysctl did not answer within the timeout
	memoryFailStart   = "start"   // sysctl did not run
	memoryFailParse   = "parse"   // sysctl printed no level or swap reading
)

// MemorySample is one reading of the machine's memory.
type MemorySample struct {
	// Level is kern.memorystatus_vm_pressure_level (MemoryNormal,
	// MemoryWarn, MemoryCritical), MemoryUnknown when it could not be read;
	// Pressure names it ("normal", "warn", "critical", "unknown").
	Level    int    `json:"level"`
	Pressure string `json:"pressure"`
	// Swap is vm.swapusage; nil when it could not be read.
	Swap      *MemorySwap `json:"swap,omitempty"`
	SampledAt time.Time   `json:"sampled_at"`
	// Cause says why the level, the swap or both are unknown ("" when both
	// were read).
	Cause string `json:"cause,omitempty"`
}

// MemorySwap is the swap in use and the swap files' size, in bytes.
type MemorySwap struct {
	UsedBytes  int64 `json:"used_bytes"`
	TotalBytes int64 `json:"total_bytes"`
}

// PressureName names a pressure level: "normal", "warn", "critical", or
// "unknown" for any other.
func PressureName(level int) string {
	switch level {
	case MemoryNormal:
		return "normal"
	case MemoryWarn:
		return "warn"
	case MemoryCritical:
		return "critical"
	}
	return "unknown"
}

// SwapText is the swap as the status line, the doctor, the events and the
// toast say it: "swap 49.1 of 50.0 GB used" (GB of 2³⁰ bytes, as the disk
// line and Activity Monitor count them), or "swap unknown".
func (s MemorySample) SwapText() string {
	if s.Swap == nil {
		return "swap unknown"
	}
	const gb = 1 << 30
	return fmt.Sprintf("swap %.1f of %.1f GB used", float64(s.Swap.UsedBytes)/gb, float64(s.Swap.TotalBytes)/gb)
}

// ReadMemory takes a memory sample at now through r: the pressure level and
// the swap, each read and parsed on its own, so one that fails leaves the
// other's reading in the sample (`magnum doctor` reads it so).
func ReadMemory(ctx context.Context, r execx.Runner, now time.Time) MemorySample {
	s, _ := readMemory(ctx, r, memoryProbe{}, now)
	return s
}

// memoryProbe is the command a sample runs and its timeout; the zero value
// is sysctlPath and memoryTimeout (tests set another).
type memoryProbe struct {
	bin     string
	timeout time.Duration
}

func (p memoryProbe) withDefaults() memoryProbe {
	if p.bin == "" {
		p.bin = sysctlPath
	}
	if p.timeout <= 0 {
		p.timeout = memoryTimeout
	}
	return p
}

// memoryFailure is a part of a sample that could not be read: its oid and
// kind of failure (memoryFailExit, …), which key the warning said once,
// and the text the sample's Cause and the warning give. missing says that
// no sysctl is at the probe's path: the probing stops.
type memoryFailure struct {
	oid, kind, text string
	missing         bool
}

// readMemory is ReadMemory through probe p, with each part that is unknown
// as a memoryFailure, in the order of the sample's Cause. A sysctl missing
// at its path, or ctx ending, leaves the swap unread.
func readMemory(ctx context.Context, r execx.Runner, p memoryProbe, now time.Time) (MemorySample, []memoryFailure) {
	p = p.withDefaults()
	s := MemorySample{SampledAt: now}
	var fails []memoryFailure
	if out, err := p.sysctl(ctx, r, pressureOID); err != nil {
		fails = append(fails, p.failure(pressureOID, err))
	} else if level, cause := parsePressure(out); cause != "" {
		fails = append(fails, memoryFailure{oid: pressureOID, kind: memoryFailParse, text: cause})
	} else {
		s.Level = level
	}
	if ctx.Err() == nil && (len(fails) == 0 || !fails[0].missing) {
		if out, err := p.sysctl(ctx, r, swapOID); err != nil {
			fails = append(fails, p.failure(swapOID, err))
		} else if swap, cause := parseSwap(out); cause != "" {
			fails = append(fails, memoryFailure{oid: swapOID, kind: memoryFailParse, text: cause})
		} else {
			s.Swap = swap
		}
	}
	s.Pressure = PressureName(s.Level)
	texts := make([]string, len(fails))
	for i, f := range fails {
		texts[i] = f.text
	}
	s.Cause = strings.Join(texts, "; ")
	return s, fails
}

// sysctl prints the value of oid (`sysctl -n <oid>`): a read-only probe
// whose failures its caller reports, once per cause (Quiet; Probe for any
// other runner that reads it).
func (p memoryProbe) sysctl(ctx context.Context, r execx.Runner, oid string) (string, error) {
	res, err := r.Run(ctx, execx.Cmd{Name: p.bin, Args: []string{"-n", oid}, Timeout: p.timeout, Probe: true, Quiet: true, Label: "sysctl " + oid})
	return string(res.Stdout), err
}

// failure is the failed read of oid: what sysctl said on stderr for a
// non-zero exit, "timed out after 3s" for its timeout, else why it did not
// run.
func (p memoryProbe) failure(oid string, err error) memoryFailure {
	f := memoryFailure{oid: oid, kind: memoryFailStart}
	why := err.Error()
	if ee, ok := errors.AsType[*execx.ExitError](err); ok {
		f.kind = memoryFailExit
		if stderr := strings.TrimSpace(ee.Stderr); stderr != "" {
			why = textx.FirstLine(stderr)
		}
	} else if errors.Is(err, context.DeadlineExceeded) {
		f.kind, why = memoryFailTimeout, "timed out after "+p.timeout.String()
	} else {
		if re, ok := errors.AsType[*execx.RunError](err); ok {
			why = re.Err.Error()
		}
		f.missing = errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
	}
	f.text = "sysctl " + oid + " failed: " + textx.Clip(execx.Redact(why), 160)
	return f
}

// parsePressure reads `sysctl -n kern.memorystatus_vm_pressure_level`: the
// level, or MemoryUnknown with the cause for anything but 1, 2 or 4.
func parsePressure(out string) (int, string) {
	v := strings.TrimSpace(out)
	n, err := strconv.Atoi(v)
	if err != nil {
		return MemoryUnknown, fmt.Sprintf("%s printed %q, not a level", pressureOID, textx.Clip(v, 40))
	}
	switch n {
	case MemoryNormal, MemoryWarn, MemoryCritical:
		return n, ""
	}
	return MemoryUnknown, fmt.Sprintf("%s is %d, not 1, 2 or 4", pressureOID, n)
}

// swapRe matches vm.swapusage's line: "total = 51200.00M  used = 50281.62M
// free = 918.38M  (encrypted)".
var swapRe = regexp.MustCompile(`^total = ([0-9]+(?:\.[0-9]+)?)([KMGT]) +used = ([0-9]+(?:\.[0-9]+)?)([KMGT])\b`)

// parseSwap reads `sysctl -n vm.swapusage`: the swap, or nil with the
// cause when the line is not one.
func parseSwap(out string) (*MemorySwap, string) {
	v := strings.TrimSpace(out)
	m := swapRe.FindStringSubmatch(v)
	if m == nil {
		return nil, fmt.Sprintf("%s printed %q, not a swap reading", swapOID, textx.Clip(textx.FirstLine(v), 60))
	}
	return &MemorySwap{TotalBytes: swapBytes(m[1], m[2]), UsedBytes: swapBytes(m[3], m[4])}, ""
}

// swapBytes is n units (K, M, G or T, of 1024) in bytes; the regular
// expression admits only a number.
func swapBytes(n, unit string) int64 {
	v, _ := strconv.ParseFloat(n, 64)
	shift := map[string]int{"K": 10, "M": 20, "G": 30, "T": 40}[unit]
	return int64(math.Round(v * float64(int64(1)<<shift)))
}

// LiveAgents counts the registry's live sessions (starting or live, as
// Store.LiveSessions lists them) and those of them herdr last saw working;
// the others are idle.
func LiveAgents(live []store.Session) (n, working int) {
	for _, s := range live {
		if herdr.Status(deref(s.AgentStatus)) == herdr.StatusWorking {
			working++
		}
	}
	return len(live), working
}

// memoryWatch is what the daemon knows of the machine's memory
// (Engine.mem). Tick goroutine only, but for done, which the sample's
// goroutine sends its one result on.
type memoryWatch struct {
	// probe is the command a sample runs (the zero value: sysctl).
	probe memoryProbe
	// started is when the last sample started: the next waits memoryEvery.
	// inflight says it has not been recorded yet: at most one runs.
	started  time.Time
	inflight bool
	done     chan memoryRead
	// off says that no sysctl is at the probe's path: the memory is not
	// sampled again until the daemon restarts, and the last sample, unknown
	// with that cause, stays recorded.
	off bool
	// level is the last known level since the daemon started
	// (MemoryUnknown: none yet); an unknown sample leaves it.
	level int
	// critical is a run of critical: from a critical sample to the next
	// normal one, whose toast went out (or was kept back by the key).
	critical bool
	// warned are the failures warned about since the daemon started, by
	// oid and kind ("vm.swapusage:exit"): one is said once, whatever
	// sysctl printed.
	warned map[string]bool
}

// memoryRead is a finished sample handed to the tick. stopped says that the
// daemon's stop cut it short (or it panicked): nothing is recorded.
type memoryRead struct {
	sample   MemorySample
	failures []memoryFailure
	stopped  bool
}

// checkMemory records the sample that finished since the last health check
// (recordMemory) and starts the next when the last started memoryEvery ago,
// none is in flight and the probing goes on. The sample runs on a goroutine
// of its own (memWG) under ctx, the tick's, so the tick never waits for
// sysctl and the daemon's stop ends it. Nothing runs in a dry run or
// without MemoryRunner.
func (e *Engine) checkMemory(ctx context.Context) {
	if e.d.DryRun || e.d.MemoryRunner == nil {
		return
	}
	select {
	case r := <-e.mem.done: // a nil channel (no sample yet) is never ready
		e.mem.inflight = false
		e.recordMemory(ctx, r)
	default:
	}
	now := e.now()
	if e.mem.inflight || e.mem.off || ctx.Err() != nil || (!e.mem.started.IsZero() && now.Sub(e.mem.started) < memoryEvery) {
		return
	}
	if e.mem.done == nil {
		e.mem.done = make(chan memoryRead, 1) // at most one sample in flight: its send never blocks
	}
	e.mem.started, e.mem.inflight = now, true
	done, run, probe := e.mem.done, e.d.MemoryRunner, e.mem.probe
	e.memWG.Go(func() {
		read := memoryRead{stopped: true}
		defer func() { done <- read }()
		e.safely(ctx, "memory sample", "", func() {
			s, fails := readMemory(ctx, run, probe, now)
			read = memoryRead{sample: s, failures: fails, stopped: ctx.Err() != nil}
		}, nil)
	})
}

// recordMemory records a finished sample (KVMachineMemory) and notes a
// change of its level. Each failure is warned about once per oid and kind
// since the daemon started (later ones are debug lines), and a sysctl
// missing at its path stops the probing. A sample the daemon's stop cut
// short records nothing.
func (e *Engine) recordMemory(ctx context.Context, r memoryRead) {
	if r.stopped || ctx.Err() != nil {
		e.log.Debug("memory sample cut short; the daemon is stopping")
		return
	}
	if e.mem.warned == nil {
		e.mem.warned = map[string]bool{}
	}
	for _, f := range r.failures {
		msg := "cannot read the machine's memory; trying again in a minute"
		if f.missing {
			e.mem.off = true
			msg = "cannot read the machine's memory: no sysctl at " + e.mem.probe.withDefaults().bin + "; not sampled again until the daemon restarts"
		}
		if key := f.oid + ":" + f.kind; e.mem.warned[key] {
			e.log.Debug(msg, "err", f.text)
		} else {
			e.mem.warned[key] = true
			e.log.Warn(msg, "err", f.text)
		}
	}
	if err := writeKVJSON(ctx, e, KVMachineMemory, "memory sample", r.sample); err != nil {
		e.warnUnlessStopped(ctx, err, "record the memory sample")
	}
	e.noteMemoryLevel(ctx, r.sample)
}

// noteMemoryLevel records EventMemoryPressure when s's level differs from
// the last known one (the first known level since the daemon started is
// info), and shows the urgent toast at the start of a run of critical. A
// sample whose level is unknown is no change, and never a return to normal.
func (e *Engine) noteMemoryLevel(ctx context.Context, s MemorySample) {
	if s.Level == MemoryUnknown {
		return
	}
	prev := e.mem.level
	e.mem.level = s.Level
	switch {
	case s.Level == MemoryNormal:
		e.mem.critical = false
	case s.Level == MemoryCritical && !e.mem.critical:
		e.mem.critical = true
		e.toastCriticalMemory(ctx, s)
	}
	if prev == s.Level {
		return
	}
	level, msg := "info", fmt.Sprintf("memory pressure %s (the first reading since the daemon started)", s.Pressure)
	switch {
	case prev == MemoryUnknown:
	case s.Level == MemoryNormal:
		msg = fmt.Sprintf("memory pressure back to normal (was %s)", PressureName(prev))
	case s.Level == MemoryWarn:
		level, msg = "warn", fmt.Sprintf("memory pressure warn (was %s)", PressureName(prev))
	default:
		level, msg = "error", fmt.Sprintf("memory pressure critical (was %s)", PressureName(prev))
	}
	data := map[string]any{"level": s.Level, "pressure": s.Pressure}
	if prev != MemoryUnknown {
		data["from"] = PressureName(prev)
	}
	if s.Swap != nil {
		data["swap_used_bytes"], data["swap_total_bytes"] = s.Swap.UsedBytes, s.Swap.TotalBytes
	}
	e.event(ctx, level, "", EventMemoryPressure, msg+": "+s.SwapText(), data)
}

// toastCriticalMemory is the urgent toast of a run of critical: the swap and
// how many of magnum's agents are live and working.
func (e *Engine) toastCriticalMemory(ctx context.Context, s MemorySample) {
	what := s.SwapText()
	if live, err := e.st.LiveSessions(ctx); err == nil {
		n, working := LiveAgents(live)
		what += fmt.Sprintf(", %s live (%d working)", textx.Count(n, "agent", "agents"), working)
	}
	e.urgent(memoryToastKey, "magnum: memory pressure critical",
		"The machine's memory pressure is critical: "+what+". magnum holds nothing on it; `magnum status` shows the memory line.",
		memoryToastWindow)
}
