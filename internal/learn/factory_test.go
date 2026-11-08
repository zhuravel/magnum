package learn

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/escapes"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
)

// localAt is a finding of the factory's local round on path:line at head.
func localAt(ref, path string, line int, head string) escapes.Finding {
	return escapes.Finding{Source: escapes.SourceLocal, Ref: ref, Path: path, StartLine: line, EndLine: line, Priority: "P2", Head: head}
}

// factoryInput is input on a pull request the factory published, whose
// local round found what local holds.
func factoryInput(threads []github.Thread, reviews []github.Review, local ...escapes.Finding) Input {
	in := input(threads, reviews)
	in.Local = &LocalRound{Findings: local}
	return in
}

func byID(cs []Candidate) map[string]Candidate {
	out := map[string]Candidate{}
	for _, c := range cs {
		out[c.ID] = c
	}
	return out
}

// On a pull request the factory published, a comment no finding of its
// local round caught is tagged origin factory; one a local finding within
// NearLines caught is not; and a comment both rounds missed stays an
// ordinary candidate for the classifier, tagged.
func TestAFactoryPRsCommentTheLocalRoundMissedIsTaggedFactory(t *testing.T) {
	in := factoryInput([]github.Thread{
		thread(1, "app/models/coupon.rb", 42, shaA, longish), // nothing local near it
		thread(2, "app/models/coupon.rb", 80, shaA, longish), // a local finding 2 lines away
		thread(3, "app/models/other.rb", 10, shaA, longish),  // a local finding 4 lines away
	}, nil, localAt("1/31", "app/models/coupon.rb", 82, shaA), localAt("1/32", "app/models/other.rb", 14, shaA))
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	got := byID(res.Candidates)
	if len(got) != 3 || res.Caught != 0 {
		t.Fatalf("candidates %v, caught %d; want three candidates", ids(res.Candidates), res.Caught)
	}
	for id, origin := range map[string]string{"t1": OriginFactory, "t2": "", "t3": OriginFactory} {
		if c := got[id]; c.Origin != origin || c.PostedCaught {
			t.Errorf("%s: origin %q, posted caught %v; want origin %q", id, c.Origin, c.PostedCaught, origin)
		}
	}
}

// A pull request the factory did not publish is never tagged, whatever
// its comments.
func TestACommentOnAPRTheFactoryDidNotPublishIsNeverTagged(t *testing.T) {
	res, err := Build(input([]github.Thread{thread(1, "a.rb", 42, shaA, longish)},
		[]github.Review{{DatabaseID: 9, State: "COMMENTED", Body: longish, CommitOid: shaA, AuthorLogin: "rev-ann", AuthorType: "User",
			SubmittedAt: t0.Add(time.Hour), URL: "https://github.com/talkable/widgets/pull/7#pullrequestreview-9"}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Candidates {
		if c.Origin != "" || c.PostedCaught {
			t.Errorf("%s tagged: %+v", c.ID, c)
		}
	}
}

// A comment magnum's posted round caught that the local round did not is
// a factory-origin candidate marked as the posted round's catch, not a
// caught comment: the local round mattered, so it is recorded, but only
// as a factory-origin miss. One both rounds caught is caught, as today.
func TestACommentOnlyThePostedRoundCaughtIsAFactoryOriginCandidate(t *testing.T) {
	threads := []github.Thread{thread(1, "a.rb", 42, shaA, longish), thread(2, "b.rb", 7, shaA, longish)}
	in := factoryInput(threads, nil, localAt("1/31", "b.rb", 9, shaA))
	in.Findings = []store.Finding{
		{RunID: "r-1", FindingID: "F1", Severity: "P2", Path: "a.rb", Line: 43, Verdict: store.FindingPosted},
		{RunID: "r-1", FindingID: "F2", Severity: "P2", Path: "b.rb", Line: 7, Verdict: store.FindingPosted},
	}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Caught != 1 || !slices.Equal(ids(res.Candidates), []string{"t1"}) {
		t.Fatalf("caught %d, candidates %v; want t2 caught and t1 a candidate", res.Caught, ids(res.Candidates))
	}
	if c := res.Candidates[0]; c.Origin != OriginFactory || !c.PostedCaught || c.Raised != store.MissRaisedNone {
		t.Errorf("t1 = %+v; want origin factory, caught by the posted round", c)
	}
	// Without the factory, the same comment is caught.
	in.Local = nil
	if res, err := Build(in); err != nil || res.Caught != 2 || len(res.Candidates) != 0 {
		t.Errorf("a PR the factory did not publish: caught %d, candidates %v, %v", res.Caught, ids(res.Candidates), err)
	}
}

// The judge's rejection of its finding as a duplicate of a comment it read
// is the posted round's catch too.
func TestACommentThePostedRoundRejectedAsADuplicateIsTheFactorysMiss(t *testing.T) {
	in := duplicateCase(t0.Add(time.Hour), t0.Add(2*time.Hour), "duplicate")
	in.Local = &LocalRound{}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Caught != 0 || !slices.Equal(ids(res.Candidates), []string{"t1"}) {
		t.Fatalf("caught %d, candidates %v; want t1 a candidate", res.Caught, ids(res.Candidates))
	}
	if c := res.Candidates[0]; c.Origin != OriginFactory || !c.PostedCaught {
		t.Errorf("t1 = %+v; want origin factory, caught by the posted round", c)
	}
	in.Local = &LocalRound{Findings: []escapes.Finding{localAt("1/31", "a.rb", 44, shaA)}}
	if res, err := Build(in); err != nil || res.Caught != 1 || len(res.Candidates) != 0 {
		t.Errorf("caught by both rounds: caught %d, candidates %v, %v", res.Caught, ids(res.Candidates), err)
	}
}

// A local finding names the approved head. A comment on a later reviewed
// head is compared with it only when the file did not change between the
// two (the retro's own comparison, asked once per pair of commits); at a
// head where it changed, or with no comparison, the local finding catches
// nothing.
func TestALocalFindingCatchesACommentOfALaterHeadOnlyWhenItsFileDidNotChange(t *testing.T) {
	in := factoryInput([]github.Thread{thread(1, "a.rb", 42, shaC, longish), thread(2, "b.rb", 5, shaC, longish)}, nil,
		localAt("1/31", "a.rb", 42, shaA), localAt("1/32", "b.rb", 5, shaA))
	in.Reviewed = append(in.Reviewed, Reviewed{SHA: shaC, At: t0.Add(30 * time.Minute)})
	var asked [][2]string
	in.Compare = func(from, to string) (Comparison, error) {
		asked = append(asked, [2]string{from, to})
		return Comparison{Descendant: true, Complete: true, Paths: []string{"b.rb"}}, nil
	}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	got := byID(res.Candidates)
	if got["t1"].Origin != "" || got["t2"].Origin != OriginFactory {
		t.Errorf("t1 origin %q (a.rb did not change: caught), t2 origin %q (b.rb changed: missed)", got["t1"].Origin, got["t2"].Origin)
	}
	if !slices.Equal(asked, [][2]string{{shaA, shaC}}) {
		t.Errorf("compared %v, want shaA...shaC once", asked)
	}
	in.Compare = nil
	if res, err := Build(in); err != nil || byID(res.Candidates)["t1"].Origin != OriginFactory {
		t.Errorf("with no comparison the local finding of another head caught t1: %+v, %v", res.Candidates, err)
	}
}

// A comparison GitHub fails for the local round is the build's error, as
// for magnum's own rounds.
func TestAComparisonThatFailsForTheLocalRoundFailsTheBuild(t *testing.T) {
	in := factoryInput([]github.Thread{thread(1, "a.rb", 42, shaC, longish)}, nil, localAt("1/31", "a.rb", 42, shaA))
	in.Reviewed = append(in.Reviewed, Reviewed{SHA: shaC, At: t0.Add(30 * time.Minute)})
	in.Compare = func(string, string) (Comparison, error) { return Comparison{}, errors.New("HTTP 502") }
	if _, err := Build(in); err == nil {
		t.Fatal("Build passed over a comparison that failed")
	}
	in.Compare = func(string, string) (Comparison, error) { return Comparison{}, github.ErrNotFound }
	res, err := Build(in)
	if err != nil || byID(res.Candidates)["t1"].Origin != OriginFactory {
		t.Errorf("a 404 proves nothing, so the local finding catches nothing: %+v, %v", res.Candidates, err)
	}
}

// A comment that cannot be placed (a review body, a comment on deleted
// lines) is the local round's miss: no local finding can catch it. An
// outside comment, on code magnum's review did not see, keeps no origin.
func TestAnUnplacedCommentIsTheLocalRoundsMissAndAnOutsideOneHasNoOrigin(t *testing.T) {
	deleted := thread(2, "a.rb", 42, shaA, longish)
	deleted.DiffSide = "LEFT"
	outside := thread(3, "moved.rb", 5, shaB, longish)
	body := github.Review{DatabaseID: 9, State: "COMMENTED", Body: longish, CommitOid: shaA, AuthorLogin: "rev-ann", AuthorType: "User",
		SubmittedAt: t0.Add(time.Hour), URL: "https://github.com/talkable/widgets/pull/7#pullrequestreview-9"}
	in := factoryInput([]github.Thread{deleted, outside}, []github.Review{body}, localAt("1/31", "a.rb", 42, shaA),
		localAt("1/32", "moved.rb", 5, shaB))
	in.Compare = func(string, string) (Comparison, error) {
		return Comparison{Descendant: true, Complete: true, Paths: []string{"moved.rb"}}, nil
	}
	res, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	got := byID(res.Candidates)
	if got["t2"].Origin != OriginFactory || got["r9"].Origin != OriginFactory {
		t.Errorf("unplaced: t2 origin %q, r9 origin %q; want factory", got["t2"].Origin, got["r9"].Origin)
	}
	if len(res.Outside) != 1 || res.Outside[0].ID != "t3" || res.Outside[0].Origin != "" {
		t.Errorf("outside = %+v; want t3 with no origin", res.Outside)
	}
}

// The classifier places a factory-origin candidate's miss: plan, code or
// review. The stage is kept on such a candidate of class miss, style or
// outside, any case and spacing aside.
func TestParseOutputKeepsTheStageOfAFactoryOriginCandidate(t *testing.T) {
	cands := []Candidate{{ID: "t1", Origin: OriginFactory}, {ID: "t2", Origin: OriginFactory}, {ID: "t3", Origin: OriginFactory}}
	out := `{"items":[` + strings.Replace(goodMiss, `"scope":"general"`, `"scope":"general","stage":"plan"`, 1) +
		`,{"id":"t2","class":"outside","stage":" Code "},{"id":"t3","class":"style","stage":"review"}]}`
	items, _, err := ParseOutput([]byte(out), cands, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"t1": store.MissStagePlan, "t2": store.MissStageCode, "t3": store.MissStageReview} {
		if got := items[id].Stage; got != want {
			t.Errorf("%s: stage %q, want %q", id, got, want)
		}
	}
}

// A stage the classifier left out or gave a value outside plan, code and
// review stays empty, never guessed, and is no error; a candidate that is
// not the factory's, or a not_issue one, keeps none.
func TestParseOutputLeavesAnUnknownMissingOrUnaskedStageEmpty(t *testing.T) {
	cands := []Candidate{{ID: "t1", Origin: OriginFactory}, {ID: "t2", Origin: OriginFactory}, {ID: "t3"},
		{ID: "t4", Origin: OriginFactory}}
	out := `{"items":[` + strings.Replace(goodMiss, `"scope":"general"`, `"scope":"general","stage":"deploy"`, 1) +
		`,{"id":"t2","class":"style"},{"id":"t3","class":"style","stage":"review"},{"id":"t4","class":"not_issue","stage":"plan"}]}`
	items, _, err := ParseOutput([]byte(out), cands, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"t1", "t2", "t3", "t4"} {
		if got := items[id].Stage; got != "" {
			t.Errorf("%s: stage %q, want none", id, got)
		}
	}
}
