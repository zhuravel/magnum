package engine

// What the daemon records about the repository notes (DECISIONS "Repository
// notes: triggers, history, usage and curation"): after each judge round the
// state the judge left becomes a version in the registry, the harness files
// the judge and the reviewers used are counted, and the notes are measured
// against the [notes] limits, which mark a repository for curation. At
// startup and on every reconcile a state magnum has no record of (the first
// start, a hand edit) is imported. No event carries notes text.

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// KVNotesOver holds the [notes] limits a repository's notes are past,
// comma-separated ("max_bytes,max_line"): the mark a curation looks for. It
// is set when they are measured past a limit and deleted when they are back
// within all of them.
func KVNotesOver(fullName string) string { return "notes." + strings.ToLower(fullName) + ".over" }

// notesReportMax bounds what is read of a reviewer report for harness paths.
const notesReportMax = 1 << 20

// notesRepo is owner/name's notes under l; false when NotesPath has none.
func notesRepo(l paths.Layout, owner, name string) (notes.Repo, bool) {
	p := NotesPath(l, owner, name)
	if p == "" {
		return notes.Repo{}, false
	}
	return notes.RepoOf(p)
}

// notesSubject is the events' subject for a repository's notes.
func notesSubject(nr notes.Repo) string { return "notes:" + nr.FullName() }

// notesLimits are the [notes] curation triggers.
func (e *Engine) notesLimits() notes.Limits {
	n := e.cfg.Notes
	return notes.Limits{MaxBytes: n.MaxBytes, MaxLine: n.MaxLine, MaxHarnessFiles: n.MaxHarnessFiles, MaxHarnessBytes: n.MaxHarnessBytes}
}

// ContentOf is a notes state as the registry stores it.
func ContentOf(s notes.State) store.NotesContent {
	c := store.NotesContent{Notes: s.Notes}
	for _, b := range s.Files {
		c.Files = append(c.Files, store.NotesBlob{Path: b.Path, SHA256: b.SHA256, Bytes: int64(len(b.Body)), Body: b.Body})
	}
	return c
}

// StateOf is a version's content as a notes state.
func StateOf(c store.NotesContent) notes.State {
	s := notes.State{Exists: len(c.Notes) > 0, Notes: c.Notes}
	for _, f := range c.Files {
		s.Files = append(s.Files, notes.Blob{Path: f.Path, SHA256: f.SHA256, Body: f.Body})
	}
	return s
}

// listingOf is a version's harness as a listing.
func listingOf(v store.NotesVersion) []notes.File {
	out := make([]notes.File, 0, len(v.Files))
	for _, f := range v.Files {
		out = append(out, notes.File{Name: f.Path, Size: f.Bytes, SHA256: f.SHA256})
	}
	return out
}

// recordRoundNotes runs after a round: when its judge was prompted (the
// round's notes snapshot exists), a state that differs from the snapshot is
// recorded as the judge's version and the harness files used are counted;
// then the notes are measured against the limits. A dry-run daemon and a
// round without notes do nothing; a failure is logged, never the round's.
func (e *Engine) recordRoundNotes(ctx context.Context, job *roundJob, in pipeline.RoundInput, res pipeline.RoundResult) {
	if in.NotesPath == "" || e.d.DryRun || res.ReportDir == "" {
		return
	}
	nr, ok := notes.RepoOf(in.NotesPath)
	if !ok {
		return
	}
	state, err := notes.ReadState(nr)
	if err != nil {
		e.log.Warn("read the repository notes after a round", "repo", nr.FullName(), "err", err)
		e.measureNotes(ctx, nr) // a harness too large to record still counts against the limits
		return
	}
	if snap, err := notes.ReadSnapshot(res.ReportDir); err == nil && snap.Round == res.Round {
		if state.Fingerprint() != snap.Fingerprint() {
			e.recordNotes(ctx, job.repo, nr, state, store.NotesFromJudge, job.pr.ID, res.JudgeRunID)
		}
		e.recordNotesUsage(ctx, job.repo.ID, nr, state, res)
	}
	e.checkNotesLimits(ctx, nr, state.Size(e.cfg.Notes.MaxLine))
}

// measureNotes checks the limits from the files' sizes alone.
func (e *Engine) measureNotes(ctx context.Context, nr notes.Repo) {
	if size, _, err := notes.Measure(nr, e.notesLimits()); err == nil {
		e.checkNotesLimits(ctx, nr, size)
	}
}

// recordNotes records state as a version of repo's notes (unless the latest
// version already holds it) and writes a notes.changed event with the lines
// and harness files it adds and removes; it returns the version that holds
// state now.
func (e *Engine) recordNotes(ctx context.Context, repo store.Repo, nr notes.Repo, state notes.State, source string, prID int64, runID string) (store.NotesVersion, error) {
	prev, perr := e.st.LatestNotesVersion(ctx, repo.ID)
	v, added, err := e.st.RecordNotesVersion(ctx, store.NotesVersionInput{RepoID: repo.ID, Source: source, PRID: prID, RunID: runID,
		Content: ContentOf(state), Dedupe: true})
	if err != nil {
		e.log.Warn("record a version of the repository notes", "repo", nr.FullName(), "err", err)
		return store.NotesVersion{}, err
	}
	if !added {
		return v, nil
	}
	var before []byte
	var files []notes.File
	if perr == nil {
		if c, err := e.st.NotesVersionContent(ctx, prev.ID); err == nil {
			before = c.Notes
		}
		files = listingOf(prev)
	}
	plus, minus := notes.LineChanges(string(before), string(state.Notes))
	hAdd, hDel, hChg := notes.HarnessDelta(files, state.Listing())
	what := source
	if v.PRNumber > 0 {
		what = fmt.Sprintf("%s of #%d", source, v.PRNumber)
	}
	msg := fmt.Sprintf("notes of %s: version %d (%s), %+d/-%d lines, harness %+d/-%d/~%d", nr.FullName(), v.ID, what, plus, minus,
		len(hAdd), len(hDel), len(hChg))
	data := map[string]any{"version": v.ID, "source": source, "lines_added": plus, "lines_removed": minus, "bytes": v.Bytes,
		"harness_added": names(hAdd), "harness_removed": names(hDel), "harness_changed": names(hChg)}
	if v.PRNumber > 0 {
		data["pr"] = v.PRNumber
	}
	if runID != "" {
		data["run"] = runID
	}
	e.event(ctx, "info", notesSubject(nr), "notes.changed", msg, data)
	if source == store.NotesFromJudge && len(hAdd)+len(hDel)+len(hChg) > 0 {
		e.announceHarness(ctx, repo, nr, v, runID, hAdd, hChg, hDel)
	}
	return v, nil
}

// kindNotesHarness counts the judges' new or changed harness scripts in a
// batch summary.
var kindNotesHarness = notify.Kind{One: "notes harness change", Many: "notes harness changes"}

// harnessToastNames is how many file names a harness toast lists.
const harnessToastNames = 3

// announceHarness tells the operator that a judge's version of the notes
// adds, changes or deletes files of the harness (notes_dir), whose scripts
// later rounds run: one notes.harness_changed event with the repository,
// the PR, the run and the file names (never their content), and, when it
// adds or changes one, one toast that names those ("talkable notes: #12's
// judge added qa/lint.rb"); a deletion is named in the event only.
func (e *Engine) announceHarness(ctx context.Context, repo store.Repo, nr notes.Repo, v store.NotesVersion, runID string, added, changed, removed []string) {
	who := "the judge"
	if v.PRNumber > 0 {
		who = fmt.Sprintf("#%d's judge", v.PRNumber)
	}
	data := map[string]any{"repo": nr.FullName(), "version": v.ID, "added": names(added), "changed": names(changed), "removed": names(removed)}
	if v.PRNumber > 0 {
		data["pr"] = v.PRNumber
	}
	if runID != "" {
		data["run"] = runID
	}
	all := len(added) + len(changed) + len(removed)
	e.event(ctx, "info", notesSubject(nr), "notes.harness_changed", oneLine(fmt.Sprintf("notes of %s: %s %s in the harness",
		nr.FullName(), who, harnessPhrase(all, added, changed, removed)), 2000), data)
	if len(added)+len(changed) == 0 {
		return
	}
	title := oneLine(fmt.Sprintf("%s notes: %s %s", repo.Name, who, harnessPhrase(harnessToastNames, added, changed, nil)), 200)
	e.info(notify.Item{Key: fmt.Sprintf("notes-harness:%d", v.ID), Title: title,
		Body: "Later rounds of " + nr.FullName() + " may run the scripts in " + nr.Harness() + ": read them before they do.",
		Line: title, Kind: kindNotesHarness})
}

// harnessPhrase is "added a, b, changed c, removed d", each list cut to limit
// names and the rest counted ("added a, b and 2 more").
func harnessPhrase(limit int, added, changed, removed []string) string {
	var parts []string
	for i, files := range [][]string{added, changed, removed} {
		if len(files) == 0 {
			continue
		}
		list := strings.Join(files[:min(limit, len(files))], ", ")
		if len(files) > limit {
			list += fmt.Sprintf(" and %d more", len(files)-limit)
		}
		parts = append(parts, []string{"added", "changed", "removed"}[i]+" "+list)
	}
	return strings.Join(parts, ", ")
}

// names is ns, never nil (an event's JSON says [] rather than null).
func names(ns []string) []string {
	if ns == nil {
		return []string{}
	}
	return ns
}

// recordNotesUsage counts the round for every harness file present and
// records the files used: the ones the judge named in harness_used (a name
// relative to the harness directory, or a path under it), and the ones a
// reviewer's report names by their path, each under its run.
func (e *Engine) recordNotesUsage(ctx context.Context, repoID int64, nr notes.Repo, state notes.State, res pipeline.RoundResult) {
	present := make([]string, 0, len(state.Files))
	for _, b := range state.Files {
		present = append(present, b.Path)
	}
	now := e.now()
	if err := e.st.RecordNotesRound(ctx, repoID, present, now); err != nil {
		e.log.Warn("count a round of the repository notes' harness", "repo", nr.FullName(), "err", err)
		return
	}
	used := map[string][]string{}
	add := func(run, file string) {
		if run != "" && !slices.Contains(used[run], file) {
			used[run] = append(used[run], file)
		}
	}
	for _, u := range res.HarnessUsed {
		if f := harnessFile(nr, u, present); f != "" {
			add(res.JudgeRunID, f)
		}
	}
	for _, role := range slices.Sorted(maps.Keys(res.Reports)) {
		rep := res.Reports[role]
		if rep.Path == "" || rep.RunID == "" {
			continue
		}
		text := readPrefix(rep.Path, notesReportMax)
		for _, f := range present {
			if strings.Contains(text, filepath.Join(nr.Harness(), filepath.FromSlash(f))) {
				add(rep.RunID, f)
			}
		}
	}
	for _, run := range slices.Sorted(maps.Keys(used)) {
		if err := e.st.RecordNotesUsage(ctx, repoID, run, used[run], now); err != nil {
			e.log.Warn("record the use of the repository notes' harness", "repo", nr.FullName(), "err", err)
		}
	}
}

// harnessFile maps a name the judge reported to a harness file present now
// ("" = none): a name relative to the harness directory ("./" allowed), or
// a path under it.
func harnessFile(nr notes.Repo, s string, present []string) string {
	s = strings.TrimSpace(s)
	if rel, ok := strings.CutPrefix(s, nr.Harness()+string(filepath.Separator)); ok {
		s = rel
	}
	s = filepath.ToSlash(strings.TrimPrefix(s, "./"))
	if slices.Contains(present, s) {
		return s
	}
	return ""
}

// readPrefix reads at most n bytes of path ("" when it cannot be read).
func readPrefix(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return string(b)
}

// checkNotesLimits compares size with the [notes] limits and keeps the
// repository's mark (KVNotesOver) current: a new or changed set of limits
// passed is a notes.over_limit event, a return within all of them a
// notes.within_limits one. Nothing else follows from it here: the mark is
// what a curation looks for, and no review waits for it.
func (e *Engine) checkNotesLimits(ctx context.Context, nr notes.Repo, size notes.Size) {
	l := e.notesLimits()
	over := size.Over(l)
	key := KVNotesOver(nr.FullName())
	prev, _ := e.getKV(ctx, key)
	cur := strings.Join(over, ",")
	if cur == prev {
		return
	}
	data := map[string]any{"limits": names(over), "bytes": size.Bytes, "lines": size.Lines, "long_lines": size.LongLines,
		"harness_files": size.HarnessFiles, "harness_bytes": size.HarnessBytes}
	if cur == "" {
		e.delKV(ctx, key)
		e.event(ctx, "info", notesSubject(nr), "notes.within_limits", "notes of "+nr.FullName()+" are within their curation triggers again", data)
		return
	}
	e.setKV(ctx, key, cur)
	parts := make([]string, 0, len(over))
	for _, o := range over {
		switch o {
		case notes.LimitBytes:
			parts = append(parts, fmt.Sprintf("max_bytes (%d > %d)", size.Bytes, l.MaxBytes))
		case notes.LimitLine:
			parts = append(parts, fmt.Sprintf("max_line (%d lines over %d characters)", size.LongLines, l.MaxLine))
		case notes.LimitHarnessFiles:
			parts = append(parts, fmt.Sprintf("max_harness_files (%d > %d)", size.HarnessFiles, l.MaxHarnessFiles))
		case notes.LimitHarnessBytes:
			parts = append(parts, fmt.Sprintf("max_harness_bytes (%d > %d)", size.HarnessBytes, l.MaxHarnessBytes))
		}
	}
	e.event(ctx, "info", notesSubject(nr), "notes.over_limit",
		fmt.Sprintf("notes of %s are past their curation triggers: %s; marked for curation", nr.FullName(), strings.Join(parts, ", ")), data)
}

// syncNotes looks at the notes of every repository in the registry: a state
// the registry has no version of is imported (source import: the first
// start, an edit by hand) and the notes are measured against the limits. At
// a reconcile a repository whose round is in its judge stage is left for
// that round to record. A dry run records nothing.
func (e *Engine) syncNotes(ctx context.Context, startup bool) {
	if e.d.DryRun || NotesRoot(e.d.Layout) == "" {
		return
	}
	repos, err := e.st.ListRepos(ctx)
	if err != nil {
		e.log.Warn("repository notes: list repositories", "err", err)
		return
	}
	for _, repo := range repos {
		nr, ok := notesRepo(e.d.Layout, repo.Owner, repo.Name)
		if !ok || (!startup && e.notesJudging(ctx, repo.ID)) {
			continue
		}
		state, err := notes.ReadState(nr)
		if err != nil {
			e.log.Warn("repository notes: read", "repo", nr.FullName(), "err", err)
			e.measureNotes(ctx, nr)
			continue
		}
		if len(state.Notes) == 0 && len(state.Files) == 0 {
			continue // never written (the harness directory every round creates)
		}
		_, _ = e.recordNotes(ctx, repo, nr, state, store.NotesFromImport, 0, "")
		e.checkNotesLimits(ctx, nr, state.Size(e.cfg.Notes.MaxLine))
	}
}

// notesJudging reports whether a round of repoID is between its judge's
// prompt and the recording of what the judge wrote: a PR of it is reviewing
// and its current round (runs created since last_round_started_at) has a
// judge run. A registry error counts as judging: the caller waits.
func (e *Engine) notesJudging(ctx context.Context, repoID int64) bool {
	prs, err := e.st.ListPRs(ctx, store.PRFilter{RepoID: repoID, States: []string{store.PRReviewing}})
	if err != nil {
		return true
	}
	for _, pr := range prs {
		runs, err := e.st.RunsByPR(ctx, pr.ID)
		if err != nil {
			return true
		}
		if slices.ContainsFunc(runs, func(r store.Run) bool {
			return e.isJudge(r.Role) && (pr.LastRoundStartedAt == nil || !r.CreatedAt.Before(*pr.LastRoundStartedAt))
		}) {
			return true
		}
	}
	return false
}
