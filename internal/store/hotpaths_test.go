package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hotSeed is what seedHot recorded.
type hotSeed struct {
	repo  Repo
	prIDs []int64
	nodes []string // the open PRs' node ids, the radar
}

// seedHot fills a registry the way months of rounds leave it: prs PRs of one
// repository (the first open of them open, the rest closed and released),
// runsPerPR verified runs each over rounds of three roles with a prompt, a
// closed session per run, and on the first PR a live session and a run in
// flight.
func seedHot(tb testing.TB, st *Store, prs, open, runsPerPR int) hotSeed {
	tb.Helper()
	ctx := context.Background()
	repo, err := st.UpsertRepo(ctx, Repo{NodeID: "R_1", Owner: "talkable", Name: "talkable", WatchOwner: "talkable",
		DefaultBranch: "master", Mode: RepoModePool})
	if err != nil {
		tb.Fatal(err)
	}
	seed := hotSeed{repo: repo}
	for n := 1; n <= prs; n++ {
		res, err := st.UpsertPRFromGitHub(ctx, GitHubPR{
			RepoID: repo.ID, NodeID: "PR_" + strconv.Itoa(n), Number: n,
			URL: "https://github.com/talkable/talkable/pull/" + strconv.Itoa(n), HeadSHA: fmt.Sprintf("%040d", n),
			Title: new("A change of some size " + strconv.Itoa(n)), GHState: GHOpen, Labels: []string{"bug", "backend"},
			Assignees: []string{"alice"}, RequestedReviewers: []string{"zhuravel", "team:core"},
			LatestReviews: []LatestReview{{Login: "rev-ann", State: "APPROVED"}},
			InitialState:  PRReviewed, Identity: "talkable-app",
		})
		if err != nil {
			tb.Fatal(err)
		}
		seed.prIDs = append(seed.prIDs, res.PR.ID)
		if n <= open {
			seed.nodes = append(seed.nodes, res.PR.NodeID)
		}
	}
	prompt := strings.Repeat("Review the change at the URL below. ", 120) // ~4 KiB, as the prompts are
	err = st.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE prs SET gh_state = ?, state = ? WHERE repo_id = ? AND number > ?",
			GHClosed, PRReleased, repo.ID, open); err != nil {
			return err
		}
		ts := FormatTime(t0)
		roles := []string{"codex-judge", "claude-review", "codex-review"}
		for _, prID := range seed.prIDs {
			for i := range runsPerPR {
				role := roles[i%len(roles)]
				res, err := tx.ExecContext(ctx, `INSERT INTO sessions (pr_id, role, generation, agent_kind, env_json, state, started_at)
VALUES (?, ?, ?, 'codex', '{}', 'closed', ?)`, prID, role, i+1, ts)
				if err != nil {
					return err
				}
				sid, _ := res.LastInsertId()
				if _, err := tx.ExecContext(ctx, `INSERT INTO runs (id, pr_id, round, role, session_id, kind, target_sha, identity,
  reviewer_login, state, prompt_text, created_at) VALUES (?, ?, ?, ?, ?, 'initial', 'h', 'talkable-app', 'talkable[bot]', 'verified', ?, ?)`,
					fmt.Sprintf("r-%d-%d", prID, i), prID, i/len(roles)+1, role, sid, prompt, ts); err != nil {
					return err
				}
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO sessions (pr_id, role, generation, agent_kind, env_json, state, started_at)
VALUES (?, 'codex-judge', 1000, 'codex', '{}', 'live', ?)`, seed.prIDs[0], ts)
		if err != nil {
			return err
		}
		sid, _ := res.LastInsertId()
		_, err = tx.ExecContext(ctx, `INSERT INTO runs (id, pr_id, round, role, session_id, kind, target_sha, identity,
  reviewer_login, state, prompt_text, created_at) VALUES ('r-live', ?, 1000, 'codex-judge', ?, 'initial', 'h', 'talkable-app', 'talkable[bot]', 'working', ?, ?)`,
			seed.prIDs[0], sid, prompt, ts)
		return err
	})
	if err != nil {
		tb.Fatal(err)
	}
	return seed
}

// benchStore is newStore for a benchmark: a copy of the migrated image.
func benchStore(b *testing.B) *Store {
	b.Helper()
	image, err := migratedImage()
	if err != nil {
		b.Fatal(err)
	}
	path := filepath.Join(b.TempDir(), "magnum.db")
	if err := os.WriteFile(path, image, 0o600); err != nil {
		b.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { st.Close() })
	return st
}

// BenchmarkHotQueries times the queries the daemon's ticks and the board run
// on a registry of 2,000 PRs (100 open) with 30 runs and sessions each.
func BenchmarkHotQueries(b *testing.B) {
	st := benchStore(b)
	seed := seedHot(b, st, 2000, 100, 30)
	ctx := context.Background()
	board := seed.prIDs[:100]
	b.Run("ActiveRuns", func(b *testing.B) {
		for b.Loop() {
			if runs, err := st.ActiveRuns(ctx); err != nil || len(runs) != 1 {
				b.Fatal(len(runs), err)
			}
		}
	})
	b.Run("LiveSessions", func(b *testing.B) {
		for b.Loop() {
			if ss, err := st.LiveSessions(ctx); err != nil || len(ss) != 1 {
				b.Fatal(len(ss), err)
			}
		}
	})
	b.Run("LiveSessionByPRRole", func(b *testing.B) {
		for b.Loop() {
			if _, err := st.LiveSessionByPRRole(ctx, seed.prIDs[0], "codex-judge"); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("LatestRoundRuns/board", func(b *testing.B) {
		for b.Loop() {
			if m, err := st.LatestRoundRuns(ctx, board...); err != nil || len(m) != len(board) {
				b.Fatal(len(m), err)
			}
		}
	})
	b.Run("LatestRoundRuns/all", func(b *testing.B) {
		for b.Loop() {
			if m, err := st.LatestRoundRuns(ctx); err != nil || len(m) != len(seed.prIDs) {
				b.Fatal(len(m), err)
			}
		}
	})
	b.Run("ListPRs/repo", func(b *testing.B) {
		for b.Loop() {
			if prs, err := st.ListPRs(ctx, PRFilter{RepoID: seed.repo.ID}); err != nil || len(prs) != len(seed.prIDs) {
				b.Fatal(len(prs), err)
			}
		}
	})
	b.Run("ListPRs/poll", func(b *testing.B) {
		for b.Loop() {
			if prs, err := st.ListPRs(ctx, PRFilter{RepoID: seed.repo.ID, GHOpen: true, OrNodeIDs: seed.nodes}); err != nil || len(prs) != len(seed.nodes) {
				b.Fatal(len(prs), err)
			}
		}
	})
}

// The observe tick reads the in-flight runs and the live sessions of tables
// no retention prunes. Their queries name the states as the partial
// indexes' predicates do, so SQLite searches runs_active and
// sessions_live_role instead of reading every run and session ever made
// (bound parameters never match an index predicate).
func TestTheLiveQueriesUseThePartialIndexes(t *testing.T) {
	st, _ := newStore(t)
	for _, c := range []struct {
		name, query, index string
		args               []any
	}{
		{"ActiveRuns", activeRunsQuery, "runs_active", nil},
		{"LiveSessions", liveSessionsQuery, "sessions_live_role", nil},
		{"LiveSessionByPRRole", liveSessionByPRRoleQuery, "sessions_live_role", []any{1, RoleJudge}},
	} {
		if plan := queryPlan(t, st, c.query, c.args...); !strings.Contains(plan, "USING INDEX "+c.index) {
			t.Errorf("%s plan: %s\nwant it to use %s", c.name, plan, c.index)
		}
	}
}

// The literal state lists select what the bound ones did: the runs still in
// flight, oldest first, and the starting and live sessions by id.
func TestTheLiveQueriesReturnTheLiveRowsOldestFirst(t *testing.T) {
	st, clk := newStore(t)
	ctx := context.Background()
	repo := mustRepo(t, st)
	var sessionIDs []int64
	for i, state := range []string{SessionClosed, SessionLive, SessionParked, SessionStarting, SessionLost} {
		pr := mustPR(t, st, repo.ID, i+1, PRReviewing)
		x, err := st.CreateSession(ctx, Session{PRID: pr.ID, Role: RoleJudge, State: state})
		if err != nil {
			t.Fatal(err)
		}
		if state == SessionLive || state == SessionStarting {
			sessionIDs = append(sessionIDs, x.ID)
		}
		if got, err := st.LiveSessionByPRRole(ctx, pr.ID, RoleJudge); (state == SessionLive || state == SessionStarting) != (err == nil) ||
			err == nil && got.ID != x.ID {
			t.Errorf("LiveSessionByPRRole of a %s session = %+v, %v", state, got, err)
		}
	}
	var runIDs []string
	for i, state := range []string{RunVerified, RunEnded, RunFailed, RunPending, RunAbandoned, RunWorking, RunSubmitted} {
		clk.Add(time.Minute)
		r, err := st.CreateRun(ctx, Run{PRID: 1, Round: i + 1, Role: RoleJudge, Kind: RunInitial, State: state,
			TargetSHA: "h", Identity: "talkable-app", ReviewerLogin: "talkable[bot]"})
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(activeRunStates, state) {
			runIDs = append(runIDs, r.ID)
		}
	}
	live, err := st.LiveSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(live, func(x Session) int64 { return x.ID }); !slices.Equal(got, sessionIDs) {
		t.Errorf("LiveSessions = %v, want %v", got, sessionIDs)
	}
	active, err := st.ActiveRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(active, func(r Run) string { return r.ID }); !slices.Equal(got, runIDs) {
		t.Errorf("ActiveRuns = %v, want %v", got, runIDs)
	}
}

func ids[T any, K comparable](xs []T, key func(T) K) []K {
	out := make([]K, len(xs))
	for i, x := range xs {
		out[i] = key(x)
	}
	return out
}
