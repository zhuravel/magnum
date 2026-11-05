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
	"fmt"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/store"
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
// creating them (only a request the screen sends creates the registry),
// never migrating the agents store (the factory daemon does), and keeps
// them for the screen's life. It keeps the ready cards of the last frame
// (the diff's pager reads nothing) and whether the screen's terminal has
// focus (the digest counts the time you were away).
type factoryScreenSource struct {
	c   *Context
	now func() time.Time

	mu     sync.Mutex
	reg    *registry.Registry
	agents *store.Store
	cards  map[int64]factory.ReadyCardFile
	// focused says the screen's terminal has focus since focusedAt (its
	// last report); a screen with no report counts as unfocused.
	focused   bool
	focusedAt time.Time
}

// registryFor is the factory's registry; nil while it does not exist,
// unless create (a request makes it).
func (s *factoryScreenSource) registryFor(create bool) (*registry.Registry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reg != nil || (!create && !fsx.Exists(s.c.Layout.FactoryDB())) {
		return s.reg, nil
	}
	reg, err := openFactoryRegistry(s.c.Layout)
	if err != nil {
		return nil, err
	}
	s.reg = reg
	return reg, nil
}

// agentsStore is the factory's agents store, nil while it does not exist
// or needs a migration only the factory daemon makes (the screen then
// shows no agents, and tries again on the next frame).
func (s *factoryScreenSource) agentsStore() *store.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.agents != nil || !fsx.Exists(s.c.Layout.FactoryAgentsDB()) {
		return s.agents
	}
	st, err := store.OpenWith(s.c.Layout.FactoryAgentsDB(), store.Options{BeforeMigrate: func(from, to int) error {
		return fmt.Errorf("the factory's agents store is at schema %d and this build needs %d: the factory daemon migrates it "+
			"when it starts on this build (`magnum factory install`)", from, to)
	}})
	if err != nil {
		return nil
	}
	s.agents = st
	return st
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

// focus notes the screen's focus report.
func (s *factoryScreenSource) focus(focused bool, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.focused, s.focusedAt = focused, at
}

// lastSeen is when you last saw home, for the digest: the time kept
// (factory.ReadSeen), or now while the screen has focus and you saw home
// since it gained it (you are at the screen, not away).
func (s *factoryScreenSource) lastSeen(now time.Time) time.Time {
	seen, err := factory.ReadSeen(s.c.Layout)
	if err != nil {
		return time.Time{} // an unreadable record digests nothing
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.focused && !seen.Before(s.focusedAt) {
		return now
	}
	return seen
}

// readyCard is task id's ready card as the last frame read it.
func (s *factoryScreenSource) readyCard(id int64) (factory.ReadyCardFile, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cards[id]
	return c, ok
}

// Frame reads the factory (factory.ScreenData) and keeps the ready cards
// it shows.
func (s *factoryScreenSource) Frame(ctx context.Context) (tui.FactoryData, error) {
	reg, err := s.registryFor(false)
	if err != nil || reg == nil {
		return tui.FactoryData{Cards: map[int64]tui.FactoryCards{}}, err
	}
	now := s.now()
	d, err := factory.ScreenData(ctx, reg, s.agentsStore(), now, s.lastSeen(now), factory.ScreenOptions{Config: s.c.Config, Layout: s.c.Layout})
	if err != nil {
		return d, err
	}
	cards := map[int64]factory.ReadyCardFile{}
	for id, c := range d.Cards {
		if c.Ready == nil {
			continue
		}
		if card, err := factory.ReadReadyCard(s.c.Layout, id); err == nil {
			cards[id] = card
		}
	}
	s.mu.Lock()
	s.cards = cards
	s.mu.Unlock()
	return d, nil
}
