// Package inventory is the read-mostly reconciliation view: the registry
// (slots, assignments, sessions) compared with what is really on disk
// (`git worktree list` of every watched main clone), in MySQL (per-worktree
// databases), in herdr (agents per path) and, on request, on GitHub (states of
// PRs checked out in manual worktrees). Scan never changes anything;
// UpsertSlotDatabases is the only write and touches slot_databases only.
//
// Sources other than the store degrade: an unreachable MySQL, herdr socket,
// main clone or GitHub becomes a line in Inventory.Warnings and the facts it
// would have supplied stay empty (DatabasesListed, AgentsListed and
// ClonesListed say which are unknown rather than empty). Dropped-database
// detection runs only when MySQL answered (Inventory.DatabasesListed);
// orphan detection only on complete ownership facts (Inventory.OrphansKnown:
// databases and every clone listed, every slot directory and slug marker
// read conclusively), since cleanup drops what it reports.
package inventory

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/gitx"
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Git is the subset of *gitx.Client the scanner reads.
type Git interface {
	WorktreeList(ctx context.Context, mainClone string) ([]gitx.Worktree, error)
	Status(ctx context.Context, dir string) (gitx.Status, error)
	BranchPR(ctx context.Context, dir, branch string) (number int, ok bool, err error)
	FindClone(ctx context.Context, cloneRoot, owner, name string) (string, error)
}

// MySQL is the subset of *mysqlx.Client the scanner reads.
type MySQL = slots.DBLister

// Herdr is the subset of *herdr.Client the scanner reads.
type Herdr interface {
	Snapshot(ctx context.Context) (herdr.Snapshot, error)
}

// GitHub is the subset of *github.Client the scanner reads.
type GitHub interface {
	ConfirmStates(ctx context.Context, owner, repo string, numbers []int) (map[int]github.PRState, []int, error)
}

// Scanner builds an Inventory. Store and Git are required; every other
// dependency is optional and its facts are skipped (with a warning) when nil.
type Scanner struct {
	Store  *store.Store
	Git    Git
	MySQL  MySQL          // per-worktree databases; nil: no database facts
	Herdr  Herdr          // agents per path; nil: no agent facts
	GitHub GitHub         // Options.GitHubStates; nil: states come from the store only
	Runner execx.Runner   // `du -sk` for Options.Sizes; nil: no sizes
	Config *config.Config // pools (slot path/name templates, DB templates) and watches (clone roots)
	Now    func() time.Time
}

// Options selects the expensive parts of a scan.
type Options struct {
	Sizes        bool // `du -sk` every existing slot (and external worktree when External)
	External     bool // list worktrees of watched main clones that are not managed slots
	GitHubStates bool // with External: one ConfirmStates call per repo for their PRs
}

// Finding kinds.
const (
	KindLostSlot       = "lost_slot"             // registry slot whose directory is missing
	KindUnknownSlotDir = "unknown_slot_dir"      // directory named like a slot that the registry does not own
	KindOrphanDB       = "orphan_db"             // suffixed databases whose slug no slot or worktree claims
	KindForeignAgent   = "foreign_agent"         // non-magnum herdr agent working inside a managed slot
	KindHeadDrift      = "head_drift"            // claimed/busy/held slot whose HEAD is not checked_out_sha
	KindMissingPR      = "assignment_missing_pr" // open assignment whose PR is gone from the registry or GitHub
	KindExternalClosed = "external_pr_closed"    // manual worktree whose PR is MERGED or CLOSED
)

// PRSource values: where ExternalView.PRNumber came from.
const (
	PRSourceGitConfig  = "git_config"  // git config branch.<b>.pr (authoritative)
	PRSourceBranchName = "branch_name" // /PR-(\d+)/ in the branch name: a guess, often a Jira key
)

// SlugSource values: where ExternalView.Slug came from.
const (
	SlugSourceFile     = "file"     // <path>/tmp/.worktree-db-slug
	SlugSourceInferred = "inferred" // directory or branch name that matches existing databases
)

// StateSource values: where ExternalView.GHState came from.
const (
	StateSourceStore  = "store"
	StateSourceGitHub = "github"
)

// droppedBy is slot_databases.dropped_by for databases that disappeared from MySQL.
const droppedBy = "reconcile"

// ErrNotListed is returned by UpsertSlotDatabases when the scan could not list
// MySQL: recording would mark every database dropped.
var ErrNotListed = errors.New("inventory: mysql databases were not listed")

// Inventory is one reconciliation snapshot. Slices are sorted (slots by
// natural name, external worktrees by natural path, databases by name,
// findings by kind then subject) and nil-safe for JSON.
type Inventory struct {
	ScannedAt       time.Time         `json:"scanned_at"`
	Slots           []SlotView        `json:"slots"`
	External        []ExternalView    `json:"external,omitempty"`
	Databases       []mysqlx.Database `json:"databases"`        // schemas named <prefix><slug> by the pools' database templates
	DatabasesListed bool              `json:"databases_listed"` // false: database facts and orphans are unknown
	// AgentsListed: the herdr snapshot was read; false means agent facts
	// (SlotView.Agents, ExternalView.Agents) are unknown, not empty.
	AgentsListed bool `json:"agents_listed"`
	// ClonesListed: `git worktree list` (and clone discovery) succeeded for
	// every watched main clone; false means some worktrees are unknown.
	ClonesListed bool `json:"clones_listed"`
	// OrphansKnown: orphan detection ran on complete facts (DatabasesListed,
	// ClonesListed and every slot directory and slug marker read
	// conclusively). False: OrphanDBs is empty because ownership is unknown.
	OrphansKnown bool              `json:"orphans_known"`
	OrphanDBs    []mysqlx.Database `json:"orphan_dbs"`
	Drift        []Finding         `json:"drift"`
	Warnings     []string          `json:"warnings,omitempty"` // sources that could not be read (redacted)
}

// SlotView is a managed (pool or per-PR, not removed) registry slot with its live facts.
type SlotView struct {
	Slot       store.Slot        `json:"slot"`
	Exists     bool              `json:"exists"`   // directory present
	Worktree   bool              `json:"worktree"` // listed by `git worktree list` of its main clone
	Head       string            `json:"head,omitempty"`
	Branch     string            `json:"branch,omitempty"` // "" when detached
	Detached   bool              `json:"detached"`
	Dirty      *bool             `json:"dirty,omitempty"` // nil when unknown
	Tracked    int               `json:"tracked"`
	Untracked  int               `json:"untracked"`
	PR         *store.PR         `json:"pr,omitempty"` // slot.pr_id, else the open assignment's PR
	Assignment *store.Assignment `json:"assignment,omitempty"`
	Slug       string            `json:"slug"` // slots.db_slug, else the sanitized slot name
	Databases  []DBView          `json:"databases"`
	Agents     []AgentView       `json:"agents"`
	SizeKB     *int64            `json:"size_kb,omitempty"`
	Drift      []string          `json:"drift,omitempty"` // finding kinds about this slot
}

// ExternalView is a worktree of a watched main clone that magnum does not
// manage (talkable.repoN, talkable__worktrees/*); the main checkout is excluded.
type ExternalView struct {
	Path        string      `json:"path"`
	MainClone   string      `json:"main_clone"`
	Repo        string      `json:"repo"` // owner/name
	Exists      bool        `json:"exists"`
	Head        string      `json:"head,omitempty"`
	Branch      string      `json:"branch,omitempty"`
	Detached    bool        `json:"detached"`
	Locked      bool        `json:"locked"`
	Prunable    bool        `json:"prunable"`
	PRNumber    int         `json:"pr_number,omitempty"`
	PRSource    string      `json:"pr_source,omitempty"`
	PRConfirmed bool        `json:"pr_confirmed"` // git config, or a branch-name guess whose PR head equals Head
	GHState     string      `json:"gh_state,omitempty"`
	StateSource string      `json:"state_source,omitempty"`
	MergedAt    *time.Time  `json:"merged_at,omitempty"`
	ClosedAt    *time.Time  `json:"closed_at,omitempty"`
	Slug        string      `json:"slug,omitempty"`
	SlugSource  string      `json:"slug_source,omitempty"`
	Databases   []DBView    `json:"databases"`
	Agents      []AgentView `json:"agents"`
	SizeKB      *int64      `json:"size_kb,omitempty"`
	Drift       []string    `json:"drift,omitempty"`
}

// DBView is one database of a slot or worktree. Present is meaningful only
// when Inventory.DatabasesListed.
type DBView struct {
	Name     string  `json:"name"`
	SizeMB   float64 `json:"size_mb"`
	Present  bool    `json:"present"`
	Expected bool    `json:"expected"` // rendered from the pool's database templates
}

// AgentView is a herdr agent whose pane or foreground cwd is inside a path.
type AgentView struct {
	Name        string       `json:"name,omitempty"`
	Agent       string       `json:"agent"` // codex, claude, ...
	Status      herdr.Status `json:"status"`
	PaneID      string       `json:"pane_id"`
	WorkspaceID string       `json:"workspace_id"`
	Cwd         string       `json:"cwd"`
	SessionID   string       `json:"session_id,omitempty"` // agent_session.value
	Magnum      bool         `json:"magnum"`               // a live magnum session or an mg- name
}

// Finding is one drift item. Safe means magnum may fix it automatically
// (only magnum-owned resources); everything else is reported for a human.
type Finding struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"` // slot:<name>, path:<dir>, slug:<slug>
	Message string `json:"message"`
	Safe    bool   `json:"safe"`
}

// Scan builds the inventory. Only a store failure (or a cancelled context)
// is an error; other sources degrade into Inventory.Warnings.
func (s *Scanner) Scan(ctx context.Context, opts Options) (Inventory, error) {
	if s.Store == nil || s.Git == nil {
		return Inventory{}, errors.New("inventory: Scanner needs Store and Git")
	}
	sc := &scan{s: s, opts: opts, cfg: s.Config, known: map[string]bool{}, cloneIdx: map[string]int{}}
	if sc.cfg == nil {
		sc.cfg = &config.Config{}
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	sc.inv.ScannedAt = now()

	if err := sc.loadRegistry(ctx); err != nil {
		return Inventory{}, err
	}
	sc.loadClones(ctx)
	sc.loadDatabases(ctx)
	sc.loadAgents(ctx)
	if err := sc.buildSlots(ctx); err != nil {
		return Inventory{}, err
	}
	if err := sc.buildExternal(ctx); err != nil {
		return Inventory{}, err
	}
	sc.findUnknownDirs()
	sc.findOrphans()
	if opts.External && opts.GitHubStates {
		sc.confirmStates(ctx)
	}
	sc.externalDrift()
	if opts.Sizes {
		sc.sizes(ctx)
	}
	if err := ctx.Err(); err != nil {
		return Inventory{}, fmt.Errorf("inventory: %w", err)
	}
	sc.finish()
	return sc.inv, nil
}

type clone struct {
	path   string // as configured (cleaned); what git is called with
	canon  string
	repo   string // owner/name
	pool   *config.Pool
	list   []gitx.Worktree
	listed bool
}

type scan struct {
	s    *Scanner
	opts Options
	cfg  *config.Config
	inv  Inventory

	managed      []store.Slot
	managedPaths map[string]bool // canonical path -> managed
	removedPaths map[string]string
	repoByName   map[string]store.Repo // lower-case owner/name
	ownedNames   map[string]bool
	ownedPanes   map[string]bool

	clones   []*clone
	cloneIdx map[string]int // canonical path -> index in clones

	dbs    []mysqlx.Database
	dbByNm map[string]mysqlx.Database
	agents []agentAt

	known      map[string]bool // slugs that some slot, worktree or slot-like dir claims
	incomplete []string        // ownership facts that could not be read; orphans are not computed
	externals  []ExternalView

	mu       sync.Mutex
	warnSeen map[string]bool
}

type agentAt struct {
	info     herdr.AgentInfo
	cwd, fgd string // canonical
}

func (sc *scan) warnf(format string, args ...any) {
	msg := execx.Redact(fmt.Sprintf(format, args...))
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.warnSeen == nil {
		sc.warnSeen = map[string]bool{}
	}
	if !sc.warnSeen[msg] {
		sc.warnSeen[msg] = true
		sc.inv.Warnings = append(sc.inv.Warnings, msg)
	}
}

func (sc *scan) addFinding(kind, subject string, safe bool, format string, args ...any) {
	sc.inv.Drift = append(sc.inv.Drift, Finding{Kind: kind, Subject: subject, Message: fmt.Sprintf(format, args...), Safe: safe})
}

// incompletef records an ownership fact that could not be read, so orphan
// detection does not run (Inventory.OrphansKnown stays false).
func (sc *scan) incompletef(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !slices.Contains(sc.incomplete, msg) {
		sc.incomplete = append(sc.incomplete, msg)
	}
}

func (sc *scan) loadRegistry(ctx context.Context) error {
	st := sc.s.Store
	slots, err := st.ListSlots(ctx, store.SlotFilter{})
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	sc.managedPaths = map[string]bool{}
	sc.removedPaths = map[string]string{}
	for _, sl := range slots {
		if sl.Kind != store.SlotKindPool && sl.Kind != store.SlotKindPerPR {
			continue
		}
		if sl.State == store.SlotRemoved {
			sc.removedPaths[fsx.Canon(sl.Path)] = sl.Name
			continue
		}
		sc.managed = append(sc.managed, sl)
		sc.managedPaths[fsx.Canon(sl.Path)] = true
	}
	repos, err := st.ListRepos(ctx)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	sc.repoByName = map[string]store.Repo{}
	for _, r := range repos {
		sc.repoByName[strings.ToLower(r.FullName())] = r
	}
	sessions, err := st.LiveSessions(ctx)
	if err != nil {
		return fmt.Errorf("inventory: %w", err)
	}
	sc.ownedNames, sc.ownedPanes = map[string]bool{}, map[string]bool{}
	for _, ss := range sessions {
		if n := store.Deref(ss.AgentName); n != "" {
			sc.ownedNames[n] = true
		}
		if p := store.Deref(ss.HerdrPaneID); p != "" {
			sc.ownedPanes[p] = true
		}
	}
	return nil
}

// loadClones collects every watched main clone (pool clones, the clones of
// registry slots, and repo clones from the store or the watch clone root)
// and lists its worktrees. Inventory.ClonesListed: every lookup and listing
// succeeded.
func (sc *scan) loadClones(ctx context.Context) {
	listed := true
	for i := range sc.cfg.Pools {
		p := &sc.cfg.Pools[i]
		sc.addClone(paths.Expand(p.MainClone), p.Repo)
	}
	for _, sl := range sc.managed {
		mc := sl.MainClone
		if mc == "" {
			if p := sc.cfg.PoolFor(sl.RepoFullName); p != nil {
				mc = paths.Expand(p.MainClone)
			}
		}
		if sl.Kind == store.SlotKindPerPR && !gitx.IsRepo(paths.Expand(mc)) {
			// Not a clone (a same-named folder, or not cloned yet): the
			// slot's own facts still come from its directory; listing
			// would only warn on every scan.
			continue
		}
		sc.addClone(mc, sl.RepoFullName)
	}
	names := make([]string, 0, len(sc.repoByName))
	for n := range sc.repoByName {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		r := sc.repoByName[n]
		if sc.cfg.PoolFor(r.FullName()) != nil {
			continue // the pool's main clone is listed above
		}
		// A recorded clone path is used while it is still a repository; a
		// stale or wrong one (a same-named folder that is not a clone) falls
		// back to discovery under the watch's clone root.
		if p := paths.Expand(store.Deref(r.ClonePath)); p != "" && gitx.IsRepo(p) {
			sc.addClone(p, r.FullName())
			continue
		}
		w := sc.cfg.WatchFor(r.FullName())
		if w == nil {
			continue
		}
		// A per-PR repository is cloned on its first review; until then
		// there is nothing to list, and no warning.
		found, err := sc.s.Git.FindClone(ctx, slots.CloneRoot(*w), r.Owner, r.Name)
		switch {
		case err == nil:
			sc.addClone(found, r.FullName())
		case !errors.Is(err, gitx.ErrNoClone):
			sc.warnf("git: find the clone of %s: %v", r.FullName(), err)
			sc.incompletef("clone discovery of %s failed", r.FullName()) // its worktrees are unknown
			listed = false
		}
	}
	for _, c := range sc.clones {
		list, err := sc.s.Git.WorktreeList(ctx, c.path)
		if err != nil {
			sc.warnf("git: worktree list %s: %v", c.path, err)
			sc.incompletef("git worktree list of %s failed", c.path)
			listed = false
			continue
		}
		c.list, c.listed = list, true
	}
	sc.inv.ClonesListed = listed
}

func (sc *scan) addClone(path, repo string) {
	if path == "" {
		return
	}
	path = filepath.Clean(paths.Expand(path))
	cp := fsx.Canon(path)
	if i, ok := sc.cloneIdx[cp]; ok {
		c := sc.clones[i]
		if c.repo == "" {
			c.repo = repo
		}
		if c.pool == nil && repo != "" {
			c.pool = sc.cfg.PoolFor(repo)
		}
		return
	}
	c := &clone{path: path, canon: cp, repo: repo}
	if repo != "" {
		c.pool = sc.cfg.PoolFor(repo)
	}
	sc.cloneIdx[cp] = len(sc.clones)
	sc.clones = append(sc.clones, c)
}

func (sc *scan) cloneFor(path string) *clone {
	if path == "" {
		return nil
	}
	if i, ok := sc.cloneIdx[fsx.Canon(paths.Expand(path))]; ok {
		return sc.clones[i]
	}
	return nil
}

// loadDatabases lists the pools' per-worktree databases (every schema named
// by a pool's database template prefixes).
func (sc *scan) loadDatabases(ctx context.Context) {
	if sc.s.MySQL == nil {
		sc.warnf("mysql: no client; database facts skipped")
		return
	}
	dbs, bad, err := slots.ListPoolDatabases(ctx, sc.s.MySQL, sc.cfg.Pools...)
	for _, tmpl := range bad {
		sc.warnf("config: database template %q does not end in __{slug}; its databases are not listed", tmpl)
	}
	if err != nil {
		sc.warnf("mysql: %v", err)
		return
	}
	slices.SortFunc(dbs, func(a, b mysqlx.Database) int { return strings.Compare(a.Name, b.Name) })
	sc.dbs = dbs
	sc.dbByNm = make(map[string]mysqlx.Database, len(dbs))
	for _, d := range dbs {
		sc.dbByNm[d.Name] = d
	}
	sc.inv.Databases = dbs
	sc.inv.DatabasesListed = true
}

func (sc *scan) loadAgents(ctx context.Context) {
	if sc.s.Herdr == nil {
		sc.warnf("herdr: no client; agent facts skipped")
		return
	}
	snap, err := sc.s.Herdr.Snapshot(ctx)
	if err != nil {
		sc.warnf("herdr: %v", err)
		return
	}
	sc.inv.AgentsListed = true
	for _, a := range snap.Agents {
		sc.agents = append(sc.agents, agentAt{info: a, cwd: fsx.Canon(a.Cwd), fgd: fsx.Canon(a.ForegroundCwd)})
	}
}

func (sc *scan) agentsUnder(root string) []AgentView {
	var out []AgentView
	for _, a := range sc.agents {
		if !under(a.cwd, root) && !under(a.fgd, root) {
			continue
		}
		i := a.info
		v := AgentView{
			Name: i.Name, Agent: i.Agent, Status: i.AgentStatus, PaneID: i.PaneID, WorkspaceID: i.WorkspaceID, Cwd: i.Cwd,
			Magnum: sc.ownedNames[i.Name] || sc.ownedPanes[i.PaneID] || strings.HasPrefix(i.Name, slots.AgentPrefix),
		}
		if i.AgentSession != nil {
			v.SessionID = i.AgentSession.Value
		}
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b AgentView) int { return strings.Compare(a.PaneID, b.PaneID) })
	return out
}

// headTracked are the slot states in which HEAD must equal checked_out_sha.
var headTracked = []string{store.SlotClaimed, store.SlotBusy, store.SlotHeld}

// lostExempt are the slot states whose directory may be missing by design:
// provisioning writes the row before the directory exists, removing deletes
// it. A lost_slot finding there would break the in-flight transition.
var lostExempt = []string{store.SlotProvisioning, store.SlotRemoving}

func (sc *scan) buildSlots(ctx context.Context) error {
	st := sc.s.Store
	for _, sl := range sc.managed {
		v := SlotView{Slot: sl, Slug: slots.SlotDBSlug(sl)}
		subject := "slot:" + sl.Name
		exists, conclusive := sc.isDir(sl.Path)
		v.Exists = exists || !conclusive // unreadable: assume present, never lost
		mc := sl.MainClone
		if mc == "" {
			if p := sc.cfg.PoolFor(sl.RepoFullName); p != nil {
				mc = p.MainClone
			}
		}
		if c := sc.cloneFor(mc); c != nil && c.listed {
			if wt, ok := gitx.FindWorktree(c.list, sl.Path); ok {
				v.Worktree, v.Head, v.Branch, v.Detached = true, wt.Head, wt.Branch, wt.Detached
			}
		}
		if v.Exists && v.Worktree {
			gs, err := sc.s.Git.Status(ctx, sl.Path)
			if err != nil {
				sc.warnf("git: status %s: %v", sl.Path, err)
			} else {
				dirty := gs.Dirty()
				v.Dirty, v.Tracked, v.Untracked = &dirty, gs.Tracked, gs.Untracked
			}
		}

		a, err := st.OpenAssignmentBySlot(ctx, sl.ID)
		switch {
		case err == nil:
			v.Assignment = &a
		case !errors.Is(err, store.ErrNotFound):
			return fmt.Errorf("inventory: slot %s: %w", sl.Name, err)
		}
		prID := sl.PRID
		if prID == nil && v.Assignment != nil {
			prID = &v.Assignment.PRID
		}
		if prID != nil {
			pr, err := st.PRByID(ctx, *prID)
			switch {
			case err == nil:
				v.PR = &pr
			case !errors.Is(err, store.ErrNotFound):
				return fmt.Errorf("inventory: slot %s: %w", sl.Name, err)
			}
		}

		var expected []string
		if p := sc.cfg.PoolFor(sl.RepoFullName); p != nil && sl.Kind == store.SlotKindPool {
			expected = p.DBNames(v.Slug)
		}
		v.Databases = sc.dbViews(v.Slug, expected)
		sc.known[v.Slug] = true
		if v.Exists {
			v.Agents = sc.agentsUnder(fsx.Canon(sl.Path))
		}

		if !v.Exists && !slices.Contains(lostExempt, sl.State) {
			sc.addFinding(KindLostSlot, subject, true, "slot directory %s is missing (registry state %s)", sl.Path, sl.State)
		}
		for _, ag := range v.Agents {
			if !ag.Magnum {
				sc.addFinding(KindForeignAgent, subject, false, "%s agent in pane %s (%s) is not a magnum session", orDash(ag.Agent), ag.PaneID, orDash(string(ag.Status)))
			}
		}
		if want := store.Deref(sl.CheckedOutSHA); want != "" && v.Head != "" && v.Head != want && slices.Contains(headTracked, sl.State) {
			sc.addFinding(KindHeadDrift, subject, false, "HEAD is %s, registry checked out %s (slot %s)", textx.ShortSHA(v.Head), textx.ShortSHA(want), sl.State)
		}
		if v.Assignment != nil {
			if err := sc.checkAssignmentPR(ctx, sl, v.Assignment, v.PR); err != nil {
				return err
			}
		}
		sc.inv.Slots = append(sc.inv.Slots, v)
	}
	return nil
}

func (sc *scan) checkAssignmentPR(ctx context.Context, sl store.Slot, a *store.Assignment, pr *store.PR) error {
	subject := "slot:" + sl.Name
	if pr == nil || pr.ID != a.PRID {
		got, err := sc.s.Store.PRByID(ctx, a.PRID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			sc.addFinding(KindMissingPR, subject, false, "open assignment %d points at PR id %d, which is not in the registry", a.ID, a.PRID)
			return nil
		case err != nil:
			return fmt.Errorf("inventory: slot %s: %w", sl.Name, err)
		}
		pr = &got
	}
	if pr.GHState == store.GHUnknown {
		sc.addFinding(KindMissingPR, subject, false, "open assignment %d holds %s#%d, which GitHub reports as not found", a.ID, sl.RepoFullName, pr.Number)
	}
	return nil
}

// dbViews lists the expected names (present or not) plus every other listed
// database carrying slug.
func (sc *scan) dbViews(slug string, expected []string) []DBView {
	out := []DBView{}
	seen := map[string]bool{}
	for _, n := range expected {
		d, ok := sc.dbByNm[n]
		out = append(out, DBView{Name: n, SizeMB: d.SizeMB, Present: ok, Expected: true})
		seen[n] = true
	}
	if slug != "" {
		for _, d := range sc.dbs {
			if !seen[d.Name] && hasSlug(d, slug) {
				out = append(out, DBView{Name: d.Name, SizeMB: d.SizeMB, Present: true})
			}
		}
	}
	slices.SortFunc(out, func(a, b DBView) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (sc *scan) slugHasDatabases(slug string) bool {
	for _, d := range sc.dbs {
		if hasSlug(d, slug) {
			return true
		}
	}
	return false
}

// buildExternal walks every listed clone. Slugs are always collected (orphan
// detection needs them); views, PR numbers and agents only with Options.External.
func (sc *scan) buildExternal(ctx context.Context) error {
	seen := map[string]bool{}
	for _, c := range sc.clones {
		if !c.listed {
			continue
		}
		for i, wt := range c.list {
			cp := fsx.Canon(wt.Path)
			if wt.Bare || seen[cp] || sc.managedPaths[cp] {
				continue
			}
			seen[cp] = true
			if i == 0 || cp == c.canon { // the main checkout
				if slug, _ := sc.readSlug(wt.Path); slug != "" {
					sc.known[slug] = true
				}
				continue
			}
			ev := ExternalView{
				Path: wt.Path, MainClone: c.path, Repo: c.repo, Head: wt.Head, Branch: wt.Branch,
				Detached: wt.Detached, Locked: wt.Locked, Prunable: wt.Prunable,
			}
			ev.Exists, _ = sc.isDir(wt.Path) // unreadable: not present (nothing acts on it)
			if ev.Exists {
				if slug, conclusive := sc.readSlug(wt.Path); slug != "" {
					ev.Slug, ev.SlugSource = slug, SlugSourceFile
				} else if conclusive { // an unreadable marker is not guessed around
					for _, cand := range []string{dirSlug(wt.Path, c.path), slots.DBSlug(wt.Branch)} {
						if cand != "" && sc.slugHasDatabases(cand) {
							ev.Slug, ev.SlugSource = cand, SlugSourceInferred
							break
						}
					}
				}
			}
			if ev.Slug != "" {
				sc.known[ev.Slug] = true
			}
			if sc.opts.External {
				if err := sc.fillExternal(ctx, &ev); err != nil {
					return err
				}
			}
			sc.externals = append(sc.externals, ev)
		}
	}
	if sc.opts.External {
		sc.inv.External = sc.externals
	}
	return nil
}

func (sc *scan) fillExternal(ctx context.Context, ev *ExternalView) error {
	ev.Databases = sc.dbViews(ev.Slug, nil)
	ev.Agents = []AgentView{}
	if ev.Exists {
		ev.Agents = sc.agentsUnder(fsx.Canon(ev.Path))
	}
	if ev.Branch != "" {
		dir := ev.MainClone
		if ev.Exists {
			dir = ev.Path
		}
		n, ok, err := sc.s.Git.BranchPR(ctx, dir, ev.Branch)
		switch {
		case err != nil:
			sc.warnf("git: branch.%s.pr in %s: %v", ev.Branch, dir, err)
		case ok && n > 0:
			ev.PRNumber, ev.PRSource, ev.PRConfirmed = n, PRSourceGitConfig, true
		}
		if ev.PRNumber == 0 {
			if n, ok := branchPRNumber(ev.Branch); ok {
				ev.PRNumber, ev.PRSource = n, PRSourceBranchName
			}
		}
	}
	if ev.PRNumber == 0 {
		return nil
	}
	repo, ok := sc.repoByName[strings.ToLower(ev.Repo)]
	if !ok {
		return nil
	}
	pr, err := sc.s.Store.PRByRepoNumber(ctx, repo.ID, ev.PRNumber)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("inventory: %s#%d: %w", ev.Repo, ev.PRNumber, err)
	}
	ev.GHState, ev.StateSource, ev.MergedAt, ev.ClosedAt = pr.GHState, StateSourceStore, pr.MergedAt, pr.ClosedAt
	if pr.HeadSHA != "" && pr.HeadSHA == ev.Head {
		ev.PRConfirmed = true
	}
	return nil
}

// findUnknownDirs reports directories named like a pool slot (slot_path with
// {n}) or a per-PR worktree (<clone>__worktrees/pr-N) that the registry does
// not own, and protects their databases from orphan detection: the slug
// marker, else the directory's slug (pool), and the marker plus the default
// per-PR slug (magnum_pr_N).
func (sc *scan) findUnknownDirs() {
	seen := map[string]bool{}
	report := func(dir, slotKind string) {
		cp := fsx.Canon(dir)
		if seen[cp] || sc.managedPaths[cp] {
			return
		}
		if exists, _ := sc.isDir(dir); !exists {
			return
		}
		seen[cp] = true
		if name, ok := sc.removedPaths[cp]; ok {
			sc.addFinding(KindUnknownSlotDir, "path:"+dir, false, "directory exists but the registry marks slot %s removed", name)
		} else {
			sc.addFinding(KindUnknownSlotDir, "path:"+dir, false, "directory named like a %s slot is not in the registry", slotKind)
		}
	}
	for _, p := range sc.cfg.Pools {
		tmpl := paths.Expand(p.SlotPath)
		if !strings.Contains(tmpl, "{n}") {
			continue
		}
		re := slots.TemplateRegexp(tmpl)
		matches := sc.glob(templateGlob(tmpl))
		for _, m := range matches {
			if !re.MatchString(m) {
				continue
			}
			report(m, "pool")
			if slug, _ := sc.readSlug(m); slug != "" {
				sc.known[slug] = true
			} else if slug := dirSlug(m, paths.Expand(p.MainClone)); slug != "" {
				sc.known[slug] = true
			}
		}
	}
	for _, c := range sc.clones {
		if c.pool != nil {
			continue
		}
		matches := sc.glob(filepath.Join(globEscape(c.path+"__worktrees"), "pr-*"))
		for _, m := range matches {
			base := filepath.Base(m)
			if !perPRName.MatchString(base) {
				continue
			}
			report(m, "per-PR")
			if slug, _ := sc.readSlug(m); slug != "" {
				sc.known[slug] = true
			}
			if n, err := strconv.Atoi(strings.TrimPrefix(base, "pr-")); err == nil {
				sc.known[slots.DBSlug(slots.PRSlug(n))] = true
			}
		}
	}
}

// findOrphans reports listed databases whose slug nothing claims, only when
// every ownership fact was read (Inventory.OrphansKnown); otherwise one
// "discovery incomplete:" warning names what is missing.
func (sc *scan) findOrphans() {
	var missing []string
	if !sc.inv.DatabasesListed {
		missing = append(missing, "mysql databases not listed")
	}
	missing = append(missing, sc.incomplete...)
	if len(missing) > 0 {
		sc.warnf("discovery incomplete: %s; orphan databases not computed", strings.Join(missing, ", "))
		return
	}
	sc.inv.OrphansKnown = true
	guards := make([]mysqlx.Guard, 0, len(sc.cfg.Pools))
	for _, p := range sc.cfg.Pools {
		guards = append(guards, slots.DropGuard(p))
	}
	known := make([]string, 0, len(sc.known))
	for k := range sc.known {
		if k != "" {
			known = append(known, k)
		}
	}
	bySlug := map[string][]mysqlx.Database{}
	var order []string
	for _, d := range sc.dbs {
		if slices.ContainsFunc(known, func(k string) bool { return hasSlug(d, k) }) {
			continue
		}
		sc.inv.OrphanDBs = append(sc.inv.OrphanDBs, d)
		if _, ok := bySlug[d.Slug]; !ok {
			order = append(order, d.Slug)
		}
		bySlug[d.Slug] = append(bySlug[d.Slug], d)
	}
	for _, slug := range order {
		dbs := bySlug[slug]
		var mb float64
		safe := len(guards) > 0
		for _, d := range dbs {
			mb += d.SizeMB
			if !slices.ContainsFunc(guards, func(g mysqlx.Guard) bool { return g.Check(d.Name) == nil }) {
				safe = false
			}
		}
		sc.addFinding(KindOrphanDB, "slug:"+slug, safe, "%d database(s), %.1f MB, with slug %s belong to no slot or worktree", len(dbs), mb, slug)
	}
}

func (sc *scan) confirmStates(ctx context.Context) {
	if sc.s.GitHub == nil {
		sc.warnf("github: no client; PR states come from the registry only")
		return
	}
	byRepo := map[string][]int{} // repo -> indexes into externals
	var repos []string
	for i, ev := range sc.externals {
		if ev.PRNumber <= 0 || !strings.Contains(ev.Repo, "/") {
			continue
		}
		if _, ok := byRepo[ev.Repo]; !ok {
			repos = append(repos, ev.Repo)
		}
		byRepo[ev.Repo] = append(byRepo[ev.Repo], i)
	}
	slices.Sort(repos)
	for _, repo := range repos {
		idx := byRepo[repo]
		var numbers []int
		for _, i := range idx {
			numbers = append(numbers, sc.externals[i].PRNumber)
		}
		slices.Sort(numbers)
		numbers = slices.Compact(numbers)
		owner, name, _ := strings.Cut(repo, "/")
		states, notFound, err := sc.s.GitHub.ConfirmStates(ctx, owner, name, numbers)
		if err != nil {
			sc.warnf("github: confirm states for %s: %v", repo, err)
			continue
		}
		for _, i := range idx {
			ev := &sc.externals[i]
			if st, ok := states[ev.PRNumber]; ok {
				ev.GHState, ev.StateSource = st.State, StateSourceGitHub
				ev.MergedAt, ev.ClosedAt = timePtr(st.MergedAt), timePtr(st.ClosedAt)
				if st.HeadRefOid != "" && st.HeadRefOid == ev.Head {
					ev.PRConfirmed = true
				}
			} else if slices.Contains(notFound, ev.PRNumber) {
				ev.GHState, ev.StateSource, ev.MergedAt, ev.ClosedAt = store.GHUnknown, StateSourceGitHub, nil, nil
			}
		}
	}
}

func (sc *scan) externalDrift() {
	for _, ev := range sc.inv.External {
		if !ev.PRConfirmed || (ev.GHState != store.GHMerged && ev.GHState != store.GHClosed) {
			continue
		}
		sc.addFinding(KindExternalClosed, "path:"+ev.Path, false, "%s#%d is %s; the worktree can be reset by hand (magnum cleanup --external)", ev.Repo, ev.PRNumber, ev.GHState)
	}
}

const sizeWorkers = 4

func (sc *scan) sizes(ctx context.Context) {
	if sc.s.Runner == nil {
		sc.warnf("du: no runner; sizes skipped")
		return
	}
	type target struct {
		path string
		dst  **int64
	}
	var targets []target
	for i := range sc.inv.Slots {
		if v := &sc.inv.Slots[i]; v.Exists {
			targets = append(targets, target{v.Slot.Path, &v.SizeKB})
		}
	}
	for i := range sc.inv.External {
		if v := &sc.inv.External[i]; v.Exists {
			targets = append(targets, target{v.Path, &v.SizeKB})
		}
	}
	sem := make(chan struct{}, sizeWorkers)
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			kb, err := sc.du(ctx, t.path)
			if err != nil {
				sc.warnf("du: %s: %v", t.path, err)
				return
			}
			*t.dst = &kb
		})
	}
	wg.Wait()
}

// du returns the size of path in KiB (DiskUsageKB).
func (sc *scan) du(ctx context.Context, path string) (int64, error) {
	return DiskUsageKB(ctx, sc.s.Runner, path, "inventory size")
}

// DiskUsageKB is the size of path in KiB from `du -sk` (label names the
// command in logs). du exits non-zero on unreadable subdirectories but still
// prints the total, which is used.
func DiskUsageKB(ctx context.Context, r execx.Runner, path, label string) (int64, error) {
	res, err := r.Run(ctx, execx.Cmd{Name: "du", Args: []string{"-sk", path}, Timeout: 10 * time.Minute, Label: label})
	if kb, ok := parseDU(res.Stdout); ok {
		return kb, nil
	}
	if err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("unexpected du output %q", res.Out())
}

func (sc *scan) finish() {
	inv := &sc.inv
	slices.SortStableFunc(inv.Slots, func(a, b SlotView) int { return naturalCmp(a.Slot.Name, b.Slot.Name) })
	slices.SortStableFunc(inv.External, func(a, b ExternalView) int { return naturalCmp(a.Path, b.Path) })
	slices.SortStableFunc(inv.OrphanDBs, func(a, b mysqlx.Database) int { return strings.Compare(a.Name, b.Name) })
	slices.SortStableFunc(inv.Drift, func(a, b Finding) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		if c := naturalCmp(a.Subject, b.Subject); c != 0 {
			return c
		}
		return strings.Compare(a.Message, b.Message)
	})
	slotIdx := map[string]int{}
	for i, v := range inv.Slots {
		slotIdx["slot:"+v.Slot.Name] = i
	}
	extIdx := map[string]int{}
	for i, v := range inv.External {
		extIdx["path:"+v.Path] = i
	}
	for _, d := range inv.Drift {
		if i, ok := slotIdx[d.Subject]; ok && !slices.Contains(inv.Slots[i].Drift, d.Kind) {
			inv.Slots[i].Drift = append(inv.Slots[i].Drift, d.Kind)
		}
		if i, ok := extIdx[d.Subject]; ok && !slices.Contains(inv.External[i].Drift, d.Kind) {
			inv.External[i].Drift = append(inv.External[i].Drift, d.Kind)
		}
	}
	if inv.Slots == nil {
		inv.Slots = []SlotView{}
	}
	if inv.OrphanDBs == nil {
		inv.OrphanDBs = []mysqlx.Database{}
	}
	if inv.Drift == nil {
		inv.Drift = []Finding{}
	}
	if inv.Databases == nil {
		inv.Databases = []mysqlx.Database{}
	}
}

// isDir reports whether p is a directory. conclusive is false when the stat
// failed for another reason than absence (permission, I/O): that is warned
// about, marks ownership discovery incomplete, and dir is false.
func (sc *scan) isDir(p string) (dir, conclusive bool) {
	if p == "" {
		return false, true
	}
	fi, err := os.Stat(p)
	switch {
	case err == nil:
		return fi.IsDir(), true
	case absent(err):
		return false, true
	}
	sc.warnf("fs: %v", err)
	sc.incompletef("stat of %s failed", p)
	return false, false
}

// readSlug returns dir's slug marker (slots.MarkerFile), "" when there is none.
// conclusive is false when the marker exists but could not be read
// (permission, I/O) or is not read (slots.ReadMarker: a symlink, a special
// file or one past its cap, as the checkout's code may leave): that is warned
// about and marks ownership discovery incomplete.
func (sc *scan) readSlug(dir string) (slug string, conclusive bool) {
	slug, err := slots.ReadMarker(dir)
	switch {
	case err == nil:
		return slug, true
	case absent(err):
		return "", true
	}
	sc.warnf("fs: %v", err)
	sc.incompletef("slug marker of %s unreadable", dir)
	return "", false
}

// glob is filepath.Glob that does not hide an unreadable directory:
// filepath.Glob skips I/O errors, so the directory holding the pattern's
// first wildcard is read once more, and a failure other than absence is
// warned about and marks ownership discovery incomplete (directories there
// may own databases).
func (sc *scan) glob(pattern string) []string {
	matches, _ := filepath.Glob(pattern) // the only error is a malformed pattern
	static := pattern
	if i := strings.IndexAny(pattern, "*?["); i >= 0 {
		static = filepath.Dir(pattern[:i+1])
	}
	if _, err := os.ReadDir(static); err != nil && !absent(err) {
		sc.warnf("fs: %v", err)
		sc.incompletef("directory %s unreadable", static)
	}
	return matches
}

// absent reports whether err says the path is not there: it does not exist,
// or a parent is not a directory.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
