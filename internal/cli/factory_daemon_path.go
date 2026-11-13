package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"

	"github.com/zhuravel/magnum/internal/store"
)

// kvFactoryDaemonPATH is the key, in the factory's agents store, of the PATH
// the running factory daemon runs with (factoryDaemonPATH's JSON). The
// review daemon records what its login shell added as a count, for
// `magnum status` (engine.KVDaemonShellPATH); this row holds the whole PATH
// and is read by `magnum doctor` alone, which names only where one tool is
// found, never the directories.
const kvFactoryDaemonPATH = "factory.daemon_path"

// factoryDaemonPATH is the PATH a factory daemon runs with, as it recorded it
// when it started: after the locale and the login shell's directories
// ([daemon] shell_path) were applied to its own environment
// (startFactoryDaemon), so it is what the copies' setup, reset, flush and
// precheck commands resolve their tools on.
type factoryDaemonPATH struct {
	// PID is the daemon's process; a row of another pid belongs to an earlier
	// daemon, which the running one has not replaced yet or never will.
	PID  int    `json:"pid"`
	Path string `json:"path"`
}

// recordFactoryDaemonPATH writes the PATH of the daemon process pid.
func recordFactoryDaemonPATH(ctx context.Context, st *store.Store, pid int, path string) error {
	raw, err := json.Marshal(factoryDaemonPATH{PID: pid, Path: path})
	if err != nil {
		return err
	}
	return st.SetKV(ctx, kvFactoryDaemonPATH, string(raw))
}

// readFactoryDaemonPATH reads the PATH the last factory daemon recorded in
// the agents store at path, read-only: false when the store, the row or its
// PATH is missing (an older daemon never wrote one) or unreadable.
func readFactoryDaemonPATH(ctx context.Context, path string) (factoryDaemonPATH, bool) {
	if _, err := os.Stat(path); err != nil {
		return factoryDaemonPATH{}, false
	}
	db, err := sql.Open("sqlite", registryReadOnlyDSN(path))
	if err != nil {
		return factoryDaemonPATH{}, false
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, completeTimeout)
	defer cancel()
	var raw string
	if db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, kvFactoryDaemonPATH).Scan(&raw) != nil {
		return factoryDaemonPATH{}, false
	}
	var rec factoryDaemonPATH
	if json.Unmarshal([]byte(raw), &rec) != nil || rec.PID <= 0 || rec.Path == "" {
		return factoryDaemonPATH{}, false
	}
	return rec, true
}
