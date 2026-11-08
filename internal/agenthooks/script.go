package agenthooks

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
)

// The hook script's header (hooks/magnum-agent-hook.sh, embedded as
// magnum.AgentHookScript): ScriptMarker, "# MAGNUM_HOOK_ID=<agent>" and
// "# MAGNUM_HOOK_VERSION=<n>".
const (
	// ScriptVersion is the version of the embedded script. It changes only
	// with the script's file: the hooks' command lines stay the same forever
	// (Codex keys its trust on them), so an upgrade asks for no new trust.
	ScriptVersion = 1
	// ScriptMarker begins the line that marks a file as magnum's own.
	ScriptMarker = "# managed by magnum"
	// scriptAgent is what Script replaces with the agent.
	scriptAgent = "{{agent}}"
	// scriptHead bounds how much of a file ScriptHeader reads.
	scriptHead = 4 << 10
)

var (
	scriptID      = regexp.MustCompile(`(?m)^# MAGNUM_HOOK_ID=([a-z]+)$`)
	scriptVersion = regexp.MustCompile(`(?m)^# MAGNUM_HOOK_VERSION=([0-9]{1,6})$`)
)

// Script is the hook script template (magnum.AgentHookScript) made for
// agent: its MAGNUM_HOOK_ID names it. An agent that sends no events, and a
// template without the placeholder, are errors.
func Script(template []byte, agent string) ([]byte, error) {
	if !knownAgent(agent) {
		return nil, fmt.Errorf("agenthooks: no hook script for agent %q", agent)
	}
	if bytes.Count(template, []byte(scriptAgent)) != 1 {
		return nil, fmt.Errorf("agenthooks: the hook script template has no single %s", scriptAgent)
	}
	return bytes.Replace(template, []byte(scriptAgent), []byte(agent), 1), nil
}

// ScriptHeader reads a hook script's header from b (its first few KiB):
// the agent it was made for, its version, and whether a line begins with
// ScriptMarker (magnum's own file, which the install may overwrite). A file
// without the marker reports no agent and version 0.
func ScriptHeader(b []byte) (agent string, version int, managed bool) {
	head := b[:min(len(b), scriptHead)]
	if !bytes.HasPrefix(head, []byte(ScriptMarker)) && !bytes.Contains(head, []byte("\n"+ScriptMarker)) {
		return "", 0, false
	}
	if m := scriptID.FindSubmatch(head); m != nil {
		agent = string(m[1])
	}
	if m := scriptVersion.FindSubmatch(head); m != nil {
		version, _ = strconv.Atoi(string(m[1]))
	}
	return agent, version, true
}
