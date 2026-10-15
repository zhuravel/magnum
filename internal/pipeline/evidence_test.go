package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// judgeWritesEvidence is the standard posting judge that first writes body
// as evidence_file, beside its result file, where its prompt names it.
func (e *env) judgeWritesEvidence(id int64, body string) behavior {
	post := e.judgePosts(id, "COMMENTED", "COMMENT").behavior(e.t)
	return func(f *fakeAgents, run store.Run, text string) error {
		file := filepath.Join(filepath.Dir(store.Deref(run.ReportPath)), EvidenceFile)
		if !strings.Contains(text, "\nevidence_file: "+file+"\n") {
			return fmt.Errorf("the judge's prompt does not name %s as evidence_file:\n%s", file, text)
		}
		if body != "" {
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				return err
			}
		}
		return post(f, run, text)
	}
}

// Nobody could tell which files and behaviours of a long PR a round
// examined: findings say where the problems are, nothing says what passed.
// The judge writes evidence.json beside its result file, one entry per
// behaviour it read or probed and one unknown entry per file it did not
// read, and the round reads it at its end and hands it to the engine. An
// entry without a path or with a state that is none of read, probed,
// skipped and unknown is dropped, and the round's warning counts them; a
// path in the checkout is made relative to it.
func TestTheJudgesEvidenceFileIsReadAndValidated(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgeWritesEvidence(601, `{"head_sha":"`+target+`","entries":[
		{"path":"app/models/order.rb","symbol":"Order#total","behaviour":"a refunded item leaves the total","state":"probed","command":"bin/rspec spec/models/order_spec.rb","control":true,"result":"passed"},
		{"path":"`+slotPath+`/app/models/user.rb","behaviour":"the name stays optional","state":"Read"},
		{"behaviour":"no path","state":"read"},
		{"path":"app/models/item.rb","state":"looked at"}
	]}`)}
	res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	want := &Evidence{HeadSHA: target, Entries: []EvidenceEntry{
		{Path: "app/models/order.rb", Symbol: "Order#total", Behaviour: "a refunded item leaves the total", State: EvidenceProbed,
			Command: "bin/rspec spec/models/order_spec.rb", Control: true, Result: "passed"},
		{Path: "app/models/user.rb", Behaviour: "the name stays optional", State: EvidenceRead},
	}}
	if !reflect.DeepEqual(res.Evidence, want) {
		t.Fatalf("Evidence = %+v\nwant %+v", res.Evidence, want)
	}
	warned := 0
	for _, w := range res.Warnings {
		if strings.Contains(w, EvidenceFile) {
			warned++
			mustContain(t, "warning", w, "2 entries dropped")
		}
	}
	if warned != 1 {
		t.Fatalf("warnings %q: want one about %s", res.Warnings, EvidenceFile)
	}

	// An earlier round's file on the same head is not this round's: the
	// round removes it before the judge's prompt, so a judge that writes
	// none hands the engine nothing.
	e2 := newEnv(t)
	dir := e2.layout.ReviewDir("talkable", "talkable", 11920, target)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, EvidenceFile), []byte(`{"entries":[{"path":"old.rb","state":"read"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e2.ag.behaviors[agents.RoleJudge] = []behavior{e2.judgeWritesEvidence(602, "")}
	res, err = e2.r.RunRound(e2.ctx, e2.input(KindInitial))
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if res.Evidence != nil {
		t.Fatalf("Evidence = %+v: an earlier round's file was read as this round's", res.Evidence)
	}
}

// The PR's evidence map (what earlier rounds examined, merged by the
// engine) is an input to the next rounds: the judge's prompt names it as
// `evidence` and the claude reviewer's in a sentence, both saying to read
// the unknown and stale areas first; a blind replay gets none, nor does a
// PR without a map.
func TestTheEvidenceMapIsNamedToTheJudgeAndTheReviewers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		write     bool
		blind     bool
		wantNamed bool
	}{
		{name: "a map", write: true, wantNamed: true},
		{name: "no map"},
		{name: "a blind replay", write: true, blind: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			path := EvidenceMapPath(e.layout, "talkable", "talkable", 11920)
			if tc.write {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(`{"entries":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			p := e.judgePosts(603, "COMMENTED", "COMMENT")
			in := e.input(KindInitial)
			if tc.blind {
				p = e.judgePosts(0, "", "COMMENT")
				p.gh, p.status = nil, "dry_run"
				in.Blind, in.DryRun = true, true
			}
			e.ag.behaviors[agents.RoleJudge] = []behavior{p.behavior(t)}
			if _, err := e.r.RunRound(e.ctx, in); err != nil {
				t.Fatal(err)
			}
			judge, review := e.ag.submitsFor(agents.RoleJudge)[0].Text, e.ag.submitsFor(agents.RoleClaude)[0].Text
			named := strings.Contains(judge, "\nevidence: "+path+"\n")
			if named != tc.wantNamed || strings.Contains(review, path) != tc.wantNamed {
				t.Fatalf("named to the judge %v, to the reviewer %v; want %v\njudge:\n%s\nreviewer:\n%s",
					named, strings.Contains(review, path), tc.wantNamed, judge, review)
			}
			if tc.wantNamed {
				mustContain(t, "judge prompt", judge, "`unknown` or `stale`")
				mustContain(t, "claude-review prompt", review, "`unknown` or `stale`")
			}
		})
	}
}
