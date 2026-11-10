package runtime

// The scrub that runs before every place the factory keeps text (spec
// section 11.1): the prompt ledger, the operator's inputs, the events. The
// masking is internal/scrub's, which the review daemon uses too; what stays
// here is the rule for a long paste.

import (
	"strings"

	"github.com/zhuravel/magnum/internal/scrub"
)

// The limits of a message Scrub keeps: a longer one is a pasted block (a
// log, a stack trace, a file), which a capture of what the operator typed
// drops.
const (
	maxKeptLines = 40
	maxKeptBytes = 4000
)

// Scrub cleans text before the factory keeps it: scrub.Text (execx.Redact:
// GitHub tokens, JWTs, Authorization values, private keys; then AWS keys and
// other token shapes, the values of password=, token: and the like, also
// under a quoted key ("password": "x") (the key stays), emails and host
// names). clean is the scrubbed text; kept is false for a long pasted block
// (more than 40 lines or 4000 bytes), which a capture of what the operator
// typed drops (the ledger keeps the factory's own prompts, scrubbed, whatever
// their length).
func Scrub(text string) (clean string, kept bool) {
	clean = scrub.Text(text)
	kept = len(text) <= maxKeptBytes && strings.Count(strings.TrimRight(text, "\n"), "\n")+1 <= maxKeptLines
	return clean, kept
}
