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
	"errors"
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
	r.settleTo(ctx, e, verdict)
}

// settleTo is settle, and returns the verdict that stands: verdict, or,
// when the entry was settled meanwhile (ErrConflict: let go, say), the
// stored one; unknown when the registry failed.
func (r *Runtime) settleTo(ctx context.Context, e registry.LedgerEntry, verdict string) string {
	if verdict == registry.DeliveredUnknown {
		return verdict
	}
	ctx = context.WithoutCancel(ctx)
	err := r.reg.MarkDelivered(ctx, e.ID, verdict)
	if err == nil {
		return verdict
	}
	if errors.Is(err, registry.ErrConflict) {
		if v, verr := r.verdict(ctx, e); verr == nil {
			return v
		}
	}
	r.log.Warn("settle a prompt's delivery", "entry", e.ID, "verdict", verdict, "err", err)
	return registry.DeliveredUnknown
}

// verdict is ledger entry e's stored delivery.
func (r *Runtime) verdict(ctx context.Context, e registry.LedgerEntry) (string, error) {
	entries, err := r.reg.LedgerOf(ctx, e.TaskID)
	if err != nil {
		return "", err
	}
	for _, x := range entries {
		if x.ID == e.ID {
			return x.Delivered, nil
		}
	}
	return "", fmt.Errorf("ledger entry %d: %w", e.ID, registry.ErrNotFound)
}

// inFlight reports whether run is pending, submitted or working.
func inFlight(run store.Run) bool {
	switch run.State {
	case store.RunPending, store.RunSubmitted, store.RunWorking:
		return true
	}
	return false
}
