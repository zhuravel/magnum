package execx

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Process is one row of the process table (ProcessTable): its id, its
// parent's, its process group's and its executable's base name (ps's comm,
// never its arguments).
type Process struct {
	PID  int    `json:"pid"`
	PPID int    `json:"-"`
	PGID int    `json:"pgid"`
	Name string `json:"name"`
}

// processTableTimeout bounds one ps of the whole table.
const processTableTimeout = 10 * time.Second

// ProcessTable reads the process table through r (`ps -A -o
// pid=,ppid=,pgid=,comm=`, on macOS and Linux alike). A line it cannot
// read is skipped.
func ProcessTable(ctx context.Context, r Runner) ([]Process, error) {
	res, err := r.Run(ctx, Cmd{Name: "ps", Args: []string{"-A", "-o", "pid=,ppid=,pgid=,comm="},
		Timeout: processTableTimeout, Label: "ps process table", Quiet: true})
	if err != nil {
		return nil, fmt.Errorf("read the process table: %w", err)
	}
	var out []Process
	for line := range strings.SplitSeq(string(res.Stdout), "\n") {
		// comm is the last column and may hold spaces (an app bundle's path).
		m := psRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pid, _ := strconv.Atoi(m[1])
		ppid, _ := strconv.Atoi(m[2])
		pgid, _ := strconv.Atoi(m[3])
		if pid <= 0 {
			continue
		}
		out = append(out, Process{PID: pid, PPID: ppid, PGID: pgid, Name: filepath.Base(strings.TrimSpace(m[4]))})
	}
	return out, nil
}

// psRow is one line of ProcessTable's ps: pid, ppid, pgid and comm.
var psRow = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+(\d+)\s+(\S.*)$`)

// Descendants are the processes of table under pid: its children, theirs
// and so on, parents before their children; pid itself is not one. A
// process that left pid's process group (a sandbox's child in a session of
// its own) is still one while its parent is.
func Descendants(table []Process, pid int) []Process {
	children := map[int][]Process{}
	for _, p := range table {
		if p.PID != p.PPID {
			children[p.PPID] = append(children[p.PPID], p)
		}
	}
	var out []Process
	seen := map[int]bool{pid: true}
	queue := []int{pid}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, c := range children[next] {
			if seen[c.PID] {
				continue
			}
			seen[c.PID] = true
			out = append(out, c)
			queue = append(queue, c.PID)
		}
	}
	return out
}
