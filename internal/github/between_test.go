package github

import (
	"context"
	"testing"
)

// A context carrying WithBetweenCalls runs its function before every gh
// command the client makes with it, between the pages of one radar too, on
// the caller's goroutine: the daemon's poll answers the requests a CLI
// queued meanwhile. A context without it runs nothing.
func TestBetweenCallsRunsBeforeEachGitHubCommand(t *testing.T) {
	f := radarFake(t)
	c := &Client{Run: f, repoPage: 8, prPage: 3}
	var at []int // gh calls made when the function ran
	ctx := WithBetweenCalls(context.Background(), func() { at = append(at, len(f.Calls)) })
	if _, _, err := c.Radar(ctx, "zhuravel"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 3 || len(at) != 3 || at[0] != 0 || at[1] != 1 || at[2] != 2 {
		t.Fatalf("ran before calls %v of %d, want before each of the 3 (2 repository pages + 1 PR page)", at, len(f.Calls))
	}
	at = nil
	if _, _, err := c.Radar(context.Background(), "zhuravel"); err != nil || len(at) != 0 {
		t.Fatalf("a plain context ran it %d times, %v", len(at), err)
	}
	BetweenCalls(context.Background()) // nothing to run: no panic
}
