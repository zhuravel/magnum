package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// newFactoryStartTest is a daemon group test with one factory repository
// and a factory registry; answer, when set, is the daemon: it completes the
// pending start request on the CLI's first wait.
func newFactoryStartTest(t *testing.T, pid int, answer func(registry.Request) (state, result string)) *daemonGroupTest {
	t.Helper()
	dt := newDaemonGroupTest(t)
	cfg := *dt.ctx.Config
	cfg.Factory.Repos = []config.FactoryRepo{{Repo: "talkable/talkable", Base: "master", MainClone: t.TempDir(), Copies: t.TempDir() + "/talkable.factory{n}"}}
	dt.ctx.Config = &cfg
	registrytest.Seed(t, dt.ctx.Layout.FactoryDB())
	daemonSys.FactoryKick = func(paths.Layout) (int, error) { return pid, nil }
	daemonSys.Sleep = func(ctx context.Context, _ time.Duration) error {
		if answer == nil {
			return nil
		}
		reg, err := registry.Open(dt.ctx.Layout.FactoryDB(), registry.Options{})
		if err != nil {
			t.Error(err)
			return err
		}
		defer reg.Close()
		reqs, err := reg.PendingRequests(ctx, 0)
		if err != nil || len(reqs) == 0 {
			return err
		}
		state, result := answer(reqs[0])
		return reg.CompleteRequest(ctx, reqs[0].ID, state, result)
	}
	return dt
}

func startRequests(t *testing.T, l paths.Layout) []registry.Request {
	t.Helper()
	reg, err := registry.Open(l.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	var out []registry.Request
	for id := int64(1); ; id++ {
		q, err := reg.RequestByID(context.Background(), id)
		if err != nil {
			return out
		}
		out = append(out, q)
	}
}

// `magnum factory start` queues a start request with your words, the kind
// and the repository, wakes the factory daemon and prints its answer.
func TestFactoryStartQueuesARequestAndPrintsTheDaemonsAnswer(t *testing.T) {
	storetest.Serial(t)
	var payload factory.StartPayload
	dt := newFactoryStartTest(t, 250, func(q registry.Request) (string, string) {
		if q.Kind != factory.ReqStart {
			t.Errorf("request kind %q", q.Kind)
		}
		if err := json.Unmarshal(q.Payload, &payload); err != nil {
			t.Error(err)
		}
		return registry.RequestDone, "started PR-12345 as task 1: bug in talkable/talkable on zhuravel-PR-12345-credit-expiry"
	})
	code := dt.run("factory", "start", "PR-12345", "only the refund path", "--kind", "bug", "--repo", "talkable/talkable")
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if payload.Text != "PR-12345 only the refund path" || payload.Kind != "bug" || payload.Repo != "talkable/talkable" {
		t.Fatalf("payload %+v", payload)
	}
	if out := dt.stdout.String(); out != "started PR-12345 as task 1: bug in talkable/talkable on zhuravel-PR-12345-credit-expiry\n" {
		t.Fatalf("stdout %q", out)
	}

	// A start the daemon refuses prints its cause on stderr and exits 1.
	dt = newFactoryStartTest(t, 250, func(registry.Request) (string, string) {
		return registry.RequestFailed, "PR-12399 is not in Jira"
	})
	if code := dt.run("factory", "start", "PR-12399"); code != 1 || !strings.Contains(dt.stderr.String(), "PR-12399 is not in Jira") {
		t.Fatalf("exit %d, stderr %q", code, dt.stderr)
	}
}

// --words-only starts a task from your words only, as the screen's "Start
// from your words only?" does: a key at the start that Jira does not know
// is words, and no key of them is read from Jira.
func TestFactoryStartFromYourWordsOnly(t *testing.T) {
	storetest.Serial(t)
	var payload factory.StartPayload
	dt := newFactoryStartTest(t, 250, func(q registry.Request) (string, string) {
		if err := json.Unmarshal(q.Payload, &payload); err != nil {
			t.Error(err)
		}
		return registry.RequestDone, `started "PR-12399 the reminder" as task 2: chore in talkable/talkable on zhuravel-pr-12399-reminder`
	})
	if code := dt.run("factory", "start", "--words-only", "PR-12399", "the", "reminder"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if payload.Text != "PR-12399 the reminder" || !payload.WordsOnly {
		t.Fatalf("payload %+v, want your words only", payload)
	}
}

// Without a factory daemon the request stays queued, and the command says
// how to start one and how long the request waits.
func TestFactoryStartWithoutADaemonSaysItIsQueued(t *testing.T) {
	storetest.Serial(t)
	dt := newFactoryStartTest(t, 0, nil)
	if code := dt.run("factory", "start", "Rename", "the", "referral", "helper"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	for _, want := range []string{"request 1", "no factory daemon runs", "magnum factory install", "30 minutes"} {
		if !strings.Contains(dt.stderr.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, dt.stderr)
		}
	}
	reqs := startRequests(t, dt.ctx.Layout)
	if len(reqs) != 1 || reqs[0].State != registry.RequestPending {
		t.Fatalf("requests %+v, want one pending start", reqs)
	}
}

// A kind or a repository the config does not have, and no text at all,
// are usage errors: nothing is queued, no registry is created.
func TestFactoryStartRefusesAnUnknownKindOrRepository(t *testing.T) {
	storetest.Serial(t)
	dt := newDaemonGroupTest(t)
	for _, tc := range []struct {
		args []string
		says string
	}{
		{[]string{"PR-12345", "--kind", "epic"}, "bug, chore, feature"},
		{[]string{"PR-12345", "--repo", "talkable/other"}, "no [[factory.repo]]"},
		{nil, "usage"},
		{[]string{"  "}, "usage"},
	} {
		dt.stderr.Reset()
		if code := dt.run("factory", append([]string{"start"}, tc.args...)...); code != 2 || !strings.Contains(dt.stderr.String(), tc.says) {
			t.Errorf("%q: exit %d, stderr %q; want 2 saying %q", tc.args, code, dt.stderr, tc.says)
		}
	}
	if _, err := os.Stat(dt.ctx.Layout.FactoryDB()); !os.IsNotExist(err) {
		t.Fatalf("a refused start created the registry: %v", err)
	}
}

// The daemon builds with its Jira client when [jira] is on, and without one
// when it is off.
func TestTheFactoryDaemonBuildsWithItsJira(t *testing.T) {
	cfg := config.Defaults()
	c := &Context{Version: "test", Layout: paths.Layout{Home: t.TempDir()}, Config: cfg, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	storetest.Seed(t, c.Layout.FactoryAgentsDB())
	registrytest.Seed(t, c.Layout.FactoryDB())
	d, err := buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	if d.Jira != nil {
		t.Fatalf("Jira %+v with [jira] off", d.Jira)
	}
	d.Close()

	cfg.Jira = config.Jira{URL: "https://example.atlassian.net", User: "alice@example.com", TokenFile: "/run/jira.token"}
	d, err = buildFactoryDaemon(c, slog.LevelInfo)
	if err != nil {
		t.Fatalf("the factory daemon does not build: %v", err)
	}
	defer d.Close()
	if d.Jira == nil || d.Jira.URL != "https://example.atlassian.net" || d.Jira.TokenFile != "/run/jira.token" {
		t.Fatalf("Jira %+v, want the [jira] site's client", d.Jira)
	}
}
