package engine

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/factoryread"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/prstatus"
	"github.com/zhuravel/magnum/internal/store"
)

// seedCapFixed writes what the factory leaves of a task that published PR n
// of talkable/talkable on head from a local review that passed at its cap
// after the writer's fix: its registry (the task, its link, three cycles,
// the last ended at the cap and its fix reviewed by no round), its
// review-summary.json (exit cap_fixed, the findings fixed after the last
// round) and the PR's status file (the local review's verdict: findings).
func seedCapFixed(h *harness, n int, head string) {
	h.t.Helper()
	path := h.layout.FactoryDB()
	registrytest.Seed(h.t, path)
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	ts := "2026-10-05T08:00:00Z"
	for _, q := range []string{
		`INSERT INTO tasks (id, repo, state, created_at, updated_at) VALUES (9, 'talkable/talkable', 'published', '` + ts + `', '` + ts + `')`,
		`INSERT INTO pr_links (task_id, repo, number, opened_head, approved_head, opened_at) VALUES (9, 'talkable/talkable', ` +
			itoa(int64(n)) + `, '` + head + `', '` + head + `', '` + ts + `')`,
		`INSERT INTO review_cycles (task_id, n, head_before, head_after, started_at, ended_at, exit) VALUES
		 (9, 1, 'c1', 'c2', '` + ts + `', '` + ts + `', ''), (9, 2, 'c2', 'c3', '` + ts + `', '` + ts + `', ''),
		 (9, 3, 'c3', 'c3', '` + ts + `', '` + ts + `', 'cap')`,
	} {
		if _, err := db.Exec(q); err != nil {
			h.t.Fatalf("factory fixture: %v\n%s", err, q)
		}
	}
	dir := h.layout.FactoryTaskDir(9)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	summary := `{"exit": "cap_fixed", "rounds": 3, "head": "` + head + `", "leftovers": [], "declined": [], "doubts": [],
		"fixed_after": [{"id": "F1", "fingerprint": "fp1", "severity": "P2", "title": "The merger maps winner 6 to the wrong arm",
		"path": "app/models/merger.rb", "line": 120}]}`
	if err := os.WriteFile(filepath.Join(dir, "review-summary.json"), []byte(summary), 0o600); err != nil {
		h.t.Fatal(err)
	}
	s := factoryStatus()
	s.TaskID, s.LocalReview = 9, prstatus.LocalReview{Rounds: 3, Verdict: prstatus.VerdictFindings}
	p, err := prstatus.Path(h.layout, "talkable", "talkable", n)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := prstatus.Write(p, s); err != nil {
		h.t.Fatal(err)
	}
}

// A PR the factory published after its local review passed at the cap,
// the writer's last fix reviewed by no local round (review-summary.json's
// exit cap_fixed), gets magnum's first public round in full, as any new PR
// does: the initial kind, every review role, nothing earlier to build on.
// Nothing reads the factory's local review as a review of the PR, so it is
// never skipped or shortened as "reviewed locally"; the factory's registry
// holds no exit that reads clean for it either.
func TestAPRPublishedAfterTheLocalReviewsCapGetsAFullFirstRound(t *testing.T) {
	h := newHarness(t)
	h.open(prSpec{n: 1, head: "a1"})
	h.startup()
	h.tick()

	seedCapFixed(h, 2, "b1")
	tasks, err := factoryread.Published(h.ctx, h.layout.FactoryDB(), "talkable/talkable", 2)
	if err != nil || len(tasks) != 1 || tasks[0].Head() != "b1" {
		t.Fatalf("the factory's published task: %+v (%v), want task 9 approved at b1", tasks, err)
	}
	h.advance(time.Minute)
	h.open(prSpec{n: 1, head: "a1"}, prSpec{n: 2, head: "b1", author: "zhuravel"})
	h.tick()
	h.wantState(2, store.PRQueued)
	h.advance(5 * time.Minute)
	h.tick()
	pr := h.wantState(2, store.PRReviewed)
	ins := h.rd.all()
	if len(ins) != 1 {
		t.Fatalf("rounds: %d, want magnum's first round of the factory's PR", len(ins))
	}
	if in := ins[0]; in.Kind != pipeline.KindInitial || in.TargetSHA != "b1" || len(in.Roles) != 4 || in.Previous != nil || in.Blind {
		t.Fatalf("round input %+v, want a full initial round at b1 with every role and nothing earlier", in)
	}
	if deref(pr.ReviewedSHA) != "b1" || deref(pr.LastReviewID) == 0 {
		t.Fatalf("PR after the round: %+v, want b1 reviewed and the review posted", pr)
	}
}
