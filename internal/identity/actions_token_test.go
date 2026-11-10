package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The daemon's Actions calls as a fixer App (ci_dispatch_as) need Actions
// write, which the pane's token never has: ActionsEnv mints a token of
// the daemon's own, narrowed as the App's are with actions = "write"
// added, hands it as GH_TOKEN over a gh config dir of the batch's own and
// writes it nowhere; the App's ConfigDir keeps the pane's token. Its
// release revokes it once.
func TestAnActionsTokenAddsActionsAndIsWrittenNowhere(t *testing.T) {
	app, f, _ := pushApp(t)
	if _, err := app.EnsureConfigDir(context.Background()); err != nil { // the pane's token: ghs_test1
		t.Fatal(err)
	}
	env, release, err := app.ActionsEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := f.lastMint()
	var asked mintBody
	if err := json.Unmarshal(body, &asked); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(asked.Repositories, []string{"talkable"}) || len(asked.Permissions) != 3 || asked.Permissions["actions"] != "write" ||
		asked.Permissions["contents"] != "write" || asked.Permissions["pull_requests"] != "write" {
		t.Errorf("the mint asked for %s, want talkable with contents, pull_requests and actions write", body)
	}
	if f.id.TokenPermissions["actions"] != "" {
		t.Error("the identity's own permissions gained actions")
	}
	if env["GH_TOKEN"] != "ghs_test2" || env["GH_CONFIG_DIR"] == "" || env["GH_CONFIG_DIR"] == app.ConfigDir() || env["GH_HOST"] != "github.com" {
		t.Errorf("env = %v, want the Actions token as GH_TOKEN over a gh config dir of the batch's own", redactedEnv(env))
	}
	wantWrittenNowhere(t, app, "ghs_test2")
	hosts, err := os.ReadFile(filepath.Join(app.ConfigDir(), "hosts.yml"))
	if err != nil || !strings.Contains(string(hosts), "ghs_test1") {
		t.Errorf("hosts.yml = %q, %v; want the pane's token", hosts, err)
	}
	release()
	release()
	if got := f.revokedTokens(); !slices.Equal(got, []string{"ghs_test2"}) {
		t.Errorf("revoked = %v, want the Actions token once", got)
	}
}

// While the App lacks Actions, GitHub refuses the mint (422): the error
// is ErrActionsNotGranted, not ErrWorkflowsNotGranted, and nothing is left
// on disk.
func TestAnActionsTokenSaysWhenTheAppLacksActions(t *testing.T) {
	app, f, _ := pushApp(t)
	f.mu.Lock()
	f.ungranted = "actions"
	f.mu.Unlock()
	_, _, err := app.ActionsEnv(context.Background())
	if !errors.Is(err, ErrActionsNotGranted) || errors.Is(err, ErrWorkflowsNotGranted) {
		t.Fatalf("err = %v, want ErrActionsNotGranted", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(app.ConfigDir())); len(entries) != 0 {
		t.Errorf("left on disk: %v", entries)
	}
}

// An Actions token nobody released (a panic past the batch's end) goes as
// a push token does: the first refresh after its hold revokes it, and
// removes the batch's gh config dir.
func TestAnActionsTokenLeftUnreleasedGoesAtTheNextRefreshAfterItsHold(t *testing.T) {
	app, f, clock := pushApp(t)
	env, _, err := app.ActionsEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(daemonPushHold + time.Second)
	if _, err := app.EnsureConfigDir(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitRefresh(t, app)
	if got := f.revokedTokens(); !slices.Equal(got, []string{"ghs_test1"}) {
		t.Errorf("revoked = %v, want the Actions token", got)
	}
	wantGone(t, env["GH_CONFIG_DIR"])
}

// The fixer pane's env names the App's ConfigDir, which it can write:
// `gh config set http_unix_socket <its socket>` there would make every gh
// that reads it send its GH_TOKEN to the pane, an Actions token too. So a
// batch's gh reads a gh config dir of its own, made fresh for it (0700,
// config.yml alone, as magnum writes it, and no token), one per batch,
// removed at its release.
func TestAnActionsTokenGoesOverAGhConfigDirOfTheBatchsOwn(t *testing.T) {
	app, _, _ := pushApp(t)
	if _, err := app.EnsureConfigDir(context.Background()); err != nil {
		t.Fatal(err)
	}
	planted := "http_unix_socket: /tmp/pane.sock\n"
	if err := os.WriteFile(filepath.Join(app.ConfigDir(), "config.yml"), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	env, release, err := app.ActionsEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	dir := env["GH_CONFIG_DIR"]
	if dir == "" || dir == app.ConfigDir() {
		t.Fatalf("GH_CONFIG_DIR = %q, want a dir of the batch's own", dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the batch's gh config dir: %v, %v; want a 0700 dir", fi, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "config.yml" {
		t.Fatalf("the batch's gh config dir holds %v, %v; want config.yml alone", entries, err)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, "config.yml"))
	if err != nil || string(cfg) != string(renderConfigYML(app.cfg.Name)) {
		t.Errorf("its config.yml = %q, %v; want magnum's", cfg, err)
	}
	other, releaseOther, err := app.ActionsEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if other["GH_CONFIG_DIR"] == dir {
		t.Errorf("two batches share the gh config dir %s", dir)
	}
	releaseOther()
	wantGone(t, other["GH_CONFIG_DIR"])
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the other batch's release removed this one's dir: %v", err)
	}
	release()
	wantGone(t, dir)
}

// The App's refresh after an Actions token rewrites the pane's hosts.yml
// with its own token, never the batch's: the panes never gain Actions.
func TestThePanesHostsNeverGainAnActionsToken(t *testing.T) {
	app, _, _ := pushApp(t)
	if _, err := app.EnsureConfigDir(context.Background()); err != nil {
		t.Fatal(err)
	}
	env, release, err := app.ActionsEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := app.EnsureConfigDir(context.Background()); err != nil {
		t.Fatal(err)
	}
	hosts, err := os.ReadFile(filepath.Join(app.ConfigDir(), "hosts.yml"))
	if err != nil || strings.Contains(string(hosts), env["GH_TOKEN"]) {
		t.Errorf("hosts.yml after a refresh = %v; want no Actions token in it", err)
	}
	wantWrittenNowhere(t, app, env["GH_TOKEN"])
}

// A batch whose context ended (the daemon stopping before the batch's
// deferred end) still revokes its token at its release, and removes its
// gh config dir.
func TestAnActionsTokenIsRevokedThoughTheBatchsContextEnded(t *testing.T) {
	app, f, _ := pushApp(t)
	ctx, cancel := context.WithCancel(context.Background())
	env, release, err := app.ActionsEnv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	release()
	if got := f.revokedTokens(); !slices.Equal(got, []string{env["GH_TOKEN"]}) {
		t.Errorf("revoked %d tokens, want the Actions token once", len(got))
	}
	wantGone(t, env["GH_CONFIG_DIR"])
}

// wantGone fails unless dir is set and no longer exists.
func wantGone(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); dir == "" || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the batch's gh config dir %q is still there (%v)", dir, err)
	}
}
