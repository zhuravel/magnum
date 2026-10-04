package cli

import (
	"context"

	"github.com/zhuravel/magnum/internal/attention"
	"github.com/zhuravel/magnum/internal/engine"
	"github.com/zhuravel/magnum/internal/store"
)

// prAttention explains why pr needs the user: attention.Explain over its last
// error and the kind the engine recorded when it parked the PR
// (engine.KVPRAttention; inferred for older rows). ok is false unless pr is in
// needs_attention. ref is how the fix names the PR.
func prAttention(ctx context.Context, st *store.Store, pr store.PR, ref string) (attention.Reason, bool) {
	if pr.State != store.PRNeedsAttention {
		return attention.Reason{}, false
	}
	msg := store.Deref(pr.LastError)
	if msg == "" {
		return attention.Reason{Summary: "no error was recorded", Fix: "`magnum review " + ref + "` tries again"}, true
	}
	kind := ""
	if st != nil {
		if v, ok, err := st.GetKV(ctx, engine.KVPRAttention(pr.ID)); err == nil && ok {
			kind = v
		}
	}
	return attention.Explain(kind, msg, ref), true
}

// errorSummary is the one-line explanation of a stored error of a PR that is
// not parked (a setup failure it retries, a pause), "" for none.
func errorSummary(msg string) string {
	if msg == "" {
		return ""
	}
	return attention.Explain("", msg, "").Summary
}
