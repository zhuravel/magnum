package engine

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

// TestRoutesToNotesTakesEveryLessonVerdictOfScopeRepo: a decline whose
// lesson needs the repository (scope repo) goes to its notes whatever the
// verdict that carries a lesson: missing context, and also a finding that
// was wrong or ranked too high there, which reads as one of the
// repository's standing decisions and had no consumer (the skill editor
// takes scope general only). A general one stays the skill editor's, and a
// finding that stands teaches nothing.
func TestRoutesToNotesTakesEveryLessonVerdictOfScopeRepo(t *testing.T) {
	for _, tc := range []struct {
		verdict, scope string
		routed         bool
	}{
		{store.VerdictMissingContext, store.MissScopeRepo, true},
		{store.VerdictWrong, store.MissScopeRepo, true},
		{store.VerdictOverRanked, store.MissScopeRepo, true},
		{store.VerdictRight, store.MissScopeRepo, false},
		{store.VerdictWrong, store.MissScopeGeneral, false},
		{store.VerdictOverRanked, store.MissScopeGeneral, false},
		{store.VerdictMissingContext, store.MissScopeGeneral, false},
		{store.VerdictWrong, "", false}, // the fixer's thread: no lesson, no scope
	} {
		it := learn.ThreadItem{Outcome: store.DeclineDeclined, Verdict: tc.verdict, Scope: tc.scope}
		if got := routesToNotes(it); got != tc.routed {
			t.Errorf("verdict %s, scope %q: routed = %v, want %v", tc.verdict, tc.scope, got, tc.routed)
		}
	}
}

// TestAWrongDeclineOfScopeRepoBecomesARepoMissForTheCuration: a finding the
// author declined that was wrong for this repository (verdict wrong, scope
// repo) is stored as a miss for the notes and marks the repository, as a
// missing-context one is; a wrong finding of scope general is a decline for
// the skill editor only, with no miss.
func TestAWrongDeclineOfScopeRepoBecomesARepoMissForTheCuration(t *testing.T) {
	repoWrong := learn.ThreadItem{Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong,
		Lesson: "Gift card balances are read through the ledger service, which locks the card, so a read after the charge is no race.",
		Scope:  store.MissScopeRepo, Match: []string{"balance"}}
	general := wrongDecline
	general.Outcome = store.DeclineArgued
	fc := threadsClassifier(map[string]learn.ThreadItem{"o301": repoWrong, "o302": general})
	h, _ := missesHarness(t, fc, &fakeCurator{write: notedProposal})
	pr := retroPR(h, 7, 25*time.Hour)
	seedRetroGitHub(h, 7)
	seedOwnThreads(h, 7)
	h.requestRetro(RetroPayload{})

	ms := h.misses(pr.ID)
	m, ok := ms["301"]
	if !ok || m.Class != store.MissMiss || m.Scope != store.MissScopeRepo || m.State != store.MissNew || !strings.Contains(m.Lesson, "ledger service") ||
		m.Title != "Gift cards skip the balance check" || m.Reviewer != "alice" {
		t.Fatalf("o301's miss = %+v (found %v)", m, ok)
	}
	if _, ok := ms["302"]; ok {
		t.Fatalf("a general decline became a miss: %+v", ms["302"])
	}
	ds := h.declinesOf(pr.ID)
	if d := ds[302]; d.Verdict != store.VerdictWrong || d.Scope != store.MissScopeGeneral || d.State != store.MissNew {
		t.Fatalf("o302's decline = %+v, want it new for the skill editor", d)
	}
	if v, _ := h.e.getKV(h.ctx, KVNotesMisses("talkable/talkable")); v == "" {
		t.Fatal("the repository is not marked for a curation")
	}
	if evs := notesEvents(t, h, "notes.misses"); len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"declines":1`) {
		t.Fatalf("notes.misses events = %+v", evs)
	}
}

// TestTheRetroStoresWhereACommentWasMadeAndWhyItIsOutside: each misses row
// keeps the commit its comment was made on beside the reviewed commit it
// applies to, and an outside one says why: t102, on a later push that
// changed its file, file_changed; t103, moved to the reviewed commit and
// then answered outside by the classifier, classifier. The others carry no
// reason.
func TestTheRetroStoresWhereACommentWasMadeAndWhyItIsOutside(t *testing.T) {
	fc := &fakeClassifier{}
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	seedRetroGitHub(h, 7)
	base := fc.answer
	fc.answer = func(job ClassifyJob) (ClassifyResult, error) {
		if _, err := (&fakeClassifier{answer: base}).Classify(context.Background(), job); err != nil {
			return ClassifyResult{}, err
		}
		b, _ := os.ReadFile(job.OutputPath)
		var out learn.Output
		_ = json.Unmarshal(b, &out)
		for i := range out.Items {
			if out.Items[i].ID == "t103" {
				out.Items[i] = learn.Item{ID: "t103", Class: store.MissOutside}
			}
		}
		b, _ = json.Marshal(out)
		return ClassifyResult{}, os.WriteFile(job.OutputPath, b, 0o600)
	}
	h.requestRetro(RetroPayload{})

	ms := h.misses(pr.ID)
	for id, want := range map[string]struct{ class, reason, comment string }{
		"101": {store.MissMiss, "", retroShaA},
		"102": {store.MissOutside, store.MissOutsideFileChanged, retroShaB},
		"103": {store.MissOutside, store.MissOutsideClassifier, retroShaB},
		"201": {store.MissStyle, "", retroShaA},
	} {
		m := ms[id]
		if m.Class != want.class || m.OutsideReason != want.reason || m.CommentSHA != want.comment || m.ReviewedSHA != retroShaA {
			t.Errorf("%s: class %s, reason %q, comment %s, reviewed %s; want %s, %q, %s, %s", id, m.Class, m.OutsideReason,
				m.CommentSHA, m.ReviewedSHA, want.class, want.reason, want.comment, retroShaA)
		}
	}
}
