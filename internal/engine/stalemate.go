package engine

// Stalemates (DECISIONS "Replies on magnum's threads get an answer without
// a push"): after two of magnum's rebuttals in one thread an author's agent
// and magnum's judge could argue forever, a round each. A thread where
// someone answered again after the second rebuttal is marked stop in the
// threads file (pipeline.StopThread): the judge replies there no more,
// post-review refuses to, and the board flags the PR for the operator until
// they act on it (a review they ask for, a verdict, a mute); a later answer
// in that thread flags it again.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// KVPRStalemate holds the threads of a PR where magnum stopped arguing and
// the operator has not acted since (Stalemate as JSON); the board flags the
// PR while it is set.
func KVPRStalemate(prID int64) string { return fmt.Sprintf("pr.%d.stalemate", prID) }

// kvPRStalemateSeen holds the stopped threads the operator acted on (thread
// id → the answer they saw): a round that finds no newer answer there does
// not flag the PR again.
func kvPRStalemateSeen(prID int64) string { return fmt.Sprintf("pr.%d.stalemate_seen", prID) }

// Stalemate is the threads where magnum stopped arguing (KVPRStalemate).
type Stalemate struct {
	Threads []StalemateThread `json:"threads"`
}

// StalemateThread is one of them.
type StalemateThread struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	LastReply int64  `json:"last_reply"`
}

// ParseStalemate reads a KVPRStalemate value; ok is false for "" or a value
// it cannot read or that names no thread.
func ParseStalemate(s string) (Stalemate, bool) {
	var st Stalemate
	if s == "" || json.Unmarshal([]byte(s), &st) != nil || len(st.Threads) == 0 {
		return Stalemate{}, false
	}
	return st, true
}

// noteStalemates records, after a round that read the threads
// (RoundResult.ThreadsRead), the stopped threads the operator has not seen
// (kvPRStalemateSeen) as the PR's stalemate, with an event when it grows;
// none left clears it.
func (e *Engine) noteStalemates(ctx context.Context, repo store.Repo, pr store.PR, res pipeline.RoundResult) {
	if !res.ThreadsRead {
		return
	}
	seen := map[string]int64{}
	if v, ok := e.getKV(ctx, kvPRStalemateSeen(pr.ID)); ok {
		_ = json.Unmarshal([]byte(v), &seen)
	}
	var cur Stalemate
	for _, s := range res.Stops {
		if seen[s.ID] != s.LastReply {
			cur.Threads = append(cur.Threads, StalemateThread{ID: s.ID, URL: s.URL, LastReply: s.LastReply})
		}
	}
	prevRaw, _ := e.getKV(ctx, KVPRStalemate(pr.ID))
	prev, _ := ParseStalemate(prevRaw)
	if len(cur.Threads) == 0 {
		e.delKV(ctx, KVPRStalemate(pr.ID))
		return
	}
	b, err := json.Marshal(cur)
	if err != nil || string(b) == prevRaw {
		return
	}
	e.setKV(ctx, KVPRStalemate(pr.ID), string(b))
	var added []string
	for _, t := range cur.Threads {
		if !slices.ContainsFunc(prev.Threads, func(p StalemateThread) bool { return p.ID == t.ID && p.LastReply == t.LastReply }) {
			added = append(added, t.URL)
		}
	}
	if len(added) > 0 {
		e.event(ctx, "warn", prSubject(repo, pr.Number), "pr.stalemate",
			fmt.Sprintf("magnum stopped arguing in %s after two rebuttals and an answer; the operator decides", stalemateWord(len(cur.Threads))),
			map[string]any{"threads": added})
	}
}

// seeStalemates marks the PR's flagged threads seen by the operator (they
// acted on the PR) and clears the flag.
func (e *Engine) seeStalemates(ctx context.Context, prID int64) {
	v, ok := e.getKV(ctx, KVPRStalemate(prID))
	st, parsed := ParseStalemate(v)
	if !ok || !parsed {
		return
	}
	seen := map[string]int64{}
	if v, ok := e.getKV(ctx, kvPRStalemateSeen(prID)); ok {
		_ = json.Unmarshal([]byte(v), &seen)
	}
	for _, t := range st.Threads {
		seen[t.ID] = t.LastReply
	}
	if b, err := json.Marshal(seen); err == nil {
		e.setKV(ctx, kvPRStalemateSeen(prID), string(b))
	}
	e.delKV(ctx, KVPRStalemate(prID))
}
