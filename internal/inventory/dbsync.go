package inventory

import (
	"context"
	"fmt"

	"github.com/zhuravel/magnum/internal/mysqlx"
	"github.com/zhuravel/magnum/internal/store"
)

// SyncResult reports what UpsertSlotDatabases recorded.
type SyncResult struct {
	Seen    int      `json:"seen"`    // databases upserted (last_seen_at = now)
	Dropped []string `json:"dropped"` // rows newly marked dropped (dropped_by = "reconcile")
}

// UpsertSlotDatabases records inv's MySQL listing in slot_databases: every
// listed database is upserted with its size and, when it belongs to a managed
// slot, that slot id (other rows keep their stored slot_id); rows that are no
// longer listed are marked dropped. It is the inventory's only write and
// refuses (ErrNotListed) when the scan could not list MySQL.
func (s *Scanner) UpsertSlotDatabases(ctx context.Context, inv Inventory) (SyncResult, error) {
	var res SyncResult
	if !inv.DatabasesListed {
		return res, ErrNotListed
	}
	if s.Store == nil {
		return res, fmt.Errorf("inventory: Scanner needs Store")
	}
	owner := map[string]int64{}
	for _, v := range inv.Slots {
		for _, d := range v.Databases {
			if d.Present {
				owner[d.Name] = v.Slot.ID
			}
		}
	}
	present := make(map[string]bool, len(inv.Databases))
	for _, d := range inv.Databases {
		slug := d.Slug
		if slug == "" {
			var ok bool
			if slug, ok = mysqlx.Slug(d.Name); !ok {
				continue
			}
		}
		present[d.Name] = true
		row := store.SlotDatabase{DBName: d.Name, Slug: slug, SizeMB: new(d.SizeMB)}
		if id, ok := owner[d.Name]; ok {
			row.SlotID = new(id)
		}
		if _, err := s.Store.UpsertSlotDatabase(ctx, row); err != nil {
			return res, fmt.Errorf("inventory: %w", err)
		}
		res.Seen++
	}
	rows, err := s.Store.ListSlotDatabases(ctx, false)
	if err != nil {
		return res, fmt.Errorf("inventory: %w", err)
	}
	for _, r := range rows {
		if present[r.DBName] {
			continue
		}
		if err := s.Store.MarkSlotDatabaseDropped(ctx, r.DBName, droppedBy); err != nil {
			return res, fmt.Errorf("inventory: %w", err)
		}
		res.Dropped = append(res.Dropped, r.DBName)
	}
	return res, nil
}
