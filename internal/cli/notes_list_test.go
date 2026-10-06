package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// Notes in four sections, so a judge and a curator can change parts of
// them that do not touch.
const (
	wideNotes = "# Notes for talkable/talkable (updated 2026-10-01)\n\n## Tests\nRun `run_spec.sh`.\n" +
		"- `campaign_snapshot_probes_spec.rb` probes a fix\n\n## Lint\nRun `bin/rubocop`.\n\n## QA\nRun `qa.sh`.\n"
	wideCurated = "# Notes for talkable/talkable (updated 2026-10-06)\n\n## Tests\nRun `run_spec.sh <spec>`.\n\n" +
		"## Lint\nRun `bin/rubocop`.\n\n## QA\nRun `qa.sh`.\n"
	wideJudged = "# Notes for talkable/talkable (updated 2026-10-01)\n\n## Tests\nRun `run_spec.sh`.\n" +
		"- `campaign_snapshot_probes_spec.rb` probes a fix\n\n## Lint\nRun `bin/rubocop`.\n\n## QA\nRun `qa.sh <url>` against the admin.\n"
	wideMerged = "# Notes for talkable/talkable (updated 2026-10-06)\n\n## Tests\nRun `run_spec.sh <spec>`.\n\n" +
		"## Lint\nRun `bin/rubocop`.\n\n## QA\nRun `qa.sh <url>` against the admin.\n"
)

func blob(path, body string) notes.Blob {
	return notes.Blob{Path: path, SHA256: notes.TextSHA([]byte(body)), Body: []byte(body)}
}

// wideProposal stores a curation of wideNotes: the header and the Tests
// section rewritten, the probe and qa.sh deleted.
func (h *notesHarness) wideProposal() store.NotesProposal {
	h.t.Helper()
	base := h.write(wideNotes, map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n", "qa.sh": "open admin\n"})
	bv := h.record(base, store.NotesFromImport)
	proposed := engine.ContentOf(notes.State{Exists: true, Notes: []byte(wideCurated), Files: []notes.Blob{blob("run_spec.sh", "bin/rspec \"$@\"\n")}})
	changes, _ := json.Marshal(notes.Changes{
		Sections: []notes.Change{{Name: "Tests", Action: notes.ActionKept, Reason: "every review runs specs"}},
		Files: []notes.Change{
			{Name: "run_spec.sh", Action: notes.ActionKept, Reason: "runs any spec"},
			{Name: "campaign_snapshot_probes_spec.rb", Action: notes.ActionDeleted, Reason: "one pull request's probe"},
			{Name: "qa.sh", Action: notes.ActionDeleted, Reason: "never used"},
		}})
	p, err := h.st.CreateNotesProposal(h.ctx, store.NotesProposalInput{RepoID: h.repo.ID, Kind: store.ProposalCuration, Trigger: "over_limit",
		BaseVersionID: bv.ID, Proposed: &proposed, Changes: changes, State: store.ProposalPending, Model: "sonnet", At: h.now})
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// A stale proposal whose changes and the notes' merge: --review says it is
// stale before the diff, shows the merge against the notes now (a harness
// file the proposal deletes that a round changed since is kept and named),
// and y applies the merge, recording the notes it replaced first and the
// merge as the curation's version.
func TestNotesReviewOfAStaleProposalAppliesTheCleanMerge(t *testing.T) {
	h := newNotesHarness(t)
	p := h.wideProposal()
	h.write(wideJudged, map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n", "qa.sh": "open admin --url \"$1\"\n"})
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "STALE: the notes changed since the proposal was made",
		"Its changes and theirs merge: below is the merge, as it would change the notes now.",
		"The proposal deletes qa.sh, which a round changed since: kept as the round left it.",
		"--- talkable/talkable notes, now", "+++ talkable/talkable notes, proposal "+itoa(p.ID)+" merged",
		"+Run `run_spec.sh <spec>`.", "  - campaign_snapshot_probes_spec.rb: deleted: one pull request's probe",
		"qa.sh: kept: the proposal deletes it, but a round changed it since", "Apply the merge of proposal "+itoa(p.ID),
		"c asks for a new curation", "applied: the notes of talkable/talkable are version", "merged with the notes' changes since")
	if strings.Index(out, "STALE") > strings.Index(out, "--- talkable/talkable notes") {
		t.Errorf("the review says it is stale after the diff:\n%s", out)
	}
	if strings.Contains(out, "-Run `qa.sh`.") || strings.Contains(out, "+Run `qa.sh`.") {
		t.Errorf("the diff undoes the judge's change:\n%s", out)
	}
	live, err := notes.ReadState(h.nr)
	if err != nil || string(live.Notes) != wideMerged || len(live.Files) != 2 || live.Files[0].Path != "qa.sh" ||
		string(live.Files[0].Body) != "open admin --url \"$1\"\n" || live.Files[1].Path != "run_spec.sh" {
		t.Fatalf("live notes = %q %+v, %v", live.Notes, live.Files, err)
	}
	got := h.proposalNow(p.ID)
	if got.State != store.ProposalApplied || got.AppliedVersionID == nil {
		t.Fatalf("proposal = %+v", got)
	}
	hist, _ := h.st.NotesHistory(h.ctx, h.repo.ID, 0)
	if len(hist) != 3 || hist[0].Source != store.NotesFromCuration || hist[0].ID != *got.AppliedVersionID ||
		hist[1].Source != store.NotesFromImport || hist[1].SHA256 != notes.TextSHA([]byte(wideJudged)) {
		t.Errorf("history = %+v", hist)
	}
}

// --review --json of a stale proposal says so, with the merge.
func TestNotesReviewJSONOfAStaleProposal(t *testing.T) {
	h := newNotesHarness(t)
	p := h.wideProposal()
	h.write(wideJudged, map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n", "qa.sh": "open admin --url \"$1\"\n"})
	if code := h.cmd("notes", "talkable/talkable", "--review", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var data notesReviewData
	if err := json.Unmarshal(h.out.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if !data.Stale || data.Merge == nil || !data.Merge.Clean || len(data.Merge.Kept) != 1 || data.Merge.Kept[0] != "qa.sh" ||
		!strings.Contains(data.Diff, "+Run `run_spec.sh <spec>`.") || strings.Contains(data.Diff, "qa.sh <url>") {
		t.Errorf("json = stale %v merge %+v diff\n%s", data.Stale, data.Merge, data.Diff)
	}
	if got := h.proposalNow(p.ID); got.State != store.ProposalPending {
		t.Errorf("--json decided the proposal: %+v", got)
	}
	// The repository's own view says it too.
	h.errb.Reset()
	if code := h.cmd("notes", "talkable/talkable"); code != 0 {
		t.Fatal(code)
	}
	actContains(t, h.errb.String(), "proposal "+itoa(p.ID)+" (curation) waits for review since",
		"(stale: the notes changed since it was made): `magnum notes talkable/talkable --review`")
}

// A stale proposal whose changes conflict with the notes' says where, and
// y asks the daemon for a new curation that supersedes it; the proposal is
// left to the daemon, which supersedes it when it takes the request.
func TestNotesReviewOfAConflictingStaleProposalAsksForANewCuration(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	h.write(oldNotes+"- a judge's new lesson\n", map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n"})
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "proposal "+itoa(p.ID)+" superseded; notes curation of talkable/talkable started")
	}
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "STALE: the notes changed since the proposal was made",
		"Its changes conflict with theirs in the notes (lines 4-5 of version 1)", "so it cannot be applied",
		"Ask for a new curation of the notes of talkable/talkable? y asks for one from the notes now, which reads proposal "+itoa(p.ID),
		"superseded; notes curation of talkable/talkable started")
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqNotesCurate {
		t.Fatalf("requests = %+v", reqs)
	}
	if pl := actDecode[engine.NotesCuratePayload](t, reqs[0].Payload); pl.Repo != "talkable/talkable" || pl.Supersede != p.ID {
		t.Errorf("payload = %+v", pl)
	}
	if b, _ := os.ReadFile(h.nr.Notes()); !strings.Contains(string(b), "a judge's new lesson") {
		t.Error("the review wrote the notes")
	}
}

// The operator may also answer c to a stale proposal that merges.
func TestNotesReviewCAsksForANewCurationInsteadOfTheMerge(t *testing.T) {
	h := newNotesHarness(t)
	p := h.wideProposal()
	h.write(wideJudged, map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n", "qa.sh": "open admin\n"})
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "queued: starts when the current round's judge stage ends")
	}
	h.terminal("c\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "queued: starts when the current round's judge stage ends")
	if reqs := h.requests(); len(reqs) != 1 || actDecode[engine.NotesCuratePayload](t, reqs[0].Payload).Supersede != p.ID {
		t.Fatalf("requests = %+v", reqs)
	}
	if b, _ := os.ReadFile(h.nr.Notes()); string(b) != wideJudged {
		t.Error("c applied the merge")
	}
}

// The notes changing between the review and the answer refuse the apply
// and leave the proposal waiting: --review again shows it merged.
func TestNotesApplyRefusesWhenTheNotesChangedDuringTheReview(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	shown, err := notes.ReadState(h.nr)
	if err != nil {
		t.Fatal(err)
	}
	h.write(oldNotes+"- a judge's new lesson\n", nil)
	content, _ := h.st.NotesVersionContent(h.ctx, *p.VersionID)
	if _, err := notesApply(context.Background(), h.st, h.nr, p, shown.Fingerprint(), content, false, h.now); !errors.Is(err, errNotesChanged) {
		t.Fatalf("apply = %v", err)
	}
	if got := h.proposalNow(p.ID); got.State != store.ProposalPending {
		t.Errorf("proposal = %+v", got)
	}
	if b, _ := os.ReadFile(h.nr.Notes()); !strings.Contains(string(b), "a judge's new lesson") {
		t.Error("the refused apply wrote the notes")
	}
}

// listHarness has notes in every state: talkable/talkable with a stale
// proposal, example/api past its limits, example/ok within them,
// example/queued with a curation waiting for a judge stage,
// example/running with one running, example/archived only on disk, and
// zhuravel/app in the registry without notes.
func listHarness(t *testing.T) *notesHarness {
	t.Helper()
	h := newNotesHarness(t)
	prev := inspNow
	inspNow = func() time.Time { return h.now }
	t.Cleanup(func() { inspNow = prev })
	h.c.Config.Notes.MaxBytes, h.c.Config.Notes.MaxHarnessFiles = 100, 2
	p := h.proposal()
	h.write(oldNotes+"- a judge's new lesson\n", map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n"})
	judge := h.seedPR("talkable/talkable", 11940, store.PRReviewed)
	if _, _, err := h.st.RecordNotesVersion(h.ctx, store.NotesVersionInput{RepoID: h.repo.ID, Source: store.NotesFromJudge, PRID: judge.ID,
		Content: engine.ContentOf(mustState(t, h.nr))}); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(2 * time.Hour)
	_ = p
	for _, full := range []string{"example/api", "example/ok", "example/queued", "example/running", "zhuravel/app"} {
		h.seedPR(full, 1, store.PRReviewed)
	}
	write := func(owner, name, text string, files map[string]string) {
		notesWrite(t, h.c.Layout, owner, name, text)
		dir := engine.NotesDir(h.c.Layout, owner, name)
		for f, body := range files {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("example", "api", strings.Repeat("x", 150)+"\n"+strings.Repeat("y", 400)+"\n", map[string]string{"a.sh": "a", "b.sh": "b", "c.sh": "c"})
	write("example", "ok", "# Notes\n", nil)
	write("example", "queued", "# Notes\n", nil)
	write("example", "running", "# Notes\n", nil)
	write("example", "archived", "# Notes\n", nil)
	if err := os.MkdirAll(engine.NotesDir(h.c.Layout, "zhuravel", "app"), 0o700); err != nil { // a reviewed repository's empty harness
		t.Fatal(err)
	}
	q, _ := json.Marshal([]engine.CurateQueued{{Repo: "example/queued", Trigger: "request", Why: engine.QueuedJudge, At: h.now.Add(-5 * time.Minute)}})
	m, _ := json.Marshal(engine.CurateMark{Repo: "example/running", Trigger: "misses", Started: h.now.Add(-3 * time.Minute)})
	if err := errors.Join(h.st.SetKV(h.ctx, engine.KVNotesCurateQueue, string(q)), h.st.SetKV(h.ctx, engine.KVNotesCurating, string(m))); err != nil {
		t.Fatal(err)
	}
	return h
}

func mustState(t *testing.T, nr notes.Repo) notes.State {
	t.Helper()
	s, err := notes.ReadState(nr)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// `magnum notes` without a repository lists every repository with notes:
// sizes with the limits they are past marked, who changed them, and their
// state, then one hint per state that asks for something.
func TestNotesWithoutARepositoryListsThemAll(t *testing.T) {
	h := listHarness(t)
	if code := h.cmd("notes"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	row := func(repo string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, repo+" ") {
				return l
			}
		}
		t.Fatalf("no row for %s:\n%s", repo, out)
		return ""
	}
	if !strings.HasPrefix(lines[0], "REPO") || !strings.Contains(lines[0], "NOTES") || !strings.Contains(lines[0], "HARNESS") ||
		!strings.Contains(lines[0], "CHANGED") || !strings.Contains(lines[0], "STATE") {
		t.Errorf("header = %q", lines[0])
	}
	actContains(t, row("talkable/talkable"), "153 B!, 6 lines", "2 files, 21 B", "2h ago, judge #11940",
		"proposal 1 stale (over_limit, 2h; the notes changed since it was made)")
	actContains(t, row("example/api"), "552 B!, 2 lines (1 long!)", "3 files!, 3 B", "not recorded", "over limit (max_bytes, max_line, max_harness_files)")
	actContains(t, row("example/ok"), "8 B, 1 line", "none", "ok")
	actContains(t, row("example/queued"), "curation queued (request, waits for a judge stage)")
	actContains(t, row("example/running"), "curation running (misses, 3m)")
	actContains(t, row("example/archived"), "ok")
	if strings.Contains(out, "zhuravel/app") {
		t.Errorf("a repository without notes is listed:\n%s", out)
	}
	actContains(t, out, "! past a [notes] limit: a curation trigger, not a cap",
		"review: magnum notes talkable/talkable --review (stale: it merges the notes' changes since, or asks for a new curation)",
		"curate: magnum notes example/api --curate")
	if strings.Contains(out, "magnum notes example/ok") || strings.Contains(out, "magnum notes example/queued") {
		t.Errorf("a hint for a state that asks for nothing:\n%s", out)
	}
}

// --json prints the same rows; no repository with notes is a note on
// stderr and an empty list.
func TestNotesListJSONAndNoRepositories(t *testing.T) {
	h := listHarness(t)
	if code := h.cmd("notes", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var out notesListJSON
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, r := range out.Repos {
		states[r.Repo] = r.State
	}
	want := map[string]string{"example/api": notesOver, "example/archived": notesOK, "example/ok": notesOK, "example/queued": notesQueued,
		"example/running": notesCurating, "talkable/talkable": notesStale}
	if len(states) != len(want) || out.Limits.MaxBytes != 100 {
		t.Fatalf("json = %+v", out)
	}
	for k, v := range want {
		if states[k] != v {
			t.Errorf("%s: state %q, want %q", k, states[k], v)
		}
	}
	if r := out.Repos[len(out.Repos)-1]; r.Repo != "talkable/talkable" || r.Proposal == nil || !r.Proposal.Stale || r.Changed == nil ||
		r.Changed.Source != store.NotesFromJudge || r.Changed.PRNumber != 11940 {
		t.Errorf("talkable/talkable = %+v", r)
	}

	c, out2, errb := bareContext(t)
	if code := execute(c, []string{"notes"}); code != 0 || out2.Len() != 0 || !strings.Contains(errb.String(), "no repository has notes yet") {
		t.Errorf("no notes: exit %d, stdout %q, stderr %q", code, out2, errb)
	}
	c, out2, _ = bareContext(t)
	if code := execute(c, []string{"notes", "--json"}); code != 0 || !strings.Contains(out2.String(), `"repos": []`) {
		t.Errorf("no notes --json: exit %d, %s", code, out2)
	}
}

// The status line sums the list up, leaving out what is zero.
func TestNotesSummaryLine(t *testing.T) {
	h := listHarness(t)
	limits, _ := notesLimits(h.c)
	rows, err := notesOverview(h.ctx, h.c.Layout, h.st, limits, h.now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := notesSummaryLine(rows), "6 repos · 1 proposal to review (talkable/talkable stale) · 2 over limit · curating example/running · "+
		"1 curation queued (example/queued)"; got != want {
		t.Errorf("line = %q\nwant   %q", got, want)
	}
	if got := notesSummaryLine([]notesRow{{Repo: "example/ok", State: notesOK}}); got != "1 repo" {
		t.Errorf("one repository = %q", got)
	}
	if got := notesSummaryLine(nil); got != "" {
		t.Errorf("no repositories = %q", got)
	}
}

// Completion names the waiting proposal of a repository with notes.
func TestNotesCompletionNamesAWaitingProposal(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	got := complete(t, h.c, "notes", "")
	want := "talkable/talkable\trepository notes, proposal " + itoa(p.ID) + " waits for review"
	if len(got) != 2 || got[0] != want || !strings.HasPrefix(got[1], "talkable\t") {
		t.Errorf("completions = %q", got)
	}
}

// `magnum status` sums the notes up in one line, and the dashboard shows
// the same; without a repository with notes the line is left out.
func TestStatusNotesLine(t *testing.T) {
	f, st, d, _ := statusFixture(t)
	r, out := statusRetroOut(t, d)
	if r.Notes != nil || strings.Contains(out, "notes:") {
		t.Fatalf("a notes line without notes: %+v\n%s", r.Notes, out)
	}
	big := strings.Repeat("x", 20000) + "\n"
	notesWrite(t, f.Ctx.Layout, "talkable", "talkable", big)
	notesWrite(t, f.Ctx.Layout, "example", "api", "# Notes\n")
	ctx := context.Background()
	repo, err := st.RepoByFullName(ctx, "talkable/talkable")
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := st.RecordNotesVersion(ctx, store.NotesVersionInput{RepoID: repo.ID, Source: store.NotesFromImport,
		Content: store.NotesContent{Notes: []byte(big)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateNotesProposal(ctx, store.NotesProposalInput{RepoID: repo.ID, Kind: store.ProposalRestore,
		Trigger: "restore", BaseVersionID: v.ID, VersionID: v.ID, State: store.ProposalPending}); err != nil {
		t.Fatal(err)
	}
	r, out = statusRetroOut(t, d)
	const line = "2 repos · 1 proposal to review (talkable/talkable) · 1 over limit"
	actContains(t, out, "notes:    "+line+"\n")
	if r.Notes == nil || len(r.Notes.Repos) != 2 {
		t.Fatalf("notes = %+v", r.Notes)
	}
	if got := statusDashData(r, "talkable/talkable").Notes; got != line {
		t.Errorf("dashboard notes = %q", got)
	}
}
