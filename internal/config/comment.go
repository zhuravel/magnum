package config

import (
	"fmt"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/github"
)

// CommentIdentity is the watch that covers repository fullName
// ("owner/name") and the identity `magnum comment` posts on its PRs as:
// the watch's fix_identity, which must be a GitHub App of the fixer's own
// (DedicatedFixApp) whose tokens reach the repository (token_repositories).
// It never falls back to another identity (FixRoleFor's fix role or poll
// identity, the operator's gh login): each refusal names the key to set.
func (c *Config) CommentIdentity(fullName string) (*Watch, *Identity, error) {
	w := c.WatchFor(fullName)
	if w == nil {
		return nil, nil, fmt.Errorf("magnum watches no repository %s: magnum comment posts only on the PRs of a [[watch]]'s repositories", fullName)
	}
	const setApp = `set fix_identity in that [[watch]] to a dedicated [[identity]] with kind = "app"`
	if w.FixIdentity == "" {
		return nil, nil, fmt.Errorf("the [[watch]] of %s sets no fix_identity: magnum comment posts only as the watch's fixer GitHub App, "+
			"never as your login; %s", fullName, setApp)
	}
	id := c.IdentityByName(w.FixIdentity)
	switch {
	case id == nil:
		return nil, nil, fmt.Errorf("fix_identity %q of the [[watch]] of %s names no [[identity]]: %s", w.FixIdentity, fullName, setApp)
	case id.Kind != "app":
		return nil, nil, fmt.Errorf("fix_identity %q of the [[watch]] of %s is kind = %q, your own login: magnum comment posts only as a GitHub App, "+
			"never as your login; %s", id.Name, fullName, id.Kind, setApp)
	case !c.DedicatedFixApp(id.Name):
		return nil, nil, fmt.Errorf("fix_identity %q of the [[watch]] of %s is the App magnum reviews as (%s): its comments would read as magnum's review; %s",
			id.Name, fullName, c.reviewAppUse(id.Name), setApp)
	case strings.TrimSpace(id.Login) == "":
		return nil, nil, fmt.Errorf("[[identity]] %s sets no login: set login to the App's bot login (name[bot])", id.Name)
	}
	_, name, _ := strings.Cut(fullName, "/")
	if !slices.ContainsFunc(id.TokenRepositories, func(r string) bool { return strings.EqualFold(r, name) }) {
		return nil, nil, fmt.Errorf("the App %s's tokens do not reach %s: add %q to token_repositories of [[identity]] %s", id.Name, fullName, name, id.Name)
	}
	return w, id, nil
}

// MagnumThreadLogins are the logins whose review threads magnum's review
// started, in Account form, each once: every watch's posting identity (the
// operator's gh login too, in a config that posts as it) and every App
// identity, whatever it is for (a former reviewer's identity is a declared
// one). A comment of magnum's own in such a thread answers nothing: the
// daemon's poll, which reads no comment text, takes the fixer App's reply
// there for the author's.
func (c *Config) MagnumThreadLogins() []string {
	var out []string
	add := func(l string) {
		if l != "" && !slices.ContainsFunc(out, func(o string) bool { return github.SameAccount(o, l) }) {
			out = append(out, l)
		}
	}
	for _, w := range c.Watches {
		if id := c.IdentityByName(w.Identity); id != nil {
			add(id.Account())
		}
	}
	for _, id := range c.Identities {
		if id.Kind == "app" {
			add(id.Account())
		}
	}
	return out
}

// Account is the identity's login in Account form: an App's with its
// "[bot]" (the config may leave it out), a gh identity's as it is.
func (i Identity) Account() string {
	l := strings.TrimSpace(i.Login)
	if i.Kind == "app" {
		return github.Account(l, "Bot")
	}
	return l
}
