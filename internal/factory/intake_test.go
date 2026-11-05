package factory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/stats"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/jira"
)

// jiraSite is a Jira site behind a fake curl: the issues it knows by key,
// the answer of its title search, and curl's exit code while it is down
// (0 = up). It records the issues and the searches curl asked for.
type jiraSite struct {
	mu       sync.Mutex
	issues   map[string]string
	search   string
	down     int
	calls    int            // every curl call, down or up
	asked    []string       // the keys of the issue reads
	searched []string       // the JQL of the searches
	refused  map[string]int // an HTTP status per key, which its read answers with
}

const jiraTestSite = "https://example.atlassian.net"

// issueJSON is an issue's answer (GET /rest/api/2/issue/<key>).
func issueJSON(key, summary, typ, desc string) string {
	b, _ := json.Marshal(map[string]any{"key": key, "fields": map[string]any{
		"summary": summary, "issuetype": map[string]string{"name": typ}, "status": map[string]string{"name": "To Do"},
		"priority": map[string]string{"name": "High"}, "description": desc,
	}})
	return string(b)
}

// searchAnswer is the search's answer listing issues of summary by key.
func searchAnswer(issues ...[2]string) string {
	var out []map[string]any
	for _, is := range issues {
		out = append(out, map[string]any{"key": is[0], "fields": map[string]any{"summary": is[1],
			"issuetype": map[string]string{"name": "Story"}, "status": map[string]string{"name": "To Do"}}})
	}
	b, _ := json.Marshal(map[string]any{"issues": out, "isLast": true})
	return string(b)
}

func (s *jiraSite) curl(c execx.Cmd) (execx.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.down != 0 {
		return execx.Result{Code: s.down}, &execx.ExitError{Cmd: c, Code: s.down}
	}
	var raw string
	for line := range strings.SplitSeq(string(c.Stdin), "\n") {
		if v, ok := strings.CutPrefix(line, "url = "); ok {
			raw = strings.Trim(v, `"`)
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return execx.Result{}, err
	}
	switch {
	case strings.HasPrefix(u.Path, "/rest/api/2/issue/"):
		key := strings.TrimPrefix(u.Path, "/rest/api/2/issue/")
		s.asked = append(s.asked, key)
		if code, ok := s.refused[key]; ok {
			return execx.Result{Stdout: []byte(fmt.Sprintf("{}\n%d", code))}, nil
		}
		if body, ok := s.issues[key]; ok {
			return execx.Result{Stdout: []byte(body + "\n200")}, nil
		}
		return execx.Result{Stdout: []byte(`{"errorMessages":["Issue does not exist"]}` + "\n404")}, nil
	case u.Path == "/rest/api/3/search/jql":
		s.searched = append(s.searched, u.Query().Get("jql"))
		body := s.search
		if body == "" {
			body = searchAnswer()
		}
		return execx.Result{Stdout: []byte(body + "\n200")}, nil
	}
	return execx.Result{Stdout: []byte("{}\n404")}, nil
}

func (s *jiraSite) reads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.asked)
}

// refuse makes the read of key answer with HTTP status code.
func (s *jiraSite) refuse(key string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refused == nil {
		s.refused = map[string]int{}
	}
	s.refused[key] = code
}

func (s *jiraSite) setDown(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = code
}

// intakeRig is a harness with one factory repository (talkable/talkable),
// a gh identity (zhuravel), a Jira site behind a fake curl, a memory the
// test sets and a fake git and gh for the overlap line.
type intakeRig struct {
	*harness
	site   *jiraSite
	client *jira.Client
	run    *execx.Fake
	memMu  sync.Mutex
	swap   int64 // percent of the swap used
	// refs are the refs the main clone has (git rev-parse answers them).
	refs map[string]bool
}

func newIntakeRig(t *testing.T) *intakeRig {
	t.Helper()
	r := &intakeRig{site: &jiraSite{issues: map[string]string{
		"PR-12345": issueJSON("PR-12345", "Credit expiry reminder email", "Story", "Send advocates a reminder before their referral credit expires."),
		"PR-12346": issueJSON("PR-12346", "Points vanish after a refund", "Bug", "A refund takes the points away twice."),
	}}}
	token := filepath.Join(t.TempDir(), "jira.token")
	if err := os.WriteFile(token, []byte("ATATT3xFfGF0-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r.client = &jira.Client{Runner: &execx.Fake{Rules: []execx.Rule{{Prefix: []string{"curl"}, Fn: r.site.curl}}},
		URL: jiraTestSite, User: "alice@example.com", TokenFile: token}
	r.run = &execx.Fake{Rules: []execx.Rule{
		{Prefix: []string{"git", "log"}},
		{Prefix: []string{"gh", "pr", "list"}, Result: execx.Result{Stdout: []byte("[]")}},
	}}
	r.refs = map[string]bool{}
	git := gitx.New(&execx.Fake{Rules: []execx.Rule{{Prefix: []string{"git"}, Fn: r.revParse}}})
	r.harness = newHarness(t, func(d *Deps) {
		d.Jira = r.client
		d.Run = r.run
		d.Memory = r.memory
		d.Git = git
	})
	r.cfg.Jira = config.Jira{URL: jiraTestSite, User: "alice@example.com", TokenFile: token}
	r.cfg.Board.Trackers = []string{jiraTestSite + "/browse/PR-{num}"}
	r.cfg.Identities = []config.Identity{{Name: "me", Kind: "gh", Login: "zhuravel"}}
	r.cfg.Factory.Repos = []config.FactoryRepo{{Repo: "talkable/talkable", Base: "master", MainClone: t.TempDir(),
		Copies: filepath.Join(t.TempDir(), "talkable.factory{n}"), CopyName: "factory{n}", MaxCopies: 3}}
	r.stage(config.FactoryStageAgents, idleStage) // intake's tests stop where the first stage begins
	return r
}

// revParse answers git rev-parse --verify of the rig's refs, in the main
// clone only.
func (r *intakeRig) revParse(c execx.Cmd) (execx.Result, error) {
	n := len(c.Args)
	if n < 3 || c.Args[0] != "-C" || c.Args[1] != r.cfg.Factory.Repos[0].MainClone || !slices.Contains(c.Args, "rev-parse") {
		return execx.Result{Code: 128}, &execx.ExitError{Cmd: c, Code: 128}
	}
	if r.refs[strings.TrimSuffix(c.Args[n-1], "^{commit}")] {
		return execx.Result{Stdout: []byte(strings.Repeat("a", 40) + "\n")}, nil
	}
	return execx.Result{Code: 1}, &execx.ExitError{Cmd: c, Code: 1}
}

func (r *intakeRig) memory(context.Context) engine.MemorySample {
	r.memMu.Lock()
	defer r.memMu.Unlock()
	return engine.MemorySample{Level: engine.MemoryNormal, Swap: &engine.MemorySwap{UsedBytes: r.swap << 30, TotalBytes: 100 << 30}}
}

func (r *intakeRig) setSwap(pct int64) {
	r.memMu.Lock()
	defer r.memMu.Unlock()
	r.swap = pct
}

func (r *intakeRig) start(text string) (registry.Task, error) {
	r.t.Helper()
	return r.e.start(r.ctx, StartPayload{Text: text})
}

// mustStart starts text and waits for the start's work off the loop (the
// overlap line, the question of a prompt).
func (r *intakeRig) mustStart(text string) registry.Task {
	r.t.Helper()
	task, err := r.start(text)
	if err != nil {
		r.t.Fatalf("start %q: %v", text, err)
	}
	r.e.waitBackground()
	return task
}

func (r *intakeRig) tasks() []registry.Task {
	r.t.Helper()
	ts, err := r.reg.Tasks(r.ctx)
	if err != nil {
		r.t.Fatal(err)
	}
	return ts
}

func (r *intakeRig) taskFile(id int64, name string) string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.layout.FactoryTaskDir(id), name))
	if err != nil {
		r.t.Fatalf("tasks/%d/%s: %v", id, name, err)
	}
	return string(b)
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// A second start for a key that a task holds returns that task and makes
// none: one that is at work says which task runs it, one whose PR is open
// names the PR. The start request answers with the same words, and is no
// failure.
func TestStartForAKeyThatRunsReturnsTheExistingTask(t *testing.T) {
	r := newIntakeRig(t)
	for i := range 6 {
		if _, err := r.reg.CreateTask(r.ctx, registry.Task{Prompt: fmt.Sprintf("filler %d", i), Kind: "bug", State: registry.TaskStopped}); err != nil {
			t.Fatal(err)
		}
	}
	first := r.mustStart("PR-12345")
	if first.ID != 7 {
		t.Fatalf("the first start made task %d, want 7", first.ID)
	}
	reads := r.site.reads()

	got, err := r.start("PR-12345 only the reminder")
	if !errors.Is(err, ErrKeyRuns) || err.Error() != "PR-12345 already runs as task 7" {
		t.Fatalf("a second start = %v, want ErrKeyRuns saying PR-12345 already runs as task 7", err)
	}
	if got.ID != 7 {
		t.Fatalf("a second start returned task %d, want 7", got.ID)
	}
	if n := len(r.tasks()); n != 7 {
		t.Fatalf("%d tasks after a second start, want 7: no new row", n)
	}
	if n := r.site.reads(); n != reads {
		t.Fatalf("a second start read Jira again (%d reads, want %d)", n, reads)
	}

	if err := r.reg.TransitionTask(r.ctx, first.ID, nil, registry.TaskPublished, func(u *registry.TaskUpdate) { u.Set("pr_number", 4527) }); err != nil {
		t.Fatal(err)
	}
	id, err := r.reg.EnqueueRequest(r.ctx, ReqStart, StartPayload{Text: "https://example.atlassian.net/browse/PR-12345"})
	if err != nil {
		t.Fatal(err)
	}
	r.tick()
	req, err := r.reg.RequestByID(r.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != registry.RequestDone || req.Result != "PR-12345 has PR #4527" {
		t.Fatalf("the start request of a published key: %s %q, want done: PR-12345 has PR #4527", req.State, req.Result)
	}
	if n := len(r.tasks()); n != 7 {
		t.Fatalf("%d tasks, want 7", n)
	}
}

// An unknown key and a missing token file make no task, and the error
// names the cause.
func TestStartWithoutJiraNamesTheCause(t *testing.T) {
	r := newIntakeRig(t)
	if _, err := r.start("PR-12399 the refund path"); err == nil || err.Error() != "PR-12399 is not in Jira" {
		t.Fatalf("an unknown key = %v, want PR-12399 is not in Jira", err)
	}
	missing := filepath.Join(t.TempDir(), "jira.token")
	r.client.TokenFile = missing
	if _, err := r.start("PR-12345"); err == nil || err.Error() != "the Jira token file is missing: "+missing {
		t.Fatalf("a missing token file = %v, want the Jira token file is missing: %s", err, missing)
	}
	if ts := r.tasks(); len(ts) != 0 {
		t.Fatalf("tasks %+v, want none", ts)
	}
}

// While Jira does not answer (curl exit 6, 7 or 28, HTTP 5xx), the task
// waits for it with no cost: it tries after 1, 2, 4, 8, 16 and then every
// 30 minutes, and after 6 hours it needs you. Once Jira answers, the task
// takes its ticket and starts.
func TestStartWhileJiraIsDownWaitsForJira(t *testing.T) {
	r := newIntakeRig(t)
	r.site.setDown(7)
	task := r.mustStart("PR-12345 the advocates only")
	if task.State != registry.TaskWaiting || task.WaitsWho != "Jira" || task.WaitsWhat != "start when Jira answers" {
		t.Fatalf("task %s waits %q: %q, want waiting on Jira: start when Jira answers", task.State, task.WaitsWho, task.WaitsWhat)
	}
	if task.JiraKey != "PR-12345" || task.Guidance != "the advocates only" {
		t.Fatalf("task %+v, want the key and the guidance kept", task)
	}
	tries := func() int { return r.site.curlCalls() }
	at := func(d time.Duration) {
		t.Helper()
		r.clock.Set(t0.Add(d))
		r.tick()
	}
	base := tries()
	for _, step := range []struct {
		at    time.Duration
		tries int
	}{
		{59 * time.Second, 0}, {time.Minute, 1}, // 1 minute after the start
		{2*time.Minute + 59*time.Second, 1}, {3 * time.Minute, 2}, // then 2
		{6*time.Minute + 59*time.Second, 2}, {7 * time.Minute, 3}, // then 4
		{15 * time.Minute, 4}, {31 * time.Minute, 5}, {61 * time.Minute, 6}, // 8, 16, then 30
		{90 * time.Minute, 6}, {91 * time.Minute, 7},
	} {
		at(step.at)
		if got := tries() - base; got != step.tries {
			t.Fatalf("at +%s Jira was tried %d times, want %d", step.at, got, step.tries)
		}
		if got := r.reload(task.ID); got.State != registry.TaskWaiting || got.WaitsWho != "Jira" {
			t.Fatalf("at +%s the task is %s waiting on %q", step.at, got.State, got.WaitsWho)
		}
	}
	if evs := r.events(task.ID, "advance"); len(evs) != 0 {
		t.Fatalf("Jira's outage counted as the task's failure: %+v", evs)
	}
	if _, held := r.e.holds[HoldInfra]; held {
		t.Fatal("Jira's outage started an infrastructure hold")
	}

	at(6 * time.Hour)
	got := r.reload(task.ID)
	if got.State != registry.TaskNeedsYou || got.WaitsWho != "you" || got.WaitsWhat != "Jira did not answer for 6h" {
		t.Fatalf("after 6h the task is %s waiting %q: %q, want needs_you: you: Jira did not answer for 6h", got.State, got.WaitsWho, got.WaitsWhat)
	}

	// Another task waits for Jira, which answers at its first try.
	r.clock.Set(t0.Add(7 * time.Hour))
	other := r.mustStart("PR-12346")
	r.site.setDown(0)
	r.clock.Add(time.Minute)
	r.tick()
	started := r.reload(other.ID)
	if started.State != registry.TaskActive || started.WaitsWho != "" || started.Title != "Points vanish after a refund" ||
		started.Kind != "bug" || started.Branch != "zhuravel-PR-12346-points-vanish-after-refund" {
		t.Fatalf("after Jira answered the task is %+v", started)
	}
	if md := r.taskFile(other.ID, "ticket.md"); !strings.HasPrefix(md, "# PR-12346 ") {
		t.Fatalf("ticket.md:\n%s", md)
	}
}

// A restart, or an outage, never reads every waiting task's key on one
// tick: the tasks that wait for Jira try it at once after a restart, but a
// tick takes at most jiraTriesPerTick of their tries, and none after a try
// Jira did not answer, since the others would wait for it too. So a hung
// Jira holds a tick for one read, and the tries stay a tick apart.
func TestARestartReadsTheWaitingKeysAFewATick(t *testing.T) {
	r := newIntakeRig(t)
	r.site.setDown(28) // curl's timeout: Jira hangs
	keys := []string{"PR-12350", "PR-12351", "PR-12352", "PR-12353", "PR-12354"}
	var waiting []registry.Task
	for _, k := range keys {
		r.site.issues[k] = issueJSON(k, "Coupon "+k, "Bug", "The coupon is off.")
		waiting = append(waiting, r.mustStart(k))
	}
	r.crash()
	calls := func() int {
		r.site.mu.Lock()
		defer r.site.mu.Unlock()
		return r.site.calls
	}
	for i := range 2 {
		before := calls()
		r.tick()
		if n := calls() - before; n != 1 {
			t.Fatalf("tick %d after the restart called a hung Jira %d times, want once", i+1, n)
		}
	}

	r.site.setDown(0)
	before := calls()
	r.tick()
	r.e.waitBackground()
	if n := calls() - before; n != jiraTriesPerTick {
		t.Fatalf("a tick read %d waiting keys from a Jira that answers, want %d", n, jiraTriesPerTick)
	}
	r.clock.Add(jiraBackoff(1)) // the two that tried while Jira hung come due again
	for range 2 {
		r.tick()
		r.e.waitBackground()
	}
	for _, w := range waiting {
		if got := r.reload(w.ID); got.State != registry.TaskActive && got.State != registry.TaskQueued {
			t.Fatalf("%s is %s waiting %q, want started once Jira answered", w.JiraKey, got.State, got.WaitsWhat)
		}
	}
}

// A task that waits for Jira has a kind from its start: the one you chose,
// or the one your words after the key pick (else the default), so a task
// that went to you and back starts its kind's first stage. When Jira
// answers, a picked kind follows the ticket's type; a chosen one stays.
func TestAJiraWaitingTaskHasAKindFromTheStart(t *testing.T) {
	r := newIntakeRig(t)
	r.site.issues["PR-12347"] = issueJSON("PR-12347", "Upgrade the gems", "Bug", "Bundler 3.")
	r.site.setDown(7)
	guessed := r.mustStart("PR-12345 fix the reminder's crash")
	plain := r.mustStart("PR-12346")
	chosen, err := r.e.start(r.ctx, StartPayload{Text: "PR-12347", Kind: "chore"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		task registry.Task
		kind string
	}{{guessed, "bug"}, {plain, "bug"}, {chosen, "chore"}} {
		if c.task.State != registry.TaskWaiting || c.task.Kind != c.kind {
			t.Fatalf("%s is %s of kind %q, want waiting of kind %q", c.task.JiraKey, c.task.State, c.task.Kind, c.kind)
		}
	}

	// You take the waiting task over and let it go on: it starts its kind's
	// first stage, with no kind to miss.
	if err := r.reg.TransitionTask(r.ctx, plain.ID, nil, registry.TaskNeedsYou, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.reg.TransitionTask(r.ctx, plain.ID, nil, registry.TaskActive, func(u *registry.TaskUpdate) {
		u.Set("waits_who", "")
		u.Set("waits_what", "")
	}); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if got := r.reload(plain.ID); got.Stage != "investigate" {
		t.Fatalf("the resumed task is at %q, want its kind's first stage", got.Stage)
	}
	if evs := r.events(plain.ID, "advance"); len(evs) != 0 {
		t.Fatalf("the resumed task failed: %+v", evs)
	}

	r.site.setDown(0)
	r.clock.Add(time.Minute)
	r.tick()
	r.e.waitBackground()
	if got := r.reload(guessed.ID); got.State != registry.TaskActive || got.Kind != "feature" {
		t.Fatalf("after Jira answered the guessed task is %s of kind %q, want active, the Story's feature", got.State, got.Kind)
	}
	if got := r.reload(chosen.ID); got.State != registry.TaskActive || got.Kind != "chore" {
		t.Fatalf("after Jira answered the chosen task is %s of kind %q, want active, your chore", got.State, got.Kind)
	}
}

// A task that waits for Jira has a branch from its start, named by intake's
// rules from your login and the key, so a task you take over can claim a
// copy and create it. When Jira answers, the summary's words join the name,
// unless the branch exists already (a claim made it).
func TestAJiraWaitingTaskHasABranchFromTheStart(t *testing.T) {
	r := newIntakeRig(t)
	r.site.setDown(7)
	named := r.mustStart("PR-12345 the advocates only")
	made := r.mustStart("PR-12346")
	if named.Branch != "zhuravel-PR-12345" || made.Branch != "zhuravel-PR-12346" {
		t.Fatalf("branches %q and %q, want the key's", named.Branch, made.Branch)
	}
	r.refs["refs/heads/zhuravel-PR-12346"] = true // a claim created it meanwhile
	r.site.setDown(0)
	r.clock.Add(time.Minute)
	r.tick()
	r.e.waitBackground()
	if got := r.reload(named.ID); got.State != registry.TaskActive || got.Branch != "zhuravel-PR-12345-credit-expiry-reminder-email" {
		t.Fatalf("after Jira answered the task is %s on %q, want active on the summary's name", got.State, got.Branch)
	}
	if got := r.reload(made.ID); got.State != registry.TaskActive || got.Branch != "zhuravel-PR-12346" {
		t.Fatalf("after Jira answered the task whose branch exists is %s on %q, want it kept", got.State, got.Branch)
	}
}

// The overlap line is advisory: it is read off the loop, after the task
// exists, and a slow gh never delays the start's answer. The line lands
// among the task's events once it is read.
func TestTheOverlapLineNeverDelaysTheStart(t *testing.T) {
	r := newIntakeRig(t)
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	defer free()
	r.run.Rules = []execx.Rule{
		{Prefix: []string{"git", "log"}, Result: execx.Result{Stdout: []byte("abc1234\tSpec the reminder\n")}},
		{Prefix: []string{"gh"}, Fn: func(execx.Cmd) (execx.Result, error) {
			<-release
			return execx.Result{Stdout: []byte("[]")}, nil
		}},
	}
	done := make(chan registry.Task, 1)
	go func() {
		task, err := r.start("PR-12345")
		if err != nil {
			t.Error(err)
		}
		done <- task
	}()
	var task registry.Task
	select {
	case task = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the start waited for the overlap's gh")
	}
	if task.State != registry.TaskActive {
		t.Fatalf("the task is %s", task.State)
	}
	if evs := r.events(task.ID, "overlap"); len(evs) != 0 {
		t.Fatalf("an overlap line before gh answered: %+v", evs)
	}
	free()
	r.e.waitBackground()
	if evs := r.events(task.ID, "overlap"); len(evs) != 1 || !strings.Contains(evs[0].Text, "1 commit names PR-12345") {
		t.Fatalf("overlap events %+v, want the line once gh answered", evs)
	}
}

func (s *jiraSite) curlCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// A start reads a Jira key or a Jira link at the start of the text, the
// words after it are guidance; a GitHub PR link and an Actions run link
// are links; anything else is a prompt, whole.
func TestParseStartReadsJiraPRAndRunLinks(t *testing.T) {
	trackers := jiraTestSite + "/browse/PR-{num} " + jiraTestSite
	for _, tc := range []struct {
		text string
		want StartInput
	}{
		{"PR-12345", StartInput{Key: "PR-12345"}},
		{"  PR-12345 only the refund path ", StartInput{Key: "PR-12345", Guidance: "only the refund path"}},
		{"PR-12345: only the refund path", StartInput{Key: "PR-12345", Guidance: "only the refund path"}},
		{"PR-12345\nonly the refund path\nand the email", StartInput{Key: "PR-12345", Guidance: "only the refund path\nand the email"}},
		{"https://example.atlassian.net/browse/PR-12346 advocates only", StartInput{Key: "PR-12346", Guidance: "advocates only"}},
		{"https://example.atlassian.net/browse/PR-12346?focusedCommentId=10", StartInput{Key: "PR-12346"}},
		{"https://example.atlassian.net/browse/OPS-7", StartInput{Key: "OPS-7"}},
		{"https://github.com/talkable/talkable/pull/4527", StartInput{PR: &Link{URL: "https://github.com/talkable/talkable/pull/4527", Repo: "talkable/talkable", Number: 4527}}},
		{"https://github.com/talkable/talkable/pull/4527/files take it over", StartInput{Guidance: "take it over",
			PR: &Link{URL: "https://github.com/talkable/talkable/pull/4527/files", Repo: "talkable/talkable", Number: 4527}}},
		{"https://github.com/talkable/talkable/actions/runs/1234567890/job/42", StartInput{
			Run: &Link{URL: "https://github.com/talkable/talkable/actions/runs/1234567890/job/42", Repo: "talkable/talkable", Number: 1234567890}}},
		{"Rename the referral helper", StartInput{Prompt: "Rename the referral helper"}},
		{"pr-12345 is lowercase, so it is words", StartInput{Prompt: "pr-12345 is lowercase, so it is words"}},
		{"PR-12345x is no key", StartInput{Prompt: "PR-12345x is no key"}},
		{"https://wiki.example.com/browse/PR-12345 a page", StartInput{Prompt: "https://wiki.example.com/browse/PR-12345 a page"}},
		{"https://github.com/talkable/talkable/issues/9 an issue", StartInput{Prompt: "https://github.com/talkable/talkable/issues/9 an issue"}},
		{"The digest\n  is wrong\n", StartInput{Prompt: "The digest\n  is wrong"}},
	} {
		if got := ParseStart(tc.text, trackers); !sameStart(got, tc.want) {
			t.Errorf("ParseStart(%q) = %s, want %s", tc.text, showStart(got), showStart(tc.want))
		}
	}
	// Without a site to tell it by, any Jira issue link counts.
	if got := ParseStart("https://other.atlassian.net/browse/PR-12347", ""); got.Key != "PR-12347" {
		t.Errorf("a Jira link without trackers = %s", showStart(got))
	}
}

func sameStart(a, b StartInput) bool {
	link := func(x, y *Link) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Key == b.Key && a.Guidance == b.Guidance && a.Prompt == b.Prompt && link(a.PR, b.PR) && link(a.Run, b.Run) &&
		slices.Equal(a.Found, b.Found)
}

func showStart(s StartInput) string {
	out := fmt.Sprintf("{Key %q Guidance %q Prompt %q", s.Key, s.Guidance, s.Prompt)
	if s.PR != nil {
		out += fmt.Sprintf(" PR %+v", *s.PR)
	}
	if s.Run != nil {
		out += fmt.Sprintf(" Run %+v", *s.Run)
	}
	if len(s.Found) > 0 {
		out += fmt.Sprintf(" Found %q", s.Found)
	}
	return out + "}"
}

// A PR link offers adoption and a run link a CI task, both of later
// milestones: the start says so and makes no task.
func TestAPRLinkOffersAdoptionLater(t *testing.T) {
	r := newIntakeRig(t)
	_, err := r.start("https://github.com/talkable/talkable/pull/4527")
	if !errors.Is(err, ErrLater) || !strings.Contains(err.Error(), "adoption comes in milestone 2") {
		t.Fatalf("a PR link = %v, want ErrLater: adoption comes in milestone 2", err)
	}
	_, err = r.start("https://github.com/talkable/talkable/actions/runs/1234567890")
	if !errors.Is(err, ErrLater) || !strings.Contains(err.Error(), "CI tasks come in milestone 3") {
		t.Fatalf("a run link = %v, want ErrLater: CI tasks come in milestone 3", err)
	}
	if ts := r.tasks(); len(ts) != 0 {
		t.Fatalf("tasks %+v, want none", ts)
	}
	if n := r.site.curlCalls(); n != 0 {
		t.Fatalf("a link read Jira %d times", n)
	}
}

// A key's start writes the ticket (Ticket.Markdown) and your words after
// the key to the task's directory as data files, records both as
// artifacts, records your words as a guidance input, and names the task's
// branch after your login, the key and the summary.
func TestStartWritesTheTicketAndTheGuidanceAsDataFiles(t *testing.T) {
	r := newIntakeRig(t)
	task := r.mustStart("PR-12345 the reminder is for advocates only, not their friends")

	want, err := r.client.Ticket(r.ctx, "PR-12345")
	if err != nil {
		t.Fatal(err)
	}
	ticket := r.taskFile(task.ID, "ticket.md")
	if ticket != string(want.Markdown()) {
		t.Fatalf("ticket.md:\n%s\nwant Ticket.Markdown():\n%s", ticket, want.Markdown())
	}
	guidance := r.taskFile(task.ID, "guidance.md")
	if !strings.Contains(guidance, "the reminder is for advocates only, not their friends") {
		t.Fatalf("guidance.md:\n%s", guidance)
	}
	arts, err := r.reg.Artifacts(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]registry.Artifact{}
	for _, a := range arts {
		byKind[a.Kind] = a
	}
	if a := byKind["ticket"]; a.Path != "ticket.md" || a.SHA256 != sum(ticket) || a.Rev != 1 {
		t.Errorf("ticket artifact %+v, want ticket.md of its sha256", a)
	}
	if a := byKind["guidance"]; a.Path != "guidance.md" || a.SHA256 != sum(guidance) {
		t.Errorf("guidance artifact %+v, want guidance.md of its sha256", a)
	}
	ins, err := r.reg.Inputs(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 1 || ins[0].Kind != registry.InputGuidance || ins[0].Text != "the reminder is for advocates only, not their friends" ||
		ins[0].Surface != "cli" {
		t.Fatalf("inputs %+v, want one guidance input with your words", ins)
	}
	got := r.reload(task.ID)
	if got.State != registry.TaskActive || got.Title != "Credit expiry reminder email" || got.Kind != "feature" ||
		got.Branch != "zhuravel-PR-12345-credit-expiry-reminder-email" || got.Repo != "talkable/talkable" {
		t.Fatalf("task %+v", got)
	}

	// A key alone writes no guidance file and records the text you typed.
	bare := r.mustStart("PR-12346")
	if _, err := os.Stat(filepath.Join(r.layout.FactoryTaskDir(bare.ID), "guidance.md")); !os.IsNotExist(err) {
		t.Fatalf("guidance.md for a key alone: %v", err)
	}
	if ins, _ := r.reg.Inputs(r.ctx, bare.ID); len(ins) != 1 || ins[0].Kind != registry.InputTaskText || ins[0].Text != "PR-12346" {
		t.Fatalf("inputs %+v, want the task's text", ins)
	}
}

// A branch the main clone has, yours or origin's, made without the
// factory, is no new task's: the task's branch gets a name of its own, so
// its claim never checks yours out and its push never meets it.
func TestABranchYourCloneHasGetsANameOfItsOwn(t *testing.T) {
	r := newIntakeRig(t)
	r.refs["refs/remotes/origin/zhuravel-PR-12345-credit-expiry-reminder-email"] = true
	r.refs["refs/heads/zhuravel-PR-12345-credit-expiry-reminder-email-2"] = true
	task := r.mustStart("PR-12345")
	if task.Branch != "zhuravel-PR-12345-credit-expiry-reminder-email-3" {
		t.Fatalf("branch %q, want one the main clone does not have", task.Branch)
	}
	other := r.mustStart("PR-12346")
	if other.Branch != "zhuravel-PR-12346-points-vanish-after-refund" {
		t.Fatalf("branch %q, want the plain name", other.Branch)
	}
}

// A prompt's start writes your words to prompt.md, the task's data file in
// place of a ticket, records them as the task's text, and names the branch
// after your login and the prompt's first words.
func TestStartFromAPromptWritesPromptMD(t *testing.T) {
	r := newIntakeRig(t)
	text := "Rename the referral helper in the checkout, and its specs.\nKeep the old name as an alias."
	task := r.mustStart(text)
	if task.JiraKey != "" || task.Prompt != text {
		t.Fatalf("task %+v, want a prompt task", task)
	}
	prompt := r.taskFile(task.ID, "prompt.md")
	if !strings.Contains(prompt, text) {
		t.Fatalf("prompt.md:\n%s", prompt)
	}
	arts, _ := r.reg.Artifacts(r.ctx, task.ID)
	if len(arts) != 1 || arts[0].Kind != "ticket" || arts[0].Path != "prompt.md" || arts[0].SHA256 != sum(prompt) {
		t.Fatalf("artifacts %+v, want prompt.md as the task's ticket", arts)
	}
	ins, _ := r.reg.Inputs(r.ctx, task.ID)
	if len(ins) != 1 || ins[0].Kind != registry.InputTaskText || ins[0].Text != text {
		t.Fatalf("inputs %+v, want the task's text", ins)
	}
	got := r.reload(task.ID)
	if got.Kind != "chore" || got.Branch != "zhuravel-rename-referral-helper-checkout-specs" ||
		got.Title != "Rename the referral helper in the checkout, and its specs." {
		t.Fatalf("task kind %q, branch %q, title %q", got.Kind, got.Branch, got.Title)
	}
	if n := len(got.Branch) - len("zhuravel-"); n > 40 {
		t.Fatalf("the slug is %d characters, want at most 40", n)
	}
	// The same words again get a branch of their own.
	again := r.mustStart(text)
	if b := r.reload(again.ID).Branch; b != "zhuravel-rename-referral-helper-checkout-specs-2" {
		t.Fatalf("the second task of the same words is on %q, want a branch of its own", b)
	}
}

// A free prompt's first question batch asks which Jira issue it is: the
// options are the 3 best title matches of a search on its first words and
// "no issue", and the best match is recommended; with no match, "no
// issue" is.
func TestAFreePromptAsksWhichJiraIssueWithARecommendedMatch(t *testing.T) {
	r := newIntakeRig(t)
	r.site.search = searchAnswer(
		[2]string{"PR-12350", "Merchant settings page"},
		[2]string{"PR-12351", "Weekly digest uses the wrong week east of UTC+10"},
		[2]string{"PR-12352", "Digest email footer"},
		[2]string{"PR-12353", "Weekly report export"},
	)
	task := r.mustStart("The weekly digest email shows last week's numbers for merchants east of UTC+10.")
	if len(r.site.searched) != 1 || !strings.Contains(r.site.searched[0], `summary ~ "weekly"`) ||
		!strings.Contains(r.site.searched[0], `summary ~ "digest"`) {
		t.Fatalf("searched %q, want the prompt's first words", r.site.searched)
	}
	qs, err := r.reg.Questions(r.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 {
		t.Fatalf("questions %+v, want one", qs)
	}
	q := qs[0]
	if q.Text != "Which Jira issue is this?" || q.Revision != 1 || q.State != registry.QuestionOpen || q.RaisedBy != "intake" {
		t.Fatalf("question %+v", q)
	}
	var labels []string
	for _, o := range q.Options {
		labels = append(labels, o.Label)
	}
	want := []string{"PR-12351 Weekly digest uses the wrong week east of UTC+10", "PR-12352 Digest email footer",
		"PR-12350 Merchant settings page", "no issue"}
	if !slices.Equal(labels, want) {
		t.Fatalf("options %q, want %q", labels, want)
	}
	if q.Recommended != 0 || q.Why == "" {
		t.Fatalf("recommended %d (%q), want the best title match, 0", q.Recommended, q.Why)
	}
	if got := r.reload(task.ID); got.QuestionRev != 1 || got.State != registry.TaskActive {
		t.Fatalf("task %s at question revision %d", got.State, got.QuestionRev)
	}

	r.site.search = searchAnswer([2]string{"PR-12360", "Merchant settings page"})
	other := r.mustStart("Rename the referral helper")
	qs, _ = r.reg.Questions(r.ctx, other.ID)
	if len(qs) != 1 || len(qs[0].Options) != 2 || qs[0].Options[1].Label != "no issue" || qs[0].Recommended != 1 {
		t.Fatalf("questions %+v, want no issue recommended when no title matches", qs)
	}
}

func TestPickKindUsesTheJiraTypeFirst(t *testing.T) {
	cfg := config.DefaultFactory()
	for _, tc := range []struct {
		typ, prompt, kind, why string
	}{
		{"Story", "fix the broken crash", "feature", "from the Jira type Story"},
		{"bug", "add a new feature", "bug", "from the Jira type bug"},
		{"Task", "", "chore", "from the Jira type Task"},
	} {
		kind, why := PickKind(cfg, &jira.Ticket{Type: tc.typ, Summary: "anything"}, tc.prompt)
		if kind != tc.kind || why != tc.why {
			t.Errorf("type %q: %q (%q), want %q (%q)", tc.typ, kind, why, tc.kind, tc.why)
		}
	}
	cfg.KindByJiraType["Epic"] = "nonexistent"
	if kind, _ := PickKind(cfg, &jira.Ticket{Type: "Epic", Summary: "Upgrade the gems"}, ""); kind != "chore" {
		t.Errorf("a type mapped to a kind that is not configured gave %q, want the words' chore", kind)
	}
}

func TestPickKindFallsBackToWords(t *testing.T) {
	cfg := config.DefaultFactory()
	for _, tc := range []struct {
		ticket    *jira.Ticket
		prompt    string
		kind, why string
	}{
		{nil, "The checkout crashes when the cart is empty", "bug", `guessed from "crash"`},
		{nil, "Upgrade the gems and bump rubocop", "chore", `guessed from "upgrade"`},
		{nil, "Add support for a second payout method", "feature", `guessed from "add"`},
		{nil, "Fix the error after the gem upgrade", "bug", `guessed from "fix"`},
		{&jira.Ticket{Type: "Spike", Summary: "Refactor the payout service"}, "", "chore", `guessed from "refactor"`},
		{nil, "The weekly digest shows last week's numbers", "bug", "no hint; tab changes it"},
		{nil, "", "bug", "no hint; tab changes it"},
	} {
		kind, why := PickKind(cfg, tc.ticket, tc.prompt)
		if kind != tc.kind || why != tc.why {
			t.Errorf("%q: %q (%q), want %q (%q)", tc.prompt, kind, why, tc.kind, tc.why)
		}
	}
}

// A start while memory is full queues the task, waiting below the gate's
// swap; it starts on the tick that finds memory below the gate again.
func TestStartQueuesWhenMemoryIsFull(t *testing.T) {
	r := newIntakeRig(t)
	r.cfg.Factory.MemoryGateSwap = 80
	r.setSwap(91)
	task := r.mustStart("PR-12345")
	if task.State != registry.TaskQueued || task.WaitsWho != "memory" || task.WaitsWhat != "full, starts below 80% swap" {
		t.Fatalf("task %s waits %q: %q, want queued on memory: full, starts below 80%% swap", task.State, task.WaitsWho, task.WaitsWhat)
	}
	r.tick()
	if got := r.reload(task.ID); got.State != registry.TaskQueued {
		t.Fatalf("with memory full a tick moved the task to %s", got.State)
	}
	r.setSwap(40)
	r.clock.Add(time.Minute)
	r.tick()
	if got := r.reload(task.ID); got.State != registry.TaskActive || got.WaitsWho != "" || got.WaitsWhat != "" {
		t.Fatalf("below the gate the task is %s waiting %q: %q, want active", got.State, got.WaitsWho, got.WaitsWhat)
	}
}

// A start while max_active tasks are at work queues the task for a copy;
// it starts when one of them ends.
func TestAQueuedTaskStartsWhenAnActiveOneEnds(t *testing.T) {
	r := newIntakeRig(t)
	r.cfg.Factory.MaxActive = 1
	first := r.mustStart("PR-12345")
	second := r.mustStart("PR-12346")
	if second.State != registry.TaskQueued || second.WaitsWho != "copy" || second.WaitsWhat != "all 1 busy" {
		t.Fatalf("task %s waits %q: %q, want queued on copy: all 1 busy", second.State, second.WaitsWho, second.WaitsWhat)
	}
	if _, err := r.e.stopTask(r.ctx, StopPayload{TaskID: first.ID, Surface: "cli"}); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if got := r.reload(second.ID); got.State != registry.TaskActive || got.WaitsWho != "" {
		t.Fatalf("after the first task stopped the second is %s waiting %q", got.State, got.WaitsWho)
	}
}

// The daemon answers a start request with the task it made; a start the
// daemon refuses fails the request with the cause.
func TestAStartRequestAnswersWithTheTask(t *testing.T) {
	r := newIntakeRig(t)
	ok, err := r.reg.EnqueueRequest(r.ctx, ReqStart, StartPayload{Text: "PR-12346 the refund path", Kind: "feature", Repo: "talkable/talkable"})
	if err != nil {
		t.Fatal(err)
	}
	bad, err := r.reg.EnqueueRequest(r.ctx, ReqStart, StartPayload{Text: "PR-12399"})
	if err != nil {
		t.Fatal(err)
	}
	r.tick()
	req, _ := r.reg.RequestByID(r.ctx, ok)
	if req.State != registry.RequestDone || req.Result != "started PR-12346 as task 1: feature in talkable/talkable on zhuravel-PR-12346-points-vanish-after-refund" {
		t.Fatalf("start request %s %q", req.State, req.Result)
	}
	if got := r.reload(1); got.Kind != "feature" {
		t.Fatalf("the kind you chose was not kept: %q", got.Kind)
	}
	req, _ = r.reg.RequestByID(r.ctx, bad)
	if req.State != registry.RequestFailed || req.Result != "PR-12399 is not in Jira" {
		t.Fatalf("refused start request %s %q", req.State, req.Result)
	}
	if _, err := r.e.start(r.ctx, StartPayload{Text: "PR-12345", Kind: "epic"}); err == nil || !strings.Contains(err.Error(), "bug, chore, feature") {
		t.Fatalf("an unknown kind = %v, want the kinds named", err)
	}
	if _, err := r.e.start(r.ctx, StartPayload{Text: "PR-12345", Repo: "talkable/other"}); err == nil || !strings.Contains(err.Error(), "talkable/talkable") {
		t.Fatalf("an unknown repository = %v, want the configured ones named", err)
	}
}

// The new-task screen's preview: the ticket the daemon read, the kind and
// why, the branch and when the task would start; an unknown key says so.
func TestThePreviewReadsTheTicketAndNamesKindBranchAndStart(t *testing.T) {
	r := newIntakeRig(t)
	f, err := r.e.preview(r.ctx, "PR-12345 the advocates only")
	if err != nil {
		t.Fatal(err)
	}
	if f.Jira == nil || f.Jira.Key != "PR-12345" || f.Jira.Type != "Story" || f.Jira.Summary != "Credit expiry reminder email" ||
		!strings.HasPrefix(f.Jira.Excerpt, "Send advocates a reminder") || f.Jira.Read.IsZero() {
		t.Fatalf("preview Jira %+v", f.Jira)
	}
	if f.Kind != "feature" || f.KindWhy != "from the Jira type Story" || f.Branch != "zhuravel-PR-12345-credit-expiry-reminder-email" ||
		!strings.HasPrefix(f.Starts, "now") || f.Repo != "talkable/talkable" || len(f.Agents) != 1 || f.Err != "" {
		t.Fatalf("preview %+v", f)
	}
	if n := len(r.tasks()); n != 0 {
		t.Fatalf("a preview made %d tasks", n)
	}
	r.cfg.Factory.MemoryGateSwap = 80
	r.setSwap(91)
	f, _ = r.e.preview(r.ctx, "The weekly digest is off")
	if f.Jira != nil || f.Title != "The weekly digest is off" || f.Starts != "queued: memory full (swap 91%) · starts when swap is under 80%" ||
		f.Branch != "zhuravel-weekly-digest-off" || len(f.Agents) != 3 {
		t.Fatalf("a prompt's preview %+v", f)
	}
	if f, _ = r.e.preview(r.ctx, "PR-12399"); f.Err != "PR-12399 is not in Jira" {
		t.Fatalf("an unknown key's preview %+v", f)
	}
}

// The words you start a task with are classified as they are recorded: a
// guidance after a key, a prompt, a key alone, by what they ask for.
func TestTheWordsOfAStartAreClassifiedWhenRecorded(t *testing.T) {
	r := newIntakeRig(t)
	for _, c := range []struct{ text, kind, class string }{
		{"PR-12345 merge master first", registry.InputGuidance, registry.ClassStepStart},
		{"PR-12346", registry.InputTaskText, registry.ClassInfo},
		{"Rename the referral helper in the checkout, and its specs.", registry.InputTaskText, registry.ClassInfo},
		{"open PR for the rename of the referral helper", registry.InputTaskText, registry.ClassStepStart},
	} {
		task := r.mustStart(c.text)
		ins, err := r.reg.Inputs(r.ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(ins) != 1 || ins[0].Kind != c.kind || ins[0].Class != c.class {
			t.Errorf("start %q: inputs %+v, want one %s input of class %q", c.text, ins, c.kind, c.class)
		}
		if len(ins) == 1 && ins[0].Role != registry.RoleIntake {
			t.Errorf("start %q: the input has the role %q, want %q: the mark of the message that starts the task", c.text, ins[0].Role, registry.RoleIntake)
		}
	}
}

// However a task is started, the message that starts it is typed and never a
// step start in the stats, even when its words name a step; guidance given
// later is neither marked nor exempt.
func TestTheMessageThatStartsATaskIsNeverAStepStart(t *testing.T) {
	r := newIntakeRig(t)
	keyed := r.mustStart("PR-12345 fix it, then open PR")
	prompt := r.mustStart("open PR for the rename of the referral helper")
	if _, err := r.e.recordInput(r.ctx, registry.Input{TaskID: keyed.ID, Kind: registry.InputGuidance, Surface: "screen", Text: "merge master first"}); err != nil {
		t.Fatal(err)
	}

	rep, err := stats.ComputeAt(r.ctx, r.reg, time.Time{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]stats.TaskStats{}
	for _, ts := range rep.Tasks {
		byID[ts.TaskID] = ts
	}
	if got := byID[prompt.ID]; got.Typed != 1 || got.StepStarts != 0 {
		t.Errorf("a prompt that names a step: typed %d, step starts %d; want 1 and 0", got.Typed, got.StepStarts)
	}
	if got := byID[keyed.ID]; got.Typed != 2 || got.StepStarts != 1 {
		t.Errorf("a key's words that name a step, then guidance that does: typed %d, step starts %d; want 2 and 1 (only the later guidance)", got.Typed, got.StepStarts)
	}
}
