package factory

// The memory hold's culprits (B #13 of the 2026-11-07 audit): the hold's
// reason names the largest resident processes and the Codex app-server
// brokers whose parent is gone, with their pids, so you see what holds the
// memory without looking for it. It reads the process table and kills
// nothing: what to stop is yours to decide.

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

const (
	// culpritsEvery is how often the memory hold reads the process table
	// again while it lasts; it reads it when it begins.
	culpritsEvery = 10 * time.Minute
	// culpritsLargest is how many of the largest processes it names, and
	// culpritsBrokers how many brokers' pids before "and N more".
	culpritsLargest = 3
	culpritsBrokers = 5
	// memPsTimeout bounds each ps read of ReadMemProcs.
	memPsTimeout = 10 * time.Second
	// brokerMark is what the arguments of a Codex app-server broker hold
	// (node .../app-server-broker.mjs): one whose parent is gone keeps its
	// MCP servers alive for nobody.
	brokerMark = "app-server-broker"
)

// MemProc is a process as the memory hold names it: its id, its parent's,
// its resident memory in bytes, its executable's base name (never its
// arguments) and whether it is a Codex app-server broker.
type MemProc struct {
	PID, PPID int
	RSS       int64
	Name      string
	Broker    bool
}

// ReadMemProcs reads the process table through r for the memory hold: `ps
// -A -o pid=,ppid=,rss=,comm=` for the ids, the resident memory and the
// names, then `ps -A -o pid=,args=` to tell the brokers, both in the C
// locale. The arguments stay here: only whether they name a broker leaves.
// It runs nothing else. A line it cannot read is skipped.
func ReadMemProcs(ctx context.Context, r execx.Runner) ([]MemProc, error) {
	ps := func(cols string) (string, error) {
		res, err := r.Run(ctx, execx.Cmd{Name: "ps", Args: []string{"-A", "-o", cols}, Env: map[string]string{"LC_ALL": "C"},
			Timeout: memPsTimeout, Label: "ps for the memory hold", Quiet: true})
		if err != nil {
			return "", fmt.Errorf("read the process table: %w", err)
		}
		return string(res.Stdout), nil
	}
	table, err := ps("pid=,ppid=,rss=,comm=")
	if err != nil {
		return nil, err
	}
	args, err := ps("pid=,args=")
	if err != nil {
		return nil, err
	}
	brokers := map[int]bool{}
	for line := range strings.SplitSeq(args, "\n") {
		if m := psArgsRow.FindStringSubmatch(line); m != nil && strings.Contains(m[2], brokerMark) {
			pid, _ := strconv.Atoi(m[1])
			brokers[pid] = true
		}
	}
	var out []MemProc
	for line := range strings.SplitSeq(table, "\n") {
		m := psMemRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		ppid, _ := strconv.Atoi(m[2])
		kb, _ := strconv.ParseInt(m[3], 10, 64)
		if pid <= 0 {
			continue
		}
		out = append(out, MemProc{PID: pid, PPID: ppid, RSS: kb << 10, Name: filepath.Base(strings.TrimSpace(m[4])), Broker: brokers[pid]})
	}
	return out, nil
}

// psMemRow is a line of the table read: pid, ppid, rss (KiB) and comm, the
// last column, which may hold spaces (an app bundle's path). psArgsRow is a
// line of the arguments read: pid and the arguments.
var (
	psMemRow  = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+(\d+)\s+(\S.*)$`)
	psArgsRow = regexp.MustCompile(`^\s*(\d+)\s+(.*)$`)
)

// culpritsText is what the memory hold adds to its reason from the process
// table procs: the culpritsLargest largest resident processes, and the
// brokers whose parent is gone (adopted by launchd, pid 1) with the memory
// of their trees, e.g. "; largest: codex 2.5 GB (pid 90), ...; orphaned
// Codex app-server brokers: pids 455, 457 (2.5 GB with their children)".
// "" for an empty table.
func culpritsText(procs []MemProc) string {
	if len(procs) == 0 {
		return ""
	}
	largest := slices.SortedFunc(slices.Values(procs), func(a, b MemProc) int {
		return cmp.Or(cmp.Compare(b.RSS, a.RSS), cmp.Compare(a.PID, b.PID))
	})
	var names []string
	for _, p := range largest[:min(culpritsLargest, len(largest))] {
		names = append(names, fmt.Sprintf("%s %s (pid %d)", p.Name, memSize(p.RSS), p.PID))
	}
	text := "; largest: " + strings.Join(names, ", ")

	children := map[int][]MemProc{}
	for _, p := range procs {
		if p.PID != p.PPID {
			children[p.PPID] = append(children[p.PPID], p)
		}
	}
	var pids []string
	var held int64
	for _, p := range slices.SortedFunc(slices.Values(procs), func(a, b MemProc) int { return cmp.Compare(a.PID, b.PID) }) {
		if !p.Broker || p.PPID != 1 {
			continue
		}
		pids = append(pids, strconv.Itoa(p.PID))
		held += treeRSS(p, children)
	}
	switch n := len(pids); {
	case n == 0:
		return text
	case n == 1:
		return text + fmt.Sprintf("; orphaned Codex app-server broker: pid %s (%s with its children)", pids[0], memSize(held))
	case n > culpritsBrokers:
		pids = append(pids[:culpritsBrokers], fmt.Sprintf("and %d more", n-culpritsBrokers))
	}
	return text + fmt.Sprintf("; orphaned Codex app-server brokers: pids %s (%s with their children)", strings.Join(pids, ", "), memSize(held))
}

// treeRSS is the resident memory of p and every process under it.
func treeRSS(p MemProc, children map[int][]MemProc) int64 {
	sum := p.RSS
	seen := map[int]bool{p.PID: true}
	queue := []int{p.PID}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, c := range children[next] {
			if seen[c.PID] {
				continue
			}
			seen[c.PID] = true
			sum += c.RSS
			queue = append(queue, c.PID)
		}
	}
	return sum
}

// memSize is a resident size as the hold says it: "2.5 GB" from 1 GiB, else
// "512 MB".
func memSize(b int64) string {
	if b >= 1<<30 {
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	}
	return fmt.Sprintf("%d MB", b>>20)
}

// memoryCulprits is what the memory hold names after its reason
// (culpritsText): read through Deps.MemProcs when the hold begins and again
// every culpritsEvery while it lasts, "" without a reader or when the read
// fails (logged).
func (e *Engine) memoryCulprits(ctx context.Context, now time.Time) string {
	if e.d.MemProcs == nil {
		return ""
	}
	if !e.culpritsAt.IsZero() && now.Sub(e.culpritsAt) < culpritsEvery {
		return e.culprits
	}
	e.culpritsAt = now
	procs, err := e.d.MemProcs(ctx)
	if err != nil {
		e.log.Info("the memory hold names no process", "err", err)
		e.culprits = ""
		return ""
	}
	e.culprits = culpritsText(procs)
	return e.culprits
}
