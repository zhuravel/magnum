package tui

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/zhuravel/magnum/internal/attention"
)

// attentionRow is PR n parked in needs_attention for kind (an
// attention.Kind*; "" when none was recorded).
func attentionRow(n int, kind string) PRBoardRow {
	return PRBoardRow{Owner: "talkable", Repo: "talkable", Number: n, Title: "Attention " + strconv.Itoa(n),
		State: "needs_attention", GHState: "OPEN", ActivityAt: ago(time.Hour), HeadSHA: "abc1234",
		LastError: "dependencies (bundle install) failed 3× on abc1234: could not find gem", AttentionKind: kind}
}

// attentionCases are the reason kinds the engine records with the word the
// attention pill names each by; an unknown kind and none keep the bare pill.
var attentionCases = []struct{ kind, word string }{
	{attention.KindFailed, "failed"},
	{attention.KindBlocked, "blocked"},
	{attention.KindIdentityError, "identity"},
	{attention.KindIdentityLeak, "identity"},
	{attention.KindFix, "fix"},
	{attention.KindOverloaded, "overload"},
	{attention.KindNoReview, "no review"},
	{"", ""},
	{"bogus", ""},
}

// The attention pill names its cause from the reason kind the engine
// recorded, "attention · failed", dimmed as every word after a pill is, in
// each icon mode; an unknown kind, or none, keeps the bare pill. Every kind
// the attention package knows has a word, the cause wins over a snooze
// (the card says both), and a row that is not in needs_attention shows
// none, whatever kind it carries.
func TestBoardShowsTheAttentionCause(t *testing.T) {
	for _, mode := range iconModes {
		m, _, _ := newBoard(t, 200, 40, PRBoardOptions{Icons: mode})
		p := m.painter()
		pill := ansi.Strip(p.stateCell("needs_attention").render(nil))
		for _, tc := range attentionCases {
			want := pill
			if tc.word != "" {
				want += "· " + tc.word
			}
			c := p.stateWaitCell(sanitizeRow(attentionRow(12300, tc.kind)))
			if got := ansi.Strip(c.render(nil)); got != want {
				t.Errorf("%s, kind %q: state cell = %q, want %q", mode, tc.kind, got, want)
			}
			if tc.word != "" && !hasSeg(c, "· "+tc.word, p.st.Dim.Render("· "+tc.word)) {
				t.Errorf("%s, kind %q: the cause is not dimmed: %q", mode, tc.kind, c.render(nil))
			}
		}

		snoozed := attentionRow(12301, attention.KindFix)
		snoozed.SnoozedUntil, snoozed.SnoozedAt = boardNow.Add(2*time.Hour), boardNow
		if got := ansi.Strip(p.stateWaitCell(sanitizeRow(snoozed)).render(nil)); got != pill+"· fix" {
			t.Errorf("%s: a snoozed PR that needs attention = %q, want %q", mode, got, pill+"· fix")
		}
		reviewed := attentionRow(12302, attention.KindFailed)
		reviewed.State = "reviewed"
		if got := ansi.Strip(p.stateWaitCell(sanitizeRow(reviewed)).render(nil)); strings.Contains(got, "failed") {
			t.Errorf("%s: a reviewed PR with a kind left behind = %q", mode, got)
		}
	}
	for _, k := range attention.Kinds() {
		if !slices.ContainsFunc(attentionCases, func(c struct{ kind, word string }) bool { return c.kind == k && c.word != "" }) {
			t.Errorf("attention kind %q has no word on the board", k)
		}
	}
}

// A reviewed PR with threads where magnum stopped arguing reads "reviewed ·
// argued", dimmed, beside the title's "!"; a round in flight, a wait, a
// snooze and an attention cause keep the cell, as do the cells that replace
// the pill (✔ auto, ✔ needs you), and a PR GitHub merged says merged.
func TestAReviewedStalemateReadsArgued(t *testing.T) {
	url := "https://github.com/talkable/example/pull/7#discussion_r101"
	argued := func() PRBoardRow {
		r := repliesRow(0)
		r.Stalemate = []string{url}
		return r
	}
	for _, mode := range iconModes {
		p := repliesPainter(mode, argued())
		reviewed := ansi.Strip(p.stateCell("reviewed").render(nil))
		c := p.stateWaitCell(sanitizeRow(argued()))
		if got := ansi.Strip(c.render(nil)); got != reviewed+"· argued" {
			t.Errorf("%s: state cell = %q, want %q", mode, got, reviewed+"· argued")
		}
		if !hasSeg(c, "· argued", p.st.Dim.Render("· argued")) {
			t.Errorf("%s: argued is not dimmed: %q", mode, c.render(nil))
		}
		if got := cellText(p.titleCell(sanitizeRow(argued()))); !strings.HasPrefix(got, p.g.errMark+" ") {
			t.Errorf("%s: the title lost its mark: %q", mode, got)
		}
		if got := ansi.Strip(p.stateWaitCell(sanitizeRow(repliesRow(0))).render(nil)); got != reviewed {
			t.Errorf("%s: a reviewed PR without a stalemate = %q", mode, got)
		}

		waits := argued()
		waits.State, waits.Wait = "rereview_pending", "re-review · quiet → 14:09"
		running := argued()
		running.State = "reviewing"
		running.Progress = &RoundProgress{StartedAt: boardNow.Add(-4 * time.Minute),
			Roles: []RoleProgress{workingRole("codex-judge", "cj", boardNow.Add(-4*time.Minute))}}
		snoozed := argued()
		snoozed.SnoozedUntil, snoozed.SnoozedAt = boardNow.Add(2*time.Hour), boardNow
		parked := argued()
		parked.State, parked.LastError, parked.AttentionKind = "needs_attention", "judge blocked: a dialog", attention.KindBlocked
		auto := argued()
		auto.AutoApproved = &AutoApproval{ReviewID: 9, Head: "abc1234def0", At: ago(time.Hour)}
		needsMe := argued()
		needsMe.NeedsMe = NeedsMeApprove
		merged := argued()
		merged.GHState = "MERGED"
		for _, tc := range []struct {
			name string
			row  PRBoardRow
			want string
		}{
			{"a wait", waits, "quiet → 14:09"},
			{"a round in flight", running, "4m"},
			{"a snooze", snoozed, "snoozed → "},
			{"an attention cause", parked, "· blocked"},
			{"an approval as the operator", auto, " auto "},
			{"a PR that needs the operator", needsMe, " needs you "},
			{"a merged PR", merged, "merged"},
		} {
			got := ansi.Strip(p.stateWaitCell(sanitizeRow(tc.row)).render(nil))
			if strings.Contains(got, "argued") || !strings.Contains(got, tc.want) {
				t.Errorf("%s, %s: state cell = %q, want %q and no argued", mode, tc.name, got, tc.want)
			}
		}
	}
}

// FINDINGS reads "held" in place of "clean" for the review whose approval
// waits for a full pass (FindingsInfo.Held), in the yellow of a wait, after
// the clean verdict's glyph and before whatever else the review left (its
// priorities, the earlier findings still open, its simplifications); a clean
// verdict posted as a comment with no hold (a comment-only repository) reads
// clean.
func TestTheFindingsCellReadsHeldForAHeldReview(t *testing.T) {
	for _, mode := range iconModes {
		p := repliesPainter(mode)
		ok, scissors := p.g.ok, p.g.simplify+p.g.gap
		p3 := p.priorityMark(3) + "P3" + p.g.times + "2"
		for _, tc := range []struct {
			name string
			f    FindingsInfo
			want string
		}{
			{"held", FindingsInfo{Verdict: "clean", Posted: "COMMENTED", Held: true}, ok + " held"},
			{"held with simplifications", FindingsInfo{Verdict: "clean", Posted: "COMMENTED", Held: true, Simplifications: 2}, ok + " held " + scissors + "2"},
			{"held with P3s", FindingsInfo{Verdict: "clean", Posted: "COMMENTED", Held: true, Counts: [4]int{0, 0, 0, 2}}, ok + " held " + p3},
			{"held with earlier findings open", FindingsInfo{Verdict: "clean", Posted: "COMMENTED", Held: true, Open: 2}, ok + " held 2 open"},
			{"a comment-only repository", FindingsInfo{Verdict: "clean", Posted: "COMMENTED"}, ok + " clean"},
			{"approved", FindingsInfo{Verdict: "clean", Posted: "APPROVED"}, ok + " clean"},
		} {
			c := p.findingsCell(&tc.f)
			if got := ansi.Strip(c.render(nil)); got != tc.want {
				t.Errorf("%s, %s: FINDINGS = %q, want %q", mode, tc.name, got, tc.want)
			}
			if tc.f.Held && !hasSeg(c, "held", p.pal.yellow.Render("held")) {
				t.Errorf("%s, %s: held is not yellow: %q", mode, tc.name, c.render(nil))
			}
		}
	}
}

// boardFactRows are the mockup of the three facts on a board: a PR whose
// setup failed and one whose fix loop needs the operator, a review held for
// a full pass, a reviewed PR with an argued thread, beside a plain clean
// review, a wait and a running delta check (the cells they share the
// columns with).
func boardFactRows() []PRBoardRow {
	base := func(n int, title, state string) PRBoardRow {
		return PRBoardRow{Owner: "talkable", Repo: "talkable", Number: n, Title: title, Author: "alice", State: state, GHState: "OPEN",
			ActivityAt: ago(40 * time.Minute), HeadSHA: "abc1234",
			LastReview:  &ReviewInfo{Login: "talkable[bot]", Event: "COMMENTED", SubmittedAt: ago(time.Hour), CommitSHA: "abc1234"},
			Reviewers:   []ReviewerInfo{{Login: "rev-ann", Verdict: "approved", SubmittedAt: ago(2 * time.Hour), CommitSHA: "abc1234"}},
			SinceReview: &ReviewDelta{Base: "reviewed", Commits: 1, Files: 2, Additions: 12, Deletions: 3},
			CI:          &CIInfo{State: "passed", Total: 65, Passed: 65}}
	}
	failed := base(12001, "Speed up the referral grid export", "needs_attention")
	failed.LastError, failed.AttentionKind = "dependencies (bundle install) failed 3× on abc1234: could not find gem", attention.KindFailed
	failed.Findings = &FindingsInfo{Counts: [4]int{0, 1, 2, 0}, Verdict: "blocking"}
	fix := base(12002, "Add the coupon list importer", "needs_attention")
	fix.LastError, fix.AttentionKind = "fix: 2 findings need your call", attention.KindFix
	fix.Findings = &FindingsInfo{Counts: [4]int{0, 0, 2, 0}, Verdict: "non_blocking"}
	held := base(12003, "Move the campaign builder to Hotwire", "reviewed")
	held.Findings = &FindingsInfo{Verdict: "clean", Posted: "COMMENTED", Simplifications: 2, Held: true}
	argued := base(12004, "Retry the webhook delivery on 5xx", "reviewed")
	argued.Findings = &FindingsInfo{Counts: [4]int{0, 0, 1, 0}, Verdict: "non_blocking"}
	argued.Stalemate = []string{"https://github.com/talkable/talkable/pull/12004#discussion_r1"}
	clean := base(12005, "Fix the widget alignment on Safari", "reviewed")
	clean.Findings = &FindingsInfo{Verdict: "clean", Posted: "APPROVED"}
	queued := base(12006, "Bump rails to 7.2.1", "rereview_pending")
	queued.Wait = "re-review · quiet → 14:09"
	queued.Findings = &FindingsInfo{Counts: [4]int{0, 0, 0, 2}, Verdict: "non_blocking"}
	start := boardNow.Add(-4 * time.Minute)
	run := base(12007, "Split the payouts job", "reviewing")
	run.Progress = &RoundProgress{StartedAt: start, Roles: []RoleProgress{{Role: "codex-judge", Label: "cj", Judge: true, Started: start, Working: true}}}
	run.RoundWhy = &RoundWhy{Kind: "rereview", DeltaCheck: true, DeltaLines: 4, Roles: []string{"codex-judge"}, At: start}
	run.Findings = &FindingsInfo{Counts: [4]int{0, 0, 1, 0}, Verdict: "non_blocking"}
	return []PRBoardRow{failed, fix, held, argued, clean, queued, run}
}

// On the board at 100, 140 and 200 columns, in each icon mode, the
// attention causes, the held review and the argued thread keep their words
// beside a running round and a wait; the rows are logged as the mockup the
// design log shows.
func TestTheAttentionHeldAndArguedWordsAcrossWidths(t *testing.T) {
	want := map[int]string{12001: "attention · failed", 12002: "attention · fix", 12003: " held ", 12004: "reviewed · argued", 12005: " clean"}
	for _, mode := range iconModes {
		for _, w := range []int{100, 140, 200} {
			v := viewOf(iconBoard(t, mode, w, 30, boardFactRows()...))
			lines := []string{strings.TrimRight(lineWith(t, v, "STATE"), " ")}
			for _, r := range boardFactRows() {
				line := lineWith(t, v, "#"+strconv.Itoa(r.Number))
				lines = append(lines, strings.TrimRight(line, " "))
				if word, ok := want[r.Number]; ok && !strings.Contains(line, word) {
					t.Errorf("%s at %d: #%d lacks %q: %q", mode, w, r.Number, word, line)
				}
				if ansi.StringWidth(line) > w {
					t.Errorf("%s at %d: #%d is %d wide", mode, w, r.Number, ansi.StringWidth(line))
				}
			}
			t.Logf("%s at %d columns:\n%s", mode, w, strings.Join(lines, "\n"))
		}
	}
}
