package fixwork

// What the factory asks of magnum through magnum's requests table (DECISIONS
// "The fix loop moves into the factory"): the factory writes it through a
// narrow writer of its own and never migrates magnum's registry, so both
// daemons share these payloads here. Magnum's daemon decodes each with
// unknown fields refused, so a field added here is never removed, and an
// older build answers a kind it does not know as unknown: the factory reads
// that answer, and starts nothing on a magnum that cannot follow it.
//
// The factory's requests, by kind: a forced re-review of the head its fixer
// pushed is magnum's review request (engine.ReqReview) with FixRef, and
// LocalRound when a local round ran at that head; the abort of that forced
// round is magnum's abort request (engine.ReqAbort) with the same FixRef in
// its payload (engine.TargetPayload.FixRef), which stops only a round of
// that fix, in flight or paused mid-turn, or takes back its re-review still
// in line, and refuses any other round (an abort without FixRef is the
// operator's, which stops any round on the PR); the PR's attention is
// ReqFixAttention. The end of the round that re-reviewed the fix, or of the
// stop that ended it, is the fix.factory_round event, which names the
// FixRef.

import "time"

// ReqFixAttention raises or lifts a PR's attention for the factory
// (AttentionPayload): silently, with no toast, since the factory alerts
// you itself.
const ReqFixAttention = "fix_attention"

// FixRef is the factory's fix that a forced re-review is for: the PR work
// record (Work, the factory's id of it), its cycle and the head its fixer
// pushed. Magnum keeps it with the round's request and passes it on to the
// round's end, where the factory reads which round reviewed its push; it
// takes the place ForcedAt takes for magnum's own loop.
type FixRef struct {
	Work  int64  `json:"work"`
	Cycle int    `json:"cycle"`
	Head  string `json:"head"`
}

// LocalRound is the factory's local review round of a fix at SHA, the
// prompt and skill hashes it ran with and the file that holds its reports
// (File, a path in the factory's task or PR work directory). A re-review
// whose request carries it may build on it (T5b); magnum keeps it with the
// round's request until then and does nothing with it.
type LocalRound struct {
	SHA        string `json:"sha"`
	PromptHash string `json:"prompt_hash,omitempty"`
	SkillHash  string `json:"skill_hash,omitempty"`
	File       string `json:"file,omitempty"`
}

// AttentionPayload is a ReqFixAttention request on the PR Number of Repo
// ("owner/name"): Raise moves it from reviewed to needs attention with Why
// as its message (the factory's words: never PR text), and Lift (Raise
// false) puts it back to reviewed while its attention is still the one the
// factory raised with Why. Cause is the factory's short word for it
// ("needs_you", "capped", "failed"), which the event names. Magnum takes it
// only for a PR the factory fixes (OwnerFactory). At is when the factory
// asked.
type AttentionPayload struct {
	Repo   string    `json:"repo"`
	Number int       `json:"number"`
	Raise  bool      `json:"raise,omitempty"`
	Cause  string    `json:"cause"`
	Why    string    `json:"why"`
	At     time.Time `json:"at,omitzero"`
}
