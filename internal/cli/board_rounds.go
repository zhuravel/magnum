package cli

// What the board's card says about a PR's reviews: the roles its last round
// ran and why (read from the round's events), and the agent time its runs
// took over the last week.

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/tui"
)

// boardRoundsWindow is how far back the card's agent time counts and how far
// back its round events are read: a round older than that says nothing.
const boardRoundsWindow = 7 * 24 * time.Hour

// The events a round leaves, as the engine writes them: engine.round_start
// (round.go, prepare) with {"kind", "roles", "requested", "post_merge"};
// round.triage (triage.go), before it, with {"runs", "skips", "reason"} for a
// decision or {"why"} when every role runs; round.rerun_role (rerun.go),
// before triage, with {"role", "lines"}. Triage and the reruns run during the
// round's setup, so the ones between the previous round's start and this
// round's start are this round's.
const (
	evRoundStart = "engine.round_start"
	evTriage     = "round.triage"
	evRerunRole  = "round.rerun_role"
)

// boardRoundFacts fills the rows' RoundWhy and Spend (ids[i] is rows[i]'s PR
// id): one query for the agent time of every row's PR and one for the round
// events of the window, whatever the rows are.
func boardRoundFacts(ctx context.Context, st *store.Store, ids []int64, rows []tui.PRBoardRow, now time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	since := now.Add(-boardRoundsWindow)
	spend, err := st.AgentTimeSince(ctx, since, now, ids...)
	if err != nil {
		return err
	}
	evs, err := st.EventsOfKindsSince(ctx, since, evRoundStart, evTriage, evRerunRole)
	if err != nil {
		return err
	}
	rowOf := make(map[string]int, len(ids)) // by the PR's event subject
	for i := range min(len(ids), len(rows)) {
		if r := rows[i]; r.Owner != "" && r.Repo != "" && r.Number > 0 {
			rowOf[prEventSubject(r.Owner+"/"+r.Repo, r.Number)] = i
		}
	}
	events := make([][]store.Event, len(rows))
	for _, e := range evs {
		if e.Subject == nil {
			continue
		}
		if i, ok := rowOf[*e.Subject]; ok {
			events[i] = append(events[i], e)
		}
	}
	spent := make(map[int64]store.PRAgentTime, len(spend))
	for _, s := range spend {
		spent[s.PRID] = s
	}
	for i := range min(len(ids), len(rows)) {
		rows[i].RoundWhy = roundWhyOf(events[i])
		rows[i].Spend = nil
		if s, ok := spent[ids[i]]; ok {
			rows[i].Spend = &tui.SpendInfo{Window: boardRoundsWindow, AgentTime: s.Time, Rounds: s.Rounds}
		}
	}
	return nil
}

// prEventSubject is the subject the engine gives a PR's events
// ("pr:owner/name#N", engine.prSubject).
func prEventSubject(full string, number int) string {
	return "pr:" + full + "#" + strconv.Itoa(number)
}

// roundWhyOf reads which roles a PR's last round ran and why from its round
// events (oldest first): the latest round_start and what the setup before it
// decided. Nil without a round that started. The events are read
// defensively: a part that is missing or of another type is left out.
func roundWhyOf(evs []store.Event) *tui.RoundWhy {
	last, prev := -1, -1
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind != evRoundStart {
			continue
		}
		if last < 0 {
			last = i
			continue
		}
		prev = i
		break
	}
	if last < 0 {
		return nil
	}
	start := eventData(evs[last])
	w := &tui.RoundWhy{
		Kind:      dataString(start, "kind"),
		PostMerge: dataBool(start, "post_merge"),
		Roles:     dataStrings(start, "roles"),
		Requested: dataStrings(start, "requested"),
	}
	setup := evs[prev+1 : last]

	// The newest triage that says something: a setup that failed after it
	// and was retried leaves several.
	for _, e := range slices.Backward(setup) {
		if e.Kind != evTriage {
			continue
		}
		d := eventData(e)
		if dataIsArray(d, "runs") {
			w.Triaged, w.Skipped, w.Reason = true, dataStrings(d, "skips"), dataString(d, "reason")
			break
		}
		if why := dataString(d, "why"); why != "" {
			w.EveryRole = why
			break
		}
	}
	for _, e := range setup {
		if e.Kind != evRerunRole {
			continue
		}
		d := eventData(e)
		role := dataString(d, "role")
		if role == "" {
			continue
		}
		rr := tui.RoleRerun{Role: role, Lines: dataInt(d, "lines")}
		if at := slices.IndexFunc(w.Reruns, func(x tui.RoleRerun) bool { return x.Role == role }); at >= 0 {
			w.Reruns[at] = rr // the newer count
		} else {
			w.Reruns = append(w.Reruns, rr)
		}
	}
	return w
}

// eventData is an event's data as an object; nil when it has none or is
// not one.
func eventData(e store.Event) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if len(e.Data) == 0 || json.Unmarshal(e.Data, &m) != nil {
		return nil
	}
	return m
}

func dataString(m map[string]json.RawMessage, key string) string {
	var s string
	if json.Unmarshal(m[key], &s) != nil {
		return ""
	}
	return s
}

func dataBool(m map[string]json.RawMessage, key string) bool {
	var b bool
	return json.Unmarshal(m[key], &b) == nil && b
}

func dataInt(m map[string]json.RawMessage, key string) int {
	var f float64
	if json.Unmarshal(m[key], &f) != nil {
		return 0
	}
	return int(f)
}

// dataStrings is the strings of a JSON array under key (any other element
// is left out); nil when it is no array or holds none.
func dataStrings(m map[string]json.RawMessage, key string) []string {
	var xs []any
	if json.Unmarshal(m[key], &xs) != nil {
		return nil
	}
	var out []string
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func dataIsArray(m map[string]json.RawMessage, key string) bool {
	raw := bytes.TrimSpace(m[key])
	return len(raw) > 0 && raw[0] == '['
}
