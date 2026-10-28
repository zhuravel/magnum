package agents

import "cmp"

// DBLockLine is the command prefix a role runs each command that touches
// the checkout's databases with (JudgeData.DBLockCommand and
// RoleData.DBLockCommand, `db_lock` in the judge's <magnum> block): `magnum
// db-lock` (the daemon's binary, else magnum on PATH) with the checkout and
// the role, each shell-quoted, ending in `--`; the role appends its
// command. The roles of a round run at the same time on one slot, whose
// databases they share; the lock makes them take turns. An empty checkout
// is left out (db-lock then takes the git work tree it runs in), and so is
// an empty role.
func DBLockLine(magnum, checkout, role string) string {
	return ShellDBLockLine(magnum, checkout, role, "")
}

// ShellDBLockLine is DBLockLine with `--run <run>` before the `--`: the
// line a shell role's command runs through (ShellData.DBLockCommand). The
// run id is magnum's run token: StopShell signals a command in a shell
// role's pane only when its db-lock names a run of that PR and role
// (runToken). An empty run is left out, as DBLockLine leaves it.
func ShellDBLockLine(magnum, checkout, role, run string) string {
	args := []string{cmp.Or(magnum, "magnum"), "db-lock"}
	if checkout != "" {
		args = append(args, "--checkout", checkout)
	}
	if role != "" {
		args = append(args, "--role", role)
	}
	if run != "" {
		args = append(args, "--run", run)
	}
	return shellLine(append(args, "--"))
}
