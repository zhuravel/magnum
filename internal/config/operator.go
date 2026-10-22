package config

import "cmp"

// OperatorIdentity is the gh identity of the operator's own account for
// watch w: the login whose review requests start a round, whose PRs count
// as self-authored and whom the review footer names. It is w's poll
// identity when that is a gh identity (its identity when it has no
// poll_identity, as Normalize sets it), else w's auto_approve_as when that
// names a gh identity, else the first gh identity declared; nil when the
// config declares none. An App that polls is never the operator: GitHub
// offers no bot as a reviewer, and its login authors no one's PRs.
func (c *Config) OperatorIdentity(w Watch) *Identity {
	if c == nil {
		return nil
	}
	for _, name := range []string{cmp.Or(w.PollIdentity, w.Identity), w.AutoApproveAs} {
		if id := c.IdentityByName(name); id != nil && id.Kind == "gh" {
			return id
		}
	}
	for i := range c.Identities {
		if c.Identities[i].Kind == "gh" {
			return &c.Identities[i]
		}
	}
	return nil
}

// OperatorLogin is OperatorIdentity's login, "" when none resolves.
func (c *Config) OperatorLogin(w Watch) string {
	if id := c.OperatorIdentity(w); id != nil {
		return id.Login
	}
	return ""
}
