package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
)

// holdStatusTasks seeds the factory's registry with the tasks the hold
// tests read, in order: an active task at its local review with no wait of
// its own, an active task that waits for a copy, and one that waits on you.
func holdStatusTasks(t *testing.T, dt *daemonGroupTest) {
	t.Helper()
	l := dt.ctx.Layout
	registrytest.Seed(t, l.FactoryDB())
	reg, err := registry.Open(l.FactoryDB(), registry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	ctx := context.Background()
	for _, task := range []registry.Task{
		{JiraKey: "PR-12340", Kind: "bug", Repo: "example/shop", Title: "Points vanish", State: registry.TaskActive, Stage: "local_review"},
		{JiraKey: "PR-12341", Kind: "bug", Repo: "example/shop", Title: "Coupon rounding", State: registry.TaskActive, Stage: "coding",
			WaitsWho: "copy", WaitsWhat: "preparing factory3"},
		{JiraKey: "PR-12342", Kind: "bug", Repo: "example/shop", Title: "Locale", State: registry.TaskNeedsYou, Stage: "questions",
			WaitsWho: "you", WaitsWhat: "2 questions before coding"},
	} {
		if _, err := reg.CreateTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
}

// writeHolds writes the holds the running factory daemon last saved.
func writeHolds(t *testing.T, dt *daemonGroupTest, hs ...factory.Hold) {
	t.Helper()
	b, err := json.Marshal(hs)
	if err != nil {
		t.Fatal(err)
	}
	p := dt.ctx.Layout.FactoryHolds()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteFileAtomic(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A hold that stops every stage stops each active task whose registry row
// names no wait, and the task's row said "-" for it: `magnum factory status`
// printed "held: infra: MySQL does not answer (since 17:03)" at the top and
// "-" under WAITS ON for a task at its local review, so nothing said what
// the factory was doing. The row and --json name the hold now; a task with a
// wait of its own keeps it.
func TestAnActiveTaskWithNoWaitShowsTheDaemonsHold(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	holdStatusTasks(t, dt)
	since := time.Now().Add(-25 * time.Minute)
	writeHolds(t, dt, factory.Hold{Kind: factory.HoldInfra, Reason: "MySQL does not answer", Since: since})
	words := "MySQL does not answer (since " + inspClock(time.Now(), since) + ")"

	if code := dt.run("factory", "status"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	out := dt.stdout.String()
	actContains(t, out, "held: infra: "+words)
	rows := map[string]string{}
	for line := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(line); len(f) > 1 && strings.HasPrefix(f[1], "PR-") {
			rows[f[1]] = strings.TrimSpace(line)
		}
	}
	if got := rows["PR-12340"]; !strings.HasSuffix(got, "held: "+words) {
		t.Errorf("the row of the task with no wait: %q, want it to end with %q", got, "held: "+words)
	}
	if got := rows["PR-12341"]; !strings.HasSuffix(got, "copy: preparing factory3") {
		t.Errorf("the row of the task that waits for a copy: %q, want its own wait", got)
	}
	if got := rows["PR-12342"]; !strings.HasSuffix(got, "you: 2 questions before coding") {
		t.Errorf("the row of the task that waits on you: %q, want its own wait", got)
	}

	dt.stdout.Reset()
	if code := dt.run("factory", "status", "--json"); code != 0 {
		t.Fatalf("--json: exit %d, stderr %s", code, dt.stderr)
	}
	var got struct {
		Tasks []struct {
			Key       string `json:"key"`
			WaitsWho  string `json:"waits_who"`
			WaitsWhat string `json:"waits_what"`
			HeldBy    *struct {
				Kind   string    `json:"kind"`
				Reason string    `json:"reason"`
				Since  time.Time `json:"since"`
			} `json:"held_by"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(dt.stdout.Bytes(), &got); err != nil {
		t.Fatalf("%v: %s", err, dt.stdout)
	}
	if len(got.Tasks) != 3 {
		t.Fatalf("tasks %+v, want 3", got.Tasks)
	}
	first := got.Tasks[0]
	if first.WaitsWho != "held" || first.WaitsWhat != words || first.HeldBy == nil || first.HeldBy.Kind != "infra" ||
		first.HeldBy.Reason != "MySQL does not answer" || !first.HeldBy.Since.Equal(since) {
		t.Errorf("the task with no wait: %+v (held_by %+v), want waits_who held, %q and the hold named", first, first.HeldBy, words)
	}
	for _, own := range got.Tasks[1:] {
		if own.HeldBy != nil || own.WaitsWho == "held" {
			t.Errorf("%s names the hold %+v with a wait of its own: %+v", own.Key, own.HeldBy, own)
		}
	}
}

// The drains record no wait in the tasks they hold back (a launch, a claim,
// a round or a check the drain stops sets none), so an active task with no
// wait of its own showed "-" while `magnum factory install --drain` or the
// drain for a reboot held it, and a reboot's drain can last long. Its row
// and --json name the drain now, as they name a hold that stops every stage;
// a task with a wait of its own keeps it.
func TestAnActiveTaskWithNoWaitShowsTheDrains(t *testing.T) {
	for _, h := range []factory.Hold{
		{Kind: factory.HoldDrain, Reason: "`magnum factory install --drain` (pid 4300, since 14:02:11): nothing new starts until the restart"},
		{Kind: factory.HoldReboot, Reason: "draining for a reboot since Nov 13 14:02: nothing new starts; `magnum resume` ends it"},
	} {
		t.Run(h.Kind, func(t *testing.T) {
			dt, _ := factoryInstallTest(t)
			holdStatusTasks(t, dt)
			h.Since = time.Now().Add(-40 * time.Minute)
			writeHolds(t, dt, h)
			words := h.Reason // a drain's reason carries its start: no second "(since …)"

			if code := dt.run("factory", "status"); code != 0 {
				t.Fatalf("exit %d, stderr %s", code, dt.stderr)
			}
			rows := map[string]string{}
			for line := range strings.SplitSeq(dt.stdout.String(), "\n") {
				if f := strings.Fields(line); len(f) > 1 && strings.HasPrefix(f[1], "PR-") {
					rows[f[1]] = strings.TrimSpace(line)
				}
			}
			if got := rows["PR-12340"]; !strings.HasSuffix(got, "held: "+words) {
				t.Errorf("the row of the task with no wait: %q, want it to end with %q", got, "held: "+words)
			}
			if got := rows["PR-12341"]; !strings.HasSuffix(got, "copy: preparing factory3") {
				t.Errorf("the row of the task that waits for a copy: %q, want its own wait", got)
			}
			if got := rows["PR-12342"]; !strings.HasSuffix(got, "you: 2 questions before coding") {
				t.Errorf("the row of the task that waits on you: %q, want its own wait", got)
			}

			dt.stdout.Reset()
			if code := dt.run("factory", "status", "--json"); code != 0 {
				t.Fatalf("--json: exit %d, stderr %s", code, dt.stderr)
			}
			var got struct {
				Tasks []struct {
					Key       string `json:"key"`
					WaitsWho  string `json:"waits_who"`
					WaitsWhat string `json:"waits_what"`
					HeldBy    *struct {
						Kind   string `json:"kind"`
						Reason string `json:"reason"`
					} `json:"held_by"`
				} `json:"tasks"`
			}
			if err := json.Unmarshal(dt.stdout.Bytes(), &got); err != nil {
				t.Fatalf("%v: %s", err, dt.stdout)
			}
			if len(got.Tasks) != 3 {
				t.Fatalf("tasks %+v, want 3", got.Tasks)
			}
			if first := got.Tasks[0]; first.WaitsWho != "held" || first.WaitsWhat != words || first.HeldBy == nil ||
				first.HeldBy.Kind != h.Kind || first.HeldBy.Reason != h.Reason {
				t.Errorf("the task with no wait: %+v (held_by %+v), want waits_who held, %q and the drain named", first, first.HeldBy, words)
			}
			for _, own := range got.Tasks[1:] {
				if own.HeldBy != nil || own.WaitsWho == "held" {
					t.Errorf("%s names the drain %+v with a wait of its own: %+v", own.Key, own.HeldBy, own)
				}
			}
		})
	}
}

// The holds of a daemon that is gone are not in force: no row names one.
func TestNoTaskNamesTheHoldOfAFactoryDaemonThatIsGone(t *testing.T) {
	dt, _ := factoryInstallTest(t)
	holdStatusTasks(t, dt)
	writeHolds(t, dt, factory.Hold{Kind: factory.HoldInfra, Reason: "MySQL does not answer", Since: time.Now().Add(-time.Hour)})
	daemonSys.FactoryPID = func(paths.Layout) (int, error) { return 0, nil }

	if code := dt.run("factory", "status"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, dt.stderr)
	}
	if out := dt.stdout.String(); strings.Contains(out, "held:") || strings.Contains(out, "MySQL") {
		t.Fatalf("status of a stopped daemon names its last hold:\n%s", out)
	}
}
