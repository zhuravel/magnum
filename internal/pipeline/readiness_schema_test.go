package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/execx"
)

// The engine learns that a schema reset loaded the checkout's schema
// (ReadinessPlan.Loaded, which records it on the slot) only when every
// reset_db command passed; a failed one leaves the slot's schema unknown,
// so the next round reloads.
func TestReadinessReportsALoadedSchemaOnlyWhenEveryResetPassed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   int
		loaded bool
	}{
		{"every reset passed", 0, true},
		{"a reset failed", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			checkout := t.TempDir()
			e.exec.Rules = []execx.Rule{
				resetRule(checkout, nil, "bin/rails db:schema:load", execx.Result{}, nil),
				{Prefix: miseResetPrefix(checkout, nil, "bin/rails db:seed"), Fn: func(c execx.Cmd) (execx.Result, error) {
					if tc.code != 0 {
						return execx.Result{Code: tc.code}, &execx.ExitError{Cmd: c, Code: tc.code}
					}
					return execx.Result{}, nil
				}},
			}
			loaded := 0
			note := "reloading the schema: its databases carry the schema of abc1234"
			rd := readinessRound(t, e, KindInitial, checkout, ReadinessPlan{
				ResetDB: []string{"bin/rails db:schema:load", "bin/rails db:seed"}, SchemaNote: note,
				Loaded: func(context.Context) { loaded++ }})
			if err := rd.readiness(e.ctx); err != nil {
				t.Fatal(err)
			}
			if loaded > 1 || (loaded == 1) != tc.loaded {
				t.Fatalf("Loaded called %d times, want loaded = %v", loaded, tc.loaded)
			}
			if f := readReadinessFile(t, rd.dir); f.Schema != note {
				t.Errorf("readiness file schema = %q, want %q", f.Schema, note)
			}
		})
	}
}

// A round whose checkout needs no schema reload says so in the readiness
// event and file ("schema unchanged since <commit>: no reset").
func TestReadinessSaysWhyNoSchemaResetRan(t *testing.T) {
	e := newEnv(t)
	e.exec.Rules = []execx.Rule{zshRule("bin/rails db:test:prepare", execx.Result{}, nil)}
	note := "schema unchanged since abc1234: no reset"
	rd := readinessRound(t, e, KindInitial, t.TempDir(), ReadinessPlan{Prepare: []string{"bin/rails db:test:prepare"}, SchemaNote: note})
	if err := rd.readiness(e.ctx); err != nil {
		t.Fatal(err)
	}
	if f := readReadinessFile(t, rd.dir); f.Schema != note || len(f.Checks) != 1 {
		t.Errorf("readiness file = %+v", f)
	}
	var msg string
	for _, x := range e.events() {
		if x.Kind == "round.readiness" {
			msg = x.Message
		}
	}
	if !strings.HasPrefix(msg, "readiness: 1 of 1 checks ok") || !strings.HasSuffix(msg, "; "+note) {
		t.Errorf("round.readiness message = %q", msg)
	}
}
