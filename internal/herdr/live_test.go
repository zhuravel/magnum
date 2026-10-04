package herdr

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// TestLiveSnapshot calls session.snapshot (read-only) on the real herdr
// socket. It runs only with MAGNUM_LIVE_HERDR=1 (so a plain `go test ./...`
// never touches the developer's herdr) and skips when no herdr server is
// reachable.
func TestLiveSnapshot(t *testing.T) {
	if os.Getenv("MAGNUM_LIVE_HERDR") != "1" {
		t.Skip("set MAGNUM_LIVE_HERDR=1 to run against the real herdr socket")
	}
	sock := DefaultSocket()
	if _, err := os.Stat(sock); err != nil {
		t.Skipf("herdr socket %s absent", sock)
	}
	c := &Client{Socket: sock, Timeout: 5 * time.Second}
	snap, err := c.Snapshot(context.Background())
	if errors.Is(err, ErrUnavailable) {
		t.Skipf("herdr not reachable: %v", err)
	}
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Version == "" || snap.Protocol == 0 {
		t.Fatalf("snapshot header empty: version=%q protocol=%d", snap.Version, snap.Protocol)
	}
	for _, p := range snap.Panes {
		if p.ID == "" || p.WorkspaceID == "" || p.AgentStatus == "" {
			t.Fatalf("pane missing ids/status: %+v", p)
		}
	}
	t.Logf("herdr %s protocol %d: %d workspaces, %d panes, %d agents",
		snap.Version, snap.Protocol, len(snap.Workspaces), len(snap.Panes), len(snap.Agents))
}
