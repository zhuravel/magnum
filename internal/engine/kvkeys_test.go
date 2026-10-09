package engine

import (
	"testing"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/store"
)

// TestEveryKVKeyTheEngineWritesKeepsItsString pins the registry key of every
// name the engine reads, writes or deletes a kv row under, its own and
// store's, so renaming a constant or a key function never moves a stored
// key: a daemon on the new build would read nothing where the old one wrote,
// and the CLI would show a different key than the daemon keeps.
func TestEveryKVKeyTheEngineWritesKeepsItsString(t *testing.T) {
	const id = 7
	cases := []struct{ name, got, want string }{
		// store's keys, under the one name the engine uses.
		{"store.KVDaemonStartedAt", store.KVDaemonStartedAt, "daemon.started_at"},
		{"store.KVDaemonLastTick", store.KVDaemonLastTick, "daemon.last_tick"},
		{"store.KVDaemonLastPoll", store.KVDaemonLastPoll, "daemon.last_poll"},
		{"store.KVDaemonLastReconcile", store.KVDaemonLastReconcile, "daemon.last_reconcile"},
		{"store.KVDaemonPid", store.KVDaemonPid, "daemon.pid"},
		{"store.KVDaemonPaused", store.KVDaemonPaused, "daemon.paused"},
		{"store.KVDaemonPausedReason", store.KVDaemonPausedReason, "daemon.paused_reason"},
		{"store.KVDaemonPausedUntil", store.KVDaemonPausedUntil, "daemon.paused_until"},
		{"store.KVGHRemaining", store.KVGHRemaining, "gh.remaining"},
		{"store.KVGHLimit", store.KVGHLimit, "gh.limit"},
		{"store.KVGHReset", store.KVGHReset, "gh.reset"},
		{"store.KVGHPollPausedUntil", store.KVGHPollPausedUntil, "gh.poll_paused_until"},
		{"store.KVHerdrUp", store.KVHerdrUp, "herdr.up"},
		{"store.KVToolPausedUntil", store.KVToolPausedUntil("codex"), "codex.paused_until"},
		{"store.KVToolPausedReason", store.KVToolPausedReason("claude"), "claude.paused_reason"},
		{"store.KVToolPausedDetail", store.KVToolPausedDetail("codex"), "codex.paused_detail"},
		{"store.KVToolBackoff", store.KVToolBackoff("claude"), "claude.backoff"},
		{"store.KVIdentityCheck", store.KVIdentityCheck("zhuravel"), "identity.zhuravel.check"},
		{"store.KVIdentityError", store.KVIdentityError("zhuravel"), "identity.zhuravel.error"},
		{"store.KVIdentityTickError", store.KVIdentityTickError("app"), "identity.app.tick_error"},
		{"store.KVIdentityTokenExpiry", store.KVIdentityTokenExpiry("app"), "identity.app.token_expiry"},
		{"store.KVWatchPaused", store.KVWatchPaused("Talkable"), "watch.talkable.paused"},
		{"store.KVWatchPoll", store.KVWatchPoll("Talkable"), "watch.talkable.poll"},
		{"store.KVPRSimplify", store.KVPRSimplify(id), "pr.7.simplify"},
		{"store.KVPRRoles", store.KVPRRoles(id), "pr.7.roles"},
		{"store.KVPRFresh", store.KVPRFresh(id), "pr.7.fresh"},
		{"store.KVPRSessionsIdentity", store.KVPRSessionsIdentity(id), "pr.7.sessions_identity"},
		{"store.KVPRGate", store.KVPRGate(id), "pr.7.gate"},
		{"store.KVPRDryRun", store.KVPRDryRun(id), "pr.7.dry_run"},
		{"store.KVPRCodexFlag", store.KVPRCodexFlag(id), "pr.7.codex_flag"},
		// agents' keys the engine deletes (skew.go).
		{"agents.KVModelLimits", agents.KVModelLimits("codex"), "kind.codex.model_limits"},
		{"agents.KVModelLimited", agents.KVModelLimited("codex", "gpt-5"), "kind.codex.model_limited.gpt-5"},
		// The engine's own keys.
		{"KVDaemonDraining", KVDaemonDraining, "daemon.draining"},
		{"KVInfraPausedUntil", KVInfraPausedUntil, "daemon.infra_paused_until"},
		{"KVInfraPausedReason", KVInfraPausedReason, "daemon.infra_paused_reason"},
		{"KVInfraPausedDetail", KVInfraPausedDetail, "daemon.infra_paused_detail"},
		{"KVInfraBackoff", KVInfraBackoff, "daemon.infra_backoff"},
		{"kvInfraProbeDir", kvInfraProbeDir, "daemon.infra_probe_dir"},
		{"kvDriftLogged", kvDriftLogged, "daemon.drift_logged"},
		{"KVUsageCodexPercent", KVUsageCodexPercent, "usage.codex_percent"},
		{"KVUsageCodexResetsAt", KVUsageCodexResetsAt, "usage.codex_resets_at"},
		{"KVUsageCodexWindow", KVUsageCodexWindow, "usage.codex_window_minutes"},
		{"KVUsageCodexPlan", KVUsageCodexPlan, "usage.codex_plan"},
		{"KVUsageCodexAt", KVUsageCodexAt, "usage.codex_at"},
		{"KVUsageCodexPace24h", KVUsageCodexPace24h, "usage.codex_pace_24h"},
		{"KVDaemonBuild", KVDaemonBuild, "daemon.build"},
		{"kvRestartForBuild", kvRestartForBuild, "daemon.restart_for_build"},
		{"KVDaemonPausedAt", KVDaemonPausedAt, "daemon.paused_at"},
		{"KVDaemonPausedHeld", KVDaemonPausedHeld, "daemon.paused_held"},
		{"KVPromptsLoadedAt", KVPromptsLoadedAt, "daemon.prompts_loaded_at"},
		{"KVPromptsChanged", KVPromptsChanged, "daemon.prompts_changed"},
		{"KVPromptsChangedFiles", KVPromptsChangedFiles, "daemon.prompts_changed_files"},
		{"kvRepliesSince", kvRepliesSince, "daemon.replies_since"},
		{"kvRequestsSince", kvRequestsSince, "daemon.requests_since"},
		{"KVRetroRunning", KVRetroRunning, "learn.retro_running"},
		{"KVRetroDay", KVRetroDay, "learn.retro_day"},
		{"KVRetroLast", KVRetroLast, "learn.retro_last"},
		{"KVNotesCurating", KVNotesCurating, "notes.curating"},
		{"KVNotesCurateQueue", KVNotesCurateQueue, "notes.curate_queue"},
		{"KVNotesMisses", KVNotesMisses("Talkable/Example"), "notes.talkable/example.misses"},
		{"KVNotesOver", KVNotesOver("Talkable/Example"), "notes.talkable/example.over"},
		{"kvPRGateReason", kvPRGateReason(id), "pr.7.gate_reason"},
		{"KVPRAutoApproveRefused", KVPRAutoApproveRefused(id), "pr.7.auto_approve_refused"},
		{"kvAutoApprovalGated", kvAutoApprovalGated(id), "pr.7.auto_approval_gated"},
		{"kvApprovalKept", kvApprovalKept(id), "pr.7.approval_kept"},
		{"kvApprovalRefused", kvApprovalRefused(id), "pr.7.approval_refused"},
		{"kvApprovalPending", kvApprovalPending(id), "pr.7.approval_pending"},
		{"KVPRTrivial", KVPRTrivial(id), "pr.7.trivial"},
		{"KVPRDelta", KVPRDelta(id), "pr.7.delta"},
		{"KVPRDeltaCheck", KVPRDeltaCheck(id), "pr.7.delta_check"},
		{"KVPRSkippedBaseline", KVPRSkippedBaseline(id), "pr.7.skipped_baseline"},
		{"KVPRFormerIdentities", KVPRFormerIdentities(id), "pr.7.former_identities"},
		{"KVPRIdentityPinned", KVPRIdentityPinned(id), "pr.7.identity_pinned"},
		{"KVPRReplyRound", KVPRReplyRound(id), "pr.7.reply_round"},
		{"kvPRRedecide", kvPRRedecide(id), "pr.7.redecide"},
		{"KVPRRequestAt", KVPRRequestAt(id), "pr.7.request_at"},
		{"KVPRRequestBy", KVPRRequestBy(id), "pr.7.request_by"},
		{"KVPRSnooze", KVPRSnooze(id), "pr.7.snooze"},
		{"kvPRFiles", kvPRFiles(id), "pr.7.files"},
		{"KVPRStalemate", KVPRStalemate(id), "pr.7.stalemate"},
		{"kvPRStalemateSeen", kvPRStalemateSeen(id), "pr.7.stalemate_seen"},
		{"kvPRPushes", kvPRPushes(id), "pr.7.pushes"},
		{"KVPRManualVerdict", KVPRManualVerdict(id), "pr.7.manual_verdict"},
		{"KVPRAttention", KVPRAttention(id), "pr.7.attention"},
		{"KVPRWait", KVPRWait(id), "pr.7.wait"},
	}
	byKey := map[string]string{}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
		if other, ok := byKey[c.got]; ok {
			t.Errorf("%s and %s both name the key %q", other, c.name, c.got)
		}
		byKey[c.got] = c.name
	}
}
