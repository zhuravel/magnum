package cli

// `magnum star <ref> [--off] [--json]`: the operator's star on a PR, which
// the board lists first in its section (tui.SortPRBoard). The registry keeps
// it (store.KVPRStar), written here at once, with or without a daemon: the
// daemon never reads it, so a star changes no review, queue or dispatch.

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/store"
)

const starUsage = "star <ref> [--off] [--json]"

type starOpts struct {
	off, json bool
	by        string // who stars: "" = magnum star; the board's * is "the board"
}

func newStarCmd(c *Context) *cobra.Command {
	var o starOpts
	cmd := newCommand(groupAct, starUsage, "star a PR you must deliver, so it leads the board (--off unstars it)",
		"Star a PR you must deliver, your own or a colleague's: the board lists the starred PRs first in their "+
			"section (the open PRs, or those merged or closed within [board] recent_closed), newest update first "+
			"whatever the sort, and marks them ✪ before the title (the word star in ASCII; ★ still marks yours). A star "+
			"never shows a PR the board's view, filter, owner scope or h hides, and changes nothing else: no review, "+
			"no queue, no `magnum pick`. Starring again keeps when the PR was first starred; --off unstars it. It sets "+
			"the state, never toggles it, and prints it (--json as an object). The registry keeps the star, written "+
			"at once without the daemon, so it outlives a merge and a reopen. A PR the registry does not know yet is "+
			"refused. The board's * does the same for the PR under the cursor (star, or unstar a starred one).",
		func(pos []string) int { return runStar(c, o, pos) })
	fs := cmd.Flags()
	fs.BoolVar(&o.off, "off", false, "unstar the PR")
	fs.BoolVar(&o.json, "json", false, "print the PR's star as JSON")
	cmd.ValidArgsFunction = completeFirst(c.completePRs)
	return cmd
}

func runStar(c *Context, o starOpts, pos []string) int {
	switch {
	case len(pos) == 0:
		return actUsage(c, "star", "which PR?", starUsage)
	case len(pos) > 1:
		return actUsage(c, "star", "one PR at a time", starUsage)
	}
	d, err := actNewDeps(c, actFull)
	if err != nil {
		return cmdFail(c, "star", err)
	}
	defer d.Close()
	ctx, stop := signalContext()
	defer stop()
	return starMain(ctx, c, d, pos[0], o)
}

// starJSON is `magnum star --json`: the PR and its star after the command.
type starJSON struct {
	Ref       string    `json:"ref"` // owner/name#N
	Starred   bool      `json:"starred"`
	StarredAt time.Time `json:"starred_at,omitzero"` // when it was first starred; left out while not starred
	StarredBy string    `json:"starred_by"`          // "magnum star" or "the board"; "" while not starred
	Changed   bool      `json:"changed"`             // this command starred or unstarred it
}

// starMain stars the PR ref (unstars it with o.off) in the registry and
// prints its star: the board's * and `magnum star` both run it.
func starMain(ctx context.Context, c *Context, d *actDeps, ref string, o starOpts) int {
	verb := "star"
	if o.off {
		verb = "unstar"
	}
	t, err := d.resolveRef(ctx, ref)
	if err != nil {
		return cmdFail(c, "star", verbFix(verb, err))
	}
	rec, starred, changed, err := setPRStar(ctx, d.Store, t.PR.ID, !o.off, cmp.Or(o.by, "magnum star"), d.now())
	if err != nil {
		return cmdFail(c, "star", err)
	}
	if o.json {
		out := starJSON{Ref: fmt.Sprintf("%s#%d", t.full(), t.PR.Number), Starred: starred, Changed: changed}
		if starred {
			out.StarredAt, out.StarredBy = rec.At, rec.By
		}
		if err := writeJSON(c.Stdout, out); err != nil {
			return cmdFail(c, "star", err)
		}
		return 0
	}
	label := d.actLabel(t.full(), t.PR.Number)
	switch {
	case starred && changed:
		fmt.Fprintf(c.Stdout, "starred %s\n", label)
	case starred:
		var since []string
		if !rec.At.IsZero() {
			since = append(since, "since "+inspClock(d.now(), rec.At))
		}
		if rec.By != "" {
			since = append(since, "by "+rec.By)
		}
		line := label + " is starred"
		if len(since) > 0 {
			line += " (" + strings.Join(since, ", ") + ")"
		}
		fmt.Fprintln(c.Stdout, line)
	case changed:
		fmt.Fprintf(c.Stdout, "unstarred %s\n", label)
	default:
		fmt.Fprintf(c.Stdout, "%s is not starred\n", label)
	}
	return 0
}

// prStar is a PR's star record (store.KVPRStar): when it was first starred
// and by whom ("magnum star", "the board").
type prStar struct {
	At time.Time `json:"at,omitzero"`
	By string    `json:"by,omitempty"`
}

// parsePRStar reads a star record. One that cannot be read still stars its
// PR, since only the key's absence means unstarred, with nothing known of
// when or by whom.
func parsePRStar(v string) prStar {
	var s prStar
	if json.Unmarshal([]byte(v), &s) != nil {
		return prStar{}
	}
	return s
}

// setPRStar is the one setter of a PR's star, `magnum star`'s and the
// board's: on stars the PR unless it is starred already, whose first record
// stays (so its time is when it was first starred); off deletes the record.
// It returns the PR's star after the call, whether the PR is starred and
// whether the call changed that.
func setPRStar(ctx context.Context, st *store.Store, prID int64, on bool, by string, now time.Time) (rec prStar, starred, changed bool, err error) {
	key := store.KVPRStar(prID)
	v, had, err := st.GetKV(ctx, key)
	if err != nil {
		return prStar{}, false, false, err
	}
	switch {
	case !on && !had:
		return prStar{}, false, false, nil
	case !on:
		if err := st.DeleteKV(ctx, key); err != nil {
			return prStar{}, false, false, err
		}
		return prStar{}, false, true, nil
	case had:
		return parsePRStar(v), true, false, nil
	}
	rec = prStar{At: now.UTC().Truncate(time.Second), By: by}
	b, err := json.Marshal(rec)
	if err != nil {
		return prStar{}, false, false, err
	}
	if err := st.SetKV(ctx, key, string(b)); err != nil {
		return prStar{}, false, false, err
	}
	return rec, true, true, nil
}

// prStars reads every PR's star in one query, by PR id: the board's source
// marks its rows with them before it filters and sorts.
func prStars(ctx context.Context, st *store.Store) (map[int64]prStar, error) {
	vals, err := st.KVValues(ctx, "pr.*.star")
	if err != nil {
		return nil, err
	}
	out := make(map[int64]prStar, len(vals))
	for key, v := range vals {
		var id int64
		if _, err := fmt.Sscanf(key, "pr.%d.star", &id); err == nil && store.KVPRStar(id) == key {
			out[id] = parsePRStar(v)
		}
	}
	return out, nil
}
