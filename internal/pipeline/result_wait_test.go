package pipeline

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// A result file that does not parse, or whose status is not a final one,
// does not end the judge's turn at once: the turn ends as before, when the
// run ends (two idle ticks) or, for a status, once the file has been there
// for ResultSettle.
func TestAResultFileWithoutAFinalStatusKeepsTheOldRule(t *testing.T) {
	const idleAfter = 5 * time.Minute // when Observe would end the run
	for _, tc := range []struct {
		name string
		file string // what the judge writes to its result file
		want time.Duration
	}{
		{"not JSON", `{"status": "posted", "run_id`, idleAfter},
		{"no status", `{"review_id": 512}`, idleAfter},
		{"a status that is not final", "", ResultSettle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			p := e.judgePosts(512, "COMMENTED", "COMMENT")
			p.keepWorking = true
			if tc.file != "" {
				p.status = ""
			} else {
				p.status = "writing"
			}
			var mu sync.Mutex
			var judgeRun string
			var submitted time.Time
			post := p.behavior(t)
			e.ag.behaviors[agents.RoleJudge] = []behavior{func(f *fakeAgents, run store.Run, text string) error {
				if err := post(f, run, text); err != nil {
					return err
				}
				if tc.file != "" {
					if err := os.WriteFile(store.Deref(run.ReportPath), []byte(tc.file), 0o600); err != nil {
						return err
					}
				}
				mu.Lock()
				judgeRun, submitted = run.ID, e.clock.Now()
				mu.Unlock()
				return nil
			}}
			sleep := e.r.Sleep
			e.r.Sleep = func(ctx context.Context, d time.Duration) error {
				if err := sleep(ctx, d); err != nil {
					return err
				}
				mu.Lock()
				id, at := judgeRun, submitted
				mu.Unlock()
				if id != "" && e.clock.Now().Sub(at) >= idleAfter {
					_ = e.ag.end(id) // the agent was seen idle on two ticks
				}
				return nil
			}

			res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
			if err != nil || res.Outcome != OutcomePosted || res.ReviewID != 512 {
				t.Fatalf("result = %+v, err = %v", res, err)
			}
			run := e.runOf(agents.RoleJudge, KindInitial)
			if run.VerifiedAt == nil {
				t.Fatalf("judge run = %+v, want verified", run)
			}
			waited := run.VerifiedAt.Sub(submitted)
			if waited < tc.want || waited >= tc.want+DefaultPollInterval {
				t.Fatalf("the judge's turn ended %s after its prompt, want %s", waited, tc.want)
			}
		})
	}
}
