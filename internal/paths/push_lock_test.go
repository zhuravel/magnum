package paths

import (
	"path/filepath"
	"testing"
)

// `magnum push` takes one lock per repository and branch: the repository
// told case aside, any branch name (slashes included) kept inside
// push-locks/ under State, and no two branches or repositories sharing a
// file.
func TestThePushLockIsOnePerRepositoryAndBranch(t *testing.T) {
	l := Layout{Home: "/m"}
	a := l.PushLock("Talkable/Talkable", "feature/x")
	if b := l.PushLock("talkable/talkable", "feature/x"); a != b {
		t.Errorf("the same repository in another case: %s and %s", a, b)
	}
	if filepath.Dir(a) != filepath.Join(l.State(), "push-locks") {
		t.Errorf("the lock %s is not in %s", a, filepath.Join(l.State(), "push-locks"))
	}
	for _, other := range []string{l.PushLock("talkable/talkable", "feature/y"), l.PushLock("talkable/other", "feature/x"),
		l.PushLock("talkable/talkable", "Feature/x")} {
		if other == a {
			t.Errorf("%s is shared by two repositories or branches", a)
		}
	}
	if got := filepath.Base(l.PushLock("Ex/../app", "../../etc")); filepath.Dir(l.PushLock("Ex/../app", "../../etc")) != filepath.Join(l.State(), "push-locks") || got[:10] != "ex-..-app-" {
		t.Errorf("a repository or branch with path parts leaves push-locks: %s", l.PushLock("Ex/../app", "../../etc"))
	}
}
