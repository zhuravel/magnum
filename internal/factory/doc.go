// Package factory is the magnum factory: a second daemon, in the magnum
// binary and with a registry of its own, that takes a Jira key or a free
// prompt and does the work up to the final merge, which stays the operator's.
// Agents investigate in parallel, one writer writes the change test first,
// and the operator answers only what no agent can answer. See
// docs/superpowers/specs/2026-11-03-factory-design.md, section 5 for the
// processes and the state the factory keeps.
//
// The registry lives in the registry package (SQLite, its own migrations);
// the daemon, its stages and its screen build on it.
package factory
