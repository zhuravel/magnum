package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

// poolNamed is the pool of repository repo ("" = daemon.default_repo, else
// the only pool).
func (e *Engine) poolNamed(repo string) (config.Pool, error) {
	if len(e.cfg.Pools) == 0 {
		return config.Pool{}, errors.New("config.toml has no [[pool]]")
	}
	if repo == "" {
		if p := e.cfg.PoolFor(e.cfg.Daemon.DefaultRepo); p != nil {
			return *p, nil
		}
		if len(e.cfg.Pools) == 1 {
			return e.cfg.Pools[0], nil
		}
		return config.Pool{}, errors.New("several pools are configured; name the pool's repository")
	}
	if p := e.cfg.PoolFor(repo); p != nil {
		return *p, nil
	}
	return config.Pool{}, fmt.Errorf("no [[pool]] for %s in config.toml", repo)
}

// provisionSlots provisions count slots of pool (count <= 0: up to
// pool.min), resuming interrupted provisioning first, never beyond pool.max
// (the `magnum slots provision` request).
func (e *Engine) provisionSlots(ctx context.Context, pool config.Pool, count int) (string, error) {
	if e.d.Slots == nil {
		return "", errors.New("slot management is not available")
	}
	sls, err := e.st.ListSlots(ctx, store.SlotFilter{RepoFullName: pool.Repo, Kind: store.SlotKindPool})
	if err != nil {
		return "", err
	}
	live := 0
	var resumable []int
	for _, sl := range sls {
		switch sl.State {
		case store.SlotRemoved:
		case store.SlotProvisioning:
			if n, ok := slotNumber(pool, sl.Name); ok {
				resumable = append(resumable, n)
			}
		default:
			live++
		}
	}
	if count <= 0 {
		count = max(pool.Min-live, len(resumable))
		if count == 0 {
			return fmt.Sprintf("%s already has %d slots (pool.min %d)", pool.Repo, live, pool.Min), nil
		}
	}
	if pool.Max > 0 && live+count > pool.Max {
		return "", fmt.Errorf("%s allows at most %d slots and has %d; lower the count or raise max in config.toml", pool.Repo, pool.Max, live)
	}
	var done []string
	for i := 0; i < count; i++ {
		var n int
		if len(resumable) > 0 {
			n, resumable = resumable[0], resumable[1:]
		} else if n, err = e.d.Slots.NextSlotNumber(ctx, pool); err != nil {
			return provisioned(done), err
		}
		name := pool.Slot(n)
		if e.d.DryRun {
			e.rec.Record(ctx, "pool:"+pool.Repo, "provision", name)
			done = append(done, name)
			continue
		}
		e.event(ctx, "info", "slot:"+name, "pool.provision", fmt.Sprintf("provisioning %s (request)", name), nil)
		if err := e.d.Slots.ProvisionPool(ctx, pool, n); err != nil {
			e.event(ctx, "warn", "slot:"+name, "pool.provision_failed", err.Error(), nil)
			return provisioned(done), fmt.Errorf("%s: %w (setup log: %s)", name, err,
				filepath.Join(e.d.Layout.Logs(), "provision-"+name+".log"))
		}
		e.event(ctx, "info", "slot:"+name, "pool.provisioned", name+" provisioned", nil)
		done = append(done, name)
	}
	return provisioned(done), nil
}

func provisioned(names []string) string {
	if len(names) == 0 {
		return "nothing provisioned"
	}
	return "provisioned " + strings.Join(names, ", ") + " (free)"
}

// repairSlot re-runs provisioning of a pool slot (the `magnum slots repair`
// request).
func (e *Engine) repairSlot(ctx context.Context, name string) (string, error) {
	if e.d.Slots == nil {
		return "", errors.New("slot management is not available")
	}
	sl, err := e.st.SlotByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("no slot named %q", name)
	}
	if err != nil {
		return "", err
	}
	pool := e.cfg.PoolFor(sl.RepoFullName)
	if sl.Kind != store.SlotKindPool || pool == nil {
		return "", fmt.Errorf("%s is not a pool slot; per-PR worktrees are recreated by their next round", name)
	}
	if e.d.DryRun {
		e.rec.Record(ctx, "slot:"+name, "repair", sl.State)
		return "dry run: would repair " + name, nil
	}
	if err := e.d.Slots.Repair(ctx, sl, *pool); err != nil {
		e.event(ctx, "warn", "slot:"+name, "slot.repair_failed", err.Error(), nil)
		return "", fmt.Errorf("%s: %w (setup log: %s)", name, err, filepath.Join(e.d.Layout.Logs(), "provision-"+name+".log"))
	}
	e.event(ctx, "info", "slot:"+name, "slot.repaired", name+" repaired (request)", nil)
	return name + " is repaired (free)", nil
}

// adoptSlot registers an existing checkout as a free pool slot (the `magnum
// slots adopt` request).
func (e *Engine) adoptSlot(ctx context.Context, p AdoptPayload) (string, error) {
	if e.d.Slots == nil {
		return "", errors.New("slot management is not available")
	}
	if p.Path == "" {
		return "", errors.New("adopt needs the checkout path")
	}
	path := filepath.Clean(paths.Expand(p.Path))
	var pool *config.Pool
	if p.Pool != "" {
		pool = e.cfg.PoolFor(p.Pool)
		if pool == nil {
			return "", fmt.Errorf("no [[pool]] for %s in config.toml", p.Pool)
		}
	} else {
		pool = poolOfPath(e.cfg, path)
		if pool == nil {
			return "", fmt.Errorf("%s is not a pool slot path (slot_path in config.toml)", path)
		}
	}
	if e.d.DryRun {
		e.rec.Record(ctx, "path:"+path, "adopt", pool.Repo)
		return "dry run: would adopt " + path, nil
	}
	sl, err := e.d.Slots.Adopt(ctx, *pool, path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("adopted %s as %s (%s)", path, sl.Name, sl.State), nil
}

// poolOfPath is the pool whose slot_path renders path for some n.
func poolOfPath(cfg *config.Config, path string) *config.Pool {
	for i := range cfg.Pools {
		for n := 1; n <= 999; n++ {
			if filepath.Clean(cfg.Pools[i].Path(n)) == path {
				return &cfg.Pools[i]
			}
		}
	}
	return nil
}
