package staging

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A mirror_dir under /Volumes on a volume that is not mounted: Mirror says
// so, every status is unknown with the volume's reason, and nothing is
// created, the volume's root least of all.
func TestAnAbsentVolumeGivesUnknownAndCreatesNothing(t *testing.T) {
	t.Parallel()
	volumes := filepath.Join(t.TempDir(), "Volumes")
	if err := os.Mkdir(volumes, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		vol   string
		plain bool // the volume's root is a plain directory, not a mount
	}{
		{"absent", "External", false},
		{"a plain directory where the volume mounts", "Plain", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.plain {
				if err := os.Mkdir(filepath.Join(volumes, tc.vol), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			vol := tc.vol
			root := filepath.Join(volumes, vol, "magnum-staging")
			mirror := mirrorDir(volumes, root)
			dir, ok := mirror("acme/App")
			if ok {
				t.Fatalf("Mirror(acme/App) = %q, true; want false while the volume is not mounted", dir)
			}
			if want := filepath.Join(root, "acme", "app.git"); dir != want {
				t.Errorf("Mirror's dir = %q, want %q", dir, want)
			}
			s := newScene(t)
			c := New(Deps{Git: s.git, GitHub: s.gh, Mirror: mirror,
				GitEnv: func(string) map[string]string { return map[string]string{} }, Now: func() time.Time { return s.now }})
			if err := c.Refresh(context.Background(), "acme/app", []OpenPR{{Number: 7, Base: "master"}}); err == nil {
				t.Error("Refresh on an absent volume: no error")
			}
			got := one(c, s.pr())
			if got.State != StateUnknown || got.Why != WhyVolumeNotMounted {
				t.Errorf("%s %q, want unknown, %q", got.State, got.Why, WhyVolumeNotMounted)
			}
			if s.git.total() != 0 || s.gh.total() != 0 {
				t.Errorf("%d git and %d GitHub calls, want none", s.git.total(), s.gh.total())
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Errorf("%s exists after the Refresh (%v): nothing may be created", root, err)
			}
			if !tc.plain {
				if _, err := os.Stat(filepath.Join(volumes, vol)); !os.IsNotExist(err) {
					t.Errorf("the volume's root was created (%v)", err)
				}
			}
		})
	}
}

// Off /Volumes, or on a mounted volume, the mirror is <root>/<owner>/<repo>.git
// in lower case; a name that is not owner/name is no mirror.
func TestMirrorDirIsOwnerRepoUnderTheRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "staging")
	mirror := mirrorDir(filepath.Join(t.TempDir(), "Volumes"), root)
	if dir, ok := mirror("Acme/App"); !ok || dir != filepath.Join(root, "acme", "app.git") {
		t.Errorf("Mirror(Acme/App) = %q, %v", dir, ok)
	}
	for _, bad := range []string{"acme", "acme/..", "../app", "acme/app/x", "-x/app", ""} {
		if dir, ok := mirror(bad); ok {
			t.Errorf("Mirror(%q) = %q, true; want no mirror", bad, dir)
		}
	}
	if dir, ok := mirrorDir("/Volumes", "")("acme/app"); ok || dir != "" {
		t.Errorf("no mirror_dir: %q, %v; want none", dir, ok)
	}
}
