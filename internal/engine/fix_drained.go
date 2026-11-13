package engine

// The drain proof (DECISIONS "The fix loop moves into the factory"): a PR
// changes owner only at a quiet point, when none of magnum's fix work on it
// is in flight (fixQuiet). Its records are the registry's: a loop's row
// (KVPRFix), a fixer's push record (kvPRFixPush), a held merge
// (KVPRHeldMerge), a held fix (KVPRFixHeld), magnum's push waiting for its
// CI (kvPRPushed), a pending `magnum fix` or `magnum abort` request, a fix
// run whose verdict is not written; and the daemon's own, a fix job's
// reservation of the PR. Each record names what it is and what the
// operator does about it, for the morning list (`magnum fix --drained`)
// and the doctor: most end by themselves, a held merge waits for P or a
// drop, and a push record that the PR's head left behind for `magnum fix
// --drop-push`.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// Kinds of fix work in flight (FixRecord.Kind).
const (
	FixRecLoop        = "loop"        // a fix loop's row
	FixRecPush        = "push"        // a fixer's push record, to confirm or to settle late
	FixRecHeldMerge   = "held_merge"  // a merge of the parent held for your y
	FixRecHeldFix     = "held_fix"    // a fix held for a restart, a pause or the GitHub budget
	FixRecPushed      = "pushed"      // magnum's push, waiting for its CI start
	FixRecRequest     = "request"     // a pending `magnum fix` or `magnum abort` request
	FixRecRun         = "run"         // a fix run whose verdict is not written
	FixRecReservation = "reservation" // a fix job holds the PR now (the daemon's alone)
)

// FixRecord is one record of magnum's fix work in flight on a PR, which
// keeps it from handing over to the factory: its Kind, What it is and the
// operator's Action about it, in words.
type FixRecord struct {
	Kind   string `json:"kind"`
	What   string `json:"what"`
	Action string `json:"action"`
}

// UndrainedPR is a PR with fix work in flight (FixInFlight): its row's id,
// repository, number and link, whether a handover waits for it (Handing),
// and its records.
type UndrainedPR struct {
	ID      int64       `json:"id"`
	Repo    string      `json:"repo"`
	Number  int         `json:"number"`
	URL     string      `json:"url"`
	Handing bool        `json:"handing,omitempty"`
	Records []FixRecord `json:"records"`
}

// Label is the PR as owner/name#N.
func (u UndrainedPR) Label() string { return fmt.Sprintf("%s#%d", u.Repo, u.Number) }

// fixFlight is the registry's fix work in flight, by PR id: the rows by
// record kind (as stored), the pending fix and abort requests, the fix
// runs whose verdict is not written.
type fixFlight struct {
	rows     map[int64]map[string]string
	requests map[int64][]store.Request
	runs     map[int64][]store.Run
}

// flightKeys are the rows of fix work in flight: their record kind, the
// key of a PR's row and its glob.
var flightKeys = []struct {
	kind string
	key  func(int64) string
}{
	{FixRecLoop, KVPRFix}, {FixRecPush, kvPRFixPush}, {FixRecHeldMerge, KVPRHeldMerge}, {FixRecHeldFix, KVPRFixHeld}, {FixRecPushed, kvPRPushed},
}

// readFixFlight reads the fix work in flight from st: of PR prID alone,
// or of every PR (prID 0). A pending request names its PR by a ref, which
// parser resolves against the registry; one that names no PR it knows is
// none's.
func readFixFlight(ctx context.Context, st *store.Store, prID int64, parser app.RefParser) (fixFlight, error) {
	f := fixFlight{rows: map[int64]map[string]string{}, requests: map[int64][]store.Request{}, runs: map[int64][]store.Run{}}
	add := func(id int64, kind, raw string) {
		if f.rows[id] == nil {
			f.rows[id] = map[string]string{}
		}
		f.rows[id][kind] = raw
	}
	for _, k := range flightKeys {
		if prID != 0 {
			raw, ok, err := st.GetKV(ctx, k.key(prID))
			if err != nil {
				return fixFlight{}, err
			}
			if ok {
				add(prID, k.kind, raw)
			}
			continue
		}
		glob := strings.Replace(k.key(0), ".0.", ".*.", 1)
		vals, err := st.KVValues(ctx, glob)
		if err != nil {
			return fixFlight{}, err
		}
		for key, raw := range vals {
			var id int64
			if _, err := fmt.Sscanf(key, "pr.%d.", &id); err == nil && k.key(id) == key {
				add(id, k.kind, raw)
			}
		}
	}
	reqs, err := st.PendingRequests(ctx, 0)
	if err != nil {
		return fixFlight{}, err
	}
	for _, r := range reqs {
		if r.Kind != ReqFix && r.Kind != ReqAbort {
			continue
		}
		var t PRTarget
		if json.Unmarshal(r.Payload, &t) != nil {
			continue
		}
		ref := t.Ref
		if t.Repo != "" && t.Number > 0 {
			ref = fmt.Sprintf("%s#%d", t.Repo, t.Number)
		}
		if ref == "" {
			continue
		}
		if _, pr, err := app.LookupPR(ctx, st, parser, ref); err == nil && (prID == 0 || pr.ID == prID) {
			f.requests[pr.ID] = append(f.requests[pr.ID], r)
		}
	}
	runs, err := st.ActiveRuns(ctx)
	if err != nil {
		return fixFlight{}, err
	}
	for _, r := range runs {
		if r.Kind == store.RunFix && (prID == 0 || r.PRID == prID) {
			f.runs[r.PRID] = append(f.runs[r.PRID], r)
		}
	}
	return f, nil
}

// ids are the PRs with fix work in flight.
func (f fixFlight) ids() []int64 {
	var out []int64
	for id := range f.rows {
		out = append(out, id)
	}
	for id := range f.requests {
		out = append(out, id)
	}
	for id := range f.runs {
		out = append(out, id)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// records are pr's (label, its owner/name#N) fix work in flight, with the
// operator's action for each; handing: a handover waits for the PR, so a
// loop ends at its cycle boundary by itself. now dates a push record's
// keep.
func (f fixFlight) records(pr store.PR, label string, handing bool, now time.Time) []FixRecord {
	var out []FixRecord
	rows := f.rows[pr.ID]
	if raw, ok := rows[FixRecLoop]; ok {
		out = append(out, loopRecord(raw, label, handing))
	}
	if raw, ok := rows[FixRecPush]; ok {
		out = append(out, pushRecord(raw, pr, label, now))
	}
	if raw, ok := rows[FixRecHeldMerge]; ok {
		what := "a merge of the parent held for your y"
		if h, ok := ParseHeldMerge(raw); ok {
			what = fmt.Sprintf("a merge of %s at %s held for your y", h.Base, textx.ShortSHA(h.Tip))
		}
		out = append(out, FixRecord{Kind: FixRecHeldMerge, What: what,
			Action: fmt.Sprintf("P pushes it (`magnum merge-parent %s`), or `magnum merge-parent --drop %s` drops it", label, label)})
	}
	if raw, ok := rows[FixRecHeldFix]; ok {
		starts := HeldFixStarts(raw)
		out = append(out, FixRecord{Kind: FixRecHeldFix, What: "a fix held: it starts " + starts,
			Action: fmt.Sprintf("nothing: it starts %s, and its loop ends at its cycle boundary; `magnum abort %s` drops it", starts, label)})
	}
	if raw, ok := rows[FixRecPushed]; ok {
		var p ciPush
		_ = json.Unmarshal([]byte(raw), &p)
		out = append(out, FixRecord{Kind: FixRecPushed, What: fmt.Sprintf("magnum's push of %s waits for its CI start", cmp.Or(textx.ShortSHA(p.Head), "a head")),
			Action: "nothing: it goes within " + humanDuration(ciPushGrace)})
	}
	for _, r := range f.requests[pr.ID] {
		out = append(out, FixRecord{Kind: FixRecRequest, What: fmt.Sprintf("a pending `magnum %s` request (%d)", r.Kind, r.ID),
			Action: "nothing: the daemon answers it at its next tick"})
	}
	for _, r := range f.runs[pr.ID] {
		action := "nothing: it ends with its loop"
		if _, ok := rows[FixRecLoop]; !ok {
			action = fmt.Sprintf("`magnum abort %s` ends it", label)
		}
		out = append(out, FixRecord{Kind: FixRecRun, What: fmt.Sprintf("the fixer's run %s (%s)", r.ID, r.State), Action: action})
	}
	return out
}

// loopRecord is a loop's row raw as fix work in flight on the PR label.
func loopRecord(raw, label string, handing bool) FixRecord {
	rec := FixRecord{Kind: FixRecLoop, What: "a fix loop whose row does not read", Action: fmt.Sprintf("`magnum abort %s` clears it", label)}
	st, ok := ParseFixState(raw)
	switch {
	case !ok:
		return rec
	case st.By == heldPushBy:
		rec.What, rec.Action = "the push of a held merge of the parent", "nothing: it ends by itself"
		return rec
	case st.Phase == FixPhaseFixing:
		rec.What = fmt.Sprintf("a fix loop at work: its fixer runs cycle %d of %d", st.Cycle, st.Cap)
	default:
		rec.What = fmt.Sprintf("a fix loop waiting for the re-review of its push (cycle %d of %d)", st.Cycle, st.Cap)
	}
	rec.Action = "nothing: it ends at its cycle boundary, with no next cycle"
	if !handing {
		rec.Action = fmt.Sprintf("`magnum fix --handover %s` ends it at its cycle boundary, with no next cycle (`magnum abort %s` ends it now)", label, label)
	}
	return rec
}

// pushRecord is a fixer's push record raw of pr (label) as fix work in
// flight: one its loop's settle confirms ends with the loop; one a fix
// failed for want of is settled late once GitHub shows it as the PR's
// head, and is the operator's to drop otherwise.
func pushRecord(raw string, pr store.PR, label string, now time.Time) FixRecord {
	rec := FixRecord{Kind: FixRecPush, What: "a fixer's push record that does not read",
		Action: fmt.Sprintf("`magnum fix --drop-push %s` drops it", label)}
	var p fixPush
	if json.Unmarshal([]byte(raw), &p) != nil {
		return rec
	}
	short := textx.ShortSHA(p.Pushed)
	switch p.Kind {
	case fixPushConfirming:
		rec.What = fmt.Sprintf("the push of %s, which its loop's settle confirms on GitHub", short)
		rec.Action = "nothing: the settle confirms it, or fails it within " + humanDuration(fixConfirmCap)
	case fixPushUnconfirmed:
		rec.What = fmt.Sprintf("the push of %s, kept for its late settle", short)
		if !p.Failed.IsZero() {
			rec.What += " until " + waitClock(p.Failed.Add(fixPushKeep), now)
		}
		rec.Action = fmt.Sprintf("`magnum fix --drop-push %s` drops it (nothing to do once the PR's head is %s: the poll settles it)", label, short)
		if strings.EqualFold(pr.HeadSHA, p.Pushed) {
			rec.Action = "nothing: GitHub shows it as the PR's head, and the poll settles it"
		}
	default:
		rec.What = fmt.Sprintf("a fixer's push record of %s", cmp.Or(short, "no head"))
	}
	return rec
}

// requestDrop handles the morning list's drops (FixPayload.DropPush, and
// Drop with FixModeMerge), each alone: the record of a fixer's push that
// magnum keeps for its late settle, or the merge of the parent it holds
// for your y, dropped by a compare with the row read, while no round or
// job holds the PR. A push record its loop's settle confirms, or whose
// late settle began, is refused: the loop or the next poll ends it.
func (e *Engine) requestDrop(ctx context.Context, repo store.Repo, pr store.PR, p FixPayload) (string, error) {
	label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
	switch {
	case p.DropPush && (p.Drop || p.Mode != "" || p.Off || p.On || p.Done || len(p.Decide) > 0):
		return "", errors.New("magnum fix --drop-push goes alone")
	case p.Drop && (p.Mode != FixModeMerge || p.Asked != nil):
		return "", errors.New("magnum merge-parent --drop goes alone")
	case e.roundActive(pr.ID):
		return "", fmt.Errorf("%s: %s; ask again shortly", label, e.heldReason(pr.ID))
	case p.Drop:
		return e.dropHeldRequest(ctx, repo, pr, label, p.DryRun || e.d.DryRun)
	}
	key := kvPRFixPush(pr.ID)
	raw, ok, err := e.st.GetKV(ctx, key)
	switch {
	case err != nil:
		return "", err
	case !ok:
		return "", fmt.Errorf("%s: magnum keeps no fixer's push record of it", label)
	}
	var rec fixPush
	if json.Unmarshal([]byte(raw), &rec) == nil {
		switch {
		case rec.Kind == fixPushConfirming && e.confirmingLoop(ctx, pr, rec):
			return "", fmt.Errorf("%s: its loop's settle confirms the push of %s now: it ends with the loop (`magnum abort %s` ends the loop)",
				label, textx.ShortSHA(rec.Pushed), label)
		case e.pushStarted(ctx, rec):
			return "", fmt.Errorf("%s: the late settle of the push of %s began: the next poll finishes it", label, textx.ShortSHA(rec.Pushed))
		}
	}
	short := cmp.Or(textx.ShortSHA(rec.Pushed), "a head")
	if p.DryRun || e.d.DryRun {
		return fmt.Sprintf("dry run: would drop the record of the fixer's push of %s on %s: magnum would settle it no more", short, label), nil
	}
	switch gone, err := e.st.DeleteKVIf(ctx, key, raw); {
	case err != nil:
		return "", err
	case !gone:
		return "", fmt.Errorf("%s: the push record changed meanwhile; nothing dropped, ask again", label)
	}
	e.event(ctx, "info", prSubject(repo, pr.Number), "fix.push_dropped", "the record of the fixer's push of "+short+
		" is dropped by magnum fix --drop-push: magnum settles it no more", map[string]any{"pushed": rec.Pushed, "run": rec.Run})
	return fmt.Sprintf("dropped the record of the fixer's push of %s on %s: magnum settles it no more", short, label), nil
}

// dropHeldRequest drops the merge of pr's parent that magnum holds for
// your y (`magnum merge-parent --drop`): its row, ref and held-merge.diff
// (dropHeldMerge, which says so in fix.held_dropped and in the PR's
// attention).
func (e *Engine) dropHeldRequest(ctx context.Context, repo store.Repo, pr store.PR, label string, dry bool) (string, error) {
	row, ok := e.heldMergeOf(ctx, pr)
	if !ok {
		return "", fmt.Errorf("%s: magnum holds no merge of its parent", label)
	}
	what := fmt.Sprintf("the held merge %s of %s at %s on %s", textx.ShortSHA(row.h.Merge), row.h.Base, textx.ShortSHA(row.h.Tip), label)
	if dry {
		return "dry run: would drop " + what, nil
	}
	if !e.dropHeldMerge(ctx, repo, pr, row, "dropped by magnum merge-parent --drop", false) {
		return "", fmt.Errorf("%s: the held merge changed meanwhile; nothing dropped, ask again", label)
	}
	return "dropped " + what + ": nothing was pushed", nil
}

// FixInFlight is the morning list (`magnum fix --drained`, the doctor):
// every PR the registry st holds fix work in flight for, by repository and
// number, with each record's action, and whether a handover waits for it
// (cfg names the operator's own PRs and resolves the requests' refs). The
// daemon's own reservations are not in the registry: fixQuiet adds them.
func FixInFlight(ctx context.Context, st *store.Store, cfg *config.Config, now time.Time) ([]UndrainedPR, error) {
	f, err := readFixFlight(ctx, st, 0, app.RefParser{DefaultRepo: cfg.Daemon.DefaultRepo})
	if err != nil {
		return nil, err
	}
	all, err := handoverAllOf(ctx, st)
	if err != nil {
		return nil, err
	}
	self := cfg.SelfMatch()
	var out []UndrainedPR
	for _, id := range f.ids() {
		pr, err := st.PRByID(ctx, id)
		if err != nil {
			continue
		}
		repo, err := st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		_, pending, err := st.GetKV(ctx, KVPRFixHandover(id))
		if err != nil {
			return nil, err
		}
		handing := pending || (all && deref(pr.AuthorLogin) != "" && self(deref(pr.AuthorLogin)))
		label := fmt.Sprintf("%s#%d", repo.FullName(), pr.Number)
		out = append(out, UndrainedPR{ID: id, Repo: repo.FullName(), Number: pr.Number, URL: pr.URL, Handing: handing,
			Records: f.records(pr, label, handing, now)})
	}
	slices.SortFunc(out, func(a, b UndrainedPR) int {
		return cmp.Or(strings.Compare(a.Repo, b.Repo), cmp.Compare(a.Number, b.Number))
	})
	return out, nil
}

// fixRecordsOf are pr's fix work in flight that the registry holds, read
// now (readFixFlight of pr alone), their actions as for a PR that hands
// over (handing) or not.
func (e *Engine) fixRecordsOf(ctx context.Context, repo store.Repo, pr store.PR, handing bool) ([]FixRecord, error) {
	f, err := readFixFlight(ctx, e.st, pr.ID, app.RefParser{DefaultRepo: e.cfg.Daemon.DefaultRepo})
	if err != nil {
		return nil, err
	}
	return f.records(pr, fmt.Sprintf("%s#%d", repo.FullName(), pr.Number), handing, e.now()), nil
}

// fixQuiet is the drain proof of pr (of repo): true while none of magnum's
// fix work is in flight on it, the registry's records (fixRecordsOf) and a
// fix job's reservation of the PR; otherwise the records say what is. A
// failed read is no proof.
func (e *Engine) fixQuiet(ctx context.Context, repo store.Repo, pr store.PR) (bool, []FixRecord, error) {
	handing, _ := e.handingOver(ctx, pr)
	return e.fixQuietAs(ctx, repo, pr, handing)
}

// fixQuietAs is fixQuiet with the records' actions as for a PR that hands
// over (handing) or not, as a handover that asks it says.
func (e *Engine) fixQuietAs(ctx context.Context, repo store.Repo, pr store.PR, handing bool) (bool, []FixRecord, error) {
	recs, err := e.fixRecordsOf(ctx, repo, pr, handing)
	if err != nil {
		return false, nil, err
	}
	e.mu.Lock()
	h := e.rounds[pr.ID]
	e.mu.Unlock()
	if h != nil && h.fix {
		recs = append(recs, FixRecord{Kind: FixRecReservation, What: "a fix job holds the PR now", Action: "nothing: it lets go when the job ends"})
	}
	return len(recs) == 0, recs, nil
}
