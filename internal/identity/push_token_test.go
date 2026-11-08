package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
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
// added, and hands it in the env of that one push alone (GH_TOKEN, which
// gh answers git with before any hosts.yml): no file holds it, so no pane
// of the App, which runs as the same user, can read it from disk, and the
// App's ConfigDir keeps the pane's token. Its release revokes the token.
func TestADaemonPushTokenAddsWorkflowsAndIsWrittenNowhere(t *testing.T) {
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
	if env["GH_TOKEN"] != "ghs_test1" || env["GITHUB_TOKEN"] != "" || env["GH_HOST"] != "github.com" || env["GH_CONFIG_DIR"] != app.ConfigDir() {
		t.Errorf("env = %v, want the push's token as GH_TOKEN over the App's ConfigDir", redactedEnv(env))
	}
	wantWrittenNowhere(t, app, "ghs_test1")
	release()
	release()
	if got := f.revokedTokens(); !slices.Equal(got, []string{"ghs_test1"}) {
		t.Errorf("revoked = %v, want the push's token once", got)
	}
}

// redactedEnv is env with its token values replaced, for a failure message.
func redactedEnv(env map[string]string) map[string]string {
	out := maps.Clone(env)
	for k, v := range out {
		if strings.HasPrefix(v, "ghs_") {
			out[k] = "<token>"
		}
	}
	return out
}

// wantWrittenNowhere fails if any file under the App's gh config root
// holds token.
func wantWrittenNowhere(t *testing.T, app *App, token string) {
	t.Helper()
	root := filepath.Dir(app.ConfigDir())
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), token) {
			t.Errorf("%s holds the push's token", path)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
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
	if strings.Contains(string(body), "workflows") || env["GH_TOKEN"] == "" {
		t.Errorf("the mint asked for %s, env %v; want no workflows", body, redactedEnv(env))
	}
}

// A push whose caller cannot release its token (staging's PushEnv hands
// back the env alone) leaves it to the App: the first refresh after the
// push's hold, a few minutes, revokes the token.
func TestADaemonPushTokenLeftUnreleasedGoesAtTheNextRefreshAfterItsHold(t *testing.T) {
	app, f, clock := pushApp(t)
	if _, _, err := app.DaemonPushEnv(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if daemonPushHold > 5*time.Minute {
		t.Errorf("the hold is %s, want a few minutes at most", daemonPushHold)
	}
	if _, err := app.EnsureConfigDir(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.revokedTokens(); len(got) != 0 {
		t.Fatalf("revoked %v before the hold", got)
	}
	clock.Advance(daemonPushHold + time.Second)
	if _, err := app.EnsureConfigDir(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitRefresh(t, app)
	if got := f.revokedTokens(); !slices.Equal(got, []string{"ghs_test1"}) {
		t.Errorf("revoked = %v, want the push's token", got)
	}
	wantWrittenNowhere(t, app, "ghs_test1")
}
