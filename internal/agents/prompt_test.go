package agents

import (
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/postreview"
	"github.com/zhuravel/magnum/internal/store"
)

func TestPromptSubmitsAndRecordsRun(t *testing.T) {
	e := newEnv(t)
	e.started()
	e.clock.Add(time.Minute)

	id, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunInitial, "/code-review https://x high")
	if err != nil {
		t.Fatal(err)
	}
	p := e.h.prompts[0]
	if p.Target != "mg-11920-claude-review-5d01cf" || p.Text != "/code-review https://x high" || p.Wait == nil ||
		p.Wait.Timeout != 60*time.Second || len(p.Wait.Until) != 2 || p.Wait.Until[0] != herdr.StatusWorking || p.Wait.Until[1] != herdr.StatusBlocked {
		t.Fatalf("prompt call = %+v (wait %+v)", p, p.Wait)
	}
	r := e.run1(id)
	sess := e.session(RoleClaude)
	if r.State != store.RunWorking || r.SubmittedAt == nil || r.WorkingSeenAt == nil || store.Deref(r.SessionID) != sess.ID ||
		r.Kind != store.RunInitial || r.Round != 1 || r.TargetSHA != e.pr.HeadSHA || r.Identity != "talkable-app" ||
		r.ReviewerLogin != "talkable[bot]" || r.PromptText != "/code-review https://x high" {
		t.Fatalf("run = %+v", r)
	}
	wantReport := filepath.Join(e.cfg.Layout.ReviewDir("talkable", "talkable", 11920, e.pr.HeadSHA), "claude-review.md")
	if store.Deref(r.ReportPath) != wantReport {
		t.Fatalf("report path = %q, want %q", store.Deref(r.ReportPath), wantReport)
	}
	if sess.LastPromptAt == nil || !sess.LastPromptAt.Equal(e.clock.Now()) || sess.IdleTicks != 0 || store.Deref(sess.AgentStatus) != "working" {
		t.Fatalf("session = %+v", sess)
	}
}

func TestSubmitPrecreatedRunAndRefuseResend(t *testing.T) {
	e := newEnv(t)
	e.started()
	run, err := e.m.NewRun(e.ctx, e.pr, RoleJudge, store.RunInitial, 3)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != store.RunPending || run.Round != 3 || store.Deref(run.SessionID) != e.session(RoleJudge).ID {
		t.Fatalf("new run = %+v", run)
	}
	if err := e.m.Submit(e.ctx, run, "judge "+run.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.run1(run.ID); got.PromptText != "judge "+run.ID || got.State != store.RunWorking {
		t.Fatalf("run = %+v", got)
	}
	err = e.m.Submit(e.ctx, run, "again")
	if !errors.Is(err, store.ErrConflict) || len(e.h.prompts) != 1 {
		t.Fatalf("resend: err = %v, prompts = %d; want ErrConflict and no second prompt", err, len(e.h.prompts))
	}
	// Prompt without an explicit round joins the latest round.
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunNudge, "nudge")
	if err != nil {
		t.Fatal(err)
	}
	if r := e.run1(id); r.Round != 3 || r.Kind != store.RunNudge {
		t.Fatalf("nudge run = %+v", r)
	}
}

func TestPromptErrorMapping(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		status    herdr.Status
		want      error
		wantState string
	}{
		{"blocked before send", &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentBlocked, Message: "blocked"}, "", ErrBlocked, store.RunFailed},
		{"stalled", &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentPromptStalled, Message: "stalled"}, "", ErrStalled, store.RunSubmitted},
		{"server timeout", &herdr.Error{Method: "agent.prompt", Code: herdr.CodeTimeout, Message: "timeout"}, "", ErrTimeout, store.RunSubmitted},
		{"client timeout", herdr.ErrTimeout, "", ErrTimeout, store.RunSubmitted},
		{"unavailable", herdr.ErrUnavailable, "", herdr.ErrUnavailable, store.RunFailed},
		{"blocked after send", nil, herdr.StatusBlocked, ErrBlocked, store.RunSubmitted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.started()
			if tc.err != nil {
				e.h.errs["AgentPrompt"] = tc.err
			}
			if tc.status != "" {
				e.h.promptStatus = tc.status
			}
			id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "go")
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if id == "" {
				t.Fatal("Prompt must return the run id even on error")
			}
			r := e.run1(id)
			if r.State != tc.wantState {
				t.Fatalf("run state = %s, want %s", r.State, tc.wantState)
			}
			if tc.err != nil && r.Error == nil {
				t.Fatal("run error should be recorded")
			}
		})
	}
}

func TestPromptRefusals(t *testing.T) {
	e := newEnv(t)
	e.workspace()
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "x"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("agent not started: err = %v, want ErrNoSession", err)
	}
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleCodexReview, store.RunInitial, "x"); !errors.Is(err, ErrNotAgent) {
		t.Fatalf("shell role: err = %v, want ErrNotAgent", err)
	}
	for _, r := range []Role{RoleJudge, RoleClaude} {
		if err := e.m.StartAgent(e.ctx, e.pr, e.spec(r), store.Deref(e.session(r).HerdrPaneID), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.st.UpdatePR(e.ctx, e.pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", e.clock.Now()) }); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(5 * time.Minute)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "x"); !errors.Is(err, ErrHumanActive) {
		t.Fatalf("human cooldown: err = %v, want ErrHumanActive", err)
	}
	if len(e.h.prompts) != 0 {
		t.Fatal("no prompt may be sent during the human cooldown")
	}
	e.clock.Add(11 * time.Minute)
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "x"); err != nil {
		t.Fatalf("after cooldown: %v", err)
	}
}

func TestRunShell(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	role := e.spec(RoleCodexReview)
	line, err := ShellLine(role, ShellData{BaseRef: "origin/master", ReportPath: "/tmp/codex.md", Marker: DoneMarker("r-1")})
	if err != nil {
		t.Fatal(err)
	}
	pane := ws.Panes[RoleCodexReview]
	e.h.waitLine = "MAGNUM_DONE_r-1 0"
	status, err := e.m.RunShell(e.ctx, e.pr, role, pane, line, DoneMarker("r-1"), 40*time.Minute)
	if err != nil || status != 0 {
		t.Fatalf("RunShell = %d, %v", status, err)
	}
	if len(e.h.runs) != 1 || e.h.runs[0].Pane != pane || e.h.runs[0].Command != line {
		t.Fatalf("pane runs = %+v", e.h.runs)
	}
	if got := e.keysSent(); len(got) != 1 || got[0] != pane+":ctrl+u" {
		t.Fatalf("keys = %v, want the line editor cleared first", got)
	}
	w := e.h.waits[0]
	if !w.Regex || w.Match != `(?m)^MAGNUM_DONE_r-1(?:[ \t]+\d+)?[ \t]*$` || w.Timeout != 40*time.Minute || w.Source != herdr.SourceRecentUnwrapped {
		t.Fatalf("wait = %+v", w)
	}
	re := regexp.MustCompile(w.Match)
	for text, want := range map[string]bool{
		"out\nMAGNUM_DONE_r-1 0\n": true, "MAGNUM_DONE_r-1 127": true, "MAGNUM_DONE_r-1\n": true,
		"$ x; printf '\\nMAGNUM_DONE_r-1 %d\\n' \"$?\"": false, "MAGNUM_DONE_r-10 0": false, "reportMAGNUM_DONE_r-1 0": false,
	} {
		if got := re.MatchString(text); got != want {
			t.Errorf("marker pattern on %q = %v, want %v", text, got, want)
		}
	}
	if got := e.h.callsWith("WaitIdleShell " + pane); len(got) != 1 {
		t.Fatalf("idle-shell check = %v", got)
	}
	s := e.session(RoleCodexReview)
	if s.State != store.SessionLive || s.LastPromptAt == nil || store.Deref(s.AgentKind) != KindShell {
		t.Fatalf("codex-review session = %+v", s)
	}

	for line, want := range map[string]int{"MAGNUM_DONE_r-1 2": 2, "MAGNUM_DONE_r-1": ShellStatusUnknown, "": ShellStatusUnknown} {
		e.h.waitLine = line
		if line == "" {
			e.h.waitLine = "something else"
		}
		if status, err := e.m.RunShell(e.ctx, e.pr, role, pane, "x", DoneMarker("r-1"), time.Minute); err != nil || status != want {
			t.Errorf("matched %q: status = %d, %v; want %d", line, status, err, want)
		}
	}

	e.h.errs["PaneWaitOutput"] = &herdr.Error{Method: "pane.wait_for_output", Code: herdr.CodeTimeout, Message: "t"}
	if _, err := e.m.RunShell(e.ctx, e.pr, role, pane, line, DoneMarker("r-1"), time.Minute); !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout: err = %v, want ErrTimeout", err)
	}
	e.h.busy[pane] = true
	if _, err := e.m.RunShell(e.ctx, e.pr, role, pane, line, DoneMarker("r-2"), time.Minute); !errors.Is(err, ErrBusy) {
		t.Fatalf("busy shell: err = %v, want ErrBusy", err)
	}
	if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleClaude), pane, line, DoneMarker("r-3"), time.Minute); err == nil {
		t.Fatal("an agent role: want an error")
	}
}

func TestRunShellHonoursHumanCooldown(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	pane := ws.Panes[RoleCodexReview]
	if err := e.st.UpdatePR(e.ctx, e.pr.ID, func(u *store.PRUpdate) { u.Set("human_active_at", e.clock.Now()) }); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(5 * time.Minute)
	if _, err := e.m.RunShell(e.ctx, e.pr, e.spec(RoleCodexReview), pane, "x", DoneMarker("r-1"), time.Minute); !errors.Is(err, ErrHumanActive) {
		t.Fatalf("err = %v, want ErrHumanActive", err)
	}
	if len(e.h.runs) != 0 {
		t.Fatal("nothing may be typed during the human cooldown")
	}
}

// failSQL makes the registry refuse the statements trigger names (a
// BEFORE trigger body's condition), for the rest of the test.
func (e *env) failSQL(name, on string) {
	e.t.Helper()
	if _, err := e.st.DB().ExecContext(e.ctx, "CREATE TRIGGER "+name+" BEFORE "+on+
		" BEGIN SELECT RAISE(ABORT, 'disk I/O error'); END"); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) logged(t *testing.T, want ...string) {
	t.Helper()
	for _, l := range e.logs.all() {
		if !slices.ContainsFunc(want, func(w string) bool { return !strings.Contains(l, w) }) {
			return
		}
	}
	t.Fatalf("logs = %q, want a line with %q", e.logs.all(), want)
}

// A refused prompt whose run cannot be marked abandoned says so in the log:
// the run stays pending and counts as in flight.
func TestARefusedPromptLogsAFailedAbandon(t *testing.T) {
	e := newEnv(t)
	e.workspace()
	e.failSQL("fail_abandon", "UPDATE OF state ON runs WHEN NEW.state = 'abandoned'")
	id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "x")
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("Prompt = %v, want ErrNoSession", err)
	}
	e.logged(t, "abandon", id, "disk I/O error")
}

// A prompt whose session row cannot record it (last_prompt_at) is still
// sent, and the failure is logged; so is a shell role's.
func TestAPromptTheSessionCannotRecordIsLogged(t *testing.T) {
	e := newEnv(t)
	ws := e.started()
	e.failSQL("fail_prompted", "UPDATE OF last_prompt_at ON sessions")
	if _, err := e.m.Prompt(e.ctx, e.pr, RoleClaude, store.RunInitial, "x"); err != nil {
		t.Fatal(err)
	}
	e.logged(t, "claude-review", "disk I/O error")

	role := e.spec(RoleCodexReview)
	e.h.waitLine = "MAGNUM_DONE_r-1 0"
	if _, err := e.m.RunShell(e.ctx, e.pr, role, ws.Panes[RoleCodexReview], "x", DoneMarker("r-1"), time.Minute); err != nil {
		t.Fatal(err)
	}
	e.logged(t, "codex-review", "disk I/O error")
}

// The fix prompt is one paragraph of facts: it opens with the fix skill's
// startup copy, then names the PR, its head and the branch to push to, the
// posted review, its file and the login that posted it, the report path with
// the run marker and the db-lock line. It carries no PR text: the title is
// never in it.
func TestFixPromptRendersTheSkillLinkAndFacts(t *testing.T) {
	e := newEnv(t)
	skill := filepath.Join(e.cfg.Layout.Home, "skills", "magnum-fix", "SKILL.md")
	writeFile(t, skill, "# fix skill\n", 0o600)
	if _, err := e.cfg.SnapshotPrompts(filepath.Join(t.TempDir(), "skill"), t0); err != nil {
		t.Fatal(err)
	}
	skillCopy := e.cfg.SkillFile(skill)
	if skillCopy == skill {
		t.Fatalf("the snapshot holds no copy of %s", skill)
	}
	role := Role(config.RoleClaudeFix)
	run, err := e.m.NewRun(e.ctx, e.pr, role, store.RunFix, 2)
	if err != nil {
		t.Fatal(err)
	}
	in := FixPromptInput{
		HeadRef:       "feature/coupon-expiry",
		ReviewURL:     "https://github.com/talkable/talkable/pull/11920#pullrequestreview-4100000001",
		ReviewFile:    "/state/reviews/talkable/talkable/11920/abc1234/review.json",
		ExternalFile:  "/state/reviews/talkable/talkable/11920/abc1234/external-threads.json",
		ReviewerLogin: "talkable[bot]",
		DBLockCommand: "/opt/magnum/bin/magnum db-lock --checkout /p/talkable.review3 --role claude-fix --",
	}
	got, err := e.m.FixPrompt(e.pr, role, run, in)
	if err != nil {
		t.Fatal(err)
	}
	if want := "[$magnum-fix](" + skillCopy + ") Read " + skillCopy + " first, by itself, "; !strings.HasPrefix(got, want) {
		t.Errorf("the fix prompt does not start with %q:\n%s", want, got)
	}
	for _, want := range []string{e.pr.URL, "`" + e.pr.HeadSHA + "`", "HEAD:" + in.HeadRef, in.ReviewURL, in.ReviewFile,
		"`" + in.ReviewerLogin + "`", store.Deref(run.ReportPath), ReportMarker(run.ID), "`" + in.DBLockCommand + " <command>`",
		"The other reviewers' open threads, review bodies and comments on the PR are in " + in.ExternalFile +
			": after the review's findings, handle each of its items that nobody answered."} {
		if !strings.Contains(got, want) {
			t.Errorf("the fix prompt lacks %q:\n%s", want, got)
		}
	}
	// Without the file (magnum could not list them) the prompt names none.
	noExternal := in
	noExternal.ExternalFile = ""
	if got, err := e.m.FixPrompt(e.pr, role, run, noExternal); err != nil || strings.Contains(got, "other reviewers") {
		t.Errorf("the fix prompt without an external file = %q, %v", got, err)
	}
	if strings.Contains(got, store.Deref(e.pr.Title)) {
		t.Errorf("the fix prompt carries the PR title:\n%s", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("the fix prompt is not one paragraph:\n%s", got)
	}

	if _, err := e.m.FixPrompt(e.pr, RoleClaude, run, in); err == nil {
		t.Error("FixPrompt rendered the prompt of claude-review, which is no fix role")
	}
	noReport := run
	noReport.ReportPath = nil
	if _, err := e.m.FixPrompt(e.pr, role, noReport, in); err == nil {
		t.Error("FixPrompt rendered a prompt for a run without a report path")
	}
	// The prompt puts the branch, unquoted, into the push the fixer runs:
	// a branch name a shell would read is refused, and never quoted.
	unsafe := in
	unsafe.HeadRef = "fix$(touch x)"
	if got, err := e.m.FixPrompt(e.pr, role, run, unsafe); !errors.Is(err, ErrUnsafeHeadRef) || strings.Contains(err.Error(), unsafe.HeadRef) {
		t.Errorf("FixPrompt with branch %q = %q, %v; want ErrUnsafeHeadRef without the name", unsafe.HeadRef, got, err)
	}
}

// The fix skill is never rendered, and magnum reads what the fixer writes:
// the re-review classifies its replies (pipeline's classifyReply) by their
// first words and the fix job parses its report by its headings. The skill
// spells out each of them and carries no template action.
func TestTheFixSkillNamesTheRepliesAndTheReportMagnumReads(t *testing.T) {
	skill := string(magnum.FixSkill)
	for _, want := range []string{
		"name: magnum-fix",
		`"Fixed in <sha7>: `, `"Already fixed in <sha7>"`, `"Fixed: the description now `, `"Not a bug: `, `"Won't fix: `,
		"\n## Pushed\n", "\n## Replied\n", "\n## Needs you\n", "\n- <review id> description: <reason>\n",
		"resolveReviewThread", "pulls/{number}/comments/{comment_id}/replies",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the fix skill lacks %q", want)
		}
	}
	if strings.Contains(skill, "{{.") {
		t.Error("the fix skill carries a template action, but nothing renders it")
	}
}

// The fixer proves each guard its fix adds with a spec that fails without
// it, always fixes a regression (not every finding of the PR's diff, which
// is every finding the review reports), applies a simplification that keeps
// behaviour without scoring it (a simplification nets 1 by construction, so
// the bar of 3 declined four of five in a week) while a defect keeps its
// scoring, and ends every reply with the fixer's marker, by which the retro
// tells its replies from the author's (learn.OwnThreads): the reply command
// carries it after the text.
func TestTheFixSkillProvesEachGuardFixesRegressionsAndSignsItsReplies(t *testing.T) {
	skill := string(magnum.FixSkill)
	for _, want := range []string{
		"For each guard, scope, permission or flag the fix adds, delete it once, run the new spec, see it fail, and restore it",
		"add `; <spec> fails without it` for each",
		"These are always `fix`, whatever the score: a regression (behaviour that works at the base and breaks at the head), " +
			"a broken reference, a failing test and a crash.",
		"A simplification the review posted is `fix` when it keeps behaviour, stays in the PR's own lines and the specs that cover the changed code pass; " +
			"it is `won't fix` only with a concrete risk: it drops a guard, changes behaviour or reaches outside the PR. Do not score it. A defect keeps its scoring.",
		"`fix` with a net score of 3 or more, a simplification that is `fix`, and each finding that section 3 always makes `fix`",
		"`fix` with a net score below 3 that is no simplification: make it `won't fix`",
		"<<'REPLY'\n<reply>\n\n" + postreview.FixReplyMarker + "\nREPLY\n",
		"End every reply with a blank line and the line `" + postreview.FixReplyMarker + "`",
		// The engine reads these words to say what each finding left to the author asks of them.
		"- <comment id> <priority> <path>:<line>: fixed in <sha7>. <why it needs you>\n- <comment id> <priority> <path>:<line>: not fixed. <reason>\n",
		"then `fixed in <sha7>.` for a finding you fixed and pushed but did not reply in (its thread is answered already, or the reply failed) " +
			"or `not fixed.` for one you did not fix, and then the reason",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the fix skill lacks %q", want)
		}
	}
	if strings.Contains(skill, "a defect that the PR's diff introduced") {
		t.Error("the fix skill still makes every finding of the PR's diff a fix whatever its score")
	}
}

// On talkable#12054 the fixer fixed magnum's four findings and never saw
// another reviewer's open follow-up an hour older, since the skill had it
// act only in the reviewer login's threads and never in one a human started
// (2026-10-20, spec 93). It now acts in magnum's threads and in the
// unanswered threads of other reviewers that the external file lists,
// judges each external item by the same policy, replies once to each that
// asks for an answer, in its thread or for a review body or comment in one
// issue comment that quotes and links it, every reply signed with the fix
// marker, and never resolves or minimizes a thread someone else started,
// replies where the author already has, or edits anyone else's comment.
// Right before each reply it reads the item's thread, or the comments and
// reviews after it, again, and skips one the author answered while it
// worked, counting only the author's and magnum's logins (the file names
// them) and ignoring outsiders as the file does. The report lists every
// external item.
func TestTheFixSkillAnswersEveryUnansweredItemOfOtherReviewers(t *testing.T) {
	skill := string(magnum.FixSkill)
	for _, want := range []string{
		"- Act only in magnum's threads (the reviewer login's), in the unanswered threads of other reviewers that the external file lists, " +
			"and in one issue comment for each unanswered review body or comment it lists. " +
			"Never resolve or minimize a thread someone else started: its reviewer resolves it. " +
			"Never reply in another reviewer's thread whose last comment is already the author's. " +
			"Reply at most once to each item, and never edit, hide or delete anyone else's comment.",
		"6. Read the external file, if the prompt names one.",
		"An item with `answered` set has its answer already: leave it. Every other item is an external item.",
		"Then judge each external item as a finding (section 3) and apply the same policy to it",
		"A choice of business logic or product behaviour is step 6, as an author's call is: do not fix it, and do not reply.",
		"An item that asks for nothing gets no verdict and no reply: a bot's status, CI or summary comment, a thanks, an approval.",
		"Each external item that gets a reply gets exactly one, in its own thread, or for a review body or a comment one issue comment (section 6).",
		"the threads of the external file that are not `answered`. Post the reply to the thread's first comment: the review comment, or an external thread's `comment_id`.",
		"repos/{owner}/{repo}/issues/{number}/comments -F body=@- <<'REPLY'\n> <the item's first line>\n<the item's url>\n\n<reply>\n\n" + postreview.FixReplyMarker + "\nREPLY\n",
		"Never resolve or minimize a thread someone else started, even one you fixed: its reviewer resolves it.",
		"\n## Other reviewers\n",
		// The author may answer while the fixer works: it looks again right before each reply.
		"Right before each reply to an external item, look again: the author may have answered it while you worked.",
		// Only the author and magnum's logins answer an item, an outsider's comment is ignored as in the file,
		// and a review body the author posts while the fixer works answers a review body or a comment.
		"`author` is the PR author's login, which you post as, and `fixer_logins` are magnum's logins: only these two answer an item.",
		"Only `author` and `fixer_logins` (section 2) answer an item; a marker or a reply from anyone else answers nothing.",
		"--jq '.[] | select(.id == <comment_id> or .in_reply_to_id == <comment_id>) | [.user.login, .user.type, .author_association, .body]'",
		"Ignore an outsider's comment, as the file does: one whose `author_association` is not `OWNER`, `MEMBER` or `COLLABORATOR` and whose author is no `Bot`, not `author` and none of `fixer_logins`.",
		"Skip the thread when the latest of the others is by `author`, or is by one of `fixer_logins` and carries the fix marker.",
		"--jq '.[] | select(.created_at > \"<created_at>\") | [.user.login, .body]'",
		"gh api repos/{owner}/{repo}/pulls/{number}/reviews --paginate --jq '.[] | select(.submitted_at > \"<created_at>\") | [.user.login, .body]'",
		"Skip the item when one of them is the author's own word: by `author`, with a body and no `<!-- magnum:` marker in it, a review body included.",
		"Skip it too when a comment by `author` or one of `fixer_logins` carries the fix marker and its line under the quote is the item's `url`.",
		"read it whole with `gh api` by its `id` (`repos/{owner}/{repo}/pulls/comments/<id>` for a thread's comment",
		"`## Other reviewers`: one line per item of the external file, answered ones included",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the fix skill lacks %q", want)
		}
	}
	for _, gone := range []string{"Act only in the reviewer login's threads", "Never reply to, resolve or minimize a thread that a human started",
		"Reply nowhere else: no issue comment,", "a thread a human started"} {
		if strings.Contains(skill, gone) {
			t.Errorf("the fix skill still says %q", gone)
		}
	}
}

// A fix starts on a review that found nothing when another reviewer's
// thread or comment waits for an answer (spec 96): its prompt says the
// review found nothing and hands the fixer the external file's unanswered
// items alone, with no "Fix the findings" to run, and the skill takes the
// external items as the run's whole work, a checkout at the wrong head
// leaving them to the author.
func TestTheFixPromptOfAReviewThatFoundNothingHandsOverTheOtherReviewersAlone(t *testing.T) {
	e := newEnv(t)
	role := Role(config.RoleClaudeFix)
	run, err := e.m.NewRun(e.ctx, e.pr, role, store.RunFix, 2)
	if err != nil {
		t.Fatal(err)
	}
	in := FixPromptInput{
		HeadRef:       "feature/coupon-expiry",
		ReviewURL:     "https://github.com/talkable/talkable/pull/11920#pullrequestreview-4100000001",
		ReviewFile:    "/state/reviews/talkable/talkable/11920/abc1234/review.json",
		ExternalFile:  "/state/reviews/talkable/talkable/11920/abc1234/external-threads.json",
		ReviewerLogin: "talkable[bot]",
		DBLockCommand: "/opt/magnum/bin/magnum db-lock --checkout /p/talkable.review3 --role claude-fix --",
		NoFindings:    true,
	}
	got, err := e.m.FixPrompt(e.pr, role, run, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"The review " + in.ReviewURL + ", which `" + in.ReviewerLogin + "` posted on " + e.pr.URL + ", found nothing to fix. " +
			"Its event, body and comments are in " + in.ReviewFile + ".",
		"The other reviewers' open threads, review bodies and comments on the PR are in " + in.ExternalFile +
			": handle each of its items that nobody answered.",
		"HEAD:" + in.HeadRef, ReportMarker(run.ID),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the fix prompt of a review that found nothing lacks %q:\n%s", want, got)
		}
	}
	for _, gone := range []string{"Fix the findings", "after the review's findings"} {
		if strings.Contains(got, gone) {
			t.Errorf("the fix prompt of a review that found nothing says %q:\n%s", gone, got)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("the fix prompt is not one paragraph:\n%s", got)
	}

	skill := string(magnum.FixSkill)
	for _, want := range []string{
		"When the prompt says that the review found nothing to fix, the external items are this run's whole work: judge, fix and answer them alone.",
		"When the review found nothing to fix, list every unanswered item of the external file (step 6) under `## Needs you` instead, " +
			"each as section 7 writes its entry, with that reason.",
	} {
		if !strings.Contains(skill, want) {
			t.Errorf("the fix skill lacks %q", want)
		}
	}
}

// A review's Fix that would undo the PR's direction, or that picks between
// fixes users would tell apart, ends with the review skill's
// `This is the author's call.`: the fixer never picks an option, whatever
// it thinks of the finding, and always leaves it under Needs you with the
// options as its question (2026-10-20).
func TestTheFixSkillLeavesTheAuthorsCallToTheAuthor(t *testing.T) {
	const phrase = "`This is the author's call.`"
	want := "A finding whose **Fix** ends with " + phrase + " is such a finding, whatever its verdict and score: " +
		"do not fix it, write `not fixed.` and ask which of its options to take, naming each."
	if !strings.Contains(string(magnum.FixSkill), want) {
		t.Errorf("the fix skill lacks %q", want)
	}
	if !strings.Contains(string(magnum.Skill), "and ends "+phrase) {
		t.Errorf("the review skill no longer ends such a Fix with %s, which the fix skill matches", phrase)
	}
}

// The fix loop answers other reviewers' threads and comments, bots'
// included, as the PR's author, and its reply rules kept nothing out of a
// reply (2026-10-21): a reply never carries a secret, a token, an
// environment value or a file from outside the checkout.
func TestTheFixSkillKeepsSecretsOutOfReplies(t *testing.T) {
	const want = "A reply never carries a secret, a token, an environment value or a file from outside the checkout."
	if !strings.Contains(string(magnum.FixSkill), want) {
		t.Errorf("the fix skill lacks %q", want)
	}
}

// A prompt herdr refuses because the agent is gone carries the error the
// CLI printed in the pane when it died at its launch (a flag the launch
// repeated), so the round's error and the attention reason say why and not
// only herdr's code; a pane without an error line adds nothing.
func TestARefusedPromptToAnAgentGoneCarriesThePanesError(t *testing.T) {
	const want = "the pane shows: error: the argument '--search' cannot be used multiple times"
	cases := map[string]struct {
		pane     string
		wantLine bool
	}{
		"codex died at its launch": {"$ codex --model gpt-6.1-sol --search\n" +
			"Found existing alias for \"codex\". You should use: \"cdx\"\n" +
			"error: the argument '--search' cannot be used multiple times\n\n" +
			"Usage: codex [OPTIONS] [PROMPT]\n$ ", true},
		"only the prompt": {"$ codex --model gpt-6.1-sol\n$ ", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			ws := e.started()
			pane := ws.Panes[RoleJudge]
			e.h.errs["AgentPrompt"] = &herdr.Error{Method: "agent.prompt", Code: herdr.CodeAgentNotFound, Message: "agent target mg-11920-codex-judge-5d01cf not found"}
			e.h.reads[pane] = tc.pane
			id, err := e.m.Prompt(e.ctx, e.pr, RoleJudge, store.RunInitial, "go")
			if err == nil || !herdr.IsCode(err, herdr.CodeAgentNotFound) {
				t.Fatalf("err = %v, want agent_not_found", err)
			}
			if strings.Contains(err.Error(), want) != tc.wantLine {
				t.Fatalf("err = %v, want the pane line: %v", err, tc.wantLine)
			}
			r := e.run1(id)
			if r.State != store.RunFailed || strings.Contains(store.Deref(r.Error), want) != tc.wantLine {
				t.Fatalf("run = %+v", r)
			}
			// The engine reads the line back from the run's error, to tell
			// launches of several PRs that end the same way.
			if got, line := PaneErrorOf(store.Deref(r.Error)), strings.TrimPrefix(want, "the pane shows: "); tc.wantLine && got != line || !tc.wantLine && got != "" {
				t.Fatalf("the line read back from %q = %q", store.Deref(r.Error), got)
			}
			if got := e.h.callsWith("PaneRead " + pane); len(got) != 1 {
				t.Fatalf("pane reads = %v, want one", got)
			}
		})
	}
}

// The whiteboard prompt carries the facts the writer needs and no PR text:
// the PR's URL and name, the reviewed head, the checkout it reads, the base
// it diffs from, the posted review and its file, and the report it writes;
// with the whiteboard of an earlier head it moves that one
// (session_set_target), else it creates one in the background.
func TestWhiteboardPromptRendersTheFacts(t *testing.T) {
	e := newEnv(t)
	role := Role(config.RoleClaudeWhiteboard)
	run, err := e.m.NewRun(e.ctx, e.pr, role, store.RunWhiteboard, 2)
	if err != nil {
		t.Fatal(err)
	}
	in := WhiteboardPromptInput{
		Owner: "talkable", Repo: "talkable",
		Checkout:   "/p/talkable.review3",
		BaseRef:    "origin/master",
		ReviewURL:  "https://github.com/talkable/talkable/pull/11920#pullrequestreview-4100000001",
		ReviewFile: "/state/reviews/talkable/talkable/11920/abc1234/review.json",
	}
	got, err := e.m.WhiteboardPrompt(e.pr, role, run, in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{e.pr.URL, "`" + e.pr.HeadSHA + "`", "`talkable/talkable#11920: `", "repositoryPath `" + in.Checkout + "`",
		"`git merge-base origin/master " + e.pr.HeadSHA + "`", in.ReviewURL, in.ReviewFile, store.Deref(run.ReportPath),
		"`session_create`", "open `false`", "`head: " + e.pr.HeadSHA + "`"} {
		if !strings.Contains(got, want) {
			t.Errorf("the whiteboard prompt lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "session_set_target") {
		t.Errorf("a first whiteboard moves one:\n%s", got)
	}
	if strings.Contains(got, store.Deref(e.pr.Title)) || strings.Contains(got, "\n") {
		t.Errorf("the whiteboard prompt carries the PR title or is not one paragraph:\n%s", got)
	}

	in.Session = "6f0c2b9e-4d1a-4c3e-9b7a-2e5d8f1a0c3b"
	got, err = e.m.WhiteboardPrompt(e.pr, role, run, in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "`session_set_target` (sessionId `"+in.Session+"`") || strings.Contains(got, "`session_create`") {
		t.Errorf("the update prompt does not move the whiteboard %s:\n%s", in.Session, got)
	}

	if _, err := e.m.WhiteboardPrompt(e.pr, RoleClaude, run, in); err == nil {
		t.Error("WhiteboardPrompt rendered the prompt of claude-review, which is no whiteboard role")
	}
	noReport := run
	noReport.ReportPath = nil
	if _, err := e.m.WhiteboardPrompt(e.pr, role, noReport, in); err == nil {
		t.Error("WhiteboardPrompt rendered a prompt for a run without a report path")
	}
	noCheckout := in
	noCheckout.Checkout = ""
	if _, err := e.m.WhiteboardPrompt(e.pr, role, run, noCheckout); err == nil {
		t.Error("WhiteboardPrompt rendered a prompt without the checkout the whiteboard reads")
	}
}
