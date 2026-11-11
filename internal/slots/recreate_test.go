package slots

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/store"
)

// fkRefusal is what db:schema:load printed when a table the base schema
// does not know (a PR's) referred to one it drops: 2026-11-04, review14
// stayed broken 32 hours on it.
const fkRefusal = "ActiveRecord::StatementInvalid: Trilogy::ProtocolError: 3730: Cannot drop table 'accounts' referenced by " +
	"a foreign key constraint 'fk_rails_c239f4c879' on table 'ai_credentials'. (trilogy_query_recv)"

func TestForeignKeyDropRefusedReadsMySQLs3730(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]bool{
		fkRefusal: true,
		"Mysql2::Error: Cannot drop table 'sites' referenced by a foreign key constraint 'fk_x' on table 'y'.":     true,
		"ERROR 3730 (HY000): Cannot drop table 'sites' referenced by a foreign key constraint 'fk_x' on table 'y'": true,
		"ActiveRecord::NoDatabaseError: Unknown database 'talkable_development__review1'":                          false,
		"Cannot add or update a child row: a foreign key constraint fails":                                         false,
		"loaded 3730 rows": false,
	} {
		if got := ForeignKeyDropRefused(text); got != want {
			t.Errorf("ForeignKeyDropRefused(%q) = %v, want %v", text, got, want)
		}
	}
}

// A release whose schema load MySQL refuses with 3730 empties the slot's
// databases (its own list, through the pool's drop guard: drop, then
// create) and loads the schema once more: the slot is free again, with
// begin and ok events of the recreation. A second refusal breaks the slot
// as today, and its error names 3730.
func TestAReleaseResetThatMySQLRefusesWith3730RecreatesTheSlotsDatabases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		failures int
		broken   bool
	}{
		{"the load passes on the empty databases", 1, false},
		{"the load fails again", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.eager()
			sl, _ := h.claimedCheckout(7, h.shaPR7)
			h.my.add("1", "talkable_development__review2", "other_development__review1") // not the slot's
			h.failStderr = fkRefusal
			h.failScript[h.pool.ResetDB[0]] = tc.failures
			h.clearScripts()
			err := h.m.Release(h.ctx, sl, h.pool, "pr_closed")
			want := h.pool.DBNames(SlotDBSlug(sl))
			if got := h.my.recreates(); !slices.Equal(got, want) {
				t.Fatalf("recreated %v, want the slot's own databases %v", got, want)
			}
			if !h.my.has("talkable_development__review2") || !h.my.has("other_development__review1") {
				t.Fatal("a database outside the slot's list was touched")
			}
			if n := len(h.scriptCalls(h.pool.ResetDB[0])); n != 2 {
				t.Fatalf("schema loads = %d, want the first and one more", n)
			}
			got := h.slot(sl.Name)
			if !tc.broken {
				if err != nil || got.State != store.SlotFree || got.DirtySchema {
					t.Fatalf("Release = %v, slot %s dirty=%v; want it free with the schema loaded", err, got.State, got.DirtySchema)
				}
				if phases := h.stepPhases("slot:"+sl.Name+":recreate_dbs", "recreate_dbs"); !slices.Equal(phases, []string{store.PhaseBegin, store.PhaseOK}) {
					t.Fatalf("recreate_dbs phases = %v, want begin and ok", phases)
				}
				return
			}
			if !errors.Is(err, ErrBroken) || got.State != store.SlotBroken || !strings.Contains(err.Error(), "MySQL error 3730") ||
				!strings.Contains(store.Deref(got.LastError), "MySQL error 3730") {
				t.Fatalf("Release = %v, slot %s last_error %q; want broken, naming MySQL error 3730", err, got.State, store.Deref(got.LastError))
			}
		})
	}
}

// A recreation drops only the databases the slot's pool lists for its slug,
// and only through the pool's guard; without a list, a guard or a MySQL
// that can recreate, it drops nothing and says why.
func TestRecreateDatabasesNeverDropsADatabaseOutsideTheSlotsList(t *testing.T) {
	h := newHarness(t)
	h.provisioned(1)
	sl := h.slot(h.pool.Slot(1))
	for name, pool := range map[string]config.Pool{
		"no databases": func() config.Pool { p := h.pool; p.Databases = nil; return p }(),
		"no guard":     func() config.Pool { p := h.pool; p.SlotName = "review"; return p }(),
	} {
		if err := h.m.RecreateDatabases(h.ctx, sl, pool); err == nil {
			t.Errorf("%s: recreated", name)
		}
	}
	m := h.newManager(Deps{})
	m.d.MySQL = dropOnlyMySQL{h.my}
	if err := m.RecreateDatabases(h.ctx, sl, h.pool); err == nil {
		t.Error("recreated through a MySQL that cannot")
	}
	if got := h.my.recreates(); len(got) != 0 {
		t.Fatalf("recreated %v, want nothing", got)
	}
}

// dropOnlyMySQL is a MySQL without Recreate.
type dropOnlyMySQL struct{ f *fakeMySQL }

func (d dropOnlyMySQL) ListPrefixed(ctx context.Context, prefixes []string) ([]mysqlx.Database, error) {
	return d.f.ListPrefixed(ctx, prefixes)
}

func (d dropOnlyMySQL) SchemaMigrationsMax(ctx context.Context, db string) (string, error) {
	return d.f.SchemaMigrationsMax(ctx, db)
}

func (d dropOnlyMySQL) DropAll(ctx context.Context, names []string, g mysqlx.Guard) []mysqlx.DropResult {
	return d.f.DropAll(ctx, names, g)
}

// stepPhases lists the phases of step name's events of subject, oldest
// first.
func (h *harness) stepPhases(subject, name string) []string {
	h.t.Helper()
	evs, err := h.st.EventsBySubject(h.ctx, subject, 0)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, e := range evs {
		if e.Kind == store.KindStep && store.Deref(e.Step) == name {
			out = append(out, store.Deref(e.Phase))
		}
	}
	return out
}
