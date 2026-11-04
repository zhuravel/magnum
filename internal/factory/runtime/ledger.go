package runtime

// The prompt ledger (registry.LedgerEntry): every prompt the factory sends
// to a pane is written, scrubbed and with the hash of its exact text,
// before it leaves, and settled yes or no once its fate is known. magnum
// sends a prompt before it records that it sent it, so a daemon stopped in
// between leaves the entry unknown; Reconcile (deliver.go) settles it from
// the agent's transcript, and nothing is sent to the role meanwhile.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
)

// PromptHash is the ledger's hash of a prompt: the sha256 of its text with
// the line ends made \n and the surrounding space trimmed, as a transcript
// records it.
func PromptHash(text string) string {
	sum := sha256.Sum256([]byte(normalPrompt(text)))
	return hex.EncodeToString(sum[:])
}

func normalPrompt(text string) string {
	return strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
}

// ledgerText is what the ledger keeps of a prompt: its scrubbed text. The
// factory's own prompts are kept whatever their length (Scrub's kept is for
// what the operator types).
func ledgerText(text string) string {
	clean, _ := Scrub(text)
	if strings.TrimSpace(clean) == "" {
		return "(empty)"
	}
	return clean
}

// unknownOf is the first of task t's entries for role whose delivery is
// still unknown.
func unknownOf(entries []registry.LedgerEntry, role string) (registry.LedgerEntry, bool) {
	for _, e := range entries {
		if e.Role == role && e.Delivered == registry.DeliveredUnknown {
			return e, true
		}
	}
	return registry.LedgerEntry{}, false
}

// stagePrompts counts the prompts that went, or may have gone, to role in
// the stage of the task's attempts named stage: the entries not settled as
// never sent.
func (r *Runtime) stagePrompts(ctx context.Context, taskID int64, stage, role string, entries []registry.LedgerEntry) (int, error) {
	as, err := r.reg.Attempts(ctx, taskID)
	if err != nil {
		return 0, err
	}
	in := map[string]bool{}
	for _, a := range as {
		if a.Stage == stage {
			in[a.ID] = true
		}
	}
	n := 0
	for _, e := range entries {
		if e.Role == role && in[e.AttemptID] && e.Delivered != registry.DeliveredNo {
			n++
		}
	}
	return n, nil
}

// firstPrompt is the attempt's first prompt to the role of st, as it was
// sent: the one Send noted, else (after a restart) the text the agents
// store kept with the run of the role's first ledger entry in the attempt.
func (r *Runtime) firstPrompt(ctx context.Context, st *roleState) (string, error) {
	r.mu.Lock()
	first := st.first
	r.mu.Unlock()
	if first != "" {
		return first, nil
	}
	entries, err := r.reg.LedgerOf(ctx, st.taskID)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.AttemptID != st.key.attempt || e.Role != st.key.role || e.Delivered == registry.DeliveredNo {
			continue
		}
		if run, err := r.st.RunByID(ctx, e.RunID); err == nil && run.PromptText != "" {
			return run.PromptText, nil
		}
		return e.Text, nil
	}
	return "", fmt.Errorf("%s of %s has had no prompt yet", st.key.role, st.key.attempt)
}

// settle records the verdict on a ledger entry; a failure is logged (the
// entry stays unknown, which only holds the role's next prompt back).
func (r *Runtime) settle(ctx context.Context, e registry.LedgerEntry, verdict string) {
	if verdict == registry.DeliveredUnknown {
		return
	}
	if err := r.reg.MarkDelivered(context.WithoutCancel(ctx), e.ID, verdict); err != nil {
		r.log.Warn("settle a prompt's delivery", "entry", e.ID, "verdict", verdict, "err", err)
	}
}

// inFlight reports whether run is pending, submitted or working.
func inFlight(run store.Run) bool {
	switch run.State {
	case store.RunPending, store.RunSubmitted, store.RunWorking:
		return true
	}
	return false
}
