package engine

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// lockedBuffer is a log sink safe for the engine's goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Triage asks a model that reads the PR's diff, the PR's own text, which
// reviewers to drop, and it may drop all but the judge; the gate that
// requires every reviewer to be heard (MissingReports) lists only the roles
// the round ran, so a judge-only round of a small PR could earn the
// operator's automatic approval. Where magnum approves a repository's PRs as
// the operator, triage keeps every role, asks no model and logs why;
// elsewhere it is unchanged.
func TestTriageKeepsEveryRoleWhereMagnumApprovesAsTheOperator(t *testing.T) {
	for _, tc := range []struct {
		name        string
		autoApprove []string
		as          string
		asked       bool
	}{
		{"the repository auto-approves", []string{"talkable"}, "zhuravel", false},
		{"every repository of the watch auto-approves", []string{"*"}, "zhuravel", false},
		{"another repository auto-approves", []string{"widget"}, "zhuravel", true},
		{"auto_approve without auto_approve_as", []string{"talkable"}, "", true},
		{"no auto-approval", nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			h, model := triageHarness(t, modelAnswers(`{"run": [], "reason": "tiny"}`), func(h *harness) {
				h.cfg.Watches[0].AutoApprove = tc.autoApprove
				h.cfg.Watches[0].AutoApproveAs = tc.as
				h.d.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			})
			h.gh.files = map[string][]github.FileDelta{"master...t1": codePatch(2)}
			job := &roundJob{
				pr:   store.PR{ID: 7, Number: 2, BaseRef: new("master")},
				repo: triageRepo, watch: config.Watch{PollIdentity: "zhuravel"}, kind: pipeline.KindInitial,
			}
			roles := h.cfg.RolesFor(nil)
			rs := &roundSetup{target: "t1", roles: slices.Clone(roles), toRun: slices.Clone(roles)}
			h.e.triage(h.ctx, job, rs)
			if asked := len(model.Calls) > 0; asked != tc.asked {
				t.Fatalf("asked = %v, want %v", asked, tc.asked)
			}
			want := judgeOnly
			if !tc.asked {
				want = allRoles
			}
			if got := roleNames(rs.toRun); !slices.Equal(got, want) || !slices.Equal(roleNames(rs.roles), want) {
				t.Fatalf("toRun = %v, roles = %v, want %v", got, roleNames(rs.roles), want)
			}
			logged := strings.Contains(logs.String(), "approves this repository's PRs as the operator")
			if logged == tc.asked {
				t.Fatalf("the why logged = %v, want %v:\n%s", logged, !tc.asked, logs.String())
			}
		})
	}
}
