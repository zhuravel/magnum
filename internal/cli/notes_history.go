package cli

// `magnum notes <repo> --log | --diff [N]`: the versions of the notes and
// the harness the registry keeps (store.NotesHistory), every one of them:
// the judges', the applied curations', the edits and restores, and the
// imports of what magnum found on disk.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// notesRegistryAction runs the notes actions that need the registry: --log,
// --diff, --curate, --review and --restore.
func notesRegistryAction(c *Context, f notesFlags, back int, full string, nr notes.Repo) int {
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	if f.curate {
		return notesCurate(ctx, c, d, f, full, 0)
	}
	repo, err := d.Store.RepoByFullName(ctx, full)
	if errors.Is(err, store.ErrNotFound) {
		return cmdFail(c, "notes", fmt.Errorf("%s is not in the registry: magnum records the notes of the repositories it reviews", full))
	}
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	switch {
	case f.log:
		return notesLog(ctx, c, d, f, repo)
	case back > 0:
		return notesDiff(ctx, c, d, back, repo, nr)
	case f.restore != 0:
		return notesRestore(ctx, c, d, f, repo, nr)
	}
	return notesReview(ctx, c, d, f, repo, nr)
}

// notesLogEntry is one version as --log --json prints it.
type notesLogEntry struct {
	store.NotesVersion
	HarnessAdded   []string `json:"harness_added"`
	HarnessRemoved []string `json:"harness_removed"`
	HarnessChanged []string `json:"harness_changed"`
}

// notesLog lists the versions, newest first: when, the source (with the
// round's PR), the notes' size and the harness with what changed since the
// version before.
func notesLog(ctx context.Context, c *Context, d *actDeps, f notesFlags, repo store.Repo) int {
	hist, err := d.Store.NotesHistory(ctx, repo.ID, 0)
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	entries := make([]notesLogEntry, 0, len(hist))
	for i, v := range hist {
		e := notesLogEntry{NotesVersion: v}
		var older []notes.File
		if i+1 < len(hist) {
			older = notesListing(hist[i+1])
		}
		e.HarnessAdded, e.HarnessRemoved, e.HarnessChanged = notes.HarnessDelta(older, notesListing(v))
		entries = append(entries, e)
	}
	if f.json {
		if err := writeJSON(c.Stdout, entries); err != nil {
			return cmdFail(c, "notes", err)
		}
		return 0
	}
	full := repo.FullName()
	if len(entries) == 0 {
		fmt.Fprintf(c.Stderr, "no versions of the notes of %s recorded yet (the daemon records them at its start and after each judge round)\n", full)
		return 0
	}
	rows := make([][]string, 0, len(entries))
	for _, e := range entries {
		pr := "-"
		if e.PRNumber > 0 {
			pr = fmt.Sprintf("#%d", e.PRNumber)
		}
		harness := fmt.Sprintf("%s, %d bytes", textx.Count(len(e.Files), "file", "files"), e.HarnessBytes())
		if delta := notesDelta(e.HarnessAdded, e.HarnessRemoved, e.HarnessChanged); delta != "" {
			harness += " (" + delta + ")"
		}
		rows = append(rows, []string{fmt.Sprint(e.ID), e.At.Local().Format("2006-01-02 15:04"), e.Source, pr, fmt.Sprintf("%d bytes", e.Bytes), harness})
	}
	actTable(c.Stdout, []string{"VERSION", "WHEN", "SOURCE", "PR", "NOTES", "HARNESS"}, rows)
	return 0
}

// notesDelta is "+2 -1 ~1": the harness files added, removed and changed.
func notesDelta(added, removed, changed []string) string {
	var parts []string
	if len(added) > 0 {
		parts = append(parts, fmt.Sprintf("+%d", len(added)))
	}
	if len(removed) > 0 {
		parts = append(parts, fmt.Sprintf("-%d", len(removed)))
	}
	if len(changed) > 0 {
		parts = append(parts, fmt.Sprintf("~%d", len(changed)))
	}
	return strings.Join(parts, " ")
}

// notesListing is a version's harness as a listing.
func notesListing(v store.NotesVersion) []notes.File {
	out := make([]notes.File, 0, len(v.Files))
	for _, f := range v.Files {
		out = append(out, notes.File{Name: f.Path, Size: f.Bytes, SHA256: f.SHA256})
	}
	return out
}

// notesFingerprint identifies a version's state (notes.State.Fingerprint).
func notesFingerprint(v store.NotesVersion) string {
	return notes.Snapshot{NotesSHA: v.SHA256, Harness: notesListing(v)}.Fingerprint()
}

// notesDiff shows what changed between the version back versions before the
// notes now and the notes now: the history without repeats (a version that
// holds what the one after it holds is not a step back), the notes as a
// unified diff and the harness by file name.
func notesDiff(ctx context.Context, c *Context, d *actDeps, back int, repo store.Repo, nr notes.Repo) int {
	hist, err := d.Store.NotesHistory(ctx, repo.ID, 0)
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	live, err := notes.ReadState(nr)
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	var steps []store.NotesVersion
	prev := live.Fingerprint()
	for _, v := range hist {
		if fp := notesFingerprint(v); fp != prev {
			steps, prev = append(steps, v), fp
		}
	}
	full := repo.FullName()
	if back > len(steps) {
		return cmdFail(c, "notes", fmt.Errorf("the notes of %s have %d earlier version(s) (`magnum notes %s --log` lists them)", full, len(steps), full))
	}
	v := steps[back-1]
	content, err := d.Store.NotesVersionContent(ctx, v.ID)
	if err != nil {
		return cmdFail(c, "notes", err)
	}
	from := fmt.Sprintf("%s notes, version %d (%s, %s)", full, v.ID, v.At.Local().Format("2006-01-02 15:04"), notesSourceText(v))
	notesPrintDiff(c.Stdout, d.StdoutTTY, from, full+" notes, now", content.Notes, live.Notes)
	added, removed, changed := notes.HarnessDelta(notesListing(v), live.Listing())
	notesPrintHarness(c.Stdout, d.StdoutTTY, added, removed, changed, nil)
	return 0
}

// notesSourceText names a version's source, with its PR.
func notesSourceText(v store.NotesVersion) string {
	if v.PRNumber > 0 {
		return fmt.Sprintf("%s of #%d", v.Source, v.PRNumber)
	}
	return v.Source
}

// ANSI colors of a diff on a terminal.
const (
	notesRed   = "\x1b[31m"
	notesGreen = "\x1b[32m"
	notesCyan  = "\x1b[36m"
	notesBold  = "\x1b[1m"
	notesReset = "\x1b[0m"
)

// notesPrintDiff writes the unified diff of the notes from a to b, colored
// on a terminal; agents wrote both sides, so control characters are
// replaced first.
func notesPrintDiff(w io.Writer, color bool, nameA, nameB string, a, b []byte) {
	diff := notes.Unified(nameA, nameB, notes.Printable(string(a)), notes.Printable(string(b)), 3)
	if diff == "" {
		fmt.Fprintf(w, "the notes are the same in %s and %s\n", nameA, nameB)
		return
	}
	for line := range strings.Lines(diff) {
		code := ""
		switch {
		case !color:
		case strings.HasPrefix(line, "---"), strings.HasPrefix(line, "+++"):
			code = notesBold
		case strings.HasPrefix(line, "@@"):
			code = notesCyan
		case strings.HasPrefix(line, "+"):
			code = notesGreen
		case strings.HasPrefix(line, "-"):
			code = notesRed
		}
		if code == "" {
			io.WriteString(w, line)
			continue
		}
		fmt.Fprintf(w, "%s%s%s\n", code, strings.TrimSuffix(line, "\n"), notesReset)
	}
}

// notesPrintHarness writes the harness changes by file name, with the
// curator's actions and reasons when there are any (reasons by name).
func notesPrintHarness(w io.Writer, color bool, added, removed, changed []string, reasons map[string]notes.Change) {
	if len(added)+len(removed)+len(changed) == 0 && len(reasons) == 0 {
		fmt.Fprintln(w, "harness: unchanged")
		return
	}
	fmt.Fprintln(w, "harness:")
	mark := func(sym, code, name string) {
		why := ""
		if ch, ok := reasons[name]; ok {
			why = ": " + ch.Action
			if ch.Into != "" {
				why += " into " + ch.Into
			}
			if ch.Reason != "" {
				why += ": " + notes.Printable(ch.Reason)
			}
			delete(reasons, name)
		}
		line := fmt.Sprintf("  %s %s%s", sym, notes.Printable(name), why)
		if color && code != "" {
			line = code + line + notesReset
		}
		fmt.Fprintln(w, line)
	}
	for _, n := range removed {
		mark("-", notesRed, n)
	}
	for _, n := range added {
		mark("+", notesGreen, n)
	}
	for _, n := range changed {
		mark("~", notesCyan, n)
	}
	for _, n := range slices.Sorted(maps.Keys(reasons)) {
		mark(" ", "", n)
	}
}

// notesNow is the CLI's clock for the notes actions.
func notesNow(d *actDeps) time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}
