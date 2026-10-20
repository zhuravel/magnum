package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhuravel/magnum"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/store/storetest"
)

func TestAClearedListIsWrittenEmpty(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	for _, v := range []DraftValue{text(""), {List: []string{}}} {
		p, err := s.Plan(Draft{{Field: "watch.auto_approve"}: v}, false, testNow)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(p.New), "\nauto_approve = []\n") {
			t.Errorf("a cleared list (%+v):\n%s", v, p.New)
		}
	}
}

// A level's cells of one role share a line: setting one while removing the
// only other rewrites that line in place, keeping its comment.
func TestARolesCellsAreSetBeforeTheyAreRemovedSoTheLineKeepsItsComment(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	p, err := s.Plan(Draft{
		g("spend.high.claude-review.model"):  {Default: true},
		g("spend.high.claude-review.effort"): text("high"),
	}, true, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want := `claude-review = { effort = "high" }  # 2026-10-20: was model "opus", effort unset; 2026-10-20: fable -> opus` + "\n"
	if !strings.Contains(string(p.New), "[spend.high]\n"+want) {
		t.Errorf("new file:\n%s", p.New)
	}
	p, err = s.Plan(Draft{g("spend.burn.codex-judge.model"): {Default: true}}, true, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := `codex-judge = { effort = "xhigh" }  # 2026-10-20: codex-judge model back to the level's default, was "gpt-astra"`; !strings.Contains(string(p.New), want) {
		t.Errorf("new file:\n%s", p.New)
	}
}

func TestANewTablesHunkIsLabelledByItsHeader(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	p, err := s.Plan(Draft{g("spend.low.claude-review.model"): text("sonnet")}, false, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Hunks) != 1 || p.Hunks[0].Table != "[spend.low]" {
		t.Errorf("hunks = %+v", p.Hunks)
	}
}

// A [[repo]] block of the base config (--config) is not offered: a block of
// the user file would configure the repository twice.
func TestARepositoryBlockOfTheBaseConfigIsLeftToAHandEdit(t *testing.T) {
	storetest.Parallel(t)
	home := t.TempDir()
	base := filepath.Join(home, "base.toml")
	if err := os.WriteFile(base, append(append([]byte(nil), magnum.DefaultConfig...),
		"\n[[repo]]\nrepo = \"talkable/api\"\nstakes = \"base\"\n"...), 0o600); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(home, "user.toml")
	if err := os.WriteFile(user, []byte(fixture(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(paths.Layout{Home: home, UserConfig: user}, base)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Repos() {
		if r.Name == "talkable/api" {
			t.Errorf("the base config's repository is offered: %+v", r)
		}
	}
	_, err = s.Plan(Draft{{Field: "repo.stakes", Repo: "talkable/api"}: text("x")}, false, testNow)
	if err == nil || !strings.Contains(err.Error(), "base config") {
		t.Errorf("Plan = %v, want the base config named", err)
	}
}

func TestTheChecksAcceptWhatTheLoaderAccepts(t *testing.T) {
	s := newSnapshot(t, fixture(t))
	for id, v := range map[FieldID]string{
		"daemon.max_total_working_codex": "0", // no cap
		"daemon.max_concurrent_reviews":  "250",
		"daemon.park_idle_after":         "0",
		"pipeline.judge_fresh_after":     "1d",
		"usage.codex_soft":               "80.5",
		"repo.max_simplifications":       "0",
	} {
		if err := s.Check(Target{Field: id, Repo: "talkable/web"}, text(v)); err != nil {
			t.Errorf("%s = %s refused: %v", id, v, err)
		}
	}
}
