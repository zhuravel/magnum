package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/notes"
	"github.com/zhuravel/magnum/internal/store"
)

// notesHarness is an act harness with talkable/talkable in the registry and
// its notes on disk.
type notesHarness struct {
	*actHarness
	repo store.Repo
	nr   notes.Repo
}

func newNotesHarness(t *testing.T) *notesHarness {
	t.Helper()
	h := newActHarness(t)
	pr := h.seedPR("talkable/talkable", 11920, store.PRReviewed)
	repo, err := h.st.RepoByID(h.ctx, pr.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	nr, _ := notes.RepoOf(engine.NotesPath(h.c.Layout, "talkable", "talkable"))
	return &notesHarness{actHarness: h, repo: repo, nr: nr}
}

// write puts the notes and harness on disk.
func (h *notesHarness) write(text string, files map[string]string) notes.State {
	h.t.Helper()
	if err := os.RemoveAll(h.nr.Harness()); err != nil {
		h.t.Fatal(err)
	}
	if err := os.MkdirAll(h.nr.Harness(), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(h.nr.Notes(), []byte(text), 0o600); err != nil {
		h.t.Fatal(err)
	}
	for name, body := range files {
		p := filepath.Join(h.nr.Harness(), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			h.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o700); err != nil {
			h.t.Fatal(err)
		}
	}
	s, err := notes.ReadState(h.nr)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

// record records s as a version of source.
func (h *notesHarness) record(s notes.State, source string) store.NotesVersion {
	h.t.Helper()
	v, _, err := h.st.RecordNotesVersion(h.ctx, store.NotesVersionInput{RepoID: h.repo.ID, Source: source, Content: engine.ContentOf(s)})
	if err != nil {
		h.t.Fatal(err)
	}
	h.now = h.now.Add(time.Hour)
	return v
}

const (
	oldNotes = "# Notes for talkable/talkable (updated 2026-10-01)\n\n## Tests\nRun `run_spec.sh`.\n- `campaign_snapshot_probes_spec.rb` probes a fix\n"
	newNotes = "# Notes for talkable/talkable (updated 2026-10-06)\n\n## Tests\nRun `run_spec.sh <spec>`.\n"
)

// proposal stores a curation proposal from the notes on disk (the base)
// to newNotes with run_spec.sh alone.
func (h *notesHarness) proposal() store.NotesProposal {
	h.t.Helper()
	base := h.write(oldNotes, map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n"})
	bv := h.record(base, store.NotesFromImport)
	proposed := engine.ContentOf(notes.State{Exists: true, Notes: []byte(newNotes),
		Files: []notes.Blob{{Path: "run_spec.sh", SHA256: notes.TextSHA([]byte("bin/rspec \"$@\"\n")), Body: []byte("bin/rspec \"$@\"\n")}}})
	changes, _ := json.Marshal(notes.Changes{
		Sections: []notes.Change{{Name: "Tests", Action: notes.ActionKept, Reason: "every review runs specs"}},
		Files: []notes.Change{
			{Name: "run_spec.sh", Action: notes.ActionKept, Reason: "runs any spec with the checkout's Ruby"},
			{Name: "campaign_snapshot_probes_spec.rb", Action: notes.ActionDeleted, Reason: "one pull request's probe"},
		}})
	p, err := h.st.CreateNotesProposal(h.ctx, store.NotesProposalInput{RepoID: h.repo.ID, Kind: store.ProposalCuration, Trigger: "over_limit",
		BaseVersionID: bv.ID, Proposed: &proposed, Changes: changes, State: store.ProposalPending, Model: "sonnet", At: h.now})
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (h *notesHarness) terminal(answer string) {
	h.d.StdinTTY = true
	h.stdin(answer)
}

func (h *notesHarness) proposalNow(id int64) store.NotesProposal {
	h.t.Helper()
	p, err := h.st.NotesProposalByID(h.ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

// --review shows the proposal (the notes' diff, the harness changes with
// the curator's reasons, the sizes) and y applies it under the notes lock:
// the live notes and harness become the proposal's, the registry records
// the applied version and links it, and the lock is released.
func TestNotesReviewYAppliesTheProposalUnderTheLock(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	out := h.out.String()
	actContains(t, out, "proposal "+itoa(p.ID)+" for the notes of talkable/talkable: a curation (over_limit) by sonnet",
		"notes: 130 → 87 bytes, 5 → 4 lines; harness: 2 → 1 files",
		"-- `campaign_snapshot_probes_spec.rb` probes a fix", "+Run `run_spec.sh <spec>`.",
		"  - campaign_snapshot_probes_spec.rb: deleted: one pull request's probe",
		"    run_spec.sh: kept: runs any spec with the checkout's Ruby",
		"  Tests: kept: every review runs specs", "[y/N]", "applied: the notes of talkable/talkable are version")
	if strings.Contains(out, "\x1b[") {
		t.Errorf("colors on a pipe:\n%s", out)
	}
	live, err := notes.ReadState(h.nr)
	if err != nil || string(live.Notes) != newNotes || len(live.Files) != 1 || live.Files[0].Path != "run_spec.sh" {
		t.Fatalf("live notes = %q %+v, %v", live.Notes, live.Files, err)
	}
	got := h.proposalNow(p.ID)
	if got.State != store.ProposalApplied || got.AppliedVersionID == nil {
		t.Fatalf("proposal = %+v", got)
	}
	hist, _ := h.st.NotesHistory(h.ctx, h.repo.ID, 0)
	if len(hist) != 2 || hist[0].ID != *got.AppliedVersionID || hist[0].Source != store.NotesFromCuration {
		t.Errorf("history = %+v", hist)
	}
	if _, err := os.Stat(h.nr.Lock()); !os.IsNotExist(err) {
		t.Errorf("the notes lock is still held: %v", err)
	}
}

// The notes changed since the proposal was made (a judge wrote them): y
// refuses, and the proposal expires.
func TestNotesReviewRefusesWhenTheNotesChangedSinceTheProposal(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	h.write(oldNotes+"- a judge's new lesson\n", map[string]string{"run_spec.sh": "bin/rspec \"$@\"\n", "campaign_snapshot_probes_spec.rb": "probe\n"})
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	actContains(t, h.errb.String(), "notes changed since the proposal; run --curate again")
	if b, _ := os.ReadFile(h.nr.Notes()); !strings.Contains(string(b), "a judge's new lesson") {
		t.Error("the refused apply wrote the notes")
	}
	if got := h.proposalNow(p.ID); got.State != store.ProposalExpired || got.Reason != "the notes changed since the proposal" {
		t.Errorf("proposal = %+v", got)
	}
}

// n rejects the proposal with --reason, which the registry keeps for the
// next curation; any other answer leaves it waiting.
func TestNotesReviewNStoresTheReasonAndOtherAnswersWait(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	h.terminal("\n")
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "left for later")
	if got := h.proposalNow(p.ID); got.State != store.ProposalPending {
		t.Fatalf("an empty answer decided the proposal: %+v", got)
	}
	h.out.Reset()
	h.terminal("n\n")
	if code := h.cmd("notes", "talkable/talkable", "--review", "--reason", "keep the QA section"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "rejected proposal "+itoa(p.ID)+"; the next curation reads why")
	if got := h.proposalNow(p.ID); got.State != store.ProposalRejected || got.Reason != "keep the QA section" {
		t.Errorf("proposal = %+v", got)
	}
	if b, _ := os.ReadFile(h.nr.Notes()); string(b) != oldNotes {
		t.Error("a rejection changed the notes")
	}
}

// --review asks on a terminal only; --json prints the data for scripts and
// decides nothing.
func TestNotesReviewJSONAndTerminal(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	if code := h.cmd("notes", "talkable/talkable", "--review"); code != 2 {
		t.Fatalf("not a terminal: exit %d", code)
	}
	actContains(t, h.errb.String(), "--review asks y/N on a terminal")
	if code := h.cmd("notes", "talkable/talkable", "--review", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	var data struct {
		Proposal store.NotesProposal `json:"proposal"`
		Diff     string              `json:"notes_diff"`
		Harness  []notesHarnessChange
		After    notes.Size `json:"size_after"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &data); err != nil {
		t.Fatalf("%v\n%s", err, h.out.String())
	}
	if data.Proposal.ID != p.ID || !strings.Contains(data.Diff, "+Run `run_spec.sh <spec>`.") || data.After.HarnessFiles != 1 || len(data.Harness) != 2 {
		t.Errorf("json = %+v", data)
	}
	if got := h.proposalNow(p.ID); got.State != store.ProposalPending {
		t.Errorf("--json decided the proposal: %+v", got)
	}
}

// --log lists every version, newest first, with its source, PR, size and
// harness changes; --diff shows what changed since a version back.
func TestNotesLogAndDiff(t *testing.T) {
	h := newNotesHarness(t)
	v1 := h.record(h.write(oldNotes, map[string]string{"run_spec.sh": "x"}), store.NotesFromImport)
	pr, _ := h.st.PRByRepoNumber(h.ctx, h.repo.ID, 11920)
	s2 := h.write(newNotes, map[string]string{"run_spec.sh": "x", "lint.sh": "bin/rubocop\n"})
	v2, _, err := h.st.RecordNotesVersion(h.ctx, store.NotesVersionInput{RepoID: h.repo.ID, Source: store.NotesFromJudge, PRID: pr.ID, RunID: "r-1",
		Content: engine.ContentOf(s2)})
	if err != nil {
		t.Fatal(err)
	}
	if code := h.cmd("notes", "talkable/talkable", "--log"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	lines := strings.Split(strings.TrimSpace(h.out.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[1], itoa(v2.ID)+" ") || !strings.Contains(lines[1], "judge") || !strings.Contains(lines[1], "#11920") ||
		!strings.Contains(lines[1], "2 files, 13 bytes (+1)") || !strings.Contains(lines[2], "import") {
		t.Fatalf("--log:\n%s", h.out.String())
	}
	h.out.Reset()
	if code := h.cmd("notes", "talkable/talkable", "--log", "--json"); code != 0 {
		t.Fatal(code)
	}
	var entries []notesLogEntry
	if err := json.Unmarshal(h.out.Bytes(), &entries); err != nil || len(entries) != 2 || entries[0].HarnessAdded[0] != "lint.sh" {
		t.Fatalf("--log --json = %+v, %v", entries, err)
	}

	// The notes now are version 2: one version back is version 1.
	h.out.Reset()
	if code := h.cmd("notes", "talkable/talkable", "--diff"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "--- talkable/talkable notes, version "+itoa(v1.ID)+" (", "+++ talkable/talkable notes, now",
		"-- `campaign_snapshot_probes_spec.rb` probes a fix", "harness:\n  + lint.sh")
	if code := h.cmd("notes", "talkable/talkable", "--diff", "2"); code != 1 {
		t.Fatalf("two back of two versions: exit %d", code)
	}
	actContains(t, h.errb.String(), "have 1 earlier version(s)")
}

// --restore proposes a recorded version back through the same review: y
// writes it, and the registry records the restore as the operator's.
// Restoring the version after it brings the notes back again.
func TestNotesRestoreRoundTrips(t *testing.T) {
	h := newNotesHarness(t)
	v1 := h.record(h.write(oldNotes, map[string]string{"run_spec.sh": "x", "campaign_snapshot_probes_spec.rb": "probe\n"}), store.NotesFromImport)
	v2 := h.record(h.write(newNotes, map[string]string{"run_spec.sh": "x"}), store.NotesFromCuration)

	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--restore", itoa(v1.ID)); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), "a restore of version "+itoa(v1.ID), "+- `campaign_snapshot_probes_spec.rb` probes a fix", "applied")
	live, _ := notes.ReadState(h.nr)
	if string(live.Notes) != oldNotes || len(live.Files) != 2 {
		t.Fatalf("after the restore: %q %+v", live.Notes, live.Files)
	}
	hist, _ := h.st.NotesHistory(h.ctx, h.repo.ID, 0)
	if hist[0].Source != store.NotesFromHuman || hist[0].SHA256 != v1.SHA256 {
		t.Errorf("latest version = %+v", hist[0])
	}

	h.out.Reset()
	h.terminal("y\n")
	if code := h.cmd("notes", "talkable/talkable", "--restore", itoa(v2.ID)); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if live, _ := notes.ReadState(h.nr); string(live.Notes) != newNotes || len(live.Files) != 1 {
		t.Fatalf("after restoring back: %q %+v", live.Notes, live.Files)
	}
	if code := h.cmd("notes", "talkable/talkable", "--restore", itoa(v2.ID)); code != 0 || !strings.Contains(h.errb.String(), "hold version "+itoa(v2.ID)+" already") {
		t.Errorf("restoring what the notes hold: exit %d %s", code, h.errb.String())
	}
	if code := h.cmd("notes", "talkable/talkable", "--restore", "999"); code != 1 || !strings.Contains(h.errb.String(), "version 999 is not a version") {
		t.Errorf("an unknown version: exit %d %s", code, h.errb.String())
	}
}

// --curate hands the request to the daemon and prints its answer.
func TestNotesCurateQueuesTheRequest(t *testing.T) {
	h := newNotesHarness(t)
	h.pid = 4242
	h.onSleep = func(h *actHarness) {
		h.completePending(store.RequestDone, "notes curation of talkable/talkable started")
	}
	if code := h.cmd("notes", "talkable", "--curate"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 || reqs[0].Kind != engine.ReqNotesCurate {
		t.Fatalf("requests = %+v", reqs)
	}
	if p := actDecode[engine.NotesCuratePayload](t, reqs[0].Payload); p.Repo != "talkable/talkable" {
		t.Errorf("payload = %+v", p)
	}
	actContains(t, h.out.String(), "notes curation of talkable/talkable started")
}

// The plain view names the triggers passed, the unused harness files and
// a waiting proposal; --json carries the same.
func TestNotesShowsTriggersUnusedFilesAndAProposal(t *testing.T) {
	h := newNotesHarness(t)
	h.c.Config.Notes.MaxBytes = 100
	p := h.proposal()
	for range store.NotesUnusedRounds {
		if err := h.st.RecordNotesRound(h.ctx, h.repo.ID, []string{"campaign_snapshot_probes_spec.rb", "run_spec.sh"}, h.now); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.st.RecordNotesUsage(h.ctx, h.repo.ID, "r-1", []string{"run_spec.sh"}, h.now); err != nil {
		t.Fatal(err)
	}
	if code := h.cmd("notes", "talkable/talkable"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	if h.out.String() != oldNotes {
		t.Errorf("stdout = %q", h.out.String())
	}
	actContains(t, h.errb.String(), "notes: 130 bytes (max_bytes 100: past)", "past max_bytes, curation triggers, not caps",
		"unused harness files (20 rounds or more, no recorded use): campaign_snapshot_probes_spec.rb",
		"proposal "+itoa(p.ID)+" (curation) waits for review")
	h.out.Reset()
	if code := h.cmd("notes", "talkable/talkable", "--json"); code != 0 {
		t.Fatal(code)
	}
	var v notesView
	if err := json.Unmarshal(h.out.Bytes(), &v); err != nil || v.Proposal == nil || v.Proposal.ID != p.ID || len(v.Unused) != 1 || v.Over[0] != "max_bytes" {
		t.Errorf("--json = %+v, %v", v, err)
	}
}

// --edit records what the editor left as the operator's version.
func TestNotesEditRecordsAHumanVersion(t *testing.T) {
	h := newNotesHarness(t)
	h.write(oldNotes, nil)
	prev := notesRunEditor
	notesRunEditor = func(_ *Context, _ string, path string) error { return os.WriteFile(path, []byte(newNotes), 0o600) }
	t.Cleanup(func() { notesRunEditor = prev })
	if code := h.cmd("notes", "talkable/talkable", "--edit"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	hist, _ := h.st.NotesHistory(h.ctx, h.repo.ID, 0)
	if len(hist) != 1 || hist[0].Source != store.NotesFromHuman || hist[0].SHA256 != notes.TextSHA([]byte(newNotes)) {
		t.Fatalf("history = %+v", hist)
	}
	actContains(t, h.errb.String(), "recorded as version")
}

func TestNotesFlagsGoOneAtATime(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--log", "--review"}, "one at a time"},
		{[]string{"--reason", "x"}, "--reason goes with --review or --restore"},
		{[]string{"--diff=0"}, "want a positive number of versions back"},
		{[]string{"--edit", "--json"}, "--json does not go with --edit or --diff"},
	} {
		c, _, errb := bareContext(t)
		if code := execute(c, append([]string{"notes", "talkable/talkable"}, tc.args...)); code != 2 || !strings.Contains(errb.String(), tc.want) {
			t.Errorf("%v: exit %d %s", tc.args, code, errb)
		}
	}
}

// notesApply holds the lock while it works: a judge holding it makes the
// apply wait, then give up.
func TestNotesApplyWaitsForTheJudgesLock(t *testing.T) {
	h := newNotesHarness(t)
	p := h.proposal()
	if err := os.Mkdir(h.nr.Lock(), 0o700); err != nil {
		t.Fatal(err)
	}
	prev := notesLockWait
	notesLockWait = 0
	t.Cleanup(func() { notesLockWait = prev })
	if _, err := notesApply(context.Background(), h.st, h.nr, p, h.now); !errors.Is(err, notes.ErrBusy) {
		t.Fatalf("apply under a held lock: %v", err)
	}
	if got := h.proposalNow(p.ID); got.State != store.ProposalPending {
		t.Errorf("proposal = %+v", got)
	}
}
