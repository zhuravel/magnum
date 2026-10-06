package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// The poll stores each PR's changed paths with the Details it reads for a
// changed PR (pr_files), and a new head's Details replace them; a PR whose
// Details carry no list has none.
func TestThePollStoresEachPRsChangedPaths(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "base1"})
	h.startup()
	h.tick() // first sync
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b1", files: []string{"app/models/order.rb", "spec/models/order_spec.rb"}},
		prSpec{n: 3, head: "c1"})
	h.tick()
	files := func(n int) (store.PRFiles, bool) {
		t.Helper()
		f, ok, err := h.st.PRFilesOf(h.ctx, h.pr(n).ID)
		if err != nil {
			t.Fatal(err)
		}
		return f, ok
	}
	if f, ok := files(2); !ok || f.HeadSHA != "b1" || !slices.Equal(f.Paths, []string{"app/models/order.rb", "spec/models/order_spec.rb"}) || f.Truncated {
		t.Fatalf("#2 files = %+v, %v", f, ok)
	}
	if f, ok := files(3); ok {
		t.Fatalf("#3 without a list has %+v", f)
	}

	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "base1"}, prSpec{n: 2, head: "b2", files: []string{"app/models/order.rb"}, filesTruncated: true},
		prSpec{n: 3, head: "c1"})
	h.tick()
	if f, ok := files(2); !ok || f.HeadSHA != "b2" || !slices.Equal(f.Paths, []string{"app/models/order.rb"}) || !f.Truncated {
		t.Fatalf("#2 files after a push = %+v, %v", f, ok)
	}
}

// A round gets the related PRs' settings of its watch: [pipeline]
// related_lookback and related_ignore, which a [[watch]] overrides.
func TestRoundInputCarriesTheWatchsRelatedSettings(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(c *config.Config)
		want config.Related
	}{
		{"default", func(*config.Config) {}, config.Related{Lookback: 14 * 24 * time.Hour, Ignore: config.DefaultRelatedIgnore()}},
		{"watch", func(c *config.Config) {
			c.Watches[0].RelatedLookback = config.Duration{Duration: 30 * 24 * time.Hour}
			c.Watches[0].RelatedIgnore = []string{}
		}, config.Related{Lookback: 30 * 24 * time.Hour, Ignore: []string{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(h *harness) { tc.set(h.cfg) })
			h.reviewedPR(2, "b1")
			ins := h.rd.all()
			if len(ins) != 1 || ins[0].Related.Lookback != tc.want.Lookback || !slices.Equal(ins[0].Related.Ignore, tc.want.Ignore) {
				t.Fatalf("rounds = %d, related %+v; want %+v", len(ins), ins[0].Related, tc.want)
			}
		})
	}
}
