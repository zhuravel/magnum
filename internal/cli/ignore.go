package cli

import "github.com/spf13/cobra"

// newIgnoreCmd is `magnum ignore <ref>`: see stopMain (abort.go).
func newIgnoreCmd(c *Context) *cobra.Command {
	return newStopCmd(c, stopIgnore, "kill a PR's review, mute it and free its slot",
		"Stop caring about a PR: its running review is killed as with `magnum abort`, its sessions are parked, its "+
			"pool slot is handed back (a per-PR worktree is kept) and it is muted with the reason \"ignored\", so the "+
			"daemon never queues it again. `magnum unmute` brings it back; a forced `magnum review` still runs.")
}
