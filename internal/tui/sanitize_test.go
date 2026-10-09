package tui

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// escPayload is text a PR, a pane or a command could carry to a terminal:
// a color escape and a bell.
const escPayload = "\x1b[31mX\x07"

// unsanitizedEnums are the string fields the sanitizers leave as they come,
// by their path from the root: none so far. A field belongs here only when
// it is an enum that no screen draws, compared with known values instead.
var unsanitizedEnums = []string{}

// fillStrings sets every string reachable from v to escPayload: exported
// struct fields, through pointers (allocated), slices and maps (given two
// elements and one entry) and arrays.
func fillStrings(v reflect.Value, depth int) {
	if depth > 8 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(escPayload)
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		fillStrings(v.Elem(), depth+1)
	case reflect.Struct:
		for _, f := range v.Fields() {
			if f.CanSet() {
				fillStrings(f, depth+1)
			}
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		for i := range s.Len() {
			fillStrings(s.Index(i), depth+1)
		}
		v.Set(s)
	case reflect.Array:
		for i := range v.Len() {
			fillStrings(v.Index(i), depth+1)
		}
	case reflect.Map:
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fillStrings(k, depth+1)
		fillStrings(e, depth+1)
		m := reflect.MakeMap(v.Type())
		m.SetMapIndex(k, e)
		v.Set(m)
	}
}

// unsafeStrings lists the paths of the strings reachable from v that
// still carry an escape or a control character.
func unsafeStrings(v reflect.Value, path string, out *[]string) {
	switch v.Kind() {
	case reflect.String:
		if strings.ContainsFunc(v.String(), isControl) && !slices.Contains(unsanitizedEnums, path) {
			*out = append(*out, path)
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			unsafeStrings(v.Elem(), path, out)
		}
	case reflect.Struct:
		for f, fv := range v.Fields() {
			if f.IsExported() {
				unsafeStrings(fv, path+"."+f.Name, out)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			unsafeStrings(v.Index(i), path+"[]", out)
		}
	case reflect.Map:
		for it := v.MapRange(); it.Next(); {
			unsafeStrings(it.Key(), path+"{key}", out)
			unsafeStrings(it.Value(), path+"{}", out)
		}
	}
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) }

// Every string a board row, the status data or a picker entry carries,
// nested ones included, is drawn safe: with an escape and a bell in each,
// nothing the sanitizers return keeps an escape or a control character. A
// field added to them later is covered without a list to keep.
func TestSanitizersCleanEveryStringField(t *testing.T) {
	var row PRBoardRow
	fillStrings(reflect.ValueOf(&row).Elem(), 0)
	var status StatusData
	fillStrings(reflect.ValueOf(&status).Elem(), 0)
	var pick PickEntry
	fillStrings(reflect.ValueOf(&pick).Elem(), 0)
	if row.Findings == nil || row.Findings.Verdict != escPayload || len(status.Slots) == 0 || pick.Review == nil {
		t.Fatal("the payload did not reach the nested fields")
	}
	for _, c := range []struct {
		name string
		v    any
	}{{"PRBoardRow", sanitizeRow(row)}, {"StatusData", sanitizeStatus(status)}, {"PickEntry", sanitizePicks([]PickEntry{pick})}} {
		var bad []string
		unsafeStrings(reflect.ValueOf(c.v), c.name, &bad)
		for _, p := range bad {
			t.Errorf("%s keeps an escape or a control character", p)
		}
	}
}

// termPayload is command output that would act on the terminal if drawn:
// a clipboard write (OSC 52), a link (OSC 8), a screen erase and a bell.
const termPayload = "\x1b]52;c;aGk=\x07\x1b]8;;https://example.com\x1b\\link\x1b]8;;\x1b\\\x1b[2J\x07\x08"

// mustDrawSafely fails when frame carries any of termPayload's sequences or
// a control character other than the frame's newlines and its styling.
func mustDrawSafely(t *testing.T, what, frame string) {
	t.Helper()
	for _, bad := range []string{"\x1b]", "\x1b[2J", "\x07", "\x08", "\x1b\\"} {
		if strings.Contains(frame, bad) {
			t.Errorf("%s draws %q:\n%q", what, bad, frame)
		}
	}
}

// The footer's flashes and failures and the screens' load and save errors
// draw command output without its escape sequences and control
// characters.
func TestFootersAndErrorLinesDrawNoTerminalSequences(t *testing.T) {
	boom := errors.New("refresh " + termPayload)
	m, _, _ := newBoard(t, 120, 30, PRBoardOptions{})
	m, _ = send(t, m, actionDoneMsg{what: "pin", text: "pinned " + termPayload})
	mustDrawSafely(t, "the board's flash", m.View().Content)
	m, _ = send(t, m, actionDoneMsg{what: "pin", err: errors.New("failed " + termPayload)})
	mustDrawSafely(t, "the board's failure", m.View().Content)
	m, _ = send(t, m, keyMsg("j"), prbDataMsg{err: boom})
	mustDrawSafely(t, "the board's refresh error", m.View().Content)
	m, _ = send(t, m, keyMsg("j"), widthsSavedMsg{err: boom})
	mustDrawSafely(t, "the board's widths error", m.View().Content)
	if bar(m).flash == "" || strings.ContainsFunc(bar(m).flash, isControl) {
		t.Errorf("the board's flash %q", bar(m).flash)
	}
	b := testPRBoard(context.Background(), &fakeBoardSource{}, nil, PRBoardOptions{Now: func() time.Time { return boardNow }})
	b, _ = send(t, b, tea.WindowSizeMsg{Width: 120, Height: 30}, prbDataMsg{err: boom})
	mustDrawSafely(t, "the board's load error", b.View().Content)

	d, _, _ := newDash(t, 120, 30)
	d, _ = send(t, d, actionDoneMsg{what: "pin", text: "pinned " + termPayload})
	mustDrawSafely(t, "the dashboard's flash", d.View().Content)
	d, _ = send(t, d, actionDoneMsg{what: "pin", err: errors.New("failed " + termPayload)})
	mustDrawSafely(t, "the dashboard's failure", d.View().Content)
	d, _ = send(t, d, keyMsg("j"), dashDataMsg{err: boom})
	mustDrawSafely(t, "the dashboard's refresh error", d.View().Content)
	d, _ = send(t, d, keyMsg("j"), widthsLoadedMsg{err: boom})
	mustDrawSafely(t, "the dashboard's widths error", d.View().Content)
	e := testDashboard(context.Background(), &fakeSource{}, nil, DashboardOptions{Now: func() time.Time { return dashNow }})
	e, _ = send(t, e, tea.WindowSizeMsg{Width: 120, Height: 30}, dashDataMsg{err: boom})
	mustDrawSafely(t, "the dashboard's load error", e.View().Content)

	p := newPicker(t, "", 120, 24)
	p, _ = send(t, p, pickReloadMsg{err: boom})
	mustDrawSafely(t, "the picker's refresh error", p.View().Content)
}
