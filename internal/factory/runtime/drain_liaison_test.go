package runtime

import "testing"

// Only a missing live session means the liaison has none: a read of its
// session that fails cannot say it is idle, so it counts as at work.
func TestALiaisonWhoseSessionCannotBeReadCountsAsAtWork(t *testing.T) {
	g := newRig(t)
	g.talk()
	g.endLiaisonTurn()
	pr := g.liaisonRowOf()
	if err := g.st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, busy := g.r.liaisonAtWork(g.ctx, pr); !busy {
		t.Fatal("a liaison whose session could not be read counts as idle")
	}
}
