package engine

import (
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/factory/registry/registrytest"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
)

// seedFactory writes the factory's registry where the layout keeps it,
// with the factory's own schema (registrytest.Seed): task 1 published PR n
// of talkable/talkable on retroShaA, and its local round found one thing,
// next to t103 (app/models/same.rb:9). The task's directory holds its plan,
// ticket and answers.
func seedFactory(h *harness, n int) {
	h.t.Helper()
	path := h.layout.FactoryDB()
	registrytest.Seed(h.t, path)
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		h.t.Fatal(err)
	}
	defer db.Close()
	ts := "2026-10-02T10:00:00Z"
	for _, q := range []string{
		`INSERT INTO tasks (id, repo, state, created_at, updated_at) VALUES (1, 'talkable/talkable', 'published', '` + ts + `', '` + ts + `')`,
		`INSERT INTO pr_links (task_id, repo, number, opened_head, approved_head, opened_at) VALUES (1, 'talkable/talkable', ` +
			strconv.Itoa(n) + `, '` + retroShaA + `', '` + retroShaA + `', '` + ts + `')`,
		`INSERT INTO review_findings (task_id, fingerprint, path, start_line, end_line, title_hash, priority, first_cycle, last_cycle)
		 VALUES (1, 'fp1', 'app/models/same.rb', 10, 10, '0123456789ab', 'P2', 1, 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			h.t.Fatalf("factory fixture: %v\n%s", err, q)
		}
	}
	dir := h.layout.FactoryTaskDir(1)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	for name, text := range map[string]string{"report.md": "# Plan\nKeep the tenant scope.\n", "ticket.md": "TK-1 Coupons per site\n",
		"answers.md": "Q1: per site\n", "notes.md": "the liaison's\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			h.t.Fatal(err)
		}
	}
}

// allMisses are pr's misses by comment id, the posted round's catches
// included.
func (h *harness) allMisses(prID int64) map[string]store.Miss {
	h.t.Helper()
	ms, err := h.st.Misses(h.ctx, store.MissFilter{PRID: prID, WithPostedCaught: true})
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]store.Miss{}
	for _, m := range ms {
		out[m.SourceURL[strings.LastIndexAny(m.SourceURL, "r-")+1:]] = m
	}
	return out
}

// stagedClassifier answers t101 and t105 as misses and every other
// candidate as style, each with stage (a stage on a candidate not of the
// factory is dropped).
func stagedClassifier(stage map[string]string) *fakeClassifier {
	return &fakeClassifier{answer: func(job ClassifyJob) (ClassifyResult, error) {
		var out learn.Output
		for _, c := range job.Candidates {
			it := learn.Item{ID: c.ID, Class: store.MissStyle, Stage: stage[c.ID]}
			if c.ID == "t101" || c.ID == "t105" {
				it = learn.Item{ID: c.ID, Class: store.MissMiss, Severity: "P2", Title: "Coupon lookup ignores the site",
					Lesson: "When a finder takes a code from the request, check it is scoped to the current tenant.",
					Scope:  store.MissScopeGeneral, Lines: []int{40, 42}, Match: []string{"tenant"}, Stage: stage[c.ID]}
			}
			out.Items = append(out.Items, it)
		}
		b, _ := json.Marshal(out)
		return ClassifyResult{}, os.WriteFile(job.OutputPath, b, 0o600)
	}}
}

// On a pull request the factory published, the retro reads the factory's
// local round from factory.db: a comment that round missed is stored with
// origin factory and the classifier's stage, given the task's plan, ticket
// and answers; one the local round caught is as before; and one only
// magnum's posted round caught (t105, next to its posted finding) is
// recorded as a factory-origin miss that the review skill's learners never
// see.
func TestTheRetroOfAFactoryPRTagsWhatTheLocalRoundMissedWithItsStage(t *testing.T) {
	fc := stagedClassifier(map[string]string{"t101": "code", "t105": "review", "r201": "plan", "t103": "plan"})
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	retroFinding(h, pr)
	seedRetroGitHub(h, 7)
	seedFactory(h, 7)

	h.requestRetro(RetroPayload{})
	ms := h.allMisses(pr.ID)
	for id, want := range map[string]struct {
		origin, stage, class string
		posted               bool
	}{
		"101": {store.MissOriginFactory, store.MissStageCode, store.MissMiss, false},
		"105": {store.MissOriginFactory, store.MissStageReview, store.MissMiss, true},
		"201": {store.MissOriginFactory, store.MissStagePlan, store.MissStyle, false},
		"103": {"", "", store.MissStyle, false},   // the local round caught it
		"102": {"", "", store.MissOutside, false}, // outside: no part in the local round's
	} {
		m, ok := ms[id]
		if !ok || m.Origin != want.origin || m.Stage != want.stage || m.Class != want.class || m.PostedCaught != want.posted {
			t.Errorf("%s = %+v (stored %v); want origin %q, stage %q, class %s, posted caught %v", id, m, ok, want.origin, want.stage,
				want.class, want.posted)
		}
	}
	// The review skill's learners read only what both rounds missed.
	general, err := h.st.Misses(h.ctx, store.MissFilter{Classes: []string{store.MissMiss}, Scopes: []string{store.MissScopeGeneral}})
	if err != nil || len(general) != 1 || !strings.HasSuffix(general[0].SourceURL, "r101") {
		t.Errorf("the review skill's misses = %+v, %v; want t101 alone", general, err)
	}
	if len(fc.jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(fc.jobs))
	}
	job := fc.jobs[0]
	if job.Factory != filepath.Join(job.Dir, learn.FactoryDir) {
		t.Fatalf("job.Factory = %q", job.Factory)
	}
	for name, want := range map[string]string{"plan.md": "# Plan\nKeep the tenant scope.\n", "ticket.md": "TK-1 Coupons per site\n",
		"answers.md": "Q1: per site\n"} {
		if b, err := os.ReadFile(filepath.Join(job.Factory, name)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v", name, b, err)
		}
	}
	if ents, _ := os.ReadDir(job.Factory); len(ents) != 3 {
		t.Errorf("the factory files are %v, want the plan, the ticket and the answers alone", ents)
	}
	cands, err := readCandidates(job.CandidatesPath)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]learn.Candidate{}
	for _, c := range cands.Candidates {
		byID[c.ID] = c
	}
	if c := byID["t105"]; c.Origin != learn.OriginFactory || !c.PostedCaught {
		t.Errorf("t105 in the candidates file = %+v", c)
	}
	sum := h.retroLast()
	if sum.Misses != 1 || sum.Factory != 2 || sum.Caught != 1 {
		t.Errorf("summary = %+v; want 1 miss of magnum's, 2 factory-origin and 1 caught", sum)
	}
	evs := h.noCommentText("pr:talkable/talkable#7")
	if ok := evs[len(evs)-1]; ok.Kind != "retro.ok" || !strings.Contains(ok.Message, "2 factory-origin") {
		t.Errorf("retro.ok = %s %q", ok.Kind, ok.Message)
	}
}

// A pull request the factory did not publish is retro'd as before, even
// with factory.db there: no origin, no factory files, t105 caught.
func TestTheRetroOfAPRTheFactoryDidNotPublishIsAsBefore(t *testing.T) {
	fc := stagedClassifier(map[string]string{"t101": "code"})
	h := newHarness(t, withClassifier(fc))
	pr := retroPR(h, 7, 24*time.Hour)
	retroFinding(h, pr)
	seedRetroGitHub(h, 7)
	seedFactory(h, 8)

	h.requestRetro(RetroPayload{})
	ms := h.allMisses(pr.ID)
	if len(ms) != 4 {
		t.Fatalf("misses = %d, want t101, t102, t103 and r201", len(ms))
	}
	for id, m := range ms {
		if m.Origin != "" || m.Stage != "" || m.PostedCaught {
			t.Errorf("%s tagged: %+v", id, m)
		}
	}
	if len(fc.jobs) != 1 || fc.jobs[0].Factory != "" {
		t.Errorf("jobs = %+v", fc.jobs)
	}
	if _, err := os.Stat(filepath.Join(fc.jobs[0].Dir, learn.FactoryDir)); !os.IsNotExist(err) {
		t.Errorf("the retro copied factory files: %v", err)
	}
	if sum := h.retroLast(); sum.Caught != 1 || sum.Factory != 0 {
		t.Errorf("summary = %+v", sum)
	}
}

// Without factory.db (no factory ever ran here), or with one that cannot
// be read, the retro runs exactly as before and says why once per retro,
// however many PRs it looks at.
func TestTheRetroRunsAsBeforeWithoutAReadableFactoryRegistryAndSaysWhyOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
		say   string
	}{
		{"missing", func(*harness) {}, "no factory registry"},
		{"unreadable", func(h *harness) {
			if err := os.MkdirAll(filepath.Dir(h.layout.FactoryDB()), 0o700); err != nil {
				h.t.Fatal(err)
			}
			if err := os.WriteFile(h.layout.FactoryDB(), []byte("not a database, only text that is long enough"), 0o600); err != nil {
				h.t.Fatal(err)
			}
		}, "the factory's registry cannot be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuffer{}
			h := newHarness(t, func(h *harness) {
				h.d.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			})
			prs := []store.PR{retroPR(h, 7, 24*time.Hour), retroPR(h, 8, 24*time.Hour)}
			retroFinding(h, prs[0])
			seedRetroGitHub(h, 7)
			seedRetroGitHub(h, 8)
			tc.setup(h)

			h.requestRetro(RetroPayload{})
			if sum := h.retroLast(); sum.PRs != 2 || sum.Caught != 1 || sum.Factory != 0 || sum.Stopped != "" {
				t.Errorf("summary = %+v", sum)
			}
			for _, pr := range prs {
				for id, m := range h.allMisses(pr.ID) {
					if m.Origin != "" || m.PostedCaught {
						t.Errorf("#%d %s tagged: %+v", pr.Number, id, m)
					}
				}
			}
			if n := strings.Count(logs.String(), tc.say); n != 1 {
				t.Errorf("the log says %q %d times, want once:\n%s", tc.say, n, logs.String())
			}
			if _, err := os.Stat(h.layout.FactoryDB()); tc.name == "missing" && !os.IsNotExist(err) {
				t.Errorf("the retro created the factory's registry: %v", err)
			}
		})
	}
}
