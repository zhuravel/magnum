package engine

import "testing"

// The push record of a merge of the parent, as the build before the fix
// loop's move wrote it, names its job's mode "merge"
// (internal/fixwork/testdata/records/fix_push.json). fixwork's round trip
// of that record spells the mode out, since FixModeMerge still lives here,
// so this pins the constant: a changed FixModeMerge would write push
// records that the rows before it no longer match.
func TestFixModeMergeIsTheModeThePushRecordsBeforeTheMoveHold(t *testing.T) {
	if FixModeMerge != "merge" {
		t.Errorf("FixModeMerge = %q, want %q, as the push records before the move hold it", FixModeMerge, "merge")
	}
}
