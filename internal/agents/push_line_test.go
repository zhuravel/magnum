package agents

import "testing"

// PushLine is the line a fixer App pushes with: `magnum push` of the
// daemon's binary (else magnum on PATH) with the fixer's checkout, each
// shell-quoted as DBLockLine quotes them; no checkout leaves --checkout out.
func TestPushLineNamesTheBinaryAndTheCheckout(t *testing.T) {
	if got := PushLine("", ""); got != "magnum push" {
		t.Errorf("PushLine without a binary or a checkout = %q", got)
	}
	if got := PushLine("/opt/my magnum/bin/magnum", "/slots/talkable review3"); got != "'/opt/my magnum/bin/magnum' push --checkout '/slots/talkable review3'" {
		t.Errorf("PushLine = %q, want the binary and the checkout shell-quoted", got)
	}
}
