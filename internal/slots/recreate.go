package slots

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/steps"
	"github.com/zhuravel/magnum/internal/store"
)

// DBRecreator is the part of *mysqlx.Client that empties a database: it
// drops it and creates it again (Recreate). A Deps.MySQL without it
// recreates nothing.
type DBRecreator interface {
	Recreate(ctx context.Context, name string, g mysqlx.Guard) error
}

// fkDropRe is MySQL's refusal to drop a table another table's foreign key
// refers to (ER_FK_CANNOT_DROP_PARENT, 3730), as every client prints it:
// "3730: Cannot drop table 'accounts' referenced by a foreign key
// constraint ... on table 'ai_credentials'".
var fkDropRe = regexp.MustCompile(`(?i)cannot drop table\b.*\breferenced by a foreign key constraint`)

// Refused3730 names MySQL's refusal in the errors and events of a reset
// that hit it.
const Refused3730 = "MySQL error 3730 (a table the schema does not know refers to one the load drops)"

// ForeignKeyDropRefused reports whether text, a failed reset_db's output or
// error, is MySQL's 3730: a table the schema load does not know (a PR's
// schema added it) refers to one the load drops, so every load fails until
// the databases start empty (RecreateDatabases).
func ForeignKeyDropRefused(text string) bool {
	return fkDropRe.MatchString(text)
}

// RecreateDatabases empties the slot's databases for a reset_db that MySQL
// refused with 3730 (ForeignKeyDropRefused): each database of the slot's own
// list (the pool's databases for its slug, pool.DBNames), and none other,
// is dropped and created again through the pool's drop guard (DropGuard), as
// the step recreate_dbs of subject slot:<name>:recreate_dbs with its begin,
// ok or fail event; the caller then runs the reset once more. A dry run, a
// pool without databases or a guard, or a MySQL that cannot recreate
// (DBRecreator) drops nothing and says why.
func (m *Manager) RecreateDatabases(ctx context.Context, slot store.Slot, pool config.Pool) error {
	names := pool.DBNames(SlotDBSlug(slot))
	rc, ok := m.d.MySQL.(DBRecreator)
	guard := DropGuard(pool)
	switch {
	case m.d.DryRun:
		m.dryRun("recreate the databases of %s", slot.Name)
		return fmt.Errorf("slots: recreate the databases of %s: a dry run recreates nothing", slot.Name)
	case slot.Kind != store.SlotKindPool || len(names) == 0:
		return fmt.Errorf("slots: recreate the databases of %s: the slot has no list of databases", slot.Name)
	case guard.AllowRegexp == nil:
		return fmt.Errorf("slots: recreate the databases of %s: the pool's databases have no drop guard", slot.Name)
	case !ok:
		return fmt.Errorf("slots: recreate the databases of %s: no MySQL that can", slot.Name)
	}
	subject := "slot:" + slot.Name + ":recreate_dbs"
	if err := steps.ResetSubject(ctx, m.d.Store, subject); err != nil {
		return err
	}
	m.event(ctx, "slot:"+slot.Name, "info", "slot.databases_recreate",
		Refused3730+": recreating "+strings.Join(names, ", ")+", then the reset once more")
	return m.step(ctx, subject, "recreate_dbs", func(ctx context.Context) error {
		var errs []error
		for _, n := range names {
			if err := rc.Recreate(ctx, n, guard); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
}

// recreateAfter3730 is a reset's recovery from its failure (failed, the
// error's text): when MySQL refused it with 3730 the slot's databases are
// recreated (RecreateDatabases) and fk is true, the caller runs the reset
// once more; recreateErr is the recreation's failure, which the caller's
// error names.
func (m *Manager) recreateAfter3730(ctx context.Context, sl store.Slot, pool config.Pool, failed string) (fk bool, recreateErr error) {
	if ctx.Err() == nil && ForeignKeyDropRefused(failed) {
		return true, m.RecreateDatabases(ctx, sl, pool)
	}
	return false, nil // another failure, or the caller gave up: nothing to recover
}

// after3730 is the error of a reset that failed (err) after recreateAfter3730
// recreated its databases, or failed to (recreateErr): it names 3730.
func after3730(err, recreateErr error) error {
	if recreateErr != nil {
		return fmt.Errorf("%s, and recreating the slot's databases failed (%w): %w", Refused3730, recreateErr, err)
	}
	return fmt.Errorf("%s again after magnum recreated the slot's databases: %w", Refused3730, err)
}
