package agents

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/store"
)

const slot1 = "/Users/x/Projects/talkable.review1"

// droidRole and ompRole are user-defined reviewer roles on the droid and
// omp kinds.
func droidRole() config.Role {
	return config.Role{Name: "droid-review", Kind: config.KindDroid, Effort: "high", Aliases: []string{"droid"}}
}

func ompRole() config.Role {
	return config.Role{Name: "omp-review", Kind: config.KindOMP, Model: "gpt-5.2", Effort: "high"}
}

// probe makes the wrapper probe of kind report out ("omp: alias", ...).
func (e *env) probe(kind, out string) {
	e.run.Rules = append([]execx.Rule{{Prefix: []string{"zsh", "-ic", "whence -w " + kind},
		Result: execx.Result{Stdout: []byte(out + "\n")}}}, e.run.Rules...)
}

func TestStartAgentDroidResumeOnly(t *testing.T) {
	e := newEnv(t)
	droid := e.addRole(droidRole())
	e.probe(KindDroid, "droid: alias")
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, nil, "talkable#11920", []config.Role{e.spec(RoleJudge), droid})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, droid, ws.Panes["droid-review"], "d1e2-session"); err != nil {
		t.Fatal(err)
	}
	st := e.h.starts[0]
	// droid has no name, model or effort flag in interactive mode: the role's
	// effort reaches its prompts only.
	if st.Name != "mg-11920-droid-review-5d01cf" || st.Kind != "droid" || !slices.Equal(st.Args, []string{"--resume", "d1e2-session"}) {
		t.Fatalf("droid start = %+v", st)
	}
	s := e.session("droid-review")
	if store.Deref(s.AgentKind) != KindDroid || store.Deref(s.SessionID) != "d1e2-session" || s.State != store.SessionLive {
		t.Fatalf("droid session = %+v", s)
	}
	// No trust handling is known for droid: its configs are never touched.
	if logs := e.logs.all(); len(logs) != 0 {
		t.Fatalf("logs = %q", logs)
	}
}

func TestStartAgentOMPModelAndThinking(t *testing.T) {
	cases := []struct {
		name, wrapper string
		kindArgs      []string
		roleArgs      []string
		want          []string
	}{
		{"wrapper alias", config.WrapperAuto, []string{"--auto-approve"}, nil,
			[]string{"--resume=o-1", "--model=gpt-5.2", "--thinking=high"}},
		{"plain binary", config.WrapperFalse, []string{"--auto-approve"}, []string{"--approval-mode=yolo"},
			[]string{"--resume=o-1", "--model=gpt-5.2", "--thinking=high", "--auto-approve", "--approval-mode=yolo"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			r := ompRole()
			r.Args = tc.roleArgs
			omp := e.addRole(r)
			e.setKind(KindOMP, func(k *config.Kind) { k.Wrapper, k.Args = tc.wrapper, tc.kindArgs })
			e.probe(KindOMP, "omp: alias")
			ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, nil, "talkable#11920", []config.Role{e.spec(RoleJudge), omp})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.m.StartAgent(e.ctx, e.pr, omp, ws.Panes["omp-review"], "o-1"); err != nil {
				t.Fatal(err)
			}
			if st := e.h.starts[0]; st.Kind != "omp" || !slices.Equal(st.Args, tc.want) {
				t.Fatalf("omp start = %+v, want args %q", st, tc.want)
			}
			if store.Deref(e.session("omp-review").AgentKind) != KindOMP {
				t.Fatal("agent_kind")
			}
		})
	}
}

func TestSessionSourceNoneNeverResumes(t *testing.T) {
	e := newEnv(t)
	omp := e.addRole(ompRole())
	e.setKind(KindOMP, func(k *config.Kind) { k.Wrapper, k.SessionSource = config.WrapperTrue, config.SessionNone })
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, nil, "talkable#11920", []config.Role{e.spec(RoleJudge), omp})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, omp, ws.Panes["omp-review"], "o-1"); err != nil {
		t.Fatal(err)
	}
	if args := e.h.starts[0].Args; slices.Contains(args, "--resume=o-1") {
		t.Fatalf("args = %q, want no resume", args)
	}
	e.h.setAgentSession("mg-11920-omp-review-5d01cf", "o-2")
	e.observe()
	if err := e.m.Quit(e.ctx, e.session("omp-review")); err != nil {
		t.Fatal(err)
	}
	var omps []store.Session
	for _, s := range e.sessions() {
		if s.Role == "omp-review" {
			omps = append(omps, s)
		}
	}
	if len(omps) != 1 || omps[0].State != store.SessionClosed {
		t.Fatalf("omp sessions = %+v, want one closed (not resumable)", omps)
	}
	if id, err := e.m.ResumeID(e.ctx, e.pr.ID, "omp-review"); err != nil || id != "" {
		t.Fatalf("ResumeID = %q, %v", id, err)
	}
}

func TestColumnsAndAnchors(t *testing.T) {
	if got := columns(6); !slices.Equal(got, []int{colRight, colRight, colLeft, colRight, colLeft, colRight}) {
		t.Fatalf("columns = %v", got)
	}
	order := []Role{RoleJudge, RoleClaude, RoleCodexReview, RoleSimplify, "droid-review", "omp-review"}
	want := map[Role][]anchor{
		RoleJudge:       {{RoleClaude, herdr.SplitDown}, {RoleCodexReview, herdr.SplitDown}, {RoleSimplify, herdr.SplitDown}, {"droid-review", herdr.SplitDown}, {"omp-review", herdr.SplitDown}},
		RoleClaude:      {{RoleJudge, herdr.SplitRight}, {RoleCodexReview, herdr.SplitDown}, {"droid-review", herdr.SplitDown}},
		RoleCodexReview: {{RoleClaude, herdr.SplitDown}, {RoleJudge, herdr.SplitRight}, {"droid-review", herdr.SplitDown}},
		RoleSimplify:    {{RoleJudge, herdr.SplitDown}, {"omp-review", herdr.SplitDown}},
		"droid-review":  {{RoleCodexReview, herdr.SplitDown}, {RoleClaude, herdr.SplitDown}, {RoleJudge, herdr.SplitRight}},
		"omp-review":    {{RoleSimplify, herdr.SplitDown}, {RoleJudge, herdr.SplitDown}},
	}
	for r, w := range want {
		if got := anchors(order, r); !slices.Equal(got, w) {
			t.Errorf("anchors(%s) = %v, want %v", r, got, w)
		}
	}
	if got := anchors(order, "unknown"); !slices.Equal(got, []anchor{{RoleJudge, herdr.SplitDown}}) {
		t.Errorf("anchors(unknown) = %v", got)
	}
}

func TestEnsureWorkspaceTwoRoles(t *testing.T) {
	e := newEnv(t)
	droid := e.addRole(droidRole())
	// The judge takes the root pane wherever it is listed.
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, map[string]string{"WT_BRANCH": "review1"}, "talkable#11920",
		[]config.Role{droid, e.spec(RoleJudge)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ws.Roles, []Role{RoleJudge, "droid-review"}) || len(ws.Panes) != 2 ||
		ws.Panes[RoleJudge] != "w1:p1" || ws.Panes["droid-review"] != "w1:p2" {
		t.Fatalf("ws = %+v", ws)
	}
	if len(e.h.splits) != 1 || e.h.splits[0].From != "w1:p1" || e.h.splits[0].Opts.Direction != herdr.SplitRight {
		t.Fatalf("splits = %+v", e.h.splits)
	}
	if e.h.renames["w1:p2"] != "PR #11920 droid-review" {
		t.Fatalf("renames = %v", e.h.renames)
	}
	if s := e.session("droid-review"); store.Deref(s.AgentName) != "mg-11920-droid-review-5d01cf" || store.Deref(s.AgentKind) != KindDroid {
		t.Fatalf("droid session = %+v", s)
	}
	if _, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, nil, "l", nil); err == nil {
		t.Fatal("no roles: want error")
	}
}

func TestEnsureWorkspaceFiveRolesAndEnv(t *testing.T) {
	e := newEnv(t)
	d := droidRole()
	d.Env = map[string]string{"DROID_MODE": "review", "SHARED": "role"}
	droid := e.addRole(d)
	e.setKind(KindDroid, func(k *config.Kind) { k.Env = map[string]string{"FACTORY_HOME": "/f", "SHARED": "kind"} })
	roles := []config.Role{e.spec(RoleJudge), e.spec(RoleClaude), e.spec(RoleCodexReview), e.spec(RoleSimplify), droid}
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, map[string]string{"WT_BRANCH": "review1", "SHARED": "ws"}, "talkable#11920", roles)
	if err != nil {
		t.Fatal(err)
	}
	want := map[Role]string{RoleJudge: "w1:p1", RoleClaude: "w1:p2", RoleCodexReview: "w1:p3", RoleSimplify: "w1:p4", "droid-review": "w1:p5"}
	for r, p := range want {
		if ws.Panes[r] != p {
			t.Fatalf("panes = %v, want %v", ws.Panes, want)
		}
	}
	// judge | claude-review / codex-review / droid-review, claude-simplify below the judge.
	type split struct{ from, dir string }
	wantSplits := []split{{"w1:p1", herdr.SplitRight}, {"w1:p2", herdr.SplitDown}, {"w1:p1", herdr.SplitDown}, {"w1:p3", herdr.SplitDown}}
	if len(e.h.splits) != len(wantSplits) {
		t.Fatalf("splits = %+v", e.h.splits)
	}
	for i, w := range wantSplits {
		if s := e.h.splits[i]; s.From != w.from || s.Opts.Direction != w.dir {
			t.Fatalf("split %d = %+v, want %+v", i, s, w)
		}
	}
	// The droid pane gets the kind's env, then the role's, over the workspace env.
	denv := e.h.splits[3].Opts.Env
	if denv["WT_BRANCH"] != "review1" || denv["FACTORY_HOME"] != "/f" || denv["DROID_MODE"] != "review" || denv["SHARED"] != "role" {
		t.Fatalf("droid pane env = %v", denv)
	}
	if s := e.session("droid-review"); s.Env["SHARED"] != "role" {
		t.Fatalf("droid session env = %v", s.Env)
	}
	if cenv := e.h.splits[0].Opts.Env; cenv["SHARED"] != "ws" || cenv["DROID_MODE"] != "" {
		t.Fatalf("claude pane env = %v", cenv)
	}
	if renv := e.h.creates[0].Env; renv["SHARED"] != "ws" {
		t.Fatalf("root pane env = %v", renv)
	}
	if s := e.session(RoleCodexReview); s.AgentName != nil || store.Deref(s.AgentKind) != KindShell {
		t.Fatalf("codex-review session = %+v", s)
	}

	// Repair: the droid pane is gone; it is split again below codex-review.
	e.h.removePane("w1:p5")
	again, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, nil, "talkable#11920", roles)
	if err != nil {
		t.Fatal(err)
	}
	if last := e.h.splits[len(e.h.splits)-1]; again.Created || last.From != "w1:p3" || last.Opts.Direction != herdr.SplitDown ||
		again.Panes["droid-review"] != last.New {
		t.Fatalf("repair = %+v, last split %+v", again, last)
	}
}

func TestEnsurePaneRoleAddedLater(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace()
	omp := e.addRole(ompRole())
	ws2, err := e.m.EnsurePane(e.ctx, e.pr, ws, slot1, map[string]string{"WT_BRANCH": "review1"}, omp)
	if err != nil {
		t.Fatal(err)
	}
	p := ws2.Panes["omp-review"]
	last := e.h.splits[len(e.h.splits)-1]
	// Third non-judge pane: below the judge.
	if p == "" || last.From != ws.Panes[RoleJudge] || last.Opts.Direction != herdr.SplitDown || last.New != p ||
		!slices.Equal(ws2.Roles, []Role{RoleJudge, RoleClaude, RoleCodexReview, "omp-review"}) {
		t.Fatalf("omp pane %q from %+v, roles %v", p, last, ws2.Roles)
	}
	if s := e.session("omp-review"); store.Deref(s.AgentKind) != KindOMP || store.Deref(s.AgentName) != "mg-11920-omp-review-5d01cf" {
		t.Fatalf("omp session = %+v", s)
	}
	e.setKind(KindOMP, func(k *config.Kind) { k.Wrapper = config.WrapperTrue })
	if err := e.m.StartAgent(e.ctx, e.pr, omp, p, ""); err != nil {
		t.Fatal(err)
	}
	if st := e.h.starts[0]; !slices.Equal(st.Args, []string{"--model=gpt-5.2", "--thinking=high"}) {
		t.Fatalf("omp start = %+v", st)
	}

	// A shell role added later gets a shell pane (no agent), down the right column.
	lint := e.addRole(config.Role{Name: "lint", Kind: config.KindShell, Command: "bin/lint {{.BaseRef}}"})
	ws3, err := e.m.EnsurePane(e.ctx, e.pr, ws2, slot1, nil, lint)
	if err != nil {
		t.Fatal(err)
	}
	last = e.h.splits[len(e.h.splits)-1]
	if last.From != ws.Panes[RoleCodexReview] || last.Opts.Direction != herdr.SplitDown || ws3.Panes["lint"] != last.New {
		t.Fatalf("lint pane from %+v, ws %+v", last, ws3)
	}
	if s := e.session("lint"); s.AgentName != nil || store.Deref(s.AgentKind) != KindShell {
		t.Fatalf("lint session = %+v", s)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, lint, ws3.Panes["lint"], ""); !errors.Is(err, ErrNotAgent) {
		t.Fatalf("StartAgent(shell) = %v, want ErrNotAgent", err)
	}
	if _, err := e.m.Prompt(e.ctx, e.pr, "lint", store.RunInitial, "x"); !errors.Is(err, ErrNotAgent) {
		t.Fatalf("Prompt(shell) = %v, want ErrNotAgent", err)
	}
}

func TestPromptDroidAndRunReportPath(t *testing.T) {
	e := newEnv(t)
	d := droidRole()
	d.Output = "droid.md"
	droid := e.addRole(d)
	e.setKind(KindDroid, func(k *config.Kind) { k.Wrapper = config.WrapperTrue })
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, nil, "talkable#11920", []config.Role{e.spec(RoleJudge), droid})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, droid, ws.Panes["droid-review"], ""); err != nil {
		t.Fatal(err)
	}
	e.clock.Add(time.Minute)
	// The alias resolves to the role's name.
	run, err := e.m.NewRun(e.ctx, e.pr, "droid", store.RunInitial, 1)
	if err != nil {
		t.Fatal(err)
	}
	if run.Role != "droid-review" || !strings.HasSuffix(store.Deref(run.ReportPath), "/droid.md") {
		t.Fatalf("run = %+v", run)
	}
	if err := e.m.Submit(e.ctx, run, "review it"); err != nil {
		t.Fatal(err)
	}
	if p := e.h.prompts[0]; p.Target != "mg-11920-droid-review-5d01cf" || p.Text != "review it" {
		t.Fatalf("prompt = %+v", p)
	}
	// droid has neither name args nor a rename command: never renamed.
	e.observe()
	if got := e.renames(ws.Panes["droid-review"]); len(got) != 0 {
		t.Fatalf("droid renames = %q", got)
	}
	if _, err := e.m.NewRun(e.ctx, e.pr, "nobody", store.RunInitial, 1); err == nil {
		t.Fatal("unknown role: want error")
	}
}

func TestNameAgentWithConfiguredRename(t *testing.T) {
	e := newEnv(t)
	droid := e.addRole(droidRole())
	e.setKind(KindDroid, func(k *config.Kind) { k.Wrapper, k.Rename = config.WrapperTrue, "/session-name {title}" })
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, nil, "talkable#11920", []config.Role{e.spec(RoleJudge), droid})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, droid, ws.Panes["droid-review"], ""); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Prompt(e.ctx, e.pr, "droid-review", store.RunInitial, "go"); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range e.h.runs {
		if r.Pane == ws.Panes["droid-review"] {
			got = append(got, r.Command)
		}
	}
	if !slices.Equal(got, []string{"/session-name PR #11920 droid-review - talkable"}) {
		t.Fatalf("pane runs = %q", got)
	}
}

func TestClassifyWithKindPatterns(t *testing.T) {
	rx, err := config.HealthPatterns{LoginRequired: []string{`session expired`}, UsageLimit: []string{`credits exhausted`}}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	cases := map[string]HealthKind{
		"Error: Session expired. Run `droid login`.": HealthLoginRequired,
		"Credits exhausted, try again in 2 hours":    HealthUsageLimit,
		"Not logged in":                         HealthOK, // the kind's list replaces the default one
		"stream disconnected before completion": HealthOK, // no overloaded patterns configured
		"Yes, I trust this folder":              HealthTrustDialog,
		"Do you want to proceed?":               HealthBlocked,
		"Conversation interrupted":              HealthStalled,
	}
	for text, want := range cases {
		h := ClassifyWith(rx, text, now)
		if h.Kind != want {
			t.Errorf("ClassifyWith(%q) = %s, want %s", text, h.Kind, want)
		}
		if h.Kind == HealthUsageLimit && (h.ResetAt == nil || !h.ResetAt.Equal(now.Add(2*time.Hour))) {
			t.Errorf("ResetAt = %v", h.ResetAt)
		}
	}
	// The defaults are what Classify uses.
	if ClassifyWith(defaultHealth, "Not logged in", now).Kind != HealthLoginRequired || ClassifyAt("Not logged in", now).Kind != HealthLoginRequired {
		t.Fatal("default patterns")
	}
}

func TestCheckHealthUsesTheSessionKind(t *testing.T) {
	e := newEnv(t)
	droid := e.addRole(droidRole())
	e.setKind(KindDroid, func(k *config.Kind) {
		k.Wrapper = config.WrapperTrue
		k.HealthPatterns.UsageLimit = []string{`credits exhausted`}
	})
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, nil, "talkable#11920",
		[]config.Role{e.spec(RoleJudge), droid, e.spec(RoleCodexReview)})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []config.Role{e.spec(RoleJudge), droid} {
		if err := e.m.StartAgent(e.ctx, e.pr, r, ws.Panes[Role(r.Name)], ""); err != nil {
			t.Fatal(err)
		}
	}
	const text = "Credits exhausted for this month"
	e.h.reads["mg-11920-droid-review-5d01cf"] = text
	e.h.reads["mg-11920-codex-judge-5d01cf"] = text
	e.h.reads[ws.Panes[RoleCodexReview]] = "Not logged in"
	for i := range 2 { // the second round comes from the cache
		if h, err := e.m.CheckHealth(e.ctx, e.session("droid-review")); err != nil || h.Kind != HealthUsageLimit {
			t.Fatalf("droid %d: %+v, %v", i, h, err)
		}
		if h, err := e.m.CheckHealth(e.ctx, e.session(RoleJudge)); err != nil || h.Kind != HealthOK {
			t.Fatalf("codex %d: %+v, %v", i, h, err)
		}
		// A shell role is classified with its tool's (codex's) patterns.
		if h, err := e.m.CheckHealth(e.ctx, e.session(RoleCodexReview)); err != nil || h.Kind != HealthLoginRequired {
			t.Fatalf("codex-review %d: %+v, %v", i, h, err)
		}
	}
}

func TestRecoverRestoresUserDefinedRole(t *testing.T) {
	e := newEnv(t)
	droid := e.addRole(droidRole())
	e.setKind(KindDroid, func(k *config.Kind) { k.Wrapper = config.WrapperTrue })
	ws, err := e.m.EnsureWorkspace(e.ctx, e.pr, slot1, nil, "talkable#11920", []config.Role{e.spec(RoleJudge), droid})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.StartAgent(e.ctx, e.pr, droid, ws.Panes["droid-review"], ""); err != nil {
		t.Fatal(err)
	}
	e.h.setAgentSession("mg-11920-droid-review-5d01cf", "droid-uuid")
	e.observe()
	if err := e.m.Park(e.ctx, e.pr); err != nil {
		t.Fatal(err)
	}
	if id, err := e.m.ResumeID(e.ctx, e.pr.ID, "droid-review"); err != nil || id != "droid-uuid" {
		t.Fatalf("ResumeID = %q, %v", id, err)
	}
	e.h.addWorkspace("w5")
	e.h.addPane(herdr.Pane{ID: "w5:p1", WorkspaceID: "w5", TabID: "w5:t1"})
	e.h.addAgent(herdr.AgentInfo{PaneID: "w5:p1", WorkspaceID: "w5", Agent: "droid", AgentStatus: herdr.StatusIdle,
		AgentSession: &herdr.AgentSession{Kind: "id", Value: "droid-uuid"}})
	rec, err := e.m.Recover(e.ctx, e.pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec) != 1 || rec[0].Role != "droid-review" || rec[0].Action != RecoverRestored {
		t.Fatalf("recovered = %+v", rec)
	}
	if s := e.session("droid-review"); store.Deref(s.HerdrPaneID) != "w5:p1" || s.State != store.SessionLive {
		t.Fatalf("droid = %+v", s)
	}
}
