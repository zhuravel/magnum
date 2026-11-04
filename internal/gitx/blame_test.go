package gitx

import (
	"context"
	"testing"
	"time"

	"github.com/zhuravel/magnum/internal/execx"
)

// BlameLine names the commit that last wrote a line of a file at a
// revision, and when it was authored: the age of a guard.
func TestBlameLineSaysWhenALineWasWritten(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.write(fx.clone, "app/credit.rb", "class Credit\n  return 0 if refunded?\nend\n")
	fx.git(fx.clone, "add", "--", "app/credit.rb")
	old := time.Date(2024, 3, 5, 10, 0, 0, 0, time.UTC)
	if _, err := fx.r.Run(ctx, execx.Cmd{Name: "git", Args: []string{"-C", fx.clone, "commit", "--quiet", "-m", "the refund guard"},
		Env: map[string]string{"GIT_AUTHOR_DATE": old.Format(time.RFC3339), "GIT_COMMITTER_DATE": old.Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	guard := fx.git(fx.clone, "rev-parse", "HEAD")
	fresh := fx.commit(fx.clone, "app/credit.rb", "class Credit\n  return 0 if refunded?\n  def points; 10; end\nend\n", "points")

	sha, at, err := fx.c.BlameLine(ctx, fx.clone, fresh, "app/credit.rb", 2)
	if err != nil {
		t.Fatal(err)
	}
	if sha != guard || !at.Equal(old) {
		t.Fatalf("line 2 blamed on %s at %s, want %s at %s", sha, at, guard, old)
	}
	if sha, _, err := fx.c.BlameLine(ctx, fx.clone, fresh, "app/credit.rb", 3); err != nil || sha != fresh {
		t.Fatalf("line 3 blamed on %s (%v), want %s", sha, err, fresh)
	}
	for _, line := range []int{0, 9} {
		if _, _, err := fx.c.BlameLine(ctx, fx.clone, fresh, "app/credit.rb", line); err == nil {
			t.Errorf("line %d of a 4-line file blamed", line)
		}
	}
}
