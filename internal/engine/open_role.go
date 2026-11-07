package engine

// What `magnum open` of a PR does without --role, from the PR's sessions:
// which role's pane it opens (OpenRole) and whether it restores the PR in a
// slot first (OpenWouldRestore: it claims a slot, resumes the sessions and
// holds the PR, so the screens ask y/N before it). The CLI's open, the PR
// board and the factory screen all read it here.

import (
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/store"
)

// OpenSessions is what the PR's sessions of role say an open of it does:
// latest is the newest, parked the newest parked one, and starting says the
// newest is starting or live (the open waits for its pane). An open
// restores when one is parked and none is starting (OpenWouldRestore).
func OpenSessions(all []store.Session, role string) (parked, latest *store.Session, starting bool) {
	for i := range all {
		s := &all[i]
		if s.Role != role {
			continue
		}
		latest = s
		if s.State == store.SessionParked {
			parked = s
		}
	}
	starting = latest != nil && (latest.State == store.SessionStarting || latest.State == store.SessionLive)
	return parked, latest, starting
}

// OpenRestoreRole is the role `magnum open` without --role restores the PR
// through when role, the one it would open, has nothing to open: its newest
// session is lost or closed, none of its sessions is parked or up, and no
// session of the PR is starting or live (a live pane is what the open
// focuses). The newest parked session by started_at (the later one of two
// that started together) of a role the daemon's open restores (restorable,
// OpenRestorable) then stands in for it; with none, role stays and the open
// refuses, as it did. A role named with --role is never replaced.
func OpenRestoreRole(all []store.Session, role string, restorable func(string) bool) string {
	parked, latest, starting := OpenSessions(all, role)
	if parked != nil || starting || latest == nil {
		return role
	}
	var newest *store.Session
	for i := range all {
		s := &all[i]
		switch s.State {
		case store.SessionStarting, store.SessionLive:
			return role
		case store.SessionParked:
			if restorable(s.Role) && (newest == nil || !s.StartedAt.Before(newest.StartedAt)) {
				newest = s
			}
		}
	}
	if newest == nil {
		return role
	}
	return newest.Role
}

// OpenRestorable reports whether a parked session of a role may stand in
// for the judge's in `magnum open` of a PR of repository full
// (OpenRestoreRole): the role is one the daemon's open restores, one the
// repository's watch runs (config.Config.RolesFor). Park parks every
// session with a session id, the fixer's, the whiteboard writer's and a
// judge's in reserve too, and the daemon refuses those roles ("unknown
// role"). Without a config any role may.
func OpenRestorable(cfg *config.Config, full string) func(string) bool {
	if cfg == nil {
		return func(string) bool { return true }
	}
	roles := cfg.RolesFor(cfg.WatchFor(full))
	return func(role string) bool {
		return slices.ContainsFunc(roles, func(r config.Role) bool { return r.Name == role })
	}
}

// OpenWouldRestore reports whether `magnum open` of role (as it opens
// without --role) asks the daemon to restore the PR (claim a slot, resume
// its sessions, hold it): no session of role is up (starting, or live with a
// pane or an agent: the open focuses it), and one is parked, role's own or,
// when role's session is gone, a restorable role's (OpenRestoreRole). The
// screens' o asks first when it does.
func OpenWouldRestore(all []store.Session, role string, restorable func(string) bool) bool {
	role = OpenRestoreRole(all, role, restorable)
	for _, s := range all {
		if s.Role == role && (s.State == store.SessionStarting || s.State == store.SessionLive) {
			return false
		}
	}
	parked, _, starting := OpenSessions(all, role)
	return parked != nil && !starting
}

// OpenRole is the role `magnum open` of a PR of repository full opens
// without --role, as the CLI's open resolves it under cfg: the fixer's
// while a fix loop's fixer works (fix: the PR's loop, nil for none), else
// the watch's judge; the parked role that stands in for it when its session
// is gone (OpenRestoreRole over all, the PR's sessions, among the roles the
// daemon's open restores, OpenRestorable).
func OpenRole(cfg *config.Config, full string, fix *FixState, all []store.Session) string {
	asked := ""
	if fix != nil && fix.Phase == FixPhaseFixing && fix.Role != "" {
		asked = fix.Role
	}
	role := store.RoleJudge
	if cfg != nil {
		w := cfg.WatchFor(full)
		if j := cfg.JudgeFor(w); j.Name != "" {
			role = j.Name
		}
		if r, ok := cfg.RoleByNameOrAlias(w, asked); ok {
			role = r.Name
		} else if r, ok := cfg.FixRoleFor(w); ok && strings.TrimSpace(asked) != "" && r.Matches(asked) {
			role = r.Name // the fixer's pane while its loop works
		}
	}
	return OpenRestoreRole(all, role, OpenRestorable(cfg, full))
}
