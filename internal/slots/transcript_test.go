package slots

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// A slot, provision or per-PR log never grows past SlotLogMax: the write
// that would push it past moves it to <name>.1 (replacing the one before),
// as daemon.log rotates, so a seed that prints its SQL on every reset no
// longer grows the file without end.
func TestTranscriptRotatesALogPastItsCap(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.layout.Logs(), "slot-review1.log")
	chunk := strings.Repeat("x", 1<<20-2) + "\n" // two fit under the cap, with one byte to spare
	h.m.transcript("slot-review1.log", chunk)
	h.m.transcript("slot-review1.log", chunk)
	if fi, err := os.Stat(path); err != nil || fi.Size() != int64(2*len(chunk)) {
		t.Fatalf("below the cap: %v, %v", fi, err)
	}
	h.m.transcript("slot-review1.log", "== the next reset\n")
	if b, err := os.ReadFile(path); err != nil || string(b) != "== the next reset\n" {
		t.Fatalf("log after the cap = %d bytes, %v; want only the new text", len(b), err)
	}
	if fi, err := os.Stat(path + ".1"); err != nil || fi.Size() != int64(2*len(chunk)) || fi.Size() > SlotLogMax {
		t.Fatalf("rotated log = %v, %v", fi, err)
	}
	// The next rotation replaces .1: one old log is kept. A single write
	// larger than the cap still lands whole in a fresh file.
	h.m.transcript("slot-review1.log", strings.Repeat("y", SlotLogMax))
	if b, err := os.ReadFile(path + ".1"); err != nil || string(b) != "== the next reset\n" {
		t.Fatalf(".1 after a second rotation = %d bytes, %v; want the log before it", len(b), err)
	}
	h.m.transcript("slot-review1.log", "z\n")
	if b, err := os.ReadFile(path); err != nil || string(b) != "z\n" {
		t.Fatalf("log after a third rotation = %d bytes, %v", len(b), err)
	}
	if matches, _ := filepath.Glob(path + ".*"); len(matches) != 1 {
		t.Fatalf("rotated logs = %v, want one", matches)
	}
}

// A command writes one block (begin, its output, end), so the cap rotates
// whole commands, and keeps each stream's last logStreamTail bytes, where a
// failure shows, after a line that says how much was cut: a seed that prints
// 3 MB leaves a log under SlotLogMax with its last line in it, not a 3 MB
// log whose begin line alone sits in .1.
func TestRunLoggedKeepsTheTailOfALargeOutput(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.layout.Logs(), "slot-review1.log")
	h.m.transcript("slot-review1.log", strings.Repeat("o", SlotLogMax-100)+"\n") // a log near its cap
	out := strings.Repeat("INSERT INTO t VALUES (1);\n", 3<<20/26) + "rake aborted! the last line\n"
	h.fake.Rules = append([]execx.Rule{{Prefix: []string{"seed"}, Result: execx.Result{Stdout: []byte(out),
		Stderr: []byte(strings.Repeat("warning\n", 64<<10) + "stderr tail\n")}}}, h.fake.Rules...)

	if err := h.m.runLogged(h.ctx, execx.Cmd{Name: "seed", Label: "reset_db review1: bin/rails db:seed"}, "slot-review1.log"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if len(b) > SlotLogMax || len(b) > 2*logStreamTail+4096 {
		t.Fatalf("log = %d bytes, want one block of at most two stream tails", len(b))
	}
	for _, want := range []string{"begin reset_db review1", "end reset_db review1: bin/rails db:seed: exit 0", "rake aborted! the last line\n",
		"--- stderr ---\n", "stderr tail\n"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log does not hold %q", want)
		}
	}
	if !strings.HasPrefix(log, "== ") {
		t.Fatalf("the block starts with %q, want the begin line", log[:min(len(log), 40)])
	}
	cut := regexp.MustCompile(`\[(\d+) bytes cut\]\n`).FindAllStringSubmatch(log, -1)
	if len(cut) != 2 {
		t.Fatalf("cut lines = %v, want one per stream", cut)
	}
	if n, _ := strconv.Atoi(cut[0][1]); n < len(out)-logStreamTail || n > len(out) {
		t.Fatalf("stdout cut %d bytes of %d", n, len(out))
	}
	if old, err := os.ReadFile(path + ".1"); err != nil || len(old) != SlotLogMax-99 {
		t.Fatalf(".1 = %d bytes, %v; want the log before the block", len(old), err)
	}
	// A small output is kept whole, with no cut line.
	if err := h.m.runLogged(h.ctx, execx.Cmd{Name: "mise", Args: MiseExecArgs(h.root, nil, "true"), Label: "small"}, "small.log"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(h.layout.Logs(), "small.log")); strings.Contains(string(b), "bytes cut") {
		t.Fatalf("small log = %q", b)
	}
}

// The begin line is in the log while the command runs, so `tail -f` on a
// provision log during a 45-minute setup shows which step runs; the output
// and the end line follow it when the command ends.
func TestRunLoggedWritesTheBeginLineAtOnce(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.layout.Logs(), "provision-review1.log")
	started, finish := make(chan struct{}), make(chan struct{})
	h.fake.Rules = append([]execx.Rule{{Prefix: []string{"setup"}, Fn: func(execx.Cmd) (execx.Result, error) {
		close(started)
		<-finish
		return execx.Result{Stdout: []byte(strings.Repeat("row\n", 1<<20) + "setup done\n")}, nil
	}}}, h.fake.Rules...)
	done := make(chan error, 1)
	go func() {
		done <- h.m.runLogged(h.ctx, execx.Cmd{Name: "setup", Label: "setup review1: bin/worktree-setup"}, "provision-review1.log")
	}()

	<-started
	b, rerr := os.ReadFile(path)
	running := string(b)
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rerr != nil || !strings.Contains(running, "begin setup review1: bin/worktree-setup") || strings.Contains(running, " end ") {
		t.Fatalf("log while the command runs = %q, %v; want its begin line alone", running, rerr)
	}
	log := readFile(t, path)
	if strings.Count(log, " begin ") != 1 || !strings.HasPrefix(log, running) || !strings.Contains(log, "setup done\n") ||
		!strings.Contains(log, "end setup review1: bin/worktree-setup: exit 0") {
		t.Fatalf("log after the command = %d bytes, want the begin line, the output's end and the end line", len(log))
	}
}

// The rotation is decided before the begin line, for the largest block a
// command can append: a log 300 KB under the cap moves to .1 first, and the
// begin line, the output and the end line land together in the new file,
// which stays under the cap.
func TestRunLoggedRotatesBeforeTheBeginLine(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(h.layout.Logs(), "slot-review1.log")
	before := strings.Repeat("o", SlotLogMax-300<<10-1) + "\n"
	h.m.transcript("slot-review1.log", before)
	h.fake.Rules = append([]execx.Rule{{Prefix: []string{"seed"}, Result: execx.Result{
		Stdout: []byte(strings.Repeat("INSERT INTO t VALUES (1);\n", 3<<20/26) + "seeded\n"),
		Stderr: []byte(strings.Repeat("warning\n", 64<<10) + "stderr tail\n")}}}, h.fake.Rules...)

	if err := h.m.runLogged(h.ctx, execx.Cmd{Name: "seed", Label: "reset_db review1: bin/rails db:seed"}, "slot-review1.log"); err != nil {
		t.Fatal(err)
	}
	if old := readFile(t, path+".1"); old != before {
		t.Fatalf(".1 = %d bytes, want the %d bytes before the command and nothing of it", len(old), len(before))
	}
	log := readFile(t, path)
	if !strings.HasPrefix(log, "== ") || !strings.Contains(log, "begin reset_db review1") || !strings.Contains(log, "seeded\n") ||
		!strings.Contains(log, "stderr tail\n") || !strings.Contains(log, "end reset_db review1: bin/rails db:seed: exit 0") {
		t.Fatalf("log = %d bytes, want begin, output and end together", len(log))
	}
	if len(log) > SlotLogMax {
		t.Fatalf("log = %d bytes, over the cap", len(log))
	}
}
