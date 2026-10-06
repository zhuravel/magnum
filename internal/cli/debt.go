package cli

// `magnum debt`: the problems the judge proved but did not post because the
// PR did not bring them (reason_code pre_existing), across PRs: the P1 and P2
// ones it marked nearby (in or near code the PR changes, SKILL.md section 7),
// and the untitled ones recorded before titles and the mark existed. Read
// from the registry's findings; it never asks GitHub or the daemon.

import (
	"cmp"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/store"
)

const debtUsage = "[<repo>] [--json]"

func newDebtCmd(c *Context) *cobra.Command {
	var asJSON bool
	cmd := newCommand(groupInspect, "debt "+debtUsage, "proven problems next to PRs' changes that the PRs did not bring",
		"List the problems magnum's judge proved at a PR's head but did not post because the PR did not bring "+
			"them (rejected as pre_existing): the P1 and P2 ones it marked nearby, in or near code the PR changes, "+
			"newest first and once per path and title (the newest find). The review lists up to three of them under "+
			"\"Found nearby, not this PR's\", never security problems, which only this list shows. Rows recorded "+
			"before findings had titles show the path and the reason only, once per path. A <repo> (owner/name, or "+
			"name in daemon.default_repo's owner) keeps one repository.",
		func(pos []string) int { return runDebt(c, asJSON, pos) })
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.ValidArgsFunction = completeFirst(c.completeRepos)
	return cmd
}

// debtItem is one row of `magnum debt` (and of --json).
type debtItem struct {
	Severity   string    `json:"severity"`
	Path       string    `json:"path,omitempty"`
	Line       int       `json:"line,omitempty"`
	Title      string    `json:"title,omitempty"` // none in rows recorded before titles
	ReasonCode string    `json:"reason_code"`
	Nearby     bool      `json:"nearby"`
	Repo       string    `json:"repo"`
	Number     int       `json:"number"`   // the PR it was found on
	FoundAt    time.Time `json:"found_at"` // when
}

func runDebt(c *Context, asJSON bool, pos []string) int {
	if len(pos) > 1 {
		return inspUsage(c, "debt", "at most one <repo>", debtUsage)
	}
	a, err := inspOpenApp(c, false)
	if err != nil {
		return cmdFail(c, "debt", err)
	}
	defer a.Close()
	repo := ""
	if len(pos) == 1 {
		if repo, err = repoArg(c.Config, "", pos[0]); err != nil {
			return inspUsage(c, "debt", err.Error(), debtUsage)
		}
	}
	ctx, cancel := signalContext()
	defer cancel()
	fs, err := a.Store.PreExistingFindings(ctx)
	if err != nil {
		return cmdFail(c, "debt", err)
	}
	items := debtItems(fs, repo)
	if asJSON {
		if err := writeJSON(c.Stdout, items); err != nil {
			return cmdFail(c, "debt", err)
		}
		return 0
	}
	debtRender(c.Stdout, items)
	return 0
}

// debtItems keeps of fs (newest first) the nearby findings and the untitled
// ones, in repo ("owner/name", or "" for all), once per repository, path and
// title (case and blanks folded; an untitled one per path), the newest.
func debtItems(fs []store.Finding, repo string) []debtItem {
	out := []debtItem{}
	seen := map[string]bool{}
	for _, f := range fs {
		if (repo != "" && !strings.EqualFold(f.Repo, repo)) || (f.Title != "" && !f.Nearby) {
			continue
		}
		key := strings.ToLower(f.Repo + "\x00" + f.Path + "\x00" + strings.Join(strings.Fields(f.Title), " "))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, debtItem{Severity: f.Severity, Path: f.Path, Line: f.Line, Title: f.Title, ReasonCode: f.ReasonCode,
			Nearby: f.Nearby, Repo: f.Repo, Number: f.Number, FoundAt: f.CreatedAt})
	}
	return out
}

// debtRender prints the items as a table: priority, path:line and title (an
// untitled one: its path and reason), the PR and the day it was found.
func debtRender(w io.Writer, items []debtItem) {
	if len(items) == 0 {
		fmt.Fprintln(w, "no proven pre-existing problems recorded")
		return
	}
	tw := inspTable(w)
	fmt.Fprintln(tw, "PRI\tWHERE\tPROBLEM\tPR\tFOUND")
	for _, it := range items {
		where, problem := cmp.Or(it.Path, "-"), it.ReasonCode
		if it.Title != "" {
			problem = it.Title
			if it.Line > 0 {
				where += ":" + strconv.Itoa(it.Line)
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s#%d\t%s\n", actClean(it.Severity), actClean(where), actClean(problem), actClean(it.Repo), it.Number,
			store.DayKey(it.FoundAt))
	}
	tw.Flush()
}
