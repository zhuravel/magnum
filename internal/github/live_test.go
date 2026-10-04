package github

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// TestLiveReadOnly exercises the real gh with read-only calls. It runs only
// with MAGNUM_LIVE_GITHUB=1 and skips when gh is missing or unauthenticated.
func TestLiveReadOnly(t *testing.T) {
	if os.Getenv("MAGNUM_LIVE_GITHUB") != "1" {
		t.Skip("set MAGNUM_LIVE_GITHUB=1 to run against api.github.com")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := &Client{Run: &execx.Real{}}
	if _, err := c.gh(ctx, "auth probe", []string{"api", "rate_limit", "--hostname", "github.com"}, nil, false); err != nil {
		t.Skipf("GitHub unreachable or gh unauthenticated: %v", err)
	}

	repos, rate, err := c.Radar(ctx, "talkable")
	if err != nil {
		t.Fatal(err)
	}
	var main RepoRadar
	for _, r := range repos {
		if r.NameWithOwner == "talkable/talkable" {
			main = r
		}
	}
	t.Logf("radar: %d repos, talkable/talkable %d open PRs, rate %+v", len(repos), len(main.PRs), rate)
	if main.NodeID == "" || len(main.PRs) == 0 || rate.Remaining == 0 {
		t.Fatalf("radar looks wrong: %+v", rate)
	}
	nums := []int{main.PRs[0].Number, 99999999}
	d, missing, err := c.Details(ctx, "talkable", "talkable", nums)
	if err != nil || len(d) != 1 || len(missing) != 1 {
		t.Fatalf("details: %d %v %v", len(d), missing, err)
	}
	s, notFound, err := c.ConfirmStates(ctx, "talkable", "talkable", nums)
	if err != nil || s[nums[0]].State != "OPEN" || len(notFound) != 1 {
		t.Fatalf("confirm: %+v %v %v", s, notFound, err)
	}
	rs, err := c.ReviewsWithMarker(ctx, "talkable", "talkable", 11973, "")
	if err != nil || len(rs) == 0 {
		t.Fatalf("reviews: %d %v", len(rs), err)
	}
	rr, err := c.ReviewREST(ctx, "talkable", "talkable", 11973, rs[0].DatabaseID)
	if err != nil || !SameLogin(rr.UserLogin, rs[0].AuthorLogin) {
		t.Fatalf("review REST: %+v %v", rr, err)
	}
	pr := d[nums[0]]
	cs, err := c.Compare(ctx, "talkable", "talkable", pr.BaseRefOid, pr.HeadRefOid)
	if err != nil || cs.Commits != pr.Commits || (cs.Files != -1 && cs.Files != pr.ChangedFiles) {
		t.Fatalf("compare %s...%s: %+v vs details %+v %v", pr.BaseRefOid, pr.HeadRefOid, cs, pr, err)
	}
}
