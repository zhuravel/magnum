package cli

// `magnum stats`: what the daemon did over a window, per local day and
// repository. Everything is read from the registry: the runs of the rounds
// that began in the window, the judge's per-finding provenance and a few audit
// events. statsCompute is the pure part; statsGather reads the store.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

const statsUsage = "[--since 7d] [--repo X] [--json]"

// Audit events `stats` reads (written by the pipeline and the agents manager).
const (
	statsKindRoundEnd = "round.end"
	statsKindFallback = "round.model_fallback"
	statsKindRestart  = "round.restarted"
)

// statsEventKinds are the event kinds statsGather loads.
var statsEventKinds = []string{statsKindRoundEnd, statsKindFallback, agents.EventPromptDenied, statsKindRestart}

const (
	// statsRoundRole is the pseudo-role under which a whole round's duration
	// is reported next to the roles' (a role literally named "round" would
	// share its entry).
	statsRoundRole = "round"
	// statsJudgeSource is how the judge's own pass is named in a finding's
	// sources.
	statsJudgeSource = "judge"
	// statsUnspecified is the reason of a rejection that carries no code.
	statsUnspecified = "unspecified"
	// statsTimeFmt formats the window's start in the text report.
	statsTimeFmt = "2006-01-02 15:04"
	// statsMaxDays bounds `--since <N>d` so the duration cannot overflow.
	statsMaxDays = 36500
)

// statsSeverities are the priorities a result file counts findings by.
var statsSeverities = []string{"P0", "P1", "P2", "P3"}

func newStatsCmd(c *Context) *cobra.Command {
	var f statsFlags
	cmd := newCommand(groupInspect, "stats "+statsUsage,
		"review statistics per day and repository: rounds, durations, findings, sources, operations",
		"Summarise what the daemon did over a window (--since, default 7d), per local day and repository with a total. "+
			"Durations count only work that has finished: a role's turn once its runs ended and a whole round once "+
			"no run is still going.\n\n"+
			"ROUNDS counts the review rounds that began in the window and how each one ended (posted, stopped, "+
			"error, ..., or running while a run is still going). FINDINGS POSTED adds up the P0 to P3 counts in "+
			"the judge's result of every posted round.\n\n"+
			"DURATIONS gives the median and the 90th percentile of each role's turn and of the whole round. "+
			"SOURCES shows, for each reviewer role and for the judge's own pass, how many findings the judge "+
			"weighed, how many it posted, how many only that source raised (unique) and how many it rejected, "+
			"with the reason codes; it comes from the judge's per-finding provenance, so rounds posted before it "+
			"existed have none.\n\n"+
			"OPERATIONS counts model-limit switches, permission prompts magnum denied and round restarts (the PR "+
			"head moved before the judge was prompted). The text report shows durations and sources over the "+
			"whole window; --json has every day and repository.\n\n"+
			"TOP PRS BY AGENT TIME lists the 10 PRs whose runs kept agents busiest in the window, with the rounds "+
			"those runs belong to and the share of all the window's agent time (--json: top_prs, agent_seconds "+
			"and share as a fraction). A run counts from its submission to its end, to now while it is going, "+
			"and only when it was created in the window.\n\n"+
			"--since takes N days (7d), a Go duration (36h, 90m) or a date (2026-10-01, local midnight).",
		func(pos []string) int { return runStats(c, f, pos) })
	fs := cmd.Flags()
	fs.StringVar(&f.since, "since", "7d", "how far back: 7d, 36h, 90m or a date such as 2026-10-01 (local midnight)")
	fs.StringVar(&f.repo, "repo", "", "only this repository (owner/name, or name in daemon.default_repo's owner)")
	fs.BoolVar(&f.json, "json", false, "the report as JSON")
	_ = cmd.RegisterFlagCompletionFunc("repo", completeFlag(c.completeRepos))
	return cmd
}

// statsFlags are the parsed `magnum stats` flags.
type statsFlags struct {
	since, repo string
	json        bool
}

func runStats(c *Context, f statsFlags, pos []string) int {
	if len(pos) > 0 {
		return inspUsage(c, "stats", "no arguments expected", statsUsage)
	}
	now := inspNow()
	since, err := statsParseSince(f.since, now)
	if err != nil {
		return inspUsage(c, "stats", err.Error(), statsUsage)
	}
	ctx, cancel := signalContext()
	defer cancel()
	a, err := inspOpenApp(c, false)
	if err != nil {
		return cmdFail(c, "stats", err)
	}
	defer a.Close()
	repo := ""
	if f.repo != "" {
		if repo, err = rolesRepo(c.Config, f.repo); err != nil {
			return inspUsage(c, "stats", err.Error(), statsUsage)
		}
	}
	r, err := statsGather(ctx, a.Store, c.Config, since, now, repo)
	if err != nil {
		return cmdFail(c, "stats", err)
	}
	if f.json {
		if err := writeJSON(c.Stdout, r); err != nil {
			return cmdFail(c, "stats", err)
		}
		return 0
	}
	statsRender(c.Stdout, r)
	return 0
}

// statsParseSince turns --since into the window's start: now minus N days
// ("7d"), now minus a Go duration ("36h", "90m") or local midnight of a date
// ("2026-10-01"). The start must lie in the past.
func statsParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	bad := fmt.Errorf("--since %q: want N days (7d), a duration (36h, 90m) or a date (2026-10-01)", s)
	var since time.Time
	switch {
	case s == "":
		return time.Time{}, bad
	case strings.HasSuffix(s, "d"):
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n <= 0 || n > statsMaxDays {
			return time.Time{}, bad
		}
		since = now.Add(-time.Duration(n) * 24 * time.Hour)
	default:
		if d, err := time.ParseDuration(s); err == nil {
			if d <= 0 {
				return time.Time{}, bad
			}
			since = now.Add(-d)
			break
		}
		t, err := time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			return time.Time{}, bad
		}
		since = t
	}
	if !since.Before(now) {
		return time.Time{}, fmt.Errorf("--since %q is not in the past", s)
	}
	return since, nil
}

// --- the report ---

// statsReport is `magnum stats --json`.
type statsReport struct {
	Since  time.Time    `json:"since"`
	Until  time.Time    `json:"until"`
	Repo   string       `json:"repo,omitempty"`
	Groups []statsGroup `json:"groups"`
	Total  statsGroup   `json:"total"`
	// AgentSeconds is all the agent time of the window (of the repository
	// under --repo), TopPRs the PRs that took the most of it.
	AgentSeconds int64        `json:"agent_seconds"`
	TopPRs       []statsTopPR `json:"top_prs"`
}

// statsTopLimit is how many PRs the top lists.
const statsTopLimit = 10

// statsTopPR is one PR's agent time in the window: the sum of its runs'
// durations (store.AgentTimeSince), the rounds they belong to and Share, its
// fraction of all the agent time in scope (four decimals).
type statsTopPR struct {
	Repo         string  `json:"repo"`
	Number       int     `json:"number"`
	AgentSeconds int64   `json:"agent_seconds"`
	Rounds       int     `json:"rounds"`
	Share        float64 `json:"share"`
}

// statsTop picks the statsTopLimit PRs of times with the most agent time
// (ties by repository, then number; one without any is left out) and returns
// them with the total of every PR in scope; repo ("owner/name", or "" for
// all) keeps one repository.
func statsTop(times []store.PRAgentTime, repo string) ([]statsTopPR, time.Duration) {
	var total time.Duration
	var in []store.PRAgentTime
	for _, p := range times {
		if repo != "" && p.Repo != repo {
			continue
		}
		total += p.Time
		if p.Time > 0 {
			in = append(in, p)
		}
	}
	slices.SortFunc(in, func(a, b store.PRAgentTime) int {
		return cmp.Or(cmp.Compare(b.Time, a.Time), cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Number, b.Number))
	})
	top := make([]statsTopPR, 0, min(len(in), statsTopLimit))
	for _, p := range in[:min(len(in), statsTopLimit)] {
		top = append(top, statsTopPR{Repo: p.Repo, Number: p.Number, AgentSeconds: statsSeconds(p.Time), Rounds: p.Rounds,
			Share: math.Round(10000*float64(p.Time)/float64(total)) / 10000})
	}
	return top, total
}

// statsGroup is one (day, repository) group, or the total (no day or repo).
type statsGroup struct {
	Day            string                   `json:"day,omitempty"`
	Repo           string                   `json:"repo,omitempty"`
	Rounds         statsRounds              `json:"rounds"`
	Durations      map[string]statsDuration `json:"durations,omitempty"`
	FindingsPosted map[string]int           `json:"findings_posted,omitempty"`
	Sources        map[string]*statsSource  `json:"sources,omitempty"`
	ModelSwitches  int                      `json:"model_switches"`
	Denies         int                      `json:"denies"`
	Restarts       int                      `json:"restarts"`
}

// statsRounds counts the rounds that began in a group and how they ended.
type statsRounds struct {
	Started  int            `json:"started"`
	Posted   int            `json:"posted"`
	Outcomes map[string]int `json:"outcomes,omitempty"`
}

// statsDuration summarises the finished spans of one role (or of the round).
type statsDuration struct {
	N             int   `json:"n"`
	MedianSeconds int64 `json:"median_seconds"`
	P90Seconds    int64 `json:"p90_seconds"`
}

// statsSource is what the judge did with the findings one source raised.
type statsSource struct {
	Judged   int            `json:"judged"`
	Posted   int            `json:"posted"`
	Unique   int            `json:"unique"` // posted, and no other source raised it
	Rejected int            `json:"rejected"`
	Reasons  map[string]int `json:"reasons,omitempty"` // rejections by reason code
}

// statsGather reads the window's runs, findings and events and computes the
// report; repo ("owner/name", or "" for all) keeps one repository.
func statsGather(ctx context.Context, st *store.Store, cfg *config.Config, since, now time.Time, repo string) (statsReport, error) {
	// A round kept below began at or after since, so its round.end events
	// are not older than since either: one event query serves all kinds.
	runs, err := st.RunsOfRoundsSince(ctx, since)
	if err != nil {
		return statsReport{}, err
	}
	findings, err := st.FindingsSince(ctx, since)
	if err != nil {
		return statsReport{}, err
	}
	events, err := st.EventsOfKindsSince(ctx, since, statsEventKinds...)
	if err != nil {
		return statsReport{}, err
	}
	times, err := st.AgentTimeSince(ctx, since, now)
	if err != nil {
		return statsReport{}, err
	}
	isJudge := func(role string) bool { return actIsJudge(cfg, role) }
	rep := statsCompute(runs, findings, events, isJudge, since, now, repo)
	var total time.Duration
	rep.TopPRs, total = statsTop(times, repo)
	rep.AgentSeconds = statsSeconds(total)
	return rep, nil
}

// statsKey identifies a group.
type statsKey struct{ day, repo string }

// statsAcc accumulates one group; spans keeps the raw samples so the total's
// percentiles come from all of them, not from the groups' percentiles.
type statsAcc struct {
	g     statsGroup
	spans map[string][]time.Duration
}

func newStatsAcc() *statsAcc {
	return &statsAcc{
		g: statsGroup{
			Rounds:         statsRounds{Outcomes: map[string]int{}},
			Durations:      map[string]statsDuration{},
			FindingsPosted: map[string]int{},
			Sources:        map[string]*statsSource{},
		},
		spans: map[string][]time.Duration{},
	}
}

// source returns the counters of one source, created on first use.
func (a *statsAcc) source(name string) *statsSource {
	s := a.g.Sources[name]
	if s == nil {
		s = &statsSource{Reasons: map[string]int{}}
		a.g.Sources[name] = s
	}
	return s
}

// finish turns the samples into durations and returns the group.
func (a *statsAcc) finish(key statsKey) statsGroup {
	g := a.g
	g.Day, g.Repo = key.day, key.repo
	for role, ds := range a.spans {
		sorted := slices.Sorted(slices.Values(ds))
		g.Durations[role] = statsDuration{N: len(sorted),
			MedianSeconds: statsSeconds(statsRank(sorted, 50)), P90Seconds: statsSeconds(statsRank(sorted, 90))}
	}
	return g
}

// statsRank is the nearest-rank percentile pct (1..100) of sorted (not empty).
func statsRank(sorted []time.Duration, pct int) time.Duration {
	rank := (pct*len(sorted) + 99) / 100
	return sorted[min(max(rank, 1), len(sorted))-1]
}

func statsSeconds(d time.Duration) int64 { return int64(d.Round(time.Second) / time.Second) }

// statsAccs holds the groups and the total.
type statsAccs struct {
	groups map[statsKey]*statsAcc
	total  *statsAcc
}

// do applies fn to the (day, repo) group, creating it, and to the total.
func (s *statsAccs) do(day, repo string, fn func(*statsAcc)) {
	k := statsKey{day, repo}
	g := s.groups[k]
	if g == nil {
		g = newStatsAcc()
		s.groups[k] = g
	}
	fn(g)
	fn(s.total)
}

// statsCompute builds the report. runs are the runs of the rounds that have
// a run in the window (oldest first), findings and events those since the
// window's start; isJudge tells the judge's role. repo ("owner/name" or "")
// keeps one repository. Rounds that began before since are left out.
func statsCompute(runs []store.RoundRun, findings []store.Finding, events []store.Event, isJudge func(role string) bool,
	since, now time.Time, repo string) statsReport {
	if isJudge == nil {
		isJudge = func(string) bool { return false }
	}
	accs := &statsAccs{groups: map[statsKey]*statsAcc{}, total: newStatsAcc()}
	ends := statsRoundEnds(events)

	// Rounds: outcomes, findings posted, durations.
	for _, rd := range statsGroupRounds(runs) {
		if (repo != "" && rd.repo != repo) || rd.start.Before(since) {
			continue
		}
		outcome := rd.outcome(ends, isJudge)
		var counts map[string]int
		if outcome == "posted" {
			counts = rd.findingsPosted(isJudge)
		}
		spans := rd.durations()
		accs.do(store.DayKey(rd.start), rd.repo, func(a *statsAcc) {
			a.g.Rounds.Started++
			a.g.Rounds.Outcomes[outcome]++
			if outcome == "posted" {
				a.g.Rounds.Posted++
			}
			for sev, n := range counts {
				a.g.FindingsPosted[sev] += n
			}
			for role, d := range spans {
				a.spans[role] = append(a.spans[role], d)
			}
		})
	}

	// Sources: what the judge did with the findings each source raised.
	for _, f := range findings {
		if (repo != "" && f.Repo != repo) || f.CreatedAt.Before(since) {
			continue
		}
		sources := slices.Compact(slices.Sorted(slices.Values(f.Sources)))
		if len(sources) == 0 {
			continue
		}
		reason := cmp.Or(f.ReasonCode, statsUnspecified)
		accs.do(store.DayKey(f.CreatedAt), f.Repo, func(a *statsAcc) {
			for _, name := range sources {
				s := a.source(name)
				s.Judged++
				switch f.Verdict {
				case store.FindingPosted:
					s.Posted++
					if len(sources) == 1 {
						s.Unique++
					}
				case store.FindingRejected:
					s.Rejected++
					s.Reasons[reason]++
				}
			}
		})
	}

	// Operations, by the day of the event and the repository of its PR.
	for _, ev := range events {
		if ev.At.Before(since) {
			continue
		}
		var count func(*statsAcc)
		switch ev.Kind {
		case statsKindFallback:
			count = func(a *statsAcc) { a.g.ModelSwitches++ }
		case agents.EventPromptDenied:
			count = func(a *statsAcc) { a.g.Denies++ }
		case statsKindRestart:
			count = func(a *statsAcc) { a.g.Restarts++ }
		default:
			continue
		}
		r, ok := statsSubjectRepo(store.Deref(ev.Subject))
		if !ok || (repo != "" && r != repo) {
			continue // not a PR subject: no repository to count it under
		}
		accs.do(store.DayKey(ev.At), r, count)
	}

	keys := slices.SortedFunc(maps.Keys(accs.groups), func(a, b statsKey) int {
		return cmp.Or(cmp.Compare(a.day, b.day), cmp.Compare(a.repo, b.repo))
	})
	rep := statsReport{Since: since.Truncate(time.Second), Until: now.Truncate(time.Second), Repo: repo,
		Groups: make([]statsGroup, 0, len(keys)), Total: accs.total.finish(statsKey{})}
	for _, k := range keys {
		rep.Groups = append(rep.Groups, accs.groups[k].finish(k))
	}
	return rep
}

// statsSubjectRepo is the repository ("owner/name") of an event subject
// "pr:<owner>/<name>#<N>"; ok is false for any other subject.
func statsSubjectRepo(subject string) (string, bool) {
	rest, ok := strings.CutPrefix(subject, "pr:")
	if !ok {
		return "", false
	}
	repo, num, ok := strings.Cut(rest, "#")
	if !ok || strings.Count(repo, "/") != 1 || strings.HasPrefix(repo, "/") || strings.HasSuffix(repo, "/") {
		return "", false
	}
	if n, err := strconv.Atoi(num); err != nil || n <= 0 {
		return "", false
	}
	return repo, true
}

// statsRoundEnd names a round's end event: the PR subject and the round.
type statsRoundEnd struct {
	subject string
	round   int
}

var statsEndedRE = regexp.MustCompile(`^round (\d+) ended: ([a-z0-9_]+)`)

// statsRoundEnds maps each round to the outcome of its latest round.end event
// (events are oldest first; a paused round that continued ends more than once).
func statsRoundEnds(events []store.Event) map[statsRoundEnd]string {
	out := map[statsRoundEnd]string{}
	for _, ev := range events {
		if ev.Kind != statsKindRoundEnd || ev.Subject == nil {
			continue
		}
		m := statsEndedRE.FindStringSubmatch(ev.Message)
		if m == nil {
			continue
		}
		round, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		outcome := m[2]
		var data struct {
			Outcome string `json:"outcome"`
		}
		if json.Unmarshal(ev.Data, &data) == nil && data.Outcome != "" {
			outcome = data.Outcome
		}
		out[statsRoundEnd{*ev.Subject, round}] = outcome
	}
	return out
}

// statsRound is one round (a PR's round number) with all its runs.
type statsRound struct {
	repo   string
	number int
	round  int
	start  time.Time // the round's first run created
	runs   []store.Run
}

// statsGroupRounds groups runs by (PR, round), in order of first appearance.
func statsGroupRounds(runs []store.RoundRun) []*statsRound {
	type key struct {
		pr    int64
		round int
	}
	idx := map[key]*statsRound{}
	var out []*statsRound
	for _, rr := range runs {
		k := key{rr.PRID, rr.Round}
		rd := idx[k]
		if rd == nil {
			rd = &statsRound{repo: rr.Repo, number: rr.Number, round: rr.Round}
			idx[k] = rd
			out = append(out, rd)
		}
		rd.runs = append(rd.runs, rr.Run)
	}
	for _, rd := range out {
		rd.start = roundStart(rd.runs)
	}
	return out
}

// statsActive reports whether a run is not finished: waiting, in its turn or
// ended without its report or review collected yet.
func statsActive(r store.Run) bool { return runActive(r) || r.State == store.RunEnded }

// lastJudgeRun returns the round's last judge run (by creation, then input
// order) for which keep holds; nil when there is none.
func (rd *statsRound) lastJudgeRun(isJudge func(string) bool, keep func(store.Run) bool) *store.Run {
	var last *store.Run
	for i := range rd.runs {
		r := &rd.runs[i]
		if isJudge(r.Role) && keep(*r) && (last == nil || !r.CreatedAt.Before(last.CreatedAt)) {
			last = r
		}
	}
	return last
}

// outcome is how the round ended: the latest round.end event, else the last
// judge run's outcome, else "running" while a run is not finished, else
// "unknown".
func (rd *statsRound) outcome(ends map[statsRoundEnd]string, isJudge func(string) bool) string {
	if o := ends[statsRoundEnd{"pr:" + rd.repo + "#" + strconv.Itoa(rd.number), rd.round}]; o != "" {
		return o
	}
	if j := rd.lastJudgeRun(isJudge, func(store.Run) bool { return true }); j != nil && store.Deref(j.Outcome) != "" {
		return *j.Outcome
	}
	if slices.ContainsFunc(rd.runs, statsActive) {
		return "running"
	}
	return "unknown"
}

// findingsPosted reads the finding counts by priority from the result of the
// round's last judge run that has one.
func (rd *statsRound) findingsPosted(isJudge func(string) bool) map[string]int {
	j := rd.lastJudgeRun(isJudge, func(r store.Run) bool { return store.Deref(r.ResultJSON) != "" })
	if j == nil {
		return nil
	}
	return statsResultFindings(*j.ResultJSON)
}

// statsResultFindings parses the "findings" object ({"P0":0,"P1":1,...}) of
// a result file; counts may be numbers or numeric strings, anything else
// (other keys, negative, fractional or non-numeric values, other shapes) is
// ignored. Zero counts are left out.
func statsResultFindings(resultJSON string) map[string]int {
	var res struct {
		Findings map[string]any `json:"findings"`
	}
	if json.Unmarshal([]byte(resultJSON), &res) != nil {
		return nil
	}
	out := map[string]int{}
	for k, v := range res.Findings {
		sev := strings.ToUpper(strings.TrimSpace(k))
		if !slices.Contains(statsSeverities, sev) {
			continue
		}
		n := -1
		switch v := v.(type) {
		case float64:
			if v == math.Trunc(v) && v >= 0 && v <= 1e6 {
				n = int(v)
			}
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && i >= 0 && i <= 1e6 {
				n = i
			}
		}
		if n > 0 {
			out[sev] += n
		}
	}
	return out
}

// durations returns the finished spans of the round: each role's from its
// first submission (else creation) to its last end, once none of the role's
// runs is still going, and under statsRoundRole the whole round, from its
// start to the last end or verification, once no run of it is.
func (rd *statsRound) durations() map[string]time.Duration {
	type span struct {
		start, end time.Time
		active     bool
	}
	byRole := map[string]*span{}
	var last time.Time
	roundActive := false
	for _, r := range rd.runs {
		s := byRole[r.Role]
		if s == nil {
			s = &span{}
			byRole[r.Role] = s
		}
		start := r.CreatedAt
		if r.SubmittedAt != nil {
			start = *r.SubmittedAt
		}
		if s.start.IsZero() || start.Before(s.start) {
			s.start = start
		}
		if r.EndedAt != nil && r.EndedAt.After(s.end) {
			s.end = *r.EndedAt
		}
		s.active = s.active || statsActive(r)
		roundActive = roundActive || statsActive(r)
		for _, at := range []*time.Time{r.EndedAt, r.VerifiedAt} {
			if at != nil && at.After(last) {
				last = *at
			}
		}
	}
	out := map[string]time.Duration{}
	for role, s := range byRole {
		if !s.active && !s.end.IsZero() {
			out[actRoleName(role)] = max(s.end.Sub(s.start), 0)
		}
	}
	if !roundActive && !last.IsZero() {
		out[statsRoundRole] = max(last.Sub(rd.start), 0)
	}
	return out
}

// --- text ---

// statsRender prints the report as tables; a section without rows is left out.
func statsRender(w io.Writer, r statsReport) {
	scope := "all repositories"
	if r.Repo != "" {
		scope = actClean(r.Repo)
	}
	fmt.Fprintf(w, "stats since %s (%s), %s\n", r.Since.Local().Format(statsTimeFmt), tui.HumanDuration(r.Until.Sub(r.Since)), scope)
	if len(r.Groups) == 0 && len(r.TopPRs) == 0 {
		fmt.Fprintf(w, "\nno rounds since %s\n", r.Since.Local().Format(statsTimeFmt))
		return
	}
	statsTable(w, "ROUNDS", []string{"DAY", "REPO", "STARTED", "POSTED", "OTHER"},
		statsGroupRows(r, func(g statsGroup) []string {
			if g.Rounds.Started == 0 {
				return nil
			}
			return []string{strconv.Itoa(g.Rounds.Started), strconv.Itoa(g.Rounds.Posted), statsCounts(g.Rounds.Outcomes, "posted")}
		}))
	statsTable(w, "FINDINGS POSTED", append([]string{"DAY", "REPO"}, statsSeverities...),
		statsGroupRows(r, func(g statsGroup) []string {
			row := make([]string, len(statsSeverities))
			sum := 0
			for i, sev := range statsSeverities {
				row[i] = strconv.Itoa(g.FindingsPosted[sev])
				sum += g.FindingsPosted[sev]
			}
			if sum == 0 {
				return nil
			}
			return row
		}))

	var durs [][]string
	for _, role := range statsOrdered(r.Total.Durations, statsRoundRole) {
		d := r.Total.Durations[role]
		durs = append(durs, []string{actClean(role), strconv.Itoa(d.N),
			tui.HumanDuration(time.Duration(d.MedianSeconds) * time.Second), tui.HumanDuration(time.Duration(d.P90Seconds) * time.Second)})
	}
	statsTable(w, "DURATIONS", []string{"ROLE", "N", "MEDIAN", "P90"}, durs)

	var top [][]string
	for _, p := range r.TopPRs {
		top = append(top, []string{actClean(p.Repo) + "#" + strconv.Itoa(p.Number), tui.HumanDuration(time.Duration(p.AgentSeconds) * time.Second),
			strconv.Itoa(p.Rounds), statsPercent(p.Share)})
	}
	statsTable(w, "TOP PRS BY AGENT TIME", []string{"PR", "AGENT TIME", "ROUNDS", "SHARE"}, top)

	var srcs [][]string
	for _, name := range statsOrdered(r.Total.Sources, statsJudgeSource) {
		s := r.Total.Sources[name]
		accepted := "-"
		if s.Judged > 0 {
			accepted = fmt.Sprintf("%d%%", (200*s.Posted+s.Judged)/(2*s.Judged))
		}
		rejected := strconv.Itoa(s.Rejected)
		if reasons := statsCounts(s.Reasons, ""); reasons != "" {
			rejected += ": " + reasons
		}
		srcs = append(srcs, []string{actClean(name), strconv.Itoa(s.Judged), strconv.Itoa(s.Posted), strconv.Itoa(s.Unique), accepted, rejected})
	}
	statsTable(w, "SOURCES", []string{"SOURCE", "JUDGED", "POSTED", "UNIQUE", "ACCEPTED", "REJECTED"}, srcs)

	statsTable(w, "OPERATIONS", []string{"DAY", "REPO", "MODEL SWITCHES", "DENIES", "RESTARTS"},
		statsGroupRows(r, func(g statsGroup) []string {
			if g.ModelSwitches+g.Denies+g.Restarts == 0 {
				return nil
			}
			return []string{strconv.Itoa(g.ModelSwitches), strconv.Itoa(g.Denies), strconv.Itoa(g.Restarts)}
		}))
}

// statsPercent is a share (a fraction) as a whole percent; "<1%" for a small
// one that is not nothing.
func statsPercent(share float64) string {
	if pct := int(math.Round(100 * share)); pct > 0 || share == 0 {
		return strconv.Itoa(pct) + "%"
	}
	return "<1%"
}

// statsGroupRows is one row per group cols returns cells for (day and repo
// first), then the total as "all"; no rows when no group has cells.
func statsGroupRows(r statsReport, cols func(statsGroup) []string) [][]string {
	var rows [][]string
	for _, g := range r.Groups {
		if cells := cols(g); cells != nil {
			rows = append(rows, append([]string{g.Day, actClean(g.Repo)}, cells...))
		}
	}
	if len(rows) == 0 {
		return nil
	}
	return append(rows, append([]string{"all", ""}, cols(r.Total)...))
}

// statsTable prints a titled table after a blank line; nothing without rows.
func statsTable(w io.Writer, title string, head []string, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s\n", title)
	tw := inspTable(w)
	fmt.Fprintln(tw, strings.Join(head, "\t"))
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	tw.Flush()
}

// statsCounts lists counts as "name n, ...", most first then by name, leaving
// out the name skip (the outcome "posted" has its own column).
func statsCounts(counts map[string]int, skip string) string {
	names := slices.SortedFunc(maps.Keys(counts), func(a, b string) int {
		return cmp.Or(cmp.Compare(counts[b], counts[a]), cmp.Compare(a, b))
	})
	var parts []string
	for _, name := range names {
		if name != skip && counts[name] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", actClean(name), counts[name]))
		}
	}
	return strings.Join(parts, ", ")
}

// statsOrdered returns m's keys sorted, with last (when present) at the end.
func statsOrdered[V any](m map[string]V, last string) []string {
	tail := func(s string) int { // 1 for last, 0 for the rest
		if s == last {
			return 1
		}
		return 0
	}
	return slices.SortedFunc(maps.Keys(m), func(a, b string) int {
		return cmp.Or(cmp.Compare(tail(a), tail(b)), cmp.Compare(a, b))
	})
}
