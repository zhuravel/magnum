package engine

// The GitHub cost meter and the shared floor (DECISIONS "A GitHub cost meter
// per call and identity, and a shared floor"). On 2026-10-20 the operator's
// gh login ran out of its GraphQL budget (5,000 points an hour, shared by
// every tool on that login) from 16:43 to 16:56: the radar poll failed, and
// magnum could not say who spent it, since it kept only the radar's last
// rate limit and only the poll held back below a floor.
//
// Every gh api command of the daemon's GitHub clients tells the meter what
// it cost (github.Client.Meter, wired by FromApp): a GraphQL call its points
// and the rate limit GitHub answered, a REST call one request. The meter
// adds them up in memory per identity, per op (the label the caller put
// into the call's context with github.WithOp, else the call's own kind) and
// per clock hour, keeps the last 24 hours, and writes each identity's
// record (KVGitHubCost) at most once a minute, for `magnum status`, and a
// run of low budget (KVGitHubLow) when it begins or ends. It
// keeps each identity's latest rate limit as well, which the floor reads:
// below [github] budget_floor an identity's budget holds the new work that
// reads as it (githubLow): a round's start, a fix job, a whiteboard, the
// retro and a notes curation wait for its reset, while a running round
// finishes and the poll keeps its own rateFloor. The meter is a hook on
// every call, so it only takes a lock and adds; the store sees it once a
// minute, from the tick.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zhuravel/magnum/internal/github"
)

// KVGitHubCost holds the cost record of one identity's GitHub calls
// (GitHubCost as JSON).
func KVGitHubCost(identity string) string { return "github.cost." + identity }

// kvGitHubCostGlob matches every identity's KVGitHubCost.
const kvGitHubCostGlob = "github.cost.*"

// githubCostHours is how many clock hours a cost record keeps, the current
// one included.
const githubCostHours = 24

// githubCostFlush is how often at most the meter writes a cost record.
const githubCostFlush = time.Minute

// GitHubCost is the meter's record of one identity (KVGitHubCost).
type GitHubCost struct {
	Identity string `json:"identity"`
	// Rate is the identity's latest GraphQL rate limit as any call of the
	// daemon answered it (nil before the first).
	Rate *GitHubRate `json:"rate,omitempty"`
	// Hours are the clock hours with calls among the last 24, oldest first.
	Hours []GitHubHour `json:"hours"`
}

// GitHubRate is a GraphQL rate limit as a call answered it.
type GitHubRate struct {
	Remaining int       `json:"remaining"`
	Limit     int       `json:"limit"`
	ResetAt   time.Time `json:"reset_at"`
	SeenAt    time.Time `json:"seen_at"` // when the call that answered it returned
}

// GitHubHour is one clock hour of an identity's calls.
type GitHubHour struct {
	Hour      time.Time `json:"hour"`       // the hour's start, UTC
	Points    int       `json:"points"`     // GraphQL points
	Calls     int       `json:"calls"`      // every call, GraphQL and REST
	RESTCalls int       `json:"rest_calls"` // REST calls, against the core budget
	// Ops splits the hour by op (an engine label, GitHubOpWords, or a call's
	// own kind).
	Ops map[string]GitHubOpCost `json:"ops"`
}

// GitHubOpCost is what one op cost in an hour.
type GitHubOpCost struct {
	Points    int `json:"points"`
	Calls     int `json:"calls"`
	RESTCalls int `json:"rest_calls,omitempty"`
}

// KVGitHubLow holds identity's run of low budget (GitHubLow as JSON), from
// the tick that found its budget below [github] budget_floor until the one
// that finds it recovered (noteGitHubBudget), written at once, apart from
// the cost record the flush writes once a minute.
func KVGitHubLow(identity string) string { return kvGitHubLowPrefix + identity }

// kvGitHubLowPrefix starts every KVGitHubLow, and kvGitHubLowGlob matches
// them.
const (
	kvGitHubLowPrefix = "github.low."
	kvGitHubLowGlob   = kvGitHubLowPrefix + "*"
)

// GitHubLow is a run of low budget (KVGitHubLow): since when, the budget
// when it began or its reset last moved (what was left, of what limit) and
// the floor then, and when the budget resets. The start reads its budget
// back into the meter (loadGitHubCost), so a daemon that stopped before the
// minute's flush still holds the work the run held.
type GitHubLow struct {
	Since     time.Time `json:"since"`
	Remaining int       `json:"remaining"`
	Limit     int       `json:"limit,omitempty"`
	Floor     int       `json:"floor"`
	Until     time.Time `json:"until"`
}

// ParseGitHubLow reads a KVGitHubLow value (ok false for one that does not
// read).
func ParseGitHubLow(s string) (GitHubLow, bool) {
	var l GitHubLow
	if s == "" || json.Unmarshal([]byte(s), &l) != nil || l.Until.IsZero() {
		return GitHubLow{}, false
	}
	return l, true
}

// ParseGitHubCost reads a KVGitHubCost value (ok false for one that does
// not read).
func ParseGitHubCost(s string) (GitHubCost, bool) {
	var c GitHubCost
	if s == "" || json.Unmarshal([]byte(s), &c) != nil || c.Identity == "" {
		return GitHubCost{}, false
	}
	return c, true
}

// Since sums the hours that started at or after from: the total points,
// calls and REST calls, and the ops by points, the costliest first (by
// calls, then by name, among equal points).
func (c GitHubCost) Since(from time.Time) (GitHubOpCost, []GitHubOpSum) {
	var total GitHubOpCost
	ops := map[string]GitHubOpCost{}
	for _, h := range c.Hours {
		if h.Hour.Before(from) {
			continue
		}
		total.Points += h.Points
		total.Calls += h.Calls
		total.RESTCalls += h.RESTCalls
		for op, oc := range h.Ops {
			s := ops[op]
			s.Points += oc.Points
			s.Calls += oc.Calls
			s.RESTCalls += oc.RESTCalls
			ops[op] = s
		}
	}
	out := make([]GitHubOpSum, 0, len(ops))
	for op, oc := range ops {
		out = append(out, GitHubOpSum{Op: op, GitHubOpCost: oc})
	}
	slices.SortFunc(out, func(a, b GitHubOpSum) int {
		return cmp.Or(cmp.Compare(b.Points, a.Points), cmp.Compare(b.Calls, a.Calls), strings.Compare(a.Op, b.Op))
	})
	return total, out
}

// GitHubOpSum is one op's cost over several hours.
type GitHubOpSum struct {
	Op string `json:"op"`
	GitHubOpCost
}

// The ops the engine labels its GitHub reads with (github.WithOp). A call
// made with none of them counts under its own kind ("details", "radar").
const (
	opRound         = "round"             // a round's run: the pipeline's reads and posts
	opRoundSetup    = "round.setup"       // a round's setup (prepare)
	opRoundSettle   = "round.settle"      // a round's settle (finish)
	opRoundTriage   = "round.triage"      // the triage's diff (triage.go)
	opDeltaCheck    = "round.delta_check" // a delta check's range (deltacheck.go)
	opFullPass      = "round.full_pass"   // a full pass's churn (fullpass.go)
	opPostMerge     = "round.post_merge"  // a merged PR's merge commit (post_merge.go)
	opRerun         = "round.rerun"       // a role's rerun size (rerun.go)
	opPushDelta     = "poll.delta"        // the trivial-delta check of a push (delta.go)
	opSinceReview   = "poll.since_review" // the size of what changed since the review (since.go)
	opAutoApprove   = "auto_approve"      // auto-approval's reads and writes (autoapprove.go)
	opApproval      = "approval"          // an App approval the head left behind (approval.go)
	opStatusComment = "status_comment"    // the PR status comments (status_comment.go)
	opFix           = "fix"               // a fix job's own reads
	opFixExternal   = "fix.external"      // the other reviewers' items (fix_external.go)
	opFixSettle     = "fix.settle"        // a fix cycle's settle (fix_settle.go)
	opWhiteboard    = "whiteboard"        // a whiteboard job (whiteboard.go)
	opRetro         = "retro"             // the retro (retro.go)
	opVerdict       = "verdict"           // magnum approve / request-changes (verdict.go)
)

// githubOpWords are the ops in the words `magnum status` shows.
var githubOpWords = map[string]string{
	opRound:         "round",
	opRoundSetup:    "round setup",
	opRoundSettle:   "settle",
	opRoundTriage:   "triage",
	opDeltaCheck:    "delta check",
	opFullPass:      "full pass",
	opPostMerge:     "post-merge",
	opRerun:         "rerun",
	opPushDelta:     "push delta",
	opSinceReview:   "since-review size",
	opAutoApprove:   "auto-approval",
	opApproval:      "approval follow-up",
	opStatusComment: "status comment",
	opFix:           "fix",
	opFixExternal:   "other reviewers",
	opFixSettle:     "fix settle",
	opWhiteboard:    "whiteboard",
	opRetro:         "retro",
	opVerdict:       "verdict",
}

// GitHubOpWords is op in the words `magnum status` shows: an engine label
// in plain words ("round.setup": "round setup"), a call's own kind as it is
// ("radar", "details").
func GitHubOpWords(op string) string {
	if w, ok := githubOpWords[op]; ok {
		return w
	}
	return strings.ReplaceAll(strings.ReplaceAll(op, ".", " "), "_", " ")
}

// githubMeter is the engine's meter: every identity's record in memory, the
// identities whose record the store has not seen yet, each identity's run
// of low budget (KVGitHubLow), when it last wrote and how many records it
// wrote.
type githubMeter struct {
	mu      sync.Mutex
	ids     map[string]*GitHubCost
	dirty   map[string]bool
	lows    map[string]*GitHubLow
	flushed time.Time
	writes  int
}

// meterGitHub is the Meter of identity's GitHub client (FromApp sets it).
func (e *Engine) meterGitHub(identity string) func(github.Call) {
	return func(c github.Call) { e.ghMeter.add(identity, c, e.now()) }
}

// record is identity's record, made on first use; the caller holds mu.
func (m *githubMeter) record(identity string) *GitHubCost {
	if m.ids == nil {
		m.ids, m.dirty = map[string]*GitHubCost{}, map[string]bool{}
	}
	c := m.ids[identity]
	if c == nil {
		c = &GitHubCost{Identity: identity}
		m.ids[identity] = c
	}
	return c
}

// add counts one call of identity at now, in the clock hour of now: a call
// stamped before the newest hour (a clock that stepped back) counts in its
// own hour, which is made in its place when the record has none.
func (m *githubMeter) add(identity string, call github.Call, now time.Time) {
	op := cmp.Or(call.Op, call.Kind, "other")
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.record(identity)
	h := c.hour(now)
	oc := h.Ops[op]
	h.Calls++
	oc.Calls++
	if call.GraphQL {
		h.Points += call.Rate.Cost
		oc.Points += call.Rate.Cost
	} else {
		h.RESTCalls++
		oc.RESTCalls++
	}
	h.Ops[op] = oc
	c.observe(call.Rate, now)
	c.prune(now)
	m.dirty[identity] = true
}

// hour is the record's hour of t, made in its place among the hours (oldest
// first) when there is none.
func (c *GitHubCost) hour(t time.Time) *GitHubHour {
	start := t.UTC().Truncate(time.Hour)
	i, found := slices.BinarySearchFunc(c.Hours, start, func(h GitHubHour, t time.Time) int { return h.Hour.Compare(t) })
	if !found {
		c.Hours = slices.Insert(c.Hours, i, GitHubHour{Hour: start})
	}
	if c.Hours[i].Ops == nil {
		c.Hours[i].Ops = map[string]GitHubOpCost{}
	}
	return &c.Hours[i]
}

// sawRate keeps rl as identity's rate limit when it is the latest (the
// poll's radar, recordRateLimit, whose cost its call already counted).
func (m *githubMeter) sawRate(identity string, rl github.RateLimit, now time.Time) {
	if rl.ResetAt.IsZero() || identity == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.record(identity).observe(rl, now) {
		m.dirty[identity] = true
	}
}

// observe keeps rl as the record's rate limit when it is at least as
// current as the one it has: a later reset window, or the same window with
// no more left (calls in flight return in any order). It reports whether
// it kept it.
func (c *GitHubCost) observe(rl github.RateLimit, now time.Time) bool {
	if rl.ResetAt.IsZero() {
		return false
	}
	if r := c.Rate; r != nil && (rl.ResetAt.Before(r.ResetAt) || rl.ResetAt.Equal(r.ResetAt) && rl.Remaining > r.Remaining) {
		return false
	}
	c.Rate = &GitHubRate{Remaining: rl.Remaining, Limit: rl.Limit, ResetAt: rl.ResetAt.UTC(), SeenAt: now.UTC()}
	return true
}

// prune drops the hours older than the last githubCostHours and reports
// whether it dropped any.
func (c *GitHubCost) prune(now time.Time) bool {
	oldest := now.UTC().Truncate(time.Hour).Add(-(githubCostHours - 1) * time.Hour)
	n := len(c.Hours)
	c.Hours = slices.DeleteFunc(c.Hours, func(h GitHubHour) bool { return h.Hour.Before(oldest) })
	return len(c.Hours) != n
}

// clone is a deep copy of the record, for a write outside the lock.
func (c *GitHubCost) clone() GitHubCost {
	out := *c
	if c.Rate != nil {
		r := *c.Rate
		out.Rate = &r
	}
	out.Hours = make([]GitHubHour, len(c.Hours))
	for i, h := range c.Hours {
		h.Ops = maps.Clone(h.Ops)
		out.Hours[i] = h
	}
	return out
}

// loadGitHubCost reads the records and the runs of low budget an earlier
// daemon wrote back into the meter, at the start: the hours and the latest
// rate limits outlive a restart, and so does a run of low budget (no second
// event or toast). The rows of an identity the config no longer declares
// go, and a record whose oldest hours fell out of the last 24 is written
// again without them.
func (e *Engine) loadGitHubCost(ctx context.Context) {
	costs, err := e.st.KVValues(ctx, kvGitHubCostGlob)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "github cost records")
		return
	}
	lows, err := e.st.KVValues(ctx, kvGitHubLowGlob)
	if err != nil {
		e.warnUnlessStopped(ctx, err, "github low budget runs")
		return
	}
	now := e.now()
	m := &e.ghMeter
	var gone []string
	m.mu.Lock()
	for _, key := range slices.Sorted(maps.Keys(costs)) {
		c, ok := ParseGitHubCost(costs[key])
		switch {
		case !ok || KVGitHubCost(c.Identity) != key:
			continue
		case e.cfg.IdentityByName(c.Identity) == nil:
			gone = append(gone, key)
			continue
		}
		cur := m.record(c.Identity)
		if len(cur.Hours) > 0 || cur.Rate != nil {
			continue // calls since the start counted already: they win
		}
		*cur = c
		if cur.prune(now) {
			m.dirty[c.Identity] = true
		}
	}
	for _, key := range slices.Sorted(maps.Keys(lows)) {
		id, _ := strings.CutPrefix(key, kvGitHubLowPrefix)
		l, ok := ParseGitHubLow(lows[key])
		switch {
		case !ok:
			continue
		case e.cfg.IdentityByName(id) == nil:
			gone = append(gone, key)
			continue
		}
		if m.lows == nil {
			m.lows = map[string]*GitHubLow{}
		}
		if _, have := m.lows[id]; !have {
			m.lows[id] = &l
		}
		if l.Limit > 0 { // the run's budget, unless the record holds a later one
			m.record(id).observe(github.RateLimit{Limit: l.Limit, Remaining: l.Remaining, ResetAt: l.Until}, l.Since)
		}
		m.record(id) // noteGitHubBudget looks at it, and ends the run when the budget recovered
	}
	m.mu.Unlock()
	if len(gone) > 0 {
		e.delKV(ctx, gone...)
	}
}

// flushGitHubCost writes the records whose calls the store has not seen,
// at most once a minute (githubCostFlush) unless force (the daemon's stop).
// It first drops every record's hours older than the last 24, so a record
// of an identity that makes no more calls is written again without them. A
// record whose write fails (a store error, a context the stop cancelled)
// stays to be written: the next flush, the stop's included, writes it, with
// whatever calls came meanwhile.
func (e *Engine) flushGitHubCost(ctx context.Context, force bool) {
	now := e.now()
	m := &e.ghMeter
	m.mu.Lock()
	for id, c := range m.ids {
		if c.prune(now) {
			m.dirty[id] = true
		}
	}
	if len(m.dirty) == 0 || !force && !m.flushed.IsZero() && now.Sub(m.flushed) < githubCostFlush {
		m.mu.Unlock()
		return
	}
	writes := make([]GitHubCost, 0, len(m.dirty))
	for id := range m.dirty {
		writes = append(writes, m.record(id).clone())
	}
	clear(m.dirty)
	m.flushed = now
	m.mu.Unlock()
	for _, c := range writes {
		b, err := json.Marshal(c)
		if err == nil {
			err = e.st.SetKV(ctx, KVGitHubCost(c.Identity), string(b))
		}
		m.mu.Lock()
		if err != nil {
			m.dirty[c.Identity] = true // its calls are still to be written; any since are in the record too
		} else {
			m.writes++
		}
		m.mu.Unlock()
		if err != nil {
			e.warnUnlessStopped(ctx, err, "github cost record", "identity", c.Identity)
		}
	}
}

// githubLowBudget is an identity whose budget the floor holds work for.
type githubLowBudget struct {
	identity         string
	remaining, floor int
	until            time.Time
}

// text is the wait reason: "github budget: zhuravel at 312 points until 17:13".
func (l githubLowBudget) text() string {
	return fmt.Sprintf("github budget: %s at %d points until %s", l.identity, l.remaining, l.until.Local().Format("15:04"))
}

// githubLow is the first of ids whose latest rate limit is below [github]
// budget_floor in a reset window that has not ended (ok false: none is, the
// floor is off, or no call told the meter of them). An identity no call
// answered for, or whose window ended, is never held: the next call tells.
func (e *Engine) githubLow(ids ...string) (githubLowBudget, bool) {
	floor := e.cfg.GitHub.BudgetFloor
	if floor <= 0 {
		return githubLowBudget{}, false
	}
	now := e.now()
	m := &e.ghMeter
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		c := m.ids[id]
		if id == "" || c == nil || c.Rate == nil {
			continue
		}
		if r := c.Rate; r.Limit > 0 && r.Remaining < floor && now.Before(r.ResetAt) {
			return githubLowBudget{identity: id, remaining: r.Remaining, floor: floor, until: r.ResetAt}, true
		}
	}
	return githubLowBudget{}, false
}

// githubLowAny is githubLow of every watch's poll identity: the retro reads
// the PRs of every watch.
func (e *Engine) githubLowAny() (githubLowBudget, bool) {
	var ids []string
	for _, w := range e.cfg.Watches {
		if !slices.Contains(ids, w.PollIdentity) {
			ids = append(ids, w.PollIdentity)
		}
	}
	return e.githubLow(ids...)
}

// githubBudgetToastKey dedupes the toast of identity's run of low budget.
func githubBudgetToastKey(identity string) string { return "github-budget:" + identity }

// noteGitHubBudget marks, once per tick, where each identity's run of low
// budget begins and ends: the tick that first finds an identity below the
// floor records the run (KVGitHubLow, written at once: it is what keeps a
// restart from announcing the run again), writes a github.budget_low event
// and toasts "GitHub budget of <identity> at N points: new rounds wait until
// HH:MM" once; the one that finds it recovered (above the floor again, or
// past its reset) removes the run with github.budget_recovered and lets the
// next run toast again. A run whose reset moves on while the budget stays
// low (a new window that starts low) is written again with its new end,
// without a second announcement. The cost records are left to the flush,
// once a minute.
func (e *Engine) noteGitHubBudget(ctx context.Context) {
	floor := e.cfg.GitHub.BudgetFloor
	now := e.now()
	type change struct {
		low   githubLowBudget // the identity, and for a run what is left and until when
		run   *GitHubLow      // the run to write (nil: the run ended)
		began bool
		left  int // a run that ended: what is left now (the limit once the window reset)
	}
	var changes []change
	m := &e.ghMeter
	m.mu.Lock()
	if m.lows == nil {
		m.lows = map[string]*GitHubLow{}
	}
	for _, id := range slices.Sorted(maps.Keys(m.ids)) {
		r, run := m.ids[id].Rate, m.lows[id]
		low := floor > 0 && r != nil && r.Limit > 0 && r.Remaining < floor && now.Before(r.ResetAt)
		switch {
		case low && run == nil:
			run = &GitHubLow{Since: now.UTC(), Remaining: r.Remaining, Limit: r.Limit, Floor: floor, Until: r.ResetAt}
			m.lows[id] = run
			cp := *run
			changes = append(changes, change{low: githubLowBudget{identity: id, remaining: r.Remaining, floor: floor, until: r.ResetAt}, run: &cp, began: true})
		case low && (!run.Until.Equal(r.ResetAt) || run.Floor != floor):
			run.Remaining, run.Limit, run.Until, run.Floor = r.Remaining, r.Limit, r.ResetAt, floor
			cp := *run
			changes = append(changes, change{low: githubLowBudget{identity: id}, run: &cp})
		case low: // the run goes on as written
		case run != nil:
			left := 0
			if r != nil {
				left = r.Remaining
				if !now.Before(r.ResetAt) {
					left = r.Limit
				}
			}
			delete(m.lows, id)
			changes = append(changes, change{low: githubLowBudget{identity: id, floor: floor}, left: left})
		}
	}
	m.mu.Unlock()
	for _, ch := range changes {
		id := ch.low.identity
		if ch.run != nil {
			if b, err := json.Marshal(ch.run); err == nil {
				e.setKV(ctx, KVGitHubLow(id), string(b))
			}
		} else {
			e.delKV(ctx, KVGitHubLow(id))
		}
		data := map[string]any{"identity": id, "floor": ch.low.floor}
		switch {
		case ch.began:
			msg := fmt.Sprintf("GitHub budget of %s at %d points: new rounds wait until %s", id, ch.low.remaining, ch.low.until.Local().Format("15:04"))
			data["remaining"], data["until"] = ch.low.remaining, ch.low.until
			e.event(ctx, "warn", "github:"+id, "github.budget_low", fmt.Sprintf("%s (below the floor of %d; fix jobs, whiteboards, the retro and notes curations wait too, running rounds finish)",
				msg, ch.low.floor), data)
			e.urgent(githubBudgetToastKey(id), "magnum: "+msg,
				fmt.Sprintf("Below the [github] budget_floor of %d points no new round, fix job, whiteboard, retro or curation reads as %s; running rounds finish.",
					ch.low.floor, id), time.Hour)
		case ch.run == nil:
			data["remaining"] = ch.left
			e.event(ctx, "info", "github:"+id, "github.budget_recovered", fmt.Sprintf("GitHub budget of %s recovered (%d points): new work starts again", id, ch.left), data)
			e.forgetSend(ctx, githubBudgetToastKey(id))
		}
	}
}
