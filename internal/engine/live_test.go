package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
)

// TestLiveDryRunOnce runs `magnum daemon --once --dry-run` against the real
// machine: GitHub, herdr and MySQL reads only (dry run: no mutating command,
// no herdr call besides snapshots, a private copy of the registry). Opt in
// with MAGNUM_LIVE_ENGINE=1.
func TestLiveDryRunOnce(t *testing.T) {
	if os.Getenv("MAGNUM_LIVE_ENGINE") != "1" {
		t.Skip("set MAGNUM_LIVE_ENGINE=1 to run against the real machine (read-only)")
	}
	home, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	layout := paths.Layout{Home: home}
	cfg, err := config.Load(layout, "")
	if err != nil {
		t.Fatal(err)
	}
	counter := &countingRunner{inner: &execx.Real{}}
	a, err := app.New(cfg, layout, app.Options{DryRun: true, Stderr: testWriter{t}, Runner: counter})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	e := FromApp(a)
	if err := e.Run(ctx, Options{Once: true, NoSignals: true}); err != nil {
		t.Fatal(err)
	}
	for _, op := range e.Planned() {
		t.Logf("planned: %s %s %s", op.Action, op.Subject, op.Detail)
	}
	for _, c := range a.DryRunner.Planned {
		t.Logf("planned command: %s", c.String())
	}
	// A second poll right away: the radar runs again, PR details only for
	// PRs that changed meanwhile (logged for comparison, not asserted: the
	// real repositories may change between the ticks).
	first := counter.graphql()
	if err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	t.Logf("graphql calls: first tick %d, second tick %d", first, counter.graphql()-first)
}

type countingRunner struct {
	inner execx.Runner
	mu    sync.Mutex
	n     int
}

func (c *countingRunner) Run(ctx context.Context, cmd execx.Cmd) (execx.Result, error) {
	if cmd.Name == "gh" && len(cmd.Args) > 1 && cmd.Args[1] == "graphql" {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
	}
	return c.inner.Run(ctx, cmd)
}

func (c *countingRunner) graphql() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
