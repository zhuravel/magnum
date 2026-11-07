package factory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
)

const (
	mib = int64(1) << 20
	gib = int64(1) << 30
)

// culpritProcs is a process table under memory pressure: three large
// processes and three Codex app-server brokers, all under launchd (pid 1),
// as the plugin detaches each one it starts: one 3 days old with MCP
// servers under it, one 30 hours old, and the 2-hour-old broker of a live
// session.
var culpritProcs = []MemProc{
	{PID: 1, PPID: 0, RSS: 10 * mib, Name: "launchd", Age: 11 * 24 * time.Hour},
	{PID: 812, PPID: 1, RSS: 3 * gib, Name: "Google Chrome Helper (Renderer)", Age: time.Hour},
	{PID: 90, PPID: 80, RSS: 5 * gib / 2, Name: "codex", Age: 3 * time.Hour},
	{PID: 77, PPID: 1, RSS: 3 * gib / 2, Name: "Slack", Age: 48 * time.Hour},
	{PID: 60, PPID: 1, RSS: 512 * mib, Name: "Finder", Age: 48 * time.Hour},
	{PID: 455, PPID: 1, RSS: 256 * mib, Name: "node", Broker: true, Age: 74 * time.Hour},
	{PID: 500, PPID: 455, RSS: gib, Name: "node", Age: 74 * time.Hour},
	{PID: 501, PPID: 500, RSS: 768 * mib, Name: "npm", Age: 74 * time.Hour},
	{PID: 457, PPID: 1, RSS: 512 * mib, Name: "node", Broker: true, Age: 2 * time.Hour},
	{PID: 458, PPID: 1, RSS: 128 * mib, Name: "node", Broker: true, Age: 30 * time.Hour},
}

// The memory hold's reason, the wait of every launch it holds, names the 3
// largest resident processes and every Codex app-server broker with its
// age, oldest first, with the memory of their trees. A broker's parent says
// nothing (the plugin detaches each one, so a live session's broker is
// under launchd too): only one under launchd older than a day "may be
// orphaned". It reads the process table when the hold begins and again
// only every culpritsEvery while it lasts, and a hold after a lift reads it
// afresh.
func TestTheMemoryHoldNamesTheLargestProcessesAndTheBrokersAges(t *testing.T) {
	var swap, reads atomic.Int64
	swap.Store(97)
	h := newHarness(t, func(d *Deps) {
		d.Memory = func(context.Context) engine.MemorySample {
			return engine.MemorySample{Level: engine.MemoryNormal, Swap: &engine.MemorySwap{UsedBytes: swap.Load() * gib, TotalBytes: 100 * gib}}
		}
		d.MemProcs = func(context.Context) ([]MemProc, error) {
			reads.Add(1)
			return culpritProcs, nil
		}
	})
	h.tick()
	want := "swap 97% used, launches wait below 80%; largest: Google Chrome Helper (Renderer) 3.0 GB (pid 812), " +
		"codex 2.5 GB (pid 90), Slack 1.5 GB (pid 77); Codex app-server brokers, oldest first: pid 455 (3d2h, may be orphaned), " +
		"pid 458 (1d6h, may be orphaned), pid 457 (2h), 2.6 GB with their children"
	if hd, held := h.e.holds[HoldMemory]; !held || hd.Reason != want {
		t.Fatalf("memory hold %+v (%v), want the reason\n%q", hd, held, want)
	}
	if who, what, ok := h.e.mayLaunch(); ok || who != HoldMemory || what != want {
		t.Errorf("a launch waits on %s: %q (%v), want the memory hold's reason", who, what, ok)
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("%d reads of the process table, want 1", n)
	}

	swap.Store(98)
	h.clock.Add(culpritsEvery - time.Second)
	h.tick()
	if n := reads.Load(); n != 1 || !strings.HasPrefix(h.e.holds[HoldMemory].Reason, "swap 98% used") ||
		!strings.Contains(h.e.holds[HoldMemory].Reason, "; largest: Google Chrome Helper") {
		t.Fatalf("%d reads, reason %q: want the swap read afresh and the culprits kept", n, h.e.holds[HoldMemory].Reason)
	}
	h.clock.Add(time.Second)
	h.tick()
	if n := reads.Load(); n != 2 {
		t.Fatalf("%d reads after culpritsEvery, want 2", n)
	}

	swap.Store(50)
	h.tick()
	if _, held := h.e.holds[HoldMemory]; held {
		t.Fatal("the memory hold did not lift")
	}
	swap.Store(90)
	h.tick()
	if n := reads.Load(); n != 3 || !strings.Contains(h.e.holds[HoldMemory].Reason, "Codex app-server brokers, oldest first") {
		t.Fatalf("%d reads, reason %q: want a new hold to read the table afresh", n, h.e.holds[HoldMemory].Reason)
	}
}

// Without a broker the reason names the largest processes alone; a lone
// young broker is named with its age and nothing more; a table that cannot
// be read leaves the swap's reason as it was.
func TestTheMemoryHoldWithoutBrokersOrATable(t *testing.T) {
	var fail atomic.Bool
	h := newHarness(t, func(d *Deps) {
		d.Memory = func(context.Context) engine.MemorySample {
			return engine.MemorySample{Level: engine.MemoryCritical, Swap: &engine.MemorySwap{UsedBytes: 4 * gib, TotalBytes: 8 * gib}}
		}
		d.MemProcs = func(context.Context) ([]MemProc, error) {
			if fail.Load() {
				return nil, fmt.Errorf("read the process table: ps: exit 1")
			}
			return culpritProcs[:5], nil
		}
	})
	h.tick()
	want := "critical pressure, swap 4.0 of 8.0 GB used; largest: Google Chrome Helper (Renderer) 3.0 GB (pid 812), " +
		"codex 2.5 GB (pid 90), Slack 1.5 GB (pid 77)"
	if got := h.e.holds[HoldMemory].Reason; got != want {
		t.Fatalf("reason %q, want %q", got, want)
	}

	if got := culpritsText([]MemProc{culpritProcs[8]}); got != "; largest: node 512 MB (pid 457); Codex app-server broker: pid 457 (2h), 512 MB with its children" {
		t.Errorf("a lone young broker: %q", got)
	}

	fail.Store(true)
	h.clock.Add(culpritsEvery)
	h.tick()
	if got := h.e.holds[HoldMemory].Reason; got != "critical pressure, swap 4.0 of 8.0 GB used" {
		t.Fatalf("reason %q, want the pressure alone when the table cannot be read", got)
	}
}

// ReadMemProcs reads the process table with two ps reads in the C locale,
// and nothing else: it kills nothing. A name is the executable's base name,
// spaces kept and a token in it masked (execx.Redact); the age is ps's
// elapsed time; a broker is told by its arguments, which the result never
// carries.
func TestReadMemProcsReadsThePsTableAndKillsNothing(t *testing.T) {
	token := "ghp_" + strings.Repeat("a", 36)
	f := &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"ps", "-A", "-o", "pid=,ppid=,rss=,etime=,comm="}, Result: execx.Result{Stdout: []byte(
			"    1     0   10240 11-03:25:51 /sbin/launchd\n" +
				"  455     1  262144  1-02:00:00 /opt/homebrew/bin/node\n" +
				"  812     1 3145728       05:07 /Applications/Some Browser.app/Contents/MacOS/Some Browser Helper (Renderer)\n" +
				"  900     1    1024    01:02:03 /tmp/" + token + "\n" +
				"garbage line\n")}},
		{Prefix: []string{"ps", "-A", "-o", "pid=,args="}, Result: execx.Result{Stdout: []byte(
			"    1 /sbin/launchd\n" +
				"  455 node /opt/codex/scripts/app-server-broker.mjs --port 0\n" +
				"  812 /Applications/Some Browser.app/Contents/MacOS/Some Browser Helper (Renderer) --type=renderer --token=s3cr3t\n")}},
	}}
	procs, err := ReadMemProcs(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	want := []MemProc{
		{PID: 1, PPID: 0, RSS: 10 * mib, Name: "launchd", Age: 11*24*time.Hour + 3*time.Hour + 25*time.Minute + 51*time.Second},
		{PID: 455, PPID: 1, RSS: 256 * mib, Name: "node", Broker: true, Age: 26 * time.Hour},
		{PID: 812, PPID: 1, RSS: 3 * gib, Name: "Some Browser Helper (Renderer)", Age: 5*time.Minute + 7*time.Second},
		{PID: 900, PPID: 1, RSS: mib, Name: "<redacted>", Age: time.Hour + 2*time.Minute + 3*time.Second},
	}
	if !slices.Equal(procs, want) {
		t.Fatalf("procs = %+v\nwant   %+v", procs, want)
	}
	if strings.Contains(fmt.Sprintf("%+v", procs), "s3cr3t") {
		t.Errorf("the result carries a process's arguments: %+v", procs)
	}
	if len(f.Calls) != 2 {
		t.Fatalf("%d commands, want the 2 ps reads", len(f.Calls))
	}
	for _, c := range f.Calls {
		if c.Name != "ps" || c.Mutates || c.Env["LC_ALL"] != "C" {
			t.Errorf("ran %s %v (mutates %v, LC_ALL %q), want only ps reads in the C locale", c.Name, c.Args, c.Mutates, c.Env["LC_ALL"])
		}
	}
}
