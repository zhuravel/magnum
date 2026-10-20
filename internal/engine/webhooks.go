package engine

// The webhooks consumer in shadow mode ([webhooks] mode = "shadow", phase 1
// of acting on the GitHub App's webhook deliveries): a goroutine reads the
// pages of magnum-relay's journal by cursor and turns each event into hints,
// the PR or the head it says changed; each poll of a covered owner then
// compares what it saw change with the hints, and the totals say how often
// the events announced the change, missed it, or came with no change at all.
// It changes nothing the daemon does: it never writes a PR, slot or request
// row, starts nothing and reads nothing from GitHub. Its own rows are three
// kv keys and webhooks.* events, none on a PR's subject (the CLI reads a
// PR's latest events by window).

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/webhooks"
)

// WebhookSource reads the relay's events (*webhooks.Client): the page of at
// most limit events after the cursor after ("" = from the oldest kept),
// waiting up to wait for one.
type WebhookSource interface {
	Events(ctx context.Context, after string, limit int, wait time.Duration) (webhooks.Page, error)
}

var _ WebhookSource = (*webhooks.Client)(nil)

// The consumer's kv keys: the cursor it reads after (webhooksCursor), the
// stream's health (WebhooksHealth) and the hourly totals (WebhooksHour).
const (
	KVWebhooksCursor = "webhooks.cursor"
	KVWebhooksHealth = "webhooks.health"
	KVWebhooksHours  = "webhooks.hours"
)

const (
	// webhooksWindow: a hint announces a poll change it came at most this
	// long before (or since the repository's previous poll, when that is
	// longer ago); a hint no poll change followed within it is early or
	// noise. An event the relay received longer than this before the page
	// that lists it (a backlog after a sleep or an outage) hints at nothing.
	webhooksWindow = 5 * time.Minute
	// webhooksLate: a hint that came at most this long after the poll saw
	// the change announces it too (a delivery a few seconds behind the
	// radar's read); a change with no hint by then is missed.
	webhooksLate = time.Minute
	// webhooksHintAge: a hint no poll of its repository was compared with
	// for this long (its radar failing) is early or noise, and a repository
	// not compared for this long no longer holds a gap open.
	webhooksHintAge = time.Hour
	// WebhooksKeepHours is how many hourly totals are kept.
	WebhooksKeepHours = 24
	// webhooksPageLimit is the most events one page asks for.
	webhooksPageLimit = 200
	// webhooksSeenMax is how many delivery ids the dedupe remembers.
	webhooksSeenMax = 10000
	// webhooksMaxBackoff bounds the wait after failed pages and gaps.
	webhooksMaxBackoff = 2 * time.Minute
	// webhooksQuiet is how long the loop waits after an empty page: a
	// relay's long poll answers one only after its wait, and one that
	// answers at once then costs a curl a second, not a busy loop.
	webhooksQuiet = time.Second
	// webhooksLastAge is how long the last event of a PR or a head is kept
	// for a missed change's event.
	webhooksLastAge = 24 * time.Hour
)

// webhooksCursor is KVWebhooksCursor's value: the relay's journal and the
// cursor the next page is read after.
type webhooksCursor struct {
	Epoch  string `json:"epoch"`
	Cursor string `json:"cursor"`
}

// WebhooksHealth is the stream's state as the consumer recorded it
// (KVWebhooksHealth): when the relay last answered a page, the last failure
// and its cause while no page answered since, and a gap the polls have not
// compared after yet.
type WebhooksHealth struct {
	PageAt  *time.Time `json:"page_at,omitempty"`
	Error   string     `json:"error,omitempty"`
	ErrorAt *time.Time `json:"error_at,omitempty"`
	// GapAt is the last gap (the relay no longer held the events after the
	// cursor, which then restarted at its newest event); GapOpen holds the
	// stream unhealthy until every repository of a covered owner was
	// compared after it, as each one's next poll is what resyncs it.
	GapAt   *time.Time `json:"gap_at,omitempty"`
	GapOpen bool       `json:"gap_open,omitempty"`
}

// State is whether the stream is healthy at now and, when not, why: a page
// that failed since the last answered one, a gap the polls have not
// resynced yet, or no page within staleAfter.
func (h WebhooksHealth) State(now time.Time, staleAfter time.Duration) (healthy bool, cause string) {
	switch {
	case h.Error != "" && h.ErrorAt != nil && (h.PageAt == nil || !h.PageAt.After(*h.ErrorAt)):
		return false, h.Error
	case h.GapOpen && h.GapAt != nil:
		return false, "gap at " + h.GapAt.Local().Format("15:04") + ": the relay lost events after the cursor; the next polls resync"
	case h.PageAt == nil:
		return false, "no page from the relay yet"
	case now.Sub(*h.PageAt) > staleAfter:
		return false, fmt.Sprintf("no page from the relay for %s", now.Sub(*h.PageAt).Round(time.Second))
	}
	return true, ""
}

// WebhooksHour is one hour's totals (KVWebhooksHours keeps the last
// WebhooksKeepHours, oldest first): the events read, the poll changes
// compared and how they came out (announced + missed = changes), the events
// no change followed and the gaps. A change counts in the hour it was
// decided.
type WebhooksHour struct {
	Hour      time.Time `json:"hour"`
	Events    int       `json:"events"`
	Changes   int       `json:"changes"`
	Announced int       `json:"announced"`
	Missed    int       `json:"missed"`
	Early     int       `json:"early"`
	Gaps      int       `json:"gaps"`
}

// ReadWebhooks reads what the consumer recorded: the stream's health and
// the hourly totals, oldest first (zero and nil when nothing was).
func ReadWebhooks(ctx context.Context, st *store.Store) (WebhooksHealth, []WebhooksHour) {
	var h WebhooksHealth
	var hours []WebhooksHour
	if v, ok, err := st.GetKV(ctx, KVWebhooksHealth); err == nil && ok {
		_ = json.Unmarshal([]byte(v), &h) // a malformed row reads as none
	}
	if v, ok, err := st.GetKV(ctx, KVWebhooksHours); err == nil && ok {
		_ = json.Unmarshal([]byte(v), &hours)
	}
	return h, hours
}

// hookKey is what a hint names: a PR of a repository (pr) or a head
// (sha), the repository in lower case.
type hookKey struct {
	repo string
	pr   int
	sha  string
}

// hookHint is one event of a covered repository: the keys it names, when
// the consumer read it, whether a poll change matched it (so it was no
// early or noise event), and the PRs whose change it announced: an event
// that names several PRs (a check run of two, a status on a head two PRs
// share) announces each one once.
type hookHint struct {
	repo      string
	keys      []hookKey
	at        time.Time
	matched   bool
	announced map[int]bool
}

// names reports whether the hint names one of keys and has not announced
// a change of PR number yet.
func (h *hookHint) names(number int, keys []hookKey) bool {
	return !h.announced[number] && slices.ContainsFunc(h.keys, func(k hookKey) bool { return slices.Contains(keys, k) })
}

// announce marks the hint as announcing PR number's change.
func (h *hookHint) announce(number int) {
	if h.announced == nil {
		h.announced = map[int]bool{}
	}
	h.announced[number], h.matched = true, true
}

// hookChange is a poll change no hint announced yet: it waits until until
// for a late one.
type hookChange struct {
	owner     string
	full      string
	number    int
	keys      []hookKey
	what      []string
	at, until time.Time
}

// hookSeen is the last event read for a PR or a head: its name, when the
// consumer read it and when the relay received it.
type hookSeen struct {
	name     string
	at       time.Time
	received time.Time
}

// hookPRState is what the comparison last counted of a PR (by node id).
// exempt marks an open PR a poll that counted nothing (an unhealthy stream,
// a gap's hold) saw leave the open list: its close, which GitHub confirms
// only after that poll, belongs to the same uncounted change.
type hookPRState struct {
	repo    string
	head    string
	draft   bool
	updated time.Time
	open    bool
	exempt  bool
}

// hookNote is an event the consumer or the comparison writes once it holds
// no lock.
type hookNote struct {
	level, subject, kind, msg string
	data                      map[string]any
}

// webhookState is the consumer's memory, shared by its goroutine and the
// tick's (the comparison) under mu; on says the consumer runs, so the
// comparison runs only then. Neither writes the registry: each marks what
// changed (dirty) for the writer goroutine (webhooksWriter), so a slow
// write never holds a page or a poll.
type webhookState struct {
	on    atomic.Bool
	dirty chan struct{}
	// flushMu orders the kv writes (flushWebhooks) and guards written, the
	// values last written; it is taken before mu, never after. setKV, when
	// set, writes a kv row in place of the registry (tests).
	flushMu sync.Mutex
	written map[string]string
	setKV   func(ctx context.Context, key, value string) error

	mu       sync.Mutex
	cursor   webhooksCursor
	seen     map[string]bool
	seenRing []string // the last webhooksSeenMax ids, seenNext the oldest's place once full
	seenNext int
	hints    []*hookHint
	pending  []hookChange
	last     map[hookKey]hookSeen
	pruned   time.Time
	memo     map[string]hookPRState
	compared map[string]time.Time
	hours    []WebhooksHour
	health   WebhooksHealth
	cause    string // the class of the failure last said (webhooks.unhealthy); "" while healthy
	gapRun   int    // gaps in a row, each waited for longer
	busyRun  int    // busy answers (429) in a row, likewise
	notes    []hookNote
}

// with runs fn holding mu, which a panic in fn releases too.
func (hk *webhookState) with(fn func()) {
	hk.mu.Lock()
	defer hk.mu.Unlock()
	fn()
}

// webhookSource is the App's relay client in shadow mode (FromApp); nil
// otherwise, or in a dry run or an eval's App.
func webhookSource(a *app.App) WebhookSource {
	if a.Config == nil || !a.Config.Webhooks.Shadow() || a.Runner == nil || a.DryRun || a.AgentTag != "" {
		return nil
	}
	return &webhooks.Client{Runner: a.Runner, URL: a.Config.Webhooks.URL, TokenFile: a.Config.Webhooks.TokenFile}
}

// goWebhooks starts the consumer for Run, when shadow mode has a source
// (never for --once or a dry run): it reads what an earlier run recorded,
// then reads pages on a goroutine of its own and writes what it and the
// polls' comparison recorded on another, until ctx ends. The channel closes
// once both have ended (at once when they do not run).
func (e *Engine) goWebhooks(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	if !e.cfg.Webhooks.Shadow() || e.d.Webhooks == nil || e.d.DryRun {
		close(done)
		return done
	}
	e.loadWebhooks(ctx)
	e.hooks.on.Store(true)
	var wg sync.WaitGroup
	wg.Go(func() {
		defer e.hooks.on.Store(false)
		e.safely(ctx, "webhooks consumer", "", func() { e.webhooksLoop(ctx) }, nil)
	})
	wg.Go(func() { e.safely(ctx, "webhooks writer", "", func() { e.webhooksWriter(ctx) }, nil) })
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

// webhooksFinalFlush bounds the last write when the daemon stops.
const webhooksFinalFlush = 5 * time.Second

// webhooksWriter writes what the consumer and the comparison recorded each
// time they mark it changed, and once more, within webhooksFinalFlush, when
// ctx ends, so the last cursor read is the one kept.
func (e *Engine) webhooksWriter(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), webhooksFinalFlush)
			e.flushWebhooks(fctx)
			cancel()
			return
		case <-e.hooks.dirty:
			e.flushWebhooks(ctx)
		}
	}
}

// changed tells the writer there is something to write, never waiting.
func (hk *webhookState) changed() {
	select {
	case hk.dirty <- struct{}{}:
	default: // already told; a nil channel (no writer) drops it
	}
}

// loadWebhooks reads the cursor and the totals an earlier run kept.
func (e *Engine) loadWebhooks(ctx context.Context) {
	hk := &e.hooks
	var cur webhooksCursor
	if v, ok := e.getKV(ctx, KVWebhooksCursor); ok {
		_ = json.Unmarshal([]byte(v), &cur) // malformed: start again at the newest event
	}
	health, hours := ReadWebhooks(ctx, e.st)
	hk.flushMu.Lock()
	hk.written = map[string]string{}
	hk.flushMu.Unlock()
	if hk.dirty == nil {
		hk.dirty = make(chan struct{}, 1)
	}
	hk.with(func() {
		hk.cursor, hk.health, hk.hours = cur, health, hours
		hk.health.GapOpen = false // this run's polls compare after it
		if health.Error != "" {
			hk.cause = failureClass(health.Error)
		}
		hk.trimHours(e.now())
	})
}

// webhooksLoop reads pages until ctx ends, waiting after a failed one (1 s,
// doubling up to webhooksMaxBackoff), after a gap (likewise) and a second
// after an empty one.
func (e *Engine) webhooksLoop(ctx context.Context) {
	owners := strings.Join(e.cfg.Webhooks.Owners, ", ")
	if owners == "" {
		owners = "no owner (webhooks.owners is empty)"
	}
	e.event(ctx, "info", "", "webhooks.shadow", "reading the relay's events in shadow mode, compared with the polls of "+owners+"; changing nothing", nil)
	fails := 0
	for ctx.Err() == nil {
		ok, pause := e.webhooksStep(ctx)
		if ok {
			fails = 0
		} else {
			fails++
			pause = webhooksBackoff(fails)
		}
		if pause > 0 && e.d.Sleep(ctx, pause) != nil {
			return
		}
	}
}

// webhooksBackoff is the wait after n failures or gaps in a row: 1 s,
// doubling up to webhooksMaxBackoff.
func webhooksBackoff(n int) time.Duration {
	return min(time.Second<<min(max(n-1, 0), 7), webhooksMaxBackoff)
}

// webhooksStep reads one page after the kept cursor and takes its events,
// or, without a cursor (a first start, a gap), moves the cursor to the
// relay's newest event: the events before it are history the polls already
// saw. It reports whether the relay answered, and how long to wait before
// the next page (webhooksQuiet after an empty one, a backoff after a gap or
// a busy relay's 429).
func (e *Engine) webhooksStep(ctx context.Context) (ok bool, pause time.Duration) {
	hk := &e.hooks
	var after string
	hk.with(func() { after = hk.cursor.Cursor })
	src := e.d.Webhooks
	if after == "" {
		page, err := src.Events(ctx, "", 1, 0)
		if err != nil {
			e.webhooksFailed(ctx, err)
			return false, 0
		}
		e.webhooksRestart(page)
		return true, 0
	}
	page, err := src.Events(ctx, after, webhooksPageLimit, e.cfg.Webhooks.Wait.Duration)
	if err == nil {
		e.webhooksPage(page)
		if len(page.Events) == 0 {
			return true, webhooksQuiet
		}
		return true, 0
	}
	var gap *webhooks.GapError
	switch {
	case ctx.Err() != nil:
	case errors.As(err, &gap):
		return true, e.webhooksGap(err)
	case errors.Is(err, webhooks.ErrBusy):
		// Too many long polls wait at the relay: another try a second
		// later, then longer; no failure of the stream, which goes stale
		// only when the relay stays busy past stale_after.
		var run int
		hk.with(func() { hk.busyRun++; run = hk.busyRun })
		return true, webhooksBackoff(run)
	}
	e.webhooksFailed(ctx, err)
	return false, 0
}

// webhooksRestart puts the cursor at the newest event of a page read from
// the start.
func (e *Engine) webhooksRestart(page webhooks.Page) {
	now := e.now()
	hk := &e.hooks
	hk.with(func() {
		hk.cursor = webhooksCursor{Epoch: page.Epoch, Cursor: cmp.Or(page.HighWater, page.Next)}
		hk.answered(now)
		hk.notes = append(hk.notes, hookNote{level: "info", kind: "webhooks.cursor", msg: "the relay's events are read from its newest one on",
			data: map[string]any{"cursor": hk.cursor.Cursor}})
	})
	hk.changed()
}

// webhooksPage takes a page's events: each delivery once, and each event of
// a covered repository the relay received within webhooksWindow before the
// page (by its own clock) a hint, which also settles a change the poll saw
// just before it; then the cursor moves past them.
func (e *Engine) webhooksPage(page webhooks.Page) {
	now := e.now()
	relayNow := page.ServerTime
	if relayNow.IsZero() {
		relayNow = now
	}
	hk := &e.hooks
	hk.with(func() {
		for _, ev := range page.Events {
			if hk.dupe(ev.DeliveryID) {
				continue
			}
			hk.hour(now).Events++
			keys := e.hintKeys(ev)
			if len(keys) == 0 || (!ev.ReceivedAt.IsZero() && relayNow.Sub(ev.ReceivedAt) > webhooksWindow) {
				continue // no hint: another repository, or a backlog too old to announce what a poll sees now
			}
			h := &hookHint{repo: keys[0].repo, keys: keys, at: now}
			for _, k := range keys {
				hk.lastOf()[k] = hookSeen{name: ev.Name(), at: now, received: ev.ReceivedAt}
			}
			hk.lateHint(h, now)
			hk.hints = append(hk.hints, h)
		}
		hk.cursor = webhooksCursor{Epoch: page.Epoch, Cursor: page.Next}
		hk.gapRun, hk.busyRun = 0, 0
		hk.answered(now)
		hk.sweep(now)
	})
	hk.changed()
}

// webhooksGap records a gap: the relay lost the events after the cursor,
// which starts again at its newest event; the stream stays unhealthy until
// every repository of a covered owner was compared after it. It returns the
// wait before the next page: one gap after another waits longer each time.
func (e *Engine) webhooksGap(err error) time.Duration {
	now := e.now()
	hk := &e.hooks
	var pause time.Duration
	hk.with(func() {
		hk.cursor = webhooksCursor{}
		hk.health.GapAt, hk.health.GapOpen = &now, true
		hk.hour(now).Gaps++
		hk.gapRun++
		pause = webhooksBackoff(hk.gapRun)
		hk.notes = append(hk.notes, hookNote{level: "warn", kind: "webhooks.gap", msg: err.Error() + "; the cursor starts again at the relay's newest event"})
	})
	hk.changed()
	return pause
}

// webhooksFailed records a page that failed: the stream's cause, said once
// while failures of its class last (a stop is no failure).
func (e *Engine) webhooksFailed(ctx context.Context, err error) {
	if isStop(ctx, err) {
		return
	}
	now := e.now()
	hk := &e.hooks
	hk.with(func() {
		cause := err.Error()
		hk.health.Error, hk.health.ErrorAt = cause, &now
		if class := failureClass(cause); class != hk.cause {
			hk.cause = class
			hk.notes = append(hk.notes, hookNote{level: "warn", kind: "webhooks.unhealthy", msg: "the relay's events cannot be read: " + cause})
		}
		hk.sweep(now)
	})
	hk.changed()
}

// digitsRe matches what differs between two failures of one kind: curl's
// times ("timed out after 35002 milliseconds"), ports and addresses.
var digitsRe = regexp.MustCompile(`[0-9]+`)

// failureClass is a failure's text with its numbers taken out, so a cause
// is said once while it repeats.
func failureClass(cause string) string { return digitsRe.ReplaceAllString(cause, "#") }

// answered records a page the relay answered (hk.mu held).
func (hk *webhookState) answered(now time.Time) {
	hk.health.PageAt = &now
	hk.health.Error, hk.health.ErrorAt = "", nil
	if hk.cause != "" {
		hk.cause = ""
		hk.notes = append(hk.notes, hookNote{level: "info", kind: "webhooks.healthy", msg: "the relay's events are read again"})
	}
}

// hintKeys are the keys an event names when a covered owner's watched
// repository is its: each PR it names, and the head of a check or a status
// (a fork's check names no PR). Any other event is no hint.
func (e *Engine) hintKeys(ev webhooks.Event) []hookKey {
	owner, _, ok := strings.Cut(ev.Repository, "/")
	if !ok || !e.cfg.Webhooks.Covers(owner) {
		return nil
	}
	if w := e.cfg.WatchFor(ev.Repository); w == nil || !strings.EqualFold(w.Owner, owner) {
		return nil
	}
	repo := strings.ToLower(ev.Repository)
	var keys []hookKey
	for _, n := range ev.PRNumbers {
		if n > 0 {
			keys = append(keys, hookKey{repo: repo, pr: n})
		}
	}
	switch ev.Event {
	case "check_run", "check_suite", "status":
		if sha := strings.ToLower(ev.HeadSHA); sha != "" {
			keys = append(keys, hookKey{repo: repo, sha: sha})
		}
	}
	return keys
}

// webhooksSaw compares one repository's poll with the hints, for a covered
// owner while the consumer runs (the poll's only hook, before it applies
// the radar): each PR the radar shows changed since the comparison last
// counted it (its head, draft, a newer updatedAt, a reopen, a new PR) or
// that left the open list and GitHub confirmed closed is announced when a
// hint for it came within webhooksWindow before (or since the repository's
// previous poll), else waits webhooksLate for a late one. A repository's
// first sync counts nothing, and neither does a poll while the stream is
// unhealthy (WebhooksHealth.State) or the repository's first poll after a
// gap: what changed during an outage or a gap is the health line's, not the
// events' miss. It reads only what the poll already read and writes
// nothing: it marks the consumer's own kv rows and events for the writer
// (webhooksWriter), so a slow registry never holds the poll; a panic in it
// is recorded, never the poll's.
func (e *Engine) webhooksSaw(ctx context.Context, w config.Watch, full string, radar []github.PRRadar, stored []store.PR, firstSync bool) {
	hk := &e.hooks
	if !hk.on.Load() || !e.cfg.Webhooks.Covers(w.Owner) {
		return
	}
	e.safely(ctx, "webhooks comparison", "", func() {
		now := e.now()
		hk.with(func() { hk.compare(w.Owner, full, radar, stored, firstSync, now, e.cfg.Webhooks.StaleAfter.Duration) })
		hk.changed()
	}, nil)
}

// compare is webhooksSaw's work (hk.mu held).
func (hk *webhookState) compare(owner, full string, radar []github.PRRadar, stored []store.PR, firstSync bool, now time.Time, staleAfter time.Duration) {
	repo := strings.ToLower(full)
	prev, compared := hk.compared[repo]
	health := hk.health
	// A gap holds each repository until its own first poll after it.
	health.GapOpen = health.GapOpen && health.GapAt != nil && (!compared || !prev.After(*health.GapAt))
	healthy, _ := health.State(now, staleAfter)
	changes := hk.diff(full, radar, stored, firstSync, healthy)
	from := now.Add(-webhooksWindow)
	if compared && prev.Before(from) {
		from = prev
	}
	if hk.compared == nil {
		hk.compared = map[string]time.Time{}
	}
	hk.compared[repo] = now
	hk.expire(repo, from, now)
	for _, c := range changes {
		if !healthy {
			break // an outage's changes are the health line's, not misses
		}
		c.owner, c.at, c.until = owner, now, now.Add(webhooksLate)
		if hk.match(c, from, now) {
			hk.decided(now, true)
			continue
		}
		hk.pending = append(hk.pending, c)
	}
	hk.closeGap()
	hk.sweep(now)
}

// closeGap ends a gap's hold once every repository compared within
// webhooksHintAge before it was compared after it (hk.mu held).
func (hk *webhookState) closeGap() {
	h := &hk.health
	if !h.GapOpen || h.GapAt == nil {
		return
	}
	for _, at := range hk.compared {
		if !at.After(*h.GapAt) && h.GapAt.Sub(at) < webhooksHintAge {
			return
		}
	}
	h.GapOpen = false
}

// diff lists the changes of one repository's radar against what the
// comparison last counted of each PR, or the stored row it has not counted
// yet (a restart), and remembers the radar's values (hk.mu held). Only a
// newer updatedAt counts (a stale answer is no change and leaves the newest
// kept). A PR that left the open list counts once GitHub confirmed it closed
// (missing_since set), so a PR the radar's list skipped once counts nothing;
// one a poll that counts nothing (counting false) saw leave it stays
// uncounted when GitHub confirms the close at a later poll (exempt).
func (hk *webhookState) diff(full string, radar []github.PRRadar, stored []store.PR, firstSync, counting bool) []hookChange {
	if hk.memo == nil {
		hk.memo = map[string]hookPRState{}
	}
	repo := strings.ToLower(full)
	byNode := make(map[string]store.PR, len(stored))
	for _, p := range stored {
		byNode[p.NodeID] = p
	}
	var out []hookChange
	add := func(number int, head string, what ...string) {
		keys := []hookKey{{repo: repo, pr: number}}
		if head != "" {
			keys = append(keys, hookKey{repo: repo, sha: head})
		}
		out = append(out, hookChange{full: full, number: number, keys: keys, what: what})
	}
	inRadar := make(map[string]bool, len(radar))
	for _, p := range radar {
		inRadar[p.NodeID] = true
		cur := hookPRState{repo: repo, head: strings.ToLower(p.HeadRefOid), draft: p.IsDraft, updated: p.UpdatedAt, open: true}
		prev, known := hk.memo[p.NodeID]
		if !known {
			if row, ok := byNode[p.NodeID]; ok {
				prev, known = hookStateOf(repo, row), true
			}
		}
		newer := !prev.updated.IsZero() && cur.updated.After(prev.updated)
		if known && !cur.updated.After(prev.updated) {
			cur.updated = prev.updated
		}
		hk.memo[p.NodeID] = cur
		if firstSync {
			continue
		}
		if !known {
			add(p.Number, cur.head, "new")
			continue
		}
		var what []string
		if !prev.open {
			what = append(what, "state")
		}
		if prev.head != cur.head {
			what = append(what, "head")
		}
		if prev.draft != cur.draft {
			what = append(what, "draft")
		}
		if newer {
			what = append(what, "updated")
		}
		if len(what) > 0 {
			add(p.Number, cur.head, what...)
		}
	}
	for _, p := range stored {
		if inRadar[p.NodeID] || p.GHState != store.GHOpen {
			continue
		}
		prev, known := hk.memo[p.NodeID]
		if !known {
			// Not counted in this run: one GitHub confirmed closed before
			// it is history, one not confirmed yet counts when it is.
			prev = hookStateOf(repo, p)
			prev.open = p.MissingSince == nil
			prev.exempt = !counting
			hk.memo[p.NodeID] = prev
			continue
		}
		if !prev.open {
			continue
		}
		if p.MissingSince == nil {
			// Gone from the list, not confirmed closed yet: a poll that
			// counts nothing makes its close uncounted too.
			if !counting && !prev.exempt {
				prev.exempt = true
				hk.memo[p.NodeID] = prev
			}
			continue
		}
		exempt := prev.exempt
		prev.open, prev.exempt = false, false
		hk.memo[p.NodeID] = prev
		if !firstSync && !exempt {
			add(p.Number, prev.head, "state")
		}
	}
	// What neither the radar nor the open rows hold any longer is gone.
	for node, s := range hk.memo {
		if row, ok := byNode[node]; s.repo == repo && !inRadar[node] && (!ok || row.GHState != store.GHOpen) {
			delete(hk.memo, node)
		}
	}
	return out
}

// hookStateOf is a stored row as the comparison counts it.
func hookStateOf(repo string, p store.PR) hookPRState {
	s := hookPRState{repo: repo, head: strings.ToLower(p.HeadSHA), draft: p.IsDraft, open: p.GHState == store.GHOpen}
	if p.GHUpdatedAt != nil {
		s.updated = *p.GHUpdatedAt
	}
	return s
}

// match marks the hints that came between from and to, name the change's
// PR or head and announced no change of that PR yet, as announcing it, and
// reports whether there was one (hk.mu held).
func (hk *webhookState) match(c hookChange, from, to time.Time) bool {
	found := false
	for _, h := range hk.hints {
		if h.at.Before(from) || h.at.After(to) || !h.names(c.number, c.keys) {
			continue
		}
		h.announce(c.number)
		found = true
	}
	return found
}

// lateHint settles the waiting changes a hint read at now names, one per PR
// as match does: each is announced (hk.mu held).
func (hk *webhookState) lateHint(h *hookHint, now time.Time) {
	kept := hk.pending[:0]
	for _, c := range hk.pending {
		if !now.After(c.until) && h.names(c.number, c.keys) {
			h.announce(c.number)
			hk.decided(now, true)
			continue
		}
		kept = append(kept, c)
	}
	clear(hk.pending[len(kept):])
	hk.pending = kept
}

// expire drops repo's hints from before from, the window of the poll just
// compared, which no later poll can match: early or noise unless matched
// (hk.mu held).
func (hk *webhookState) expire(repo string, from, now time.Time) {
	kept := hk.hints[:0]
	for _, h := range hk.hints {
		if h.repo == repo && h.at.Before(from) {
			if !h.matched {
				hk.hour(now).Early++
			}
			continue
		}
		kept = append(kept, h)
	}
	clear(hk.hints[len(kept):])
	hk.hints = kept
}

// sweep decides the changes whose wait for a late hint is over (missed,
// each with a webhooks.missed event at debug level naming the PR, what
// changed and the last event read for it), drops the hints no poll
// compared for webhooksHintAge, and forgets the last events older than
// webhooksLastAge once an hour (hk.mu held).
func (hk *webhookState) sweep(now time.Time) {
	kept := hk.pending[:0]
	for _, c := range hk.pending {
		if !now.After(c.until) {
			kept = append(kept, c)
			continue
		}
		hk.decided(now, false)
		hk.notes = append(hk.notes, hk.missedNote(c))
	}
	clear(hk.pending[len(kept):])
	hk.pending = kept
	hints := hk.hints[:0]
	for _, h := range hk.hints {
		if now.Sub(h.at) > webhooksHintAge {
			if !h.matched {
				hk.hour(now).Early++
			}
			continue
		}
		hints = append(hints, h)
	}
	clear(hk.hints[len(hints):])
	hk.hints = hints
	if now.Sub(hk.pruned) >= time.Hour {
		hk.pruned = now
		for k, s := range hk.last {
			if now.Sub(s.at) > webhooksLastAge {
				delete(hk.last, k)
			}
		}
	}
	hk.trimHours(now)
}

// missedNote is the webhooks.missed event of a change no hint announced:
// the PR, what changed and the last event read for it or its head. Its
// subject is the watch's, never the PR's: the CLI reads a PR's latest
// events by window (a fix's or a whiteboard's wait), which these must not
// crowd.
func (hk *webhookState) missedNote(c hookChange) hookNote {
	ref := fmt.Sprintf("%s#%d", c.full, c.number)
	data := map[string]any{"pr": ref, "what": c.what, "seen_at": c.at.UTC().Format(time.RFC3339)}
	last := "no event for it in the last 24h"
	for _, k := range c.keys {
		if s, ok := hk.last[k]; ok {
			last = fmt.Sprintf("the last event for it: %s, read %s", s.name, s.at.Local().Format("15:04:05"))
			data["last_event"], data["last_event_at"] = s.name, s.at.UTC().Format(time.RFC3339)
			if !s.received.IsZero() {
				data["last_event_received_at"] = s.received.UTC().Format(time.RFC3339)
			}
			break
		}
	}
	msg := fmt.Sprintf("the poll saw %s change (%s) with no webhook event within %s before it or %s after; %s",
		ref, strings.Join(c.what, ", "), webhooksWindow, webhooksLate, last)
	return hookNote{level: "debug", subject: "watch:" + strings.ToLower(c.owner), kind: "webhooks.missed", msg: msg, data: data}
}

// decided counts a poll change announced or missed (hk.mu held).
func (hk *webhookState) decided(now time.Time, announced bool) {
	h := hk.hour(now)
	h.Changes++
	if announced {
		h.Announced++
	} else {
		h.Missed++
	}
}

// dupe reports whether the delivery was read already, remembering the last
// webhooksSeenMax ids in a ring (hk.mu held); a delivery without an id is
// never one.
func (hk *webhookState) dupe(id string) bool {
	if id == "" {
		return false
	}
	if hk.seen == nil {
		hk.seen = map[string]bool{}
	}
	if hk.seen[id] {
		return true
	}
	hk.seen[id] = true
	if len(hk.seenRing) < webhooksSeenMax {
		hk.seenRing = append(hk.seenRing, id)
		return false
	}
	delete(hk.seen, hk.seenRing[hk.seenNext])
	hk.seenRing[hk.seenNext] = id
	hk.seenNext = (hk.seenNext + 1) % webhooksSeenMax
	return false
}

// lastOf is hk.last, made on first use (hk.mu held).
func (hk *webhookState) lastOf() map[hookKey]hookSeen {
	if hk.last == nil {
		hk.last = map[hookKey]hookSeen{}
	}
	return hk.last
}

// hour is the totals of now's hour, kept in order of their hours even
// after the clock stepped back (hk.mu held).
func (hk *webhookState) hour(now time.Time) *WebhooksHour {
	at := now.UTC().Truncate(time.Hour)
	i, found := slices.BinarySearchFunc(hk.hours, at, func(h WebhooksHour, t time.Time) int { return h.Hour.Compare(t) })
	if !found {
		hk.hours = slices.Insert(hk.hours, i, WebhooksHour{Hour: at})
	}
	return &hk.hours[i]
}

// trimHours keeps the totals of the last WebhooksKeepHours hours (hk.mu
// held).
func (hk *webhookState) trimHours(now time.Time) {
	floor := now.UTC().Truncate(time.Hour).Add(-(WebhooksKeepHours - 1) * time.Hour)
	hk.hours = slices.DeleteFunc(hk.hours, func(h WebhooksHour) bool { return h.Hour.Before(floor) })
}

// flushWebhooks writes the consumer's kv rows that changed since they were
// last written and the events it queued, holding mu only to read them: the
// writer's work (webhooksWriter), never the poll's or a page's. flushMu
// keeps the writes in the order they were read, so an older value never
// lands last, and a value counts as written only once its write took (a
// write cut short by the stop is made again by the final flush).
func (e *Engine) flushWebhooks(ctx context.Context) {
	hk := &e.hooks
	hk.flushMu.Lock()
	defer hk.flushMu.Unlock()
	if hk.written == nil {
		hk.written = map[string]string{}
	}
	setKV := hk.setKV
	if setKV == nil {
		setKV = e.st.SetKV
	}
	var values map[string]any
	var notes []hookNote
	hk.with(func() {
		values = map[string]any{KVWebhooksCursor: hk.cursor, KVWebhooksHealth: hk.health, KVWebhooksHours: slices.Clone(hk.hours)}
		notes, hk.notes = hk.notes, nil
	})
	for key, v := range values {
		b, err := json.Marshal(v)
		if err != nil || hk.written[key] == string(b) {
			continue
		}
		if err := setKV(ctx, key, string(b)); err != nil {
			e.warnUnlessStopped(ctx, err, "kv write", "key", key)
			continue
		}
		hk.written[key] = string(b)
	}
	for _, n := range notes {
		e.event(ctx, n.level, n.subject, n.kind, n.msg, n.data)
	}
}
