package slots

// The schema a pool slot's databases carry (store.Slot.SchemaFP, SchemaSHA,
// SchemaVersion): the fingerprint of the files under the pool's
// schema_paths at the commit they were last loaded from. One reload
// (reset_db) rewrites every table, about 0.3-0.5 GB of MySQL writes and
// minutes of round time, and the readiness step used to run it before every
// round of a slot marked dirty_schema while the release loaded the base
// schema again for the next PR to replace. Now a round reloads only when its
// checkout's fingerprint differs from the recorded one (LazySchema), and the
// release keeps whatever the databases carry, checked against the
// development database's schema_migrations.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// SchemaCheck is CheckSchema's answer for a pool slot.
type SchemaCheck struct {
	// Need is true when the slot's databases must be reloaded (the pool's
	// reset_db) to carry the checkout's schema.
	Need bool
	// Why says why a reload is needed, or why not ("schema unchanged since
	// abc1234: no reset").
	Why string
	// FP, SHA and Version are the checkout's schema: the fingerprint of
	// the files schema_paths match at SHA (its HEAD) and the version its
	// db/schema.rb declares ("" = none). RecordSchema stores them once the
	// reload succeeded.
	FP, SHA, Version string
}

// LazySchema reports whether pool's slots reload their databases only when
// a round's checkout needs another schema (CheckSchema, before the
// reviewers) and keep them through the release: the pool names
// schema_paths and reset_db, and reset_db_on_schema_change is on. Otherwise
// the release loads the base schema again, as before.
func LazySchema(pool config.Pool) bool {
	return len(pool.SchemaPaths) > 0 && len(pool.ResetDB) > 0 && pool.ResetsDBOnSchemaChange()
}

// checkoutSchema is the schema of the slot's checkout: its HEAD, the
// fingerprint of the files pool.SchemaPaths match there (their modes, blob
// ids and paths) and the version db/schema.rb declares in the worktree.
func (m *Manager) checkoutSchema(ctx context.Context, sl store.Slot, pool config.Pool) (SchemaCheck, error) {
	head, err := m.git.RevParse(ctx, sl.Path, "HEAD")
	if err != nil {
		return SchemaCheck{}, err
	}
	files, err := m.git.TreeFiles(ctx, sl.Path, head, pool.SchemaPaths...)
	if err != nil {
		return SchemaCheck{}, err
	}
	h := sha256.New()
	for _, f := range files {
		io.WriteString(h, f+"\n")
	}
	version, _, err := schemaVersion(sl.Path)
	if err != nil {
		return SchemaCheck{}, err
	}
	return SchemaCheck{FP: hex.EncodeToString(h.Sum(nil)), SHA: head, Version: version}, nil
}

// loadedSchema is the schema of the slot's checkout right after reset_db
// (or the setup) loaded it into the databases, for the record: none
// (unknown) when the pool names no schema_paths or git cannot tell.
func (m *Manager) loadedSchema(ctx context.Context, sl store.Slot, pool config.Pool) SchemaCheck {
	if len(pool.SchemaPaths) == 0 {
		return SchemaCheck{}
	}
	c, err := m.checkoutSchema(ctx, sl, pool)
	if err != nil {
		m.logErr(ctx, err, "slots: schema of %s: %v", sl.Name, err)
		return SchemaCheck{}
	}
	return c
}

// CheckSchema compares the schema a pool slot's checkout needs with the one
// its databases carry: a reload is needed when the fingerprints differ or
// none was recorded. A match is checked once more against the development
// database, whose MAX(schema_migrations.version) must be the version the
// checkout's db/schema.rb declares (a migration someone ran by hand, or an
// agent, moves it; a failed read counts as a mismatch). Read-only.
func (m *Manager) CheckSchema(ctx context.Context, slot store.Slot, pool config.Pool) (SchemaCheck, error) {
	sl, err := m.reload(ctx, slot)
	if err != nil {
		return SchemaCheck{}, err
	}
	c, err := m.checkoutSchema(ctx, sl, pool)
	if err != nil {
		return SchemaCheck{}, fmt.Errorf("slots: schema of %s: %w", sl.Name, err)
	}
	since := textx.ShortSHA(store.Deref(sl.SchemaSHA))
	switch {
	case sl.SchemaFP == nil:
		c.Need, c.Why = true, "the schema its databases carry is unknown"
	case *sl.SchemaFP != c.FP:
		c.Need, c.Why = true, "its databases carry the schema of "+since
	default:
		if why := m.devSchemaDiffers(ctx, sl, pool, c.Version); why != "" {
			c.Need, c.Why = true, why
		} else {
			c.Why = "schema unchanged since " + since + ": no reset"
		}
	}
	return c, nil
}

// devSchemaDiffers is why the slot's development database is not at
// version ("" when it is, or when there is nothing to compare).
func (m *Manager) devSchemaDiffers(ctx context.Context, sl store.Slot, pool config.Pool, version string) string {
	db := devDatabase(pool, SlotDBSlug(sl))
	if m.d.MySQL == nil || version == "" || db == "" {
		return ""
	}
	have, err := m.d.MySQL.SchemaMigrationsMax(ctx, db)
	switch {
	case errors.Is(err, mysqlx.ErrNotFound):
		return db + " has no schema_migrations"
	case err != nil:
		return fmt.Sprintf("could not read the schema version of %s: %v", db, err)
	case have != version:
		return fmt.Sprintf("%s is at %q, %s says %q", db, have, SchemaFile, version)
	}
	return ""
}

// RecordSchema stores c, CheckSchema's answer, as the schema the slot's
// databases carry, once its reload succeeded.
func (m *Manager) RecordSchema(ctx context.Context, slot store.Slot, c SchemaCheck) error {
	if m.d.DryRun || c.FP == "" {
		return nil
	}
	if err := m.d.Store.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) { setSchema(u, c) }); err != nil {
		return fmt.Errorf("slots: record the schema of %s: %w", slot.Name, err)
	}
	return nil
}

// ForgetSchema marks the schema of the slot's databases unknown, before a
// reload that may stop half-way: the next round reloads them again unless
// RecordSchema follows.
func (m *Manager) ForgetSchema(ctx context.Context, slot store.Slot) error {
	if m.d.DryRun {
		return nil
	}
	if err := m.d.Store.UpdateSlotFields(ctx, slot.ID, func(u *store.SlotUpdate) { setSchema(u, SchemaCheck{}) }); err != nil {
		return fmt.Errorf("slots: forget the schema of %s: %w", slot.Name, err)
	}
	return nil
}

// setSchema sets the slot's recorded schema to c's (none when c has no
// fingerprint).
func setSchema(u *store.SlotUpdate, c SchemaCheck) {
	if c.FP == "" {
		u.Set("schema_fp", nil)
		u.Set("schema_sha", nil)
		u.Set("schema_version", nil)
		return
	}
	u.Set("schema_fp", c.FP)
	u.Set("schema_sha", c.SHA)
	var version any
	if c.Version != "" {
		version = c.Version
	}
	u.Set("schema_version", version)
}

// EnsureSchema gives a pool slot handed to a person (magnum open) the
// databases of its checkout, as a round's readiness step does: when
// CheckSchema finds they carry another schema, the pool's reset_db runs
// through mise exec (ResetDBTimeout, the slot's log) and the new schema is
// recorded. It returns what it did for the person. A failed reload is an
// error and leaves the schema unknown. A pool that is not LazySchema, or a
// per-PR worktree, runs nothing: its released slots carry the base schema.
func (m *Manager) EnsureSchema(ctx context.Context, slot store.Slot, pool config.Pool) (string, error) {
	if slot.Kind != store.SlotKindPool || !LazySchema(pool) {
		return "", nil
	}
	if m.d.DryRun {
		m.dryRun("load the schema of %s's checkout when its databases carry another", slot.Name)
		return "", nil
	}
	c, err := m.CheckSchema(ctx, slot, pool)
	if err != nil {
		return "", err
	}
	subject := "slot:" + slot.Name
	if !c.Need {
		m.event(ctx, subject, "info", "slot.schema_unchanged", c.Why)
		return c.Why, nil
	}
	if err := m.ForgetSchema(ctx, slot); err != nil {
		return "", err
	}
	m.event(ctx, subject, "info", "slot.schema_reset", "reloading schema: "+c.Why)
	if err := m.runScripts(ctx, slot, pool.SlotEnv(slot.Name), pool.ResetDB, "reset_db", "slot-"+slot.Name+".log", ResetDBTimeout); err != nil {
		return "", err
	}
	if err := m.RecordSchema(ctx, slot, c); err != nil {
		return "", err
	}
	return "loaded the schema of " + textx.ShortSHA(c.SHA) + " (" + c.Why + ")", nil
}

// keptSchemaDrifted is the release's check of a slot that keeps its
// databases (LazySchema): the development database's
// MAX(schema_migrations.version) must be the version the recorded schema
// declares; a migration someone ran by hand, or an agent, moves it. A slot
// without a recorded schema, or one whose db/schema.rb declared no version,
// is not checked: the next round reloads an unknown schema anyway.
func (m *Manager) keptSchemaDrifted(ctx context.Context, sl store.Slot, pool config.Pool) (string, error) {
	want := store.Deref(sl.SchemaVersion)
	db := devDatabase(pool, SlotDBSlug(sl))
	if m.d.MySQL == nil || sl.SchemaFP == nil || want == "" || db == "" {
		return "", nil
	}
	have, err := m.d.MySQL.SchemaMigrationsMax(ctx, db)
	switch {
	case errors.Is(err, mysqlx.ErrNotFound):
		return db + " has no schema_migrations", nil
	case err != nil:
		return "", fmt.Errorf("slots: schema version of %s: %w", db, err)
	case have != want:
		return fmt.Sprintf("%s is at %q, the schema loaded from %s says %q", db, have, textx.ShortSHA(store.Deref(sl.SchemaSHA)), want), nil
	}
	return "", nil
}

// keepSchema ends the release's schema step of a LazySchema slot whose
// databases stay as they are: the checkout is back at the base, so
// dirty_schema goes, and a slot.schema_kept event says what they carry.
func (m *Manager) keepSchema(ctx context.Context, sl store.Slot) error {
	if sl.DirtySchema {
		if err := m.d.Store.UpdateSlotFields(ctx, sl.ID, func(u *store.SlotUpdate) { u.Set("dirty_schema", false) }); err != nil {
			return err
		}
	}
	msg := "kept the databases (schema unknown): the next round reloads them"
	if sl.SchemaFP != nil {
		msg = "kept the schema of " + textx.ShortSHA(store.Deref(sl.SchemaSHA)) + ": no reset at release; the next round reloads only if its checkout needs another"
	}
	m.event(ctx, "slot:"+sl.Name+":release", "info", "slot.schema_kept", msg)
	return nil
}
