package factory

// The review daemon as the factory's local review sees it (spec 5 and 7.2):
// a local round runs at the lowest priority and yields to magnum's real
// rounds, so it waits while one is in flight or due. The screen and the
// tick read the factory's PRs there too (PRStates): magnum's row, its
// autofix = "open" verdict, its fix loop, its stalemate and its attention;
// the tick their fix loops' record (FixRecords), for the PRs' status
// files, and magnum's first posted round on a published PR
// (FirstPostedRound), for the escape check of its task's local review; and
// the screen your own open PRs (OwnPRs), for home's rows. The
// factory reads magnum's registry and never writes or migrates it: it
// opens it query_only, so a write through its handle fails, and a registry
// of another schema than this build's is an error, and its file stays as
// it was.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/eligibility"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/prview"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
	"github.com/zhuravel/magnum/internal/tui"
)

// magnumRegistry is Magnum over magnum's registry and its daemon's pidfile
// in layout l.
type magnumRegistry struct {
	l   paths.Layout
	cfg *config.Config
	now func() time.Time
	// dueSince is when each queued PR was first seen due, by its id: one
	// that never starts holds the local review dueHold at most.
	mu       sync.Mutex
	dueSince map[int64]time.Time
	// fixFolds are the PRs' fix records as FixRecords folded them, by
	// magnum's subject of the PR; fixMu guards them.
	fixMu    sync.Mutex
	fixFolds map[string]fixFold
}

// dueHold is how long a queued round of magnum's that is due and never
// starts may hold the local review: magnum's gates that the factory cannot
// read (the GitHub floor, a pinned slot waiting for unpin, a paused agent
// kind) hold such a PR without a next try it could see.
const dueHold = 30 * time.Minute

// NewMagnum is magnum's review daemon of layout l, configured by cfg, as
// the factory's local review sees it (Magnum).
func NewMagnum(l paths.Layout, cfg *config.Config) Magnum {
	return &magnumRegistry{l: l, cfg: cfg, now: time.Now, dueSince: map[int64]time.Time{}}
}

// refuseMagnumMigration refuses every migration of magnum's registry
// (store.Options.BeforeMigrate): the factory never migrates it.
func refuseMagnumMigration(from, to int) error {
	return fmt.Errorf("magnum's registry is at schema %d and this build needs %d; the factory never migrates it: "+
		"restart magnum's daemon first (`magnum daemon-restart --drain`), then the factory goes on", from, to)
}

// open opens magnum's registry read only (store.Options.QueryOnly: a write
// through the handle fails) with every migration refused
// (refuseMagnumMigration), so one of a schema this build does not know is
// an error and keeps its file; nil and no error when there is no registry,
// which it never creates.
func (m *magnumRegistry) open() (*store.Store, error) {
	if _, err := os.Stat(m.l.DB()); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("magnum's registry: %w", err)
	}
	return store.OpenWith(m.l.DB(), store.Options{BeforeMigrate: refuseMagnumMigration, QueryOnly: true})
}

// Busy reports whether magnum's review rounds fill its limit
// ([daemon] max_concurrent_reviews, at least 1): the rounds in flight (a PR
// claimed, reviewing or verifying) and the rounds due (a queued PR whose
// next try has come and that magnum's gates would dispatch now). Below the
// limit the local review takes a free round, as one more of magnum's. Inside [daemon] quiet_hours they
// hold every queued round nobody forced (eligibility.QuietHoursHold, the
// dispatcher's own rule; a re-review of the judge alone that they let start
// shows as in flight once it does), and a due PR that has not started
// within dueHold is one of the gates the factory cannot read: it holds the
// local review no longer, and why says so while busy is false. No magnum
// daemon running (its pidfile names no live process) runs no round, and no
// registry holds none: neither is busy, and nothing is created. The
// registry opens with every migration refused (refuseMagnumMigration), so
// one that is a version behind this build (magnum's daemon still runs the
// build before) is an error and keeps its schema. why is plain words,
// never a PR's text.
func (m *magnumRegistry) Busy(ctx context.Context) (bool, string, error) {
	pid, err := engine.DaemonPID(m.l)
	if err != nil {
		return false, "", fmt.Errorf("magnum's daemon: %w", err)
	}
	if pid == 0 {
		return false, "", nil
	}
	st, err := m.open()
	if st == nil || err != nil {
		return false, "", err
	}
	defer st.Close()
	prs, err := st.ListPRs(ctx, store.PRFilter{States: append(slices.Clone(store.InFlightStates), store.PRQueued)})
	if err != nil {
		return false, "", fmt.Errorf("magnum's registry: %w", err)
	}
	now := m.now()
	quiet, limit := "", 1
	if m.cfg != nil {
		quiet, limit = m.cfg.Daemon.QuietHours, max(1, m.cfg.Daemon.MaxConcurrentReviews)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[int64]bool{}
	inFlight, due, stale := 0, 0, false
	for _, p := range prs {
		if slices.Contains(store.InFlightStates, p.State) {
			inFlight++
			continue
		}
		switch {
		case p.NextAttemptAt != nil && p.NextAttemptAt.After(now), p.NextEligibleAt != nil && p.NextEligibleAt.After(now):
			continue // its next try is later
		case !p.Forced && quiet != "" && eligibility.QuietHoursHold(quiet, now.Local(), false):
			continue // quiet hours hold it
		}
		seen[p.ID] = true
		since, ok := m.dueSince[p.ID]
		if !ok {
			since = now
			m.dueSince[p.ID] = now
		}
		if now.Sub(since) < dueHold {
			due++
		} else {
			stale = true
		}
	}
	for id := range m.dueSince {
		if !seen[id] {
			delete(m.dueSince, id)
		}
	}
	switch {
	case inFlight >= limit && limit == 1:
		return true, "a review round of magnum's runs", nil
	case inFlight >= limit:
		return true, fmt.Sprintf("magnum runs %d review rounds, its limit of %d", inFlight, limit), nil
	case inFlight+due >= limit && limit == 1:
		return true, "a review round of magnum's is due", nil
	case inFlight+due >= limit:
		return true, fmt.Sprintf("magnum's review rounds fill its limit of %d (%d running, %d due)", limit, inFlight, due), nil
	case stale:
		return false, fmt.Sprintf("a review round of magnum's is due but has not started for %s (magnum's own gates hold it): "+
			"the local review goes on", dueHold), nil
	}
	return false, "", nil
}

// PRRef names a PR as the factory knows it: its repository (owner/name)
// and number.
type PRRef struct {
	Repo   string
	Number int
}

// PRState is what magnum's registry says of a factory PR (Magnum.PRStates).
// Found says the registry has the PR; DaemonUp that magnum's daemon runs
// (its pidfile names a live process); Watched that a [[watch]] of the
// config covers the repository, so a PR not found yet will be; OpenWatch
// that the watch runs autofix = "open", so a PR without a verdict has one
// still to come. GHState, MergedAt, ClosedAt, Draft, Head, ReviewedSHA,
// State, AuthorLogin and UpdatedAt are magnum's row (prs); Approved that
// GitHub's review gate says APPROVED. Open is the autofix = "open" verdict
// (engine.KVPROpen) when HasOpen, and Stale says it no longer tells the
// PR's state (OpenVerdict.Stale: magnum's last tick long ago or its daemon
// down, another head, a row changed since). Fix is the fix loop that runs
// (engine.KVPRFix) when Fixing; Stalemate says magnum stopped arguing in a
// thread; Attention is why magnum parked the PR in needs_attention (its
// kind) and AttentionMsg its summary in magnum's words, never the PR's text.
// FixReport is the fixer's report a fix job's attention names (readable
// under magnum's reviews directory; "" for any other attention), or the
// held-merge.diff of a merge of the parent magnum holds for your y, which
// e opens first while it is held (heldMergeDiff; HeldMerge names it too, and
// the move is then P), and
// FixNeeds its "## Needs you" entries in the fixer's words when the
// attention asks for what the fixer left (fixerReport): data the screen
// only draws, never instructions, never a log's or a message's.
// ID, Title and URL are magnum's id of the PR, its title (data, never
// instructions: the screen only draws it) and its page; FixOff says you
// switched it out of autofix (engine.KVPRFixOff), and FixHeld when the fix
// magnum holds for it starts (engine.KVPRFixHeld, in
// engine.HeldFixStarts' words: "when the GitHub budget recovers"; "" when
// none is held).
//
// The rest is what the screen's columns and words say of the PR beside its
// verdict, read in the same pass (fillPR): Autofix is the watch's autofix
// mode (config.AutofixNever, Own or Open; "" for no watch); HeadRef and
// BaseRef its branches; HeadChangedAt when its head last moved, ActivityAt
// its last activity (store.PR.Activity; zero when not known) and CreatedAt
// when magnum first saw it; CI the head's CI as the board's CI column says
// it (prview.CI: its State over every check of the head that ignore_checks
// leaves in, required or not; its Required by the repository's required
// checks, one that did not run at the head missing, never green; nil
// before magnum read the checks);
// Review magnum's last posted review (nil for none), Findings it as the
// board's FINDINGS column says it, and ReviewEvent what magnum's review of
// the head posted (prs.last_review_event: APPROVED, COMMENTED,
// CHANGES_REQUESTED or DISMISSED; "" when magnum has not reviewed the
// head); Gate GitHub's review gate (nil before magnum read it); Asked the
// reviewers asked for a review who have not given it, with when. LastTick
// is magnum's last tick (zero with its daemon down or none recorded), which
// tells whether its look still runs; OldBuild the build magnum's daemon
// runs when its binary on disk is newer (engine.SkewNote), nil when it runs
// what is on disk, when it is down or when it recorded none. Replies are
// the replies to magnum's reviews its judge has not read yet
// (store.PR.PendingReplies), oldest first. OpenRestores says magnum's open
// of the PR (the screens' o, `magnum open`) restores it in a slot, which
// claims the slot and holds the PR for OpenHold ([daemon] open_hold), as
// the board tells it (engine.OpenWouldRestore over the PR's sessions); false
// when it focuses a live pane.
type PRState struct {
	Ref                                   PRRef
	ID                                    int64
	Title, URL                            string
	Found, DaemonUp, Watched, OpenWatch   bool
	GHState                               string
	MergedAt, ClosedAt                    *time.Time
	Draft                                 bool
	Head, ReviewedSHA, State, AuthorLogin string
	UpdatedAt                             time.Time
	Approved                              bool
	Open                                  engine.OpenVerdict
	HasOpen, Stale                        bool
	Fix                                   engine.FixState
	Fixing, Stalemate, FixOff             bool
	FixHeld                               string
	Attention, AttentionMsg               string
	FixReport                             string
	FixNeeds                              []string
	HeldMerge                             string

	Autofix                   string
	HeadRef, BaseRef          string
	HeadChangedAt, ActivityAt time.Time
	CreatedAt                 time.Time
	CI                        *tui.CIInfo
	Review                    *store.ReviewSummary
	Findings                  *tui.FindingsInfo
	ReviewEvent               string
	Gate                      *store.ReviewGate
	Asked                     []ReviewAsk
	LastTick                  time.Time
	OldBuild                  *engine.Build
	Replies                   []store.Reply
	OpenRestores              bool
	OpenHold                  time.Duration

	// NeverDeploys says the PR can never deploy to a staging, whoever asks
	// (engine.StagingNeverDeploys: its head lives in a fork, or its head
	// branch is master, main or staging-*): the deploy before the merge is
	// not asked of it, E would be refused.
	NeverDeploys bool

	// Files, DeployingHead and Stagings are read only for a PR of a
	// repository with staging envs, where the deploy before the merge can
	// be asked (staging.DeployBeforeMerge): Files, for every such PR
	// (fillPR), are the paths magnum's poller listed at the PR's head
	// (pr_files; nil without a list of that head, or when it cannot be
	// read); DeployingHead says a staging deploy of the PR's head is live,
	// queued or running (the review daemon's staging.deploy.* records, as
	// the board reads them, while its daemon runs; one that stopped for you,
	// ended or is of an older head is not): the deploy before the merge is
	// on its way, and E is refused until the push lands. It is read only
	// for the screen (ScreenData asks, deployRecords), never for the tick.
	// Stagings,
	// for your own PRs alone (OwnPRs: a task's are the factory daemon's
	// own), where the PR stands on each env as the review daemon last told
	// it (its record, engine.KVPRStaging, as engine.StagingShown shows it:
	// the config's envs in order, unknown without a record of the head that
	// reads, or with magnum's daemon not ticking).
	Files         []string
	DeployingHead bool
	Stagings      []engine.StagingEnv
}

// decision is GitHub's review decision of v as magnum last read it (its
// review gate's); "" before magnum read the gate.
func (v PRState) decision() string {
	if v.Gate == nil {
		return ""
	}
	return v.Gate.Decision
}

// ReviewAsk is a review asked of Who ("rev-ann", "team:<slug>") that is
// still asked, since At (the newest request of the PR's timeline; zero
// when the timeline does not name it).
type ReviewAsk struct {
	Who string
	At  time.Time
}

// PRStates reads refs' PRs from magnum's registry, one view per ref (see
// PRState), in one opening of the registry: per frame of the screen or per
// tick, never per task. It reads and never writes: the prs rows, the kv
// rows of the verdict, the fix loop, the held fix, the stalemate, the
// attention and the approval hold, magnum's last posted review of each PR, each
// repository's required checks, the paths the poller listed at each PR's
// head (pr_files, in a repository with staging envs only), and magnum's last
// tick. No registry finds nothing and creates none; one of a
// schema this build does not know is an error (refuseMagnumMigration), as
// is any read that fails, and the caller then knows nothing of the PRs,
// but for the paths: a failed read of them is no list.
func (m *magnumRegistry) PRStates(ctx context.Context, refs []PRRef) (map[PRRef]PRState, error) {
	pid, err := engine.DaemonPID(m.l)
	if err != nil {
		return nil, fmt.Errorf("magnum's daemon: %w", err)
	}
	out := make(map[PRRef]PRState, len(refs))
	for _, r := range refs {
		out[r] = m.stateOf(r, pid)
	}
	st, err := m.open()
	if st == nil || err != nil {
		return out, err
	}
	defer st.Close()
	pass, err := m.passOf(ctx, st, pid)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		v := out[r]
		if err := m.readPR(ctx, st, &v, pass); err != nil {
			return nil, fmt.Errorf("magnum's registry: %s#%d: %w", r.Repo, r.Number, err)
		}
		out[r] = v
	}
	return out, nil
}

// OwnPRs reads your own open PRs from magnum's registry: the PRs GitHub has
// open that are not drafts, whose author is one of your logins
// (config.SelfMatch, the logins magnum's look takes as yours) and whose
// repository a [[watch]] covers (magnum polls no other, so the row of a PR
// there tells nothing true of it), ordered by repository and number, each
// as PRStates reads a PR, with its stagings (fillStaging), in one opening
// of the registry. It reads and
// never writes or migrates the registry; no registry lists nothing and
// creates none, and neither does a config with no login of yours.
func (m *magnumRegistry) OwnPRs(ctx context.Context) ([]PRState, error) {
	if m.cfg == nil {
		return nil, nil
	}
	self := m.cfg.SelfMatch()
	pid, err := engine.DaemonPID(m.l)
	if err != nil {
		return nil, fmt.Errorf("magnum's daemon: %w", err)
	}
	st, err := m.open()
	if st == nil || err != nil {
		return nil, err
	}
	defer st.Close()
	pass, err := m.passOf(ctx, st, pid)
	if err != nil {
		return nil, err
	}
	prs, err := st.ListPRs(ctx, store.PRFilter{GHOpen: true})
	if err != nil {
		return nil, fmt.Errorf("magnum's registry: %w", err)
	}
	repos := map[int64]store.Repo{}
	var out []PRState
	for _, pr := range prs {
		if pr.GHState != store.GHOpen || pr.IsDraft || !self(store.Deref(pr.AuthorLogin)) {
			continue
		}
		repo, ok := repos[pr.RepoID]
		if !ok {
			if repo, err = st.RepoByID(ctx, pr.RepoID); err != nil {
				return nil, fmt.Errorf("magnum's registry: %w", err)
			}
			repos[pr.RepoID] = repo
		}
		v := m.stateOf(PRRef{Repo: repo.FullName(), Number: pr.Number}, pid)
		if !v.Watched {
			continue
		}
		if err := m.fillPR(ctx, st, &v, repo, pr, pass); err != nil {
			return nil, fmt.Errorf("magnum's registry: %s#%d: %w", v.Ref.Repo, pr.Number, err)
		}
		m.fillStaging(ctx, st, &v, pr, pass)
		out = append(out, v)
	}
	return out, nil
}

// fillStaging fills your PR v (magnum's row pr), in a repository with
// staging envs, with where it stands on each as the review daemon last
// told it (PRState.Stagings), as the board reads it: one read of st, no
// git. A record that cannot be read is none, every env unknown, as on the
// board: it hides no PR, and unknown asks no deploy.
func (m *magnumRegistry) fillStaging(ctx context.Context, st *store.Store, v *PRState, pr store.PR, pass *magnumPass) {
	envs := engine.StagingEnvs(m.cfg, v.Ref.Repo)
	if len(envs) == 0 {
		return
	}
	s, found, err := st.GetKV(ctx, engine.KVPRStaging(pr.ID))
	var rec engine.StagingRecord
	if found = found && err == nil; found {
		rec, found = engine.ParseStagingRecord(s)
	}
	v.Stagings = engine.StagingShown(rec, found, envs, pr.HeadSHA, pass.last, pass.now)
}

// headPaths are the paths magnum's poller listed at PR pr's head in st
// (pr_files); nil without a list of that head, and when the list cannot be
// read: it hides no PR, and no path asks no deploy.
func headPaths(ctx context.Context, st *store.Store, pr store.PR) []string {
	files, ok, err := st.PRFilesOf(ctx, pr.ID)
	if err != nil || !ok || files.HeadSHA != pr.HeadSHA {
		return nil
	}
	return files.Paths
}

// stateOf is what the config and magnum's daemon (pid, 0 for none) say of
// r before its registry is read: not found yet, whether a watch covers it,
// its autofix mode and whether that is "open".
func (m *magnumRegistry) stateOf(r PRRef, pid int) PRState {
	v := PRState{Ref: r, DaemonUp: pid > 0}
	if m.cfg != nil {
		if w := m.cfg.WatchFor(r.Repo); w != nil {
			v.Watched, v.Autofix = true, m.cfg.AutofixFor(w)
			v.OpenWatch = v.Autofix == config.AutofixOpen
		}
	}
	return v
}

// magnumPass is what one opening of magnum's registry reads once for all
// its PRs: what a verdict's staleness reads (OpenVerdict.Stale: magnum's
// last tick, zero with its daemon down, as no daemon decides anything now;
// its poll interval; and now), the build magnum's daemon runs when its
// binary on disk is newer (PRState.OldBuild), and each repository's
// required checks as the PRs' CI reads them (requiredOf), by repository.
type magnumPass struct {
	last, now time.Time
	poll      time.Duration
	required  map[string]store.RequiredChecks
	oldBuild  *engine.Build
}

// passOf reads magnum's last tick and its daemon's build from st, for its
// daemon of pid (0: none).
func (m *magnumRegistry) passOf(ctx context.Context, st *store.Store, pid int) (*magnumPass, error) {
	t := &magnumPass{now: m.now(), required: map[string]store.RequiredChecks{}}
	if s, ok, err := st.GetKV(ctx, store.KVDaemonLastTick); err != nil {
		return t, fmt.Errorf("magnum's registry: %w", err)
	} else if ok && pid > 0 {
		t.last, _ = store.ParseTime(s)
	}
	if m.cfg != nil {
		t.poll = m.cfg.Daemon.PollInterval.Duration
	}
	if b, ok := engine.ReadDaemonBuild(ctx, st); ok && pid > 0 && engine.SkewNote(b, "", engine.FileModTime) != "" {
		t.oldBuild = &b
	}
	return t, nil
}

// requiredOf is repository full's required checks ([[repo]]
// required_checks, else GitHub's as magnum's poller last read them), read
// once per pass.
func (m *magnumRegistry) requiredOf(ctx context.Context, st *store.Store, pass *magnumPass, full string) (store.RequiredChecks, error) {
	key := strings.ToLower(full)
	if req, ok := pass.required[key]; ok {
		return req, nil
	}
	var configured []string
	if m.cfg != nil {
		configured = m.cfg.RequiredChecks(full)
	}
	req, err := st.RequiredChecks(ctx, full, configured)
	if err != nil {
		return req, err
	}
	pass.required[key] = req
	return req, nil
}

// deployRecordsKey is the context key of deployRecords.
type deployRecordsKey struct{}

// deployRecords are magnum's staging deploy records (engine.KVStagingDeploys)
// as one frame of the screen reads them, once: the screen asks for them on
// the context of its reads of magnum's registry (withDeployRecords), and
// the tasks' PRs (PRStates) and yours (OwnPRs) share the one read. A read
// of the registry without them on its context, as the daemon's tick makes
// (followPRs, the status files), reads none and says no deploy runs.
type deployRecords struct {
	mu     sync.Mutex
	read   bool
	values map[string]string
}

// withDeployRecords is ctx with the request for the deploy records, for one
// frame's reads.
func withDeployRecords(ctx context.Context) context.Context {
	return context.WithValue(ctx, deployRecordsKey{}, &deployRecords{})
}

// of is every staging deploy's record, read from st on the first call. A
// read that fails is no record, for the rest of the frame too.
func (d *deployRecords) of(ctx context.Context, st *store.Store) map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.read {
		d.values, _ = st.KVValues(ctx, engine.KVStagingDeploys)
		d.read = true
	}
	return d.values
}

// deployingHead reports whether a staging deploy of PR pr (of repository
// full) at its head is live, queued or running, as the board reads it
// (deployingHead in the CLI's board): from every deploy's record, read
// once per frame, and only when the screen asked for them (deployRecords).
// A deploy that stopped for you is not live (E resumes it), nor is one that
// ended or one of an older head. A read that fails is no record: the line
// stays, E refuses a deploy that runs.
func (m *magnumRegistry) deployingHead(ctx context.Context, st *store.Store, full string, pr store.PR) bool {
	d, _ := ctx.Value(deployRecordsKey{}).(*deployRecords)
	if d == nil || pr.HeadSHA == "" {
		return false
	}
	return slices.ContainsFunc(engine.StagingDeploysOf(d.of(ctx, st), full, pr.Number), func(r engine.StagingDeployRecord) bool {
		return r.Live() && r.Head == pr.HeadSHA
	})
}

// readPR fills v from st: magnum's row of v.Ref and its kv rows; a
// repository or a PR st does not have leaves v not found.
func (m *magnumRegistry) readPR(ctx context.Context, st *store.Store, v *PRState, pass *magnumPass) error {
	repo, err := st.RepoByFullName(ctx, v.Ref.Repo)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	pr, err := st.PRByRepoNumber(ctx, repo.ID, v.Ref.Number)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	return m.fillPR(ctx, st, v, repo, pr, pass)
}

// fillPR fills v with magnum's row pr of repo, its kv rows and its last
// posted review in st.
func (m *magnumRegistry) fillPR(ctx context.Context, st *store.Store, v *PRState, repo store.Repo, pr store.PR, pass *magnumPass) error {
	v.Found, v.ID, v.Title, v.URL = true, pr.ID, store.Deref(pr.Title), pr.URL
	v.GHState, v.MergedAt, v.ClosedAt, v.Draft = pr.GHState, pr.MergedAt, pr.ClosedAt, pr.IsDraft
	v.Head, v.ReviewedSHA, v.State, v.AuthorLogin = pr.HeadSHA, store.Deref(pr.ReviewedSHA), pr.State, store.Deref(pr.AuthorLogin)
	v.UpdatedAt = pr.UpdatedAt
	v.Approved = pr.ReviewGate != nil && pr.ReviewGate.Decision == "APPROVED"
	v.HeadRef, v.BaseRef, v.HeadChangedAt, v.CreatedAt = store.Deref(pr.HeadRef), store.Deref(pr.BaseRef), pr.HeadChangedAt, pr.CreatedAt
	v.ActivityAt, v.Gate, v.Asked, v.LastTick, v.OldBuild = store.Deref(pr.Activity()), pr.ReviewGate, asked(pr), pass.last, pass.oldBuild
	if pr.ReviewedSHA != nil && *pr.ReviewedSHA == pr.HeadSHA {
		v.ReviewEvent = store.Deref(pr.LastReviewEvent)
	}
	req, err := m.requiredOf(ctx, st, pass, repo.FullName())
	if err != nil {
		return err
	}
	var ignore []string
	if m.cfg != nil {
		ignore = m.cfg.IgnoreChecks(repo.FullName())
	}
	v.CI = prview.CI(store.SeenCI(pr.CI, pr.HeadSHA, ignore), pr.HeadSHA, req, prview.Merge{Draft: pr.IsDraft, Decision: v.decision()})
	sums, err := st.LastReviewSummaries(ctx, []int64{pr.ID})
	if err != nil {
		return err
	}
	if s, ok := sums[pr.ID]; ok {
		v.Review, v.Findings = &s, prview.Findings(s, prview.Held(ctx, st, pr.ID, s))
	}
	kv := func(key string) (string, error) {
		s, _, err := st.GetKV(ctx, key)
		return s, err
	}
	s, err := kv(engine.KVPROpen(pr.ID))
	if err != nil {
		return err
	}
	if v.Open, v.HasOpen = engine.ParseOpenVerdict(s); v.HasOpen {
		v.Stale = v.Open.Stale(pr.HeadSHA, pr.UpdatedAt, pass.last, pass.now, pass.poll)
	}
	if s, err = kv(engine.KVPRFix(pr.ID)); err != nil {
		return err
	}
	v.Fix, v.Fixing = engine.ParseFixState(s)
	if _, v.FixOff, err = st.GetKV(ctx, engine.KVPRFixOff(pr.ID)); err != nil {
		return err
	}
	switch raw, held, err := st.GetKV(ctx, engine.KVPRFixHeld(pr.ID)); {
	case err != nil:
		return err
	case held:
		v.FixHeld = engine.HeldFixStarts(raw)
	}
	if err := m.openFacts(ctx, st, v, repo.FullName()); err != nil {
		return err
	}
	v.Replies = pr.PendingReplies()
	v.NeverDeploys = engine.StagingNeverDeploys(pr)
	if len(engine.StagingEnvs(m.cfg, v.Ref.Repo)) > 0 { // the deploy before the merge reads them (PRState.Files, DeployingHead)
		v.Files = headPaths(ctx, st, pr)
		// no live record is believed while magnum's daemon is down: the one that
		// was running when it died stays so for a day, and E would only say it is down
		v.DeployingHead = v.DaemonUp && m.deployingHead(ctx, st, v.Ref.Repo, pr)
	}
	if s, err = kv(engine.KVPRStalemate(pr.ID)); err != nil {
		return err
	}
	_, v.Stalemate = engine.ParseStalemate(s)
	if pr.State == store.PRNeedsAttention {
		if v.Attention, err = kv(engine.KVPRAttention(pr.ID)); err != nil {
			return err
		}
		ref := fmt.Sprintf("%s#%d", repo.Name, pr.Number)
		why := attention.Explain(v.Attention, store.Deref(pr.LastError), ref)
		v.AttentionMsg = why.Summary
		if why.Kind == attention.KindFix {
			v.FixReport, v.FixNeeds = m.fixerReport(store.Deref(pr.LastError))
		}
		if held := m.heldMergeDiff(ctx, st, repo, pr); held != "" { // e opens it first while the merge is held
			v.HeldMerge, v.FixReport, v.FixNeeds = held, held, nil
		}
	}
	return nil
}

// heldMergeDiff is the held-merge.diff of the merge of the parent magnum
// holds for your y on pr of repo (engine.KVPRHeldMerge), in magnum's
// report directory of the PR (never a path the row names), when a regular
// file is there, looked at through an os.Root of magnum's reviews
// directory so that no link leads out of it; "" for none.
func (m *magnumRegistry) heldMergeDiff(ctx context.Context, st *store.Store, repo store.Repo, pr store.PR) string {
	raw, ok, err := st.GetKV(ctx, engine.KVPRHeldMerge(pr.ID))
	if err != nil || !ok {
		return ""
	}
	if _, ok := engine.ParseHeldMerge(raw); !ok {
		return ""
	}
	path := filepath.Join(m.l.PRReviews(repo.Owner, repo.Name, pr.Number), engine.HeldMergeFile)
	rel, err := filepath.Rel(m.l.Reviews(), path)
	if err != nil || !filepath.IsLocal(rel) {
		return ""
	}
	root, err := os.OpenRoot(m.l.Reviews())
	if err != nil {
		return ""
	}
	defer root.Close()
	if fi, err := root.Lstat(rel); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	return path
}

// openFacts fills what magnum's open of PR v (of repository full) does,
// from its sessions in st: whether it restores the PR (OpenRestores) and how
// long a restore holds it.
func (m *magnumRegistry) openFacts(ctx context.Context, st *store.Store, v *PRState, full string) error {
	ss, err := st.SessionsByPR(ctx, v.ID)
	if err != nil {
		return err
	}
	var fix *engine.FixState
	if v.Fixing {
		fix = &v.Fix
	}
	v.OpenRestores = engine.OpenWouldRestore(ss, engine.OpenRole(m.cfg, full, fix, ss), engine.OpenRestorable(m.cfg, full))
	if m.cfg != nil {
		v.OpenHold = m.cfg.Daemon.OpenHold.Duration
	}
	return nil
}

// asked are the reviewers pr asks for a review who have not given it
// (store.PR.RequestedReviewers), each with its newest request in the PR's
// timeline (zero when the timeline does not name it).
func asked(pr store.PR) []ReviewAsk {
	var out []ReviewAsk
	for _, who := range pr.RequestedReviewers {
		a := ReviewAsk{Who: who}
		for _, r := range pr.ReviewRequests {
			if strings.EqualFold(r.To, who) && r.At.After(a.At) {
				a.At = r.At
			}
		}
		out = append(out, a)
	}
	return out
}

// fixReportMax bounds the read of a fixer's report: a report is a few
// lines, and a file past this is cut, so it does not parse.
const fixReportMax = 1 << 20

// fixNeedMax bounds an entry of a fixer's Needs you as the screen keeps it
// (runes): a row draws its start, the card a line.
const fixNeedMax = 300

// fixerReport reads the fixer's report that msg, a fix job's message (a
// PR's last_error), names (attention.FixReport): its path, when it lies
// under magnum's reviews directory and reads there (through an os.Root of
// that directory, so no link leads out of it; "" otherwise), and, when the
// message asks for what the fixer left (attention.FixAsks: not a cap's or a
// failed run's), the entries of its "## Needs you" (engine.ParseFixReport,
// none for a report that does not parse) that the message still asks for
// (engine.FixNeedsAsked: not one whose thread was resolved or answered
// before magnum wrote it; none when a count does not say which), each with
// its control characters as spaces, clipped and with token-shaped text
// masked. The entries are the fixer's own words about the PR: the screen
// only draws them, never as instructions, and nothing writes them to a
// log, an event or a message.
func (m *magnumRegistry) fixerReport(msg string) (string, []string) {
	path := attention.FixReport(msg)
	rel, err := filepath.Rel(m.l.Reviews(), path)
	if path == "" || err != nil || !filepath.IsLocal(rel) {
		return "", nil
	}
	root, err := os.OpenRoot(m.l.Reviews())
	if err != nil {
		return "", nil
	}
	defer root.Close()
	f, err := root.Open(rel)
	if err != nil {
		return "", nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, fixReportMax))
	if err != nil {
		return "", nil
	}
	if !attention.FixAsks(msg) {
		return path, nil
	}
	run, _ := agents.ReportRun(b)
	rep, err := engine.ParseFixReport(string(b), run)
	if err != nil {
		return path, nil
	}
	asked := engine.FixNeedsAsked(msg, rep.NeedsYou)
	if len(asked) == 0 {
		return path, nil
	}
	needs := make([]string, len(asked))
	for i, e := range asked {
		needs[i] = textx.Clip(execx.Redact(fixerText(e)), fixNeedMax)
	}
	return path, needs
}

// fixerText is s, the fixer's words, with each control character a space,
// so no escape sequence it copied from the PR's text reaches the screen
// (whose own cleaning is the last line of defence, not the first).
func fixerText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// FixLoops counts the fix loops magnum started on a PR: by hand (`magnum
// fix`, f on the board) and by autofix; and the fixers' pushes on it
// (fix.pushed) and the commits they pushed.
type FixLoops struct {
	ByHand, ByAutofix int
	Pushes, Commits   int
}

// fixStartKind is the event magnum writes as a fix loop's cycle starts,
// fixPushedKind the one as its fixer's push lands.
const (
	fixStartKind  = "fix.start"
	fixPushedKind = "fix.pushed"
)

// FixLoopsOf counts, per PR of refs, the fix loops magnum's registry in
// layout l recorded: its fix.start events of cycle 1 (a later cycle is the
// same loop), by who started each (engine.AutofixBy, or by hand), and its
// fix.pushed events with the commits each names (one that names none is
// one commit: a push carries at least one), over the whole history the
// registry keeps ([daemon] keep_events). A PR it has no event of counts
// zero. It reads and never writes or migrates the registry, as PRStates;
// no registry counts nothing and creates none.
func FixLoopsOf(ctx context.Context, l paths.Layout, refs []PRRef) (map[PRRef]FixLoops, error) {
	out := make(map[PRRef]FixLoops, len(refs))
	st, err := (&magnumRegistry{l: l}).open()
	if st == nil || err != nil {
		return out, err
	}
	defer st.Close()
	for _, r := range refs {
		repo, err := st.RepoByFullName(ctx, r.Repo)
		if errors.Is(err, store.ErrNotFound) {
			out[r] = FixLoops{}
			continue
		} else if err != nil {
			return nil, fmt.Errorf("magnum's registry: %w", err)
		}
		evs, err := st.EventsBySubject(ctx, engine.PRSubject(repo, r.Number), 0) // the whole history: a busy PR has hundreds
		if err != nil {
			return nil, fmt.Errorf("magnum's registry: %w", err)
		}
		var c FixLoops
		for _, ev := range evs {
			var d struct {
				Cycle   int    `json:"cycle"`
				By      string `json:"by"`
				Commits int    `json:"commits"`
			}
			if ev.Kind == fixPushedKind {
				_ = json.Unmarshal(ev.Data, &d) // a push that names no count is one commit
				c.Pushes++
				c.Commits += max(d.Commits, 1)
				continue
			}
			if ev.Kind != fixStartKind || json.Unmarshal(ev.Data, &d) != nil || d.Cycle != 1 {
				continue
			}
			if d.By == engine.AutofixBy {
				c.ByAutofix++
			} else {
				c.ByHand++
			}
		}
		out[r] = c
	}
	return out, nil
}

// FixRecord is what magnum's registry recorded of the fix loops on a PR
// (Magnum.FixRecords), from its fix events over the history it keeps
// ([daemon] keep_events): the loops started (a fix.start of cycle 1), the
// cycle of the newest fix.start, the fixers' pushes (fix.pushed), the
// review threads they replied in (fix.replied's count), and whether the
// newest of the loop's starts and ends left the PR to you (fix.needs_you,
// fix.capped, fix.failed) rather than ending it (fix.done, fix.ended,
// fix.stopped) or starting a cycle. Counts only, never an event's text.
type FixRecord struct {
	Loops, Cycle, Pushes, Threads int
	LeftToYou                     bool
}

// fixFold is a PR's FixRecord as folded so far, and the id of the last
// event of the PR it folded.
type fixFold struct {
	last int64
	rec  FixRecord
}

// add folds magnum's event ev of the PR into f.
func (f *fixFold) add(ev store.Event) {
	f.last = max(f.last, ev.ID)
	var d struct {
		Cycle   int `json:"cycle"`
		Threads int `json:"threads"`
	}
	if len(ev.Data) > 0 {
		_ = json.Unmarshal(ev.Data, &d) // an event without them counts no cycle and no thread
	}
	switch ev.Kind {
	case fixStartKind:
		if d.Cycle == 1 {
			f.rec.Loops++
		}
		f.rec.Cycle, f.rec.LeftToYou = max(d.Cycle, 0), false
	case fixPushedKind:
		f.rec.Pushes++
	case "fix.replied":
		f.rec.Threads += max(d.Threads, 0)
	case "fix.needs_you", "fix.capped", "fix.failed":
		f.rec.LeftToYou = true
	case "fix.done", "fix.ended", "fix.stopped":
		f.rec.LeftToYou = false
	}
}

// FixRecords reads, per PR of refs, its FixRecord from magnum's registry,
// in one opening of the registry. m keeps each PR's record and the last
// event it folded, so a call reads only the PR's events written since: its
// whole kept history only the first time. A repository or a PR magnum
// does not have, and no registry, are the zero record (nothing created).
// It reads and never writes or migrates the registry, as PRStates; a read
// that fails is an error, and the caller then knows no record.
func (m *magnumRegistry) FixRecords(ctx context.Context, refs []PRRef) (map[PRRef]FixRecord, error) {
	out := make(map[PRRef]FixRecord, len(refs))
	st, err := m.open()
	if st == nil || err != nil {
		if err != nil {
			return nil, err
		}
		return out, nil
	}
	defer st.Close()
	m.fixMu.Lock()
	defer m.fixMu.Unlock()
	for _, r := range refs {
		repo, err := st.RepoByFullName(ctx, r.Repo)
		if errors.Is(err, store.ErrNotFound) {
			out[r] = FixRecord{}
			continue
		} else if err != nil {
			return nil, fmt.Errorf("magnum's registry: %w", err)
		}
		subject := engine.PRSubject(repo, r.Number)
		f := m.fixFolds[subject]
		evs, err := st.EventsMatching(ctx, []store.SubjectMatch{{Exact: subject}}, f.last, 0)
		if err != nil {
			return nil, fmt.Errorf("magnum's registry: %w", err)
		}
		for _, ev := range evs {
			f.add(ev)
		}
		if m.fixFolds == nil {
			m.fixFolds = map[string]fixFold{}
		}
		m.fixFolds[subject] = f
		out[r] = f.rec
	}
	return out, nil
}

// PostedRound is magnum's round on a PR that posted a review
// (Magnum.FirstPostedRound): the run of its judge that posted it (RunID,
// magnum's runs.id), the round's number, the head it reviewed, the review's
// id on GitHub, when the round began (its first run), when magnum recorded
// the post (Posted: the posting run's verification, else its end; zero
// when the run has neither), and the findings it posted, in the judge's
// order. Counts are the new findings the round's
// result counts, P0 to P3 (store.ReviewSummary.Counts), when Counted: a
// result without provenance counts findings magnum recorded no row of.
type PostedRound struct {
	RunID    string
	Round    int
	Head     string
	ReviewID int64
	Start    time.Time
	Posted   time.Time
	Findings []PostedFinding
	Counts   [4]int
	Counted  bool
}

// PostedFinding is one finding a posted round posted: magnum's id of it
// (findings.id), its path and its lines (magnum keeps one line, so
// StartLine and EndLine are both it; 0 for a finding in the review's body),
// its priority as the judge wrote it, and its title. The title is the
// judge's words about the PR, data only: the escape check keeps nothing of
// it but the factory's hash (titleHash), and nothing writes it to the
// factory's registry, a log, an event or a message.
type PostedFinding struct {
	ID                 int64
	Path               string
	StartLine, EndLine int
	Priority, Title    string
}

// FirstPostedRound reads, in one opening of magnum's registry, its first
// round on PR ref that posted a review and began at or after since (the
// task's publish), with the findings it posted, every priority, and its
// result's counts (store.FirstPostedReview: one query): false while there
// is none, and for a repository, a PR or a registry magnum does not have
// (nothing is created). It reads and never writes or migrates the
// registry, as PRStates; a read that fails is an error.
func (m *magnumRegistry) FirstPostedRound(ctx context.Context, ref PRRef, since time.Time) (PostedRound, bool, error) {
	st, err := m.open()
	if st == nil || err != nil {
		return PostedRound{}, false, err
	}
	defer st.Close()
	repo, err := st.RepoByFullName(ctx, ref.Repo)
	if errors.Is(err, store.ErrNotFound) {
		return PostedRound{}, false, nil
	} else if err != nil {
		return PostedRound{}, false, fmt.Errorf("magnum's registry: %w", err)
	}
	pr, err := st.PRByRepoNumber(ctx, repo.ID, ref.Number)
	if errors.Is(err, store.ErrNotFound) {
		return PostedRound{}, false, nil
	} else if err != nil {
		return PostedRound{}, false, fmt.Errorf("magnum's registry: %w", err)
	}
	rv, fs, ok, err := st.FirstPostedReview(ctx, pr.ID, since)
	if err != nil || !ok {
		if err != nil {
			err = fmt.Errorf("magnum's registry: %w", err)
		}
		return PostedRound{}, false, err
	}
	out := PostedRound{RunID: rv.RunID, Round: rv.Round, Head: rv.SHA, ReviewID: rv.ReviewID, Start: rv.RoundStart, Posted: rv.PostedAt,
		Counted: rv.OK}
	if rv.OK {
		out.Counts = rv.Summary.Counts
	}
	for _, f := range fs {
		out.Findings = append(out.Findings, PostedFinding{ID: f.ID, Path: f.Path, StartLine: f.Line, EndLine: f.Line, Priority: f.Severity,
			Title: f.Title})
	}
	return out, true, nil
}
