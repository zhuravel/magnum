package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
)

// genericPrompt is the prompt file addRole writes for an agent role that
// brings none: it names everything a test wants to see rendered.
const genericPrompt = "{{.Mode}} review of {{.URL}} at {{.HeadSHA}}: write the report to {{.ReportPath}}"

// addRole appends role to the env's configuration, normalizes it and opens a
// session for it (the next pane). An agent role's prompt is written to
// <name>.md in a per-test prompts_dir (genericPrompt when prompt is empty),
// before Normalize, which probes <name>-rereview.md there. It returns the
// normalized role.
func (e *env) addRole(role config.Role, prompt string) config.Role {
	e.t.Helper()
	if !filepath.IsAbs(e.cfg.Pipeline.PromptsDir) {
		e.cfg.Pipeline.PromptsDir = e.t.TempDir()
	}
	if role.Kind != config.KindShell {
		if prompt == "" {
			prompt = genericPrompt
		}
		if err := os.WriteFile(filepath.Join(e.cfg.Pipeline.PromptsDir, role.Name+".md"), []byte(prompt+"\n"), 0o600); err != nil {
			e.t.Fatalf("write prompt of %s: %v", role.Name, err)
		}
	}
	e.cfg.Roles = append(e.cfg.Roles, role)
	e.cfg.Normalize()
	got := e.role(role.Name)
	e.addSession(got)
	return got
}

// role is the configured role called name.
func (e *env) role(name string) config.Role {
	e.t.Helper()
	for _, r := range e.cfg.Roles {
		if r.Name == name {
			return r
		}
	}
	e.t.Fatalf("no role %q among %v", name, roleNames(e.cfg.Roles))
	return config.Role{}
}

// editRole changes the configured role called name and normalizes the config.
func (e *env) editRole(name string, edit func(*config.Role)) {
	e.t.Helper()
	i := slices.IndexFunc(e.cfg.Roles, func(r config.Role) bool { return r.Name == name })
	if i < 0 {
		e.t.Fatalf("no role %q among %v", name, roleNames(e.cfg.Roles))
	}
	edit(&e.cfg.Roles[i])
	e.cfg.Normalize()
}

// watchRoles are the candidate roles of a watch that lists names.
func (e *env) watchRoles(names ...string) []config.Role {
	e.t.Helper()
	roles := e.cfg.RolesFor(&config.Watch{Roles: names})
	if len(roles) != len(names) {
		e.t.Fatalf("watch roles %v resolve to %v", names, roleNames(roles))
	}
	return roles
}

// mustPost runs a round that has to end posted.
func (e *env) mustPost(in RoundInput) RoundResult {
	e.t.Helper()
	res, err := e.r.RunRound(e.ctx, in)
	if err != nil {
		e.t.Fatalf("RunRound: %v", err)
	}
	if res.Outcome != OutcomePosted {
		e.t.Fatalf("outcome = %s, want %s: %+v", res.Outcome, OutcomePosted, res)
	}
	return res
}

// judgeText is the n-th (0-based) prompt the judge got.
func (e *env) judgeText(n int) string {
	e.t.Helper()
	subs := e.ag.submitsFor(agents.RoleJudge)
	if n >= len(subs) {
		e.t.Fatalf("judge submits = %d, want a %dth", len(subs), n+1)
	}
	return subs[n].Text
}

// runsOfRole lists the run rows of role.
func (e *env) runsOfRole(role string) []store.Run {
	e.t.Helper()
	var out []store.Run
	for _, r := range e.runs() {
		if r.Role == role {
			out = append(out, r)
		}
	}
	return out
}

func roleNames(roles []config.Role) []string {
	names := make([]string, len(roles))
	for i, r := range roles {
		names[i] = r.Name
	}
	return names
}

// listedReport is one `  - <role>: <value>` line of a judge prompt's reports
// block: value is the report path, or "missing (<detail>)".
type listedReport struct{ Role, Value string }

var reportLine = regexp.MustCompile(`(?m)^  - ([a-z0-9-]+): (.+)$`)

// listedReports parses the reports block of a judge prompt.
func listedReports(t *testing.T, prompt string) []listedReport {
	t.Helper()
	_, rest, ok := strings.Cut(prompt, "\nreports:\n")
	if !ok {
		t.Fatalf("judge prompt has no reports block:\n%s", prompt)
	}
	block, _, _ := strings.Cut(rest, "\nresult_file:")
	var out []listedReport
	for _, m := range reportLine.FindAllStringSubmatch(block, -1) {
		out = append(out, listedReport{Role: m[1], Value: m[2]})
	}
	return out
}

// wantListed compares the reports block of a judge prompt with want (the
// role names in order) and the report path of each.
func wantListed(t *testing.T, prompt string, want ...listedReport) {
	t.Helper()
	if got := listedReports(t, prompt); !slices.Equal(got, want) {
		t.Errorf("judge prompt lists %v, want %v", got, want)
	}
}

// onePatch is the output of the stubbed `git diff` of a git-diff role.
const onePatch = "diff --git a/app/x.rb b/app/x.rb\n--- a/app/x.rb\n+++ b/app/x.rb\n@@ -1 +1 @@\n-a\n+b\n"

// patchRules stubs the git calls of a git-diff role (collect, restore).
func patchRules() []execx.Rule {
	return []execx.Rule{
		{Prefix: []string{"git", "-C", slotPath, "diff"}, Result: execx.Result{Stdout: []byte(onePatch)}},
		{Prefix: []string{"git", "-C", slotPath, "reset", "--hard", "--quiet"}},
		{Prefix: []string{"git", "-C", slotPath, "clean", "-fd"}},
	}
}

func TestDroidAndOmpSessionRoles(t *testing.T) {
	e := newEnv(t)
	const droidRole, ompRole = agents.Role("droid-review"), agents.Role("omp-review")
	e.addRole(config.Role{Name: string(droidRole), Kind: config.KindDroid},
		"droid review of {{.URL}} at {{.HeadSHA}}, report to {{.ReportPath}}")
	e.addRole(config.Role{Name: string(ompRole), Kind: config.KindOMP, Effort: "high"},
		"omp review of {{.URL}} at {{.HeadSHA}} with {{.Effort}} effort, report to {{.ReportPath}}")
	e.ag.behaviors[droidRole] = []behavior{writeReport("## P1 droid finding\n")}
	e.ag.behaviors[ompRole] = []behavior{writeReport("## P3 omp finding\n")}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(601, "COMMENTED", "COMMENT").behavior(t)}

	res := e.mustPost(e.input(KindInitial))

	// Preflight: once per agent kind, the judge's first.
	pf := e.ag.preflights
	if len(pf) == 0 || pf[0] != config.KindCodex {
		t.Errorf("preflights = %v, want the judge's kind (codex) first", pf)
	}
	for _, kind := range []string{config.KindClaude, config.KindDroid, config.KindOMP} {
		if !slices.Contains(pf, kind) {
			t.Errorf("preflights = %v, missing %s", pf, kind)
		}
	}
	if uniq := slices.Compact(slices.Sorted(slices.Values(pf))); len(uniq) != len(pf) {
		t.Errorf("preflights = %v, want each kind checked once", pf)
	}

	// Prompts: one each, rendered from the role's own prompt file.
	dir := e.reportDir()
	droidPath, ompPath := filepath.Join(dir, "droid-review.md"), filepath.Join(dir, "omp-review.md")
	droid, omp := e.ag.submitsFor(droidRole), e.ag.submitsFor(ompRole)
	if len(droid) != 1 || len(omp) != 1 {
		t.Fatalf("submits: droid %d, omp %d, want 1 each", len(droid), len(omp))
	}
	mustContain(t, "droid prompt", droid[0].Text, "droid review of https://github.com/talkable/talkable/pull/11920 at "+target, droidPath)
	mustContain(t, "omp prompt", omp[0].Text, "omp review of https://github.com/talkable/talkable/pull/11920 at "+target, "with high effort", ompPath)
	if strings.Contains(droid[0].Text, "omp review") || strings.Contains(omp[0].Text, "droid review") {
		t.Errorf("prompts crossed:\ndroid: %s\nomp: %s", droid[0].Text, omp[0].Text)
	}

	// Runs and reports.
	for _, tc := range []struct {
		role       agents.Role
		path, kind string
	}{{droidRole, droidPath, config.KindDroid}, {ompRole, ompPath, config.KindOMP}} {
		run := e.runOf(tc.role, store.RunInitial)
		if run.Round != 1 || run.State != store.RunVerified {
			t.Errorf("%s run: round %d state %s (outcome %v)", tc.role, run.Round, run.State, store.Deref(run.Outcome))
		}
		if got := store.Deref(run.ReportPath); got != tc.path {
			t.Errorf("%s run report path = %q, want %q", tc.role, got, tc.path)
		}
		rep, ok := res.Reports[tc.role]
		if !ok {
			t.Fatalf("no report for %s: %+v", tc.role, res.Reports)
		}
		if rep.Status != ReportOK || rep.Path != tc.path || rep.Kind != tc.kind || rep.Capture != config.CaptureFile || rep.RunID != run.ID {
			t.Errorf("%s report = %+v, want ok at %s (kind %s, capture file, run %s)", tc.role, rep, tc.path, tc.kind, run.ID)
		}
		if _, err := os.Stat(tc.path); err != nil {
			t.Errorf("%s report file: %v", tc.role, err)
		}
	}

	// The judge lists every report, in [[role]] order.
	wantListed(t, e.judgeText(0),
		listedReport{"claude-review", filepath.Join(dir, "claude-review.md")},
		listedReport{"codex-review", filepath.Join(dir, "codex-review.md")},
		listedReport{"droid-review", droidPath},
		listedReport{"omp-review", ompPath})
}

func TestWatchRolesSubset(t *testing.T) {
	e := newEnv(t)
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(602, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.Roles = e.cfg.RolesFor(&config.Watch{Roles: []string{"codex-judge", "claude-review"}})

	res := e.mustPost(in)

	if got := e.ag.order; !slices.Equal(got, []string{"submit:claude-review", "submit:codex-judge"}) {
		t.Errorf("call order = %v, want claude-review then the judge only", got)
	}
	if n := len(e.ag.shellCallsFor(agents.RoleCodexReview)) + len(e.ag.codexCalls); n != 0 {
		t.Errorf("RunShell calls = %d, want none (codex-review is not in the watch)", n)
	}
	if runs := e.runsOfRole("codex-review"); len(runs) != 0 {
		t.Errorf("codex-review runs = %+v, want none", runs)
	}
	if pf := e.ag.preflights; !slices.Equal(pf, []string{config.KindCodex, config.KindClaude}) {
		t.Errorf("preflights = %v, want codex and claude", pf)
	}
	if len(res.Reports) != 1 || res.Reports[agents.RoleClaude].Status != ReportOK {
		t.Errorf("reports = %+v, want only claude-review ok", res.Reports)
	}
	prompt := e.judgeText(0)
	wantListed(t, prompt, listedReport{"claude-review", filepath.Join(e.reportDir(), "claude-review.md")})
	if strings.Contains(prompt, "codex-review") {
		t.Errorf("judge prompt mentions codex-review:\n%s", prompt)
	}
	if got := len(e.runs()); got != 2 {
		t.Errorf("runs = %d, want the claude-review and judge runs only", got)
	}
}

func TestRunsFirstSkippedAfterFirstRun(t *testing.T) {
	e := newEnv(t)
	e.editRole(config.RoleClaudeSimplify, func(r *config.Role) { r.Runs = config.RunsFirst })
	e.ag.behaviors[agents.RoleSimplify] = []behavior{endSilently()}
	e.ag.behaviors[agents.RoleJudge] = []behavior{
		e.judgePosts(611, "COMMENTED", "COMMENT").behavior(t),
		e.judgePosts(612, "COMMENTED", "COMMENT").behavior(t),
		e.judgePosts(613, "COMMENTED", "COMMENT").behavior(t),
	}
	e.exec.Rules = patchRules()
	patch := filepath.Join(e.reportDir(), "claude-simplify.patch")
	diffs := func() int { return len(e.exec.CallsWithPrefix("git", "-C", slotPath, "diff")) }
	// what RolesToRun decides for the next round of kind.
	decide := func(kind string, requested ...string) []string {
		t.Helper()
		roles, err := RolesToRun(e.ctx, e.st, e.cfg, e.pr, nil, requested, kind)
		if err != nil {
			t.Fatalf("RolesToRun(%s, %v): %v", kind, requested, err)
		}
		return roleNames(roles)
	}
	allFour := []string{"codex-judge", "claude-review", "codex-review", "claude-simplify"}
	reviewersOnly := []string{"codex-judge", "claude-review", "codex-review"}

	// Round 1: the PR's first round runs claude-simplify.
	if got := decide(KindInitial); !slices.Equal(got, allFour) {
		t.Errorf("before round 1 RolesToRun = %v, want %v", got, allFour)
	}
	res := e.mustPost(e.input(KindInitial))
	if got := res.Reports[agents.RoleSimplify]; got.Status != ReportOK || got.Path != patch || got.Capture != config.CaptureGitDiff {
		t.Errorf("round 1 simplify report = %+v, want ok at %s", got, patch)
	}
	if n := len(e.ag.submitsFor(agents.RoleSimplify)); n != 1 || diffs() != 1 {
		t.Errorf("after round 1: simplify submits %d, diffs %d, want 1 and 1", n, diffs())
	}
	if run := e.runOf(agents.RoleSimplify, store.RunInitial); run.State != store.RunVerified {
		t.Errorf("round 1 simplify run = %s (outcome %v), want verified", run.State, store.Deref(run.Outcome))
	}
	wantListed(t, e.judgeText(0),
		listedReport{"claude-review", filepath.Join(e.reportDir(), "claude-review.md")},
		listedReport{"codex-review", filepath.Join(e.reportDir(), "codex-review.md")},
		listedReport{"claude-simplify", patch})

	// Round 2: it has run for this PR, so a rereview leaves it out ...
	if got := decide(KindRereview); !slices.Equal(got, reviewersOnly) {
		t.Errorf("after round 1 RolesToRun(rereview) = %v, want %v", got, reviewersOnly)
	}
	prev := &PreviousReview{ID: 700, Event: "COMMENTED", SHA: prevSHA, SubmittedAt: t0}
	in := e.input(KindRereview)
	in.Round, in.Previous, in.Since = 2, prev, t0
	res = e.mustPost(in)
	if _, ran := res.Reports[agents.RoleSimplify]; ran {
		t.Errorf("round 2 ran claude-simplify again: %+v", res.Reports[agents.RoleSimplify])
	}
	if n := len(e.ag.submitsFor(agents.RoleSimplify)); n != 1 || diffs() != 1 {
		t.Errorf("after round 2: simplify submits %d, diffs %d, want still 1 and 1", n, diffs())
	}
	if n := len(e.ag.submitsFor(agents.RoleClaude)); n != 2 {
		t.Errorf("claude-review submits = %d, want 2 (it runs every round)", n)
	}
	wantListed(t, e.judgeText(1),
		listedReport{"claude-review", filepath.Join(e.reportDir(), "claude-review.md")},
		listedReport{"codex-review", filepath.Join(e.reportDir(), "codex-review.md")})
	if got := e.runsOfRole("claude-simplify"); len(got) != 1 {
		t.Errorf("claude-simplify runs = %d after round 2, want 1", len(got))
	}

	// Round 3: ... unless it is asked for.
	if got := decide(KindRereview, "simplify"); !slices.Equal(got, allFour) {
		t.Errorf("RolesToRun(rereview, simplify) = %v, want %v", got, allFour)
	}
	in = e.input(KindRereview)
	in.Round, in.Previous, in.Since, in.Requested = 3, prev, t0, []string{"simplify"}
	res = e.mustPost(in)
	if got := res.Reports[agents.RoleSimplify]; got.Status != ReportOK || got.Path != patch {
		t.Errorf("round 3 simplify report = %+v, want ok at %s", got, patch)
	}
	if n := len(e.ag.submitsFor(agents.RoleSimplify)); n != 2 || diffs() != 2 {
		t.Errorf("after round 3: simplify submits %d, diffs %d, want 2 and 2", n, diffs())
	}
	if got := e.runsOfRole("claude-simplify"); len(got) != 2 || got[1].Round != 3 || got[1].Kind != store.RunRereview {
		t.Errorf("claude-simplify runs = %+v, want round 1 and a round 3 rereview", got)
	}
	wantListed(t, e.judgeText(2),
		listedReport{"claude-review", filepath.Join(e.reportDir(), "claude-review.md")},
		listedReport{"codex-review", filepath.Join(e.reportDir(), "codex-review.md")},
		listedReport{"claude-simplify", patch})
	// The checkout was restored after every patch role.
	if n := len(e.exec.CallsWithPrefix("git", "-C", slotPath, "clean", "-fd")); n != 2 {
		t.Errorf("clean calls = %d, want 2 (one per simplify run)", n)
	}
}

func TestAfterOrderingThreeStages(t *testing.T) {
	e := newEnv(t)
	// Declared in the reverse of their stage order, so the order a round runs
	// them in can only come from After.
	for _, r := range []config.Role{
		{Name: "stage-c", Kind: config.KindDroid, After: []string{"stage-b"}},
		{Name: "stage-b", Kind: config.KindDroid, After: []string{"stage-a"}},
		{Name: "stage-a", Kind: config.KindDroid},
	} {
		e.addRole(r, "")
	}
	dir := e.reportDir()
	// A stage starts only after the report of the stage before it exists.
	afterReport := func(role, previous string) behavior {
		return func(f *fakeAgents, run store.Run, text string) error {
			if _, err := os.Stat(filepath.Join(dir, previous+".md")); err != nil {
				t.Errorf("%s started before %s finished: %v", role, previous, err)
			}
			return writeReport("report of "+role+"\n")(f, run, text)
		}
	}
	e.ag.behaviors["stage-a"] = []behavior{writeReport("report of stage-a\n")}
	e.ag.behaviors["stage-b"] = []behavior{afterReport("stage-b", "stage-a")}
	e.ag.behaviors["stage-c"] = []behavior{afterReport("stage-c", "stage-b")}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(603, "COMMENTED", "COMMENT").behavior(t)}
	in := e.input(KindInitial)
	in.Roles = e.watchRoles("codex-judge", "stage-c", "stage-b", "stage-a")
	if got := roleNames(in.Roles); !slices.Equal(got, []string{"codex-judge", "stage-c", "stage-b", "stage-a"}) {
		t.Fatalf("candidate roles = %v", got)
	}

	res := e.mustPost(in)

	wantOrder := []string{"submit:stage-a", "submit:stage-b", "submit:stage-c", "submit:codex-judge"}
	if got := e.ag.order; !slices.Equal(got, wantOrder) {
		t.Errorf("call order = %v, want %v", got, wantOrder)
	}
	wantListed(t, e.judgeText(0),
		listedReport{"stage-a", filepath.Join(dir, "stage-a.md")},
		listedReport{"stage-b", filepath.Join(dir, "stage-b.md")},
		listedReport{"stage-c", filepath.Join(dir, "stage-c.md")})
	if len(res.Reports) != 3 {
		t.Errorf("reports = %+v, want the three stage roles", res.Reports)
	}
	for _, name := range []string{"stage-a", "stage-b", "stage-c"} {
		if rep := res.Reports[agents.Role(name)]; rep.Status != ReportOK || rep.Kind != config.KindDroid {
			t.Errorf("%s report = %+v, want ok (kind droid)", name, rep)
		}
	}
	if pf := e.ag.preflights; !slices.Equal(pf, []string{config.KindCodex, config.KindDroid}) {
		t.Errorf("preflights = %v, want codex and droid once each", pf)
	}
}

func TestShellRoleCaptureFile(t *testing.T) {
	e := newEnv(t)
	e.addRole(config.Role{Name: "lint", Kind: config.KindShell,
		Command: "lint-tool --out {{.ReportPath}} --base {{.BaseRef}}", Capture: config.CaptureFile}, "")
	report := filepath.Join(e.reportDir(), "lint.md")
	e.ag.shells = map[agents.Role]func(*fakeAgents, codexCall) error{
		// capture = "file": the tool writes the report itself, nothing is tee'd.
		"lint": func(*fakeAgents, codexCall) error {
			return os.WriteFile(report, []byte("lint: 1 offense\n"), 0o600)
		},
	}
	e.ag.behaviors[agents.RoleJudge] = []behavior{e.judgePosts(604, "COMMENTED", "COMMENT").behavior(t)}

	res := e.mustPost(e.input(KindInitial))

	calls := e.ag.shellCallsFor("lint")
	if len(calls) != 1 {
		t.Fatalf("lint RunShell calls = %d, want 1", len(calls))
	}
	run := e.runOf("lint", store.RunInitial)
	line := calls[0].Script
	mustContain(t, "lint line", line, "lint-tool --out "+report+" --base origin/master")
	if strings.Contains(line, "| tee") || strings.Contains(line, "2>&1") {
		t.Errorf("lint line captures stdout: %q", line)
	}
	if want := `; printf '\nMAGNUM_DONE_` + run.ID + ` %d\n' "$?"`; !strings.HasSuffix(line, want) {
		t.Errorf("lint line = %q, want suffix %q", line, want)
	}
	if pane := store.Deref(e.sess["lint"].HerdrPaneID); calls[0].Marker != agents.DoneMarker(run.ID) || calls[0].Pane != pane {
		t.Errorf("lint call = %+v, want marker %s in pane %s", calls[0], agents.DoneMarker(run.ID), pane)
	}
	if run.State != store.RunVerified {
		t.Errorf("lint run = %s (outcome %v), want verified", run.State, store.Deref(run.Outcome))
	}

	// codex-review (capture stdout) is still tee'd to its report.
	codex := e.ag.shellCallsFor(agents.RoleCodexReview)
	if len(codex) != 1 || !strings.Contains(codex[0].Script, "| tee "+filepath.Join(e.reportDir(), "codex-review.md")) {
		t.Errorf("codex-review calls = %+v, want one tee'd to its report", codex)
	}

	rep := res.Reports["lint"]
	if rep.Status != ReportOK || rep.Path != report || rep.Capture != config.CaptureFile || rep.Kind != "" || rep.RunID != run.ID {
		t.Errorf("lint report = %+v, want ok at %s (capture file, no agent kind)", rep, report)
	}
	listed := listedReports(t, e.judgeText(0))
	if !slices.Contains(listed, listedReport{"lint", report}) {
		t.Errorf("judge prompt lists %v, want lint: %s", listed, report)
	}
	// A shell role without a tool has no login preflight.
	if pf := e.ag.preflights; !slices.Equal(pf, []string{config.KindCodex, config.KindClaude}) {
		t.Errorf("preflights = %v, want codex and claude only", pf)
	}
}

func TestRolesToRun(t *testing.T) {
	e := newEnv(t)
	all := e.cfg.RolesFor(nil)
	if got := roleNames(all); !slices.Equal(got, []string{"codex-judge", "claude-review", "codex-review", "claude-simplify"}) {
		t.Fatalf("env roles = %v", got)
	}
	// withRuns is a copy of the roles with one role's runs changed.
	withRuns := func(name, runs string) []config.Role {
		roles := e.cfg.RolesFor(nil)
		i := slices.IndexFunc(roles, func(r config.Role) bool { return r.Name == name })
		roles[i].Runs = runs
		return roles
	}
	second := all[0]
	second.Name, second.Aliases = "second-judge", nil
	withSecondJudge := func(at int) []config.Role {
		return slices.Insert(e.cfg.RolesFor(&config.Watch{Roles: []string{"codex-judge", "claude-review"}}), at, second)
	}
	reviewers := []string{"codex-judge", "claude-review", "codex-review"}

	for _, tc := range []struct {
		name      string
		roles     []config.Role // nil = the config's roles
		requested []string
		kind      string
		want      []string
	}{
		{name: "continue runs only the judge", kind: KindContinue, want: []string{"codex-judge"}},
		{name: "continue ignores requests", kind: KindContinue, requested: []string{"simplify", "claude-review"}, want: []string{"codex-judge"}},
		{name: "initial skips a manual role", kind: KindInitial, want: reviewers},
		{name: "rereview skips a manual role", kind: KindRereview, want: reviewers},
		{name: "recovery skips a manual role", kind: KindRecovery, want: reviewers},
		{name: "manual role requested by name", kind: KindInitial, requested: []string{"claude-simplify"}, want: append(slices.Clone(reviewers), "claude-simplify")},
		{name: "manual role requested by alias", kind: KindInitial, requested: []string{"simplify"}, want: append(slices.Clone(reviewers), "claude-simplify")},
		{name: "request matching ignores case and padding", kind: KindRereview, requested: []string{" Simplify "}, want: append(slices.Clone(reviewers), "claude-simplify")},
		{name: "request for other roles does not enable it", kind: KindInitial, requested: []string{"claude", "codex-review"}, want: reviewers},
		{name: "never role does not run", roles: withRuns("claude-review", config.RunsNever), kind: KindInitial, want: []string{"codex-judge", "codex-review"}},
		{name: "never role does not run when requested", roles: withRuns("claude-review", config.RunsNever), kind: KindInitial,
			requested: []string{"claude-review", "claude"}, want: []string{"codex-judge", "codex-review"}},
		{name: "only the first judge is kept", roles: withSecondJudge(2), kind: KindInitial, want: []string{"codex-judge", "claude-review"}},
		{name: "the first judge wins wherever it is", roles: withSecondJudge(0), kind: KindInitial, want: []string{"second-judge", "claude-review"}},
		{name: "roles keep the order they were given", roles: slices.Clone([]config.Role{all[2], all[1], all[0]}), kind: KindInitial,
			want: []string{"codex-review", "claude-review", "codex-judge"}},
		{name: "a watch subset with a requested manual role", roles: e.cfg.RolesFor(&config.Watch{Roles: []string{"claude-simplify", "judge"}}),
			kind: KindInitial, requested: []string{"simplify"}, want: []string{"codex-judge", "claude-simplify"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RolesToRun(e.ctx, e.st, e.cfg, e.pr, tc.roles, tc.requested, tc.kind)
			if err != nil {
				t.Fatalf("RolesToRun: %v", err)
			}
			if names := roleNames(got); !slices.Equal(names, tc.want) {
				t.Errorf("RolesToRun = %v, want %v", names, tc.want)
			}
		})
	}

	t.Run("nil roles are the config's roles", func(t *testing.T) {
		for _, kind := range []string{KindInitial, KindRereview, KindContinue, KindRecovery} {
			for _, requested := range [][]string{nil, {"simplify"}} {
				fromNil, err := RolesToRun(e.ctx, e.st, e.cfg, e.pr, nil, requested, kind)
				if err != nil {
					t.Fatalf("RolesToRun(nil, %s): %v", kind, err)
				}
				explicit, err := RolesToRun(e.ctx, e.st, e.cfg, e.pr, e.cfg.RolesFor(nil), requested, kind)
				if err != nil {
					t.Fatalf("RolesToRun(RolesFor(nil), %s): %v", kind, err)
				}
				if !slices.EqualFunc(fromNil, explicit, func(a, b config.Role) bool { return a.Name == b.Name }) {
					t.Errorf("%s %v: nil roles give %v, RolesFor(nil) gives %v", kind, requested, roleNames(fromNil), roleNames(explicit))
				}
			}
		}
	})

	t.Run("invalid input", func(t *testing.T) {
		if _, err := RolesToRun(e.ctx, e.st, nil, e.pr, nil, nil, KindInitial); !errors.Is(err, ErrInvalid) {
			t.Errorf("no roles and no config: err = %v, want ErrInvalid", err)
		}
		first := withRuns("claude-simplify", config.RunsFirst)
		if _, err := RolesToRun(e.ctx, nil, e.cfg, e.pr, first, nil, KindInitial); !errors.Is(err, ErrInvalid) {
			t.Errorf("a runs-first role without a store: err = %v, want ErrInvalid", err)
		}
		// A request answers "first" without consulting the store.
		got, err := RolesToRun(e.ctx, nil, e.cfg, e.pr, first, []string{"simplify"}, KindInitial)
		if err != nil || !slices.Contains(roleNames(got), "claude-simplify") {
			t.Errorf("requested runs-first role without a store: %v, %v", roleNames(got), err)
		}
	})
}
