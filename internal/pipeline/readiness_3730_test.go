package pipeline

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/slots"
)

// A schema reset MySQL refuses with 3730 (a table the schema does not know
// refers to one the load drops: review14 stayed broken 32 hours on it,
// 2026-11-04) recreates the slot's databases (ReadinessPlan.Recreate) and
// runs the reset once more, whose checks the judge reads. A second pass
// records the schema (Loaded); a second refusal, or a recreation that
// failed, counts the round for the slot (ResetFailed) with a cause that
// names 3730. Any other failure recreates nothing.
func TestAResetMySQLRefusesWith3730RecreatesTheDatabasesAndRunsOnceMore(t *testing.T) {
	const refusal = "ActiveRecord::StatementInvalid: Trilogy::ProtocolError: 3730: Cannot drop table 'accounts' referenced by " +
		"a foreign key constraint 'fk_rails_c239f4c879' on table 'ai_credentials'. (trilogy_query_recv)"
	const other = "ActiveRecord::NoDatabaseError: Unknown database 'talkable_development__review4'"
	rake := func(cause string) execx.Result {
		return execx.Result{Code: 1, Stderr: []byte("bin/rails aborted!\n" + cause + "\n\nTasks: TOP => db:schema:load\n")}
	}
	for _, tc := range []struct {
		name        string
		failures    []string // each run's refusal, in order; a run past them passes
		recreateErr error
		recreates   int
		runs        int
		loaded      bool
		cause       string // ResetFailed's; "" = not called
	}{
		{name: "the second run passes", failures: []string{refusal}, recreates: 1, runs: 2, loaded: true},
		{name: "the second run is refused too", failures: []string{refusal, refusal}, recreates: 1, runs: 2,
			cause: slots.Refused3730 + " again after magnum recreated the slot's databases: " + refusal},
		{name: "the recreation fails", failures: []string{refusal}, recreateErr: errors.New("mysqlx: drop: lock wait timeout"),
			recreates: 1, runs: 1,
			cause: slots.Refused3730 + ", and recreating the slot's databases failed (mysqlx: drop: lock wait timeout): " + refusal},
		{name: "another failure", failures: []string{other}, runs: 1, cause: other},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			var mu sync.Mutex
			runs := 0
			e.exec.Rules = []execx.Rule{{Prefix: []string{"mise"}, Fn: func(execx.Cmd) (execx.Result, error) {
				mu.Lock()
				defer mu.Unlock()
				runs++
				if runs <= len(tc.failures) {
					return rake(tc.failures[runs-1]), &execx.ExitError{Code: 1}
				}
				return execx.Result{}, nil
			}}}
			var causes []string
			loaded, recreates := 0, 0
			rd := readinessRound(t, e, KindInitial, t.TempDir(), ReadinessPlan{ResetDB: []string{"bin/rails db:schema:load"},
				SchemaNote:  "reloading the schema: its databases carry the schema of abc1234",
				Loaded:      func(context.Context) { loaded++ },
				ResetFailed: func(_ context.Context, c string) { causes = append(causes, c) },
				Recreate:    func(context.Context) error { recreates++; return tc.recreateErr }})
			if err := rd.readiness(e.ctx); err != nil {
				t.Fatal(err)
			}
			if recreates != tc.recreates || runs != tc.runs {
				t.Fatalf("recreated %d times, ran the reset %d times; want %d and %d", recreates, runs, tc.recreates, tc.runs)
			}
			if (loaded == 1) != tc.loaded || loaded > 1 {
				t.Fatalf("Loaded called %d times, want it called %v", loaded, tc.loaded)
			}
			if want := []string{tc.cause}; tc.cause == "" && len(causes) != 0 || tc.cause != "" && !slices.Equal(causes, want) {
				t.Fatalf("ResetFailed got %q, want %q", causes, tc.cause)
			}
			checks := rd.readinessData().Checks
			if len(checks) != 1 || checks[0].OK != tc.loaded {
				t.Fatalf("checks = %+v, want the last run's one check, ok %v", checks, tc.loaded)
			}
			if tc.recreates == 0 {
				return
			}
			var msg string
			for _, x := range e.events() {
				if x.Kind == "round.readiness" {
					msg = x.Message
				}
			}
			if f := readReadinessFile(t, rd.dir); !strings.Contains(f.Schema, "MySQL error 3730") || !strings.Contains(msg, "MySQL error 3730") {
				t.Fatalf("schema line %q, event %q; want both to name MySQL error 3730", f.Schema, msg)
			}
		})
	}
}
