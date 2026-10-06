package cli

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// rolesTestConfig adds a pipeline to inspTestConfig: the judge and
// claude-review inherited from the built-ins, an on-request droid role with
// an alias, a prompt override in prompts_dir and a watch running two roles.
const rolesTestConfig = `
[pipeline]
prompts_dir = "HOME/prompts"

[[role]]
name = "codex-judge"

[[role]]
name = "claude-review"

[[role]]
name = "droid-lint"
kind = "droid"
runs = "manual"
prompt = "claude-review.md"
aliases = ["lint"]

[kinds.claude]
on_permission_prompt = "wait"

[[watch]]
owner = "example"
include = ["*"]
identity = "zhuravel"
poll_identity = "zhuravel"
clone_root = "HOME"
roles = ["judge", "lint"]
`

// newRolesFixture is an inspect fixture whose config declares rolesTestConfig.
func newRolesFixture(t *testing.T) *inspFixture {
	t.Helper()
	f := newInspFixture(t)
	path := filepath.Join(f.Home, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(b) + strings.ReplaceAll(rolesTestConfig, "HOME", f.Home)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.Home, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.Home, "prompts", "claude-review.md"), []byte("Review {{.URL}}.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// rolesGroups splits `magnum roles` output into its header lines and the
// role names under each.
func rolesGroups(out string) map[string][]string {
	groups := map[string][]string{}
	head := ""
	for _, line := range strings.Split(out, "\n") {
		switch {
		case line == "" || strings.HasPrefix(line, "prompts_dir:"):
		case !strings.HasPrefix(line, "  "):
			head = line
			groups[head] = []string{}
		case strings.HasPrefix(line, "  NAME"), strings.Contains(line, " runs: "):
		default:
			groups[head] = append(groups[head], strings.Fields(line)[0])
		}
	}
	return groups
}

func TestRolesPrintsTheEffectiveRolesPerWatch(t *testing.T) {
	f := newRolesFixture(t)
	if code := f.run("roles"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err.String())
	}
	out := f.Out.String()
	actContains(t, out, "prompts_dir: "+f.Home+"/prompts",
		"NAME           KIND    RUNS    MODEL  EFFORT  CAPTURE  OUTPUT            PROMPT",
		"claude-review.md (prompts_dir)", "judge-initial.md (embedded)")
	groups := rolesGroups(out)
	want := map[string][]string{
		"watch talkable/talkable, posts as talkable-app, every role": {"codex-judge", "claude-review", "droid-lint"},
		"watch zhuravel/*, posts as zhuravel, every role":            {"codex-judge", "claude-review", "droid-lint"},
		"watch example/*, posts as zhuravel, roles judge, lint":      {"codex-judge", "droid-lint"},
	}
	for head, names := range want {
		if got, ok := groups[head]; !ok || !slices.Equal(got, names) {
			t.Errorf("group %q = %v (present %v), want %v\n%s", head, got, ok, names, out)
		}
	}
	var lint string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "droid-lint") {
			lint = strings.Join(strings.Fields(line), " ")
			break
		}
	}
	if want := "droid-lint droid manual - - file droid-lint.md claude-review.md (prompts_dir) - - lint"; lint != want {
		t.Errorf("droid-lint row = %q, want %q", lint, want)
	}

	if code := f.run("roles", "--repo", "example/site"); code != 0 {
		t.Fatalf("--repo exit %d: %s", code, f.Err.String())
	}
	g := rolesGroups(f.Out.String())
	if names := g["example/site: watch example/*, posts as zhuravel, roles judge, lint"]; !slices.Equal(names, []string{"codex-judge", "droid-lint"}) {
		t.Errorf("--repo example/site groups = %v", g)
	}
	if code := f.run("roles", "--repo", "nobody/x"); code != 0 || !strings.Contains(f.Out.String(), "nobody/x: no [[watch]] covers it") {
		t.Errorf("--repo nobody/x: exit %d\n%s", code, f.Out.String())
	}
	if code := f.run("roles", "--repo", "talkable"); code != 0 || !strings.Contains(f.Out.String(), "talkable/talkable: watch talkable/talkable") {
		t.Errorf("--repo talkable (default owner): exit %d\n%s", code, f.Out.String())
	}
	if code := f.run("roles", "--repo", "a/b/c"); code != 2 {
		t.Errorf("--repo a/b/c: exit %d", code)
	}
	if code := f.run("roles", "extra"); code != 2 {
		t.Errorf("extra argument: exit %d", code)
	}
}

func TestRolesJSONIsKeyedLikeConfig(t *testing.T) {
	f := newRolesFixture(t)
	if code := f.run("roles", "--repo", "example/site", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err.String())
	}
	var got []struct {
		Watch string           `json:"watch"`
		Repo  string           `json:"repo"`
		Roles []map[string]any `json:"roles"`
	}
	if err := json.Unmarshal(f.Out.Bytes(), &got); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	if len(got) != 1 || got[0].Watch != "example" || got[0].Repo != "example/site" || len(got[0].Roles) != 2 {
		t.Fatalf("groups = %+v", got)
	}
	judge, lint := got[0].Roles[0], got[0].Roles[1]
	if judge["name"] != "codex-judge" || judge["judge"] != true || judge["timeout"] != "1h30m0s" || judge["prompt_source"] != "embedded" {
		t.Errorf("judge = %v", judge)
	}
	if lint["name"] != "droid-lint" || lint["runs"] != "manual" || lint["prompt_source"] != "prompts_dir" ||
		!slices.Equal(anyStrings(lint["aliases"]), []string{"lint"}) {
		t.Errorf("droid-lint = %v", lint)
	}
	if !strings.Contains(f.Out.String(), `"name": "codex-judge",`+"\n"+`        "kind": "codex"`) {
		t.Errorf("keys not in config order:\n%s", f.Out.String())
	}
}

func anyStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestRolesKinds(t *testing.T) {
	f := newRolesFixture(t)
	if code := f.run("roles", "--kinds"); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err.String())
	}
	out := f.Out.String()
	actContains(t, out, "codex (used by codex-judge)\n", "claude (used by claude-review)\n", "droid (used by droid-lint)\n",
		"omp (no role uses it)\n", "codex login status (logged in: text:Logged in; fix: codex login)", "--resume {session}",
		"-c model_reasoning_effort={effort}", "codex -c model_reasoning_effort=xhigh", "claude --name {title}",
		"-c agents.max_concurrent_threads_per_session={subagents} (none: -c agents.enabled=false)",
		"mcp:", "-c mcp_servers.{server}.enabled=false per MCP server of the Codex config (allowed: -)",
		"project:", "-c projects={projects} when the PR changes .codex/ (the checkout untrusted for the session); else its MCP servers: allow")
	if i, j := strings.Index(out, "droid (used by"), strings.Index(out, "omp (no role"); i < 0 || j < i {
		t.Errorf("kinds the roles use come first:\n%s", out)
	}
	// Each kind says what magnum does at a permission prompt.
	permission := map[string]string{}
	for _, section := range strings.Split(out, "\n\n") {
		kind, _, _ := strings.Cut(section, " ")
		for _, line := range strings.Split(section, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "permission prompts:"); ok {
				permission[kind] = strings.TrimSpace(v)
			}
		}
	}
	if want := map[string]string{"codex": "deny", "claude": "wait", "droid": "deny", "omp": "deny"}; !maps.Equal(permission, want) {
		t.Errorf("permission prompts = %v, want %v\n%s", permission, want, out)
	}
	if code := f.run("roles", "--kinds", "--json"); code != 0 {
		t.Fatalf("--json exit %d: %s", code, f.Err.String())
	}
	var kinds []struct {
		Name   string         `json:"name"`
		UsedBy []string       `json:"used_by"`
		Spec   map[string]any `json:"spec"`
	}
	if err := json.Unmarshal(f.Out.Bytes(), &kinds); err != nil {
		t.Fatalf("%v\n%s", err, f.Out.String())
	}
	if len(kinds) != 4 || kinds[0].Name != "codex" || kinds[0].Spec["login_check"] != "codex login status" ||
		kinds[1].Spec["on_permission_prompt"] != "wait" ||
		!slices.Equal(kinds[2].UsedBy, []string{"droid-lint"}) || kinds[3].Name != "omp" || len(kinds[3].UsedBy) != 0 {
		t.Errorf("kinds = %+v", kinds)
	}
	if code := f.run("roles", "--kinds", "--repo", "talkable/talkable"); code != 2 {
		t.Errorf("--kinds with --repo: exit %d", code)
	}
}

func TestRoleCompletionListsConfiguredRoles(t *testing.T) {
	f := newRolesFixture(t)
	names := func(args ...string) string {
		var out []string
		for _, c := range complete(t, f.Ctx, args...) {
			name, _, _ := strings.Cut(c, "\t")
			out = append(out, name)
		}
		return strings.Join(out, ",")
	}
	if got := names("open", "5", "--role", ""); got != "codex-judge,claude-review,droid-lint,judge,claude,lint" {
		t.Errorf("open --role = %s", got)
	}
	if got := names("review", "5", "--role", ""); got != "droid-lint,codex-judge,claude-review" {
		t.Errorf("review --role = %s", got)
	}
	if got := names("resume", "--tool", ""); got != "codex,claude,droid,omp,all" {
		t.Errorf("resume --tool = %s", got)
	}
	if got := strings.Join(complete(t, f.Ctx, "watch", "5", "--role", ""), "\n"); !strings.Contains(got, "droid-lint\tdroid session, runs manual") ||
		!strings.Contains(got, "lint\talias of droid-lint") || !strings.Contains(got, "codex-judge\tcodex session, the judge that posts the review") {
		t.Errorf("descriptions:\n%s", got)
	}
}

func TestReviewRoleRequestsOnDemandRoles(t *testing.T) {
	h := newActHarness(t)
	h.pid = 4242 // a daemon runs: reviews and verdicts are queued only then
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	if code := h.cmd("review", "5", "--role", "simplify", "--role", "claude-simplify", "--role", "CLAUDE"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	reqs := h.requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %+v", reqs)
	}
	if p := actDecode[engine.ReviewPayload](t, reqs[0].Payload); !slices.Equal(p.Roles, []string{"claude-simplify", "claude-review"}) || p.Simplify {
		t.Fatalf("payload = %+v", p)
	}

	// --simplify stays the old flag.
	h.out.Reset()
	if code := h.cmd("review", "5", "--simplify", "--dry-run"); code != 0 {
		t.Fatalf("--simplify exit %d: %s", code, h.errb.String())
	}
	actContains(t, h.out.String(), `"simplify":true`)
	if strings.Contains(h.out.String(), `"roles"`) {
		t.Errorf("--simplify sent roles: %s", h.out.String())
	}

	cases := []struct {
		setup func(cfg *config.Config)
		args  []string
		code  int
		want  string
	}{
		{nil, []string{"5", "--role", "nope"}, 2, `unknown role "nope": use codex-judge, claude-review, codex-review or claude-simplify (aliases judge, claude, codex, codex_review, simplify)`},
		{nil, []string{"5", "--role", ""}, 2, "--role needs a role name"},
		{func(cfg *config.Config) { cfg.Watches[0].Roles = []string{"codex-judge", "claude-review"} },
			[]string{"5", "--role", "simplify"}, 1, `talkable/talkable does not run role "simplify": use codex-judge or claude-review (aliases judge, claude)`},
		{func(cfg *config.Config) {
			cfg.Watches[0].Roles = nil
			cfg.Roles[3].Runs = config.RunsNever
		}, []string{"5", "--role", "simplify"}, 1, `role claude-simplify is disabled (runs = "never" in config.toml)`},
	}
	for _, tc := range cases {
		if tc.setup != nil {
			tc.setup(h.d.Cfg)
		}
		h.errb.Reset()
		if code := h.cmd("review", tc.args...); code != tc.code || !strings.Contains(h.errb.String(), tc.want) {
			t.Errorf("review %v: exit %d, stderr %q; want %d and %q", tc.args, code, h.errb.String(), tc.code, tc.want)
		}
	}
	if n := len(h.requests()); n != 1 {
		t.Errorf("refused --role requests were queued: %d requests", n)
	}
}

func TestOpenRoleIsResolvedAgainstThePRsWatch(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	h.d.Cfg.Watches[0].Roles = []string{"codex-judge", "codex-review"}
	if code := h.cmd("open", "5", "--role", "claude"); code != 1 || !strings.Contains(h.errb.String(), `talkable/talkable does not run role "claude"`) {
		t.Fatalf("exit %d: %s", code, h.errb.String())
	}
	h.errb.Reset()
	if code := h.cmd("watch", "5", "--role", "claude"); code != 1 || !strings.Contains(h.errb.String(), `does not run role "claude"`) {
		t.Fatalf("watch: exit %d: %s", code, h.errb.String())
	}
	h.errb.Reset()
	if code := h.cmd("open", "5"); code != 1 || !strings.Contains(h.errb.String(), "has no codex-judge session yet") {
		t.Fatalf("default role: exit %d: %s", code, h.errb.String())
	}
}

func TestResumeArgvUsesTheKindsResumeArgs(t *testing.T) {
	cfg := config.Defaults()
	id, cwd := "s-1", "/tmp/x"
	kind := func(k string) *string { return &k }
	cases := []struct {
		s    store.Session
		want string
	}{
		{store.Session{Role: "codex-judge", SessionID: &id, AgentKind: kind("codex")}, "codex resume s-1"},
		{store.Session{Role: "claude-simplify", SessionID: &id}, "claude --resume s-1"},
		{store.Session{Role: "x", SessionID: &id, AgentKind: kind("omp")}, "omp --resume=s-1"},
		{store.Session{Role: "codex-review", SessionID: &id}, ""},
		{store.Session{Role: "codex-judge", AgentKind: kind("codex")}, ""},
	}
	for _, tc := range cases {
		if got := strings.Join(actResumeArgv(cfg, tc.s), " "); got != tc.want {
			t.Errorf("%s/%s: %q, want %q", tc.s.Role, store.Deref(tc.s.AgentKind), got, tc.want)
		}
	}
	s := store.Session{Role: "claude-review", SessionID: &id, Cwd: &cwd}
	if got := statusResume(nil, s); got != "cd /tmp/x && claude --resume s-1" {
		t.Errorf("statusResume = %q", got)
	}
	if got := openResumeHint(cfg, &s); got != "\nresume by hand: cd /tmp/x && claude --resume s-1" {
		t.Errorf("openResumeHint = %q", got)
	}
}
