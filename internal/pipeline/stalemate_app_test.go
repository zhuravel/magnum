package pipeline

import (
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/postreview"
)

// A fixer that acts as a GitHub App of its own answers in magnum's threads
// as the App, not as the reviewer: its reply is someone else's, so after
// two rebuttals it stops the thread, as the author's answer does. A pin:
// the rule tells the reviewer's login from any other, the App's included.
func TestAStalemateCountsTheFixerAppsAnswer(t *testing.T) {
	reply := func(login, typ, kind string) agents.ThreadReply {
		return agents.ThreadReply{Author: login, Own: OwnHistory(login, typ, "talkable[bot]", true, nil), Kind: kind}
	}
	replies := []agents.ThreadReply{
		reply("alice", "User", ""), reply("talkable", "Bot", postreview.ReplyRebuttal),
		reply("fixer-app", "Bot", ""), reply("talkable", "Bot", postreview.ReplyRebuttal),
		reply("fixer-app", "Bot", ""),
	}
	if replies[2].Own || replies[4].Own {
		t.Fatal("the fixer App's reply reads as the reviewer's own")
	}
	if n, stop := rebuttals(replies); n != 2 || !stop {
		t.Errorf("rebuttals = %d, stop %v; want 2 and the thread stopped", n, stop)
	}
}
