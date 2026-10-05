package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// bareContext is a Context over an empty temp home (no config, no registry).
func bareContext(t *testing.T) (*Context, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	t.Setenv("MAGNUM_CONFIG", "")
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	return &Context{Version: "1.2.3", Layout: paths.Layout{Home: t.TempDir()}, Stdout: out, Stderr: errb}, out, errb
}

func TestRootHelpGroupsEveryCommand(t *testing.T) {
	c, out, _ := bareContext(t)
	if code := execute(c, []string{"--help"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	help := out.String()
	titles := map[string]string{groupInspect: "Inspect:", groupAct: "Act:", groupDaemon: "Daemon:"}
	section := func(title string) string {
		i := strings.Index(help, "\n"+title+"\n")
		if i < 0 {
			t.Fatalf("help lacks group %q:\n%s", title, help)
		}
		s := help[i+len(title)+2:]
		return s[:strings.Index(s, "\n\n")]
	}
	seen := 0
	for _, cmd := range newRoot(c).Commands() {
		name := cmd.Name()
		if cmd.Hidden || name == "help" || name == "completion" {
			continue
		}
		title, ok := titles[cmd.GroupID]
		if !ok {
			t.Errorf("command %q has no help group (GroupID %q)", name, cmd.GroupID)
			continue
		}
		if sec := section(title); !strings.Contains(sec, "\n  "+name+" ") && !strings.HasPrefix(sec, "  "+name+" ") {
			t.Errorf("%s does not list %q:\n%s", title, name, sec)
		}
		seen++
	}
	// Spot-check that the walk found the whole tree, including the late
	// additions the old hard-coded lists missed.
	if seen < 30 || !strings.Contains(section("Inspect:"), "  roles ") || !strings.Contains(section("Daemon:"), "  daemon-stop ") {
		t.Errorf("walked %d commands:\n%s", seen, help)
	}
	if !strings.Contains(help, "completion") || !strings.Contains(help, "--config") {
		t.Errorf("help lacks the completion command or --config:\n%s", help)
	}
}

func TestCommandHelpHasDescriptionAndFlags(t *testing.T) {
	c, out, _ := bareContext(t)
	if code := execute(c, []string{"status", "--help"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"detail card", "magnum status [<ref>|<slot>]", "--watch", "--sizes", "--config"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status --help lacks %q:\n%s", want, out)
		}
	}
	out.Reset()
	if code := execute(c, []string{"slots", "provision", "--help"}); code != 0 || !strings.Contains(out.String(), "--count") {
		t.Fatalf("slots provision --help: exit %d\n%s", code, out)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{nil, []string{"Usage:", "Inspect:"}},
		{[]string{"bogus"}, []string{`magnum: unknown command "bogus"`}},
		{[]string{"status", "--bogus"}, []string{"magnum status: unknown flag: --bogus", "usage: magnum status [<ref>|<slot>]"}},
		{[]string{"logs", "-n", "many"}, []string{"magnum logs: invalid argument"}},
		{[]string{"slots", "repair", "--nope"}, []string{"magnum slots repair: unknown flag: --nope"}},
	} {
		c, _, errb := bareContext(t)
		if code := execute(c, tc.args); code != 2 {
			t.Errorf("%v: exit %d, want 2 (stderr %s)", tc.args, code, errb)
		}
		for _, w := range tc.want {
			if !strings.Contains(errb.String(), w) {
				t.Errorf("%v: stderr lacks %q:\n%s", tc.args, w, errb)
			}
		}
	}
}

func TestVersionAndGlobalConfigFlag(t *testing.T) {
	c, out, _ := bareContext(t)
	if code := execute(c, []string{"version"}); code != 0 || out.String() != "magnum 1.2.3\n" {
		t.Fatalf("version: exit %d %q", code, out)
	}
	c, _, errb := bareContext(t)
	missing := filepath.Join(c.Layout.Home, "missing.toml")
	if code := execute(c, []string{"config", "--config", missing}); code != 1 || !strings.Contains(errb.String(), "missing.toml") {
		t.Fatalf("config --config: exit %d %s", code, errb)
	}
	c, _, _ = bareContext(t)
	if execute(c, []string{"--config", missing, "version"}); c.cfgPath != missing {
		t.Fatalf("--config before the command: cfgPath %q", c.cfgPath)
	}
}

func TestFlagsAnywhereAmongArguments(t *testing.T) {
	c, _, _ := bareContext(t)
	cmd, _, err := newRoot(c).Find([]string{"logs"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags([]string{"11920", "--json", "-n", "3", "extra", "--", "--literal"}); err != nil {
		t.Fatal(err)
	}
	if j, _ := cmd.Flags().GetBool("json"); !j {
		t.Error("--json after a positional was not parsed")
	}
	if n, _ := cmd.Flags().GetInt("lines"); n != 3 {
		t.Errorf("-n = %d", n)
	}
	if got := strings.Join(cmd.Flags().Args(), "|"); got != "11920|extra|--literal" {
		t.Errorf("positional = %q", got)
	}
}

// complete runs `magnum __complete args...` and returns the candidates
// (without the trailing directive line).
func complete(t *testing.T, c *Context, args ...string) []string {
	t.Helper()
	out := &bytes.Buffer{}
	c.Stdout, c.Stderr = out, &bytes.Buffer{}
	if code := execute(c, append([]string{"__complete"}, args...)); code != 0 {
		t.Fatalf("__complete %v: exit %d", args, code)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l != "" && !strings.HasPrefix(l, ":") {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestCompletionFromRegistryAndConfig(t *testing.T) {
	f := newInspFixture(t)
	st := f.store()
	_, held := inspSeedPR(t, st, "talkable/talkable", 11920, store.PRReviewed, nil)
	inspSeedPR(t, st, "zhuravel/app", 3, store.PRQueued, nil)
	inspSeedPR(t, st, "talkable/talkable", 11000, store.PRReleased, func(u *store.PRUpdate) { u.Set("gh_state", store.GHMerged) })
	inspSeedSlot(t, st, f.Home, "review1", store.SlotHeld, &held.ID)
	inspSeedSlot(t, st, f.Home, "review2", store.SlotFree, nil)
	inspSeedSlot(t, st, f.Home, "review3", store.SlotRemoved, nil)
	st.Close()

	prs := complete(t, f.Ctx, "open", "")
	if strings.Join(prs, "\n") != "talkable/talkable#11920\tPR xxx\nzhuravel/app#3\tPR xxx" &&
		strings.Join(prs, "\n") != "zhuravel/app#3\tPR xxx\ntalkable/talkable#11920\tPR xxx" {
		t.Errorf("open completions = %q", prs)
	}
	for _, p := range prs {
		ref, _, _ := strings.Cut(p, "\t")
		if _, _, _, err := github.ParseRef(ref, ""); err != nil {
			t.Errorf("candidate %q does not parse: %v", ref, err)
		}
	}
	// A number gets the bare form of default-repo PRs only.
	if got := complete(t, f.Ctx, "review", "119"); !strings.Contains(strings.Join(got, "\n"), "11920\tPR xxx") || strings.Contains(strings.Join(got, "\n"), "\n3\t") {
		t.Errorf("numeric completions = %q", got)
	}
	if got := complete(t, f.Ctx, "open", "talkable#5", ""); len(got) != 0 {
		t.Errorf("second argument completed %q", got)
	}
	slots := strings.Join(complete(t, f.Ctx, "slots", "repair", ""), "\n")
	if !strings.Contains(slots, "review1\tslot, held, talkable/talkable#11920") || !strings.Contains(slots, "review2\tslot, free") || strings.Contains(slots, "review3") {
		t.Errorf("slot completions = %q", slots)
	}
	where := strings.Join(complete(t, f.Ctx, "where", ""), "\n")
	if !strings.Contains(where, "talkable/talkable#11920") || !strings.Contains(where, "review2") {
		t.Errorf("where completions = %q", where)
	}
	if got := strings.Join(complete(t, f.Ctx, "mute", ""), "\n"); strings.Contains(got, "review2") {
		t.Errorf("mute takes PRs only: %q", got)
	}
	if got := strings.Join(complete(t, f.Ctx, "cleanup", "--slot", ""), "\n"); !strings.Contains(got, "review1") {
		t.Errorf("cleanup --slot completions = %q", got)
	}
	if got := strings.Join(complete(t, f.Ctx, "review", "5", "--as", ""), "\n"); got != "zhuravel\tgh zhuravel\ntalkable-app\tapp talkable[bot]" {
		t.Errorf("--as completions = %q", got)
	}
	if got := strings.Join(complete(t, f.Ctx, "identities", "check", "--name", ""), "\n"); !strings.Contains(got, "talkable-app") {
		t.Errorf("identities --name completions = %q", got)
	}
	if got := strings.Join(complete(t, f.Ctx, "slots", "provision", "--repo", ""), "\n"); !strings.HasPrefix(got, "talkable/talkable\t") {
		t.Errorf("--repo completions = %q", got)
	}
	var roles []string
	for _, c := range complete(t, f.Ctx, "watch", "5", "--role", "") {
		name, _, _ := strings.Cut(c, "\t")
		roles = append(roles, name)
	}
	if got := strings.Join(roles, ","); got != "codex-judge,claude-review,codex-review,claude-simplify,judge,claude,codex,codex_review,simplify" {
		t.Errorf("--role completions = %q", got)
	}
	roles = roles[:0]
	for _, c := range complete(t, f.Ctx, "review", "5", "--role", "") {
		name, _, _ := strings.Cut(c, "\t")
		roles = append(roles, name)
	}
	if got := strings.Join(roles, ","); got != "claude-simplify,codex-judge,claude-review,codex-review" {
		t.Errorf("review --role completions = %q", got)
	}
	if got := complete(t, f.Ctx, "kick", ""); len(got) != 0 { // its old targets are accepted, not offered
		t.Errorf("kick completions = %q", got)
	}
	if got := complete(t, f.Ctx, "pause", "--reason", ""); len(got) != 0 {
		t.Errorf("free-text flag completed %q", got)
	}
}

func TestCompletionFailsSoft(t *testing.T) {
	c, _, _ := bareContext(t) // no config.toml, no registry
	if got := complete(t, c, "open", ""); len(got) != 0 {
		t.Errorf("completions without a registry: %q", got)
	}
	if got := complete(t, c, "review", "5", "--as", ""); len(got) != 0 {
		t.Errorf("completions without a config: %q", got)
	}
	if _, err := os.Stat(c.Layout.DB()); !os.IsNotExist(err) {
		t.Errorf("completion created the registry: %v", err)
	}
}

func TestCompletionScript(t *testing.T) {
	c, out, _ := bareContext(t)
	if code := execute(c, []string{"completion", "zsh"}); code != 0 || !strings.Contains(out.String(), "#compdef magnum") {
		t.Fatalf("completion zsh: exit %d\n%.200s", code, out)
	}
}

// nextPendingRequest is the oldest pending request, or store.ErrNotFound.
func nextPendingRequest(ctx context.Context, st *store.Store) (store.Request, error) {
	q, err := st.PendingRequests(ctx, 1)
	if err != nil {
		return store.Request{}, err
	}
	if len(q) == 0 {
		return store.Request{}, store.ErrNotFound
	}
	return q[0], nil
}
