package config

import "github.com/zhuravel/magnum/internal/textx"

// SelfLogins are the logins that count as the operator's own (the board's
// "mine", ★): every watch's posting identity and every gh identity (the
// user), each once (textx.FoldLogin tells them apart).
func (c *Config) SelfLogins() []string {
	if c == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(login string) {
		if k := textx.FoldLogin(login); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, login)
		}
	}
	for _, w := range c.Watches {
		if id := c.IdentityByName(w.Identity); id != nil {
			add(id.Login)
		}
	}
	for _, id := range c.Identities {
		if id.Kind == "gh" {
			add(id.Login)
		}
	}
	return out
}

// SelfMatch returns a test of whether a login is one of SelfLogins
// (textx.FoldLogin: case, "@" and "[bot]" do not matter).
func (c *Config) SelfMatch() func(login string) bool { return textx.MatchLogins(c.SelfLogins()) }

// CommentsWhenClean reports whether the identity named identity posts a
// comment, not an approval, for a clean verdict on repository repo
// ("owner/name"): VerdictsFor's no-findings event is COMMENT.
func (c *Config) CommentsWhenClean(repo, identity string) bool {
	if c == nil {
		return false
	}
	noFindings, _ := c.VerdictsFor(repo, c.IdentityByName(identity))
	return noFindings == "COMMENT"
}
