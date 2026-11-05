package cli

// `magnum factory` with no subcommand: the factory's screen (tui.RunFactory)
// when stdin and stdout are terminals; anywhere else its usage, as before.
//
// The screen reads the factory's registry for home's rows and the tasks'
// cards (factoryScreenSource) and, in this build, makes no request: its
// action keys say so. The screen's full data (the decision cards, the
// agents, the digest) and its requests to the factory daemon are the
// factory's screen adapter's.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/tui"
)

// tuiFactory is the factory's screen; tests replace it.
var tuiFactory = tui.RunFactory

// runFactoryScreen opens the factory's screen until the operator quits.
func runFactoryScreen(c *Context) int {
	if !loadDaemonGroupConfig(c, "factory") {
		return 1
	}
	ctx, stop := signalContext()
	defer stop()
	src := &factoryScreenSource{c: c, now: inspNow}
	defer src.close()
	opts := tui.FactoryOptions{Icons: screenIcons(c.Config), NoMouse: !c.Config.Terminal.Mouse, Now: inspNow}
	if err := tuiFactory(ctx, src, nil, opts); err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory: %v\n", err)
		return 1
	}
	return 0
}

// factoryScreenSource reads the factory's registry for the screen: one row
// per task, grouped by who it waits on (the tasks done today last, the ones
// stopped or done before today left out), with its task card's facts. It
// opens the registry once it exists, never creating it, and keeps it open
// for the screen's life.
type factoryScreenSource struct {
	c   *Context
	now func() time.Time

	mu  sync.Mutex
	reg *registry.Registry
}

func (s *factoryScreenSource) registry() (*registry.Registry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reg != nil || !fsx.Exists(s.c.Layout.FactoryDB()) {
		return s.reg, nil
	}
	reg, err := openFactoryRegistry(s.c.Layout)
	if err != nil {
		return nil, err
	}
	s.reg = reg
	return reg, nil
}

func (s *factoryScreenSource) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reg != nil {
		s.reg.Close()
		s.reg = nil
	}
}

// Frame reads the factory's tasks.
func (s *factoryScreenSource) Frame(ctx context.Context) (tui.FactoryData, error) {
	d := tui.FactoryData{Cards: map[int64]tui.FactoryCards{}}
	reg, err := s.registry()
	if err != nil || reg == nil {
		return d, err
	}
	tasks, err := reg.Tasks(ctx)
	if err != nil {
		return d, err
	}
	now := s.now()
	for _, t := range tasks {
		row, ok := factoryScreenRow(t, now)
		if !ok {
			continue
		}
		d.Rows = append(d.Rows, row)
		waiting := ""
		if len(row.WaitsOn) > 0 {
			waiting = strings.TrimPrefix(row.WaitsOn[0].Who+": "+row.WaitsOn[0].What, ": ")
		}
		d.Cards[t.ID] = tui.FactoryCards{Task: tui.TaskCard{Key: row.Key, Kind: t.Kind, Title: t.Title, Branch: t.Branch,
			Copy: t.Copy, WaitingFor: waiting, Started: t.Created}}
		if d.Repo == "" {
			_, d.Repo, _ = strings.Cut(t.Repo, "/")
		}
	}
	return d, nil
}

// factoryScreenRow is task t's row on home; false for a task home leaves out
// (stopped, or done before today).
func factoryScreenRow(t registry.Task, now time.Time) (tui.FactoryRow, bool) {
	row := tui.FactoryRow{ID: t.ID, Key: factory.TaskKey(t), Title: t.Title, Since: t.Updated}
	if t.PRNumber > 0 {
		row.GitHub = fmt.Sprintf("#%d", t.PRNumber)
	}
	switch t.State {
	case registry.TaskNeedsYou:
		row.Group, row.Mark = tui.GroupNeedsYou, tui.MarkNeedsYou
	case registry.TaskDone, registry.TaskPublished:
		if y, m, dd := t.Updated.In(now.Location()).Date(); y != now.Year() || m != now.Month() || dd != now.Day() {
			return row, false
		}
		row.Group, row.Mark, row.Since = tui.GroupDone, tui.MarkDone, time.Time{}
	case registry.TaskStopped:
		return row, false
	default:
		row.Group, row.Mark = tui.GroupWaits, tui.MarkWaits
		if t.State == registry.TaskActive && strings.HasPrefix(t.WaitsWho, "agent") { // only a working agent moves
			row.Mark = tui.MarkWorking
		}
	}
	if t.WaitsWho != "" || t.WaitsWhat != "" {
		row.WaitsOn = []tui.WaitsOn{{Who: t.WaitsWho, What: t.WaitsWhat, OnYou: t.State == registry.TaskNeedsYou}}
	}
	return row, true
}
