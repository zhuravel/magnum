package cli

// The byte splice of `magnum hooks`: magnum's agent hook goes into Claude
// Code's settings.json and each Codex home's hooks.json without a byte of
// anything else changing. Codex keys each hook's trust by its place
// ("<event>:<group>:<index>"), so an insertion before another tool's group
// costs every later hook its trust (the 2026-10-22 outage): magnum's group
// is only ever appended at the end of its event's array, rewritten in its
// own place, or removed (which uninstall names). The document is read with
// json.Decoder offsets and edited one change at a time, read again after
// each, in the file's own indentation and line ends.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/zhuravel/magnum/internal/agenthooks"
	"github.com/zhuravel/magnum/internal/agents"
)

// hookScriptName is the hook script's file name; a handler whose command
// names it is magnum's.
const hookScriptName = "magnum-agent-hook.sh"

// hookEvent is one event magnum hooks: as the settings name it, and as the
// hook script's second argument (agenthooks' Name*) and Codex's trust keys
// name it, with the timeout of magnum's handler in seconds. The handlers
// are async, so the agent never waits on them, and the hook bounds itself
// to 300 ms (agenthooks.HookBound). Codex's trust hash covers the command,
// the timeout and async, so none of them ever changes once installed (a
// change is a rewrite, and a new trust in every Codex home).
type hookEvent struct {
	name, arg string
	timeout   int
}

// claudeHookEvents and codexHookEvents are the events magnum hooks, in the
// order the install adds them, with their timeouts within the agents'
// documented limits: Codex takes 1 to 3 s for Interrupt (1 by default, in
// the background too) and defaults the others to 600 s; Claude enforces no
// timeout on an async hook, but a SessionEnd hook's own timeout raises the
// 1.5 s budget all SessionEnd hooks share, so magnum's is 1 s there.
var (
	claudeHookEvents = []hookEvent{{"Stop", agenthooks.NameStop, 5}, {"StopFailure", agenthooks.NameStopFailure, 5},
		{"PermissionRequest", agenthooks.NamePermissionRequest, 5}, {"SessionStart", agenthooks.NameSessionStart, 5},
		{"SessionEnd", agenthooks.NameSessionEnd, 1}}
	codexHookEvents = []hookEvent{{"Stop", agenthooks.NameStop, 5}, {"SessionStart", agenthooks.NameSessionStart, 5},
		{"PermissionRequest", agenthooks.NamePermissionRequest, 5}, {"Interrupt", agenthooks.NameInterrupt, 1}}
)

// hookEntry is magnum's handler of one event: Event as the settings name
// it, Command the line the agent runs (hookCommand), Timeout its timeout in
// seconds (hookEvent's).
type hookEntry struct {
	Event, Command string
	Timeout        int
}

// hookCommand is the command of magnum's handler for agent's event arg with
// the script at script: `sh '<script>' <agent> <arg>`. It never changes
// across versions (only the script does), so Codex's trust of the hook's
// definition holds; whether Codex 0.167 also hashes the script it runs
// (its state has a trusted_content_hash) is untested, so check that before
// the first change of agenthooks.ScriptVersion.
func hookCommand(script, agent, arg string) string {
	return "sh " + shSingleQuote(script) + " " + agent + " " + arg
}

// shSingleQuote is s as one single-quoted shell word.
func shSingleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// hookChange is one change the install makes: magnum's group added to
// event, or its handler rewritten in place (its command, timeout or async
// differed) at place ("stop:1:0"). newTrust says that Codex hashes the
// rewritten hook unlike the one it replaced, so its trust entry there no
// longer holds; a rewrite Codex hashes the same (an Interrupt timeout of 0
// is Codex's 1 s) keeps it.
type hookChange struct {
	event    string
	rewrote  bool
	place    string
	newTrust bool
}

// The state of one event's entry (readHookEntries).
const (
	entryAbsent  = iota // no handler of the event names the script
	entryCurrent        // a handler names it and is magnum's as the install writes it
	entryDrifted        // a handler names it, with another command, type, timeout or async
)

// hookEntryState is what a settings file holds of one of magnum's entries:
// its state and the places of the handlers that name the script
// ("stop:1:0", as Codex keys its trust).
type hookEntryState struct {
	event  string
	state  int
	places []string
}

// spliceHooks adds entries to the settings doc of agent (empty: a file to
// make; agent says how the file is read, readHooksDoc):
// each event's handler is appended in a group of its own at the end of the
// event's array, the event's key or the hooks object made when missing; a
// handler of magnum's already there (its command names the script) stays
// where it stands, rewritten in place only when its type, command, timeout
// or async differ. changes says what changed (none: doc as it was). A doc
// that is no JSON object, or whose hooks are not an object of arrays, is
// refused (nil, nil, the reason).
func spliceHooks(doc []byte, agent string, entries []hookEntry) ([]byte, []hookChange, error) {
	if len(doc) == 0 {
		doc = []byte("{}\n")
	}
	doc = slices.Clone(doc)
	var changes []hookChange
	for _, e := range entries {
		for {
			d, err := readHooksDoc(doc, agent)
			if err != nil {
				return nil, nil, err
			}
			edit, ok := d.entryEdit(e)
			if !ok {
				break
			}
			doc = edit.apply(doc)
			changes = append(changes, hookChange{event: e.Event, rewrote: edit.rewrite, place: edit.place, newTrust: edit.newTrust})
		}
	}
	return doc, changes, nil
}

// unspliceHooks removes every handler of magnum's (its command names the
// script) from doc's hooks, with its group when nothing else is left in
// it, and the event's key and the hooks object when the removal leaves them
// empty; removed counts the handlers. A doc that is no JSON object, or
// whose hooks are not an object of arrays, is refused.
func unspliceHooks(doc []byte, agent string) (out []byte, removed int, err error) {
	doc = slices.Clone(doc)
	for {
		d, err := readHooksDoc(doc, agent)
		if err != nil {
			return nil, 0, err
		}
		edit, ok := d.removeFirst()
		if !ok {
			return doc, removed, nil
		}
		doc = edit.apply(doc)
		removed++
	}
}

// readHookEntries reads which of entries the settings doc of agent holds
// (empty: no file). It refuses what spliceHooks refuses.
func readHookEntries(doc []byte, agent string, entries []hookEntry) ([]hookEntryState, error) {
	var d hooksDoc
	if len(doc) > 0 {
		var err error
		if d, err = readHooksDoc(doc, agent); err != nil {
			return nil, err
		}
	}
	out := make([]hookEntryState, len(entries))
	for i, e := range entries {
		out[i] = hookEntryState{event: e.Event}
		ev := d.event(e.Event)
		if ev == nil {
			continue
		}
		for _, h := range ev.handlers() {
			if !h.magnum {
				continue
			}
			out[i].places = append(out[i].places, h.place(e.Event))
			switch {
			case h.matches(e):
				out[i].state = max(out[i].state, entryCurrent)
			default:
				out[i].state = entryDrifted
			}
		}
	}
	return out, nil
}

// hookShift is another tool's hook whose place a removal moves: Codex keeps
// no trust for it at its new place.
type hookShift struct{ name, from, to string }

func (s hookShift) String() string { return fmt.Sprintf("%s (%s to %s)", s.name, s.from, s.to) }

// hookShifts names the hooks that are not magnum's whose place differs
// between the Codex hooks.json before and after (unspliceHooks of before),
// in before's order.
func hookShifts(before, after []byte) ([]hookShift, error) {
	b, err := readHooksDoc(before, agenthooks.AgentCodex)
	if err != nil {
		return nil, err
	}
	a, err := readHooksDoc(after, agenthooks.AgentCodex)
	if err != nil {
		return nil, err
	}
	var out []hookShift
	for _, ev := range b.events {
		was := slices.DeleteFunc(ev.handlers(), func(h hookHandler) bool { return h.magnum })
		var now []hookHandler
		if e := a.event(ev.name); e != nil {
			now = slices.DeleteFunc(e.handlers(), func(h hookHandler) bool { return h.magnum })
		}
		if len(was) != len(now) {
			return nil, fmt.Errorf("the hooks of %s changed in number (%d to %d)", ev.name, len(was), len(now))
		}
		for i, h := range was {
			if from, to := h.place(ev.name), now[i].place(ev.name); from != to {
				out = append(out, hookShift{name: agents.CodexHookName(h.typ, h.command), from: from, to: to})
			}
		}
	}
	return out, nil
}

// hookStateName is an event as Codex's trust keys name it: "PermissionRequest"
// is "permission_request".
func hookStateName(event string) string {
	var b strings.Builder
	for i, r := range event {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// jsonSpan is the bytes [start, end) of a value in a document.
type jsonSpan struct{ start, end int }

// jsonMember is one member of an object: its key, where the key's quote
// begins and its value.
type jsonMember struct {
	key      string
	keyStart int
	val      jsonSpan
}

// jsonObject is an object of a document: the offsets of its braces and its
// members.
type jsonObject struct {
	open, close int
	members     []jsonMember
}

// jsonArray is an array of a document: the offsets of its brackets and its
// elements.
type jsonArray struct {
	open, close int
	elems       []jsonSpan
}

// readJSONObject reads the object at sp of doc; false when it is none.
func readJSONObject(doc []byte, sp jsonSpan) (jsonObject, bool) {
	b := doc[sp.start:sp.end]
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return jsonObject{}, false
	}
	o := jsonObject{open: sp.start + int(dec.InputOffset()) - 1}
	for dec.More() {
		prev := int(dec.InputOffset())
		t, err := dec.Token()
		key, ok := t.(string)
		if err != nil || !ok {
			return jsonObject{}, false
		}
		val, ok := readJSONValue(doc, sp.start, dec)
		if !ok {
			return jsonObject{}, false
		}
		o.members = append(o.members, jsonMember{key: key, keyStart: sp.start + skipToValue(b, prev), val: val})
	}
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return jsonObject{}, false
	}
	o.close = sp.start + int(dec.InputOffset()) - 1
	return o, true
}

// readJSONArray reads the array at sp of doc; false when it is none.
func readJSONArray(doc []byte, sp jsonSpan) (jsonArray, bool) {
	dec := json.NewDecoder(bytes.NewReader(doc[sp.start:sp.end]))
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return jsonArray{}, false
	}
	a := jsonArray{open: sp.start + int(dec.InputOffset()) - 1}
	for dec.More() {
		val, ok := readJSONValue(doc, sp.start, dec)
		if !ok {
			return jsonArray{}, false
		}
		a.elems = append(a.elems, val)
	}
	if t, err := dec.Token(); err != nil || t != json.Delim(']') {
		return jsonArray{}, false
	}
	a.close = sp.start + int(dec.InputOffset()) - 1
	return a, true
}

// readJSONValue reads dec's next value, whose decoder reads doc from base,
// and returns its span; false when the bytes there are not the value's.
func readJSONValue(doc []byte, base int, dec *json.Decoder) (jsonSpan, bool) {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return jsonSpan{}, false
	}
	end := base + int(dec.InputOffset())
	sp := jsonSpan{end - len(raw), end}
	return sp, sp.start >= base && bytes.Equal(doc[sp.start:sp.end], raw)
}

// skipToValue is the offset in b of the first byte after off that is
// neither white space nor a comma: where a member or element begins.
func skipToValue(b []byte, off int) int {
	for off < len(b) && (isJSONSpace(b[off]) || b[off] == ',') {
		off++
	}
	return off
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// hooksDoc is a settings document as the splice reads it.
type hooksDoc struct {
	doc   []byte
	style jsonStyle
	root  jsonObject
	// hooks is the root's "hooks" member and obj its object; hooks is nil
	// when the root has none.
	hooks  *jsonMember
	obj    jsonObject
	events []hookEventArray
}

// hookEventArray is one event's array of groups.
type hookEventArray struct {
	name   string
	member int // its index among the hooks object's members
	arr    jsonArray
	groups []hookGroup
}

// hookGroup is one group of an event: its handlers' array, when it is an
// object with one.
type hookGroup struct {
	span     jsonSpan
	list     *jsonArray
	handlers []hookHandler
}

// hookHandler is one handler of a group, at its place.
type hookHandler struct {
	group, index int
	span         jsonSpan
	typ, command string
	timeout      any
	async        any
	magnum       bool // its command names hookScriptName
	extra        bool // it has keys besides these four (statusMessage, ...), which a rewrite drops
}

// readHooksDoc reads doc, agent's settings: a JSON object whose "hooks",
// when it has one, is an object whose every member is an array of groups.
// Anything else is refused, with the reason. A key twice is read as the
// agent reads it. Codex's derived structs reject one among the keys they
// define (codexKeys) and ignore any other key, so Codex skips such a
// hooks.json, and magnum refuses it with that reason and reads the rest as
// Codex does. Claude's parser takes the last, so magnum does too, and
// refuses a key twice only in a group or a hook that holds magnum's
// command, or for an event magnum hooks, where it writes. A "hooks" key
// twice in the root is refused for both. Codex reads the root itself
// strictly: a key there besides description and hooks makes it skip the
// file (codexSkipsFileError).
func readHooksDoc(doc []byte, agent string) (hooksDoc, error) {
	if !json.Valid(doc) {
		return hooksDoc{}, errors.New("is not valid JSON")
	}
	d := hooksDoc{doc: doc}
	var ok bool
	if d.root, ok = readJSONObject(doc, jsonSpan{0, len(doc)}); !ok {
		return hooksDoc{}, errors.New("is not a JSON object")
	}
	d.style = readJSONStyle(doc, d.root)
	for i := range d.root.members {
		if d.root.members[i].key != "hooks" {
			continue
		}
		if d.hooks != nil {
			return hooksDoc{}, errors.New(`has two "hooks" keys`)
		}
		d.hooks = &d.root.members[i]
	}
	if d.hooks == nil {
		return d, nil
	}
	if d.obj, ok = readJSONObject(doc, d.hooks.val); !ok {
		return hooksDoc{}, errors.New(`has a "hooks" that is not an object`)
	}
	codex := agent == agenthooks.AgentCodex
	if codex {
		for _, m := range d.root.members {
			if m.key != "description" && m.key != "hooks" {
				return hooksDoc{}, &codexSkipsFileError{key: m.key}
			}
		}
	}
	if codex && slices.Contains(twiceKeys(d.root, nil), "description") {
		return hooksDoc{}, fmt.Errorf("has two %q keys: %s", "description", codexSkipsTwice)
	}
	for _, k := range twiceKeys(d.obj, nil) {
		switch {
		case codex && codexKeys.events[k]:
			return hooksDoc{}, fmt.Errorf("has two %q keys in its hooks: %s", k, codexSkipsTwice)
		case codex:
		case slices.ContainsFunc(hookEventsOf(agent), func(e hookEvent) bool { return e.name == k }):
			return hooksDoc{}, fmt.Errorf("has two %q keys in its hooks, an event magnum hooks", k)
		}
	}
	for i, m := range d.obj.members {
		arr, ok := readJSONArray(doc, m.val)
		if !ok {
			return hooksDoc{}, fmt.Errorf("has a hooks %q that is not an array", m.key)
		}
		ev := hookEventArray{name: m.key, member: i, arr: arr}
		for g, sp := range arr.elems {
			grp, err := readHookGroup(doc, g, sp, codex)
			if err != nil {
				return hooksDoc{}, err.in(m.key, codex)
			}
			ev.groups = append(ev.groups, grp)
		}
		d.events = append(d.events, ev)
	}
	return d, nil
}

// codexSkipsFileError is a Codex hooks.json whose root has a key besides
// description and hooks: Codex reads the root strictly (HooksFile's
// deny_unknown_fields) and skips the whole file, so no hook in it runs.
type codexSkipsFileError struct{ key string }

func (e *codexSkipsFileError) Error() string {
	return fmt.Sprintf("Codex skips this file: unknown key %q", e.key)
}

// codexSkipsTwice is why a Codex hooks.json with a key twice is refused.
const codexSkipsTwice = "Codex skips a hooks.json with one of its own keys twice"

// codexKeys are the keys Codex's hooks.json structs define (openai/codex
// HookEventsToml, MatcherGroup, HookHandlerConfig), where a key twice makes
// Codex skip the file; it ignores any other key, twice or not. A handler's
// command_windows is commandWindows' other name.
var codexKeys = struct{ events, group, handler map[string]bool }{
	events: setOf("PreModelRequest", "PostModelRequest", "PreToolUse", "PermissionRequest", "PostToolUse", "PreCompact",
		"PostCompact", "SessionStart", "SessionEnd", "UserPromptSubmit", "SubagentStart", "SubagentStop", "Stop", "Interrupt"),
	group: setOf("matcher", "hooks"),
	handler: setOf("type", "command", "commandWindows", "timeout", "async", "statusMessage", "additionalContextLimit",
		"server", "tool", "input"),
}

func setOf(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// codexHandlerKey is a handler key as Codex's struct names it.
func codexHandlerKey(k string) string {
	if k == "command_windows" {
		return "commandWindows"
	}
	return k
}

// refusedTwice is the first of keys (keys twice in a group or a hook) the
// reader refuses ("" for none): for Codex one its structs define (known),
// for Claude any where magnum's command is (ours).
func refusedTwice(keys []string, codex, ours bool, known map[string]bool) string {
	for _, k := range keys {
		if codex && known[k] || !codex && ours {
			return k
		}
	}
	return ""
}

// hookEventsOf are the events magnum hooks for agent.
func hookEventsOf(agent string) []hookEvent {
	if agent == agenthooks.AgentCodex {
		return codexHookEvents
	}
	return claudeHookEvents
}

// twiceError is a key twice in a group or a hook (readHookGroup) that the
// reader refuses.
type twiceError struct {
	key, where string // where: "a group" or "a hook"
}

// in is the refusal of the key twice in event's hooks, as the reader
// explains it.
func (e *twiceError) in(event string, codex bool) error {
	if codex {
		return fmt.Errorf("has %q twice in %s of its hooks %q: %s", e.key, e.where, event, codexSkipsTwice)
	}
	return fmt.Errorf("has %q twice in %s of magnum's in its hooks %q", e.key, e.where, event)
}

// twiceKeys are the keys o has more than once, in order, each read through
// fold (nil: as written), so two names of one key count as one.
func twiceKeys(o jsonObject, fold func(string) string) []string {
	seen := map[string]int{}
	var out []string
	for _, m := range o.members {
		k := m.key
		if fold != nil {
			k = fold(k)
		}
		if seen[k]++; seen[k] == 2 {
			out = append(out, k)
		}
	}
	return out
}

// readHookGroup reads group g at sp: an object whose "hooks" is an array
// of handlers (the last "hooks", as Claude reads a key twice); any other
// value holds none. Keys are read exactly, as Claude and Codex read them.
// A key twice in the group or in a handler is refused for Codex when its
// structs define the key (codexKeys), and for Claude where the group holds
// magnum's command in any of its "hooks".
func readHookGroup(doc []byte, g int, sp jsonSpan, codex bool) (hookGroup, *twiceError) {
	grp := hookGroup{span: sp}
	obj, ok := readJSONObject(doc, sp)
	if !ok {
		return grp, nil
	}
	ours := false // a handler of any of its "hooks" names magnum's script
	var twice *twiceError
	for _, m := range obj.members {
		if m.key != "hooks" {
			continue
		}
		list, ok := readJSONArray(doc, m.val)
		if !ok {
			grp.list, grp.handlers = nil, nil
			continue
		}
		var handlers []hookHandler
		for i, hs := range list.elems {
			h, named, keys := readHookHandler(doc, g, i, hs)
			ours = ours || named
			if k := refusedTwice(keys, codex, named, codexKeys.handler); k != "" && twice == nil {
				twice = &twiceError{key: k, where: "a hook"}
			}
			handlers = append(handlers, h)
		}
		grp.list, grp.handlers = &list, handlers
	}
	if k := refusedTwice(twiceKeys(obj, nil), codex, ours, codexKeys.group); k != "" && twice == nil {
		twice = &twiceError{key: k, where: "a group"}
	}
	return grp, twice
}

// readHookHandler reads handler i of group g at sp by its exact keys, the
// last of a key twice winning as Claude reads it; a handler that is no
// object has no command. ours says that any of its "command" values names
// magnum's script, and twice are the keys it has twice (Codex's two names of
// commandWindows counting as one).
func readHookHandler(doc []byte, g, i int, sp jsonSpan) (h hookHandler, ours bool, twice []string) {
	h = hookHandler{group: g, index: i, span: sp}
	obj, ok := readJSONObject(doc, sp)
	if !ok {
		return h, false, nil
	}
	for _, m := range obj.members {
		var v any
		_ = json.Unmarshal(doc[m.val.start:m.val.end], &v) // a valid document's value always decodes
		switch m.key {
		case "type":
			h.typ, _ = v.(string)
		case "command":
			h.command, _ = v.(string)
			ours = ours || strings.Contains(h.command, hookScriptName)
		case "timeout":
			h.timeout = v
		case "async":
			h.async = v
		default:
			h.extra = true
		}
	}
	h.magnum = strings.Contains(h.command, hookScriptName)
	return h, ours, twiceKeys(obj, codexHandlerKey)
}

// event is the event named name, nil when the hooks have none.
func (d *hooksDoc) event(name string) *hookEventArray {
	for i := range d.events {
		if d.events[i].name == name {
			return &d.events[i]
		}
	}
	return nil
}

// handlers are the event's handlers, in order.
func (ev *hookEventArray) handlers() []hookHandler {
	var out []hookHandler
	for _, g := range ev.groups {
		out = append(out, g.handlers...)
	}
	return out
}

// place is h's place in event as Codex keys its trust: "stop:1:0".
func (h hookHandler) place(event string) string {
	return hookStateName(event) + ":" + strconv.Itoa(h.group) + ":" + strconv.Itoa(h.index)
}

// codexSame reports whether Codex hashes h as the handler the install
// writes for e: Codex's trust hash covers the command, the timeout as Codex
// reads it and async, and a key the rewrite drops (statusMessage, ...)
// changes it too. Codex reads a missing or non-numeric timeout as its
// default and clamps the rest (codexTimeout), and a missing async as false.
func (h hookHandler) codexSame(e hookEntry) bool {
	old, ok := codexTimeout(e.Event, h.timeout)
	now, _ := codexTimeout(e.Event, float64(e.Timeout))
	async, _ := h.async.(bool)
	return ok && h.typ == "command" && !h.extra && h.command == e.Command && async && old == now
}

// codexTimeout is the timeout Codex runs a hook of event with for v, the
// value of its "timeout" (nil: none): its default when none, 1 to 3 s for
// Interrupt and SessionEnd, at least 1 s for the rest. ok is false for a
// value Codex does not parse (not a whole number of seconds).
func codexTimeout(event string, v any) (int, bool) {
	short := event == "Interrupt" || event == "SessionEnd"
	f, isNum := v.(float64)
	switch {
	case v == nil && short:
		return 1, true
	case v == nil:
		return 600, true
	case !isNum || f < 0 || f != float64(int64(f)):
		return 0, false
	case short:
		return min(max(int(f), 1), 3), true
	}
	return max(int(f), 1), true
}

// matches reports whether h is magnum's handler for e as the install writes
// it: a command hook with e's command and timeout, and async.
func (h hookHandler) matches(e hookEntry) bool {
	timeout, _ := h.timeout.(float64)
	async, _ := h.async.(bool)
	return h.typ == "command" && h.command == e.Command && timeout == float64(e.Timeout) && async
}

// jsonEdit replaces doc[start:end] with text.
type jsonEdit struct {
	start, end int
	text       string
	rewrite    bool   // it rewrites a handler of magnum's in place
	place      string // the place of the handler a rewrite rewrites
	newTrust   bool   // Codex hashes the rewritten handler unlike the old (codexSame)
}

func (e jsonEdit) apply(doc []byte) []byte {
	out := make([]byte, 0, len(doc)-(e.end-e.start)+len(e.text))
	out = append(out, doc[:e.start]...)
	out = append(out, e.text...)
	return append(out, doc[e.end:]...)
}

// entryEdit is the next edit that puts e in place, false when e is in
// place: every handler of magnum's for its event matches. A drifted one is
// rewritten in place; with none, the group is appended to the event's
// array, which is made when missing, as is the hooks object.
func (d *hooksDoc) entryEdit(e hookEntry) (jsonEdit, bool) {
	ev := d.event(e.Event)
	if ev != nil {
		found := false
		for _, h := range ev.handlers() {
			if !h.magnum {
				continue
			}
			found = true
			if !h.matches(e) {
				edit := d.replace(h.span, hookHandlerValue(e))
				edit.rewrite, edit.place, edit.newTrust = true, h.place(e.Event), !h.codexSame(e)
				return edit, true
			}
		}
		if found {
			return jsonEdit{}, false
		}
		return d.appendElem(ev.arr, hookGroupValue(e)), true
	}
	if d.hooks != nil {
		return d.appendMember(d.obj, e.Event, jsonArr{hookGroupValue(e)}), true
	}
	return d.appendMember(d.root, "hooks", jsonObj{{e.Event, jsonArr{hookGroupValue(e)}}}), true
}

// removeFirst is the edit that removes the first handler of magnum's: the
// handler alone when its group holds another, else the group, else (the
// event left with no group) the event's key, else (the hooks left with no
// event) the hooks object. False when the hooks hold none of magnum's.
func (d *hooksDoc) removeFirst() (jsonEdit, bool) {
	for _, ev := range d.events {
		for _, g := range ev.groups {
			for _, h := range g.handlers {
				if !h.magnum {
					continue
				}
				switch {
				case len(g.handlers) > 1:
					return removeAt(g.list.open, g.list.close, g.list.elems, h.index), true
				case len(ev.groups) > 1:
					return removeAt(ev.arr.open, ev.arr.close, ev.arr.elems, h.group), true
				case len(d.obj.members) > 1:
					return removeAt(d.obj.open, d.obj.close, memberSpans(d.obj), ev.member), true
				}
				return removeAt(d.root.open, d.root.close, memberSpans(d.root), slices.IndexFunc(d.root.members,
					func(m jsonMember) bool { return m.keyStart == d.hooks.keyStart })), true
			}
		}
	}
	return jsonEdit{}, false
}

// memberSpans are o's members, each from its key to the end of its value.
func memberSpans(o jsonObject) []jsonSpan {
	out := make([]jsonSpan, len(o.members))
	for i, m := range o.members {
		out[i] = jsonSpan{m.keyStart, m.val.end}
	}
	return out
}

// removeAt is the edit that removes element k of the container whose
// brackets are at openAt and closeAt: with the comma and white space after it
// up to the next element, or, the last, from the end of the one before it;
// the only one leaves the brackets empty.
func removeAt(openAt, closeAt int, elems []jsonSpan, k int) jsonEdit {
	switch {
	case len(elems) == 1:
		return jsonEdit{start: openAt + 1, end: closeAt}
	case k < len(elems)-1:
		return jsonEdit{start: elems[k].start, end: elems[k+1].start}
	}
	return jsonEdit{start: elems[k-1].end, end: elems[k].end}
}

// appendElem is the edit that appends v at the end of a: after its last
// element, in the white space that comes before that element, or, a empty,
// on a line of its own.
func (d *hooksDoc) appendElem(a jsonArray, v any) jsonEdit {
	spans := a.elems
	if len(spans) == 0 {
		return d.fillEmpty(a.open, a.close, '[', ']', func(base string) string { return d.style.render(v, base, d.style.inline()) })
	}
	ws, inline := d.lead(a.open, spans)
	base := afterLastLineEnd(ws)
	return jsonEdit{start: spans[len(spans)-1].end, end: spans[len(spans)-1].end, text: "," + ws + d.style.render(v, base, inline)}
}

// appendMember is appendElem for a member key: v of the object o.
func (d *hooksDoc) appendMember(o jsonObject, key string, v any) jsonEdit {
	member := func(base string, inline bool) string {
		return jsonQuote(key) + d.style.colon + d.style.render(v, base, inline)
	}
	spans := memberSpans(o)
	if len(spans) == 0 {
		return d.fillEmpty(o.open, o.close, '{', '}', func(base string) string { return member(base, d.style.inline()) })
	}
	ws, inline := d.lead(o.open, spans)
	base := afterLastLineEnd(ws)
	return jsonEdit{start: spans[len(spans)-1].end, end: spans[len(spans)-1].end, text: "," + ws + member(base, inline)}
}

// lead is the white space before the last of spans (after the comma before
// it, or after the bracket at open) and whether it is on the same line: a
// one-line container, whose only element gets the style's separator.
func (d *hooksDoc) lead(open int, spans []jsonSpan) (string, bool) {
	prev := open + 1
	if n := len(spans); n > 1 {
		prev = spans[n-2].end
	}
	ws := string(d.doc[prev:spans[len(spans)-1].start])
	if i := strings.LastIndexByte(ws, ','); i >= 0 {
		ws = ws[i+1:]
	}
	if strings.Contains(ws, "\n") {
		return ws, false
	}
	if len(spans) == 1 {
		ws = d.style.inlineSep()
	}
	return ws, true
}

// fillEmpty is the edit that puts the text body makes (at the base it is
// given) into the empty container at openAt and closeAt: on a line of its own,
// one step deeper than the container's line, or on the container's line in
// a one-line document.
func (d *hooksDoc) fillEmpty(openAt, closeAt int, l, r byte, body func(base string) string) jsonEdit {
	if d.style.inline() {
		return jsonEdit{start: openAt, end: closeAt + 1, text: string(l) + body("") + string(r)}
	}
	base := lineIndent(d.doc, openAt)
	inner := base + d.style.unit
	return jsonEdit{start: openAt, end: closeAt + 1, text: string(l) + d.style.nl + inner + body(inner) + d.style.nl + base + string(r)}
}

// replace is the edit that writes v over the value at sp, on one line when
// the value was.
func (d *hooksDoc) replace(sp jsonSpan, v any) jsonEdit {
	inline := d.style.inline() || !bytes.ContainsRune(d.doc[sp.start:sp.end], '\n')
	return jsonEdit{start: sp.start, end: sp.end, text: d.style.render(v, lineIndent(d.doc, sp.start), inline)}
}

// afterLastLineEnd is the part of ws after its last line end.
func afterLastLineEnd(ws string) string {
	if _, after, ok := strings.CutLast(ws, "\n"); ok {
		return after
	}
	return ws
}

// lineIndent is the white space that begins the line of offset off.
func lineIndent(doc []byte, off int) string {
	start := bytes.LastIndexByte(doc[:off], '\n') + 1
	end := start
	for end < off && (doc[end] == ' ' || doc[end] == '\t') {
		end++
	}
	return string(doc[start:end])
}

// jsonStyle is how a document is written: its line end ("" for a document
// on one line), one step of its indentation and what stands between a key
// and its value.
type jsonStyle struct {
	nl, unit, colon string
}

// readJSONStyle reads doc's style: CRLF line ends when it has one, the
// indentation of its first indented line (two spaces when none is), and
// the colon of the root's first member (": " when it has none).
func readJSONStyle(doc []byte, root jsonObject) jsonStyle {
	s := jsonStyle{nl: "\n", unit: "  ", colon: ": "}
	if bytes.Contains(doc, []byte("\r\n")) {
		s.nl = "\r\n"
	}
	if len(root.members) > 0 {
		first := root.members[0]
		if !bytes.ContainsRune(doc[root.open:first.keyStart], '\n') {
			s.nl = "" // the root's first member is on its brace's line
		}
		between := doc[first.keyStart:first.val.start]
		if i := bytes.LastIndexByte(between, ':'); i >= 0 && !bytes.ContainsAny(between[i+1:], "\r\n") {
			s.colon = string(between[i:])
		}
	}
	for line := range bytes.SplitSeq(doc, []byte("\n")) {
		n := 0
		for n < len(line) && (line[n] == ' ' || line[n] == '\t') {
			n++
		}
		if n > 0 && n < len(line) && line[n] != '\r' {
			s.unit = string(line[:n])
			break
		}
	}
	return s
}

// inline reports whether the document is on one line.
func (s jsonStyle) inline() bool { return s.nl == "" }

// inlineSep is what follows a comma on one line: a space when the colon
// has one.
func (s jsonStyle) inlineSep() string {
	if strings.HasSuffix(s.colon, " ") {
		return " "
	}
	return ""
}

// jsonObj and jsonArr are values the splice writes: an object with its keys
// in order, and an array. Their leaves are strings, bools and ints.
type (
	jsonObj []jsonKV
	jsonArr []any
	jsonKV  struct {
		k string
		v any
	}
)

// hookHandlerValue is magnum's handler for e; hookGroupValue its group.
func hookHandlerValue(e hookEntry) jsonObj {
	return jsonObj{{"type", "command"}, {"command", e.Command}, {"async", true}, {"timeout", e.Timeout}}
}

func hookGroupValue(e hookEntry) jsonObj { return jsonObj{{"hooks", jsonArr{hookHandlerValue(e)}}} }

// render writes v in s: on one line (inline), or with each member and
// element on a line of its own, one step deeper than base, the line the
// value begins on.
func (s jsonStyle) render(v any, base string, inline bool) string {
	var items []string
	var l, r string
	switch v := v.(type) {
	case jsonObj:
		l, r = "{", "}"
		for _, kv := range v {
			items = append(items, jsonQuote(kv.k)+s.colon+s.render(kv.v, base+s.unit, inline))
		}
	case jsonArr:
		l, r = "[", "]"
		for _, e := range v {
			items = append(items, s.render(e, base+s.unit, inline))
		}
	case string:
		return jsonQuote(v)
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	default:
		panic(fmt.Sprintf("cli: render of %T", v))
	}
	switch {
	case len(items) == 0:
		return l + r
	case inline:
		return l + strings.Join(items, ","+s.inlineSep()) + r
	}
	nl := s.nl
	if nl == "" {
		nl = "\n"
	}
	inner := base + s.unit
	return l + nl + inner + strings.Join(items, ","+nl+inner) + nl + base + r
}

// jsonQuote is s as a JSON string, with <, > and & as they are.
func jsonQuote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return strings.TrimSuffix(b.String(), "\n")
}
