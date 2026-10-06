package cli

// `magnum notes` without a repository: one row per repository with notes on
// disk or in the registry, with their sizes against the [notes] curation
// triggers, who changed them last and what the curation of them waits for.
// `magnum status` and the dashboard sum the same rows up in one line
// (notesSummaryLine).

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// The states of a repository's notes, most pressing last
// (notesRow.State; the list says them in words).
const (
	notesOK       = "ok"
	notesOver     = "over_limit"
	notesProposal = "proposal" // a proposal waits for review
	notesStale    = "stale"    // it waits, and the notes changed since it was made
	notesQueued   = "queued"   // a curation waits for a start (engine.KVNotesCurateQueue)
	notesCurating = "curating" // a curation runs (engine.KVNotesCurating)
)

// notesRow is one repository of the list (its --json form).
type notesRow struct {
	Repo     string             `json:"repo"`
	Path     string             `json:"path"`
	Size     notes.Size         `json:"size"`
	Over     []string           `json:"over"`              // the limits the notes are past
	Changed  *notesRowChange    `json:"changed,omitempty"` // the latest recorded version
	Modified *time.Time         `json:"modified,omitempty"`
	State    string             `json:"state"`
	Proposal *notesRowProposal  `json:"proposal,omitempty"` // the newest waiting for review
	Curation *engine.CurateMark `json:"curation,omitempty"` // running or queued: its trigger and since when
	Waits    string             `json:"waits_for,omitempty"`
	Error    string             `json:"error,omitempty"`
}

// notesRowChange is who changed the notes last: a version's source, with the
// round's PR for a judge's.
type notesRowChange struct {
	Version  int64     `json:"version"`
	At       time.Time `json:"at"`
	Source   string    `json:"source"`
	PRNumber int       `json:"pr_number,omitempty"`
}

// notesRowProposal is a proposal waiting for review; Stale when the notes
// changed since it was made.
type notesRowProposal struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Trigger   string    `json:"trigger,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Stale     bool      `json:"stale"`
}

// notesListJSON is what `magnum notes --json` prints.
type notesListJSON struct {
	Limits   notes.Limits `json:"limits"`
	Repos    []notesRow   `json:"repos"`
	Registry string       `json:"registry_error,omitempty"`
}

// notesList prints the list, or --json.
func notesList(c *Context, f notesFlags) int {
	ctx, cancel := signalContext()
	defer cancel()
	limits, _ := notesLimits(c)
	out := notesListJSON{Limits: limits, Repos: []notesRow{}}
	st, err := notesOpenStore(c)
	if err != nil {
		out.Registry = err.Error()
	}
	if st != nil {
		defer st.Close()
	}
	now := inspNow()
	rows, err := notesOverview(ctx, c.Layout, st, limits, now)
	if err != nil {
		out.Registry = err.Error()
	}
	out.Repos = append(out.Repos, rows...)
	if f.json {
		if err := writeJSON(c.Stdout, out); err != nil {
			return cmdFail(c, "notes", err)
		}
		return 0
	}
	if len(rows) == 0 {
		fmt.Fprintf(c.Stderr, "no repository has notes yet (%s); the judges write them as they review\n", inspTilde(engine.NotesRoot(c.Layout)))
	} else {
		notesPrintList(c.Stdout, rows, now)
	}
	if out.Registry != "" {
		fmt.Fprintf(c.Stderr, "registry: %s\n", out.Registry)
	}
	return 0
}

// notesOverview gathers every repository with notes, sorted by name: a notes
// file on disk, or in the registry a recorded version with content, a
// harness on disk, a proposal waiting or a curation running or queued. st
// may be nil (no registry yet: the files alone). An error reading the
// registry is returned with the rows the files gave.
func notesOverview(ctx context.Context, l paths.Layout, st *store.Store, limits notes.Limits, now time.Time) ([]notesRow, error) {
	names := notesOnDisk(l)
	var repos map[string]store.Repo
	var regErr error
	var curating *engine.CurateMark
	var queue []engine.CurateQueued
	if st != nil {
		list, err := st.ListRepos(ctx)
		regErr = err
		repos = make(map[string]store.Repo, len(list))
		for _, r := range list {
			full := strings.ToLower(r.FullName())
			repos[full] = r
			if !slices.Contains(names, full) {
				names = append(names, full)
			}
		}
		curating, queue = engine.ReadCurating(ctx, st), engine.ReadCurateQueue(ctx, st)
	}
	slices.Sort(names)
	var out []notesRow
	for _, full := range names {
		owner, name, _ := strings.Cut(full, "/")
		nr, ok := notes.RepoOf(engine.NotesPath(l, owner, name))
		if !ok {
			continue
		}
		row := notesRow{Repo: full, Path: nr.Notes(), Over: []string{}, State: notesOK}
		if size, _, err := notes.Measure(nr, limits); err != nil {
			row.Error = err.Error()
		} else {
			row.Size = size
			if over := size.Over(limits); over != nil {
				row.Over = over
			}
		}
		if fi, err := os.Stat(nr.Notes()); err == nil {
			t := fi.ModTime()
			row.Modified = &t
		}
		has := row.Size.Exists || row.Size.HarnessFiles > 0
		if repo, ok := repos[full]; ok && st != nil {
			if v, err := st.LatestNotesVersion(ctx, repo.ID); err == nil {
				row.Changed = &notesRowChange{Version: v.ID, At: v.At, Source: v.Source, PRNumber: v.PRNumber}
				has = has || v.Bytes > 0 || len(v.Files) > 0
			}
			ps, err := st.NotesProposals(ctx, store.NotesProposalFilter{RepoID: repo.ID, States: []string{store.ProposalPending}, Limit: 1})
			if err != nil {
				regErr = cmp.Or(regErr, err)
			} else if len(ps) > 0 {
				p := ps[0]
				row.Proposal = &notesRowProposal{ID: p.ID, Kind: p.Kind, Trigger: p.Trigger, CreatedAt: p.CreatedAt}
				stale, err := engine.ProposalStale(ctx, st, nr, p)
				if err != nil {
					row.Error = cmp.Or(row.Error, err.Error())
				}
				row.Proposal.Stale, has = stale, true
			}
		}
		if curating != nil && strings.EqualFold(curating.Repo, full) {
			row.Curation, has = curating, true
		}
		if i := slices.IndexFunc(queue, func(q engine.CurateQueued) bool { return strings.EqualFold(q.Repo, full) }); i >= 0 && row.Curation == nil {
			q := queue[i]
			row.Curation, row.Waits, has = &engine.CurateMark{Repo: q.Repo, Trigger: q.Trigger, Started: q.At}, q.Why, true
		}
		if !has {
			continue
		}
		switch {
		case row.Curation != nil && row.Waits == "":
			row.State = notesCurating
		case row.Curation != nil:
			row.State = notesQueued
		case row.Proposal != nil && row.Proposal.Stale:
			row.State = notesStale
		case row.Proposal != nil:
			row.State = notesProposal
		case len(row.Over) > 0:
			row.State = notesOver
		}
		out = append(out, row)
	}
	return out, regErr
}

// notesOnDisk lists the repositories with a notes file under l's notes
// root, as lower-case owner/name.
func notesOnDisk(l paths.Layout) []string {
	root := engine.NotesRoot(l)
	if root == "" {
		return nil
	}
	var out []string
	owners, _ := os.ReadDir(root)
	for _, o := range owners {
		if !o.IsDir() || strings.HasPrefix(o.Name(), ".") { // .curate holds curations, no owner
			continue
		}
		files, _ := os.ReadDir(filepath.Join(root, o.Name()))
		for _, f := range files {
			if name, ok := strings.CutSuffix(f.Name(), ".md"); ok && name != "" && f.Type().IsRegular() {
				out = append(out, strings.ToLower(o.Name()+"/"+name))
			}
		}
	}
	return out
}

// notesPrintList writes the table, a legend for the marks and one hint per
// repository whose state asks for something.
func notesPrintList(w io.Writer, rows []notesRow, now time.Time) {
	table := make([][]string, 0, len(rows))
	marked := false
	var hints []string
	for _, r := range rows {
		past := func(limit string) string {
			if slices.Contains(r.Over, limit) {
				marked = true
				return "!"
			}
			return ""
		}
		text := "none"
		if r.Size.Exists {
			text = notesBytes(r.Size.Bytes) + past(notes.LimitBytes) + ", " + textx.Count(r.Size.Lines, "line", "lines")
			if r.Size.LongLines > 0 {
				text += fmt.Sprintf(" (%d long%s)", r.Size.LongLines, past(notes.LimitLine))
			}
		}
		harness := "none"
		if r.Size.HarnessFiles > 0 {
			harness = textx.Count(r.Size.HarnessFiles, "file", "files") + past(notes.LimitHarnessFiles) + ", " +
				notesBytes(r.Size.HarnessBytes) + past(notes.LimitHarnessBytes)
		}
		table = append(table, []string{r.Repo, text, harness, notesChangedText(r, now), notesStateText(r, now)})
		switch r.State {
		case notesProposal:
			hints = append(hints, "review: magnum notes "+r.Repo+" --review")
		case notesStale:
			hints = append(hints, "review: magnum notes "+r.Repo+" --review (stale: it merges the notes' changes since, or asks for a new curation)")
		case notesOver:
			hints = append(hints, "curate: magnum notes "+r.Repo+" --curate (or wait: the daemon curates notes past a limit once they change)")
		}
	}
	actTable(w, []string{"REPO", "NOTES", "HARNESS", "CHANGED", "STATE"}, table)
	if marked || len(hints) > 0 {
		fmt.Fprintln(w)
	}
	if marked {
		fmt.Fprintln(w, "! past a [notes] limit: a curation trigger, not a cap")
	}
	for _, h := range hints {
		fmt.Fprintln(w, h)
	}
}

// notesBytes is a size as the list writes it: "812 B", "26.5 KB", "1.2 MB".
func notesBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}

// notesChangedText is the CHANGED cell: how long ago and by whom (the judge
// of a PR, a curation, a person, an import), or the file's age when no
// version is recorded.
func notesChangedText(r notesRow, now time.Time) string {
	switch {
	case r.Changed != nil:
		who := r.Changed.Source
		if r.Changed.PRNumber > 0 {
			who += fmt.Sprintf(" #%d", r.Changed.PRNumber)
		}
		return actAgo(now, r.Changed.At) + " ago, " + who
	case r.Modified != nil:
		return actAgo(now, *r.Modified) + " ago, not recorded"
	}
	return "-"
}

// notesStateText is the STATE cell.
func notesStateText(r notesRow, now time.Time) string {
	switch r.State {
	case notesCurating:
		return fmt.Sprintf("curation running (%s, %s)", r.Curation.Trigger, actAgo(now, r.Curation.Started))
	case notesQueued:
		what := "a judge stage"
		if r.Waits == engine.QueuedBusy {
			what = "the running curation"
		}
		return fmt.Sprintf("curation queued (%s, waits for %s)", r.Curation.Trigger, what)
	case notesStale, notesProposal:
		p := r.Proposal
		why := cmp.Or(p.Trigger, p.Kind)
		if p.Kind == store.ProposalRestore {
			why = "restore"
		}
		if r.State == notesStale {
			return fmt.Sprintf("proposal %d stale (%s, %s; the notes changed since it was made)", p.ID, why, actAgo(now, p.CreatedAt))
		}
		return fmt.Sprintf("proposal %d to review (%s, %s)", p.ID, why, actAgo(now, p.CreatedAt))
	case notesOver:
		return "over limit (" + strings.Join(r.Over, ", ") + ")"
	}
	if r.Error != "" {
		return "unreadable: " + statusSafe(r.Error, 80)
	}
	return "ok"
}

// notesSummaryLine sums rows up for `magnum status` and the dashboard: "4
// repos · 2 proposals to review (talkable/talkable, example/api stale) · 1
// over limit", leaving out the parts that are zero, then a curation running
// or queued; "" without a repository with notes.
func notesSummaryLine(rows []notesRow) string {
	if len(rows) == 0 {
		return ""
	}
	parts := []string{textx.Count(len(rows), "repo", "repos")}
	var proposals, queued []string
	over, curating := 0, ""
	for _, r := range rows {
		if p := r.Proposal; p != nil {
			name := r.Repo
			if p.Stale {
				name += " stale"
			}
			proposals = append(proposals, name)
		}
		if len(r.Over) > 0 {
			over++
		}
		switch r.State {
		case notesCurating:
			curating = r.Repo
		case notesQueued:
			queued = append(queued, r.Repo)
		}
	}
	if n := len(proposals); n > 0 {
		parts = append(parts, textx.Count(n, "proposal to review", "proposals to review")+" ("+strings.Join(proposals, ", ")+")")
	}
	if over > 0 {
		parts = append(parts, fmt.Sprintf("%d over limit", over))
	}
	if curating != "" {
		parts = append(parts, "curating "+curating)
	}
	if n := len(queued); n > 0 {
		parts = append(parts, textx.Count(n, "curation queued", "curations queued")+" ("+strings.Join(queued, ", ")+")")
	}
	return strings.Join(parts, " · ")
}

// completeNotesRepos offers the repositories with notes: a notes file on
// disk, or in the registry (read-only) a recorded version with content or a
// proposal waiting for review, which the description names; a repository of
// daemon.default_repo's owner also by its bare name.
func (c *Context) completeNotesRepos(string) []cobra.Completion {
	defOwner := ""
	if c.LoadConfig() == nil {
		defOwner, _, _ = strings.Cut(strings.ToLower(c.Config.Daemon.DefaultRepo), "/")
	}
	waiting := map[string]int64{}
	names := notesOnDisk(c.Layout)
	c.completeQuery(`SELECT r.owner, r.name, (SELECT MAX(p.id) FROM notes_proposals p WHERE p.repo_id = r.id AND p.state = 'pending')
FROM repos r WHERE EXISTS (SELECT 1 FROM notes_versions v WHERE v.repo_id = r.id
    AND (v.bytes > 0 OR EXISTS (SELECT 1 FROM notes_version_files f WHERE f.version_id = v.id)))
  OR EXISTS (SELECT 1 FROM notes_proposals p WHERE p.repo_id = r.id AND p.state = 'pending')`, func(rows *sql.Rows) error {
		var owner, name string
		var pending sql.NullInt64
		if err := rows.Scan(&owner, &name, &pending); err != nil {
			return err
		}
		full := strings.ToLower(owner + "/" + name)
		if !slices.Contains(names, full) {
			names = append(names, full)
		}
		if pending.Valid {
			waiting[full] = pending.Int64
		}
		return nil
	})
	slices.Sort(names)
	var out []cobra.Completion
	for _, full := range names {
		desc := "repository notes"
		if id, ok := waiting[full]; ok {
			desc += fmt.Sprintf(", proposal %d waits for review", id)
		}
		out = append(out, cobra.CompletionWithDesc(full, desc))
		if owner, name, _ := strings.Cut(full, "/"); defOwner != "" && owner == defOwner {
			out = append(out, cobra.CompletionWithDesc(name, desc))
		}
	}
	return out
}
