package identity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// pushApp is a fixer App's identity: tokens narrowed to its repositories,
// with contents and pull_requests write.
func pushApp(t *testing.T) (*App, *fakeGitHub, *fakeClock) {
	t.Helper()
	clock := newClock()
	f := newFakeGitHub(t, clock)
	f.id.TokenRepositories = []string{"talkable"}
	f.id.TokenPermissions = map[string]string{"contents": "write", "pull_requests": "write"}
	app, _ := newTestApp(t, f, clock)
	return app, f, clock
}

// The daemon's push of a merge that carries its base's workflow files
// needs Workflows write, which the pane's token never has: it mints a
// token of its own, narrowed as the App's are with workflows = "write"
// added, into a gh config dir of its own that no pane names (never the
// App's ConfigDir, whose files it leaves alone), and its release revokes
// the token and removes the dir.
func TestADaemonPushTokenAddsWorkflowsInADirNoPaneNames(t *testing.T) {
	app, f, _ := pushApp(t)
	env, release, err := app.DaemonPushEnv(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := f.lastMint()
	var asked mintBody
	if err := json.Unmarshal(body, &asked); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"contents": "write", "pull_requests": "write", "workflows": "write"}; !slices.Equal(asked.Repositories, []string{"talkable"}) ||
		len(asked.Permissions) != len(want) || asked.Permissions["workflows"] != "write" || asked.Permissions["contents"] != "write" {
		t.Errorf("the mint asked for %s, want talkable with %v", body, want)
	}
	if f.id.TokenPermissions["workflows"] != "" {
		t.Error("the identity's own permissions gained workflows")
	}
	dir := env["GH_CONFIG_DIR"]
	if dir == "" || dir == app.ConfigDir() || filepath.Dir(dir) != filepath.Dir(app.ConfigDir()) {
		t.Fatalf("GH_CONFIG_DIR = %q, want a dir of its own beside %s", dir, app.ConfigDir())
	}
	if env["GH_TOKEN"] != "" || env["GITHUB_TOKEN"] != "" || env["GH_HOST"] != "github.com" {
		t.Errorf("env = %v, want the App env's blanks and host", env)
	}
	if hosts, err := os.ReadFile(filepath.Join(dir, "hosts.yml")); err != nil || !strings.Contains(string(hosts), `oauth_token: "ghs_test1"`) {
		t.Errorf("hosts.yml = %q, %v", hosts, err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("the dir's mode = %v, %v; want 0700", st, err)
	}
	if _, err := os.Stat(filepath.Join(app.ConfigDir(), "hosts.yml")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the App's ConfigDir got a token: %v", err)
	}
	release()
	release()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dir is left after the release: %v", err)
	}
	if got := f.revokedTokens(); !slices.Equal(got, []string{"ghs_test1"}) {
		t.Errorf("revoked = %v, want the push's token once", got)
	}
}

// While the App lacks the Workflows permission, GitHub refuses to mint the
// token (422): the error says so (ErrWorkflowsNotGranted), nothing is left
// on disk, and a push without workflows still mints its token.
func TestADaemonPushTokenSaysWhenTheAppLacksWorkflows(t *testing.T) {
	app, f, _ := pushApp(t)
	f.mu.Lock()
	f.ungranted = "workflows"
	f.mu.Unlock()
	if _, _, err := app.DaemonPushEnv(context.Background(), true); !errors.Is(err, ErrWorkflowsNotGranted) {
		t.Fatalf("err = %v, want ErrWorkflowsNotGranted", err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(app.ConfigDir())); len(entries) != 0 {
		t.Errorf("left on disk: %v", entries)
	}
	env, release, err := app.DaemonPushEnv(context.Background(), false)
	if err != nil {
		t.Fatalf("a push without workflows: %v", err)
	}
	defer release()
	body, _ := f.lastMint()
	if strings.Contains(string(body), "workflows") || env["GH_CONFIG_DIR"] == "" {
		t.Errorf("the mint asked for %s, env %v; want no workflows", body, env)
	}
}

// A push whose caller cannot release its token (staging's PushEnv hands
// back the env alone) leaves it to the App: the next refresh after the
// push's hold revokes the token and removes its dir.
func TestADaemonPushTokenLeftUnreleasedGoesAtTheNextRefreshAfterItsHold(t *testing.T) {
	app, f, clock := pushApp(t)
	env, _, err := app.DaemonPushEnv(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	dir := env["GH_CONFIG_DIR"]
	if _, err := app.EnsureConfigDir(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the dir went before its hold: %v", err)
	}
	clock.Advance(daemonPushHold + time.Second)
	if _, err := app.EnsureConfigDir(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitRefresh(t, app)
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dir is left after its hold: %v", err)
	}
	if got := f.revokedTokens(); !slices.Equal(got, []string{"ghs_test1"}) {
		t.Errorf("revoked = %v, want the push's token", got)
	}
}
