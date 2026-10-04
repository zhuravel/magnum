package cli

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/zhuravel/magnum/internal/config"
)

// doctorMySQLTimeout bounds the whole MySQL check: ping and schema listing
// share one deadline, so a server that answers the ping and then stalls the
// query cannot hold doctorRun.
var doctorMySQLTimeout = 5 * time.Second

// doctorMySQLCheck checks MySQL when a [[pool]] declares databases; magnum
// touches MySQL for nothing else.
func doctorMySQLCheck(ctx context.Context, d doctorDeps) []doctorCheck {
	if !slices.ContainsFunc(d.Config.Pools, func(p config.Pool) bool { return len(p.Databases) > 0 }) {
		return []doctorCheck{doctorSkipped("mysql", "no [[pool]] declares databases: magnum does not use MySQL")}
	}
	fix := "start MySQL in DBngin (127.0.0.1:3306, root, no password) or set MAGNUM_MYSQL_DSN"
	if d.MySQL == nil {
		return []doctorCheck{doctorFailed("mysql", "MySQL DSN is invalid", "fix MAGNUM_MYSQL_DSN (unset it for the DBngin default)")}
	}
	ctx, cancel := context.WithTimeout(ctx, doctorMySQLTimeout)
	defer cancel()
	if err := d.MySQL.Ping(ctx); err != nil {
		return []doctorCheck{doctorFailed("mysql", "MySQL does not answer: "+err.Error(), fix)}
	}
	dbs, err := d.MySQL.ListSuffixed(ctx)
	if err != nil {
		return []doctorCheck{doctorFailed("mysql", "MySQL answers but listing schemas failed: "+err.Error(), fix)}
	}
	slugs := map[string]bool{}
	for _, db := range dbs {
		slugs[db.Slug] = true
	}
	return []doctorCheck{doctorOK("mysql", fmt.Sprintf("MySQL reachable: %d suffixed databases (talkable_%%__%%) in %d slugs", len(dbs), len(slugs)))}
}
