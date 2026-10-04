package slots

import (
	"github.com/zhuravel/magnum/internal/herdr"
	"github.com/zhuravel/magnum/internal/mysqlx"
)

// The real clients plug straight into Deps (checked at compile time; the
// function is never called).
var (
	_ MySQL = (*mysqlx.Client)(nil)
	_       = func(c *herdr.Client) Deps { return Deps{Snapshot: c.Snapshot, ProcessInfo: c.PaneProcessInfo} }
)
