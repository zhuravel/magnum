package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/store"
)

// syncBuf is a goroutine-safe buffer for follow tests.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func inspAppendEvent(t *testing.T, st *store.Store, subject, kind, msg string) {
	t.Helper()
	ev := store.Event{Kind: kind, Message: msg}
	if subject != "" {
		ev.Subject = &subject
	}
	if _, err := st.AppendEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

func logsFixture(t *testing.T) (*inspFixture, *store.Store, store.PR) {
	t.Helper()
	f := newInspFixture(t)
	st := f.store()
	_, pr := inspSeedPR(t, st, "talkable/talkable", 11920, store.PRReviewed, nil)
	sl := inspSeedSlot(t, st, f.Home, "review1", store.SlotFree, nil)
	if _, err := st.OpenAssignment(context.Background(), store.Assignment{PRID: pr.ID, SlotID: sl.ID, Path: sl.Path}); err != nil {
		t.Fatal(err)
	}
	inspAppendEvent(t, st, "pr:talkable/talkable#11920", "round.start", "round 1 started")
	inspAppendEvent(t, st, "pr:"+strconv.FormatInt(pr.ID, 10), "cleanup.pr_released", "released by cleanup")
	inspAppendEvent(t, st, "slot:review1:pr:11920:abcdef0", "slot.head_moved", "checkout moved")
	inspAppendEvent(t, st, "slot:review1", "slot.provisioned", "review1 provisioned")
	inspAppendEvent(t, st, "slot:review10", "slot.provisioned", "review10 provisioned")
	inspAppendEvent(t, st, "slot:review1:release", "cleanup.release", "review1 released")
	inspAppendEvent(t, st, "request:7", "request.done", "queued talkable#11920")
	inspAppendEvent(t, st, "pr:talkable/talkable#119", "round.start", "other PR")
	return f, st, pr
}

func TestLogsSubjects(t *testing.T) {
	f, st, pr := logsFixture(t)
	ctx := context.Background()
	refs := app.RefParser{DefaultRepo: "talkable/talkable"}
	m, label, err := logsMatchers(ctx, st, refs, "11920")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := st.EventsMatching(ctx, m, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, e := range evs {
		msgs = append(msgs, e.Message)
	}
	got := strings.Join(msgs, "|")
	if got != "round 1 started|released by cleanup|checkout moved" || label != "talkable/talkable#11920" {
		t.Fatalf("pr events = %q label %q", got, label)
	}
	_ = pr
	m, _, _ = logsMatchers(ctx, st, refs, "review1")
	evs, _ = st.EventsMatching(ctx, m, 0, 0)
	msgs = nil
	for _, e := range evs {
		msgs = append(msgs, e.Message)
	}
	if got := strings.Join(msgs, "|"); got != "checkout moved|review1 provisioned|review1 released" {
		t.Fatalf("slot events = %q", got)
	}
	m, _, _ = logsMatchers(ctx, st, refs, "request:7")
	evs, _ = st.EventsMatching(ctx, m, 0, 0)
	if len(evs) != 1 || evs[0].Message != "queued talkable#11920" {
		t.Fatalf("raw subject events = %+v", evs)
	}
	// limit keeps the newest rows, oldest first.
	m, _, _ = logsMatchers(ctx, st, refs, "review1")
	evs, _ = st.EventsMatching(ctx, m, 0, 2)
	if len(evs) != 2 || evs[1].Message != "review1 released" {
		t.Fatalf("limited = %+v", evs)
	}
	if _, _, err := logsMatchers(ctx, st, refs, "what?"); err == nil {
		t.Fatal("bad ref must fail")
	}
	_ = f
}

func TestLogsCommandEvents(t *testing.T) {
	f, st, _ := logsFixture(t)
	st.Close()
	if code := f.run("logs", "review1"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	out := f.Out.String()
	if !strings.Contains(out, "slot:review1:release") || !strings.Contains(out, "review1 released") || strings.Contains(out, "review10") {
		t.Fatalf("out:\n%s", out)
	}
	if code := f.run("logs", "review1", "--json"); code != 0 || !strings.Contains(f.Out.String(), `"kind":"slot.provisioned"`) {
		t.Fatalf("json code %d out %s", code, f.Out.String())
	}
	if code := f.run("logs", "a", "b"); code != 2 {
		t.Fatalf("two args code %d", code)
	}
}

// logsWaitFor polls cond for up to 2s.
func logsWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestLogsFollowEvents(t *testing.T) {
	_, st, _ := logsFixture(t) // the fixture's slot:review1 events are history
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := []store.SubjectMatch{logsBelow("slot:review1")}
	_, cursor, err := logsBacklog(ctx, st, m, 0, true)
	if err != nil || cursor == 0 {
		t.Fatalf("cursor %d err %v", cursor, err)
	}
	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- logsFollowEvents(ctx, &out, st, m, cursor, false) }()
	// Let the follower go round a few empty polls: the event below can only
	// reach the output through the poll loop, never through a backlog.
	time.Sleep(6 * inspPoll)
	if got := out.String(); got != "" {
		t.Fatalf("history was printed:\n%s", got)
	}
	inspAppendEvent(t, st, "slot:review1:remove", "cleanup.remove_slot", "review1 removed")
	inspAppendEvent(t, st, "slot:review2", "slot.provisioned", "another slot")
	logsWaitFor(t, "the new event", func() bool { return strings.Contains(out.String(), "review1 removed") })
	time.Sleep(6 * inspPoll) // more polls must not print it again
	cancel()
	<-done
	got := out.String()
	if n := strings.Count(got, "review1 removed"); n != 1 || strings.Contains(got, "another slot") || strings.Contains(got, "review1 provisioned") {
		t.Fatalf("follow printed:\n%s", got)
	}
}

func TestLogsBacklogCursorIsItsNewestEvent(t *testing.T) {
	_, st, _ := logsFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := []store.SubjectMatch{logsBelow("slot:review1")}
	evs, cursor, err := logsBacklog(ctx, st, m, 2, true)
	if err != nil || len(evs) != 2 || cursor != evs[1].ID || evs[1].Message != "review1 released" {
		t.Fatalf("backlog %+v cursor %d err %v", evs, cursor, err)
	}
	// Another subject's newer event moves nothing; the follower prints only
	// what lands after the backlog, once.
	inspAppendEvent(t, st, "slot:review2", "slot.provisioned", "another slot")
	inspAppendEvent(t, st, "slot:review1:remove", "cleanup.remove_slot", "review1 removed")
	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- logsFollowEvents(ctx, &out, st, m, cursor, false) }()
	logsWaitFor(t, "review1 removed", func() bool { return strings.Contains(out.String(), "review1 removed") })
	time.Sleep(6 * inspPoll)
	cancel()
	<-done
	got := out.String()
	if strings.Count(got, "review1 removed") != 1 || strings.Contains(got, "review1 released") || strings.Contains(got, "another slot") {
		t.Fatalf("follow printed:\n%s", got)
	}

	// Without -f there is no cursor.
	if _, cursor, err := logsBacklog(context.Background(), st, m, 50, false); err != nil || cursor != 0 {
		t.Fatalf("no follow: cursor %d err %v", cursor, err)
	}
}

func TestLogsBacklogWithoutAnyEvents(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	ctx := context.Background()
	// No matching event yet: the cursor is 0, so the follower prints the
	// first one that lands, which the backlog never showed.
	m := []store.SubjectMatch{logsBelow("slot:review1")}
	evs, cursor, err := logsBacklog(ctx, st, m, 10, true)
	if err != nil || len(evs) != 0 || cursor != 0 {
		t.Fatalf("backlog %+v cursor %d err %v", evs, cursor, err)
	}
	inspAppendEvent(t, st, "slot:review1", "slot.provisioned", "first")
	after, err := st.EventsMatching(ctx, m, cursor, 0)
	if err != nil || len(after) != 1 || after[0].Message != "first" {
		t.Fatalf("follower would see %+v err %v", after, err)
	}
}

func TestLogsCleansUntrustedText(t *testing.T) {
	ev := store.Event{At: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), Level: "warn", Kind: store.KindStep,
		Subject: new("slot:r1" + statusNoise), Step: new("step" + statusNoise), Message: "msg " + statusNoise}
	var b bytes.Buffer
	logsPrintEvent(&b, ev, false)
	line := strings.TrimSuffix(b.String(), "\n")
	statusNoControls(t, "logsPrintEvent", line)
	if !strings.Contains(line, "msg "+actClean(statusNoise)) {
		t.Fatalf("message lost: %q", line)
	}
	// --json keeps the raw text.
	b.Reset()
	logsPrintEvent(&b, ev, true)
	var back store.Event
	if err := json.Unmarshal(b.Bytes(), &back); err != nil || back.Message != "msg "+statusNoise {
		t.Fatalf("json %q err %v", b.String(), err)
	}

	// Daemon log lines: JSON decoding brings a real ESC back.
	jsonLine := `{"time":"2026-10-03T12:00:00Z","level":"WARN","msg":"pane\u001b[31m","err":"x\u001b]0;pwned\u0007y"}`
	statusNoControls(t, "logsLine json", logsLine(jsonLine, false))
	statusNoControls(t, "logsLine text", logsLine("plain \x1b[31mred\x1b[0m\r", false))
	if got := logsLine(jsonLine, true); got != jsonLine {
		t.Fatalf("--raw changed the line: %q", got)
	}
}

func TestLogsDaemonLog(t *testing.T) {
	f := newInspFixture(t)
	if code := f.run("logs"); code != 1 || !strings.Contains(f.Err.String(), "no daemon log") {
		t.Fatalf("missing log: code %d err %s", code, f.Err.String())
	}
	if err := f.Ctx.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"time":"2026-10-03T12:00:00.000+02:00","level":"INFO","msg":"tick","prs":3}`,
		`not json at all`,
		`{"time":"2026-10-03T12:00:01.000+02:00","level":"WARN","msg":"herdr down","err":"dial unix: no such file"}`,
	}
	if err := os.WriteFile(f.Ctx.Layout.DaemonLog(), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := f.run("logs", "-n", "2"); code != 0 {
		t.Fatalf("code %d err %s", code, f.Err.String())
	}
	out := f.Out.String()
	if strings.Contains(out, "tick") || !strings.Contains(out, "not json at all") ||
		!strings.Contains(out, "WARN  herdr down err=\"dial unix: no such file\"") {
		t.Fatalf("out:\n%s", out)
	}
	if code := f.run("logs", "--raw", "-n", "1"); code != 0 || !strings.HasPrefix(f.Out.String(), `{"time"`) {
		t.Fatalf("raw: %s", f.Out.String())
	}
}

func logsWriteFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func logsAppendFile(t *testing.T, path, text string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// logsFollowForTest starts logsFollowFile from pos and returns its output and a stop
// function that cancels it and waits.
func logsFollowForTest(t *testing.T, path string, pos logsFilePos) (*syncBuf, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuf{}
	done := make(chan error, 1)
	go func() { done <- logsFollowFile(ctx, out, path, pos, true) }()
	return out, func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("follower: %v", err)
		}
	}
}

func TestLogsFollowFile(t *testing.T) {
	path := t.TempDir() + "/daemon.log"
	logsWriteFile(t, path, "old\n")
	prevPoll := inspPoll
	inspPoll = 5 * time.Millisecond
	defer func() { inspPoll = prevPoll }()
	lines, pos, err := logsOpenTail(path, 50)
	if err != nil || len(lines) != 1 || lines[0] != "old" || pos.Offset != 4 || pos.Info == nil {
		t.Fatalf("tail = %q %+v err %v", lines, pos, err)
	}
	out, stop := logsFollowForTest(t, path, pos)
	logsAppendFile(t, path, "new line\npart")
	logsWaitFor(t, "new line", func() bool { return strings.Contains(out.String(), "new line") })
	logsAppendFile(t, path, "ial\n") // the unfinished line completes
	logsWaitFor(t, "partial", func() bool { return strings.Contains(out.String(), "partial") })
	stop()
	if got := out.String(); got != "new line\npartial\n" {
		t.Fatalf("followed %q", got)
	}
}

func TestLogsFollowFileWaitsForTheLog(t *testing.T) {
	path := t.TempDir() + "/daemon.log"
	prevPoll := inspPoll
	inspPoll = 5 * time.Millisecond
	defer func() { inspPoll = prevPoll }()
	if _, _, err := logsOpenTail(path, 5); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing log: %v", err)
	}
	out, stop := logsFollowForTest(t, path, logsFilePos{})
	time.Sleep(4 * inspPoll)
	logsWriteFile(t, path, "first\n")
	logsWaitFor(t, "first", func() bool { return strings.Contains(out.String(), "first") })
	stop()
	if got := out.String(); got != "first\n" {
		t.Fatalf("followed %q", got)
	}
}

// A log replaced by a longer file is a new file even though it is longer than
// the offset: only the identity check (os.SameFile) sees it. The old file's
// unfinished line is dropped, not glued to the first line of the new one.
func TestLogsFollowFileRotatedToALongerFile(t *testing.T) {
	path := t.TempDir() + "/daemon.log"
	logsWriteFile(t, path, "old line\nsecond old line\n")
	prevPoll := inspPoll
	inspPoll = 5 * time.Millisecond
	defer func() { inspPoll = prevPoll }()
	_, pos, err := logsOpenTail(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	out, stop := logsFollowForTest(t, path, pos)
	logsAppendFile(t, path, "new line\npart") // an unfinished line is pending
	logsWaitFor(t, "new line", func() bool { return strings.Contains(out.String(), "new line") })

	fresh := "rotated one\nrotated two\nrotated three that is long enough\n"
	if int64(len(fresh)) <= pos.Offset+int64(len("new line\npart")) {
		t.Fatal("the rotated file must be longer than the old offset")
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	logsWriteFile(t, path, fresh)
	logsWaitFor(t, "rotated three", func() bool { return strings.Contains(out.String(), "rotated three") })
	stop()
	want := "new line\n" + fresh
	if got := out.String(); got != want {
		t.Fatalf("followed %q\nwant     %q", got, want)
	}
}

func TestLogsFollowFileTruncatedInPlace(t *testing.T) {
	path := t.TempDir() + "/daemon.log"
	logsWriteFile(t, path, "a long first line\n")
	prevPoll := inspPoll
	inspPoll = 5 * time.Millisecond
	defer func() { inspPoll = prevPoll }()
	_, pos, err := logsOpenTail(path, 50)
	if err != nil {
		t.Fatal(err)
	}
	out, stop := logsFollowForTest(t, path, pos)
	time.Sleep(4 * inspPoll)
	logsWriteFile(t, path, "short\n") // same inode, now smaller than the offset
	logsWaitFor(t, "short", func() bool { return strings.Contains(out.String(), "short") })
	stop()
	if got := out.String(); got != "short\n" {
		t.Fatalf("followed %q", got)
	}
}

// The follow position is the end of the bytes the tail read from its own
// handle: a line appended after the handle was opened but before the tail
// ran is shown by the tail and not again by the follower.
func TestLogsTailPositionIsFromTheSameHandle(t *testing.T) {
	path := t.TempDir() + "/daemon.log"
	logsWriteFile(t, path, "a\nb\n")
	prevPoll := inspPoll
	inspPoll = 5 * time.Millisecond
	defer func() { inspPoll = prevPoll }()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	logsAppendFile(t, path, "c\n")
	lines, pos, err := logsTail(f, 50)
	if err != nil || strings.Join(lines, "|") != "a|b|c" || pos.Offset != 6 {
		t.Fatalf("tail = %q %+v err %v", lines, pos, err)
	}
	out, stop := logsFollowForTest(t, path, pos)
	logsAppendFile(t, path, "d\n")
	logsWaitFor(t, "d", func() bool { return strings.Contains(out.String(), "d") })
	time.Sleep(4 * inspPoll)
	stop()
	if got := out.String(); got != "d\n" {
		t.Fatalf("followed %q", got)
	}
}

func TestLogsTail(t *testing.T) {
	numbered := func(n int) string {
		var b strings.Builder
		for i := 1; i <= n; i++ {
			fmt.Fprintf(&b, "line-%06d\n", i)
		}
		return b.String()
	}
	long := strings.Repeat("x", logsTailChunk+logsTailChunk/2)
	cases := []struct {
		name, content string
		n             int
		want          string // lines joined by |
		pending       string
	}{
		{"empty", "", 5, "", ""},
		{"fewer than n", "a\nb\nc\n", 5, "a|b|c", ""},
		{"last two", "a\nb\nc\n", 2, "b|c", ""},
		{"all with n=0", "a\nb\nc\n", 0, "a|b|c", ""},
		{"unfinished last line", "a\nb\npar", 5, "a|b", "par"},
		{"no newline at all", "abc", 5, "", "abc"},
		{"trailing blank lines", "a\nb\n\n\n", 1, "b", ""},
		{"a line longer than a chunk", "head\n" + long + "\ntail\n", 2, long + "|tail", ""},
		{"many chunks, last 3", numbered(20000), 3, "line-019998|line-019999|line-020000", ""},
		{"blank lines fill the last chunk", "start\n" + strings.Repeat("\n", logsTailChunk+10), 1, "start", ""},
	}
	for _, tc := range cases {
		path := t.TempDir() + "/log"
		logsWriteFile(t, path, tc.content)
		lines, pos, err := logsOpenTail(path, tc.n)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := strings.Join(lines, "|"); got != tc.want || string(pos.Pending) != tc.pending || pos.Offset != int64(len(tc.content)) {
			t.Errorf("%s: lines %q pending %q offset %d, want %q %q %d", tc.name, logsClip(got), pos.Pending, pos.Offset,
				logsClip(tc.want), tc.pending, len(tc.content))
		}
	}
	// n=0 on a log of many chunks returns every line.
	path := t.TempDir() + "/log"
	logsWriteFile(t, path, numbered(20000))
	if lines, _, err := logsOpenTail(path, 0); err != nil || len(lines) != 20000 || lines[0] != "line-000001" {
		t.Errorf("n=0: %d lines, err %v", len(lines), err)
	}
}

func logsClip(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

func TestLogsDaemonLogLastLineWithoutNewline(t *testing.T) {
	f := newInspFixture(t)
	if err := f.Ctx.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	logsWriteFile(t, f.Ctx.Layout.DaemonLog(), "first\nlast without newline")
	if code := f.run("logs"); code != 0 || f.Out.String() != "first\nlast without newline\n" {
		t.Fatalf("code %d out %q err %s", code, f.Out.String(), f.Err.String())
	}
}
