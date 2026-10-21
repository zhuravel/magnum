package pipeline

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/jira"
	"github.com/zhuravel/magnum/internal/store"
)

const (
	ticketSite    = "https://example.atlassian.net"
	ticketSummary = "Let referrers pick a payout method"
)

// ticketIssue is Jira's answer for key: a story in progress with a
// description and acceptance criteria.
func ticketIssue(key string) string {
	return `{"key":"` + key + `","fields":{"summary":"` + ticketSummary + `","status":{"name":"In Progress"},` +
		`"issuetype":{"name":"Story"},"description":"Referrers choose PayPal or a gift card.",` +
		`"customfield_10010":"Given a referrer without a payout method, the dashboard asks for one.",` +
		`"assignee":{"displayName":"Alice Example"}},"names":{"customfield_10010":"Acceptance Criteria"}}`
}

// withJira turns [jira] on for the env's rounds: the site, a Jira tracker
// for PS- and a Linear one for DMA-, and a Jira client whose curl answers
// with answer (the issue's key from the URL, "200" and its JSON when nil).
// It returns curl's fake.
func (e *env) withJira(answer func(key string) execx.Result) *execx.Fake {
	e.t.Helper()
	token := filepath.Join(e.t.TempDir(), "jira.token")
	if err := os.WriteFile(token, []byte("jira-api-token-0123456789\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.cfg.Jira.URL, e.cfg.Jira.User, e.cfg.Jira.TokenFile = ticketSite, "alice@example.com", token
	e.cfg.Board.Trackers = []string{ticketSite + "/browse/PS-{num}", "https://linear.app/example/issue/DMA-{num}"}
	if answer == nil {
		answer = func(key string) execx.Result { return execx.Result{Stdout: []byte(ticketIssue(key) + "\n200")} }
	}
	curl := &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"curl"}, Fn: func(c execx.Cmd) (execx.Result, error) {
		_, rest, _ := strings.Cut(string(c.Stdin), "/rest/api/2/issue/")
		key, _, _ := strings.Cut(rest, "?")
		res := answer(key)
		if res.Code != 0 {
			return res, &execx.ExitError{Cmd: c, Code: res.Code}
		}
		return res, nil
	}}}}
	e.r.Tickets = &jira.Client{Runner: curl, URL: ticketSite, User: "alice@example.com", TokenFile: token}
	return curl
}

// askedKeys are the issue keys curl was asked for.
func askedKeys(curl *execx.Fake) []string {
	var out []string
	for _, c := range curl.CallsWithPrefix("curl") {
		_, rest, _ := strings.Cut(string(c.Stdin), "/rest/api/2/issue/")
		key, _, _ := strings.Cut(rest, "?")
		out = append(out, key)
	}
	return out
}

// noTicket fails when the round wrote ticket.md in dir or a judge prompt
// names one.
func (e *env) noTicket(dir string) {
	e.t.Helper()
	if _, err := os.Stat(filepath.Join(dir, TicketFile)); !errors.Is(err, os.ErrNotExist) {
		e.t.Errorf("%s: %v, want none", TicketFile, err)
	}
	for _, p := range e.ag.submitsFor(agents.RoleJudge) {
		if strings.Contains(p.Text, "\nticket:") {
			e.t.Errorf("a judge prompt names a ticket:\n%s", p.Text)
		}
	}
}

// The issue a round reads is the first key of a tracker on the [jira] site
// in the PR's title, else in its head branch; a key of a tracker elsewhere
// (Linear) is never read, and a PR whose title and branch name none gets no
// ticket.md and one info line saying so. One Jira read a round, and no
// GitHub call for it: the PR's description is not read (pipeline.GitHub
// has no call that would).
func TestTheRoundFindsTheIssueKeyInTheTitleThenTheBranch(t *testing.T) {
	for _, tc := range []struct {
		name, title, branch string
		want                string
	}{
		{"title", "[PS-123] Payout methods", "feature/PS-999-other", "PS-123"},
		{"branch", "Payout methods", "feature/PS-124-payout-methods", "PS-124"},
		{"linear skipped", "[DMA-7] Payout methods", "PS-125-payout", "PS-125"},
		{"linear only", "[DMA-7] Payout methods", "dma-7-payout", ""},
		{"none", "Payout methods", "payout-methods", ""},
		{"no branch", "Payout methods", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			curl := e.withJira(nil)
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(841, "COMMENTED", "COMMENT").behavior(t)}
			in := e.input(KindInitial)
			in.PR.Title = new(tc.title)
			if tc.branch != "" {
				in.PR.HeadRef = new(tc.branch)
			}
			res, err := e.r.RunRound(e.ctx, in)
			if err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			evs := e.eventsOf("round.ticket")
			if len(evs) != 1 || evs[0].Level != "info" {
				t.Fatalf("round.ticket events = %+v, want one at info", evs)
			}
			if tc.want == "" {
				if keys := askedKeys(curl); len(keys) != 0 {
					t.Errorf("Jira was asked for %q", keys)
				}
				e.noTicket(res.ReportDir)
				mustContain(t, "round.ticket", evs[0].Message, "no Jira issue key", "title", "head branch", "no "+TicketFile)
				return
			}
			if keys := askedKeys(curl); len(keys) != 1 || keys[0] != tc.want {
				t.Fatalf("Jira was asked for %q, want %s once", keys, tc.want)
			}
			b, err := os.ReadFile(filepath.Join(res.ReportDir, TicketFile))
			if err != nil || !strings.HasPrefix(string(b), "# "+tc.want+" "+ticketSite+"/browse/"+tc.want+"\n") {
				t.Errorf("%s = %q, %v", TicketFile, b, err)
			}
		})
	}
}

// ticket.md is in the report directory, written atomically and private,
// and every judge prompt names it (`ticket: <path>`), the claude
// reviewer's none; the round.ticket event names the key, the status, the
// number of requirement fields and the sizes, never the summary or any
// text of the issue. A continued turn reads no ticket again: its prompt
// names the paused turn's file when there is one.
func TestTheTicketFileReachesTheJudgePrompt(t *testing.T) {
	e := newEnv(t)
	curl := e.withJira(nil)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(842, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.PR.Title = new("[PS-123] Payout methods")
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	path := filepath.Join(res.ReportDir, TicketFile)
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v (mode %v)", TicketFile, err, fi)
	}
	b, _ := os.ReadFile(path)
	mustContain(t, TicketFile, string(b), "# PS-123 "+ticketSite+"/browse/PS-123\n", "## Summary\n\n"+ticketSummary+"\n",
		"## Status\n\nIn Progress\n", "## Acceptance Criteria\n\nGiven a referrer")
	if strings.Contains(string(b), "Alice") {
		t.Errorf("%s names the assignee:\n%s", TicketFile, b)
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if len(judge) == 0 {
		t.Fatal("no judge prompt")
	}
	for _, p := range judge {
		mustContain(t, "judge prompt", p.Text, "\nticket: "+path+"\n")
		if strings.Contains(p.Text, ticketSummary) {
			t.Errorf("a judge prompt carries the issue's text:\n%s", p.Text)
		}
	}
	for _, p := range e.ag.submitsFor(agents.RoleClaude) {
		if strings.Contains(p.Text, TicketFile) {
			t.Errorf("the claude reviewer's prompt names the ticket:\n%s", p.Text)
		}
	}
	if keys := askedKeys(curl); len(keys) != 1 {
		t.Errorf("Jira was asked %d times, want once a round", len(keys))
	}
	evs := e.eventsOf("round.ticket")
	if len(evs) != 1 || evs[0].Level != "info" {
		t.Fatalf("round.ticket events = %+v", evs)
	}
	mustContain(t, "round.ticket", evs[0].Message, TicketFile, "PS-123", "In Progress", "1 requirement field")
	var data map[string]any
	if err := json.Unmarshal(evs[0].Data, &data); err != nil || data["key"] != "PS-123" || data["status"] != "In Progress" ||
		data["fields"] != float64(1) || data["description_bytes"] != float64(39) || data["bytes"] != float64(len(b)) || data["file"] != path {
		t.Errorf("round.ticket data = %s (%v)", evs[0].Data, err)
	}
	for _, ev := range e.events() {
		if s := ev.Message + string(ev.Data); strings.Contains(s, "referrer") || strings.Contains(s, "Referrers") || strings.Contains(s, "Alice") {
			t.Errorf("event %s carries the issue's text: %s %s", ev.Kind, ev.Message, ev.Data)
		}
	}

	// A continued turn of a paused round: no Jira read, the paused turn's file named.
	for _, paused := range []bool{true, false} {
		e := newEnv(t)
		curl := e.withJira(nil)
		dir := e.reportDir()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if paused {
			if err := os.WriteFile(filepath.Join(dir, TicketFile), []byte("# PS-123 "+ticketSite+"/browse/PS-123\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "claude-review.md"), []byte("## P2 something\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(843, "COMMENTED", "COMMENT").behavior(t)}
		in := e.input(KindContinue)
		in.ContinueRunID = "r-20261003T100000-7"
		in.PR.Title = new("[PS-123] Payout methods")
		if res, err := e.r.RunRound(e.ctx, in); err != nil || res.Outcome != OutcomePosted {
			t.Fatalf("continue: RunRound = %+v, %v", res, err)
		}
		if keys := askedKeys(curl); len(keys) != 0 {
			t.Errorf("continue: Jira was asked for %q", keys)
		}
		text := e.ag.submitsFor(agents.RoleJudge)[0].Text
		if has := strings.Contains(text, "\nticket: "+filepath.Join(dir, TicketFile)+"\n"); has != paused {
			t.Errorf("continue (paused turn's file %v): the prompt names it %v:\n%s", paused, has, text)
		}
	}
}

// A ticket that cannot be read (a refusal, a timeout, an answer that is
// not an issue) is one round.ticket warning with its class and the key,
// never the answer's body, and the round goes on without ticket.md (an
// earlier round's on the same head removed). A token file problem, which
// the config already warned about, warns once per daemon start (one
// client), then is info.
func TestATicketFetchFailureDoesNotFailTheRound(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer execx.Result
		class  string
	}{
		{"404", execx.Result{Stdout: []byte(`{"errorMessages":["Issue does not exist: Payouts"]}` + "\n404")}, jira.ClassNotFound},
		{"401", execx.Result{Stdout: []byte(`{"message":"Payouts"}` + "\n401")}, jira.ClassUnauthorized},
		{"timeout", execx.Result{Code: 28, Stderr: []byte("curl: (28) Operation timed out")}, jira.ClassTimeout},
		{"malformed", execx.Result{Stdout: []byte("<html>Payouts</html>\n200")}, jira.ClassMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.withJira(func(string) execx.Result { return tc.answer })
			stale := filepath.Join(e.reportDir(), TicketFile)
			if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stale, []byte("# PS-123 an earlier round's\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(844, "COMMENTED", "COMMENT").behavior(t)}
			in := e.input(KindInitial)
			in.PR.Title = new("[PS-123] Payout methods")
			res, err := e.r.RunRound(e.ctx, in)
			if err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			e.noTicket(res.ReportDir)
			evs := e.eventsOf("round.ticket")
			if len(evs) != 1 || evs[0].Level != "warn" {
				t.Fatalf("round.ticket events = %+v, want one warning", evs)
			}
			mustContain(t, "round.ticket", evs[0].Message, "PS-123", "the judge goes without it")
			var data map[string]any
			if err := json.Unmarshal(evs[0].Data, &data); err != nil || data["class"] != tc.class || data["key"] != "PS-123" {
				t.Errorf("round.ticket data = %s (%v), want class %s", evs[0].Data, err, tc.class)
			}
			if s := evs[0].Message + string(evs[0].Data); strings.Contains(s, "Payouts") || strings.Contains(s, "Operation timed out") {
				t.Errorf("round.ticket carries the answer: %s %s", evs[0].Message, evs[0].Data)
			}
		})
	}

	e := newEnv(t)
	curl := e.withJira(nil)
	tickets := e.r.Tickets.(*jira.Client)
	if err := os.Chmod(tickets.TokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	for round, want := range []string{"warn", "info"} {
		e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(int64(845+round), "COMMENTED", "COMMENT").behavior(t)}
		in := e.input(KindInitial)
		in.Round = round + 1
		in.PR.Title = new("[PS-123] Payout methods")
		res, err := e.r.RunRound(e.ctx, in)
		if err != nil || res.Outcome != OutcomePosted {
			t.Fatalf("round %d: RunRound = %+v, %v", round+1, res, err)
		}
		evs := e.eventsOf("round.ticket")
		if len(evs) != round+1 || evs[round].Level != want {
			t.Fatalf("round %d: round.ticket events = %+v, want the last at %s", round+1, evs, want)
		}
		mustContain(t, "round.ticket", evs[round].Message, "jira.token_file", "chmod 600")
	}
	if n := len(curl.Calls); n != 0 {
		t.Errorf("curl ran %d times with a token file open to others", n)
	}
}

// A push that restarts the round on a new head does not read Jira again:
// the issue read for the first head is written into the new head's report
// directory too, and the judge's prompt names that copy.
func TestARestartWritesTheTicketReadOnceForTheNewHead(t *testing.T) {
	e := newEnv(t)
	curl := e.withJira(nil)
	in := e.input(KindInitial)
	in.PR.Title = new("[PS-123] Payout methods")
	e.withRestarts(&in, 2)
	pushAfterCodex := func(f *fakeAgents, run store.Run, text string) error {
		e.waitRun(agents.RoleCodexReview, target, store.RunVerified)
		e.push(head2)
		return nil
	}
	e.ag.behaviors[agents.RoleClaude] = []behavior{pushAfterCodex, writeReport("## P2 on the new head\n")}
	post := e.judgePosts(848, "COMMENTED", "COMMENT")
	post.commit = head2
	e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}

	res, err := e.r.RunRound(e.ctx, in)
	if err != nil || res.Outcome != OutcomePosted || res.TargetSHA != head2 || res.Restarts != 1 {
		t.Fatalf("RunRound = %+v, %v", res, err)
	}
	if keys := askedKeys(curl); len(keys) != 1 {
		t.Errorf("Jira was asked %d times (%q), want once a round", len(keys), keys)
	}
	for _, head := range []string{target, head2} {
		if _, err := os.Stat(filepath.Join(e.dirOf(head), TicketFile)); err != nil {
			t.Errorf("%s of %s: %v", TicketFile, head, err)
		}
	}
	judge := e.ag.submitsFor(agents.RoleJudge)
	if len(judge) != 1 {
		t.Fatalf("judge submits = %d, want 1", len(judge))
	}
	mustContain(t, "judge prompt", judge[0].Text, "\nticket: "+filepath.Join(e.dirOf(head2), TicketFile)+"\n")
}
