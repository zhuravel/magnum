package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/store"
)

const logsUsage = "[<ref>|<slot>|<subject>] [-f] [-n N] [--json] [--raw]"

func newLogsCmd(c *Context) *cobra.Command {
	var f logsFlags
	cmd := newCommand(groupInspect, "logs "+logsUsage, "daemon log, or the audit events of a PR, slot or subject (-f follows)",
		"Print the tail of the daemon log (state/logs/daemon.log), or the audit events of a PR reference, a slot "+
			"name or a subject such as request:<id>, identity:<name>, tool:codex or pool:<repo>. -f keeps following "+
			"new lines or events, -n sets how many to show first, --raw prints daemon log lines as written (JSON) "+
			"and --json prints events as JSON lines.",
		func(pos []string) int { return runLogs(c, f, pos) })
	fs := cmd.Flags()
	fs.BoolVarP(&f.follow, "follow", "f", false, "follow: keep printing new lines")
	fs.IntVarP(&f.n, "lines", "n", 50, "how many recent lines or events to show first")
	fs.BoolVar(&f.json, "json", false, "events as JSON lines")
	fs.BoolVar(&f.raw, "raw", false, "daemon log lines as written (JSON)")
	cmd.ValidArgsFunction = completeFirst(c.completePRs, c.completeSlots)
	return cmd
}

// logsFlags are the parsed `magnum logs` flags.
type logsFlags struct {
	follow, json, raw bool
	n                 int
}

// logsSubjectPrefixes are the event subject kinds a raw "<kind>:<rest>"
// argument may name.
var logsSubjectPrefixes = []string{"pr", "slot", "request", "identity", "tool", "pool", "repo", "slug", "path", "watch"}

func runLogs(c *Context, f logsFlags, pos []string) int {
	if len(pos) > 1 {
		return inspUsage(c, "logs", "at most one PR reference, slot name or subject", logsUsage)
	}
	ctx, cancel := signalContext()
	defer cancel()
	if len(pos) == 0 {
		return runLogsFile(ctx, c, f)
	}
	return runLogsEvents(ctx, c, f, pos[0])
}

// runLogsFile prints the tail of the daemon log and, with -f, follows it.
func runLogsFile(ctx context.Context, c *Context, f logsFlags) int {
	path := c.Layout.DaemonLog()
	lines, fpos, err := logsOpenTail(path, f.n)
	missing := errors.Is(err, fs.ErrNotExist)
	switch {
	case err == nil:
	case missing && f.follow:
		// -f waits for the daemon to create the log.
	case missing:
		return cmdFail(c, "logs", fmt.Errorf("no daemon log yet at %s; the daemon writes it once it runs (`magnum install` starts it)",
			inspTilde(path)))
	default:
		return cmdFail(c, "logs", err)
	}
	for _, l := range lines {
		fmt.Fprintln(c.Stdout, logsLine(l, f.raw))
	}
	if !f.follow {
		if len(fpos.Pending) > 0 { // a last line without its newline is still a line
			fmt.Fprintln(c.Stdout, logsLine(string(fpos.Pending), f.raw))
		}
		return 0
	}
	if err := logsFollowFile(ctx, c.Stdout, path, fpos, f.raw); err != nil && !errors.Is(err, context.Canceled) {
		return cmdFail(c, "logs", err)
	}
	return 0
}

// runLogsEvents prints the audit events of arg (a PR reference, slot name or
// subject) and, with -f, follows them.
func runLogsEvents(ctx context.Context, c *Context, f logsFlags, arg string) int {
	a, err := inspOpenApp(c, false)
	if err != nil {
		return cmdFail(c, "logs", err)
	}
	defer a.Close()
	m, label, err := logsMatchers(ctx, a.Store, a.Refs(), arg)
	if err != nil {
		return cmdFail(c, "logs", err)
	}
	evs, cursor, err := logsBacklog(ctx, a.Store, m, f.n, f.follow)
	if err != nil {
		return cmdFail(c, "logs", err)
	}
	for _, ev := range evs {
		logsPrintEvent(c.Stdout, ev, f.json)
	}
	if len(evs) == 0 && !f.json {
		fmt.Fprintf(c.Stderr, "no events for %s yet\n", label)
	}
	if !f.follow {
		return 0
	}
	if err := logsFollowEvents(ctx, c.Stdout, a.Store, m, cursor, f.json); err != nil && !errors.Is(err, context.Canceled) {
		return cmdFail(c, "logs", err)
	}
	return 0
}

// logsBacklog returns the newest n matching events (all when n <= 0) to
// print first and, when following, the id to follow from: the newest
// backlog event's (0 when there is none). Event ids only grow, so an event
// appended after the backlog query has a larger id and is printed once by
// the follower, never skipped or shown twice.
func logsBacklog(ctx context.Context, st *store.Store, m []store.SubjectMatch, n int, follow bool) ([]store.Event, int64, error) {
	evs, err := st.EventsMatching(ctx, m, 0, n)
	if err != nil || !follow || len(evs) == 0 {
		return evs, 0, err
	}
	return evs, evs[len(evs)-1].ID, nil
}

// logsMatchers maps the argument to subject matchers:
//   - "<kind>:<rest>" (pr:, slot:, request:, identity:, tool:, ...) is taken
//     as a subject and everything below it (subject:...);
//   - a slot name matches slot:<name> and slot:<name>:* (provision, checkout, release, remove);
//   - a PR matches pr:<owner>/<name>#<N> (engine, pipeline), pr:<store id>
//     (cleanup) and the checkout steps slot:<slot>:pr:<N>:* of every slot it was in.
func logsMatchers(ctx context.Context, st *store.Store, refs app.RefParser, arg string) ([]store.SubjectMatch, string, error) {
	if kind, rest, ok := strings.Cut(arg, ":"); ok && rest != "" && slices.Contains(logsSubjectPrefixes, kind) {
		return []store.SubjectMatch{logsBelow(arg)}, arg, nil
	}
	if sl, err := st.SlotByName(ctx, arg); err == nil && sl.Kind != store.SlotKindPerPR {
		return []store.SubjectMatch{logsBelow("slot:" + sl.Name)}, "slot " + sl.Name, nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, "", err
	}
	repo, pr, err := app.LookupPR(ctx, st, refs, arg)
	if err != nil {
		owner, name, number, perr := refs.ResolvePR(ctx, arg)
		if perr != nil {
			return nil, "", fmt.Errorf("%q is neither a slot name, a PR reference nor an event subject (pr:…, slot:…, request:N)", arg)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, "", err
		}
		label := fmt.Sprintf("%s/%s#%d", owner, name, number)
		return []store.SubjectMatch{{Exact: "pr:" + label}, logsBelow("slot:" + label)}, label, nil
	}
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	m := []store.SubjectMatch{{Exact: "pr:" + label}, {Exact: "pr:" + strconv.FormatInt(pr.ID, 10)}, logsBelow("slot:" + label)}
	as, err := st.AssignmentsByPR(ctx, pr.ID)
	if err != nil {
		return nil, "", err
	}
	seen := map[int64]bool{}
	for _, as := range as {
		if seen[as.SlotID] {
			continue
		}
		seen[as.SlotID] = true
		sl, err := st.SlotByID(ctx, as.SlotID)
		if err != nil || sl.Kind == store.SlotKindPerPR {
			continue
		}
		m = append(m, store.SubjectMatch{Prefix: "slot:" + sl.Name + ":pr:" + strconv.Itoa(pr.Number) + ":"})
	}
	return m, label, nil
}

// logsBelow matches subject itself and every subject below it
// ("<subject>:..."), never a sibling ("slot:review1" is not "slot:review10").
func logsBelow(subject string) store.SubjectMatch {
	return store.SubjectMatch{Exact: subject, Prefix: subject + ":"}
}

// logsPrintEvent prints one event line (or one JSON line).
func logsPrintEvent(w io.Writer, ev store.Event, asJSON bool) {
	if asJSON {
		b, err := json.Marshal(ev)
		if err == nil {
			fmt.Fprintf(w, "%s\n", b)
		}
		return
	}
	kind := ev.Kind
	if ev.Kind == store.KindStep {
		kind = strings.TrimSpace("step " + store.Deref(ev.Step) + " " + store.Deref(ev.Phase))
	}
	// The subject, step and message can carry text from outside magnum.
	fmt.Fprintf(w, "%s %-5s %s  %s  %s\n", ev.At.Local().Format("2006-01-02 15:04:05"), actClean(strings.ToUpper(ev.Level)),
		actClean(inspOrDash(store.Deref(ev.Subject))), actClean(kind), actClean(ev.Message))
}

// logsFollowEvents prints new matching events until ctx ends.
func logsFollowEvents(ctx context.Context, w io.Writer, st *store.Store, m []store.SubjectMatch, after int64, asJSON bool) error {
	for {
		evs, err := st.EventsMatching(ctx, m, after, 0)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		for _, ev := range evs {
			logsPrintEvent(w, ev, asJSON)
			after = ev.ID
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(inspPoll):
		}
	}
}

// logsFilePos is where `logs -f` continues in the daemon log: just past what
// the tail read, in the very file it read.
type logsFilePos struct {
	Offset  int64       // first byte not read yet
	Info    os.FileInfo // identity of the file read (os.SameFile detects a replaced one); nil when there was none
	Pending []byte      // an unfinished last line (no newline yet): the follower completes it
}

// logsTailChunk is how much logsTail reads backwards at a time.
const logsTailChunk = 64 << 10

// logsOpenTail opens path once and returns its last n complete lines (all
// of them when n <= 0) and the position to follow from.
func logsOpenTail(path string, n int) ([]string, logsFilePos, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, logsFilePos{}, err
	}
	defer f.Close()
	return logsTail(f, n)
}

// logsTail returns the last n complete lines of f (all of them when n <= 0),
// reading backwards from the end so a large log is not loaded whole. The
// size, the identity and the bytes come from the same handle: Offset is
// the end of the bytes read and Info is the file they came from, so lines
// appended while the tail runs are neither shown twice nor missed. A last
// line without its newline is returned as Pending, not as a line.
func logsTail(f *os.File, n int) ([]string, logsFilePos, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, logsFilePos{}, err
	}
	pos := logsFilePos{Info: fi, Offset: fi.Size()}
	var chunks [][]byte // the tail of the file, newest chunk first
	newlines := 0
	for start := fi.Size(); ; {
		if start > 0 {
			size := min(start, logsTailChunk)
			b := make([]byte, size)
			got, err := f.ReadAt(b, start-size)
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, logsFilePos{}, err
			}
			if got < len(b) {
				if len(chunks) > 0 {
					return nil, logsFilePos{}, fmt.Errorf("%s shrank while it was being read", f.Name())
				}
				pos.Offset = start - size + int64(got) // truncated meanwhile: follow from where the bytes end
				b = b[:got]
			}
			start -= size
			newlines += bytes.Count(b, []byte{'\n'})
			chunks = append(chunks, b)
			if n <= 0 || newlines <= n {
				continue // read on: the first wanted line may not be whole yet
			}
		}
		var data []byte
		for _, chunk := range slices.Backward(chunks) {
			data = append(data, chunk...)
		}
		complete := data[:bytes.LastIndexByte(data, '\n')+1]
		var lines []string
		if text := strings.TrimRight(string(complete), "\n"); text != "" {
			lines = strings.Split(text, "\n")
			if start > 0 {
				lines = lines[1:] // cut by the chunk boundary
			}
		}
		if start > 0 && len(lines) <= n {
			continue // trailing blank lines used up the surplus
		}
		pos.Pending = bytes.Clone(data[len(complete):])
		if n > 0 && len(lines) > n {
			lines = lines[len(lines)-n:]
		}
		return lines, pos, nil
	}
}

// logsLine renders one daemon.log line: slog JSON becomes
// "2006-01-02 15:04:05 LEVEL msg k=v ...", anything else is printed as is.
// Control characters (ESC, CR, tabs) become spaces: the values can carry text
// from outside magnum, and JSON decoding turns \u001b back into a real ESC.
// raw prints the line exactly as written.
func logsLine(line string, raw bool) string {
	if raw {
		return line
	}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var rec map[string]any
	if err := dec.Decode(&rec); err != nil || rec["msg"] == nil {
		return actClean(line)
	}
	ts := ""
	if s, ok := rec["time"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			ts = t.Local().Format("2006-01-02 15:04:05")
		} else {
			ts = s
		}
	}
	level, _ := rec["level"].(string)
	msg := fmt.Sprint(rec["msg"])
	var keys []string
	for k := range rec {
		if k != "time" && k != "level" && k != "msg" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "%s %-5s %s", ts, level, msg)
	for _, k := range keys {
		v := rec[k]
		var s string
		switch x := v.(type) {
		case string:
			s = x
			if s == "" || strings.ContainsAny(s, " \t\"=") {
				s = strconv.Quote(s)
			}
		default:
			bs, _ := json.Marshal(x)
			s = string(bs)
		}
		fmt.Fprintf(&b, " %s=%s", k, s)
	}
	return actClean(b.String())
}

// logsFollowFile prints lines appended to path after from until ctx ends.
// A file that is no longer the one from.Info names (replaced, even by a
// longer one) or that is shorter than the offset (truncated) is read again
// from its start; the unfinished line of the old file is dropped, not glued
// to the first line of the new one.
func logsFollowFile(ctx context.Context, w io.Writer, path string, from logsFilePos, raw bool) error {
	prev, offset, pending := from.Info, from.Offset, bytes.Clone(from.Pending)
	for {
		// Open once per poll: the identity, size and bytes are one file.
		f, err := os.Open(path)
		switch {
		case err == nil:
			chunk, fi, rotated, err := logsPoll(f, prev, offset)
			f.Close()
			if err != nil {
				return err
			}
			if rotated {
				offset, pending = 0, nil
			}
			prev = fi
			offset += int64(len(chunk))
			pending = append(pending, chunk...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				fmt.Fprintln(w, logsLine(string(pending[:i]), raw))
				pending = pending[i+1:]
			}
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(inspPoll):
		}
	}
}

// logsPoll reads what f gained since offset. rotated reports that f is not
// the file prev names or is shorter than offset, so the read starts at 0.
func logsPoll(f *os.File, prev os.FileInfo, offset int64) (chunk []byte, fi os.FileInfo, rotated bool, err error) {
	fi, err = f.Stat()
	if err != nil {
		return nil, nil, false, err
	}
	if (prev != nil && !os.SameFile(prev, fi)) || fi.Size() < offset {
		rotated, offset = true, 0
	}
	if fi.Size() <= offset {
		return nil, fi, rotated, nil
	}
	chunk = make([]byte, fi.Size()-offset)
	got, err := f.ReadAt(chunk, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, false, err
	}
	return chunk[:got], fi, rotated, nil
}
