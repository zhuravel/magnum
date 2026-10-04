package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// promptIn reads prompt answers and single keys from one buffered reader
// (stdin), so consecutive prompts of a command never lose input. Reads are
// cancellable: a command's context ends on ctrl+c or SIGTERM (its signal
// handler keeps the default action from killing the process), and a read
// returns ctx.Err() then. A read blocked on a terminal cannot be
// interrupted, so the reader is abandoned and every later read fails at
// once; the command is ending anyway.
type promptIn struct {
	src  io.Reader
	r    *bufio.Reader
	dead error
}

func newPromptIn(src io.Reader) *promptIn {
	if src == nil {
		src = strings.NewReader("")
	}
	return &promptIn{src: src, r: bufio.NewReader(src)}
}

type promptRead struct {
	s   string
	b   byte
	err error
}

// read runs fn on its own goroutine and waits for it or for ctx.
func (p *promptIn) read(ctx context.Context, fn func(*bufio.Reader) promptRead) promptRead {
	if p.dead != nil {
		return promptRead{err: p.dead}
	}
	if err := ctx.Err(); err != nil {
		return promptRead{err: err}
	}
	done := make(chan promptRead, 1)
	go func() { done <- fn(p.r) }()
	select {
	case res := <-done:
		return res
	case <-ctx.Done():
		p.dead = ctx.Err()
		return promptRead{err: p.dead}
	}
}

// line reads one trimmed line; io.EOF only when nothing was typed.
func (p *promptIn) line(ctx context.Context) (string, error) {
	res := p.read(ctx, func(r *bufio.Reader) promptRead {
		s, err := r.ReadString('\n')
		return promptRead{s: s, err: err}
	})
	if res.err != nil && (!errors.Is(res.err, io.EOF) || res.s == "") {
		return "", res.err
	}
	return strings.TrimSpace(res.s), nil
}

// key reads one byte (a key press in cbreak mode).
func (p *promptIn) key(ctx context.Context) (byte, error) {
	res := p.read(ctx, func(r *bufio.Reader) promptRead {
		b, err := r.ReadByte()
		return promptRead{b: b, err: err}
	})
	return res.b, res.err
}

// confirm asks a yes/no question on w; default no (also on EOF, a read
// error or ctrl+c: callers check ctx.Err() to tell the last apart).
func (p *promptIn) confirm(ctx context.Context, w io.Writer, question string) bool {
	fmt.Fprintf(w, "%s [y/N] ", question)
	s, err := p.line(ctx)
	if err != nil {
		fmt.Fprintln(w)
		return false
	}
	s = strings.ToLower(s)
	return s == "y" || s == "yes"
}

// confirmTyped asks the user to type want exactly.
func (p *promptIn) confirmTyped(ctx context.Context, w io.Writer, question, want string) bool {
	fmt.Fprintf(w, "%s\nType %q to confirm: ", question, want)
	s, err := p.line(ctx)
	if err != nil {
		fmt.Fprintln(w)
		return false
	}
	return s == want
}

// ask prints question (with def in brackets when set) on w and reads an
// answer; an empty answer takes def. check validates and normalizes the
// answer: a rejected one prints why and asks again. It returns the read
// error (io.EOF, ctx.Err()) when input ends or the command is cancelled.
func (p *promptIn) ask(ctx context.Context, w io.Writer, question, def string, check func(string) (string, error)) (string, error) {
	for {
		if def != "" {
			fmt.Fprintf(w, "%s [%s]: ", question, def)
		} else {
			fmt.Fprintf(w, "%s: ", question)
		}
		s, err := p.line(ctx)
		if err != nil {
			fmt.Fprintln(w)
			return "", err
		}
		if s == "" {
			s = def
		}
		if s == "" {
			fmt.Fprintln(w, "  an answer is needed")
			continue
		}
		if check == nil {
			return s, nil
		}
		v, err := check(s)
		if err == nil {
			return v, nil
		}
		fmt.Fprintln(w, "  "+err.Error())
	}
}
