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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
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

// factoryScreenFlags are the flags of `magnum factory` that tune its screen.
type factoryScreenFlags struct {
	repo string // the repository scope, as the board's --repo
}

// addFactoryScreenFlags defines the screen's flags on `magnum factory`.
func addFactoryScreenFlags(c *Context, cmd *cobra.Command, f *factoryScreenFlags) {
	cmd.Flags().StringVar(&f.repo, "repo", "", "the screen shows one repository (owner/name or name), as the board's --repo")
	_ = cmd.RegisterFlagCompletionFunc("repo", completeFlag(c.completeRepos))
}

// runFactoryScreen opens the factory's screen until the operator quits;
// the time it closes is the last time you saw home. The screen opens in the
// owner scope magnum's registry keeps for it and the board, and keeps every
// change of it there.
func runFactoryScreen(c *Context, f factoryScreenFlags) int {
	repo, err := repoFlag(f.repo)
	if err != nil {
		return inspUsage(c, "factory", err.Error(), "[--repo owner/name|name]")
	}
	if !loadDaemonGroupConfig(c, "factory") {
		return 1
	}
	ctx, stop := signalContext()
	defer stop()
	src := &factoryScreenSource{c: c, now: inspNow, magnum: factory.NewMagnum(c.Layout, c.Config)}
	defer src.close()
	acts := newScreenActions(c) // the settings screen's and your PRs' keys, as the board's
	defer acts.Close()
	kv := openScreenKV(c.Layout) // the owner scope the board shares; nil: every owner, kept nowhere
	if kv != nil {
		defer kv.Close()
	}
	owner, _ := screenOwner(ctx, kv)
	opts := tui.FactoryOptions{Icons: screenIcons(c.Config), NoMouse: !c.Config.Terminal.Mouse, Now: inspNow,
		Settings: &tui.SettingsScreen{Source: newSettingsSource(c, acts), Runtime: screenRuntime{acts}},
		Repo:     repo, DefaultRepo: defaultRepo(c.Config), DefaultOwner: owner,
		OwnerChanged: func(owner string) { keepScreenOwner(ctx, kv, owner) }}
	err = tuiFactory(ctx, src, newFactoryActions(c, src, acts), opts)
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

// openScreenKV is magnum's registry for the owner scope the screen shares
// with the PR board (kvScreenOwner): nil when there is none or it cannot be
// opened. It never creates the registry and never migrates it: a registry of
// another schema than this build's keeps no scope for the screen.
func openScreenKV(l paths.Layout) *store.Store {
	if !fsx.Exists(l.DB()) {
		return nil
	}
	st, err := store.OpenWith(l.DB(), store.Options{BeforeMigrate: func(from, to int) error {
		return fmt.Errorf("magnum's registry is at schema %d and this build reads %d", from, to)
	}})
	if err != nil {
		return nil
	}
	return st
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
	// magnum reads the published tasks' PRs from magnum's registry, never
	// migrated or written (factory.NewMagnum); nil: a published task keeps
	// the words of a screen without it.
	magnum factory.Magnum

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

// Frame reads the factory (factory.ScreenData), with the published tasks'
// PRs and your own open PRs from magnum's registry, and keeps the ready
// cards it drew. Without the factory's registry (no task was ever started)
// home lists your PRs alone, and creates nothing: with [board] own_prs =
// "factory" the board leaves them out. The rows' PRs carry what r, R, z, M
// and U go by (prFacts). An agents store it cannot open is a
// blocker line of home, with why, and the rest is drawn. A factory daemon
// that is down (daemonDown) is home's blocker line. A frame that could not
// read your PRs says so (tui.FactoryData.OwnUnread), beside its blocker
// line, so the screen keeps the owner scope their rows would show.
func (s *factoryScreenSource) Frame(ctx context.Context) (tui.FactoryData, error) {
	reg, err := s.registryFor(false)
	if err != nil || reg == nil && s.magnum == nil {
		return tui.FactoryData{Cards: map[int64]tui.FactoryCards{}}, err
	}
	agents, aerr := s.agentsStore()
	cards := map[int64]factory.ReadyCardFile{}
	var magnum factory.Magnum
	var own *ownReadWatch
	if s.magnum != nil { // no wrapper around none: ScreenData reads no PRs then
		own = &ownReadWatch{Magnum: s.magnum}
		magnum = own
	}
	d, err := factory.ScreenData(ctx, reg, agents, s.now(), s.lastSeen(), factory.ScreenOptions{Config: s.c.Config, Layout: s.c.Layout,
		ReadyCards: func(id int64, c factory.ReadyCardFile) { cards[id] = c }, DaemonDown: daemonDown(s.c.Layout), Magnum: magnum})
	if err != nil {
		return d, err
	}
	d.OwnUnread = own != nil && own.failed
	s.cards.Store(&cards)
	s.prFacts(ctx, d.Rows)
	if aerr != nil {
		d.Blocking = append(slices.Clone(d.Blocking), "agents not read")
		d.BlockingLong = append(slices.Clone(d.BlockingLong), "agents not read ("+textx.Clip(textx.FirstLine(aerr.Error()), 200)+")")
	}
	return d, nil
}

// ownReadWatch is magnum's registry as one frame reads it, noting whether
// its read of your open PRs failed: ScreenData then draws none of them and
// adds a blocker line, but returns no error.
type ownReadWatch struct {
	factory.Magnum
	failed bool
}

func (w *ownReadWatch) OwnPRs(ctx context.Context) ([]factory.PRState, error) {
	prs, err := w.Magnum.OwnPRs(ctx)
	w.failed = w.failed || err != nil
	return prs, err
}

// prFacts gives the rows' PRs what r, R, z, M and U go by
// (tui.FactoryPRFacts, the board's predicate's facts): magnum's row of each
// PR (with the replies its judge has not re-decided), its snooze while it
// holds and its Codex flag, read from magnum's registry in one opening per
// frame, read only and never migrated (a registry of another schema than
// this build's is not read), and whether its watch runs autofix. A PR the
// registry has no row of, and a registry that is not there or cannot be
// read, leave its facts unread: the keys then refuse only on what the rest
// of the row tells, and magnum's daemon decides the rest.
func (s *factoryScreenSource) prFacts(ctx context.Context, rows []tui.FactoryRow) {
	if !slices.ContainsFunc(rows, func(r tui.FactoryRow) bool { return r.PR != nil }) || !fsx.Exists(s.c.Layout.DB()) {
		return
	}
	st, err := store.OpenWith(s.c.Layout.DB(), store.Options{QueryOnly: true, BeforeMigrate: func(from, to int) error {
		return fmt.Errorf("magnum's registry is at schema %d and this build reads %d", from, to)
	}})
	if err != nil {
		return
	}
	defer st.Close()
	now := s.now()
	for i, r := range rows {
		if r.PR == nil {
			continue
		}
		full, num, _ := strings.Cut(r.PR.Ref, "#")
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		repo, err := st.RepoByFullName(ctx, full)
		if err != nil {
			continue
		}
		pr, err := st.PRByRepoNumber(ctx, repo.ID, n)
		if err != nil {
			continue
		}
		f := &tui.FactoryPRFacts{State: pr.State, GHState: pr.GHState, Head: pr.HeadSHA, Reviewed: store.Deref(pr.ReviewedSHA),
			Muted: pr.Muted, MergedUnreviewed: pr.MergedUnreviewed(), FlagDismissed: pr.FlagDismissed(), Replies: len(pr.PendingReplies())}
		if v, ok, err := st.GetKV(ctx, engine.KVPRSnooze(pr.ID)); err == nil && ok {
			if z, ok := engine.ParseSnooze(v); ok && z.Active(now) {
				f.SnoozedUntil = z.Until
			}
		}
		if flag, ok := codexFlagOf(ctx, st, pr.ID); ok { // codex_flag.go, as the board reads it
			f.CodexFlag = flag.Short()
		}
		if w := s.c.Config.WatchFor(repo.FullName()); w != nil {
			f.Autofix = config.AutofixStarts(s.c.Config.AutofixFor(w))
		}
		withFacts := *r.PR
		withFacts.Facts = f
		rows[i].PR = &withFacts
	}
}
