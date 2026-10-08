package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// kv keys shared by the daemon (which writes them) and the CLI (`magnum
// status`, `doctor`, `pause`, `resume`, `identities`), so neither side spells
// them as string literals. Values are never secrets.
const (
	KVDaemonStartedAt     = "daemon.started_at"     // store.FormatTime
	KVDaemonLastTick      = "daemon.last_tick"      // store.FormatTime
	KVDaemonLastPoll      = "daemon.last_poll"      // store.FormatTime
	KVDaemonLastReconcile = "daemon.last_reconcile" // store.FormatTime
	KVDaemonPid           = "daemon.pid"
	// KVDaemonPaused is "1" while automation is paused (magnum pause);
	// KVDaemonPausedReason and KVDaemonPausedUntil (optional) explain it.
	KVDaemonPaused       = "daemon.paused"
	KVDaemonPausedReason = "daemon.paused_reason"
	KVDaemonPausedUntil  = "daemon.paused_until"

	KVGHRemaining       = "gh.remaining"
	KVGHLimit           = "gh.limit"
	KVGHReset           = "gh.reset"
	KVGHPollPausedUntil = "gh.poll_paused_until"
	KVHerdrUp           = "herdr.up" // "1" or "0"
)

// KVToolPausedUntil holds when a tool's pause ends ("codex", "claude";
// store.FormatTime).
func KVToolPausedUntil(tool string) string { return tool + ".paused_until" }

// KVToolPausedReason holds why a tool is paused (usage_limit | login_required).
func KVToolPausedReason(tool string) string { return tool + ".paused_reason" }

// KVToolPausedDetail holds the pane line that caused a tool pause.
func KVToolPausedDetail(tool string) string { return tool + ".paused_detail" }

// KVToolBackoff holds the last fallback pause of a tool (a Go duration).
func KVToolBackoff(tool string) string { return tool + ".backoff" }

// KVIdentityCheck holds "pass" or "fail" from an identity's last Check.
func KVIdentityCheck(name string) string { return "identity." + name + ".check" }

// KVIdentityError holds why an identity's Check failed.
func KVIdentityError(name string) string { return "identity." + name + ".error" }

// KVIdentityTickError holds a tick-time token refresh failure.
func KVIdentityTickError(name string) string { return "identity." + name + ".tick_error" }

// KVIdentityTokenExpiry holds when an App identity's token expires
// (store.FormatTime).
func KVIdentityTokenExpiry(name string) string { return "identity." + name + ".token_expiry" }

// KVWatchPaused holds the reason a watch owner's automation was paused
// (identity leak); magnum resume --watch clears it.
func KVWatchPaused(owner string) string { return "watch." + strings.ToLower(owner) + ".paused" }

// KVWatchPoll holds how a watch owner's radar calls went (engine.WatchPoll
// as JSON): when one last answered, and since when they fail and why.
func KVWatchPoll(owner string) string { return "watch." + strings.ToLower(owner) + ".poll" }

// KVPRSimplify is "1" when the PR's next round runs /simplify (magnum
// review --simplify).
func KVPRSimplify(prID int64) string { return fmt.Sprintf("pr.%d.simplify", prID) }

// KVPRRoles holds the role names (a JSON list) requested for the PR's next
// round (magnum review --role, --simplify); cleared once a review posted.
func KVPRRoles(prID int64) string { return fmt.Sprintf("pr.%d.roles", prID) }

// KVPRFresh is "1" when the PR's next round parks its sessions and starts
// new conversations (magnum review --fresh).
func KVPRFresh(prID int64) string { return fmt.Sprintf("pr.%d.fresh", prID) }

// KVPRSessionsIdentity is the identity the PR's live agent sessions were
// created for (their panes carry that identity's gh config); a round for
// another identity parks them and starts fresh.
func KVPRSessionsIdentity(prID int64) string { return fmt.Sprintf("pr.%d.sessions_identity", prID) }

// KVPRGate is why the daemon skipped the PR at its last dispatch (an agent
// still working, a human in its panes, an unhealthy identity, ...); cleared
// when a round starts. Shown by `magnum status` as "waiting: <reason>".
func KVPRGate(prID int64) string { return fmt.Sprintf("pr.%d.gate", prID) }

// KVPRDryRun holds the PR state a dry-run round (review request with
// dry_run) returns the PR to; while set, the next round posts nothing.
func KVPRDryRun(prID int64) string { return fmt.Sprintf("pr.%d.dry_run", prID) }

// KVPRProject records a session of agent kind kind ("codex", "claude")
// launched without the checkout's project configuration because the PR
// changes it (.codex/ and AGENTS.md; .claude/, .mcp.json, CLAUDE.md and
// AGENTS.md), e.g. pr.7.codex_project (JSON, see agents.ProjectNote); a
// launch of that kind that finds it unchanged clears it.
func KVPRProject(prID int64, kind string) string { return fmt.Sprintf("pr.%d.%s_project", prID, kind) }

// KVPRCodexFlag holds a PR's Codex flag (engine.CodexFlag as JSON): Codex
// flagged its review as a possible cybersecurity risk, and magnum never
// reviews it again nor names it to another PR's agents. Any value counts,
// one that cannot be read too.
func KVPRCodexFlag(prID int64) string { return fmt.Sprintf("pr.%d.codex_flag", prID) }

// CodexFlaggedPRs returns the ids of the PRs that hold a Codex flag
// (KVPRCodexFlag), whatever its value.
func (s *Store) CodexFlaggedPRs(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key FROM kv WHERE key GLOB 'pr.*.codex_flag'")
	if err != nil {
		return nil, fmt.Errorf("codex-flagged prs: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("codex-flagged prs: %w", err)
		}
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.codex_flag", &id); err == nil && KVPRCodexFlag(id) == key {
			out[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("codex-flagged prs: %w", err)
	}
	return out, nil
}

// KVScreenWidths holds the column widths dragged with the mouse on a
// screen ("board", "dashboard") as a JSON object of column name to cells;
// written by the CLI's screens, absent until a column is dragged.
func KVScreenWidths(screen string) string { return "tui." + screen + ".widths" }

// GetKV returns the value for key and whether it exists.
func (s *Store) GetKV(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM kv WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("kv %s: %w", key, err)
	}
	return v, true, nil
}

// SetKV stores value under key. A key that holds value already is left
// alone, updated_at included: the daemon sets the same keys every tick, and
// nothing reads updated_at (a heartbeat such as daemon.last_tick carries its
// time in the value, so it is written every time). Never store secrets here.
func (s *Store) SetKV(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
WHERE kv.value IS NOT excluded.value`, key, value, FormatTime(s.now()))
	if err != nil {
		return fmt.Errorf("set kv %s: %w", key, err)
	}
	return nil
}

// DeleteKV removes key (no error when absent).
func (s *Store) DeleteKV(ctx context.Context, key string) error {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", key); err != nil {
		return fmt.Errorf("delete kv %s: %w", key, err)
	}
	return nil
}
