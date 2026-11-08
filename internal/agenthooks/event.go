package agenthooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Bounds of the fields Parse keeps: an id longer than idMax is no id, and
// the free text is cut.
const (
	idMax    = 128
	pathMax  = 1024
	errorMax = 256
	shortMax = 64
	// errorFit is what fit cuts the error to before it drops it.
	errorFit = 64
)

// Parse builds the Event of hook event name of agent from its payload (the
// JSON the agent wrote to the hook's stdin, ReadPayload) and its env
// (getenv: EnvPane, EnvCodexThread). It keeps only session_id,
// transcript_path, turn_id, source, error, tool_name, agent_id and the
// number of background_tasks; every other field (the last assistant
// message, a prompt, a tool's input, the cwd) is dropped unread. A payload
// cut short or that is no JSON keeps the fields read before the break; a
// value of another type than its field's is no value. An id that is too
// long or holds a character no id has (a space, a quote, a backslash, <, >,
// &, anything outside printable ASCII) is left out whole, the free text
// (the path, the error, the tool, the source) loses its control characters
// and is cut, and the event always fits one datagram (fit). An unknown agent
// or event, and a hook outside a herdr pane (no valid HERDR_PANE_ID), are
// errors: there is nothing to send.
func Parse(agent, name string, payload []byte, getenv func(string) string) (Event, error) {
	if !knownAgent(agent) {
		return Event{}, fmt.Errorf("agenthooks: unknown agent %q", agent)
	}
	if !knownName(name) {
		return Event{}, fmt.Errorf("agenthooks: unknown event %q", name)
	}
	pane := hookID(getenv(EnvPane))
	if pane == "" {
		return Event{}, errors.New("agenthooks: not in a herdr pane (no " + EnvPane + ")")
	}
	ev := Event{V: Version, Agent: agent, Name: name, Pane: pane, CodexThread: hookID(getenv(EnvCodexThread))}
	readFields(payload, &ev)
	fit(&ev)
	return ev, nil
}

// readFields reads payload's top-level fields into ev, one at a time, until
// the payload ends or breaks.
func readFields(payload []byte, ev *Event) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return
		}
		key, _ := t.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return
		}
		switch key {
		case "session_id":
			ev.SessionID = hookID(jsonString(raw))
		case "turn_id":
			ev.TurnID = hookID(jsonString(raw))
		case "agent_id":
			ev.AgentID = hookID(jsonString(raw))
		case "transcript_path":
			ev.TranscriptPath = freeText(jsonString(raw), pathMax)
		case "source":
			ev.Source = freeText(jsonString(raw), shortMax)
		case "error":
			ev.Error = freeText(jsonString(raw), errorMax)
		case "tool_name":
			ev.ToolName = freeText(jsonString(raw), shortMax)
		case "background_tasks":
			var tasks []json.RawMessage
			if json.Unmarshal(raw, &tasks) == nil {
				ev.Background = len(tasks)
			}
		}
	}
}

// jsonString is raw as a string, "" when it is another type.
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// hookID is s when it may be an id (a session's, a turn's, a pane's): 1 to
// idMax bytes of printable ASCII without a space, a quote, a backslash, <,
// > or &; "" otherwise.
func hookID(s string) string {
	if s == "" || len(s) > idMax {
		return ""
	}
	for i := range len(s) {
		if c := s[i]; c <= ' ' || c > '~' || strings.IndexByte(`"\<>&`, c) >= 0 {
			return ""
		}
	}
	return s
}

// freeText is s, valid UTF-8 without control characters, cut to at most
// limit bytes on a character's boundary.
func freeText(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	return clip(s, limit)
}

// clip cuts s to at most limit bytes on a character's boundary.
func clip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// fit makes ev's JSON fit one datagram: it drops the transcript's path,
// then cuts the error to errorFit bytes, then drops the error, the tool and
// the source. The ids alone (each at most idMax bytes, which JSON never
// escapes) always fit.
func fit(ev *Event) {
	for _, cut := range []func(){
		func() { ev.TranscriptPath = "" },
		func() { ev.Error = clip(ev.Error, errorFit) },
		func() { ev.Error, ev.ToolName, ev.Source = "", "", "" },
	} {
		if b, err := json.Marshal(ev); err == nil && len(b) <= MaxDatagram {
			return
		}
		cut()
	}
}

// ReadPayload reads the hook's payload from r (its stdin): at most
// MaxPayload bytes, and only what came within ReadTimeout. An agent that
// keeps its end of stdin open costs the hook ReadTimeout, no more; what was
// read by then is the payload (Parse keeps the fields before a cut).
func ReadPayload(r io.Reader) []byte {
	var (
		mu  sync.Mutex
		buf []byte
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		lr := io.LimitReader(r, MaxPayload)
		chunk := make([]byte, payloadChunk)
		for {
			n, err := lr.Read(chunk)
			mu.Lock()
			buf = append(buf, chunk[:n]...)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	deadline := time.NewTimer(ReadTimeout)
	defer deadline.Stop()
	select {
	case <-done:
	case <-deadline.C:
	}
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(buf) // a reader past the deadline may still append
}

// payloadChunk is how much ReadPayload asks of r at a time.
const payloadChunk = 16 << 10
