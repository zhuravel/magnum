package engine

import (
	"slices"
	"testing"
)

// A round that reloads its slot's schema can recreate the slot's databases
// when MySQL refuses the load with 3730 (a table the schema does not know
// refers to one the load drops): the plan's Recreate empties that slot's
// databases, as its pool lists them (Slots.RecreateDatabases), and the
// pipeline runs the reset once more. Before, review14 stayed broken 32
// hours on such a refusal.
func TestARoundThatReloadsTheSchemaCanRecreateItsSlotsDatabases(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		p := &h.cfg.Pools[0]
		p.SchemaPaths, p.ResetDB = []string{"db/"}, []string{"bin/rails db:schema:load"}
	})
	h.reviewedPR(2, "b1")
	ins := h.rd.all()
	if len(ins) != 1 {
		t.Fatalf("%d rounds ran", len(ins))
	}
	rd := ins[0].Readiness
	if len(rd.ResetDB) == 0 || rd.Recreate == nil {
		t.Fatalf("the round reloads the schema (%q) without Recreate", rd.ResetDB)
	}
	if err := rd.Recreate(h.ctx); err != nil { // the pipeline's call
		t.Fatal(err)
	}
	if want := "recreate_dbs:review1:" + h.cfg.Pools[0].Repo; !slices.Contains(h.sl.all(), want) {
		t.Fatalf("slots calls %q, want %q", h.sl.all(), want)
	}
}
