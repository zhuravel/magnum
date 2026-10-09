package engine

// Declines in the retro (DECISIONS "The retro classifies the replies to
// magnum's own findings", "A missing-context decline is a repo miss for the
// notes"): the classifier's answer for each of magnum's threads that
// someone answered (learn.OwnThread) becomes a declines row with the run
// that posted the review, its findings row and the skill copy the round ran
// with. A finding the author declined because the repository does it
// otherwise (verdict missing_context, scope repo) is also a miss for the
// repository's notes, so the notes curation takes it unchanged. Reply text
// stays in the retro's files: no row, event or log line carries it.

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/learn"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// skillCopyRe finds the judge skill copy a prompt names:
// <state>/skill/<12 hex>/SKILL.md (config.SnapshotPrompts).
var skillCopyRe = regexp.MustCompile(`/skill/([0-9a-f]{12})/`)

// skillSHAOf is the 12-hex hash of the first skill copy promptText names
// ("" when it names none: a dry run, a judge that reads the skill in
// place).
func skillSHAOf(promptText string) string {
	if m := skillCopyRe.FindStringSubmatch(promptText); m != nil {
		return m[1]
	}
	return ""
}

// routesToNotes reports whether a thread item is also a miss for the
// repository's notes: magnum was right in general, wrong for this
// repository, and the lesson is the repository's.
func routesToNotes(it learn.ThreadItem) bool {
	return it.Verdict == store.VerdictMissingContext && it.Scope == store.MissScopeRepo
}

// storeDeclines stores the classifier's answer for each of pr's own
// threads as a decline (store.UpsertDecline, which keeps the state of an
// earlier retro's row): the run that posted the thread's review (runs),
// the skill hash of its prompt, the findings row the thread's finding
// names (none when the run recorded no findings), and the lesson scrubbed
// of the PR's author, the repliers and magnum's logins (learn.ScrubLesson;
// a dropped lesson is a retro.lesson_rejected event, never its text). A
// missing-context item of scope repo is also upserted as a miss for the
// notes (routeDecline). It returns how many declines it stored and how
// many of them the author declined or argued; the error joins the rows it
// could not store.
func (e *Engine) storeDeclines(ctx context.Context, pr store.PR, threads []learn.OwnThread, items map[string]learn.ThreadItem,
	runs []store.Run) (stored, declined int, err error) {
	if len(threads) == 0 || len(items) == 0 {
		return 0, 0, nil
	}
	wctx := context.WithoutCancel(ctx)
	repo, err := e.st.RepoByID(wctx, pr.RepoID)
	if err != nil {
		return 0, 0, err
	}
	findings, err := e.st.FindingsByPR(wctx, pr.ID)
	if err != nil {
		return 0, 0, err
	}
	subject := prSubject(repo, pr.Number)
	rows := map[string]int64{} // "<run id>/<finding id>" -> findings row
	for _, f := range findings {
		rows[f.RunID+"/"+f.FindingID] = f.ID
	}
	byReview := map[int64]store.Run{}
	for _, r := range runs {
		if r.ReviewID != nil {
			byReview[*r.ReviewID] = r
		}
	}
	own := e.retroOwnLogins(pr, runs)
	repoWords := []string{repo.Owner, repo.Name}
	var errs []error
	for _, t := range threads {
		it, ok := items[t.ID]
		if !ok {
			continue
		}
		people := []string{deref(pr.AuthorLogin)}
		for _, r := range t.Replies {
			if r.Author != learn.OwnAuthor {
				people = append(people, r.Author)
			}
		}
		if it.Lesson != "" {
			var reason string
			if it.Lesson, reason = learn.ScrubLesson(it.Lesson, it.Scope, people, own, repoWords); reason != "" {
				e.event(wctx, "info", subject, "retro.lesson_rejected", fmt.Sprintf("retro: the lesson of %s was dropped (%s)", t.ID, reason),
					map[string]any{"candidate": t.ID, "reason": reason})
			}
		}
		d := store.Decline{PRID: pr.ID, SourceURL: t.URL, CommentID: t.CommentID, ReviewID: t.ReviewID, Path: t.Path, Line: t.Line,
			Title: t.Title, Priority: t.Priority, ReviewedSHA: t.ReviewedSHA, Outcome: it.Outcome, Verdict: it.Verdict,
			Lesson: it.Lesson, Scope: it.Scope, Match: it.Match}
		if r, ok := byReview[t.ReviewID]; ok && t.ReviewID > 0 {
			d.RunID, d.SkillSHA = r.ID, skillSHAOf(r.PromptText)
		}
		if id, ok := rows[t.FindingRef]; ok && t.FindingRef != "" {
			d.FindingRowID = &id
		}
		got, err := e.st.UpsertDecline(wctx, d)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.ID, err))
			continue
		}
		stored++
		if got.Outcome == store.DeclineDeclined || got.Outcome == store.DeclineArgued {
			declined++
		}
		if routesToNotes(it) {
			if err := e.routeDecline(wctx, pr, t, it); err != nil {
				errs = append(errs, fmt.Errorf("%s as a miss: %w", t.ID, err))
			}
		}
	}
	return stored, declined, errors.Join(errs...)
}

// routeDecline upserts own thread t, answered missing_context with scope
// repo, as a miss for the repository's notes: class miss, scope repo, the
// finding's priority, title (clipped to learn.TitleMax), path and line,
// the item's lesson and patterns, and as its reviewer the first person who
// answered (a bot only with [learn] include_bots).
func (e *Engine) routeDecline(ctx context.Context, pr store.PR, t learn.OwnThread, it learn.ThreadItem) error {
	reviewer := deref(pr.AuthorLogin)
	for _, r := range t.Replies {
		if r.Author != learn.OwnAuthor && (e.cfg.Learn.IncludeBots || !github.IsBot("", r.Author)) {
			reviewer = r.Author
			break
		}
	}
	m := store.Miss{PRID: pr.ID, SourceURL: t.URL, SourceKind: store.MissSourceThread, Reviewer: reviewer, Path: t.Path, Line: t.Line,
		ReviewedSHA: t.ReviewedSHA, Class: store.MissMiss, Severity: t.Priority, Title: textx.Clip(t.Title, learn.TitleMax),
		Lesson: it.Lesson, Scope: store.MissScopeRepo, Match: it.Match}
	if t.Line > 0 {
		m.Lines = []int{t.Line, t.Line}
	}
	_, err := e.st.UpsertMiss(ctx, m)
	return err
}

// declinesForNotes counts pr's own threads routed to the notes
// (routesToNotes) whose miss is still new: what the retro adds to the
// repository's misses for its next curation (markRepoMisses).
func (e *Engine) declinesForNotes(ctx context.Context, pr store.PR, threads []learn.OwnThread, items map[string]learn.ThreadItem) int {
	routed := map[string]bool{}
	for _, t := range threads {
		if it, ok := items[t.ID]; ok && routesToNotes(it) {
			routed[t.URL] = true
		}
	}
	if len(routed) == 0 {
		return 0
	}
	ms, err := e.st.Misses(context.WithoutCancel(ctx), store.MissFilter{PRID: pr.ID, Classes: []string{store.MissMiss},
		Scopes: []string{store.MissScopeRepo}, States: []string{store.MissNew}})
	if err != nil {
		e.log.Warn("retro: count the declines for the notes", "pr", pr.ID, "err", err)
		return 0
	}
	n := 0
	for _, m := range ms {
		if routed[m.SourceURL] {
			n++
		}
	}
	return n
}
