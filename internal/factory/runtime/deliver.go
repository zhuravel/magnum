package runtime

// Sending a prompt and settling its delivery. Send writes the ledger entry
// before Submit and settles it from what Submit says: refused before the
// prompt left (a human in the pane, no session) is no, acked is yes, and a
// prompt herdr may have typed without an ack (stalled, timed out, cut short)
// stays unknown. Reconcile settles an unknown entry from the agent's
// transcript: the prompt's hash in a turn after it was sent is yes; no is
// only proven by two reads RedeliverAfter apart that find no such turn in a
// transcript written past the prompt, with the run never submitted and the
// session never prompted since. Until then nothing more goes to the role.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/store"
)

// deliverySlack is how much earlier than its ledger entry a transcript may
// stamp a prompt (the CLI's clock and the daemon's are one machine's).
const deliverySlack = 5 * time.Second

// Send sends text to role's agent in attempt a of task t (role as it runs:
// RunsAs) as a new run, and returns the run's id. The ledger entry, with
// the text scrubbed and its hash, is written before the prompt leaves. It
// refuses without sending: a pause of the role's kind (a *WaitError), an
// entry of the role whose delivery is unknown (ErrDeliveryUnknown), the
// operator typing in the pane (ErrOperatorInPane), and a loop without
// progress whose steps ran out (ErrNoProgress, with EventNoProgress at the
// next Observe). Past MaxStagePrompts prompts in the stage, a context over
// MaxContext or the same failure SameFailures times, it first compacts the
// session, the next time starts a fresh session from the attempt's first
// prompt (returning that run), then refuses.
func (r *Runtime) Send(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("send to %s: an empty prompt", role.Name)
	}
	if a.TaskID != t.ID {
		return "", fmt.Errorf("send to %s: attempt %s is not %s's", role.Name, a.ID, taskKey(t))
	}
	role, err := r.RunsAs(t, role)
	if err != nil {
		return "", err
	}
	if w := r.paused(role.AgentKind()); w != nil {
		return "", w
	}
	entries, err := r.reg.LedgerOf(ctx, t.ID)
	if err != nil {
		return "", fmt.Errorf("send to %s: %w", role.Name, err)
	}
	if e, ok := unknownOf(entries, role.Name); ok {
		return "", fmt.Errorf("send to %s: prompt %d: %w", role.Name, e.ID, ErrDeliveryUnknown)
	}
	pr, err := r.owner(ctx, t, a.ID)
	if err != nil {
		return "", err
	}
	st, err := r.stateFor(ctx, a.ID, role.Name)
	if err != nil {
		return "", fmt.Errorf("send to %s: %w", role.Name, err)
	}
	switch step, err := r.loopStep(ctx, t, a, role, pr, entries); {
	case err != nil:
		return "", err
	case step == loopFresh:
		return r.fromFiles(ctx, t, a, st, "a loop without progress")
	}
	kind := store.RunNudge
	if !hasPrompt(entries, a.ID, role.Name) {
		kind = store.RunInitial
	}
	id, err := r.send(ctx, t, a, role, pr, text, kind)
	if id != "" && kind == store.RunInitial {
		r.mu.Lock()
		st.first = text
		r.mu.Unlock()
	}
	return id, err
}

// hasPrompt reports whether the ledger holds a prompt to role in attempt
// that may have reached it.
func hasPrompt(entries []registry.LedgerEntry, attempt, role string) bool {
	for _, e := range entries {
		if e.AttemptID == attempt && e.Role == role && e.Delivered != registry.DeliveredNo {
			return true
		}
	}
	return false
}

// send is Send past its refusals: a new run of kind, its ledger entry, the
// prompt (agents.Manager.Submit) and the entry settled from what Submit
// says. The run's id comes back whenever the prompt may have left.
func (r *Runtime) send(ctx context.Context, t registry.Task, a registry.Attempt, role config.Role, pr store.PR, text, kind string) (string, error) {
	run, err := r.mgr.NewRun(ctx, pr, agents.Role(role.Name), kind, max(a.N, 1))
	if err != nil {
		return "", fmt.Errorf("send to %s: %w", role.Name, err)
	}
	e, err := r.reg.AppendLedger(ctx, registry.LedgerEntry{TaskID: t.ID, AttemptID: a.ID, Role: role.Name, RunID: run.ID,
		Hash: PromptHash(text), Text: ledgerText(text)})
	if err != nil {
		r.abandon(ctx, run.ID, "the ledger did not take the prompt")
		return "", fmt.Errorf("send to %s: %w", role.Name, err)
	}
	serr := r.mgr.Submit(ctx, run, text)
	verdict := registry.DeliveredUnknown
	cur, rerr := r.st.RunByID(context.WithoutCancel(ctx), run.ID)
	switch {
	case serr == nil:
		verdict = registry.DeliveredYes
	case rerr != nil:
	case cur.State == store.RunPending: // refused before the prompt left
		verdict = registry.DeliveredNo
		r.abandon(ctx, run.ID, serr.Error())
	case cur.State == store.RunFailed: // herdr refused it, or the agent was gone
		verdict = registry.DeliveredNo
	case cur.State == store.RunWorking, cur.State == store.RunSubmitted && cur.Error == nil:
		verdict = registry.DeliveredYes // acked: working, or blocked once it was in
	}
	r.settle(ctx, e, verdict)
	if verdict != registry.DeliveredNo {
		r.sent(a.ID, role.Name)
	}
	switch {
	case serr == nil:
		return run.ID, nil
	case errors.Is(serr, agents.ErrHumanActive):
		return "", fmt.Errorf("send to %s: %w: %w", role.Name, ErrOperatorInPane, serr)
	case verdict == registry.DeliveredNo:
		return "", fmt.Errorf("send to %s: %w", role.Name, serr)
	}
	return run.ID, fmt.Errorf("send to %s: %w", role.Name, serr)
}

// sent notes a new run of a role: the watchdog reads its progress afresh.
func (r *Runtime) sent(attempt, role string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if st := r.roles[roleKey{attempt, role}]; st != nil {
		st.sig, st.since = "", time.Time{}
	}
}

// abandon ends a run nobody waits for any more.
func (r *Runtime) abandon(ctx context.Context, id, why string) {
	err := r.st.TransitionRun(context.WithoutCancel(ctx), id,
		[]string{store.RunPending, store.RunSubmitted, store.RunWorking, store.RunEnded}, store.RunAbandoned,
		func(u *store.RunUpdate) {
			u.Set("error", execx.Redact(why))
			u.Set("ended_at", r.now())
		})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		r.log.Warn("abandon a run", "run", id, "err", err)
	}
}

// Reconcile settles the delivery of ledger entry e from its agent's
// transcript and returns the verdict (registry.DeliveredYes, DeliveredNo
// or DeliveredUnknown). A turn holding the prompt's hash, stamped after it
// was sent, is yes: the entry is settled and a run still pending is marked
// submitted, so it is observed and never sent again. No needs proof: a
// read that finds no such turn, and another at least RedeliverAfter later
// that finds none either, in a transcript written past the prompt, with
// the run never submitted and the session not prompted since. Every other
// case (no transcript, a run that went further) stays unknown.
func (r *Runtime) Reconcile(ctx context.Context, e registry.LedgerEntry) (string, error) {
	if e.Delivered == registry.DeliveredYes || e.Delivered == registry.DeliveredNo {
		return e.Delivered, nil
	}
	unknown := func() (string, error) {
		r.mu.Lock()
		delete(r.misses, e.ID)
		r.mu.Unlock()
		return registry.DeliveredUnknown, nil
	}
	run, err := r.st.RunByID(ctx, e.RunID)
	if errors.Is(err, store.ErrNotFound) {
		return unknown()
	}
	if err != nil {
		return registry.DeliveredUnknown, fmt.Errorf("reconcile prompt %d: %w", e.ID, err)
	}
	s, ok := r.sessionOf(ctx, run)
	if !ok {
		return unknown()
	}
	path, err := r.mgr.TranscriptPath(ctx, s)
	if err != nil {
		return unknown()
	}
	at, found, last, err := findPrompt(store.Deref(s.AgentKind), path, e.Sent.Add(-deliverySlack), e.Hash)
	if err != nil {
		return unknown()
	}
	if found {
		r.settle(ctx, e, registry.DeliveredYes)
		r.submitted(ctx, run, s, at)
		r.mu.Lock()
		delete(r.misses, e.ID)
		r.mu.Unlock()
		return registry.DeliveredYes, nil
	}
	proof := run.State == store.RunPending && run.SubmittedAt == nil && last.After(e.Sent) &&
		(s.LastPromptAt == nil || !s.LastPromptAt.After(e.Sent))
	if !proof {
		return unknown()
	}
	now := r.now()
	r.mu.Lock()
	first, seen := r.misses[e.ID]
	if !seen {
		r.misses[e.ID] = now
	}
	r.mu.Unlock()
	if !seen || now.Sub(first) < RedeliverAfter {
		return registry.DeliveredUnknown, nil
	}
	r.settle(ctx, e, registry.DeliveredNo)
	r.abandon(ctx, run.ID, "the prompt never arrived (its transcript, read twice, has no such turn)")
	r.mu.Lock()
	delete(r.misses, e.ID)
	r.mu.Unlock()
	return registry.DeliveredNo, nil
}

// sessionOf is the session run went to: its own, else the role's newest
// session of the owner row.
func (r *Runtime) sessionOf(ctx context.Context, run store.Run) (store.Session, bool) {
	if run.SessionID != nil {
		if s, err := r.st.SessionByID(ctx, *run.SessionID); err == nil {
			return s, true
		}
	}
	ss, err := r.st.SessionsByPR(ctx, run.PRID)
	if err != nil {
		return store.Session{}, false
	}
	for _, s := range slices.Backward(ss) {
		if s.Role == run.Role {
			return s, true
		}
	}
	return store.Session{}, false
}

// submitted marks a run whose prompt the transcript shows arrived at at as
// submitted to session s, when it is still pending.
func (r *Runtime) submitted(ctx context.Context, run store.Run, s store.Session, at time.Time) {
	if run.State != store.RunPending {
		return
	}
	err := r.st.TransitionRun(ctx, run.ID, []string{store.RunPending}, store.RunSubmitted, func(u *store.RunUpdate) {
		u.Set("session_id", s.ID)
		u.Set("submitted_at", at)
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		r.log.Warn("mark a delivered prompt's run submitted", "run", run.ID, "err", err)
	}
}

// findPrompt looks for a turn with the prompt hash hash in the transcript
// at path of a session of kind, stamped at or after since: its time, and
// the latest stamp of any line of the transcript.
func findPrompt(kind, path string, since time.Time, hash string) (at time.Time, found bool, last time.Time, err error) {
	var turns []agents.UserTurn
	switch kind {
	case agents.KindClaude:
		typed, err := agents.ClaudeUserTurns(path, since)
		if err != nil {
			return at, false, last, err
		}
		queued, err := agents.ClaudeQueuedTurns(path, since)
		if err != nil {
			return at, false, last, err
		}
		turns = append(turns, typed...)
		turns = append(turns, queued...)
	case agents.KindCodex:
		if turns, err = agents.CodexUserTurns(path, since); err != nil {
			return at, false, last, err
		}
	default:
		return at, false, last, fmt.Errorf("a %q session keeps no transcript the factory reads", kind)
	}
	for _, t := range turns {
		if PromptHash(t.Text) == hash {
			return t.At, true, last, nil
		}
	}
	last, err = lastStamp(path)
	return at, false, last, err
}

// stamped is the part of a transcript line every reader here needs.
type stamped struct {
	Timestamp time.Time `json:"timestamp"`
}

// lastStamp is the latest timestamp of the lines of the transcript at path
// (the zero time when none has one): how far the CLI has written it.
func lastStamp(path string) (time.Time, error) {
	var last time.Time
	err := eachLine(path, func(line []byte) {
		var s stamped
		if json.Unmarshal(line, &s) == nil && s.Timestamp.After(last) {
			last = s.Timestamp
		}
	})
	return last, err
}

// eachLine calls fn with each line of the file at path (a half-written last
// line too).
func eachLine(path string, fn func(line []byte)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	return sc.Err()
}

// tokensSince counts the tokens session s used since since, by model, from
// its transcript at path: a Claude transcript's assistant messages (each
// message once: its input, output and cache tokens), a Codex rollout's
// token counts (the growth of its total, under the model its turn context
// names). nil when nothing is known.
func tokensSince(s store.Session, path string, since time.Time) map[string]int64 {
	out := map[string]int64{}
	switch store.Deref(s.AgentKind) {
	case agents.KindClaude:
		seen := map[string]bool{}
		_ = eachLine(path, func(line []byte) {
			if !strings.Contains(string(line), `"usage"`) {
				return
			}
			var e struct {
				Type      string    `json:"type"`
				Timestamp time.Time `json:"timestamp"`
				Message   struct {
					ID    string `json:"id"`
					Model string `json:"model"`
					Usage struct {
						Input       int64 `json:"input_tokens"`
						Output      int64 `json:"output_tokens"`
						CacheCreate int64 `json:"cache_creation_input_tokens"`
						CacheRead   int64 `json:"cache_read_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.Timestamp.Before(since) || seen[e.Message.ID] {
				return
			}
			if e.Message.ID != "" {
				seen[e.Message.ID] = true
			}
			u := e.Message.Usage
			if n := u.Input + u.Output + u.CacheCreate + u.CacheRead; n > 0 {
				out[cmpModel(e.Message.Model)] += n
			}
		})
	case agents.KindCodex:
		model, base, total := "", int64(-1), int64(0)
		_ = eachLine(path, func(line []byte) {
			var e struct {
				Timestamp time.Time `json:"timestamp"`
				Payload   struct {
					Type  string `json:"type"`
					Model string `json:"model"`
					Info  *struct {
						Total struct {
							Tokens int64 `json:"total_tokens"`
						} `json:"total_token_usage"`
					} `json:"info"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &e) != nil {
				return
			}
			if e.Payload.Model != "" {
				model = e.Payload.Model
			}
			if e.Payload.Type != "token_count" || e.Payload.Info == nil {
				return
			}
			if e.Timestamp.Before(since) {
				base = e.Payload.Info.Total.Tokens
				return
			}
			if base < 0 {
				base = 0
			}
			total = e.Payload.Info.Total.Tokens
		})
		if n := total - max(base, 0); n > 0 {
			out[cmpModel(model)] = n
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cmpModel(m string) string {
	if m == "" {
		return agents.UnknownModel
	}
	return m
}
