package fixwork

// The diff of a merge of the parent that magnum holds for your y (the
// engine's fix_held_merge.go holds it, and pushes it after the y):
// held-merge.diff beside the PR's reports, which the screens' e opens in a
// pager, read where no setting of the clone applies and escaped so that no
// line hides behind a control or format character.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zhuravel/magnum/internal/fsx"
	"github.com/zhuravel/magnum/internal/textx"
)

// HeldMergeFile is the file in the PR's report directory
// (paths.Layout.PRReviews) that shows what a held merge does to the
// workflow files: the merge, how the fixer resolved their conflicts, then
// what its push changes in them. The screens' e opens it in the pager.
const HeldMergeFile = "held-merge.diff"

// HeldDiffGit is the git HeldDiff reads a held merge's diff with beyond
// IsolatedGit (*gitx.Client has it): how the merge resolved its conflicts
// in some paths (RemergeDiff) and what one commit changes against another
// there (PathsDiff).
type HeldDiffGit interface {
	IsolatedGit
	RemergeDiff(ctx context.Context, dir, merge string, paths ...string) (string, error)
	PathsDiff(ctx context.Context, dir, a, b string, paths ...string) (string, error)
}

// heldNoConflict is held-merge.diff's line, in place of the remerge diff,
// for a merge that resolved no conflict in a workflow file.
const heldNoConflict = "No conflict in a workflow file was resolved: the workflow changes come unchanged from %s and the PR.\n"

// HeldDiff is held-merge.diff of h in dir's objects: a first line that
// names the merge, how it resolved the conflicts in the workflow files
// (`git show --remerge-diff`), or heldNoConflict's one line when it
// resolved none, then everything its push changes in them against its
// head (`git diff`), every control character but tab and newline escaped
// (EscapeControls). Both read in a repository of the daemon's own, after
// the workflow files' objects at the head, the tip and the merge are
// hashed again (WorkflowRead): no merge driver or attribute of the
// clone's runs, and a forged object is gitx.ErrObjectMismatch.
func HeldDiff(ctx context.Context, hg HeldDiffGit, dir string, h HeldMerge) ([]byte, error) {
	var remerge, plain string
	_, err := WorkflowRead(ctx, hg, dir, []string{h.Head, h.Tip, h.Merge}, func(iso string) (struct{}, error) {
		var err error
		if remerge, err = hg.RemergeDiff(ctx, iso, h.Merge, WorkflowsPath); err != nil {
			return struct{}{}, err
		}
		plain, err = hg.PathsDiff(ctx, iso, h.Head, h.Merge, WorkflowsPath)
		return struct{}{}, err
	})
	if err != nil {
		return nil, err
	}
	switch {
	case strings.TrimSpace(remerge) == "":
		remerge = fmt.Sprintf(heldNoConflict, h.Base)
	case !strings.HasSuffix(remerge, "\n"):
		remerge += "\n"
	}
	head := fmt.Sprintf("Held merge %s: %s's merge of %s at %s into the PR's head %s.\n", h.Merge, h.Role, h.Base, textx.ShortSHA(h.Tip),
		textx.ShortSHA(h.Head))
	return []byte(EscapeControls(head + remerge + plain)), nil
}

// WriteHeldDiff writes diff at path, in dir, made private when missing.
func WriteHeldDiff(dir, path string, diff []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(path, diff, 0o600)
}

// EscapeControls is s with every control character but tab and newline
// (C0, DEL and C1) and every bidi or invisible format character
// (hiddenFormat) written as \xNN or \uNNNN: workflow content an agent
// wrote reaches a pager that runs with LESS=R, where an escape sequence or
// a carriage return could hide a line, and a "Trojan Source" override could
// show a line in another order than git runs it. A byte that is no UTF-8
// stays.
func EscapeControls(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			b.WriteByte(s[i])
		case r == '\t' || r == '\n':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, "\\x%02x", r)
		case r >= 0x80 && r <= 0x9f, hiddenFormat(r):
			fmt.Fprintf(&b, "\\u%04x", r)
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// hiddenFormat reports whether r is a character a line can hide behind or
// be reordered by: every format character (Unicode's Cf: the embeddings
// and overrides U+202A–202E, the isolates U+2066–2069, the marks U+200E,
// U+200F and U+061C, the zero-width U+200B–200D, the byte order mark
// U+FEFF and their kin) and the line and paragraph separators U+2028,
// U+2029, which a terminal can break a line at.
func hiddenFormat(r rune) bool {
	return unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}
