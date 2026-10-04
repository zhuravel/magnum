package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
)

func TestActiveRoundsReadsAnOddHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "my home #1 100%")
	c, _, _ := bareContext(t)
	c.Layout = paths.Layout{Home: home}
	if rounds, err := c.activeRounds(context.Background()); err != nil || len(rounds) != 0 {
		t.Fatalf("no registry yet: %v %v", rounds, err)
	}
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(c.Layout.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	inspSeedPR(t, st, "talkable/talkable", 7, store.PRReviewing, nil)
	inspSeedPR(t, st, "talkable/talkable", 8, store.PRReviewed, nil)
	rounds, err := c.activeRounds(context.Background())
	if err != nil || strings.Join(rounds, ",") != "talkable/talkable#7 (reviewing)" {
		t.Fatalf("rounds %v err %v", rounds, err)
	}
	if c.refuseWhileRoundsRun("daemon-stop", false) {
		t.Fatal("a round in flight must refuse")
	}
	if !c.refuseWhileRoundsRun("daemon-stop", true) {
		t.Fatal("--now must override")
	}
}

func TestRefuseWhileRoundsRunFailsClosed(t *testing.T) {
	c, _, errb := bareContext(t)
	if err := c.Layout.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// Not a database: the read fails, so the stop is refused.
	if err := os.WriteFile(c.Layout.DB(), []byte("not a sqlite file, not at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c.refuseWhileRoundsRun("daemon-restart", false) {
		t.Fatal("an unreadable registry must refuse")
	}
	actContains(t, errb.String(), "cannot tell whether review rounds are in flight", "--now")
	if !c.refuseWhileRoundsRun("daemon-restart", true) {
		t.Fatal("--now must override an unreadable registry")
	}
}
