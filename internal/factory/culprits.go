package factory

// The memory hold's culprits (B #13 of the 2026-11-07 audit): the hold's
// reason names the largest resident processes and every Codex app-server
// broker with its age, so you see what holds the memory without looking for
// it. It reads the process table and kills nothing: what to stop is yours
// to decide.

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
	// (node .../app-server-broker.mjs), which keeps MCP servers alive.
	brokerMark = "app-server-broker"
	// brokerOrphanAge is the age from which a broker under launchd may be
	// orphaned. The plugin starts each broker detached, so a live Claude
	// session's broker is under launchd too, and stops it at the session's
	// end: only an old one may have outlived its session.
	brokerOrphanAge = 24 * time.Hour
)

// MemProc is a process as the memory hold names it: its id, its parent's,
// its resident memory in bytes, its executable's base name (never its
// arguments), whether it is a Codex app-server broker and how long it has
// run.
type MemProc struct {
	PID, PPID int
	RSS       int64
	Name      string
	Broker    bool
	Age       time.Duration
}

// ReadMemProcs reads the process table through r for the memory hold: `ps
// -A -o pid=,ppid=,rss=,etime=,comm=` for the ids, the resident memory, the
// ages and the names (each through execx.Redact), then `ps -A -o pid=,args=`
// to tell the brokers, both in the C locale. The arguments stay here: only
// whether they name a broker leaves. It runs nothing else. A line it cannot
// read is skipped.
func ReadMemProcs(ctx context.Context, r execx.Runner) ([]MemProc, error) {
	ps := func(cols string) (string, error) {
		res, err := r.Run(ctx, execx.Cmd{Name: "ps", Args: []string{"-A", "-o", cols}, Env: map[string]string{"LC_ALL": "C"},
			Timeout: memPsTimeout, Label: "ps for the memory hold", Quiet: true})
		if err != nil {
			return "", fmt.Errorf("read the process table: %w", err)
		}
		return string(res.Stdout), nil
	}
	table, err := ps("pid=,ppid=,rss=,etime=,comm=")
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
		out = append(out, MemProc{PID: pid, PPID: ppid, RSS: kb << 10, Age: parseEtime(m[4]),
			Name: execx.Redact(filepath.Base(strings.TrimSpace(m[5]))), Broker: brokers[pid]})
	}
	return out, nil
}

// psMemRow is a line of the table read: pid, ppid, rss (KiB), etime and
// comm, the last column, which may hold spaces (an app bundle's path).
// psArgsRow is a line of the arguments read: pid and the arguments.
var (
	psMemRow  = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+(\d+)\s+(\S+)\s+(\S.*)$`)
	psArgsRow = regexp.MustCompile(`^\s*(\d+)\s+(.*)$`)
)

// parseEtime is ps's elapsed time, [[dd-]hh:]mm:ss, as a duration; 0 for
// one it cannot read.
func parseEtime(s string) time.Duration {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	var secs int
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0
		}
		secs = secs*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(secs)*time.Second
}

// culpritsText is what the memory hold adds to its reason from the process
// table procs: the culpritsLargest largest resident processes, then every
// Codex app-server broker with its age, oldest first, the one under launchd
// (pid 1) past brokerOrphanAge "may be orphaned", and the memory of their
// trees, e.g. "; largest: codex 2.5 GB (pid 90), ...; Codex app-server
// brokers, oldest first: pid 455 (3d2h, may be orphaned), pid 457 (2h),
// 2.3 GB with their children". "" for an empty table.
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
	var brokers []string
	var held int64
	for _, p := range slices.SortedFunc(slices.Values(procs), func(a, b MemProc) int {
		return cmp.Or(cmp.Compare(b.Age, a.Age), cmp.Compare(a.PID, b.PID))
	}) {
		if !p.Broker {
			continue
		}
		age := brokerAge(p.Age)
		if p.PPID == 1 && p.Age >= brokerOrphanAge {
			age += ", may be orphaned"
		}
		brokers = append(brokers, fmt.Sprintf("pid %d (%s)", p.PID, age))
		held += treeRSS(p, children)
	}
	switch n := len(brokers); {
	case n == 0:
		return text
	case n == 1:
		return text + fmt.Sprintf("; Codex app-server broker: %s, %s with its children", brokers[0], memSize(held))
	case n > culpritsBrokers:
		brokers = append(brokers[:culpritsBrokers], fmt.Sprintf("and %d more", n-culpritsBrokers))
	}
	return text + fmt.Sprintf("; Codex app-server brokers, oldest first: %s, %s with their children", strings.Join(brokers, ", "), memSize(held))
}

// brokerAge is a broker's age as the hold says it: "3d2h", "2h", "45m".
func brokerAge(d time.Duration) string {
	days, hours := int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour)
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
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
