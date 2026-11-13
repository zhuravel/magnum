package cli

import (
	"context"
	"database/sql"
	"os"

	"github.com/zhuravel/magnum/internal/slots"
)

// doctorSaves names the slots whose save before a discard keeps failing
// (slots.SaveFailures): a review slot's in the registry, a factory copy's
// or fix checkout's in the factory's agents store. Such a slot's step (a
// switch, a release's reset, a removal) discards nothing and retries at
// each attempt, so it never moves until its save works; the slots package
// raised it once, with an event and the review daemon's toast. A WARN per
// slot with its fix, a PASS with none.
func doctorSaves(ctx context.Context, d doctorDeps) []doctorCheck {
	var all []slots.SaveFailure
	if d.Store != nil {
		review, err := slots.SaveFailures(ctx, d.Store)
		if err != nil {
			return []doctorCheck{doctorWarned("saves", "the review slots' failing saves cannot be read: "+err.Error(), "")}
		}
		all = append(all, review...)
	}
	all = append(all, doctorFactorySaveFailures(ctx, d.Layout.FactoryAgentsDB())...)
	if len(all) == 0 {
		return []doctorCheck{doctorOK("saves", "no slot's save before a discard fails")}
	}
	out := make([]doctorCheck, 0, len(all))
	for _, f := range all {
		out = append(out, doctorWarned("save "+f.Slot, f.Sentence(),
			"fix what the error names (a full disk, a file or folder magnum cannot write in its data directory, a git "+
				"listing that fails in the checkout): the step's next attempt saves and goes on (`magnum logs "+f.Slot+"`)"))
	}
	return out
}

// doctorFactorySaveFailures reads the failing saves of the factory's
// checkouts from its agents store at path, read-only; none when the store
// is missing or cannot be read.
func doctorFactorySaveFailures(ctx context.Context, path string) []slots.SaveFailure {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	db, err := sql.Open("sqlite", registryReadOnlyDSN(path))
	if err != nil {
		return nil
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, `SELECT value FROM kv WHERE key GLOB ? ORDER BY key`, slots.SaveFailedGlob)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []slots.SaveFailure
	for rows.Next() {
		var v string
		if rows.Scan(&v) != nil {
			return out
		}
		if f, ok := slots.ParseSaveFailure(v); ok {
			out = append(out, f)
		}
	}
	return out
}
