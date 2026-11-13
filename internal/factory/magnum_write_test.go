package factory

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

// writerPayload is a request payload of the test's own: the writer takes
// any payload as its JSON (the fix lane's are internal/fixwork's).
type writerPayload struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Head   string `json:"head"`
}

// The factory never migrates magnum's registry, here either: its writer of
// requests refuses one a migration behind this build (magnum's daemon
// still runs the build before) or one ahead of it (a newer magnum), and one
// that does not exist, writing nothing, and the file stays byte for byte
// as it was; on a registry at this build's schema it writes the request,
// pending, its payload as the JSON magnum's daemon decodes, and wakes the
// daemon.
func TestTheMagnumWriterRefusesToMigrate(t *testing.T) {
	p := writerPayload{Repo: "example/shop", Number: 7, Head: strings.Repeat("c", 40)}
	for name, delta := range map[string]int{"behind": -1, "ahead": 1} {
		t.Run(name, func(t *testing.T) {
			l, st := magnumLayout(t)
			v := store.LatestSchemaVersion() + delta
			if _, err := st.DB().Exec("PRAGMA user_version = " + strconv.Itoa(v)); err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				t.Fatal(err)
			}
			st.Close()
			before, err := os.ReadFile(l.DB())
			if err != nil {
				t.Fatal(err)
			}
			id, err := NewMagnumWriter(l).EnqueueRequest(t.Context(), "fix_test", p)
			if err == nil {
				t.Fatalf("EnqueueRequest on a registry at schema %d = %d, want an error", v, id)
			}
			if delta < 0 && !strings.Contains(err.Error(), "restart magnum's daemon first") {
				t.Errorf("the refusal %q does not say to restart magnum's daemon first", err)
			}
			after, err := os.ReadFile(l.DB())
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("the writer changed magnum's registry")
			}
			if got := userVersion(t, l.DB()); got != v {
				t.Fatalf("user_version %d after the write, want %d", got, v)
			}
		})
	}

	t.Run("none", func(t *testing.T) {
		storetest.Parallel(t)
		l := paths.Layout{Home: t.TempDir()}
		if _, err := NewMagnumWriter(l).EnqueueRequest(t.Context(), "fix_test", p); err == nil {
			t.Fatal("EnqueueRequest with no registry wrote one")
		}
		if _, err := os.Stat(l.DB()); !os.IsNotExist(err) {
			t.Fatalf("the writer created magnum's registry: %v", err)
		}
	})

	t.Run("this build's", func(t *testing.T) {
		l, st := magnumLayout(t)
		w := NewMagnumWriter(l)
		kicks := 0
		w.kick = func() (int, error) { kicks++; return 4242, nil }
		id, err := w.EnqueueRequest(t.Context(), "fix_test", p)
		if err != nil || id == 0 {
			t.Fatalf("EnqueueRequest = %d, %v", id, err)
		}
		if pid, err := w.KickDaemon(); err != nil || pid != 4242 || kicks != 1 {
			t.Fatalf("KickDaemon = %d, %v (%d kicks); want magnum's daemon woken once", pid, err, kicks)
		}
		reqs, err := st.PendingRequests(t.Context(), 0)
		if err != nil || len(reqs) != 1 || reqs[0].ID != id || reqs[0].Kind != "fix_test" {
			t.Fatalf("pending requests = %+v, %v; want the factory's one", reqs, err)
		}
		var got writerPayload
		if err := json.Unmarshal(reqs[0].Payload, &got); err != nil || got != p {
			t.Fatalf("the payload reads %s (%v), want %+v", reqs[0].Payload, err, p)
		}
		if got := userVersion(t, l.DB()); got != store.LatestSchemaVersion() {
			t.Fatalf("user_version %d, want this build's %d", got, store.LatestSchemaVersion())
		}
		if _, err := w.EnqueueRequest(t.Context(), "", p); err == nil {
			t.Error("a request without a kind was written")
		}
	})
}
