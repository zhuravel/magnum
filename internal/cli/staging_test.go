package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// stagingRun runs `magnum staging args...`, wanting exit code want, and
// returns its stdout and stderr.
func (h *actHarness) stagingRun(want int, args ...string) (string, string) {
	h.t.Helper()
	h.out.Reset()
	h.errb.Reset()
	if code := h.cmd("staging", args...); code != want {
		h.t.Fatalf("staging %v: exit %d, want %d: %s%s", args, code, want, h.out.String(), h.errb.String())
	}
	return h.out.String(), h.errb.String()
}

// stagingHarness is an act harness whose talkable/talkable has the envs
// void and bart, a daemon that ticked just now, and three open PRs: #5 live
// on void, #6 on no staging, #7 an older head on bart.
func stagingHarness(t *testing.T) (*actHarness, map[int]store.PR) {
	t.Helper()
	h := newActHarness(t)
	stagingConfig(h.d.Cfg)
	if err := h.st.SetKV(h.ctx, store.KVDaemonLastTick, store.FormatTime(h.now)); err != nil {
		t.Fatal(err)
	}
	prs := map[int]store.PR{}
	for _, n := range []int{5, 6, 7} {
		prs[n] = h.seedPR("talkable/talkable", n, store.PRReviewed)
	}
	none := []engine.StagingEnv{{Env: "void", State: "none"}, {Env: "bart", State: "none"}}
	h.setStagingRecord(prs[5].ID, engine.StagingRecord{Head: prs[5].HeadSHA, Envs: []engine.StagingEnv{
		{Env: "void", State: "latest", InTip: prs[5].HeadSHA, PushedAt: h.now.Add(-time.Hour), DeployedAt: h.now.Add(-40 * time.Minute),
			RunURL: "https://github.com/talkable/talkable/actions/runs/7"},
		{Env: "bart", State: "none"},
	}})
	h.setStagingRecord(prs[6].ID, engine.StagingRecord{Head: prs[6].HeadSHA, Envs: none})
	h.setStagingRecord(prs[7].ID, engine.StagingRecord{Head: prs[7].HeadSHA, Envs: []engine.StagingEnv{
		{Env: "void", State: "none"},
		{Env: "bart", State: "outdated", InTip: "0ld0000aaaa1111", Behind: 2, PushedAt: h.now.Add(-48 * time.Hour)},
	}})
	return h, prs
}

// `magnum staging <pr>` prints where the PR stands on each of its
// repository's envs, in the config's order, as the board's card says it,
// with the CI run's page; --json prints it as an object.
func TestStagingCommandPrintsAPRsStagingsPerEnv(t *testing.T) {
	h, prs := stagingHarness(t)
	h.d.Now = func() time.Time { return h.now }
	out, _ := h.stagingRun(0, "talkable#5")
	actContains(t, out, "talkable#5 at abc1234",
		"void  live · head abc1234 · deployed 40m ago",
		"run https://github.com/talkable/talkable/actions/runs/7",
		"bart  not there")
	if strings.Index(out, "void") > strings.Index(out, "bart") {
		t.Errorf("the envs are not in the config's order:\n%s", out)
	}

	raw, _ := h.stagingRun(0, "talkable#5", "--json")
	var got stagingJSON
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if got.Ref != "talkable/talkable#5" || got.Head != prs[5].HeadSHA || len(got.Stagings) != 2 ||
		got.Stagings[0].Env != "void" || got.Stagings[0].State != "latest" || got.Stagings[0].RunURL == "" ||
		got.Stagings[1].Env != "bart" || got.Stagings[1].State != "none" {
		t.Fatalf("--json %+v", got)
	}
	for _, key := range []string{`"ref"`, `"head"`, `"stagings"`, `"in_tip"`, `"deployed_at"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("--json lacks %s:\n%s", key, raw)
		}
	}
}

// Without a PR, `magnum staging` lists every open PR of the repositories
// with stagings that is on any of them (live, pushed or an older head),
// never one on none; --json prints them as a list.
func TestStagingCommandWithoutAPRListsTheOpenPRsOnAnyStaging(t *testing.T) {
	h, _ := stagingHarness(t)
	h.d.Now = func() time.Time { return h.now }
	out, _ := h.stagingRun(0)
	actContains(t, out, "talkable#5 at abc1234", "void  live", "talkable#7 at abc1234",
		"bart  an older head · 0ld0000 · 2 commits behind · pushed 2d ago")
	if strings.Contains(out, "talkable#6") {
		t.Errorf("a PR on no staging is listed:\n%s", out)
	}
	raw, _ := h.stagingRun(0, "--json")
	var got []stagingJSON
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	if len(got) != 2 || got[0].Ref != "talkable/talkable#5" || got[1].Ref != "talkable/talkable#7" {
		t.Fatalf("--json %+v", got)
	}

	// A stopped daemon's records no longer tell anything: none is listed,
	// and the command says why.
	h.now = h.now.Add(time.Hour)
	out, _ = h.stagingRun(0)
	actContains(t, out, "no open PR is on a staging", "the daemon's last tick is 1h old")
}

// With no staging envs configured the feature is off: the list says so,
// --json prints [], and a PR of a repository without envs is refused.
func TestStagingCommandWithoutStagingsSaysTheFeatureIsOff(t *testing.T) {
	h := newActHarness(t)
	h.seedPR("talkable/talkable", 5, store.PRReviewed)
	out, _ := h.stagingRun(0)
	actContains(t, out, "no watched repository has staging envs")
	if raw, _ := h.stagingRun(0, "--json"); strings.TrimSpace(raw) != "[]" {
		t.Errorf("--json without stagings: %q, want []", raw)
	}
	_, errOut := h.stagingRun(1, "talkable#5")
	actContains(t, errOut, "talkable/talkable has no staging envs")
	_, errOut = h.stagingRun(2, "talkable#5", "talkable#6")
	actContains(t, errOut, "one PR at a time", "usage: magnum staging")
}
