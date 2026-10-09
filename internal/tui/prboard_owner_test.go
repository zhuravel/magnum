package tui

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// ownerRows are boardRows (owned by talkable) with a PR of the example
// organization that is ready to merge and one of the user alice.
func ownerRows() []PRBoardRow {
	return append(boardRows(),
		PRBoardRow{Ref: "example/widgets#7", Owner: "example", Repo: "widgets", Number: 7, Title: "Widget OAuth settings", Author: "bob",
			State: "reviewed", GHState: "OPEN", ActivityAt: ago(10 * time.Minute), HeadSHA: "7777777",
			Reviewers: []ReviewerInfo{{Login: "bob", Verdict: "approved", CommitSHA: "7777777"}}},
		PRBoardRow{Ref: "alice/dotfiles#3", Owner: "alice", Repo: "dotfiles", Number: 3, Title: "Dotfiles cleanup", Author: "alice",
			State: "queued", GHState: "OPEN", ActivityAt: ago(30 * time.Minute)},
	)
}

// ownerBoard is a board w x h whose first rows are ownerRows.
func ownerBoard(t *testing.T, w, h int, opts PRBoardOptions) prBoardModel {
	t.Helper()
	opts.Now = func() time.Time { return boardNow }
	if opts.SelfLogins == nil {
		opts.SelfLogins = boardSelf
	}
	src := &fakeBoardSource{rows: ownerRows()}
	m := testPRBoard(context.Background(), src, &fakeActions{}, opts)
	m, _ = send(t, m, tea.WindowSizeMsg{Width: w, Height: h}, prbDataMsg{rows: src.rows})
	return m
}

// O cycles the owner scope: every owner, then each owner with PRs (the
// default repository's owner first, then alphabetically), then every
// owner again; the footer says which.
func TestPRBoardOwnerKeyCycles(t *testing.T) {
	for _, tc := range []struct {
		defaultRepo string
		want        []string
	}{
		{"", []string{"alice", "example", "talkable", ""}},
		{"talkable/talkable", []string{"talkable", "alice", "example", ""}},
		{"Example/site", []string{"example", "alice", "talkable", ""}}, // case does not matter
		{"nobody/else", []string{"alice", "example", "talkable", ""}},
	} {
		m := ownerBoard(t, 200, 30, PRBoardOptions{DefaultRepo: tc.defaultRepo})
		var got []string
		for range tc.want {
			m, _ = send(t, m, keyMsg("O"))
			got = append(got, m.owner)
			mustContain(t, viewOf(m), "owner: "+cmp.Or(m.owner, "all"))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("default repo %q: O cycles %q, want %q", tc.defaultRepo, got, tc.want)
		}
	}

	m := ownerBoard(t, 200, 30, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("O"))
	if got := boardRefs(m); !slices.Equal(got, []string{"alice/dotfiles#3"}) {
		t.Errorf("owner alice shows %v", got)
	}
	m, _ = send(t, m, keyMsg("O"), keyMsg("O"), keyMsg("O"))
	if got := len(m.view); got != len(ownerRows()) {
		t.Errorf("back to every owner: %d rows, want %d", got, len(ownerRows()))
	}

	// The help and the footer name the key.
	mustContain(t, viewOf(m), "O owner")
	h, _ := send(t, m, keyMsg("?"))
	mustContain(t, viewOf(h), "v / O", "next view / next owner (all, then each)")
}

// The owner scope narrows the view and the filter: a row shows when it
// matches all three, and the view's count follows the scope.
func TestPRBoardOwnerCombinesWithViewAndFilter(t *testing.T) {
	m := ownerBoard(t, 200, 30, PRBoardOptions{DefaultRepo: "talkable/talkable", DefaultView: ViewReady})
	sorted := func(m prBoardModel) []string { return boardRefs(m) }
	if got := sorted(m); !slices.Equal(got, []string{"example/widgets#7", "talkable/talkable#11950"}) {
		t.Fatalf("ready, every owner = %v", got)
	}
	m, _ = send(t, m, keyMsg("O")) // talkable
	if got := sorted(m); !slices.Equal(got, []string{"talkable/talkable#11950"}) {
		t.Errorf("ready, talkable = %v", got)
	}
	mustContain(t, lineWith(t, viewOf(m), "magnum · pull requests"), "view ready 1")
	m, _ = send(t, m, keyMsg("O"), keyMsg("O")) // alice, then example
	if got := sorted(m); m.owner != "example" || !slices.Equal(got, []string{"example/widgets#7"}) {
		t.Errorf("ready, %s = %v", m.owner, got)
	}

	m, _ = send(t, m, keyMsg("/"))
	m, _ = send(t, m, typed("oauth")...)
	if got := sorted(m); !slices.Equal(got, []string{"example/widgets#7"}) {
		t.Errorf("ready, example, oauth = %v", got)
	}
	m, _ = send(t, m, keyMsg("enter"), keyMsg("v")) // all
	if got := sorted(m); !slices.Equal(got, []string{"example/widgets#7"}) {
		t.Errorf("all, example, oauth = %v", got)
	}
	m, _ = send(t, m, keyMsg("O")) // every owner
	if got := sorted(m); !slices.Equal(got, []string{"example/widgets#7", "talkable/talkable#11950"}) {
		t.Errorf("all, every owner, oauth = %v", got)
	}
	m, _ = send(t, m, keyMsg("O"), keyMsg("O")) // alice
	mustContain(t, viewOf(m), `no PR matches "oauth"`)
}

// The title names the owner scope ("all owners" only when the rows span
// several), the summary and the open count follow it and, with a single
// owner in scope, the refs drop it.
func TestPRBoardOwnerHeaderSummaryAndRefs(t *testing.T) {
	m := ownerBoard(t, 200, 30, PRBoardOptions{})
	v := viewOf(m)
	title := lineWith(t, v, "magnum · pull requests")
	mustContain(t, title, "8 open", "all owners · view all")
	mustContain(t, v, "example/widgets", "● 2 reviewed", "● 1 attention")

	m, _ = send(t, m, keyMsg("O"), keyMsg("O")) // example
	v = viewOf(m)
	mustContain(t, lineWith(t, v, "magnum · pull requests"), "1 open", "owner example · view all")
	mustContain(t, v, "● 1 reviewed", "widgets  #7")
	mustNotContain(t, v, "example/widgets", "attention", "all owners")

	// A narrow title keeps the owner, without the word.
	n, _ := send(t, m, keyMsg("O"), keyMsg("O"), keyMsg("O"), keyMsg("O"))
	n, _ = send(t, n, tea.WindowSizeMsg{Width: 80, Height: 20})
	title = strings.Split(viewOf(n), "\n")[0]
	mustContain(t, title, "example", "updated ↓")
	mustNotContain(t, title, "owner example")

	// One owner on the board: no "all owners" to tell apart.
	s, _, _ := newBoard(t, 200, 30, PRBoardOptions{})
	mustNotContain(t, viewOf(s), "all owners")
	s, _ = send(t, s, keyMsg("O"))
	mustContain(t, viewOf(s), "owner talkable")

	// The rows key carries the scope, so no cached row or layout outlives it.
	a, b := m, m
	b.owner = "talkable"
	if a.rowsKey(120) == b.rowsKey(120) {
		t.Error("the rows key must include the owner scope")
	}

	// The one-shot render spans every owner.
	out := ansi.Strip(RenderPRBoard(ownerRows(), 200, PRBoardOptions{Now: func() time.Time { return boardNow }}))
	mustContain(t, out, "all owners", "example/widgets", "alice/dotfiles")
}

// An owner whose last PR leaves the board falls back to every owner, and
// the footer says why; an owner that still has PRs stays.
func TestPRBoardOwnerVanishesOnRefresh(t *testing.T) {
	m := ownerBoard(t, 200, 30, PRBoardOptions{})
	m, _ = send(t, m, keyMsg("O"), keyMsg("O")) // example
	m, _ = send(t, m, prbDataMsg{rows: ownerRows()})
	if m.owner != "example" || len(m.view) != 1 {
		t.Fatalf("a refresh with example's PR: owner %q, %d rows", m.owner, len(m.view))
	}
	without := slices.DeleteFunc(ownerRows(), func(r PRBoardRow) bool { return r.Owner == "example" })
	m, _ = send(t, m, prbDataMsg{rows: without})
	if m.owner != "" || len(m.view) != len(without) {
		t.Fatalf("example gone: owner %q, %d rows of %d", m.owner, len(m.view), len(without))
	}
	mustContain(t, viewOf(m), "owner: all (example has no pull requests now)")
	mustNotContain(t, viewOf(m), "owner example")
	m, _ = send(t, m, keyMsg("O"), keyMsg("O"))
	if m.owner != "talkable" {
		t.Errorf("after example left, O cycles to %q second, want talkable", m.owner)
	}
}

// The board opens in DefaultOwner's scope and reports every change of it
// (O, or the owner's last PR leaving), so the next board opens the same
// way; an owner without PRs opens every owner. The one-shot render
// honours it too.
func TestPRBoardOwnerPersists(t *testing.T) {
	var heard []string
	m := ownerBoard(t, 200, 30, PRBoardOptions{DefaultOwner: "Example", OwnerChanged: func(o string) { heard = append(heard, o) }})
	if got := boardRefs(m); m.owner != "Example" || !slices.Equal(got, []string{"example/widgets#7"}) {
		t.Fatalf("opened in owner %q with %v", m.owner, got)
	}
	mustContain(t, viewOf(m), "owner Example")
	m, _ = send(t, m, keyMsg("O"), keyMsg("O"), keyMsg("O"))
	if !slices.Equal(heard, []string{"talkable", "", "alice"}) {
		t.Errorf("OwnerChanged heard %q", heard)
	}

	heard = nil
	g := ownerBoard(t, 200, 30, PRBoardOptions{DefaultOwner: "gone", OwnerChanged: func(o string) { heard = append(heard, o) }})
	if g.owner != "" || len(g.view) != len(ownerRows()) || !slices.Equal(heard, []string{""}) {
		t.Errorf("an owner without PRs: owner %q, %d rows, heard %q", g.owner, len(g.view), heard)
	}

	now := func() time.Time { return boardNow }
	out := ansi.Strip(RenderPRBoard(ownerRows(), 200, PRBoardOptions{Now: now, DefaultOwner: "example"}))
	mustContain(t, out, "owner example", "widgets")
	mustNotContain(t, out, "talkable/", "dotfiles")
	out = ansi.Strip(RenderPRBoard(ownerRows(), 200, PRBoardOptions{Now: now, DefaultOwner: "gone"}))
	mustContain(t, out, "all owners", "dotfiles", "#11950")
}
