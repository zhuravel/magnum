package cli

// `magnum factory` with no subcommand: the factory's screen (tui.RunFactory)
// when stdin and stdout are terminals; anywhere else its usage, as before.
//
// The screen reads the factory's registry, its agents store and the tasks'
// directories through factory.ScreenData (factoryScreenSource) and makes
// its requests through factoryActions; `,` opens the settings screen, as on
// the board.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// tuiFactory is the factory's screen; tests replace it.
var tuiFactory = tui.RunFactory

// runFactoryScreen opens the factory's screen until the operator quits;
// the time it closes is the last time you saw home.
func runFactoryScreen(c *Context) int {
	if !loadDaemonGroupConfig(c, "factory") {
		return 1
	}
	ctx, stop := signalContext()
	defer stop()
	src := &factoryScreenSource{c: c, now: inspNow}
	defer src.close()
	acts := newScreenActions(c) // the settings screen's, as the board's
	defer acts.Close()
	opts := tui.FactoryOptions{Icons: screenIcons(c.Config), NoMouse: !c.Config.Terminal.Mouse, Now: inspNow,
		Settings: &tui.SettingsScreen{Source: newSettingsSource(c, acts), Runtime: screenRuntime{acts}}}
	err := tuiFactory(ctx, src, newFactoryActions(c, src), opts)
	if fsx.Exists(c.Layout.FactoryDB()) { // a factory with no task has nothing you saw
		if serr := factory.SaveSeen(c.Layout, inspNow()); serr != nil {
			fmt.Fprintf(c.Stderr, "magnum factory: keep the time you saw home: %v\n", serr)
		}
	}
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory: %v\n", err)
		return 1
	}
	return 0
}

// factoryScreenSource reads the factory for the screen (factory.ScreenData).
// It opens the registry and the agents store once they exist, never
// creating them (only a request the screen sends creates the registry) and
// never migrating the agents store (the factory daemon does), and keeps
// them for the screen's life. A database opens outside the lock (an open
// may migrate, or ask ps whether a daemon runs) and is swapped in under it.
// The ready cards the last frame drew are apart, for the pager, which runs
// on the screen's own goroutine and must never wait for an open.
type factoryScreenSource struct {
	c   *Context
	now func() time.Time

	mu     sync.Mutex
	reg    *registry.Registry
	agents *store.Store

	// cards are the ready cards the last frame drew, by task.
	cards atomic.Pointer[map[int64]factory.ReadyCardFile]
}

// registryFor is the factory's registry; nil while it does not exist,
// unless create (a request makes it).
func (s *factoryScreenSource) registryFor(create bool) (*registry.Registry, error) {
	s.mu.Lock()
	reg := s.reg
	s.mu.Unlock()
	if reg != nil || (!create && !fsx.Exists(s.c.Layout.FactoryDB())) {
		return reg, nil
	}
	reg, err := openFactoryRegistry(s.c.Layout)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reg != nil { // another call opened it meanwhile
		reg.Close()
		return s.reg, nil
	}
	s.reg = reg
	return reg, nil
}

// agentsStore is the factory's agents store, nil while it does not exist;
// an error when it cannot be opened (one that needs a migration only the
// factory daemon makes, a file that is no database), which the frame says.
func (s *factoryScreenSource) agentsStore() (*store.Store, error) {
	s.mu.Lock()
	st := s.agents
	s.mu.Unlock()
	if st != nil || !fsx.Exists(s.c.Layout.FactoryAgentsDB()) {
		return st, nil
	}
	st, err := openFactoryAgentsStore(s.c.Layout)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agents != nil {
		st.Close()
		return s.agents, nil
	}
	s.agents = st
	return st, nil
}

// openFactoryAgentsStore opens the factory's agents store, which the CLI
// never migrates: a store that needs it is the factory daemon's to migrate
// when it starts on this build.
func openFactoryAgentsStore(l paths.Layout) (*store.Store, error) {
	return store.OpenWith(l.FactoryAgentsDB(), store.Options{BeforeMigrate: func(from, to int) error {
		return fmt.Errorf("the factory's agents store is at schema %d and this build needs %d: the factory daemon migrates it "+
			"when it starts on this build (`magnum factory install`)", from, to)
	}})
}

func (s *factoryScreenSource) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reg != nil {
		s.reg.Close()
		s.reg = nil
	}
	if s.agents != nil {
		s.agents.Close()
		s.agents = nil
	}
}

// lastSeen is when you last saw home, for the digest (factory.ReadSeen):
// the screen keeps it at your input, a blur and its end.
func (s *factoryScreenSource) lastSeen() time.Time {
	seen, err := factory.ReadSeen(s.c.Layout)
	if err != nil {
		return time.Time{} // an unreadable record digests nothing
	}
	return seen
}

// daemonDown reports whether no factory daemon runs: its pidfile names none
// (or cannot be read), or kill -0 finds its process gone. It runs on every
// frame, so it reads no process table: a pid reused by another process
// reads as up, as status's ps would tell, and the daemon's own requests
// then say it does not answer.
func daemonDown(l paths.Layout) bool {
	pid, err := daemonSys.factoryPID(l)
	if err != nil || pid <= 0 {
		return true
	}
	return errors.Is(daemonSys.Kill(pid, 0), syscall.ESRCH)
}

// readyCard is task id's ready card as the last frame drew it.
func (s *factoryScreenSource) readyCard(id int64) (factory.ReadyCardFile, bool) {
	cards := s.cards.Load()
	if cards == nil {
		return factory.ReadyCardFile{}, false
	}
	c, ok := (*cards)[id]
	return c, ok
}

// Frame reads the factory (factory.ScreenData) and keeps the ready cards
// it drew. An agents store it cannot open is a blocker line of home, with
// why, and the rest is drawn. A factory daemon that is down (daemonDown)
// is home's blocker line.
func (s *factoryScreenSource) Frame(ctx context.Context) (tui.FactoryData, error) {
	reg, err := s.registryFor(false)
	if err != nil || reg == nil {
		return tui.FactoryData{Cards: map[int64]tui.FactoryCards{}}, err
	}
	agents, aerr := s.agentsStore()
	cards := map[int64]factory.ReadyCardFile{}
	d, err := factory.ScreenData(ctx, reg, agents, s.now(), s.lastSeen(), factory.ScreenOptions{Config: s.c.Config, Layout: s.c.Layout,
		ReadyCards: func(id int64, c factory.ReadyCardFile) { cards[id] = c }, DaemonDown: daemonDown(s.c.Layout)})
	if err != nil {
		return d, err
	}
	s.cards.Store(&cards)
	if aerr != nil {
		d.Blocking = append(slices.Clone(d.Blocking), "agents not read")
		d.BlockingLong = append(slices.Clone(d.BlockingLong), "agents not read ("+textx.Clip(textx.FirstLine(aerr.Error()), 200)+")")
	}
	return d, nil
}
