package engine

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
// factory"): the records of plan section 3 that the factory reads with the
// same types once the fix loop moves. testdata/fixrecords holds each one
// as the build before the move wrote it (json.MarshalIndent of the values
// below, at 309999fb): a set one, each field given, and, for a struct, its
// zero value, which shows the fields always written. Never write these
// files again: a changed JSON name, omitempty or omitzero must fail here.

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
		Waits:        []string{"thread:4200@4201"}, Mode: FixModeCI, CIChecks: []string{"rspec"},
		ParentRefused: "needs Workflows on the App", Confirm: &FixConfirm{Head: "e6c1f37000000000000000000000000000000000", Since: fixRecordAt},
		Late: true,
	}
}

// fixRecords are the records and the files that hold them.
func fixRecords() map[string]any {
	merge := &fixPushMerge{Base: "master", SHA: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2", Paths: []string{".github/workflows/ci.yml"}}
	thread := openItem{Key: "thread:4100@4101", Class: classPerson, Kind: itemThread, URL: "https://github.com/talkable/talkable/pull/2#discussion_r4100",
		FixerReplies: 1, Resolved: true, Decision: true, By: "rev-ann", ChangesAt: fixRecordAt, Accepted: true}
	baseCheck := ciBaseCheck{Name: "Completion", SHA: "c3c3c3c3", State: ciBaseFailed,
		Each: []ciBaseCheck{{Name: "rspec", SHA: "c3c3c3c3", State: ciBasePassed}, {Name: "jest", State: ciBaseNever}}}
	return map[string]any{
		"fix.json":      fixRecordState(),
		"fix-zero.json": FixState{},
		"fix_off.json":  FixOff{Since: fixRecordAt, By: "magnum fix --off"},
		"fix_push.json": fixPush{Kind: fixPushUnconfirmed, Run: "fix-r1", Pushed: "e6c1f37000000000000000000000000000000000",
			Commits: []string{"e6c1f37"}, Since: fixRecordAt, Failed: fixRecordAt.Add(3 * time.Minute), Typed: true, RepliesNamed: true,
			ConversationNamed: true, Job: fixPushJob{State: fixRecordState(), Role: "claude-fix", Head: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1",
				Started: fixRecordAt, Login: "zhuravel-factory[bot]", Mode: FixModeMerge, Conflict: merge, Merged: merge, ForMerge: true, Alone: true,
				Described: true, BaseChecked: true}},
		"fix_push-zero.json": fixPush{},
		"held_merge.json": HeldMerge{Repo: "talkable/talkable", Number: 2, Merge: "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4",
			Head: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", Base: "master", Tip: "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2",
			Workflows: []string{".github/workflows/ci.yml"}, Conflicts: 1, Role: "claude-fix", Identity: "zhuravel-factory",
			Login: "zhuravel-factory[bot]", Clone: "/pool/talkable-1", Diff: "/reviews/talkable/2/held-merge.diff", Findings: true,
			Attention: "the merge of master waits for your y", At: fixRecordAt},
		"held_merge-zero.json": HeldMerge{},
		"pushed.json":          ciPush{Head: "e6c1f37000000000000000000000000000000000", At: fixRecordAt},
		"fix_held.json":        heldFix{At: fixRecordAt, Head: "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1", By: "magnum fix", Why: heldPause, Kind: "claude"},
		"fix_held-zero.json":   heldFix{},
		"fix_seen.json": openLedger{
			Handed: map[string]time.Time{"thread:4100@4101": fixRecordAt}, Reviews: []string{"review:r1"},
			FixerHeads: []string{"e6c1f37000000000000000000000000000000000"}, Cursor: 42, Since: fixRecordAt,
			RaisedMsg: "2 items need you", Raised: []string{"thread:4100@4101"}, Starts: []time.Time{fixRecordAt},
			Read: openRead{Print: "p1", At: fixRecordAt, Head: "a1a1a1a1", Cut: []string{"threads"}, Others: []openItem{thread},
				Mine: []magnumThread{{RootID: 5100, URL: "https://github.com/talkable/talkable/pull/2#discussion_r5100", ReviewID: 101,
					LatestID: 5101, LatestAt: fixRecordAt, Rebuttal: true, Answered: true, AnsweredAt: fixRecordAt}},
				FailedAt: fixRecordAt},
			Conflict:  openConflict{Head: "a1a1a1a1", Base: "master", Tip: "b2b2b2b2", At: fixRecordAt, Paths: []string{"app.go"}, Unchecked: true},
			ReadFails: 1, LateToasted: "a1a1a1a1", Approval: openApprovalCount{SHA: "a0a0a0a0", Head: "a1a1a1a1", After: 2},
			Refused: "fp1", RefusedWhy: "nothing to fix", WaitAt: fixRecordAt, WaitWhy: "the slot is busy",
			Spell: openSpell{Start: fixRecordAt, Seen: fixRecordAt.Add(time.Minute)}, Waited: map[string]time.Time{"rev-ann": fixRecordAt},
		},
		"fix_seen-zero.json": openLedger{},
		"ci_fix.json": ciFixRecord{
			Starts: []ciFixStart{{Head: "a1a1a1a1", Entries: []string{"rspec"}, At: fixRecordAt, By: "autofix", Outcome: ciOutcomeCannot,
				Why: "flaky", Pushed: "e6c1f37"}},
			Read: ciFailRead{Head: "a1a1a1a1", Key: "rspec@1", At: fixRecordAt, File: "/reviews/talkable/2/ci-failures.json", Digest: "sha256:00",
				Jobs: 2, Steps: 3, Lines: 120, Bytes: 4096, Unread: true, UnreadWhy: "needs Actions on the App", Running: true,
				Runs: []ciReadRun{{ID: 42673098082, Attempt: 2}}, Checked: fixRecordAt},
			Base: ciBaseRead{Head: "a1a1a1a1", Key: "rspec@1", Tip: "b2b2b2b2", Checks: []ciBaseCheck{baseCheck}, At: fixRecordAt, Failed: true},
		},
		"ci_fix-zero.json": ciFixRecord{},
		"ci_dispatch.json": ciStart{Head: "a1a1a1a1", Workflow: "ci.yml", At: fixRecordAt, Failed: "GitHub answered HTTP 422",
			Refused: "a pause", Run: &ciRun{ID: 42673098082, Status: "completed", Conclusion: "failure"}, Moved: fixRecordAt},
		"ci_dispatch-zero.json": ciStart{},
		"ci_rerun.json": ciRerun{Head: "a1a1a1a1", ReadAt: fixRecordAt, Queued: 42673098083, QueuedStatus: "queued", Unread: true,
			UnreadWhy: "needs Actions on the App", Run: &ciNoJob{ID: 42673098082, Workflow: "ci.yml", Attempt: 1, Conclusion: "failure"},
			At: fixRecordAt, Failed: "GitHub answered HTTP 403", Refused: "a pause",
			Seen: &ciAttempt{Attempt: 2, Status: "completed", Conclusion: "failure", NoJob: true}, Moved: fixRecordAt},
		"ci_rerun-zero.json":  ciRerun{},
		"fix_calls.json":      map[string]time.Time{"4100": fixRecordAt, "5100": fixRecordAt.Add(time.Minute)},
		"optional_calls.json": []optionalCall{{Thread: 5200, URL: "https://github.com/talkable/talkable/pull/2#discussion_r5200", At: fixRecordAt}},
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
	files, err := filepath.Glob(filepath.Join("testdata", "fixrecords", "*.json"))
	if err != nil || len(files) != len(records) {
		t.Fatalf("testdata/fixrecords holds %d files (%v), want %d", len(files), err, len(records))
	}
	for name, want := range records {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "fixrecords", name))
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
