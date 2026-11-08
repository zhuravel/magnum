package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// statusRetroSet records the last retro's summary the way the daemon does.
func statusRetroSet(t *testing.T, st *store.Store, sum engine.RetroSummary) {
	t.Helper()
	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(context.Background(), engine.KVRetroLast, string(b)); err != nil {
		t.Fatal(err)
	}
}

// statusRetroSeed adds new misses of class miss: a general P2 one (the
// skill editor's), a general P3 one (no job's) and a repository P3 one (the
// notes curation's); a used general P1 one and a style one.
func statusRetroSeed(t *testing.T, st *store.Store) {
	t.Helper()
	_, pr := inspSeedPR(t, st, "talkable/talkable", 11800, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	general, repo := store.MissScopeGeneral, store.MissScopeRepo
	for i, m := range []store.Miss{
		{Class: store.MissMiss, Scope: general, Severity: "P2"}, {Class: store.MissMiss, Scope: general, Severity: "P3"},
		{Class: store.MissMiss, Scope: repo, Severity: "P3"}, {Class: store.MissMiss, Scope: general, Severity: "P1", State: store.MissUsed},
		{Class: store.MissStyle},
	} {
		m.SourceURL = "https://example.com/c/" + string(rune('a'+i))
		m.Title = "t"
		missSeed(t, st, pr, m)
	}
}

// statusRetroInputs is the retro line's counts of statusRetroSeed's rows.
const statusRetroInputs = " · 1 miss for the skill editor · 1 miss for the notes curation"

func statusRetroOut(t *testing.T, d statusDeps) (statusReport, string) {
	t.Helper()
	r, err := statusGather(context.Background(), d, statusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	statusRenderHeader(&b, r)
	return r, b.String()
}

func TestStatusRetroLineIsAbsentWhenLearnIsOffAndNoRetroRan(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	statusRetroSeed(t, st) // misses alone do not show the line
	r, out := statusRetroOut(t, d)
	if r.Retro != nil || strings.Contains(out, "retro:") {
		t.Errorf("retro = %+v\n%s", r.Retro, out)
	}
	// An unreadable summary is absence too.
	if err := st.SetKV(context.Background(), engine.KVRetroLast, "{not json"); err != nil {
		t.Fatal(err)
	}
	if r, out = statusRetroOut(t, d); r.Retro != nil || strings.Contains(out, "retro:") {
		t.Errorf("malformed summary: retro = %+v\n%s", r.Retro, out)
	}
}

func TestStatusRetroLineWhenEnabledNeverRan(t *testing.T) {
	_, st, d, _ := statusFixture(t)
	d.Config.Learn.Enabled = true
	_, out := statusRetroOut(t, d)
	actContains(t, out, "retro:    never\n")
	statusRetroSeed(t, st)
	_, out = statusRetroOut(t, d)
	actContains(t, out, "retro:    never"+statusRetroInputs+"\n")
}

func TestStatusRetroLineAfterARetroEvenWhenLearnIsOff(t *testing.T) {
	_, st, d, now := statusFixture(t)
	statusRetroSeed(t, st)
	statusRetroSet(t, st, engine.RetroSummary{Run: "20261003-100000", At: now.Add(-3 * time.Hour), Finished: now.Add(-2 * time.Hour),
		PRs: 3, Classified: 1, Misses: 2, Caught: 4})
	r, out := statusRetroOut(t, d)
	actContains(t, out, "retro:    2h ago, 3 PRs, 1 classified"+statusRetroInputs+"\n")
	if r.Retro == nil || r.Retro.Enabled || r.Retro.Last == nil || r.Retro.Inputs == nil ||
		*r.Retro.Inputs != (engine.LearnInputs{EditorMisses: 1, NotesMisses: 1}) {
		t.Errorf("retro = %+v", r.Retro)
	}
	// The line sits after the codex line (or the github one) and before the rounds line.
	if i, j := strings.Index(out, "retro:"), strings.Index(out, "rounds:"); i < 0 || j < i || strings.Index(out, "github:") > i {
		t.Errorf("retro line out of place:\n%s", out)
	}
}

func TestStatusRetroLineWordsOneAndFailures(t *testing.T) {
	_, st, d, now := statusFixture(t)
	statusRetroSet(t, st, engine.RetroSummary{At: now.Add(-90 * time.Minute), Finished: now.Add(-time.Hour), PRs: 1, Classified: 0, Failed: 1})
	_, out := statusRetroOut(t, d)
	actContains(t, out, "retro:    1h ago, 1 PR, 0 classified, 1 failed\n")
}

// On 2026-11-08 the line read "87 new misses · 221 declined", which reads
// as work for you, yet 63 of the misses were general P3s that no job takes,
// and the declines are the retro's inputs, 171 of them findings that stand
// (verdict right: the author declined a correct finding). The line counts
// what a job will use and says which:
// the skill editor's general misses of P0 to P2 and general findings
// declined as wrong or over-ranked, still new, and the notes curation's
// repository misses; a count of nothing is left out, and `magnum misses`
// keeps the rest.
func TestStatusRetroLineCountsOnlyWhatAJobWillUse(t *testing.T) {
	_, st, d, now := statusFixture(t)
	statusRetroSet(t, st, engine.RetroSummary{At: now.Add(-3 * time.Hour), Finished: now.Add(-2 * time.Hour), PRs: 3, Classified: 1,
		Declines: 4, Declined: 3})
	_, pr := inspSeedPR(t, st, "talkable/talkable", 11801, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	general, repo := store.MissScopeGeneral, store.MissScopeRepo
	for i, dl := range []store.Decline{
		{Outcome: store.DeclineDeclined, Verdict: store.VerdictRight},
		{Outcome: store.DeclineArgued, Verdict: store.VerdictRight},
		{Outcome: store.DeclineFixed},
		{Outcome: store.DeclineDeclined, Verdict: store.VerdictMissingContext, Scope: repo, Lesson: "l", Match: []string{"x"}},
		{Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong, Scope: repo, Lesson: "l", Match: []string{"x"}},
		{Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong, Scope: general, Lesson: "l", Match: []string{"x"}},
		{Outcome: store.DeclineArgued, Verdict: store.VerdictOverRanked, Scope: general, Lesson: "l", Match: []string{"x"}},
		{Outcome: store.DeclineDeclined, Verdict: store.VerdictWrong, Scope: general, Lesson: "l", Match: []string{"x"}},
	} {
		got := declineSeed(t, st, pr, int64(i+1), dl)
		if i == 7 { // a lesson was drawn from it
			if _, err := st.SetDeclineState(context.Background(), []int64{got.ID}, []string{store.MissNew}, store.MissUsed); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, out := statusRetroOut(t, d)
	actContains(t, out, "retro:    2h ago, 3 PRs, 1 classified · 2 declined findings for the skill editor\n")
	if strings.Contains(out, "declined ·") || strings.Contains(out, "new misses") {
		t.Errorf("the line counts what no job uses:\n%s", out)
	}
	statusRetroSeed(t, st)
	r, out := statusRetroOut(t, d)
	actContains(t, out, "retro:    2h ago, 3 PRs, 1 classified · 1 miss and 2 declined findings for the skill editor"+
		" · 1 miss for the notes curation\n")
	if r.Retro == nil || r.Retro.Inputs == nil || *r.Retro.Inputs != (engine.LearnInputs{EditorMisses: 1, EditorDeclines: 2, NotesMisses: 1}) {
		t.Errorf("retro = %+v", r.Retro)
	}
}

// The last retro's factory-origin misses (comments on PRs the factory
// published that its local review missed) are on the line, and the count is
// left out when there is none.
func TestStatusRetroLineCountsTheLastRetrosFactoryOriginMisses(t *testing.T) {
	_, st, d, now := statusFixture(t)
	statusRetroSet(t, st, engine.RetroSummary{At: now.Add(-3 * time.Hour), Finished: now.Add(-2 * time.Hour), PRs: 3, Classified: 2,
		Misses: 1, Factory: 2})
	_, out := statusRetroOut(t, d)
	actContains(t, out, "retro:    2h ago, 3 PRs, 2 classified, 2 factory-origin misses\n")
	statusRetroSet(t, st, engine.RetroSummary{At: now.Add(-3 * time.Hour), Finished: now.Add(-2 * time.Hour), PRs: 1, Classified: 1, Factory: 1})
	_, out = statusRetroOut(t, d)
	actContains(t, out, "retro:    2h ago, 1 PR, 1 classified, 1 factory-origin miss\n")
}

func TestStatusRetroLineSaysWhyARetroStoppedAndCleansIt(t *testing.T) {
	_, st, d, now := statusFixture(t)
	statusRetroSet(t, st, engine.RetroSummary{At: now.Add(-50 * time.Minute), Finished: now.Add(-45 * time.Minute), PRs: 2, Classified: 1,
		Stopped: "claude " + statusNoise})
	_, out := statusRetroOut(t, d)
	statusNoControls(t, "status retro line", out)
	actContains(t, out, "retro:    45m ago, stopped: claude ")
	if strings.Contains(out, "classified ·") {
		t.Errorf("a stopped retro also reports its counts:\n%s", out)
	}
}

func TestStatusJSONCarriesTheRetro(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	statusRetroSeed(t, st)
	statusRetroSet(t, st, engine.RetroSummary{Run: "20261003-100000", At: time.Now().Add(-time.Hour), Finished: time.Now().Add(-50 * time.Minute), PRs: 3, Classified: 2, Misses: 2})
	st.Close()
	if code := f.run("status", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err.String())
	}
	var r struct {
		Retro struct {
			Enabled bool `json:"enabled"`
			Last    struct {
				Run string `json:"run"`
				PRs int    `json:"prs"`
			} `json:"last"`
			Inputs *struct {
				EditorMisses   int `json:"editor_misses"`
				EditorDeclines int `json:"editor_declines"`
				NotesMisses    int `json:"notes_misses"`
			} `json:"inputs"`
			NewMisses *int `json:"new_misses"`
		} `json:"retro"`
	}
	if err := json.Unmarshal(f.Out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Retro.Last.Run != "20261003-100000" || r.Retro.Last.PRs != 3 || r.Retro.Inputs == nil || r.Retro.Inputs.EditorMisses != 1 ||
		r.Retro.Inputs.EditorDeclines != 0 || r.Retro.Inputs.NotesMisses != 1 || r.Retro.NewMisses != nil || r.Retro.Enabled {
		t.Errorf("retro = %+v", r.Retro)
	}
	if code := f.run("status"); code != 0 || !strings.Contains(f.Out.String(), "retro:    ") || !strings.Contains(f.Out.String(), "3 PRs, 2 classified"+statusRetroInputs) {
		t.Errorf("status text: exit %d\n%s", code, f.Out.String())
	}
}
