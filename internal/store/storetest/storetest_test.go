package storetest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/store"
)

func TestOpenGivesEachTestItsOwnCurrentRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := Open(t, filepath.Join(t.TempDir(), "state", "magnum.db"))
	b := Open(t, filepath.Join(t.TempDir(), "state", "magnum.db"))
	for _, st := range []*store.Store{a, b} {
		v, err := st.SchemaVersion(ctx)
		if err != nil || v != store.LatestSchemaVersion() {
			t.Fatalf("schema version = %d, %v, want %d", v, err, store.LatestSchemaVersion())
		}
	}
	if err := a.SetKV(ctx, "probe", "a"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := b.GetKV(ctx, "probe"); err != nil || ok {
		t.Fatalf("a write to one copy shows in another: ok=%v, %v", ok, err)
	}
}

func TestSeedLeavesAnExistingRegistryAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "magnum.db")
	st := Open(t, path)
	if err := st.SetKV(ctx, "probe", "kept"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	Seed(t, path)
	again := Open(t, path)
	if v, _, err := again.GetKV(ctx, "probe"); err != nil || v != "kept" {
		t.Fatalf("probe = %q, %v after a second Seed, want kept", v, err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v, want 0600", fi.Mode().Perm(), err)
	}
}

func TestClockMovesOnlyWhenTold(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	c := NewClock(t0)
	if !c.Now().Equal(t0) {
		t.Fatalf("Now = %v, want %v", c.Now(), t0)
	}
	c.Add(time.Minute)
	if want := t0.Add(time.Minute); !c.Now().Equal(want) {
		t.Fatalf("after Add: %v, want %v", c.Now(), want)
	}
	c.Set(t0)
	if !c.Now().Equal(t0) {
		t.Fatalf("after Set: %v, want %v", c.Now(), t0)
	}
}
