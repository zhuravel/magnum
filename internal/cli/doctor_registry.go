package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zhuravel/magnum/internal/inventory"
)

// doctorScanTimeout bounds the registry check's inventory scan (it lists the
// MySQL databases, runs git per clone and reads the herdr snapshot), so a
// stalled source cannot hold doctorRun.
var doctorScanTimeout = 30 * time.Second

func doctorRegistry(ctx context.Context, d doctorDeps) []doctorCheck {
	if d.Inventory == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, doctorScanTimeout)
	defer cancel()
	inv, err := d.Inventory.Scan(ctx, inventory.Options{})
	if errors.Is(err, context.DeadlineExceeded) {
		return []doctorCheck{doctorFailed("registry", fmt.Sprintf("inventory scan did not finish within %s: %s", doctorScanTimeout, err),
			"check that MySQL (DBngin), herdr and git answer, then run `magnum doctor` again")}
	}
	if err != nil {
		return []doctorCheck{doctorFailed("registry", "inventory scan failed: "+err.Error(), "check "+inspTilde(d.Layout.DB()))}
	}
	safe := 0
	for _, f := range inv.Drift {
		if f.Safe {
			safe++
		}
	}
	var out []doctorCheck
	if n := len(inv.Drift); n > 0 {
		out = append(out, doctorWarned("registry", fmt.Sprintf("%d drift findings between the registry and disk/MySQL/herdr (%d magnum can fix itself)", n, safe),
			"`magnum status` lists them; `magnum cleanup --dry-run` shows what cleanup would fix"))
	} else {
		out = append(out, doctorOK("registry", fmt.Sprintf("registry matches disk, MySQL and herdr (%d slots)", len(inv.Slots))))
	}
	for _, w := range inv.Warnings {
		out = append(out, doctorWarned("registry source", "inventory could not read a source: "+w, ""))
	}
	return out
}
