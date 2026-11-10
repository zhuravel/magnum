package learn

import (
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/github"
	"github.com/zhuravel/magnum/internal/postreview"
)

// TestTheRetroTakesTheAppsChatCommentAsMagnums: a chat session's comment
// goes out as the watch's fixer App (`magnum comment`, the comment marker),
// one of the logins the retro takes as magnum's own, so in one of magnum's
// threads it is magnum's word (OwnAuthor), never a person's answer: a
// thread with only that reply teaches nothing and is left out, and a
// person's reply after it still makes the thread theirs. A pin: it held
// before.
func TestTheRetroTakesTheAppsChatCommentAsMagnums(t *testing.T) {
	chat := "It is right: the spec covers it.\n\n" + postreview.CommentMarker
	alone := ownThread(1, "app/a.rb", 10, shaA, 501, [2]string{"fixer-app", chat})
	answered := ownThread(2, "app/a.rb", 20, shaA, 501, [2]string{"fixer-app", chat}, [2]string{"alice", "Agreed, it stays."})
	for _, th := range []*github.Thread{&alone, &answered} {
		th.Comments[1].AuthorType = "Bot"
	}
	in := ownInput([]github.Thread{alone, answered})
	in.Own = append(in.Own, "fixer-app[bot]")
	got := OwnThreads(in)
	if ids := threadIDs(got); !slices.Equal(ids, []string{"o2"}) {
		t.Fatalf("own threads = %v, want o2 alone: the App's comment is no person's answer", ids)
	}
	var authors []string
	for _, r := range got[0].Replies {
		authors = append(authors, r.Author)
	}
	if !slices.Equal(authors, []string{OwnAuthor, "alice"}) || got[0].Fixer {
		t.Errorf("o2: authors %v, fixer %v; want magnum's word, then alice's", authors, got[0].Fixer)
	}
}
