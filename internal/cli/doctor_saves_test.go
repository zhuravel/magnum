package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/slots"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// recordSaveFailure writes f as slot id's save failure in st, as the slots
// package records it.
func recordSaveFailure(t *testing.T, st *store.Store, id int64, f slots.SaveFailure) {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetKV(context.Background(), fmt.Sprintf("slot.%d.save_failed", id), string(b)); err != nil {
		t.Fatal(err)
	}
}

// Doctor names every slot whose save before a discard keeps failing, a
// review slot's in the registry as a factory copy's in the factory's agents
// store, with what fails, since when and that the step retries; with none
// it passes. Nothing showed a slot that could not switch but its round's
// failures.
func TestDoctorNamesTheSlotsWhoseSaveFails(t *testing.T) {
	home := t.TempDir()
	d := doctorDeps{Layout: paths.Layout{Home: home}, Store: storetest.Open(t, filepath.Join(home, "state", "magnum.db"))}
	if got := doctorByName(doctorSaves(context.Background(), d))["saves"]; got.Status != doctorPass {
		t.Fatalf("no failing save: %+v, want a PASS", got)
	}

	since := time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC)
	recordSaveFailure(t, d.Store, 3, slots.SaveFailure{Slot: "review3", Kind: slots.SaveFailedSave,
		What: "the switch to PR #8's head 1a2b3c4", Error: "mkdir /x: not a directory", Since: since})
	recordSaveFailure(t, storetest.OpenFile(t, d.Layout.FactoryAgentsDB()), 5, slots.SaveFailure{Slot: "factory5",
		Kind: slots.SaveFailedListing, What: "the release's reset to origin/master", Error: "git status: exit 128", Since: since})

	got := doctorByName(doctorSaves(context.Background(), d))
	for name, want := range map[string][]string{
		"save review3":  {"review3", "the save of its changes", "the switch to PR #8's head", "not a directory", "retries"},
		"save factory5": {"factory5", "the listing of its changes", "the release's reset", "exit 128", "retries"},
	} {
		c := got[name]
		if c.Status != doctorWarn || c.Fix == "" {
			t.Fatalf("%s: %+v, want a WARN with its fix", name, c)
		}
		for _, w := range want {
			if !strings.Contains(c.Detail, w) {
				t.Fatalf("%s: %q does not say %q", name, c.Detail, w)
			}
		}
	}
	if _, ok := got["saves"]; ok {
		t.Fatalf("a PASS beside the failing saves: %+v", got)
	}
}
