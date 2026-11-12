package factory

// live.log: what the factory's own commands print while they run. The
// proof's and the precheck's commands run off the loop, through script, for
// minutes (a precheck's jest for 20), and execx keeps their output in
// memory until they end: precheck.md and factory-proof.json get it then. So
// each run of a task's checks (the proof and the precheck of one head)
// writes live.log in the task's directory from its start, empty: a line
// that says the run, then for each command a begin line (its time, its
// step and the command), what it prints as it prints it, and an end line
// (its exit and how long it took). The screen's `o` follows it with less
// +F, and reads the last begin line for the step that runs and since when
// (readLiveStep). Each stream of a command is cut into lines apart
// (execx's Stream and StreamErr), each complete line is stripped of what a
// terminal acts on and its secrets masked (scrub.Secrets: the hosts, emails
// and file names a failure prints stay, as outputOf keeps them), then
// written: a secret split across two writes of the process is masked
// whole, and a private key's block over its lines, from its BEGIN line to
// its END line. An output line that starts as a marker line does is
// indented, so no command writes a begin or an end line. Past liveMax bytes
// of output in one run, the output stops with one line that says so; the
// begin and end lines still follow.

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/scrub"
	"github.com/zhuravel/magnum/internal/textx"
)

const (
	// liveFile is the live log of the task's checks, in its directory.
	liveFile = "live.log"
	// liveMax bounds a run's output in live.log.
	liveMax = 8 << 20
	// liveLineMax bounds one output line: a longer one is cut there, and the
	// rest of it up to its newline is dropped (only its start is shown,
	// scrubbed whole, so no part of a secret it cut shows on its own).
	liveLineMax = 64 << 10
	// liveCommandMax bounds the command a begin line shows, in runes.
	liveCommandMax = 2000
	// livePrefix starts every marker line: the run's, a command's begin and
	// end, the bound's.
	livePrefix = "== "
	// liveBegin and liveEnd are the words of a command's marker lines.
	liveBegin = "begin"
	liveEnd   = "end"
)

// liveCut ends an output line cut at liveLineMax.
const liveCut = " [the factory cut this line at 64 KiB]"

// liveLog is a run's live.log: its file (nil after a failed write), the
// engine's clock, how many bytes of output it holds, and whether it is past
// its bound (limit). Its writes take mu: a command's two streams write from
// their own goroutines. A nil *liveLog writes nothing.
type liveLog struct {
	mu    sync.Mutex
	f     *os.File
	now   func() time.Time
	n     int
	limit int
	full  bool
}

// liveKey is the context key of a run's live log.
type liveKey struct{}

// withLive is ctx carrying l: script streams its commands to it.
func withLive(ctx context.Context, l *liveLog) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, liveKey{}, l)
}

// liveOf is the live log ctx carries, nil for none.
func liveOf(ctx context.Context) *liveLog {
	l, _ := ctx.Value(liveKey{}).(*liveLog)
	return l
}

// openLive starts task id's live.log for a run of its checks, what: the
// file emptied (removed and made anew, mode 0600, never through a link
// another wrote there) with the run's line. A failure is logged and the
// run has no live log: the checks run all the same.
func (e *Engine) openLive(id int64, what string) *liveLog {
	l, err := newLiveLog(e.d.Layout.FactoryTaskDir(id), e.now, cmp.Or(e.liveLimit, liveMax))
	if err != nil {
		e.log.Warn("start the live log of a task's checks; they run without one", "task", id, "err", err)
		return nil
	}
	l.marker("checks " + what)
	return l
}

// newLiveLog is dir's live.log, empty, bounded by limit bytes of output.
func newLiveLog(dir string, now func() time.Time, limit int) (*liveLog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := root.Remove(liveFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := root.OpenFile(liveFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil { // whatever the umask
		_ = f.Close()
		return nil, err
	}
	return &liveLog{f: f, now: now, limit: limit}, nil
}

// close closes the file.
func (l *liveLog) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

// marker writes a marker line: its time and text, on one line, scrubbed.
// Marker lines are written past the bound too.
func (l *liveLog) marker(text string) {
	if l == nil {
		return
	}
	text = strings.Join(strings.Fields(textx.StripTerminal(text)), " ")
	l.write(livePrefix+l.now().Local().Format(time.RFC3339)+" "+scrub.Secrets(text)+"\n", false)
}

// write appends s to the file; output counts against the bound: the first
// output past it is dropped for the bound's line, and so is all after it.
func (l *liveLog) write(s string, output bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if output {
		if l.full {
			return
		}
		if l.n+len(s) > l.limit {
			l.full = true
			s = livePrefix + l.now().Local().Format(time.RFC3339) + " " + fmt.Sprintf(
				"the output of this run is past %s: the factory writes no more of it (each command's begin and end lines still follow)\n",
				sizeWords(l.limit))
		}
		l.n += len(s)
	}
	if _, err := l.f.WriteString(s); err != nil {
		_ = l.f.Close()
		l.f = nil // a disk that refuses a write gets no more
	}
}

// sizeWords is n bytes in words: "8 MiB", "1 KiB", "100 bytes".
func sizeWords(n int) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

// command starts a command of the run in live.log: its begin line (its
// step, label, and the command), and the writers of its two streams, nil
// for a nil log.
func (l *liveLog) command(label, cmd string) (stdout, stderr *liveLines) {
	if l == nil {
		return nil, nil
	}
	l.marker(liveBegin + " " + label + ": " + textx.Clip(cmd, liveCommandMax))
	return &liveLines{l: l}, &liveLines{l: l}
}

// ended ends a command of the run: what its streams hold of a line without
// its newline, then its end line (status: its exit, or why it did not run
// to one; took: how long it ran).
func (l *liveLog) ended(label string, stdout, stderr *liveLines, res execx.Result, err error, took time.Duration) {
	if l == nil {
		return
	}
	stdout.flush()
	stderr.flush()
	l.marker(liveEnd + " " + label + ": " + endWords(res, err) + " after " + capText(took))
}

// endWords is how a command ended, for its end line: its exit, the stop
// of its bound or of the daemon, the deny list's refusal, or why it did not
// start.
func endWords(res execx.Result, err error) string {
	code, denied, rerr := ranTo(res, err)
	switch {
	case denied != "":
		return "denied: " + denied
	case rerr == nil:
		return fmt.Sprintf("exit %d", code)
	case errors.Is(rerr, context.DeadlineExceeded):
		return "stopped at its bound"
	case errors.Is(rerr, context.Canceled):
		return "stopped"
	}
	return "did not run: " + textx.Clip(textx.FirstLine(rerr.Error()), 200)
}

// liveLines is one stream of a command into live.log: it keeps the line
// the stream has not ended yet, and writes each complete line, without
// what a terminal acts on, its secrets masked. Its stream writes from one
// goroutine.
type liveLines struct {
	l    *liveLog
	buf  []byte
	skip bool // the rest of a line cut at liveLineMax is dropped up to its newline
	// inKey is a private key's block the stream printed whose END line did
	// not come yet: each of its lines is masked whole (the scrub, which sees
	// one line, masks the BEGIN line to its end and no line after it).
	inKey bool
}

var _ io.Writer = (*liveLines)(nil)

func (w *liveLines) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			w.take(p)
			break
		}
		w.take(p[:i])
		if !w.skip {
			w.line(w.buf)
		}
		w.buf, w.skip, p = w.buf[:0], false, p[i+1:]
	}
	return n, nil
}

// take adds p to the line not ended yet: a line past liveLineMax is
// written cut, and the rest of it dropped.
func (w *liveLines) take(p []byte) {
	if w.skip {
		return
	}
	w.buf = append(w.buf, p...)
	if len(w.buf) > liveLineMax {
		w.line(append(w.buf[:liveLineMax:liveLineMax], liveCut...))
		w.buf, w.skip = w.buf[:0], true
	}
}

// flush writes the line the stream did not end, at the command's end.
func (w *liveLines) flush() {
	if w == nil {
		return
	}
	if len(w.buf) > 0 && !w.skip {
		w.line(w.buf)
	}
	w.buf, w.skip = nil, false
}

// line writes one output line: stripped of what a terminal acts on, its
// secrets masked (a private key's block whole, over its lines), indented
// when it starts as a marker line does.
func (w *liveLines) line(b []byte) {
	s := textx.StripTerminal(strings.ToValidUTF8(string(b), "�"))
	switch begin := strings.Index(s, "-----BEGIN "); {
	case w.inKey:
		w.inKey = !privateKeyEnds(s)
		s = "<redacted>"
	case begin >= 0 && strings.Contains(s[begin:], "PRIVATE KEY-----") && !privateKeyEnds(s[begin:]):
		w.inKey = true
		s = scrub.Secrets(s) // the BEGIN line, masked from it to its end
	default:
		s = scrub.Secrets(s)
	}
	if strings.HasPrefix(s, livePrefix) {
		s = " " + s
	}
	w.l.write(s+"\n", true)
}

// privateKeyEnds reports whether s holds a private key's END line.
func privateKeyEnds(s string) bool {
	end := strings.Index(s, "-----END ")
	return end >= 0 && strings.Contains(s[end:], "PRIVATE KEY-----")
}

// liveStream gives cmd the writers of its streams into the run's live log
// ctx carries, and returns its end: what the caller runs once the command
// returned. Without a live log, cmd is left as it is.
func (e *Engine) liveStream(ctx context.Context, cmd *execx.Cmd, label, script string) func(execx.Result, error) {
	l := liveOf(ctx)
	if l == nil {
		return func(execx.Result, error) {}
	}
	out, errOut := l.command(label, script)
	cmd.Stream, cmd.StreamErr = out, errOut
	start := e.now()
	return func(res execx.Result, err error) { l.ended(label, out, errOut, res, err, e.now().Sub(start)) }
}

// liveStep is what readLiveStep reads of live.log: the step of the last
// command that began (its label) and when, and whether it still runs (no
// end line after its begin).
type liveStep struct {
	label   string
	at      time.Time
	running bool
}

// liveRead is how much of live.log's end readLiveStep reads first; a step
// that printed more is found in the whole file.
const liveRead = 64 << 10

// readLiveStep reads the last begin or end line of the live log at path:
// false when it has none, or cannot be read.
func readLiveStep(path string) (liveStep, bool) {
	f, err := os.Open(path)
	if err != nil {
		return liveStep{}, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return liveStep{}, false
	}
	size := fi.Size()
	for _, n := range []int64{liveRead, size} {
		off := max(size-n, 0)
		b := make([]byte, size-off)
		if _, err := f.ReadAt(b, off); err != nil && !errors.Is(err, io.EOF) {
			return liveStep{}, false
		}
		if st, ok := lastStep(b, off == 0); ok || off == 0 {
			return st, ok
		}
	}
	return liveStep{}, false
}

// lastStep is the last begin or end line in b, the end of the file (whole:
// all of it; else its first line may be cut and is not read).
func lastStep(b []byte, whole bool) (liveStep, bool) {
	end := len(b)
	for end > 0 {
		start := bytes.LastIndexByte(b[:end], '\n') + 1
		if start == 0 && !whole {
			break
		}
		if st, ok := parseLiveMarker(string(b[start:end])); ok {
			return st, true
		}
		end = start - 1
	}
	return liveStep{}, false
}

// parseLiveMarker reads a begin or an end line: "== <time> begin <label>:
// <command>", "== <time> end <label>: <status>".
func parseLiveMarker(line string) (liveStep, bool) {
	rest, ok := strings.CutPrefix(line, livePrefix)
	if !ok {
		return liveStep{}, false
	}
	stamp, rest, _ := strings.Cut(rest, " ")
	kind, rest, _ := strings.Cut(rest, " ")
	label, _, found := strings.Cut(rest, ": ")
	at, err := time.Parse(time.RFC3339, stamp)
	if !found || err != nil || label == "" || (kind != liveBegin && kind != liveEnd) {
		return liveStep{}, false
	}
	return liveStep{label: label, at: at, running: kind == liveBegin}, true
}

// livePath is task id's live.log in layout dir (the task's directory).
func livePath(dir string) string { return filepath.Join(dir, liveFile) }

// liveExists reports whether the task's directory dir holds a live.log.
func liveExists(dir string) bool { return fsx.Exists(livePath(dir)) }
