package engine

// A round that fails maxAttempts times on one head parks its PR in
// needs_attention (attention.KindFailed). When the failure was magnum's own
// (an unknown role, a launch line that no longer started the judge) the fix
// is a new build, config or prompts, and the PRs stayed parked after the
// daemon restarted on it until the operator forced each one. The settle
// records with the attention what the daemon ran (runFingerprint), and the
// next start puts back in line every PR a failed round parked under another
// run (retryParked; DECISIONS "A restart on a new build, config or prompts
// retries the PRs a failed round parked").

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/notify"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/textx"
)

// kvPRAttentionRun holds the runFingerprint (JSON) of the daemon that parked
// a PR in needs_attention, written with KVPRAttention.
func kvPRAttentionRun(prID int64) string { return fmt.Sprintf("pr.%d.attention_run", prID) }

// kindRetriedAfterRestart counts the start's toast in a batch summary.
var kindRetriedAfterRestart = notify.Kind{One: "retry after the restart", Many: "retries after the restart"}

// runFingerprint identifies what a daemon runs: its build (buildID), its
// config (configSum) and the prompts and skills it loaded at its start
// (config.PromptSnapshot.Sum; "" without a snapshot).
type runFingerprint struct {
	Build   string `json:"build"`
	Config  string `json:"config"`
	Prompts string `json:"prompts,omitempty"`
}

// runFingerprint is this daemon's, taken at its first use (the start's
// retryParked, before any round): the config and the prompt snapshot never
// change under a running daemon, and Run sets the build before its start.
func (e *Engine) runFingerprint() runFingerprint {
	e.runOnce.Do(func() {
		e.run = runFingerprint{Build: buildID(e.build), Config: configSum(e.cfg), Prompts: e.cfg.PromptSnapshot().Sum()}
	})
	return e.run
}

// sum is f in 12 hex digits (the attention toast's dedupe key names it).
func (f runFingerprint) sum() string {
	s := sha256.Sum256([]byte(f.Build + "\x00" + f.Config + "\x00" + f.Prompts))
	return hex.EncodeToString(s[:])[:12]
}

// changes names the parts of cur that differ from f, in order: build,
// config, prompts.
func (f runFingerprint) changes(cur runFingerprint) []string {
	var out []string
	if f.Build != cur.Build {
		out = append(out, "build")
	}
	if f.Config != cur.Config {
		out = append(out, "config")
	}
	if f.Prompts != cur.Prompts {
		out = append(out, "prompts")
	}
	return out
}

// differs says what changes names: "build differs", "config and prompts
// differ"; "" when nothing does.
func differs(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		if parts[0] == "prompts" {
			return "prompts differ"
		}
		return parts[0] + " differs"
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1] + " differ"
}

// buildID identifies b: its revision (its version without one) and the
// binary's mtime, so a rebuild of one commit (a tree with changes) is
// another build.
func buildID(b Build) string {
	id := cmp.Or(b.Revision, b.Version)
	if !b.ModTime.IsZero() {
		id += "@" + b.ModTime.UTC().Format(time.RFC3339Nano)
	}
	return id
}

// configSum is the SHA-256 of cfg as JSON (every key, the defaults
// included) in 12 hex digits; "" when it cannot be encoded.
func configSum(cfg *config.Config) string {
	b, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])[:12]
}

// recordAttentionRun records this daemon's fingerprint with the PR's
// attention (needsAttentionKeyed).
func (e *Engine) recordAttentionRun(ctx context.Context, prID int64, run runFingerprint) {
	raw, err := json.Marshal(run)
	if err != nil {
		return
	}
	e.setKV(ctx, kvPRAttentionRun(prID), string(raw))
}

// retryParked runs at the start, after recovery and before the first
// dispatch: every open PR a failed round parked (attention.KindFailed)
// under another build, config or prompts than this start's goes back where
// it waits for a round (queued, or rereview_pending for a reviewed PR) with
// a fresh retry budget, not forced, so it waits its turn and the throttle
// as any PR does; a PR parked before the run was recorded was parked by an
// older build. One pr.retried_after_restart event per PR and one toast for
// them all. A PR that fails again under this run is parked with it and
// stays until another one; the other kinds (a fix loop's call, a blocked
// judge, an identity failure, an overloaded API) stay parked, and so do a
// PR with a Codex flag and one the watch no longer accepts.
func (e *Engine) retryParked(ctx context.Context) {
	if e.d.DryRun {
		return
	}
	prs, err := e.st.ListPRs(ctx, store.PRFilter{States: []string{store.PRNeedsAttention}})
	if err != nil {
		e.warnUnlessStopped(ctx, err, "retry parked PRs")
		return
	}
	cur := e.runFingerprint()
	on := e.build.Label()
	if e.build.Revision != "" {
		on = textx.ShortSHA(e.build.Revision)
	}
	now := e.now()
	var lines []string
	for _, pr := range prs {
		if why, _ := e.getKV(ctx, KVPRAttention(pr.ID)); why != attention.KindFailed || pr.GHState != store.GHOpen {
			continue
		}
		changed := []string{"build"} // parked by a build before the record
		if raw, ok := e.getKV(ctx, kvPRAttentionRun(pr.ID)); ok {
			var parked runFingerprint
			if json.Unmarshal([]byte(raw), &parked) == nil {
				changed = parked.changes(cur)
			}
		}
		if len(changed) == 0 {
			continue
		}
		if _, flagged := e.codexFlag(ctx, pr.ID); flagged {
			continue
		}
		repo, err := e.st.RepoByID(ctx, pr.RepoID)
		if err != nil {
			continue
		}
		w := e.cfg.WatchFor(repo.FullName())
		if w == nil || !e.classify(ctx, *w, pr, now).Eligible {
			continue
		}
		to := claimableState(pr)
		td := e.throttle(ctx, *w, pr, e.factsFor(ctx, pr, *w, now), now)
		err = e.st.TransitionPR(ctx, pr.ID, []string{store.PRNeedsAttention}, to, func(u *store.PRUpdate) {
			u.Set("attempts", 0)
			u.Set("next_attempt_at", nil)
			u.Set("next_eligible_at", td.NextEligibleAt)
			u.Set("skip_reason", nil)
			u.Set("last_error", nil)
		})
		if err != nil {
			e.warnUnlessStopped(ctx, err, "retry a parked PR", "pr", pr.ID)
			continue
		}
		e.delKV(ctx, KVPRAttention(pr.ID), kvPRAttentionRun(pr.ID))
		e.event(ctx, "info", prSubject(repo, pr.Number), "pr.retried_after_restart",
			fmt.Sprintf("retried after the restart on %s: the failure's %s (%s → %s)", on, differs(changed), store.PRNeedsAttention, to),
			map[string]any{"to": to, "changed": changed, "build": cur.Build})
		lines = append(lines, fmt.Sprintf("%s#%d %s", repo.FullName(), pr.Number, pr.URL))
	}
	if len(lines) == 0 {
		return
	}
	title := fmt.Sprintf("magnum: retrying %d PRs parked by failed rounds before this build", len(lines))
	if len(lines) == 1 {
		title = "magnum: retrying 1 PR parked by a failed round before this build"
	}
	e.info(notify.Item{Key: "retried-after-restart:" + store.FormatTime(now), Title: title,
		Body: strings.Join(lines, "\n"), Line: strings.TrimPrefix(title, "magnum: "), Kind: kindRetriedAfterRestart})
}
