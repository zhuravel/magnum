package pipeline

import (
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/github"
)

// GitHub keeps an HTML block (</details>, a comment) running until a blank
// line, so a footer glued to one is posted as raw text. After a review is
// verified, a footer glued to the line before it gets its blank line (the
// body is rewritten once, through UpdateReviewBody); a footer that is its
// own paragraph already, or a review without one, is left alone.
func TestVerifiedReviewGetsItsFooterOnItsOwnParagraph(t *testing.T) {
	footer := config.DefaultReviewFooter
	for name, tc := range map[string]struct {
		after   string
		rewrite bool
	}{
		"glued to the marker":  {"\n" + footer, true},
		"on its own paragraph": {"\n\n" + footer + "\n", false},
		"no footer":            {"\nthanks", false},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			post := e.judgePosts(665, "COMMENTED", "COMMENT")
			post.body = "**Verdict** findings\n\n<details><summary>Checks</summary>\n\nspecs ran\n</details>"
			post.after = tc.after
			e.ag.behaviors[agents.RoleJudge] = []behavior{post.behavior(t)}
			res, err := e.r.RunRound(e.ctx, e.input(KindInitial))
			if err != nil || res.Outcome != OutcomePosted {
				t.Fatalf("RunRound = %+v, %v", res, err)
			}
			if !tc.rewrite {
				if len(e.gh.updates) != 0 {
					t.Fatalf("updates = %+v, want none", e.gh.updates)
				}
				return
			}
			if len(e.gh.updates) != 1 {
				t.Fatalf("updates = %+v, want one", e.gh.updates)
			}
			got := e.gh.updates[0].Body
			if !strings.HasSuffix(got, " -->\n\n"+footer) || !strings.HasPrefix(got, post.body+"\n<!-- magnum:run=") {
				t.Fatalf("rewritten body = %q", got)
			}
		})
	}
}

// footerParagraph is idempotent and handles the footer's places: glued to
// the line before (an LF or CRLF), on the same line, at the start of the
// body, after a blank line (spaces included).
func TestFooterParagraph(t *testing.T) {
	const f = "_Automated review by Magnum._"
	for _, tc := range []struct {
		body, want string
		changed    bool
	}{
		{"</details>\n<!-- m -->\n" + f, "</details>\n<!-- m -->\n\n" + f, true},
		{"</details>\r\n" + f + "\r\n", "</details>\n\n" + f + "\r\n", true},
		{"</details> " + f, "</details>\n\n" + f, true},
		{"</details>\n\n" + f, "</details>\n\n" + f, false},
		{"</details>\n  \n" + f, "</details>\n  \n" + f, false},
		{f, f, false},
		{"no footer here", "no footer here", false},
	} {
		got, changed := footerParagraph(tc.body, f)
		if got != tc.want || changed != tc.changed {
			t.Errorf("footerParagraph(%q) = %q, %v; want %q, %v", tc.body, got, changed, tc.want, tc.changed)
		}
		if again, changed := footerParagraph(got, f); changed || again != got {
			t.Errorf("footerParagraph is not idempotent on %q: %q", got, again)
		}
	}
	if got, changed := footerParagraph("x\n"+f, ""); changed || got != "x\n"+f {
		t.Errorf("no footer configured: %q, %v", got, changed)
	}
}

// AppendToReview puts its note before the identity's footer when the body
// ends with it, so the footer stays the last paragraph (and gets its blank
// line); a body without the footer gets the note last. Both are idempotent.
func TestAppendToReviewKeepsTheFooterLast(t *testing.T) {
	e := newEnv(t)
	footer := config.DefaultReviewFooter
	line := "_Reviewed abc1234; 1 commit arrived during the review, re-review follows._"
	for _, tc := range []struct {
		id         int64
		body, want string
	}{
		{710, "**Verdict**\n<!-- magnum:run=r-1 head=abc1234 -->\n\n" + footer + "\n",
			"**Verdict**\n<!-- magnum:run=r-1 head=abc1234 -->\n\n" + line + "\n\n" + footer},
		{711, "**Verdict**\n<!-- magnum:run=r-1 head=abc1234 -->\n" + footer,
			"**Verdict**\n<!-- magnum:run=r-1 head=abc1234 -->\n\n" + line + "\n\n" + footer},
		{712, "**Verdict**\n<!-- magnum:run=r-1 head=abc1234 -->\n",
			"**Verdict**\n<!-- magnum:run=r-1 head=abc1234 -->\n\n" + line},
	} {
		e.gh.add(github.Review{DatabaseID: tc.id, Body: tc.body}, github.RESTReview{ID: tc.id, UserLogin: "talkable[bot]", UserType: "Bot", Body: tc.body})
		before := len(e.gh.updates)
		for range 2 { // the second call finds the note already there
			if err := e.r.AppendToReview(e.ctx, "talkable", "talkable", 11920, tc.id, line); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(e.gh.updates) - before; n != 1 || e.gh.updates[len(e.gh.updates)-1].Body != tc.want {
			t.Errorf("review %d: %d updates, last body %q; want %q", tc.id, n, e.gh.updates[len(e.gh.updates)-1].Body, tc.want)
		}
	}
}
