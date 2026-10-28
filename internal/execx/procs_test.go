package execx

import (
	"context"
	"slices"
	"testing"
)

// The process table is ps's pid, ppid, pgid and comm; a comm with spaces
// (an app bundle's path) keeps them, and the name is its base. The tree
// under a process holds a child that left its process group, and nothing
// under another parent.
func TestProcessTableAndTheTreeUnderAProcess(t *testing.T) {
	f := &Fake{Rules: []Rule{{Prefix: []string{"ps", "-A", "-o", "pid=,ppid=,pgid=,comm="}, Result: Result{Stdout: []byte(
		"    1     0     1 /sbin/launchd\n" +
			"  201   200   200 /opt/magnum/bin/magnum\n" +
			"  202   201   200 /usr/local/bin/codex\n" +
			"  300   202   300 /usr/bin/sandbox-exec\n" +
			"  301   300   300 /bin/sh\n" +
			"  400     1   400 /Applications/Some App.app/Contents/MacOS/Some App\n" +
			"garbage line\n")}}}}
	table, err := ProcessTable(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 6 || table[5] != (Process{PID: 400, PPID: 1, PGID: 400, Name: "Some App"}) {
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
