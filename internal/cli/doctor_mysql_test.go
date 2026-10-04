package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/inventory"
	"github.com/zhuravel/magnum/internal/mysqlx"
)

// stallingMySQL answers the ping and then blocks the schema query until its
// context ends.
type stallingMySQL struct{}

func (stallingMySQL) Ping(context.Context) error { return nil }
func (stallingMySQL) ListSuffixed(ctx context.Context) ([]mysqlx.Database, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// stallingScanner blocks the inventory scan until its context ends.
type stallingScanner struct{}

func (stallingScanner) Scan(ctx context.Context, _ inventory.Options) (inventory.Inventory, error) {
	<-ctx.Done()
	return inventory.Inventory{}, fmt.Errorf("inventory: %w", ctx.Err())
}

// shortDoctorTimeouts shrinks the MySQL and scan deadlines for one test.
func shortDoctorTimeouts(t *testing.T, mysql, scan time.Duration) {
	t.Helper()
	oldMySQL, oldScan := doctorMySQLTimeout, doctorScanTimeout
	doctorMySQLTimeout, doctorScanTimeout = mysql, scan
	t.Cleanup(func() { doctorMySQLTimeout, doctorScanTimeout = oldMySQL, oldScan })
}

// doctorRunWithin runs doctorRun and fails the test when it does not return
// within limit (a stalled probe holds the wait group forever).
func doctorRunWithin(t *testing.T, limit time.Duration, d doctorDeps) []doctorCheck {
	t.Helper()
	done := make(chan []doctorCheck, 1)
	go func() { done <- doctorRun(context.Background(), d) }()
	select {
	case cs := <-done:
		return cs
	case <-time.After(limit):
		t.Fatalf("doctorRun still blocked after %v: a probe has no deadline", limit)
		return nil
	}
}

func TestDoctorMySQLStalledQueryIsBounded(t *testing.T) {
	shortDoctorTimeouts(t, 50*time.Millisecond, time.Hour)
	_, d, _, _ := doctorFixture(t)
	d.MySQL = stallingMySQL{}
	m := doctorByName(doctorRunWithin(t, 10*time.Second, d))
	c := m["mysql"]
	if c.Status != doctorFail || !strings.Contains(c.Detail, "listing schemas failed") || !strings.Contains(c.Detail, "deadline") {
		t.Fatalf("mysql = %+v", c)
	}
}

func TestDoctorRegistryStalledScanIsBounded(t *testing.T) {
	shortDoctorTimeouts(t, time.Hour, 50*time.Millisecond)
	_, d, _, _ := doctorFixture(t)
	d.Inventory = stallingScanner{}
	m := doctorByName(doctorRunWithin(t, 10*time.Second, d))
	c := m["registry"]
	if c.Status != doctorFail || !strings.Contains(c.Detail, "did not finish") || !strings.Contains(c.Fix, "MySQL") {
		t.Fatalf("registry = %+v", c)
	}
}
