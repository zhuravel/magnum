package fixwork

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// The fix loop's registry rows (DECISIONS "The fix loop moves into the
// factory"): each record that the engine writes and the factory reads with
// the same types once the fix loop moves. testdata/records holds each one
// as the build before the move wrote it (json.MarshalIndent of the values
// below, by the engine's types at 309999fb): a set one, each field given,
// and, for a struct, its zero value, which shows the fields always written.
// Never write these files again: a changed JSON name, omitempty or omitzero
// must fail here.

// fixRecordAt is the time of every record below.
var fixRecordAt = time.Date(2026, 11, 12, 14, 20, 53, 0, time.UTC)

// fixRecordState is a fix loop's row with each field set.
func fixRecordState() FixState {
	return FixState{
		Cycle: 2, Cap: 5, Phase: FixPhaseFixing, Since: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1",
		Head: "e6c1f37000000000000000000000000000000000", By: "autofix", Report: "/reviews/talkable/2/fix-1.md",
		StartedAt: fixRecordAt, NeedsYou: []string{"- 4100: a call (needs: decision)"}, Answered: []string{"4100"},
		Role: "claude-fix", ForcedAt: fixRecordAt.Add(time.Minute), Held: true, Context: "compacted 82% → 31%",
		OthersUnread: "not read (HTTP 502)",
		Carried:      []FixCarry{{Report: "/reviews/talkable/2/fix-0.md", NeedsYou: []string{"- 4000: a call"}, Answered: []string{"4000"}}},
		Waits:        []string{"thread:4200@4201"}, Mode: "ci", CIChecks: []string{"rspec"},
		ParentRefused: "needs Workflows on the App", Confirm: &FixConfirm{Head: "e6c1f37000000000000000000000000000000000", Since: fixRecordAt},
		Late: true,
	}
}

// fixRecords are the records and the files that hold them.
func fixRecords() map[string]any {
	merge := &PushMerge{Base: "master", SHA: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2", Paths: []string{".github/workflows/ci.yml"}}
	thread := Item{Key: "thread:4100@4101", Class: ClassPerson, Kind: ItemThread, URL: "https://github.com/talkable/talkable/pull/2#discussion_r4100",
		FixerReplies: 1, Resolved: true, Decision: true, By: "rev-ann", ChangesAt: fixRecordAt, Accepted: true}
	baseCheck := CIBaseCheck{Name: "Completion", SHA: "c3c3c3c3", State: CIBaseFailed,
		Each: []CIBaseCheck{{Name: "rspec", SHA: "c3c3c3c3", State: CIBasePassed}, {Name: "jest", State: CIBaseNever}}}
	return map[string]any{
		"fix.json":      fixRecordState(),
		"fix-zero.json": FixState{},
		"fix_off.json":  FixOff{Since: fixRecordAt, By: "magnum fix --off"},
		"fix_push.json": Push{Kind: PushUnconfirmed, Run: "fix-r1", Pushed: "e6c1f37000000000000000000000000000000000",
			Commits: []string{"e6c1f37"}, Since: fixRecordAt, Failed: fixRecordAt.Add(3 * time.Minute), Typed: true, RepliesNamed: true,
			ConversationNamed: true, Job: PushJob{State: fixRecordState(), Role: "claude-fix", Head: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1",
				Started: fixRecordAt, Login: "zhuravel-factory[bot]", Mode: "merge", Conflict: merge, Merged: merge, ForMerge: true, Alone: true,
				Described: true, BaseChecked: true}},
		"fix_push-zero.json": Push{},
		"held_merge.json": HeldMerge{Repo: "talkable/talkable", Number: 2, Merge: "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4",
			Head: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", Base: "master", Tip: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2",
			Workflows: []string{".github/workflows/ci.yml"}, Conflicts: 1, Role: "claude-fix", Identity: "zhuravel-factory",
			Login: "zhuravel-factory[bot]", Clone: "/pool/talkable-1", Diff: "/reviews/talkable/2/held-merge.diff", Findings: true,
			Attention: "the merge of master waits for your y", At: fixRecordAt},
		"held_merge-zero.json": HeldMerge{},
		"pushed.json":          CIPush{Head: "e6c1f37000000000000000000000000000000000", At: fixRecordAt},
		"fix_held.json":        HeldFix{At: fixRecordAt, Head: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", By: "magnum fix", Why: HeldPause, Kind: "claude"},
		"fix_held-zero.json":   HeldFix{},
		"fix_seen.json": Ledger{
			Handed: map[string]time.Time{"thread:4100@4101": fixRecordAt}, Reviews: []string{"review:r1"},
			FixerHeads: []string{"e6c1f37000000000000000000000000000000000"}, Cursor: 42, Since: fixRecordAt,
			RaisedMsg: "2 items need you", Raised: []string{"thread:4100@4101"}, Starts: []time.Time{fixRecordAt},
			Read: LedgerRead{Print: "p1", At: fixRecordAt, Head: "a1a1a1a1", Cut: []string{"threads"}, Others: []Item{thread},
				Mine: []MagnumThread{{RootID: 5100, URL: "https://github.com/talkable/talkable/pull/2#discussion_r5100", ReviewID: 101,
					LatestID: 5101, LatestAt: fixRecordAt, Rebuttal: true, Answered: true, AnsweredAt: fixRecordAt}},
				FailedAt: fixRecordAt},
			Conflict:  LedgerConflict{Head: "a1a1a1a1", Base: "master", Tip: "b2b2b2b2", At: fixRecordAt, Paths: []string{"app.go"}, Unchecked: true},
			ReadFails: 1, LateToasted: "a1a1a1a1", Approval: ApprovalCount{SHA: "a0a0a0a0", Head: "a1a1a1a1", After: 2},
			Refused: "fp1", RefusedWhy: "nothing to fix", WaitAt: fixRecordAt, WaitWhy: "the slot is busy",
			Spell: Spell{Start: fixRecordAt, Seen: fixRecordAt.Add(time.Minute)}, Waited: map[string]time.Time{"rev-ann": fixRecordAt},
		},
		"fix_seen-zero.json": Ledger{},
		"ci_fix.json": CIFixRecord{
			Starts: []CIFixStart{{Head: "a1a1a1a1", Entries: []string{"rspec"}, At: fixRecordAt, By: "autofix", Outcome: CIOutcomeCannot,
				Why: "flaky", Pushed: "e6c1f37"}},
			Read: CIFailRead{Head: "a1a1a1a1", Key: "rspec@1", At: fixRecordAt, File: "/reviews/talkable/2/ci-failures.json", Digest: "sha256:00",
				Jobs: 2, Steps: 3, Lines: 120, Bytes: 4096, Unread: true, UnreadWhy: "needs Actions on the App", Running: true,
				Runs: []CIReadRun{{ID: 42673098082, Attempt: 2}}, Checked: fixRecordAt},
			Base: CIBaseRead{Head: "a1a1a1a1", Key: "rspec@1", Tip: "b2b2b2b2", Checks: []CIBaseCheck{baseCheck}, At: fixRecordAt, Failed: true},
		},
		"ci_fix-zero.json": CIFixRecord{},
		"ci_dispatch.json": CIStart{Head: "a1a1a1a1", Workflow: "ci.yml", At: fixRecordAt, Failed: "GitHub answered HTTP 422",
			Refused: "a pause", Run: &CIRun{ID: 42673098082, Status: "completed", Conclusion: "failure"}, Moved: fixRecordAt},
		"ci_dispatch-zero.json": CIStart{},
		"ci_rerun.json": CIRerun{Head: "a1a1a1a1", ReadAt: fixRecordAt, Queued: 42673098083, QueuedStatus: "queued", Unread: true,
			UnreadWhy: "needs Actions on the App", Run: &CINoJob{ID: 42673098082, Workflow: "ci.yml", Attempt: 1, Conclusion: "failure"},
			At: fixRecordAt, Failed: "GitHub answered HTTP 403", Refused: "a pause",
			Seen: &CIAttempt{Attempt: 2, Status: "completed", Conclusion: "failure", NoJob: true}, Moved: fixRecordAt},
		"ci_rerun-zero.json":  CIRerun{},
		"fix_calls.json":      map[string]time.Time{"4100": fixRecordAt, "5100": fixRecordAt.Add(time.Minute)},
		"optional_calls.json": []OptionalCall{{Thread: 5200, URL: "https://github.com/talkable/talkable/pull/2#discussion_r5200", At: fixRecordAt}},
		"open.json": OpenVerdict{Verdict: OpenNeedsYou, Why: "1 thread of a person needs you", Head: "a1a1a1a1", At: fixRecordAt,
			Since: fixRecordAt, ReadAt: fixRecordAt, Until: fixRecordAt.Add(time.Hour), Open: 1,
			Needs: []OpenNeed{{Kind: "thread", URL: "https://github.com/talkable/talkable/pull/2#discussion_r4100"}}, Approved: "a0a0a0a",
			AfterApproval: 2, Off: OpenOffDraft, Optional: []string{"https://github.com/talkable/talkable/pull/2#discussion_r5200"}},
		"open-zero.json": OpenVerdict{},
	}
}

// Every fix record decodes from the file the build before the move wrote,
// into the value it was written from, with no field left over, and writes
// the same bytes again.
func TestFixRecordsWrittenBeforeTheMoveStillDecode(t *testing.T) {
	records := fixRecords()
	files, err := filepath.Glob(filepath.Join("testdata", "records", "*.json"))
	if err != nil || len(files) != len(records) {
		t.Fatalf("testdata/records holds %d files (%v), want %d", len(files), err, len(records))
	}
	for name, want := range records {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "records", name))
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(want, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if got = append(got, '\n'); !bytes.Equal(got, raw) {
				t.Errorf("this build writes\n%s\nthe build before the move wrote\n%s", got, raw)
			}
			back := reflect.New(reflect.TypeOf(want))
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(back.Interface()); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !reflect.DeepEqual(back.Elem().Interface(), want) {
				t.Errorf("decoded\n%+v\nwant\n%+v", back.Elem().Interface(), want)
			}
		})
	}
}

// The CLI reads the loop's rows with ParseFixState and ParseFixOff: a value
// that is no loop or no opt-out reads as none, and the board's cell for a
// fixer at work names the cycle, the cap and the role.
func TestParseFixStateAndItsShortForm(t *testing.T) {
	for _, raw := range []string{"", "not json", `{"cycle":0,"cap":3,"phase":"fixing"}`, `{"cycle":1,"cap":3}`} {
		if st, ok := ParseFixState(raw); ok {
			t.Errorf("ParseFixState(%q) = %+v, want none", raw, st)
		}
	}
	st, ok := ParseFixState(`{"cycle":2,"cap":3,"phase":"fixing","role":"claude-fix","by":"autofix"}`)
	if !ok || st.Cycle != 2 || st.Phase != FixPhaseFixing || st.By != "autofix" {
		t.Fatalf("ParseFixState = %+v, %v", st, ok)
	}
	if got := st.Short(); got != "fixing 2/3 · claude-fix" {
		t.Errorf("Short = %q", got)
	}
	st.Role = ""
	if got := st.Short(); got != "fixing 2/3" {
		t.Errorf("Short without a role = %q", got)
	}
	if o, ok := ParseFixOff(`{"since":"2026-10-10T09:30:15Z","by":"magnum fix --off"}`); !ok || o.By != "magnum fix --off" || o.Since.IsZero() {
		t.Errorf("ParseFixOff = %+v, %v", o, ok)
	}
	if _, ok := ParseFixOff("not json"); ok {
		t.Error("ParseFixOff read junk as an opt-out")
	}
}
