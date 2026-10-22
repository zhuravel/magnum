package settings

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

var testNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)

// newSnapshot loads src as the user config of a fresh home (the user file
// under its own directory: a config.toml in the home itself would be a base
// file).
func newSnapshot(t *testing.T, src string) *Snapshot {
	t.Helper()
	storetest.Parallel(t)
	home := t.TempDir()
	dir := filepath.Join(home, "xdg", "magnum")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(dir, "config.toml")
	if src != "" {
		if err := os.WriteFile(user, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Load(paths.Layout{Home: home, UserConfig: user}, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/config.toml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func g(id FieldID) Target { return Target{Field: id} }

func text(s string) DraftValue { return DraftValue{Text: s} }

// TestFieldsNeverReachAForbiddenKey lists what the screen must never
// write: identities, auto_approve_as, keys, paths, commands, args, env,
// hooks, MCP, the sandbox and pools.
func TestFieldsNeverReachAForbiddenKey(t *testing.T) {
	forbidden := []string{"identity", "poll_identity", "auto_approve_as", "auto_approve_body", "private_key", "app_id", "client_id",
		"installation_id", "socket", "path", "dir", "clone_root", "codex_home", "command", "args", "start", "resume", "wrapper", "env",
		"strip_env", "hooks", "wt_hooks", "mcp", "plugin", "sandbox", "project_", "pool", "setup", "teardown", "prepare", "ready",
		"copy_files", "databases", "reset_db", "post_checkout", "skill", "prompt", "owner", "include", "exclude", "no_findings_event",
		"blocking_event", "review_footer", "transport", "login", "on_permission_prompt", "on_hooks_review", "health_patterns"}
	s := newSnapshot(t, fixture(t))
	all := append(Fields(), s.Fields()...)
	for _, f := range all {
		for part := range strings.SplitSeq(f.Key, ".") {
			if slices.Contains(forbidden, part) || strings.HasPrefix(part, "project_") || strings.Contains(part, "private_key") ||
				strings.HasPrefix(part, "mcp_") || strings.HasPrefix(part, "plugin_") || strings.HasPrefix(part, "login_") {
				t.Errorf("field %s reaches %s", f.ID, f.Key)
			}
		}
		if f.Section == "" || f.Label == "" || f.Description == "" || f.Key == "" {
			t.Errorf("field %s lacks a section, label, description or key: %+v", f.ID, f)
		}
	}
}

func TestFieldsListTheV1SettingsOncePerID(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	seen := map[FieldID]bool{}
	for _, f := range s.Fields() {
		if seen[f.ID] {
			t.Errorf("field %s listed twice", f.ID)
		}
		seen[f.ID] = true
		if !slices.Contains(Sections(), f.Section) {
			t.Errorf("field %s in an unknown section %q", f.ID, f.Section)
		}
	}
	for _, id := range []FieldID{"pipeline.spend", "kinds.codex.spend", "kinds.claude.spend", "spend.burn.claude-review.model",
		"spend.low.codex-judge.effort", "spend.high.claude-fix.rereview_effort", "role.claude-whiteboard.model", "usage.codex_soft",
		"watch.roles", "watch.auto_approve_authors", "repo.required_checks", "daemon.quiet_hours", "daemon.park_idle_after",
		"pipeline.judge_fallback"} {
		if !seen[id] {
			t.Errorf("no field %s", id)
		}
	}
	f, _ := s.Field("spend.burn.claude-review.rereview_effort")
	if f.Level != "burn" || f.Role != "claude-review" || f.Cell != CellRereviewEffort || f.Type != TypeEffort || f.Scope != ScopeSpendLevel {
		t.Errorf("a grid cell = %+v", f)
	}
	if f, _ := s.Field("role.claude-review.model"); f.Level != "" || f.Role != "claude-review" || f.Cell != CellModel {
		t.Errorf("a base model = %+v", f)
	}
	if got := s.Levels(); !slices.Equal(got, []string{"low", "medium", "high", "burn"}) {
		t.Errorf("Levels = %v", got)
	}
}

// The fallback judge shows the one that resolves: the built-in default is
// none under a base config whose own [[role]] list has no claude-judge,
// and an explicit "" in the user file is none too.
func TestTheFallbackJudgeShowsTheOneThatResolves(t *testing.T) {
	off := newSnapshot(t, "[pipeline]\njudge_fallback = \"\"\n")
	if v := off.Value(g("pipeline.judge_fallback")); v.Effective != "none" || v.State != StateSet {
		t.Errorf("judge_fallback = \"\": %+v", v)
	}
	home := t.TempDir()
	base := filepath.Join(home, "base.toml")
	if err := os.WriteFile(base, []byte("[[role]]\nname = \"codex-judge\"\n[[role]]\nname = \"claude-review\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(home, "xdg", "magnum", "config.toml")
	if err := os.MkdirAll(filepath.Dir(user), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := Load(paths.Layout{Home: home, UserConfig: user}, base)
	if err != nil {
		t.Fatal(err)
	}
	if v := s.Value(g("pipeline.judge_fallback")); v.Effective != "none" || v.State != StateDefault {
		t.Errorf("a base without claude-judge: %+v", v)
	}
}

func TestLoadReadsWhatTheFileSetsAndWhatApplies(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	for _, c := range []struct {
		t                       Target
		text, effective, source string
		state                   State
		line                    int
	}{
		{g("pipeline.spend"), "high", "high", "your file, line 42", StateSet, 42},
		{g("daemon.push_quiet_period"), "15m", "15m", "your file, line 7", StateSet, 7},
		{g("daemon.park_idle_after"), "", "15m", "built-in default", StateDefault, 0},
		{g("usage.codex_soft"), "90", "90", "your file, line 46", StateSet, 46},
		{g("kinds.codex.spend"), "", "inherit (high)", "[pipeline] spend", StateDefault, 0},
		{g("spend.burn.codex-judge.model"), "gpt-astra", "gpt-astra", "your file, line 53", StateSet, 53},
		{g("spend.high.claude-review.effort"), "", "inherit (xhigh)", "", StateDefault, 0},
		{g("spend.low.claude-review.model"), "", "inherit (opus)", "", StateDefault, 0},
		{g("role.claude-review.model"), "opus", "opus", "your file, line 39", StateSet, 39},
		{g("role.claude-judge.model"), "", "opus", "", StateDefault, 0},
		{Target{Field: "watch.include_drafts", Watch: 0}, "false", "false", "your file, line 22", StateSet, 22},
		{Target{Field: "watch.include_drafts", Watch: 1}, "", "false", "built-in default", StateDefault, 0},
		{g("pipeline.judge_fallback"), "", "claude-judge", "built-in default", StateDefault, 0},
		{Target{Field: "watch.auto_approve_skip_authors", Watch: 0}, "bob-rev", "bob-rev", "your file, line 25", StateSet, 25},
		{Target{Field: "watch.roles", Watch: 1}, "", "codex-judge, claude-review, codex-review, claude-simplify", "", StateDefault, 0},
		{Target{Field: "repo.stakes", Repo: "talkable/web"}, "payments and rewards", "payments and rewards", "your file, line 35", StateSet, 35},
		{Target{Field: "repo.stakes", Repo: "talkable/api"}, "", "inherit (referral emails and rewards reach real customers)", "", StateDefault, 0},
		{Target{Field: "repo.max_simplifications", Repo: "talkable/api"}, "", "inherit (5)", "", StateDefault, 0},
	} {
		v := s.Value(c.t)
		if v.Text != c.text || v.Effective != c.effective || v.State != c.state || v.Line != c.line ||
			(c.source != "" && v.Source != c.source) {
			t.Errorf("Value(%v) = %+v; want text %q, effective %q, state %d, line %d, source %q", c.t, v, c.text, c.effective,
				c.state, c.line, c.source)
		}
	}
	if v := s.Value(Target{Field: "watch.auto_approve", Watch: 0}); !slices.Equal(v.List, []string{"docs"}) {
		t.Errorf("auto_approve list = %v", v.List)
	}
	if got := s.KeyText(g("spend.high.claude-simplify.model")); got != "[spend.high].claude-simplify.model" {
		t.Errorf("KeyText = %q", got)
	}
	if got := s.KeyText(Target{Field: "watch.auto_approve_authors", Watch: 0}); got != "[[watch]] 1 (talkable) · auto_approve_authors" {
		t.Errorf("KeyText = %q", got)
	}
	if got := s.KeyText(g("pipeline.spend")); got != "[pipeline] spend" {
		t.Errorf("KeyText = %q", got)
	}
	ws := s.Watches()
	if len(ws) != 2 || ws[0].Owner != "talkable" || ws[0].Label != "talkable · web, api +1" || ws[0].Line != 17 || ws[1].Owner != "example" {
		t.Errorf("Watches = %+v", ws)
	}
	var names []string
	for _, r := range s.Repos() {
		names = append(names, r.Name)
		if r.HasBlock != (r.Name == "talkable/web") || r.Watch != 0 {
			t.Errorf("repo %+v", r)
		}
	}
	if !slices.Equal(names, []string{"talkable/api", "talkable/docs", "talkable/web"}) {
		t.Errorf("Repos = %v", names)
	}
	if got := s.Options(g("pipeline.judge_fallback")); !slices.Equal(got, []string{"", "codex-judge", "claude-judge"}) {
		t.Errorf("judge_fallback options = %v", got)
	}
	if got := s.Options(Target{Field: "watch.auto_approve", Watch: 0}); !slices.Equal(got, []string{"*", "web", "api", "docs"}) {
		t.Errorf("auto_approve options = %v", got)
	}
	if got := s.Options(g("spend.burn.claude-review.model")); !slices.Contains(got, "gpt-astra") || !slices.Contains(got, "opus") {
		t.Errorf("model options = %v", got)
	}
}

func TestLoadTakesAMissingFileAsAnEmptyOne(t *testing.T) {
	s := newSnapshot(t, "")
	if len(s.Bytes) != 0 || s.Sum != sha256.Sum256(nil) {
		t.Errorf("bytes %q", s.Bytes)
	}
	if v := s.Value(g("pipeline.spend")); v.State != StateDefault || v.Effective != "medium" {
		t.Errorf("spend = %+v", v)
	}
	p, err := s.Plan(Draft{g("pipeline.spend"): text("low")}, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Commit(p)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(s.Path)
	if string(got) != "[pipeline]\nspend = \"low\"\n" || res.History != "" {
		t.Errorf("new file %q, history %q", got, res.History)
	}
	if fi, _ := os.Stat(s.Path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
}

func TestPreviewResolvesSpendAsTheLoaderDoes(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	row := func(rows []RoleRow, name string) RoleRow {
		for _, r := range rows {
			if r.Role == name {
				return r
			}
		}
		t.Fatalf("no role %s in %+v", name, rows)
		return RoleRow{}
	}
	saved := s.Saved()
	if r := row(saved, "codex-judge"); r.Spend != "high" || r.Effort != "xhigh" || r.Kind != "codex" {
		t.Errorf("saved codex-judge = %+v", r)
	}
	if r := row(saved, "claude-review"); r.Model != "opus" || r.Effort != "xhigh" {
		t.Errorf("saved claude-review = %+v", r)
	}
	// burn: medium's efforts, then the table over them.
	rows, err := s.Preview(Draft{g("pipeline.spend"): text("burn")})
	if err != nil {
		t.Fatal(err)
	}
	if r := row(rows, "codex-judge"); r.Model != "gpt-astra" || r.Effort != "xhigh" || r.RereviewEffort != "high" || r.Spend != "burn" {
		t.Errorf("burn codex-judge = %+v", r)
	}
	if r := row(rows, "claude-review"); r.Model != "opus" || r.Effort != "high" || r.RereviewEffort != "medium" {
		t.Errorf("burn claude-review = %+v", r)
	}
	// A kind's own level takes its roles alone.
	rows, err = s.Preview(Draft{g("kinds.codex.spend"): text("low")})
	if err != nil {
		t.Fatal(err)
	}
	if r := row(rows, "codex-judge"); r.Spend != "low" || r.Effort != "high" {
		t.Errorf("codex at low = %+v", r)
	}
	if r := row(rows, "claude-review"); r.Spend != "high" || r.Effort != "xhigh" {
		t.Errorf("claude at high = %+v", r)
	}
	// A level's table wins over the [[role]] block; the role block's model
	// shows under a level whose table does not name the role.
	rows, err = s.Preview(Draft{g("spend.high.claude-review.model"): text("sonnet"), g("role.claude-review.model"): text("fable")})
	if err != nil {
		t.Fatal(err)
	}
	if r := row(rows, "claude-review"); r.Model != "sonnet" {
		t.Errorf("table over role block = %+v", r)
	}
	rows, err = s.Preview(Draft{g("pipeline.spend"): text("medium"), g("role.claude-review.model"): text("fable")})
	if err != nil {
		t.Fatal(err)
	}
	if r := row(rows, "claude-review"); r.Model != "fable" {
		t.Errorf("role block under medium = %+v", r)
	}
	if _, err := s.Preview(Draft{g("kinds.codex.spend"): text("nope")}); err == nil {
		t.Error("an unknown level previewed")
	}
}

func TestPlanWritesDatedNotesAndADiffOfTheChangedLinesOnly(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	d := Draft{
		g("pipeline.spend"):                          text("burn"),
		g("spend.burn.codex-judge.model"):            text("gpt-sol"),
		g("spend.burn.codex-judge.effort"):           text("high"),
		g("usage.codex_soft"):                        text("85"),
		g("daemon.park_idle_after"):                  text("30m"),
		g("daemon.quiet_hours"):                      {Default: true},
		{Field: "watch.auto_approve_authors"}:        {List: []string{"alice", "rev-ann"}},
		{Field: "repo.stakes", Repo: "talkable/web"}: text("payments and rewards"), // unchanged: no edit
	}
	p, err := s.Plan(d, true, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if p.Count != 7 {
		t.Errorf("Count = %d, want 7", p.Count)
	}
	want := strings.NewReplacer(
		"max_concurrent_reviews = 5        # 2026-10-01: was 3\n",
		"max_concurrent_reviews = 5        # 2026-10-01: was 3\n",
		// quiet_hours goes first (Schedule before Reviews & agents) and leaves
		// its comment; park_idle_after then follows the table's last key.
		"                                  # fewer rounds for ten more minutes\nquiet_hours = \"03:00-10:00\"       # local time\n",
		"                                  # fewer rounds for ten more minutes\npark_idle_after = \"30m\"           # 2026-10-20: was the default\n# 2026-10-20: quiet_hours back to the default, was \"03:00-10:00\"; local time\n",
		"stakes = \"referral emails and rewards reach real customers\"\n",
		"stakes = \"referral emails and rewards reach real customers\"\nauto_approve_authors = [\"alice\", \"rev-ann\"]  # 2026-10-20: was the default\n",
		"spend = \"high\"                    # 2026-10-14: medium -> high\n",
		"spend = \"burn\"                    # 2026-10-20: was \"high\"; 2026-10-14: medium -> high\n",
		"codex_soft = 90                   # 2026-10-16: 80 -> 90\n",
		"codex_soft = 85                   # 2026-10-20: was 90; 2026-10-16: 80 -> 90\n",
		"codex-judge = { model = \"gpt-astra\", effort = \"xhigh\" }\n",
		"codex-judge = { model = \"gpt-sol\", effort = \"high\" }  # 2026-10-20: was model \"gpt-astra\", effort \"xhigh\"\n",
	).Replace(fixture(t))
	if string(p.New) != want {
		t.Errorf("new file:\n%s\nwant:\n%s", p.New, want)
	}
	var tables []string
	for _, h := range p.Hunks {
		tables = append(tables, h.Table)
	}
	if !slices.Equal(tables, []string{"[daemon]", "[[watch]] 1 (talkable)", "[pipeline]", "[usage]", "[spend.burn]"}) {
		t.Errorf("hunk tables = %v", tables)
	}
	if h := p.Hunks[2]; h.Line != 42 || !slices.Equal(h.Removed, []string{`spend = "high"                    # 2026-10-14: medium -> high`}) {
		t.Errorf("the spend hunk = %+v", h)
	}
	if !strings.Contains(p.Diff, "@@ -42 +44 @@ [pipeline]\n-spend = \"high\"") || !strings.Contains(p.Diff, "@@ -26,0 +28 @@ [[watch]] 1 (talkable)\n+auto") {
		t.Errorf("diff:\n%s", p.Diff)
	}
	if !slices.Contains(p.Keys, "pipeline.spend") || !slices.Contains(p.Keys, "watch[0].auto_approve_authors[1]") {
		t.Errorf("Keys = %v", p.Keys)
	}
	for _, k := range p.Keys {
		if strings.Contains(k, "rev-ann") || strings.Contains(k, "gpt-sol") {
			t.Errorf("a key names a value: %s", k)
		}
	}
	if got, _ := os.ReadFile(s.Path); string(got) != fixture(t) {
		t.Error("Plan wrote the file")
	}
}

func TestPlanWithoutNotesChangesOnlyTheValue(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	p, err := s.Plan(Draft{g("pipeline.fix_cycles"): text("3")}, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(fixture(t), "fix_cycles = 5\n", "fix_cycles = 3\n", 1); string(p.New) != want {
		t.Errorf("new file:\n%s", p.New)
	}
	empty, err := s.Plan(Draft{g("pipeline.fix_cycles"): text("5")}, true, testNow)
	if err != nil || empty.Count != 0 || len(empty.Hunks) != 0 || string(empty.New) != fixture(t) {
		t.Errorf("a draft of the values the file has = %+v, %v", empty, err)
	}
}

func TestPlanCreatesTheBlocksATargetNeeds(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	d := Draft{
		{Field: "repo.stakes", Repo: "talkable/api"}:              text("the public API"),
		{Field: "repo.max_simplifications", Repo: "talkable/api"}: text("2"),
		{Field: "repo.allow_bot_authors", Repo: "talkable/docs"}:  {List: []string{"renovate[bot]"}},
		g("role.claude-simplify.model"):                           text("sonnet"),
		g("kinds.claude.spend"):                                   text("low"),
		g("spend.medium.claude-review.effort"):                    text("high"),
	}
	p, err := s.Plan(d, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	got := string(p.New)
	for _, want := range []string{
		"[[repo]]\nrepo = \"talkable/web\"\nstakes = \"payments and rewards\"   # 2026-10-15\n\n[[repo]]\nrepo = \"talkable/api\"\nstakes = \"the public API\"\nmax_simplifications = 2\n\n[[repo]]\nrepo = \"talkable/docs\"\nallow_bot_authors = [\"renovate[bot]\"]\n\n[[role]]",
		"model = \"opus\"                    # 2026-10-20: fable -> opus\n\n[[role]]\nname = \"claude-simplify\"\nmodel = \"sonnet\"\n\n[pipeline]",
		"[spend.burn]\ncodex-judge = { model = \"gpt-astra\", effort = \"xhigh\" }\n\n[spend.medium]\nclaude-review = { effort = \"high\" }\n",
		"# local time\n\n[kinds.claude]\nspend = \"low\"\n\n[[identity]]\n", // before the first array of tables
	} {
		if !strings.Contains(got, want) {
			t.Errorf("new file lacks:\n%s\n\ngot:\n%s", want, got)
		}
	}
	if p.Count != 6 {
		t.Errorf("Count = %d", p.Count)
	}
}

func TestPlanRefusesAChangedFileAndAnInvalidValue(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	for name, d := range map[string]Draft{
		"soft above hard":      {g("usage.codex_soft"): text("99")},
		"a level not declared": {g("pipeline.spend"): text("nope")},
		"a bad duration":       {g("daemon.park_idle_after"): text("30s")},
		"a judge-less watch":   {{Field: "watch.roles"}: {List: []string{"claude-review"}}},
		"an unknown role":      {{Field: "watch.roles"}: {List: []string{"codex-judge", "nobody"}}},
		"an app login":         {{Field: "watch.auto_approve_authors"}: {List: []string{"app/x"}}},
		"stakes too long":      {{Field: "watch.stakes"}: text(strings.Repeat("x", 301))},
		"a quiet window":       {g("daemon.quiet_hours"): text("25:00-03:00")},
		"no such watch":        {{Field: "watch.stakes", Watch: 5}: text("x")},
	} {
		if _, err := s.Plan(d, true, testNow); err == nil {
			t.Errorf("%s: Plan passed", name)
		}
	}
	if err := os.WriteFile(s.Path, []byte(fixture(t)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Plan(Draft{g("pipeline.spend"): text("low")}, true, testNow); !errors.Is(err, ErrChanged) {
		t.Errorf("Plan over a changed file = %v, want ErrChanged", err)
	}
}

func TestCommitKeepsTheModeAndTheLastTwentyVersions(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	if err := os.Chmod(s.Path, 0o640); err != nil {
		t.Fatal(err)
	}
	cur := s
	for i := range 25 {
		p, err := cur.Plan(Draft{g("pipeline.fix_cycles"): text(strconv.Itoa(10 + i))}, false, testNow)
		if err != nil {
			t.Fatal(i, err)
		}
		old := cur.Bytes
		res, err := Commit(p)
		if err != nil {
			t.Fatal(i, err)
		}
		if kept, _ := os.ReadFile(res.History); string(kept) != string(old) {
			t.Fatalf("history %s holds another version", res.History)
		}
		if got, _ := os.ReadFile(s.Path); res.Sum != sha256.Sum256(got) || string(got) != string(p.New) {
			t.Fatal("the file is not the plan's")
		}
		if _, err := Commit(p); !errors.Is(err, ErrChanged) {
			t.Fatalf("a second commit of the same plan = %v, want ErrChanged", err)
		}
		if cur, err = Load(cur.layout, ""); err != nil {
			t.Fatal(err)
		}
	}
	if fi, _ := os.Stat(s.Path); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, want 0640 kept", fi.Mode().Perm())
	}
	hist, err := cur.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != HistoryKeep {
		t.Errorf("%d history entries, want %d", len(hist), HistoryKeep)
	}
	for i := 1; i < len(hist); i++ {
		if hist[i].Path >= hist[i-1].Path {
			t.Errorf("history not newest first: %s after %s", hist[i].Path, hist[i-1].Path)
		}
	}
	if fi, _ := os.Stat(filepath.Dir(hist[0].Path)); fi.Mode().Perm() != 0o700 {
		t.Errorf("history dir mode %v", fi.Mode().Perm())
	}
}

func TestCommitRefusesASymlinkedConfig(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	target := s.Path + ".real"
	if err := os.Rename(s.Path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, s.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Plan(Draft{g("pipeline.spend"): text("low")}, false, testNow); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("Plan over a symlink = %v", err)
	}
	if fi, _ := os.Lstat(s.Path); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced")
	}
}

func TestRestorePutsAnEarlierVersionBackThroughTheSameChecks(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	p, err := s.Plan(Draft{g("pipeline.spend"): text("low")}, true, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(p); err != nil {
		t.Fatal(err)
	}
	s2, err := Load(s.layout, "")
	if err != nil {
		t.Fatal(err)
	}
	hist, err := s2.History()
	if err != nil || len(hist) != 1 {
		t.Fatalf("history = %v, %v", hist, err)
	}
	r, err := s2.Restore(hist[0], testNow)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.New) != fixture(t) || r.Count != 1 || len(r.Hunks) != 1 || r.Hunks[0].Table != "[pipeline]" {
		t.Errorf("restore plan = %+v", r)
	}
	if _, err := Commit(r); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(s.Path); string(got) != fixture(t) {
		t.Error("the restore did not put the first version back")
	}
	if hist, _ := s2.History(); len(hist) != 2 {
		t.Errorf("the restore kept %d versions, want 2", len(hist))
	}
	if _, err := s2.Restore(HistoryEntry{Path: s.Path}, testNow); err == nil {
		t.Error("a file outside the history restored")
	}
}

// TestEveryFieldWritesAKeyMagnumLoads saves each field to a valid value of
// its own (and back to the default): the key exists where the loader reads
// it, and the save path accepts it.
func TestEveryFieldWritesAKeyMagnumLoads(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	sample := map[Type]DraftValue{
		TypeBool: text("true"), TypeDuration: text("45m"), TypeInt: text("2"), TypePercent: text("50"), TypeText: text("a short line"),
		TypeModel: text("opus"), TypeEffort: text("high"), TypeLogins: {List: []string{"alice"}}, TypeRepos: {List: []string{"web"}},
		TypeChecks: {List: []string{"workflow:CI"}},
	}
	for _, f := range s.Fields() {
		tg := Target{Field: f.ID, Repo: "talkable/api"}
		v, ok := sample[f.Type]
		switch {
		case f.ID == "watch.roles":
			v, ok = DraftValue{List: []string{"codex-judge", "claude-review"}}, true
		case f.ID == "daemon.quiet_hours":
			v, ok = text("01:00-06:00"), true
		case f.ID == "watch.auto_approve" || f.ID == "watch.manual_repos":
			v = DraftValue{List: []string{"api"}}
		case f.ID == "usage.codex_soft":
			v = text("50")
		case f.ID == "usage.codex_hard":
			v = text("97")
		case f.Type == TypeEnum:
			opts := slices.DeleteFunc(s.Options(tg), func(o string) bool { return o == "" })
			v, ok = text(opts[len(opts)-1]), true
		}
		if !ok {
			t.Errorf("no sample for %s", f.ID)
			continue
		}
		if _, err := s.Plan(Draft{tg: v}, true, testNow); err != nil {
			t.Errorf("%s = %+v: %v", f.ID, v, err)
		}
		if _, err := s.Plan(Draft{tg: {Default: true}}, true, testNow); err != nil {
			t.Errorf("%s back to the default: %v", f.ID, err)
		}
	}
}
