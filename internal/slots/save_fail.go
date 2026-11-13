package slots

// A save before a discard that fails (its folder, the patch, the listing
// of the untracked files, a write into it, saved.txt), or a listing of the
// tree's changes that fails, fails the step, which discards nothing and
// retries at its next attempt (a round's checkout, a release resumed): a
// slot whose save keeps failing never switches, resets or goes. That is
// raised once per slot, not at every retry: the first failure of its kind
// is recorded in the kv store (saveFailedKey), with a slot.save_failed
// event at level error and the alert of SetSaveAlert (the review daemon's
// toast); the retries that fail the same way add nothing, and `magnum
// doctor` names the slot while the record stands (SaveFailures). The
// step's next save that works, or nothing left to save, clears it with a
// slot.save_ok event; the slot's removal deletes it with none
// (markRemoved). A forced removal goes on over a failure, which it does
// not raise: its slot.discarded warning, written before the removal, says
// what it deletes unsaved and names the part of the save kept
// (noteRemoval, keepPartial).

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// The kinds of a SaveFailure: the save itself, or the listing of the
// tree's changes it starts from.
const (
	SaveFailedSave    = "save"
	SaveFailedListing = "listing"
)

// SaveFailure is a slot's save before a discard that fails: the slot, the
// kind, the step it stops (its words: "the switch to PR #8's head
// 1a2b3c4"), the error's first line and since when.
type SaveFailure struct {
	Slot  string    `json:"slot"`
	Kind  string    `json:"kind"`
	What  string    `json:"what"`
	Error string    `json:"error"`
	Since time.Time `json:"since"`
}

// Sentence says what fails, since when and that the step retries.
func (f SaveFailure) Sentence() string {
	what := "the save of its changes"
	if f.Kind == SaveFailedListing {
		what = "the listing of its changes"
	}
	return fmt.Sprintf("%s: %s before %s has failed since %s (%s); the step discards nothing and retries at each attempt until it works",
		f.Slot, what, f.What, f.Since.Local().Format("2006-01-02 15:04"), f.Error)
}

// saveFailedKey is the kv key of slot id's SaveFailure.
func saveFailedKey(id int64) string { return fmt.Sprintf("slot.%d.save_failed", id) }

// SaveFailedGlob matches every slot's SaveFailure key (a GLOB of the kv
// store), for a reader of another store than this package's (doctor's of
// the factory's agents store).
const SaveFailedGlob = "slot.*.save_failed"

// SetSaveAlert names what raises a slot's save that fails, once per slot
// and kind (the review daemon's toast); nil raises none but the event.
func (m *Manager) SetSaveAlert(alert func(ctx context.Context, f SaveFailure)) {
	m.saveAlert.Store(&alert)
}

// saveFailed raises err, the failure of kind of slot sl's save before what,
// once: unless the slot's record already holds a failure of that kind, it
// records it, writes a slot.save_failed event and calls the alert. It
// returns err.
func (m *Manager) saveFailed(ctx context.Context, sl store.Slot, kind, what string, err error) error {
	if sl.ID == 0 || m.d.DryRun || ctx.Err() != nil {
		return err
	}
	if old, ok := m.saveFailure(ctx, sl.ID); ok && old.Kind == kind {
		return err
	}
	f := SaveFailure{Slot: sl.Name, Kind: kind, What: what, Error: failureLine(err), Since: m.now()}
	b, jerr := json.Marshal(f)
	if jerr == nil {
		jerr = m.d.Store.SetKV(context.WithoutCancel(ctx), saveFailedKey(sl.ID), string(b))
	}
	if jerr != nil {
		m.logErr(ctx, jerr, "slots: record the failed save of %s: %v", sl.Name, jerr)
	}
	m.event(ctx, "slot:"+sl.Name, "error", "slot.save_failed", f.Sentence())
	if alert := m.saveAlert.Load(); alert != nil && *alert != nil {
		(*alert)(ctx, f)
	}
	return err
}

// failureLine is what a record or an event says of err, a save's or a
// listing's: its first line, redacted, 300 characters at most.
func failureLine(err error) string {
	return textx.Clip(textx.FirstLine(execx.Redact(err.Error())), 300)
}

// saveWorked clears slot sl's recorded save failure, if any, with a
// slot.save_ok event that says why (its save works again, nothing is left
// to save).
func (m *Manager) saveWorked(ctx context.Context, sl store.Slot, why string) {
	if sl.ID == 0 || m.d.DryRun {
		return
	}
	f, ok := m.saveFailure(ctx, sl.ID)
	if !ok {
		return
	}
	if err := m.d.Store.DeleteKV(context.WithoutCancel(ctx), saveFailedKey(sl.ID)); err != nil {
		m.logErr(ctx, err, "slots: clear the failed save of %s: %v", sl.Name, err)
		return
	}
	m.event(ctx, "slot:"+sl.Name, "info", "slot.save_ok",
		fmt.Sprintf("%s: %s (its %s had failed since %s)", sl.Name, why, f.Kind, f.Since.Local().Format("2006-01-02 15:04")))
}

// saveFailure is slot id's recorded SaveFailure.
func (m *Manager) saveFailure(ctx context.Context, id int64) (SaveFailure, bool) {
	v, ok, err := m.d.Store.GetKV(ctx, saveFailedKey(id))
	if err != nil || !ok {
		return SaveFailure{}, false
	}
	return ParseSaveFailure(v)
}

// ParseSaveFailure reads a SaveFailure record (the value of a key
// SaveFailedGlob matches); false for one that is not.
func ParseSaveFailure(v string) (SaveFailure, bool) {
	var f SaveFailure
	if json.Unmarshal([]byte(v), &f) != nil || f.Slot == "" {
		return SaveFailure{}, false
	}
	return f, true
}

// SaveFailures lists the slots of st whose save before a discard fails,
// by slot name; never a removed slot's, whose removal is done (a record a
// removal left that ended with no save, or that is not this package's).
func SaveFailures(ctx context.Context, st *store.Store) ([]SaveFailure, error) {
	vals, err := st.KVValues(ctx, SaveFailedGlob)
	if err != nil {
		return nil, err
	}
	var out []SaveFailure
	for key, v := range vals {
		if f, ok := ParseSaveFailure(v); ok && !slotRemoved(ctx, st, key) {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b SaveFailure) int { return strings.Compare(a.Slot, b.Slot) })
	return out, nil
}

// slotRemoved reports whether the slot of save failure key (saveFailedKey)
// is removed in st; a key of no slot row, or one that cannot be read, is
// not.
func slotRemoved(ctx context.Context, st *store.Store, key string) bool {
	id := strings.TrimSuffix(strings.TrimPrefix(key, "slot."), ".save_failed")
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || saveFailedKey(n) != key {
		return false
	}
	sl, err := st.SlotByID(ctx, n)
	return err == nil && sl.State == store.SlotRemoved
}
