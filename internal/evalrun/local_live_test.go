package evalrun

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/app"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/paths"
	"github.com/zhuravel/magnum/internal/pipeline"
)

// TestLiveLocalRoundWithTheUnmodifiedJudge is the spike of the factory's
// design (docs/superpowers/specs/2026-11-03-factory-design.md, section
// 6.5; docs/spikes.md): one hand-seeded local round of a change in a
// checkout, with the unmodified judge, to see whether it survives the
// missing PR. The round is a blind dry run of the watch's roles under the
// live config (no review candidate) as a replay runs one (engine.RunEval:
// no local block, the base from the checkout's base branch, post-review's
// --local-base), on a synthetic PR number GitHub does not have. With
// MAGNUM_LIVE_LOCAL_BLOCK=1 a second round follows as the factory runs it
// (RunLocal: the local block, the description and ticket files). It starts
// real agents in magnum's herdr session and takes up to two hours a round,
// so it runs only with MAGNUM_LIVE_ENGINE=1 and MAGNUM_LIVE_LOCAL set to
// "<checkout>,<owner/name>,<base sha>,<head sha>" (the checkout on the
// change's branch at its head), never in the gate. It logs what each round
// ended with: the outcome, the verdict, the counts, the reports the judge
// went without and where the reports are.
func TestLiveLocalRoundWithTheUnmodifiedJudge(t *testing.T) {
	if os.Getenv("MAGNUM_LIVE_ENGINE") != "1" {
		t.Skip("set MAGNUM_LIVE_ENGINE=1 to run a local round with real agents")
	}
	parts := strings.Split(os.Getenv("MAGNUM_LIVE_LOCAL"), ",")
	if len(parts) != 4 {
		t.Skip("set MAGNUM_LIVE_LOCAL=<checkout>,<owner/name>,<base sha>,<head sha>")
	}
	l, err := paths.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(l, "")
	if err != nil {
		t.Fatal(err)
	}
	w := cfg.WatchFor(parts[1])
	if w == nil {
		t.Fatalf("no [[watch]] covers %s", parts[1])
	}
	logger := app.NewLogger(nil, os.Stderr, slog.LevelInfo)
	d := Deps{Layout: l, Config: cfg, Logger: logger, Stderr: os.Stderr, Progress: t.Logf, Warn: t.Logf}
	c := LocalCase{Checkout: parts[0], Repo: parts[1], BaseSHA: parts[2], HeadSHA: parts[3], Branch: "factory-spike",
		Title: "Factory spike: a local round", Body: "A hand-seeded local round of the factory's spike (section 6.5).",
		Watch: *w, Round: 1, TaskID: 99999, Base: config.DefaultFactoryBase}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	unmodified := d
	unmodified.localRound = func(ctx context.Context, cfg *config.Config, layout paths.Layout, tag string, lr engine.LocalRound) (pipeline.RoundResult, error) {
		a, err := app.New(cfg, layout, app.Options{AgentTag: tag, Stderr: os.Stderr, Logger: logger})
		if err != nil {
			return pipeline.RoundResult{}, err
		}
		defer a.Close()
		e := engine.FromApp(a)
		res, pr, err := e.RunEval(ctx, lr.EvalCase)
		if pr.ID != 0 {
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), ReleaseTimeout)
			if rerr := e.ReleaseEval(rctx, pr); rerr != nil {
				t.Logf("closing the round's agents: %v", rerr)
			}
			rcancel()
		}
		return res, err
	}
	report := func(what string, res LocalResult, err error) {
		t.Logf("%s: outcome %s, verdict %q, counts %v, missing %v, refused %q, error %q, reports in %s (scratch %s, PR #%d, tag %s): %v",
			what, res.Outcome, res.Verdict, res.Counts, res.Missing, res.Refused, res.Error, res.ReportDir, res.Scratch, res.Number, res.Tag, err)
	}
	res, err := RunLocal(ctx, unmodified, c)
	report("the unmodified judge", res, err)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("MAGNUM_LIVE_LOCAL_BLOCK") == "1" {
		c.Round = 2
		res, err := RunLocal(ctx, d, c)
		report("the local block", res, err)
		if err != nil {
			t.Fatal(err)
		}
	}
}
