package agents

// Verification readiness (backlog 14): before the reviewers start, a round
// runs the repository's prepare commands and ready probes ([[repo]] or
// [[pool]] prepare/ready), after the pool's reset_db when the slot's
// databases carry another schema than the checkout's, and a built-in Ruby
// check in the checkout, so the
// judge knows up front which checks cannot work on this machine instead of
// spending agent time rediscovering it. The judge prompt lists the outcome
// (JudgeData.Readiness); the full results, including each command's last
// output line, are in a JSON file next to the result file.

// Readiness check kinds (ReadinessCheck.Kind).
const (
	// ReadinessResetDB is a [[pool]] reset_db command, run first when the
	// slot's databases carry another schema than the checkout's: it loads
	// the PR's schema into them.
	ReadinessResetDB = "reset_db"
	ReadinessPrepare = "prepare" // a [[repo]]/[[pool]] prepare command
	ReadinessReady   = "ready"   // a [[repo]]/[[pool]] ready probe (exit 0 = ready)
	ReadinessRuby    = "ruby"    // the built-in check that the login shell runs the Ruby the checkout pins
)

// Readiness check statuses (ReadinessCheck.Status).
const (
	ReadinessOK      = "ok"
	ReadinessFailed  = "failed"  // a non-zero exit, a failed start, or (ruby) the wrong version
	ReadinessTimeout = "timeout" // the readiness budget (ready_timeout) ran out while it ran
	ReadinessSkipped = "skipped" // not run: the budget was spent, or the round was cancelled
)

// ReadinessCheck is one command of the readiness step.
type ReadinessCheck struct {
	Kind    string `json:"kind"`    // ReadinessResetDB, ReadinessPrepare, ReadinessReady or ReadinessRuby
	Command string `json:"command"` // as configured (ruby: the version command)
	OK      bool   `json:"ok"`
	Status  string `json:"status"` // ReadinessOK, ReadinessFailed, ReadinessTimeout or ReadinessSkipped
	// Detail is magnum's own one-line account ("exit 1", "the checkout pins
	// Ruby 3.3.4 (.ruby-version); `zsh -lc` runs 3.2.2"): never command
	// output, so prompts may show it.
	Detail string `json:"detail,omitempty"`
	// LastLine is the last non-empty line the command printed (redacted,
	// shortened). It comes from code in the PR's checkout, so it is data:
	// it goes into the readiness file only, never into a prompt.
	LastLine string `json:"last_line,omitempty"`
	Duration string `json:"duration"` // e.g. "4.2s"
}

// Readiness is the readiness step's outcome as a judge prompt shows it. The
// zero value means no step ran (nothing configured, a continued turn).
type Readiness struct {
	Checks []ReadinessCheck
	Failed int    // checks whose Status is not ReadinessOK
	File   string // the JSON file with every check (readiness.json in the report directory); "" when it could not be written
}
