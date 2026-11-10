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

// The daemon's Actions calls as a fixer App (ci_dispatch_as) need Actions
// write, which the pane's token never has: ActionsEnv mints a token of
// the daemon's own, narrowed as the App's are with actions = "write"
// added, hands it as GH_TOKEN over the App's ConfigDir and writes it
// nowhere; the App's own refresh still mints without actions, so the
// pane's hosts.yml never holds it. Its release revokes it once.
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
	if env["GH_TOKEN"] != "ghs_test2" || env["GH_CONFIG_DIR"] != app.ConfigDir() || env["GH_HOST"] != "github.com" {
		t.Errorf("env = %v, want the Actions token as GH_TOKEN over the App's ConfigDir", redactedEnv(env))
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
// a push token does: the first refresh after its hold revokes it.
func TestAnActionsTokenLeftUnreleasedGoesAtTheNextRefreshAfterItsHold(t *testing.T) {
	app, f, clock := pushApp(t)
	if _, _, err := app.ActionsEnv(context.Background()); err != nil {
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
}
