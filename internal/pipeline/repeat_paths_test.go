package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// postedRound records the env's PR's judge run of round n as posted, with
// fs as the findings of its result (store.RecordFindings, as a posted
// round's recordFindings does).
func (e *env) postedRound(n int, fs ...store.Finding) {
	e.t.Helper()
	run, err := e.st.CreateRun(e.ctx, store.Run{PRID: e.pr.ID, Round: n, Role: "codex-judge", Kind: store.RunInitial, State: store.RunVerified,
		TargetSHA: prevSHA, Identity: "talkable-app", ReviewerLogin: "talkable[bot]", PromptText: "p",
		CreatedAt: t0.Add(time.Duration(n-20) * time.Hour), Outcome: new(OutcomePosted)})
	if err != nil {
		e.t.Fatal(err)
	}
	for i := range fs {
		fs[i].FindingID = fmt.Sprintf("F%d", i+1)
		if fs[i].Verdict == "" {
			fs[i].Verdict = store.FindingPosted
		}
		fs[i].Sources = []string{"claude-review"}
	}
	if err := e.st.RecordFindings(e.ctx, run.ID, e.pr.ID, n, fs); err != nil {
		e.t.Fatal(err)
	}
}

// repeatPathsRound runs a round of kind after the posted rounds the env
// holds (the judge alone, posting) and returns its report directory and
// its judge prompts.
func (e *env) repeatPathsRound(kind string, own bool) (string, []submitCall) {
	e.t.Helper()
	post := e.judgePosts(960, "COMMENTED", "COMMENT").behavior(e.t)
	in := e.input(kind)
	in.Round, in.Roles = 6, judgeAlone(e)
	e.ag.behaviors[agents.RoleJudge] = []behavior{post}
	if own {
		in = e.ownInput(kind)
		in.Round = 6
		e.ag.behaviors[agents.RoleJudge] = []behavior{writeOwn(), post}
	}
	if kind != KindInitial {
		in.Previous = &PreviousReview{ID: 905, Event: "COMMENTED", SHA: prevSHA, SubmittedAt: t0.Add(-time.Hour), Login: "talkable[bot]"}
	}
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		e.t.Fatalf("%s round: RunRound = %+v, %v", kind, res, err)
	}
	return res.ReportDir, e.ag.submitsFor(agents.RoleJudge)
}

// The rule that a mechanism two fixes patched cannot be completed posted no
// finding from 2026-10-12 to 2026-11-11: the judge sees only its own threads
// and cannot count the rounds per file, while four (PR, path) pairs drew P1
// or P2 findings in three or more rounds, one path 8 findings over 7 rounds
// of a PR that reached round 36. A re-review's and a recovery's judge (its
// own pass too) gets repeat-paths.json as `repeat_paths`: each path of the
// PR with P1 or P2 findings in three or more posted rounds, with the
// rounds, those findings' titles and the "(in the fix for ...)" each names,
// with that finding's round. A path with such findings in two rounds is
// not listed, nor a P3 or a rejected candidate; without a listed path the
// key is left out, as it is from a first review.
func TestARereviewNamesThePathsWithP1OrP2FindingsInThreeOrMoreRounds(t *testing.T) {
	const order, other = "app/models/order.rb", "app/other.rb"
	seed := func(e *env) {
		e.postedRound(1,
			store.Finding{Title: "Total skips tax", Severity: "P1", Path: order, Line: 42},
			store.Finding{Title: "Export skips site check", Severity: "P2", Path: other, Line: 3})
		e.postedRound(2, store.Finding{Title: "Wrong field named (optional, no reply needed)", Severity: "P3", Path: order, Line: 7})
		e.postedRound(3,
			store.Finding{Title: "Refund keeps tax", Severity: "P2", Path: "./" + order, Line: 50},
			store.Finding{Title: "Export skips account check", Severity: "P2", Path: other, Line: 9})
		e.postedRound(5,
			store.Finding{Title: "Credit keeps tax (in the fix for Refund keeps tax)", Severity: "P2", Path: order, Line: 61},
			store.Finding{Title: "Export skips owner check", Severity: "P1", Path: other, Line: 12, Verdict: store.FindingRejected, ReasonCode: "speculative"})
	}
	want := repeatPaths{PR: "talkable/talkable#11920", Paths: []repeatPath{{Path: order, Rounds: []int{1, 3, 5}, Findings: []repeatFinding{
		{Round: 1, Severity: "P1", Title: "Total skips tax"},
		{Round: 3, Severity: "P2", Title: "Refund keeps tax"},
		{Round: 5, Severity: "P2", Title: "Credit keeps tax (in the fix for Refund keeps tax)", InTheFixFor: "Refund keeps tax", InTheFixForRound: 3},
	}}}}
	for _, tc := range []struct {
		name string
		kind string
		own  bool
	}{
		{name: "a re-review", kind: KindRereview},
		{name: "a re-review's own pass and candidates", kind: KindRereview, own: true},
		{name: "a recovery", kind: KindRecovery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			seed(e)
			dir, prompts := e.repeatPathsRound(tc.kind, tc.own)
			path := filepath.Join(dir, RepeatPathsFile)
			if wantPrompts := map[bool]int{false: 1, true: 2}[tc.own]; len(prompts) != wantPrompts {
				t.Fatalf("judge prompts = %d, want %d", len(prompts), wantPrompts)
			}
			for i, p := range prompts {
				mustContain(t, fmt.Sprintf("judge prompt %d", i), p.Text, "\nrepeat_paths: "+path+"\n")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var got repeatPaths
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("%s: %v\n%s", RepeatPathsFile, err, raw)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s = %+v\nwant %+v", RepeatPathsFile, got, want)
			}
		})
	}

	none := func(name, kind string, seed func(e *env)) {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			seed(e)
			dir, prompts := e.repeatPathsRound(kind, false)
			for _, p := range prompts {
				if strings.Contains(p.Text, "repeat_paths") {
					t.Errorf("the judge prompt names repeat_paths:\n%s", p.Text)
				}
			}
			if _, err := os.Stat(filepath.Join(dir, RepeatPathsFile)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s: stat = %v, want none", RepeatPathsFile, err)
			}
		})
	}
	none("a first review", KindInitial, seed)
	// An earlier round's file on this head is not this round's: it goes.
	none("findings in two rounds", KindRereview, func(e *env) {
		e.postedRound(1, store.Finding{Title: "Total skips tax", Severity: "P1", Path: order, Line: 42})
		e.postedRound(3, store.Finding{Title: "Refund keeps tax", Severity: "P2", Path: order, Line: 50})
		e.postedRound(5, store.Finding{Title: "Wrong field named (optional, no reply needed)", Severity: "P3", Path: order, Line: 7})
		dir := e.layout.ReviewDir("talkable", "talkable", 11920, target)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, RepeatPathsFile), []byte(`{"paths":[{"path":"old.rb"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	})
}

// The judge's "(in the fix for <earlier title>)" names the earlier title up
// to its own closing parenthesis, parentheses in the earlier title kept,
// or to the end of a title the registry cut before it.
func TestInTheFixForReadsTheEarlierTitle(t *testing.T) {
	for title, want := range map[string]string{
		"Credit keeps tax (in the fix for Refund keeps tax)":                                              "Refund keeps tax",
		"Credit keeps tax (in the fix for Refund keeps tax (missed earlier)) (optional, no reply needed)": "Refund keeps tax (missed earlier)",
		"Credit keeps tax (in the fix for Refund keeps ta":                                                "Refund keeps ta",
		"Credit keeps tax (missed earlier)":                                                               "",
		"Credit keeps tax (in the fix for )":                                                              "",
	} {
		got, ok := inTheFixFor(title)
		if got != want || ok != (want != "") {
			t.Errorf("inTheFixFor(%q) = %q, %v; want %q", title, got, ok, want)
		}
	}
}
