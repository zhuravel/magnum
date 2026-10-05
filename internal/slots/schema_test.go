package slots

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// hasEvent reports whether an event of kind was written.
func (h *harness) hasEvent(kind string) bool {
	h.t.Helper()
	evs, err := h.st.EventsOfKindsSince(h.ctx, time.Time{}, kind)
	if err != nil {
		h.t.Fatal(err)
	}
	return len(evs) > 0
}

// loadSchema does what a round's readiness step does with a CheckSchema that
// needs a reload: the reset_db commands load the checkout's schema (the fake
// MySQL takes db/schema.rb's version), then RecordSchema.
func (h *harness) loadSchema(sl store.Slot, c SchemaCheck) {
	h.t.Helper()
	if err := h.m.ForgetSchema(h.ctx, sl); err != nil {
		h.t.Fatal(err)
	}
	h.my.add(strings.ReplaceAll(h.schemaVersionAt(sl.Path), "_", ""), h.pool.DBNames(SlotDBSlug(sl))...)
	if err := h.m.RecordSchema(h.ctx, sl, c); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) checkSchema(sl store.Slot) SchemaCheck {
	h.t.Helper()
	c, err := h.m.CheckSchema(h.ctx, sl, h.pool)
	if err != nil {
		h.t.Fatalf("CheckSchema: %v", err)
	}
	return c
}

// A provisioned slot records the schema its setup loaded, so the first
// round of a PR that keeps the base schema does not reload it; a PR that
// changes it does, once: its second round finds the schema unchanged.
func TestCheckSchemaReloadsOnlyWhenTheCheckoutNeedsAnotherSchema(t *testing.T) {
	h := newHarness(t)
	sl := h.provisioned(1)
	if sl.SchemaFP == nil || store.Deref(sl.SchemaSHA) != h.shaMaster || store.Deref(sl.SchemaVersion) != strings.ReplaceAll(schemaV1, "_", "") {
		t.Fatalf("provisioned schema = %v %v %v, want master's", sl.SchemaFP, sl.SchemaSHA, sl.SchemaVersion)
	}
	pr8 := h.pr(h.repo().ID, 8, h.shaPR8, store.PRQueued)
	claimed, err := h.m.Claim(h.ctx, pr8, h.pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.m.Checkout(h.ctx, claimed, pr8, h.pool, h.shaPR8); err != nil {
		t.Fatal(err)
	}
	c := h.checkSchema(claimed)
	if want := "schema unchanged since " + textx.ShortSHA(h.shaMaster) + ": no reset"; c.Need || c.Why != want {
		t.Fatalf("a README-only PR on a fresh slot: %+v, want no reload (%q)", c, want)
	}
	if err := h.m.Release(h.ctx, h.slot(claimed.Name), h.pool, "evicted"); err != nil {
		t.Fatal(err)
	}

	sl, pr7 := h.claimAgain(t, 7, h.shaPR7)
	c = h.checkSchema(sl)
	if !c.Need || c.SHA != h.shaPR7 || c.Version != strings.ReplaceAll(schemaV2, "_", "") {
		t.Fatalf("a PR that changes the schema: %+v, want a reload of PR 7's", c)
	}
	h.loadSchema(sl, c)

	// Its second round: the same checkout, nothing to reload.
	if err := h.m.Checkout(h.ctx, h.slot(sl.Name), pr7, h.pool, h.shaPR7); err != nil {
		t.Fatal(err)
	}
	if again := h.checkSchema(sl); again.Need || again.Why != "schema unchanged since "+textx.ShortSHA(h.shaPR7)+": no reset" {
		t.Fatalf("second round of the same PR: %+v, want no reload", again)
	}

	// A push that changes the schema again reloads.
	writeFile(t, filepath.Join(sl.Path, "db", "schema.rb"), schemaRB("2026_09_30_000000"))
	gitT(t, sl.Path, "commit", "--quiet", "-am", "another migration")
	if c := h.checkSchema(sl); !c.Need || c.Why != "its databases carry the schema of "+textx.ShortSHA(h.shaPR7) {
		t.Fatalf("a push that changes the schema again: %+v, want a reload", c)
	}
}

// The release keeps whatever schema the databases carry; the next claim
// reloads only when its PR needs another one.
func TestReleaseKeepsTheSchemaForTheNextClaimToCompare(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(7, h.shaPR7)
	h.loadSchema(sl, h.checkSchema(sl))
	h.clearScripts()
	if err := h.m.Release(h.ctx, h.slot(sl.Name), h.pool, "pr_closed"); err != nil {
		t.Fatal(err)
	}
	if n := len(h.scriptCalls(h.pool.ResetDB[0])); n != 0 {
		t.Fatalf("release ran reset_db %d times, want none", n)
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotFree || got.DirtySchema || store.Deref(got.SchemaSHA) != h.shaPR7 {
		t.Fatalf("released slot = %s dirty %v schema of %s, want free with PR 7's schema", got.State, got.DirtySchema, store.Deref(got.SchemaSHA))
	}

	// A PR on the base schema claims it next: its readiness reloads.
	sl8, _ := h.claimAgain(t, 8, h.shaPR8)
	if sl8.ID != sl.ID {
		t.Fatalf("claimed %s, want the released %s", sl8.Name, sl.Name)
	}
	if c := h.checkSchema(sl8); !c.Need || c.SHA != h.shaPR8 || c.Why != "its databases carry the schema of "+textx.ShortSHA(h.shaPR7) {
		t.Fatalf("next claim of a PR with another schema: %+v, want a reload", c)
	}
}

// The safety net: a development database whose schema_migrations no longer
// match the recorded schema (a migration run by hand or by an agent) is
// reloaded, at the next round and at the release, which then records the
// base schema.
func TestSchemaSafetyNetCatchesAMigratedDatabase(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	h.my.add("20991231000000", "talkable_development__review1")
	if c := h.checkSchema(sl); !c.Need || !strings.Contains(c.Why, `talkable_development__review1 is at "20991231000000"`) {
		t.Fatalf("CheckSchema of a migrated database: %+v, want a reload", c)
	}
	h.clearScripts()
	if err := h.m.Release(h.ctx, sl, h.pool, "evicted"); err != nil {
		t.Fatal(err)
	}
	if n := len(h.scriptCalls(h.pool.ResetDB[0])); n != 1 {
		t.Fatalf("release ran reset_db %d times, want 1", n)
	}
	got := h.slot(sl.Name)
	if got.State != store.SlotFree || store.Deref(got.SchemaSHA) != h.shaMaster || store.Deref(got.SchemaVersion) != strings.ReplaceAll(schemaV1, "_", "") {
		t.Fatalf("slot = %s schema of %s version %s, want free with master's", got.State, store.Deref(got.SchemaSHA), store.Deref(got.SchemaVersion))
	}
}

// A slot whose schema was never recorded (one the previous binary
// released) reloads at its next round.
func TestCheckSchemaReloadsAnUnknownSchema(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(8, h.shaPR8)
	if err := h.m.ForgetSchema(h.ctx, sl); err != nil {
		t.Fatal(err)
	}
	if c := h.checkSchema(sl); !c.Need || c.Why != "the schema its databases carry is unknown" {
		t.Fatalf("unknown schema: %+v, want a reload", c)
	}
}

// reset_db_on_schema_change = false keeps today's release: it loads the base
// schema when the PR changed it, and records the base schema.
func TestReleaseReloadsTheBaseSchemaWhenTheResetIsNotLazy(t *testing.T) {
	h := newHarness(t)
	h.eager()
	sl, _ := h.claimedCheckout(7, h.shaPR7)
	h.clearScripts()
	if err := h.m.Release(h.ctx, sl, h.pool, "pr_closed"); err != nil {
		t.Fatal(err)
	}
	var scripts []string
	for _, c := range h.scriptCalls("") {
		scripts = append(scripts, c.Script)
		if strings.Contains(c.Script, "db:") && (c.Cmd.Timeout != ResetDBTimeout || !c.Cmd.Mutates || c.Env["WT_BRANCH"] != "review1") {
			t.Fatalf("reset cmd = %+v", c.Cmd)
		}
	}
	if want := []string{h.pool.PostCheckout[0], h.pool.ResetDB[0], h.pool.ResetDB[1]}; !slices.Equal(scripts, want) {
		t.Fatalf("scripts = %q, want %q", scripts, want)
	}
	if got := h.slot(sl.Name); got.State != store.SlotFree || got.DirtySchema || store.Deref(got.SchemaSHA) != h.shaMaster {
		t.Fatalf("slot = %s dirty %v schema of %s, want free with master's", got.State, got.DirtySchema, store.Deref(got.SchemaSHA))
	}
}

// magnum open hands a person a slot whose databases fit the checkout:
// EnsureSchema reloads them when they carry another schema, and leaves them
// when they do not.
func TestEnsureSchemaLoadsTheCheckoutsSchemaForAPerson(t *testing.T) {
	h := newHarness(t)
	sl, _ := h.claimedCheckout(7, h.shaPR7)
	h.clearScripts()
	note, err := h.m.EnsureSchema(h.ctx, sl, h.pool)
	if err != nil || !strings.HasPrefix(note, "loaded the schema of "+textx.ShortSHA(h.shaPR7)) {
		t.Fatalf("EnsureSchema = %q, %v", note, err)
	}
	if n := len(h.scriptCalls(h.pool.ResetDB[0])); n != 1 {
		t.Fatalf("reset_db ran %d times, want 1", n)
	}
	if got := h.slot(sl.Name); store.Deref(got.SchemaSHA) != h.shaPR7 {
		t.Fatalf("recorded schema of %s, want PR 7's", store.Deref(got.SchemaSHA))
	}
	h.clearScripts()
	if note, err := h.m.EnsureSchema(h.ctx, sl, h.pool); err != nil || !strings.HasPrefix(note, "schema unchanged since") {
		t.Fatalf("second EnsureSchema = %q, %v", note, err)
	}
	if n := len(h.scriptCalls("")); n != 0 {
		t.Fatalf("an unchanged schema ran %d scripts", n)
	}

	// A failed reload leaves the schema unknown.
	writeFile(t, filepath.Join(sl.Path, "db", "schema.rb"), schemaRB("2026_09_30_000000"))
	gitT(t, sl.Path, "commit", "--quiet", "-am", "another migration")
	h.failScript[h.pool.ResetDB[0]] = 1
	if _, err := h.m.EnsureSchema(h.ctx, sl, h.pool); err == nil {
		t.Fatal("a failed reload returned no error")
	}
	if got := h.slot(sl.Name); got.SchemaFP != nil {
		t.Fatalf("schema after a failed reload = %v, want unknown", store.Deref(got.SchemaSHA))
	}
	if got := h.slot(sl.Name); got.State != store.SlotClaimed {
		t.Fatalf("slot after a failed reload = %s, want claimed", got.State)
	}
	if note, err := h.m.EnsureSchema(h.ctx, store.Slot{ID: sl.ID, Name: sl.Name, Kind: store.SlotKindPerPR}, h.pool); err != nil || note != "" {
		t.Fatalf("per-PR worktree: %q, %v", note, err)
	}
}
