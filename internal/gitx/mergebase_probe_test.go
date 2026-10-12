package gitx

import (
	"context"
	"testing"
)

// A restart check asks for the merge base of a head the checkout has not
// fetched yet; git answers exit 128 ("Not a valid object name"). Every
// caller takes any error as "no base known" (a force push, an unknown base,
// no push), so the exit is an answer, not a fault: the command runs as a
// probe and execx logs it at Debug (14 warnings in a day for the restart
// check alone).
func TestMergeBaseRunsAsAProbeSoAnUnfetchedHeadIsNoWarning(t *testing.T) {
	c, f := newFake(exitRule(128, "fatal: Not a valid object name", "git", "-C", slot, "merge-base"))
	if _, err := c.MergeBase(context.Background(), slot, sha1, "b"); err == nil {
		t.Fatal("an unfetched head must still be an error")
	}
	if len(f.Calls) != 1 || !f.Calls[0].Probe {
		t.Fatalf("merge-base calls = %+v, want one probe", f.Calls)
	}

	c, f = newFake(outRule(sha1+"\n", "git", "-C", slot, "merge-base"))
	if _, err := c.MergeBase(context.Background(), slot, "origin/master", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if len(f.Calls) != 1 || !f.Calls[0].Probe {
		t.Fatalf("merge-base calls = %+v, want one probe", f.Calls)
	}
}
