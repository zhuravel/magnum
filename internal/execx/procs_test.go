package execx

import (
	"context"
	"slices"
	"testing"
)

// The process table is ps's pid, ppid, pgid, stat, lstart and comm, read in
// the C locale; a comm with spaces (an app bundle's path) keeps them, and
// the name is its base. The tree under a process holds a child that left
// its process group, and nothing under another parent.
func TestProcessTableAndTheTreeUnderAProcess(t *testing.T) {
	f := &Fake{Rules: []Rule{{Prefix: []string{"ps", "-A", "-o", "pid=,ppid=,pgid=,stat=,lstart=,comm="}, Result: Result{Stdout: []byte(
		"    1     0     1 Ss   Tue Oct 27 03:25:51 2026     /sbin/launchd\n" +
			"  201   200   200 S+   Wed Oct 28 10:15:00 2026     /opt/magnum/bin/magnum\n" +
			"  202   201   200 S+   Wed Oct 28 10:15:01 2026     /usr/local/bin/codex\n" +
			"  300   202   300 Ss   Wed Oct 28 10:15:02 2026     /usr/bin/sandbox-exec\n" +
			"  301   300   300 Z    Wed Oct 28 10:15:03 2026     /bin/sh\n" +
			"  400     1   400 S    Wed Oct 28  9:01:00 2026     /Applications/Some App.app/Contents/MacOS/Some App\n" +
			"garbage line\n")}}}}
	table, err := ProcessTable(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Calls[0].Env["LC_ALL"]; got != "C" {
		t.Errorf("ps ran with LC_ALL=%q, want C (lstart's words)", got)
	}
	want := Process{PID: 400, PPID: 1, PGID: 400, Start: "Wed Oct 28 9:01:00 2026", Name: "Some App"}
	if len(table) != 6 || table[5] != want || !table[4].Zombie || table[3].Zombie {
		t.Fatalf("table = %+v", table)
	}
	var pids []int
	for _, p := range Descendants(table, 201) {
		pids = append(pids, p.PID)
	}
	if !slices.Equal(pids, []int{202, 300, 301}) {
		t.Errorf("the tree under 201 = %v, want 202, 300 and 301", pids)
	}
	if got := Descendants(table, 999); len(got) != 0 {
		t.Errorf("the tree under a process the table lacks = %+v", got)
	}
}

// A process is the one captured only with the same id, group and start: a
// later process that got its id (another start), one that moved to another
// group, one without a start and a zombie are not it.
func TestAProcessIsItsIDGroupAndStart(t *testing.T) {
	captured := Process{PID: 300, PGID: 300, Start: "Wed Oct 28 10:15:02 2026", Name: "sandbox-exec"}
	for name, tc := range map[string]struct {
		now  Process
		same bool
	}{
		"itself, reparented":    {Process{PID: 300, PPID: 1, PGID: 300, Start: "Wed Oct 28 10:15:02 2026", Name: "sandbox-exec"}, true},
		"its id, a later start": {Process{PID: 300, PGID: 300, Start: "Wed Oct 28 10:15:07 2026", Name: "sandbox-exec"}, false},
		"another group":         {Process{PID: 300, PGID: 301, Start: "Wed Oct 28 10:15:02 2026", Name: "sandbox-exec"}, false},
		"no start":              {Process{PID: 300, PGID: 300, Name: "sandbox-exec"}, false},
		"a zombie":              {Process{PID: 300, PGID: 300, Start: "Wed Oct 28 10:15:02 2026", Zombie: true}, false},
	} {
		if _, ok := Find([]Process{tc.now}, captured); ok != tc.same {
			t.Errorf("%s: Find = %t, want %t", name, ok, tc.same)
		}
	}
	if (Process{PID: 300, PGID: 300}).Same(Process{PID: 300, PGID: 300}) {
		t.Error("two processes without a start are the same")
	}
}
