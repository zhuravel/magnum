package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
)

// snapTOML is a minimal config for the snapshot tests (prompts_dir defaults to
// <home>/prompts).
const snapTOML = `
[[identity]]
name = "z"
kind = "gh"
login = "z"
[[watch]]
owner = "example"
include = ["*"]
identity = "z"
`

var snapNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

const snapDay = 24 * time.Hour

// snapLoad loads snapTOML from a fresh home holding files (relative paths).
func snapLoad(t *testing.T, files map[string]string) (*Config, string) {
	t.Helper()
	all := map[string]string{"config.toml": snapTOML}
	for k, v := range files {
		all[k] = v
	}
	home := t.TempDir()
	cfg, err := loadFiles(t, home, all)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, home
}

func snapWrite(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func snapSetMod(t *testing.T, path string, mod time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func snapModTime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func snapRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func snapExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// snapSetSkill points every judge role at the skill file src.
func snapSetSkill(cfg *Config, src string) {
	for i := range cfg.Roles {
		if cfg.Roles[i].Judge {
			cfg.Roles[i].Skill = src
		}
	}
}

func snapCopyPath(skillDir, text string) string {
	sum := sha256.Sum256([]byte(text))
	return filepath.Join(skillDir, hex.EncodeToString(sum[:])[:12], SkillCopyName)
}

// A daemon keeps answering with the prompt text it loaded at startup: an
// edited or deleted file shows up in Changed (the next restart loads it), a
// file put back with the same content (new mtime) does not.
func TestSnapshotPromptSurvivesEditsAndChangedComparesContent(t *testing.T) {
	const orig = "custom judge {{.URL}}"
	cfg, home := snapLoad(t, map[string]string{"prompts/judge-initial.md": orig})
	file := filepath.Join(home, "prompts", "judge-initial.md")
	snap, err := cfg.SnapshotPrompts("", snapNow)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PromptSnapshot() != snap {
		t.Fatal("SnapshotPrompts did not install the snapshot")
	}
	text := func() string {
		t.Helper()
		p, err := cfg.ResolvePrompt("judge-initial.md")
		if err != nil {
			t.Fatal(err)
		}
		if p.Embedded || p.Path != file {
			t.Fatalf("prompt = %+v, want the file %s", p, file)
		}
		return p.Text
	}
	changed := func(step string, want ...string) {
		t.Helper()
		if got := snap.Changed(); !slices.Equal(got, want) {
			t.Fatalf("%s: Changed() = %q, want %q", step, got, want)
		}
	}
	base := snapModTime(t, file)

	if text() != orig {
		t.Fatalf("text = %q", text())
	}
	changed("untouched")

	snapWrite(t, file, "edited judge {{.URL}} and more")
	if text() != orig {
		t.Fatalf("after an edit ResolvePrompt = %q, want the snapshot's %q", text(), orig)
	}
	changed("edited", "judge-initial.md")

	snapWrite(t, file, orig)
	snapSetMod(t, file, base.Add(time.Minute))
	changed("restored with a new mtime")

	snapWrite(t, file, "CUSTOM judge {{.URL}}") // same size, other text
	snapSetMod(t, file, base.Add(2*time.Minute))
	changed("same size, other content", "judge-initial.md")

	snapWrite(t, file, orig)
	snapSetMod(t, file, base.Add(3*time.Minute))
	changed("restored again")

	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if text() != orig {
		t.Fatalf("after a delete ResolvePrompt = %q, want the snapshot's %q", text(), orig)
	}
	changed("deleted", "judge-initial.md")
}

// A prompt that came from the embedded defaults at startup is reported once a
// file of that name appears in prompts_dir (the next restart would use it),
// while the daemon keeps the default.
func TestSnapshotEmbeddedDefaultIsChangedWhenAFileAppears(t *testing.T) {
	cfg, home := snapLoad(t, nil)
	snap, err := cfg.SnapshotPrompts("", snapNow)
	if err != nil {
		t.Fatal(err)
	}
	before, err := cfg.ResolvePrompt("judge-stop.md")
	if err != nil || !before.Embedded {
		t.Fatalf("judge-stop.md = %+v %v, want the embedded default", before, err)
	}
	if got := snap.Changed(); len(got) != 0 {
		t.Fatalf("Changed() = %q, want none", got)
	}
	snapWrite(t, filepath.Join(home, "prompts", "judge-stop.md"), "my stop")
	if got := snap.Changed(); !slices.Equal(got, []string{"judge-stop.md"}) {
		t.Fatalf("Changed() = %q, want [judge-stop.md]", got)
	}
	after, err := cfg.ResolvePrompt("judge-stop.md")
	if err != nil || !after.Embedded || after.Text != before.Text {
		t.Fatalf("after the file appeared: %+v %v, want the embedded default", after, err)
	}
}

// The CLI never takes a snapshot: ResolvePrompt reads the file as it is now.
func TestResolvePromptWithoutSnapshotReadsCurrentDisk(t *testing.T) {
	cfg, home := snapLoad(t, map[string]string{"prompts/judge-initial.md": "v1 {{.URL}}"})
	file := filepath.Join(home, "prompts", "judge-initial.md")
	if cfg.PromptSnapshot() != nil {
		t.Fatal("a loaded Config must not hold a snapshot")
	}
	p, err := cfg.ResolvePrompt("judge-initial.md")
	if err != nil || p.Text != "v1 {{.URL}}" {
		t.Fatalf("first read: %+v %v", p, err)
	}
	snapWrite(t, file, "v2 {{.URL}}")
	p, err = cfg.ResolvePrompt("judge-initial.md")
	if err != nil || p.Text != "v2 {{.URL}}" {
		t.Fatalf("after an edit: %+v %v, want the new text", p, err)
	}
	// A missing snapshot is safe to ask.
	var none *PromptSnapshot
	if none.Changed() != nil || none.Files() != 0 {
		t.Fatalf("nil snapshot: Changed %q, Files %d", none.Changed(), none.Files())
	}
}

// Extra names (model-fallback.md) are loaded with the roles' prompts.
func TestSnapshotLoadsExtraPromptNames(t *testing.T) {
	files := map[string]string{"prompts/model-fallback.md": "fallback v1"}
	plain, _ := snapLoad(t, files)
	ps, err := plain.SnapshotPrompts("", snapNow)
	if err != nil {
		t.Fatal(err)
	}

	cfg, home := snapLoad(t, files)
	snap, err := cfg.SnapshotPrompts("", snapNow, "model-fallback.md", "model-fallback.md")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Files() != ps.Files()+1 {
		t.Fatalf("Files() = %d, want %d (one extra, listed twice)", snap.Files(), ps.Files()+1)
	}
	snapWrite(t, filepath.Join(home, "prompts", "model-fallback.md"), "fallback v2, longer")
	p, err := cfg.ResolvePrompt("model-fallback.md")
	if err != nil || p.Text != "fallback v1" {
		t.Fatalf("extra prompt = %+v %v, want the snapshot's text", p, err)
	}
	if got := snap.Changed(); !slices.Equal(got, []string{"model-fallback.md"}) {
		t.Fatalf("Changed() = %q", got)
	}
}

// A prompt name that resolves nowhere fails the whole snapshot, wrapping
// ErrPromptNotFound, and installs nothing.
func TestSnapshotUnknownPromptFailsAndInstallsNothing(t *testing.T) {
	cases := map[string]struct {
		edit  func(*Config)
		extra []string
	}{
		"role prompt": {edit: func(c *Config) {
			for i := range c.Roles {
				if c.Roles[i].Name == RoleClaudeReview {
					c.Roles[i].Prompt = "nope.md"
				}
			}
		}},
		"extra name": {extra: []string{"nope.md"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, _ := snapLoad(t, nil)
			if tc.edit != nil {
				tc.edit(cfg)
			}
			snap, err := cfg.SnapshotPrompts("", snapNow, tc.extra...)
			if !errors.Is(err, ErrPromptNotFound) {
				t.Fatalf("err = %v, want ErrPromptNotFound", err)
			}
			if !strings.Contains(err.Error(), "nope.md") {
				t.Fatalf("err = %v does not name the prompt", err)
			}
			if snap != nil || cfg.PromptSnapshot() != nil {
				t.Fatalf("snapshot = %v / %v, want nothing installed", snap, cfg.PromptSnapshot())
			}
		})
	}
}

// A judge's skill is copied to <skillDir>/<12 hex of its SHA-256>/SKILL.md;
// the judge is pointed at the copy and an edit of the source only shows up in
// Changed.
func TestSnapshotCopiesJudgeSkillByContentHash(t *testing.T) {
	cfg, home := snapLoad(t, map[string]string{"mine/SKILL.md": "skill v1"})
	src := filepath.Join(home, "mine", "SKILL.md")
	snapSetSkill(cfg, src)
	skillDir := filepath.Join(t.TempDir(), "copies")

	snap, err := cfg.SnapshotPrompts(skillDir, snapNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Warnings) != 0 {
		t.Fatalf("Warnings = %q", snap.Warnings)
	}
	want := snapCopyPath(skillDir, "skill v1")
	if got := cfg.SkillFile(src); got != want {
		t.Fatalf("SkillFile(src) = %q, want %q", got, want)
	}
	if got := snapRead(t, want); got != "skill v1" {
		t.Fatalf("copy = %q", got)
	}
	other := filepath.Join(home, "elsewhere", "SKILL.md")
	if got := cfg.SkillFile(other); got != other {
		t.Fatalf("SkillFile of an unknown path = %q, want it unchanged", got)
	}
	if got := snap.Changed(); len(got) != 0 {
		t.Fatalf("Changed() = %q, want none", got)
	}

	snapWrite(t, src, "skill v2, edited")
	if got := snap.Changed(); !slices.Equal(got, []string{src}) {
		t.Fatalf("Changed() = %q, want [%s]", got, src)
	}
	if got := snapRead(t, want); got != "skill v1" {
		t.Fatalf("the copy changed with the source: %q", got)
	}
	if got := cfg.SkillFile(src); got != want {
		t.Fatalf("SkillFile(src) after the edit = %q, want the copy %q", got, want)
	}
}

// A skill file that cannot be read is a warning, not a failure: the judge
// keeps the configured path.
func TestSnapshotMissingSkillWarnsAndKeepsThePath(t *testing.T) {
	cfg, home := snapLoad(t, nil)
	src := filepath.Join(home, "gone", "SKILL.md")
	snapSetSkill(cfg, src)
	skillDir := filepath.Join(t.TempDir(), "copies")

	snap, err := cfg.SnapshotPrompts(skillDir, snapNow)
	if err != nil {
		t.Fatalf("a missing skill must not fail the snapshot: %v", err)
	}
	if len(snap.Warnings) != 1 || !strings.Contains(snap.Warnings[0], src) || !strings.Contains(snap.Warnings[0], RoleCodexJudge) {
		t.Fatalf("Warnings = %q, want one naming %s and %s", snap.Warnings, RoleCodexJudge, src)
	}
	if got := cfg.SkillFile(src); got != src {
		t.Fatalf("SkillFile(src) = %q, want the configured path", got)
	}
	if ents, _ := os.ReadDir(skillDir); len(ents) != 0 {
		t.Fatalf("skill dir holds %d entries, want none", len(ents))
	}
}

// skillDir "" turns the copies off: the judges keep their configured paths.
func TestSnapshotWithoutSkillDirMakesNoCopies(t *testing.T) {
	cfg, home := snapLoad(t, map[string]string{"mine/SKILL.md": "skill v1"})
	src := filepath.Join(home, "mine", "SKILL.md")
	snapSetSkill(cfg, src)

	snap, err := cfg.SnapshotPrompts("", snapNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Warnings) != 0 {
		t.Fatalf("Warnings = %q", snap.Warnings)
	}
	if got := cfg.SkillFile(src); got != src {
		t.Fatalf("SkillFile(src) = %q, want the configured path", got)
	}
	if snap.Files() != len(snap.prompts) {
		t.Fatalf("Files() = %d, want only the %d prompts", snap.Files(), len(snap.prompts))
	}
}

// Copies no judge uses and older than a week go; recent ones, directories
// that are not copies and the copy in use stay.
func TestSnapshotPrunesOnlyStaleUnusedSkillCopies(t *testing.T) {
	cfg, home := snapLoad(t, map[string]string{"mine/SKILL.md": "skill v1"})
	src := filepath.Join(home, "mine", "SKILL.md")
	snapSetSkill(cfg, src)
	skillDir := t.TempDir()

	if _, err := cfg.SnapshotPrompts(skillDir, snapNow); err != nil {
		t.Fatal(err)
	}
	inUse := cfg.SkillFile(src)
	if inUse != snapCopyPath(skillDir, "skill v1") {
		t.Fatalf("copy = %q", inUse)
	}
	snapSetMod(t, inUse, snapNow.Add(-30*snapDay))

	mk := func(dir string, age time.Duration) string {
		t.Helper()
		f := filepath.Join(skillDir, dir, SkillCopyName)
		snapWrite(t, f, "old skill")
		snapSetMod(t, f, snapNow.Add(-age))
		return filepath.Join(skillDir, dir)
	}
	stale := mk("aaaaaaaaaaaa", 8*snapDay)
	recent := mk("bbbbbbbbbbbb", snapDay)
	notACopy := mk("zzzzzzzzzzzz", 30*snapDay) // 12 characters, not hex
	short := mk("abcdef", 30*snapDay)
	noFile := filepath.Join(skillDir, "cccccccccccc") // a copy dir without SKILL.md
	if err := os.Mkdir(noFile, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := cfg.SnapshotPrompts(skillDir, snapNow); err != nil {
		t.Fatal(err)
	}
	for dir, wantKept := range map[string]bool{
		stale:               false,
		noFile:              false,
		recent:              true,
		notACopy:            true,
		short:               true,
		filepath.Dir(inUse): true,
		skillDir:            true,
	} {
		if got := snapExists(dir); got != wantKept {
			t.Errorf("%s exists = %v, want %v", dir, got, wantKept)
		}
	}
	if got := snapRead(t, inUse); got != "skill v1" {
		t.Fatalf("in-use copy = %q", got)
	}
}

func TestSkillPath(t *testing.T) {
	layout := paths.Layout{Home: filepath.Join(string(filepath.Separator), "home", "magnum")}
	cases := map[string]struct{ skill, want string }{
		"default":  {"", layout.Skill()},
		"{{repo}}": {"{{repo}}/x/SKILL.md", filepath.Join(layout.Home, "x", "SKILL.md")},
		"absolute": {"/opt/skills/SKILL.md", "/opt/skills/SKILL.md"},
	}
	for name, tc := range cases {
		if got := SkillPath(tc.skill, layout); got != tc.want {
			t.Errorf("%s: SkillPath(%q) = %q, want %q", name, tc.skill, got, tc.want)
		}
	}
}

// Each readiness key comes from the [[repo]] block when it sets it, else from
// the [[pool]]; the timeout defaults to five minutes.
func TestReadinessForMergesRepoOverPoolKeyByKey(t *testing.T) {
	pool := Pool{Repo: "example/big", MainClone: "/p/big", SlotName: "r{n}", SlotPath: "/p/big.r{n}", Max: 1,
		Prepare: []string{"pool-prepare"}, Ready: []string{"pool-ready"}, ReadyTimeout: Duration{2 * time.Minute}}
	cases := map[string]struct {
		pools []Pool
		repos []Repo
		repo  string
		want  Readiness
	}{
		"pool only": {
			pools: []Pool{pool}, repo: "example/big",
			want: Readiness{Prepare: []string{"pool-prepare"}, Ready: []string{"pool-ready"}, Timeout: 2 * time.Minute},
		},
		"repo overrides prepare only": {
			pools: []Pool{pool}, repos: []Repo{{Repo: "example/big", Prepare: []string{"repo-prepare"}}}, repo: "example/big",
			want: Readiness{Prepare: []string{"repo-prepare"}, Ready: []string{"pool-ready"}, Timeout: 2 * time.Minute},
		},
		"repo overrides the timeout only": {
			pools: []Pool{pool}, repos: []Repo{{Repo: "example/big", ReadyTimeout: Duration{30 * time.Second}}}, repo: "example/big",
			want: Readiness{Prepare: []string{"pool-prepare"}, Ready: []string{"pool-ready"}, Timeout: 30 * time.Second},
		},
		"repo names fold case": {
			pools: []Pool{pool}, repos: []Repo{{Repo: "Example/Big", Ready: []string{"repo-ready"}}}, repo: "EXAMPLE/big",
			want: Readiness{Prepare: []string{"pool-prepare"}, Ready: []string{"repo-ready"}, Timeout: 2 * time.Minute},
		},
		"repo without a pool": {
			repos: []Repo{{Repo: "example/app", Prepare: []string{"bin/prep"}, Ready: []string{"bin/up"}, ReadyTimeout: Duration{time.Minute}}}, repo: "example/app",
			want: Readiness{Prepare: []string{"bin/prep"}, Ready: []string{"bin/up"}, Timeout: time.Minute},
		},
		"repo without a pool or a timeout": {
			repos: []Repo{{Repo: "example/app", Ready: []string{"bin/up"}}}, repo: "example/app",
			want: Readiness{Ready: []string{"bin/up"}, Timeout: DefaultReadyTimeout},
		},
		"pool without a timeout": {
			pools: []Pool{{Repo: "example/big", Prepare: []string{"pool-prepare"}}}, repo: "example/big",
			want: Readiness{Prepare: []string{"pool-prepare"}, Timeout: DefaultReadyTimeout},
		},
		"neither": {
			repos: []Repo{{Repo: "example/other"}}, repo: "example/app",
			want: Readiness{Timeout: DefaultReadyTimeout},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Pools, cfg.Repos = tc.pools, tc.repos
			got := cfg.ReadinessFor(tc.repo)
			if !slices.Equal(got.Prepare, tc.want.Prepare) || !slices.Equal(got.Ready, tc.want.Ready) || got.Timeout != tc.want.Timeout {
				t.Fatalf("ReadinessFor(%s) = %+v, want %+v", tc.repo, got, tc.want)
			}
		})
	}
	if DefaultReadyTimeout != 5*time.Minute {
		t.Fatalf("DefaultReadyTimeout = %s, want 5m", DefaultReadyTimeout)
	}
}

// Approvals are dismissed on new commits unless the [[repo]] block, else the
// covering [[watch]], keeps them.
func TestKeepApprovalsRepoBlockOverridesWatch(t *testing.T) {
	cases := map[string]struct {
		repo  string
		block *Repo
		want  bool
	}{
		"default is false":                {repo: "example/app", want: false},
		"watch true":                      {repo: "talkable/app", want: true},
		"repo block without the key":      {repo: "talkable/app", block: &Repo{Repo: "talkable/app"}, want: true},
		"repo false overrides watch true": {repo: "talkable/app", block: &Repo{Repo: "talkable/app", KeepApprovals: new(false)}, want: false},
		"repo true":                       {repo: "example/app", block: &Repo{Repo: "example/app", KeepApprovals: new(true)}, want: true},
		"repo block names fold case":      {repo: "Example/App", block: &Repo{Repo: "example/app", KeepApprovals: new(true)}, want: true},
		"uncovered repository":            {repo: "other/app", want: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Watches = []Watch{
				{Owner: "example", Include: []string{"*"}},
				{Owner: "talkable", Include: []string{"*"}, KeepApprovals: true},
			}
			if tc.block != nil {
				cfg.Repos = []Repo{*tc.block}
			}
			if got := cfg.KeepApprovals(tc.repo); got != tc.want {
				t.Fatalf("KeepApprovals(%s) = %v, want %v", tc.repo, got, tc.want)
			}
		})
	}
}

// prepare, ready, ready_timeout and keep_approvals load from TOML; a [[repo]]
// for a pooled repository may set them.
func TestLoadReadinessAndKeepApprovalsKeys(t *testing.T) {
	cfg, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": `
[[identity]]
name = "z"
kind = "gh"
login = "z"
[[watch]]
owner = "example"
include = ["*"]
identity = "z"
keep_approvals = true
[[pool]]
repo = "example/big"
main_clone = "/p/big"
slot_name = "r{n}"
slot_path = "/p/big.r{n}"
max = 1
prepare = ["bin/rails db:test:prepare"]
ready = ["bin/rails runner 'exit 0'"]
ready_timeout = "2m"
[[repo]]
repo = "example/big"
prepare = ["bin/prep --fast"]
keep_approvals = false
[[repo]]
repo = "example/app"
ready = ["curl -fsS localhost:3000"]
ready_timeout = "90s"
`})
	if err != nil {
		t.Fatal(err)
	}
	big := cfg.ReadinessFor("example/big")
	if !slices.Equal(big.Prepare, []string{"bin/prep --fast"}) || !slices.Equal(big.Ready, []string{"bin/rails runner 'exit 0'"}) || big.Timeout != 2*time.Minute {
		t.Fatalf("example/big = %+v", big)
	}
	app := cfg.ReadinessFor("example/app")
	if len(app.Prepare) != 0 || !slices.Equal(app.Ready, []string{"curl -fsS localhost:3000"}) || app.Timeout != 90*time.Second {
		t.Fatalf("example/app = %+v", app)
	}
	if cfg.KeepApprovals("example/big") {
		t.Error("example/big: the repo's keep_approvals = false must override the watch's true")
	}
	if !cfg.KeepApprovals("example/app") {
		t.Error("example/app: the watch's keep_approvals = true must apply")
	}
}

func TestValidateReadinessKeys(t *testing.T) {
	valid := func() *Config {
		cfg := validPipelineConfig()
		cfg.Watches = []Watch{{Owner: "example", Include: []string{"*"}, Identity: "z", PollIdentity: "z"}}
		cfg.Pools = []Pool{{Repo: "example/big", MainClone: "/p/big", SlotName: "r{n}", SlotPath: "/p/big.r{n}", Max: 1}}
		return cfg
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		edit func(*Config)
		want []string
	}{
		"pool empty prepare": {func(c *Config) { c.Pools[0].Prepare = []string{""} },
			[]string{"pool example/big", "empty prepare command"}},
		"pool blank ready": {func(c *Config) { c.Pools[0].Ready = []string{"ok", "  "} },
			[]string{"pool example/big", "empty ready command"}},
		"pool multi-line prepare": {func(c *Config) { c.Pools[0].Prepare = []string{"a\nb"} },
			[]string{"pool example/big", "prepare command", "spans lines"}},
		"pool negative timeout": {func(c *Config) { c.Pools[0].ReadyTimeout = Duration{-time.Minute} },
			[]string{"pool example/big", "ready_timeout must not be negative"}},
		"repo multi-line ready": {func(c *Config) { c.Repos = []Repo{{Repo: "example/app", Ready: []string{"a\nb"}}} },
			[]string{"repo example/app", "ready command", "spans lines"}},
		"repo carriage return": {func(c *Config) { c.Repos = []Repo{{Repo: "example/app", Prepare: []string{"a\rb"}}} },
			[]string{"repo example/app", "spans lines"}},
		"repo empty prepare": {func(c *Config) { c.Repos = []Repo{{Repo: "example/app", Prepare: []string{" "}}} },
			[]string{"repo example/app", "empty prepare command"}},
		"repo negative timeout": {func(c *Config) { c.Repos = []Repo{{Repo: "example/app", ReadyTimeout: Duration{-time.Second}}} },
			[]string{"repo example/app", "ready_timeout must not be negative"}},
		"pooled repo is checked too": {func(c *Config) { c.Repos = []Repo{{Repo: "example/big", Ready: []string{"a\nb"}}} },
			[]string{"repo example/big", "spans lines"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			tc.edit(cfg)
			wantError(t, cfg.Validate(), tc.want...)
		})
	}

	t.Run("pooled repo may set readiness and keep_approvals", func(t *testing.T) {
		cfg := valid()
		cfg.Repos = []Repo{{Repo: "example/big", Prepare: []string{"bin/prep"}, Ready: []string{"bin/up"},
			ReadyTimeout: Duration{time.Minute}, KeepApprovals: new(true)}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("a [[repo]] for a pooled repository with readiness keys: %v", err)
		}
	})
	t.Run("pooled repo still refuses worktree keys", func(t *testing.T) {
		cfg := valid()
		cfg.Repos = []Repo{{Repo: "example/big", Setup: []string{"bin/setup"}, Prepare: []string{"bin/prep"}}}
		wantError(t, cfg.Validate(), "repo example/big", "has a [[pool]]")
	})
	t.Run("negative ready_timeout from TOML", func(t *testing.T) {
		_, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": snapTOML + `
[[repo]]
repo = "example/app"
ready_timeout = "-1m"
`})
		wantError(t, err, "repo example/app", "ready_timeout must not be negative")
	})
}

// required_checks loads from TOML for a pooled and a per-PR repository;
// Config.RequiredChecks finds the block by name, case-insensitively.
func TestRequiredChecks(t *testing.T) {
	cfg, err := loadFiles(t, t.TempDir(), map[string]string{"config.toml": `
[[identity]]
name = "z"
kind = "gh"
login = "z"
[[watch]]
owner = "example"
include = ["*"]
identity = "z"
[[pool]]
repo = "example/big"
main_clone = "/p/big"
slot_name = "r{n}"
slot_path = "/p/big.r{n}"
max = 1
[[repo]]
repo = "example/big"
required_checks = ["completion", "rspec (*)", "workflow:CI"]
[[repo]]
repo = "example/app"
keep_approvals = true
`})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RequiredChecks("Example/Big"); !slices.Equal(got, []string{"completion", "rspec (*)", "workflow:CI"}) {
		t.Errorf("example/big = %q", got)
	}
	for _, repo := range []string{"example/app", "example/other", "talkable/big"} {
		if got := cfg.RequiredChecks(repo); got != nil {
			t.Errorf("%s = %q, want none", repo, got)
		}
	}

	valid := func() *Config {
		cfg := validPipelineConfig()
		cfg.Watches = []Watch{{Owner: "example", Include: []string{"*"}, Identity: "z", PollIdentity: "z"}}
		return cfg
	}
	cases := map[string]struct {
		checks []string
		want   []string
	}{
		"malformed glob":          {[]string{"completion", "rspec ["}, []string{"repo example/app", `required_checks pattern "rspec ["`, "syntax error in pattern"}},
		"malformed workflow glob": {[]string{"workflow:CI ["}, []string{"repo example/app", `required_checks pattern "workflow:CI ["`, "syntax error in pattern"}},
		"empty entry":             {[]string{" "}, []string{"repo example/app", "empty required_checks entry"}},
		"empty workflow":          {[]string{"workflow:"}, []string{"repo example/app", `empty required_checks entry "workflow:"`}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := valid()
			cfg.Repos = []Repo{{Repo: "example/app", RequiredChecks: tc.checks}}
			wantError(t, cfg.Validate(), tc.want...)
		})
	}
	cfg = valid()
	cfg.Repos = []Repo{{Repo: "example/app", RequiredChecks: []string{"completion", "rspec (?)", "lint-*", "workflow:CI", "workflow:Deploy *"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("well-formed globs: %v", err)
	}
}
