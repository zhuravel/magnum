package cli

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/textx"
)

// Shell completion reads the registry through its own read-only SQLite
// connection (store.Open would create the file and take the write lock to
// check migrations) and config.toml through Context.LoadConfig. Every source
// fails soft: an error yields no candidates, never a message.

// completeTimeout bounds one completion's registry reads.
const completeTimeout = 2 * time.Second

// completeLimit caps the PRs offered.
const completeLimit = 200

// roleFlagHelp describes the --role flag of open and watch.
const roleFlagHelp = "a `role` name or alias of the PR's watch (default: its judge; magnum roles lists them)"

// completeWatchedRoles are the roles of every [[watch]] (config.RolesFor),
// in [[role]] order; every role without watches.
func completeWatchedRoles(cfg *config.Config) []config.Role {
	all := cfg.RolesFor(nil)
	if len(cfg.Watches) == 0 {
		return all
	}
	var out []config.Role
	for _, r := range all {
		for i := range cfg.Watches {
			if slices.ContainsFunc(cfg.RolesFor(&cfg.Watches[i]), func(x config.Role) bool { return x.Name == r.Name }) {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// completeRoleDesc describes a role in one line: its kind, judge, runs.
func completeRoleDesc(r config.Role) string {
	desc := r.Kind + " session"
	if r.IsShell() {
		desc = "shell command"
		if r.Tool != "" {
			desc += " (" + r.Tool + ")"
		}
	}
	if r.Judge {
		desc += ", the judge that posts the review"
	}
	if r.Runs != "" && r.Runs != config.RunsAlways {
		desc += ", runs " + r.Runs
	}
	return desc
}

// completeRoleNames offers the configured roles of the watches for --role:
// the names, then the aliases.
func (c *Context) completeRoleNames(string) []cobra.Completion {
	if c.LoadConfig() != nil {
		return nil
	}
	roles := completeWatchedRoles(c.Config)
	var out []cobra.Completion
	for _, r := range roles {
		out = append(out, cobra.CompletionWithDesc(r.Name, completeRoleDesc(r)))
	}
	for _, r := range roles {
		for _, a := range r.Aliases {
			out = append(out, cobra.CompletionWithDesc(a, "alias of "+r.Name))
		}
	}
	return out
}

// completeRequestableRoles offers the roles `magnum review --role` is for
// first (runs "first" or "manual"), then every other role name.
func (c *Context) completeRequestableRoles(string) []cobra.Completion {
	if c.LoadConfig() != nil {
		return nil
	}
	roles := completeWatchedRoles(c.Config)
	onRequest := func(r config.Role) bool { return r.Runs == config.RunsFirst || r.Runs == config.RunsManual }
	var out []cobra.Completion
	for _, r := range roles {
		if onRequest(r) {
			out = append(out, cobra.CompletionWithDesc(r.Name, completeRoleDesc(r)))
		}
	}
	for _, r := range roles {
		if !onRequest(r) {
			out = append(out, cobra.CompletionWithDesc(r.Name, completeRoleDesc(r)))
		}
	}
	return out
}

// completeKinds offers the agent kinds of config.toml (the roles' first)
// and "all", for resume --tool.
func (c *Context) completeKinds(string) []cobra.Completion {
	if c.LoadConfig() != nil {
		return nil
	}
	var out []cobra.Completion
	for _, k := range rolesAllKinds(c.Config) {
		desc := "agent kind, no role uses it"
		if users := rolesUsers(c.Config, k); len(users) > 0 {
			desc = "agent kind of " + strings.Join(users, ", ")
		}
		out = append(out, cobra.CompletionWithDesc(k, desc))
	}
	return append(out, cobra.CompletionWithDesc("all", "every agent kind"))
}

// completeQuery runs a read-only query against the registry; it returns
// false when there is no registry yet or it cannot be read.
func (c *Context) completeQuery(query string, scan func(*sql.Rows) error, args ...any) bool {
	path := c.Layout.DB()
	if _, err := os.Stat(path); err != nil {
		return false
	}
	db, err := sql.Open("sqlite", registryReadOnlyDSN(path))
	if err != nil {
		return false
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), completeTimeout)
	defer cancel()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		if scan(rows) != nil {
			return false
		}
	}
	return rows.Err() == nil
}

// completePRs offers the registry's open or still-active PRs as
// owner/repo#N with the title as description. A number being typed also
// gets the bare N of default-repo PRs (N resolves against
// daemon.default_repo).
func (c *Context) completePRs(toComplete string) []cobra.Completion {
	return c.completePRsWhere(toComplete, "p.gh_state = 'OPEN' OR p.state NOT IN ('closed', 'releasing', 'released')", "p.updated_at")
}

// completeClosedPRs is completePRs for the PRs closed or merged on GitHub,
// the latest closed first: the ones `retro` looks at and `misses` lists.
func (c *Context) completeClosedPRs(toComplete string) []cobra.Completion {
	return c.completePRsWhere(toComplete, "p.gh_state IN ('MERGED', 'CLOSED')", "COALESCE(p.closed_at, p.merged_at, p.updated_at)")
}

// completePRsWhere offers the PRs where (a SQL condition over prs p) selects,
// ordered by order, newest first.
func (c *Context) completePRsWhere(toComplete, where, order string) []cobra.Completion {
	defaultRepo := ""
	if c.LoadConfig() == nil {
		defaultRepo = strings.ToLower(c.Config.Daemon.DefaultRepo)
	}
	_, numErr := strconv.Atoi(toComplete)
	numeric := toComplete != "" && numErr == nil
	var out []cobra.Completion
	c.completeQuery(`SELECT r.owner, r.name, p.number, COALESCE(p.title, '')
		FROM prs p JOIN repos r ON r.id = p.repo_id
		WHERE (`+where+`)
		ORDER BY `+order+` DESC LIMIT ?`, func(rows *sql.Rows) error {
		var owner, name, title string
		var n int
		if err := rows.Scan(&owner, &name, &n, &title); err != nil {
			return err
		}
		full := owner + "/" + name
		if numeric && strings.EqualFold(full, defaultRepo) {
			out = append(out, cobra.CompletionWithDesc(strconv.Itoa(n), completeDesc(title)))
		}
		out = append(out, cobra.CompletionWithDesc(fmt.Sprintf("%s#%d", full, n), completeDesc(title)))
		return nil
	}, completeLimit)
	return out
}

// completeSlots offers the registry's slots that are not removed, described
// by state and the PR they hold.
func (c *Context) completeSlots(string) []cobra.Completion {
	var out []cobra.Completion
	c.completeQuery(`SELECT s.name, s.state, COALESCE(r.owner || '/' || r.name, ''), COALESCE(p.number, 0)
		FROM slots s LEFT JOIN prs p ON p.id = s.pr_id LEFT JOIN repos r ON r.id = p.repo_id
		WHERE s.state <> 'removed' ORDER BY s.name`, func(rows *sql.Rows) error {
		var name, state, repo string
		var n int
		if err := rows.Scan(&name, &state, &repo, &n); err != nil {
			return err
		}
		desc := "slot, " + state
		if held := fmt.Sprintf("%s#%d", repo, n); repo != "" && n > 0 && held != name {
			desc += ", " + held
		}
		out = append(out, cobra.CompletionWithDesc(name, desc))
		return nil
	})
	return out
}

// completeIdentities offers the [[identity]] names of config.toml.
func (c *Context) completeIdentities(string) []cobra.Completion {
	if c.LoadConfig() != nil {
		return nil
	}
	var out []cobra.Completion
	for _, id := range c.Config.Identities {
		desc := id.Kind
		if id.Login != "" {
			desc += " " + id.Login
		}
		out = append(out, cobra.CompletionWithDesc(id.Name, desc))
	}
	return out
}

// completePools offers the [[pool]] repositories of config.toml.
func (c *Context) completePools(string) []cobra.Completion {
	if c.LoadConfig() != nil {
		return nil
	}
	var out []cobra.Completion
	for _, p := range c.Config.Pools {
		out = append(out, cobra.CompletionWithDesc(p.Repo, fmt.Sprintf("pool %s, min %d, max %d", p.SlotName, p.Min, p.Max)))
	}
	return out
}

// completeWatches offers the [[watch]] owners of config.toml.
func (c *Context) completeWatches(string) []cobra.Completion {
	if c.LoadConfig() != nil {
		return nil
	}
	var out []cobra.Completion
	for _, w := range c.Config.Watches {
		out = append(out, cobra.CompletionWithDesc(w.Owner, "watch, posts as "+w.Identity))
	}
	return out
}

// completeRepos offers the repositories the watches polled (the registry's
// repos) as owner/name, plus the bare name for daemon.default_repo's owner.
func (c *Context) completeRepos(string) []cobra.Completion {
	defOwner := ""
	if c.LoadConfig() == nil {
		defOwner, _, _ = strings.Cut(strings.ToLower(c.Config.Daemon.DefaultRepo), "/")
	}
	var out []cobra.Completion
	c.completeQuery(`SELECT owner, name, mode FROM repos ORDER BY owner, name`, func(rows *sql.Rows) error {
		var owner, name, mode string
		if err := rows.Scan(&owner, &name, &mode); err != nil {
			return err
		}
		desc := "watched repository, review slots"
		if mode == "per_pr" {
			desc = "watched repository, per-PR worktrees"
		}
		out = append(out, cobra.CompletionWithDesc(owner+"/"+name, desc))
		if defOwner != "" && strings.EqualFold(owner, defOwner) {
			out = append(out, cobra.CompletionWithDesc(name, desc))
		}
		return nil
	})
	return out
}

// completeDesc keeps a description to one short line.
func completeDesc(s string) string {
	return textx.Clip(strings.Join(strings.Fields(s), " "), 80)
}

// completeFirst completes the first positional argument from sources (in
// order); later arguments complete nothing.
func completeFirst(sources ...func(toComplete string) []cobra.Completion) cobra.CompletionFunc {
	return func(_ *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeFrom(toComplete, sources...)
	}
}

// completeFlag completes a flag value from sources.
func completeFlag(sources ...func(toComplete string) []cobra.Completion) cobra.CompletionFunc {
	return func(_ *cobra.Command, _ []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
		return completeFrom(toComplete, sources...)
	}
}

func completeFrom(toComplete string, sources ...func(string) []cobra.Completion) ([]cobra.Completion, cobra.ShellCompDirective) {
	var out []cobra.Completion
	for _, src := range sources {
		out = append(out, src(toComplete)...)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

// completeRole registers --role completion on cmd.
func (c *Context) completeRole(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("role", completeFlag(c.completeRoleNames))
}

// completePluginContext marks the plugin-context flags: --cwd is a
// directory, --workspace a herdr id the shell cannot know.
func completePluginContext(cmd *cobra.Command) {
	_ = cmd.MarkFlagDirname("cwd")
}
