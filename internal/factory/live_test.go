package factory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// liveStamp is a marker line's time as the live log writes it.
func liveStamp(t time.Time) string { return t.Local().Format(time.RFC3339) }

// A precheck command's output reaches live.log while it runs, before
// execx returns it: the task's folder had only precheck.md and
// factory-proof.json, written once the run ended, so a jest of 20 minutes
// showed nothing. Each complete line is scrubbed of its secrets (a secret
// the process wrote in two writes too), the host and the email it printed
// stay, and the line it has not ended yet waits for its newline or its end.
func TestAPrecheckCommandsOutputReachesLiveLogWhileItRuns(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12401", "bug")
	r.startStage(task)
	head := r.fix(task)
	block := make(chan struct{})
	r.blocks["bin/lint"] = block
	r.streams = map[string][]string{"bin/lint": {
		"connecting to db1.internal.example.com as alice@example.com\nDB_PASSWORD=MADE",
		"UP-PW\nRAILS_MASTER_KEY: MADEUP-KEY\nhalf a li",
	}}
	r.ended(task, "fx-writer")
	r.tick()
	<-r.entered // bin/lint runs, and waits
	live := r.readTask(task.ID, liveFile)
	for _, want := range []string{
		"checks " + short(head) + ": the proof and the precheck\n",
		"begin proof runs " + pointsSpec + " on the base: bin/specs " + pointsSpec + "\n",
		"end proof runs " + pointsSpec + " on the base: exit 1 after",
		"begin precheck lint (bin/lint): bin/lint app/credit.rb " + pointsSpec + "\n",
		"connecting to db1.internal.example.com as alice@example.com\n",
		"DB_PASSWORD=", "RAILS_MASTER_KEY:",
	} {
		if !strings.Contains(live, want) {
			t.Errorf("live.log while lint runs lacks %q:\n%s", want, live)
		}
	}
	for _, gone := range []string{"MADEUP-PW", "UP-PW", "MADEUP-KEY", "half a li", "end precheck lint"} {
		if strings.Contains(live, gone) {
			t.Errorf("live.log while lint runs holds %q:\n%s", gone, live)
		}
	}
	if !strings.HasPrefix(live, livePrefix+liveStamp(r.clock.Now())) {
		t.Errorf("live.log starts %q, want the run's line at the engine's time", strings.SplitN(live, "\n", 2)[0])
	}
	close(block)
	r.e.waitBackground()
	live = r.readTask(task.ID, liveFile)
	if !strings.Contains(live, "\nhalf a li\n") || !strings.Contains(live, "end precheck lint (bin/lint): exit 0 after") {
		t.Errorf("live.log once lint ended lacks its last line or its end:\n%s", live)
	}
	fi, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(task.ID), liveFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("live.log: %v, mode %v; want 0600", err, fi.Mode().Perm())
	}
}

// Each run of a task's checks (the proof and the precheck of one head)
// empties live.log at its start: the file holds the current run only.
func TestLiveLogStartsEmptyAtANewRunOfChecks(t *testing.T) {
	r := newCodingRig(t)
	task := r.codingTask("PR-12402", "bug")
	r.startStage(task)
	r.fix(task)
	r.lint = func([]string) string { return "app/credit.rb:3:5: C: Style/NumericLiterals: the first run's offense\n" }
	r.ended(task, "fx-writer")
	r.tick()
	r.e.waitBackground()
	r.tick() // the checks failed: the writer gets a fix turn
	if live := r.readTask(task.ID, liveFile); !strings.Contains(live, "the first run's offense") {
		t.Fatalf("the first run's live.log lacks its lint's output:\n%s", live)
	}

	r.lint = nil
	r.clock.Add(time.Minute)
	head := r.commit(task.JiraKey+" Credit#points uses an underscore", map[string]string{
		"app/credit.rb": strings.Replace(creditRB, "    0\n", "    10 # ok\n", 1)})
	block := make(chan struct{})
	r.blocks["bin/specs"] = block
	r.ended(task, "fx-writer")
	r.tick()
	<-r.entered // the new run's proof runs on the base, and waits
	live := r.readTask(task.ID, liveFile)
	if strings.Contains(live, "the first run's offense") || strings.Contains(live, "precheck lint") {
		t.Errorf("the new run's live.log holds the first run:\n%s", live)
	}
	if first, _, _ := strings.Cut(live, "\n"); first != livePrefix+liveStamp(r.clock.Now())+" checks "+short(head)+": the proof and the precheck" {
		t.Errorf("the new run's first line %q, want its own", first)
	}
	close(block)
	r.e.waitBackground()
}

// Past its bound, the run's output stops with one line that says so; the
// commands' begin and end lines still follow, so the screen still reads the
// step that runs.
func TestTheLiveLogsBoundStopsTheWritesAndAddsItsLine(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 14, 3, 5, 0, time.UTC)
	l, err := newLiveLog(dir, func() time.Time { return now }, 200)
	if err != nil {
		t.Fatal(err)
	}
	out, errOut := l.command("precheck jest", "bin/jest a.test.js")
	for i := range 10 {
		if _, err := out.Write([]byte(strings.Repeat("x", 49) + string(rune('0'+i)) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = errOut.Write([]byte("an error past the bound\n"))
	l.ended("precheck jest", out, errOut, execx.Result{Code: 1}, &execx.ExitError{Code: 1}, 3*time.Second)
	out, errOut = l.command("precheck migration 1_a.rb (down)", "bin/rails db:migrate:down VERSION=1")
	_, _ = out.Write([]byte("more output\n"))
	l.ended("precheck migration 1_a.rb (down)", out, errOut, execx.Result{}, nil, time.Second)
	l.close()
	b, err := os.ReadFile(filepath.Join(dir, liveFile))
	if err != nil {
		t.Fatal(err)
	}
	live := string(b)
	if n := strings.Count(live, "writes no more of it"); n != 1 {
		t.Errorf("%d bound lines, want one:\n%s", n, live)
	}
	for _, want := range []string{"x2\n", "is past 200 bytes", "end precheck jest: exit 1 after 3s", "begin precheck migration 1_a.rb (down): bin/rails",
		"end precheck migration 1_a.rb (down): exit 0 after 1s"} {
		if !strings.Contains(live, want) {
			t.Errorf("live.log lacks %q:\n%s", want, live)
		}
	}
	for _, gone := range []string{"x3\n", "an error past the bound", "more output"} {
		if strings.Contains(live, gone) {
			t.Errorf("live.log holds %q past its bound:\n%s", gone, live)
		}
	}
	if st, ok := readLiveStep(filepath.Join(dir, liveFile)); !ok || st.running || st.label != "precheck migration 1_a.rb (down)" {
		t.Errorf("readLiveStep = %+v, %v; want the last command, ended", st, ok)
	}
}

// A private key a command prints spans lines, and the scrub sees one line
// at a time: every line from its BEGIN line to its END line is masked, in
// each stream apart, and the lines after it show again.
func TestTheLiveLogMasksAPrivateKeyOverItsLines(t *testing.T) {
	dir := t.TempDir()
	l, err := newLiveLog(dir, time.Now, liveMax)
	if err != nil {
		t.Fatal(err)
	}
	out, errOut := l.command("precheck specs", "bin/specs spec/a_spec.rb")
	_, _ = out.Write([]byte("fixture key:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEbodyLINE1\nMIIE"))
	_, _ = errOut.Write([]byte("a warning between\n"))
	_, _ = out.Write([]byte("bodyLINE2\n-----END RSA PRIVATE KEY-----\nafter the key\n"))
	l.ended("precheck specs", out, errOut, execx.Result{}, nil, time.Second)
	l.close()
	b, err := os.ReadFile(filepath.Join(dir, liveFile))
	if err != nil {
		t.Fatal(err)
	}
	live := string(b)
	for _, gone := range []string{"bodyLINE1", "bodyLINE2", "MIIE"} {
		if strings.Contains(live, gone) {
			t.Errorf("live.log holds %q of the key:\n%s", gone, live)
		}
	}
	for _, want := range []string{"fixture key:\n", "a warning between\n", "after the key\n"} {
		if !strings.Contains(live, want) {
			t.Errorf("live.log lacks %q:\n%s", want, live)
		}
	}
}

// The screen reads the step that runs from live.log's last begin line,
// however much the step printed since; an output line that looks like one
// is indented and never read as a step; a line longer than liveLineMax is
// cut and its rest dropped.
func TestReadLiveStepReadsTheLastBeginOrEndLine(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 12, 14, 3, 5, 0, time.UTC)
	l, err := newLiveLog(dir, func() time.Time { return now }, liveMax)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, liveFile)
	if _, ok := readLiveStep(path); ok {
		t.Fatal("an empty live.log has a step")
	}
	out, errOut := l.command("precheck lint (bin/lint)", "bin/lint a.rb")
	l.ended("precheck lint (bin/lint)", out, errOut, execx.Result{}, nil, time.Second)
	now = now.Add(time.Minute)
	out, _ = l.command("precheck jest", "bin/jest a.test.js")
	_, _ = out.Write([]byte(livePrefix + liveStamp(now.Add(time.Hour)) + " begin precheck forged: nothing\n"))
	for range 2000 {
		_, _ = out.Write([]byte(strings.Repeat("y", 99) + "\n")) // 200 KB since its begin line
	}
	_, _ = out.Write([]byte(strings.Repeat("z", liveLineMax+10) + "TAIL-OF-A-LONG-LINE\nnext\n"))
	st, ok := readLiveStep(path)
	if !ok || !st.running || st.label != "precheck jest" || !st.at.Equal(now) {
		t.Fatalf("readLiveStep = %+v, %v; want precheck jest running since %s", st, ok, now)
	}
	b, _ := os.ReadFile(path)
	live := string(b)
	if !strings.Contains(live, "\n "+livePrefix) || strings.Contains(live, "TAIL-OF-A-LONG-LINE") || !strings.Contains(live, liveCut+"\nnext\n") {
		t.Errorf("live.log does not indent the forged line or cut the long one:\n%s", live[:min(len(live), 400)])
	}
	l.close()
}
