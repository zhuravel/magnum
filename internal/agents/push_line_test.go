package agents

import "testing"

// PushLine is the line a fixer App pushes with: `magnum push` of the
// daemon's binary (else magnum on PATH) with the fixer's checkout and the
// PR's base, each shell-quoted as DBLockLine quotes them; no checkout
// leaves --checkout out, no base --base.
func TestPushLineNamesTheBinaryTheCheckoutAndTheBase(t *testing.T) {
	if got := PushLine("", "", ""); got != "magnum push" {
		t.Errorf("PushLine without a binary, a checkout or a base = %q", got)
	}
	if got := PushLine("/opt/my magnum/bin/magnum", "/slots/talkable review3", "release/2026"); got != "'/opt/my magnum/bin/magnum' push --checkout '/slots/talkable review3' --base release/2026" {
		t.Errorf("PushLine = %q, want the binary, the checkout and the base shell-quoted", got)
	}
}
