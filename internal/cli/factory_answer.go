package cli

// `magnum factory answer`: your answers to a factory task's questions,
// through the factory daemon's requests table as the screen sends them:
// a batch from a JSON file (for tests and scripts), or the answer to one
// question from a file (the answer of someone you asked); without either,
// the task's questions as they stand.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/zhuravel/magnum/internal/factory"
	"github.com/zhuravel/magnum/internal/factory/registry"
	"github.com/zhuravel/magnum/internal/fsx"
)

const factoryAnswerUsage = "answer <task> [--json <file> | [--revision <r>] --question <n> --text <file>]"

// factoryAnswerFlags are the flags of `magnum factory answer`.
type factoryAnswerFlags struct {
	json, text         string
	question, revision int
}

func newFactoryAnswerCmd(c *Context) *cobra.Command {
	var f factoryAnswerFlags
	cmd := newCommand("", factoryAnswerUsage, "answer a factory task's questions",
		"Answer the questions a factory task waits on, as the screen does: the daemon stores them, writes them to "+
			"answers.md in the task's directory (the agents read it as data) and moves the task on. <task> is the "+
			"task's id or its Jira key. Without a flag it prints the questions of the task's newest revision, "+
			"numbered from 1, with their options and the recommended one. --json <file> sends a batch: "+
			`{"revision": 3, "answers": {"1": "Seven days before it expires", "2": "ask:rev-ann"}, "needless": {"3": "obvious"}}`+
			", the questions by their numbers; an answer is an option's label or your own words, \"ask:<login>\" "+
			"asks someone else (the question stays open, the task waits on them, and the comment drafted for them "+
			"is in the task's directory), and needless flags a question with the key of your reason. --question <n> "+
			"--text <file> answers question n with the file's text (- for stdin), such as the answer of someone you "+
			"asked: question n of --revision r, the revision that asked it (the daemon's answer to an ask names "+
			"both), wherever a newer revision carried it; without --revision, of the newest revision. A batch for "+
			"questions that changed since is refused, and nothing of it is kept.",
		func(pos []string) int { return runFactoryAnswerCmd(c, f, pos) })
	cmd.Flags().StringVar(&f.json, "json", "", "a JSON file of answers to send (- for stdin)")
	cmd.Flags().IntVar(&f.question, "question", 0, "the number of the question --text answers (from 1)")
	cmd.Flags().IntVar(&f.revision, "revision", 0, "the revision whose numbering --question uses (default: the newest)")
	cmd.Flags().StringVar(&f.text, "text", "", "the file whose text answers --question (- for stdin)")
	return cmd
}

// factoryAnswerFile is the --json file: the questions by number, from 1.
type factoryAnswerFile struct {
	Revision int            `json:"revision"`
	Answers  map[int]string `json:"answers"`
	Needless map[int]string `json:"needless"`
}

func runFactoryAnswerCmd(c *Context, f factoryAnswerFlags, pos []string) int {
	switch {
	case len(pos) != 1:
		return inspUsage(c, "factory answer", "which task? its id or its Jira key", factoryAnswerUsage)
	case f.json != "" && (f.question != 0 || f.text != "" || f.revision != 0):
		return inspUsage(c, "factory answer", "--json sends a batch (its revision in the file); --question and --text answer one "+
			"question: not both", factoryAnswerUsage)
	case (f.question != 0) != (f.text != ""):
		return inspUsage(c, "factory answer", "--question and --text go together", factoryAnswerUsage)
	case f.revision != 0 && f.question == 0:
		return inspUsage(c, "factory answer", "--revision names the revision of --question", factoryAnswerUsage)
	case f.question < 0 || f.revision < 0:
		return inspUsage(c, "factory answer", "--question and --revision count from 1", factoryAnswerUsage)
	}
	if !fsx.Exists(c.Layout.FactoryDB()) {
		fmt.Fprintln(c.Stderr, "magnum factory answer: the factory has no task yet (`magnum factory start` makes one)")
		return 1
	}
	reg, err := openFactoryRegistry(c.Layout)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory answer: %v\n", err)
		return 1
	}
	defer reg.Close()
	ctx, stop := daemonSys.signals()
	defer stop()
	t, err := factoryTaskOf(ctx, reg, pos[0])
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory answer: %v\n", err)
		return 1
	}
	p := factory.AnswerPayload{TaskID: t.ID, Revision: t.QuestionRev, Surface: "cli"}
	switch {
	case f.json != "":
		file, err := readAnswerFile(f.json)
		if err != nil {
			fmt.Fprintf(c.Stderr, "magnum factory answer: %s: %v\n", f.json, err)
			return 1
		}
		p.Revision = file.Revision
		if p.Answers, err = byIndex(file.Answers); err == nil {
			p.Needless, err = byIndex(file.Needless)
		}
		if err != nil {
			fmt.Fprintf(c.Stderr, "magnum factory answer: %s: %v\n", f.json, err)
			return 1
		}
	case f.text != "":
		text, err := readArgFile(f.text)
		if err != nil {
			fmt.Fprintf(c.Stderr, "magnum factory answer: %v\n", err)
			return 1
		}
		idx, err := questionNow(ctx, reg, t, f.revision, f.question)
		if err != nil {
			fmt.Fprintf(c.Stderr, "magnum factory answer: %v\n", err)
			return 1
		}
		p.Answers = map[int]string{idx: strings.TrimSpace(string(text))}
	default:
		return printFactoryQuestions(ctx, c, reg, t)
	}
	return sendFactoryRequest(ctx, c, reg, "factory answer", factory.ReqAnswer, p, "it takes the answers")
}

// questionNow is the index in task t's newest revision of question n (from
// 1) of revision rev (0 = the newest): a newer revision puts the open
// questions it carries over first, so the same question may hold another
// number there; it is found by the role that raised it and its text. A
// question open in no newer revision is refused.
func questionNow(ctx context.Context, reg *registry.Registry, t registry.Task, rev, n int) (int, error) {
	if rev == 0 || rev == t.QuestionRev {
		return n - 1, nil
	}
	all, err := reg.Questions(ctx, t.ID)
	if err != nil {
		return 0, err
	}
	i := slices.IndexFunc(all, func(q registry.Question) bool { return q.Revision == rev && q.Index == n-1 })
	if i < 0 {
		return 0, fmt.Errorf("revision %d of %s has no question %d", rev, factory.TaskKey(t), n)
	}
	asked := all[i]
	for _, q := range all {
		if q.Revision == t.QuestionRev && q.State == registry.QuestionOpen && q.RaisedBy == asked.RaisedBy &&
			strings.TrimSpace(q.Text) == strings.TrimSpace(asked.Text) {
			return q.Index, nil
		}
	}
	return 0, fmt.Errorf("question %d of revision %d of %s is no longer open (`magnum factory answer %d` lists the questions)",
		n, rev, factory.TaskKey(t), t.ID)
}

// factoryTaskOf is the task an argument names: its id, else its Jira key.
func factoryTaskOf(ctx context.Context, reg *registry.Registry, arg string) (registry.Task, error) {
	if id, err := strconv.ParseInt(strings.TrimPrefix(arg, "task "), 10, 64); err == nil {
		return reg.TaskByID(ctx, id)
	}
	t, err := reg.TaskByKey(ctx, strings.ToUpper(strings.TrimSpace(arg)))
	if errors.Is(err, registry.ErrNotFound) {
		return t, fmt.Errorf("no factory task holds %s (`magnum factory status` lists them)", arg)
	}
	return t, err
}

// readAnswerFile reads the --json file, refusing a key it does not know.
func readAnswerFile(name string) (factoryAnswerFile, error) {
	var f factoryAnswerFile
	b, err := readArgFile(name)
	if err != nil {
		return f, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, err
	}
	if f.Revision < 1 {
		return f, errors.New(`it needs the "revision" of the questions it answers (` + "`magnum factory answer <task>` prints it)")
	}
	return f, nil
}

// readArgFile reads a file a flag names, - for stdin.
func readArgFile(name string) ([]byte, error) {
	if name == "-" {
		return io.ReadAll(inspStdin)
	}
	return os.ReadFile(name)
}

// byIndex turns questions numbered from 1 into the request's indexes, from 0.
func byIndex(m map[int]string) (map[int]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[int]string, len(m))
	for n, v := range m {
		if n < 1 {
			return nil, fmt.Errorf("question %d: the questions count from 1", n)
		}
		out[n-1] = v
	}
	return out, nil
}

// printFactoryQuestions prints the questions of task t's newest revision.
func printFactoryQuestions(ctx context.Context, c *Context, reg *registry.Registry, t registry.Task) int {
	all, err := reg.Questions(ctx, t.ID)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum factory answer: %v\n", err)
		return 1
	}
	qs := slices.DeleteFunc(all, func(q registry.Question) bool { return q.Revision != t.QuestionRev })
	if len(qs) == 0 {
		fmt.Fprintf(c.Stdout, "%s asks no question\n", factory.TaskKey(t))
		return 0
	}
	open := 0
	for _, q := range qs {
		if q.State == registry.QuestionOpen {
			open++
		}
	}
	fmt.Fprintf(c.Stdout, "%s · revision %d, asked %s · %d of %d open\n", factory.TaskKey(t), t.QuestionRev,
		qs[0].Asked.Local().Format("Jan 2 15:04"), open, len(qs))
	for _, q := range qs {
		state := q.State
		if q.State == registry.QuestionAnswered {
			state = "answered: " + q.Answer
		}
		fmt.Fprintf(c.Stdout, "\n%d  %s  [%s, asked by %s]\n", q.Index+1, q.Text, state, q.RaisedBy)
		if q.State != registry.QuestionOpen {
			continue
		}
		for _, line := range []string{q.Context, exampleLine(q.Example)} {
			if line != "" {
				fmt.Fprintf(c.Stdout, "   %s\n", line)
			}
		}
		for i, o := range q.Options {
			mark := ""
			if i == q.Recommended {
				mark = "  (recommended: " + q.Why + ")"
			}
			fmt.Fprintf(c.Stdout, "   %c %s%s%s\n", 'a'+rune(i), o.Label, means(o.Means), mark)
		}
		if q.Recommended < 0 && q.Why != "" {
			fmt.Fprintf(c.Stdout, "   no recommendation: %s\n", q.Why)
		}
	}
	fmt.Fprintf(c.Stdout, "\nanswer with `magnum factory answer %d --json <file>` (see --help)\n", t.ID)
	return 0
}

func exampleLine(s string) string {
	if s == "" {
		return ""
	}
	return "example: " + s
}

func means(s string) string {
	if s == "" {
		return ""
	}
	return " → " + s
}

// sendFactoryRequest queues a request of kind with payload for the factory
// daemon, wakes it and prints its answer, as name; does says what the
// daemon does with it once one runs, for the message when none runs.
func sendFactoryRequest(ctx context.Context, c *Context, reg *registry.Registry, name, kind string, payload any, does string) int {
	id, err := reg.EnqueueRequest(ctx, kind, payload)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum %s: %v\n", name, err)
		return 1
	}
	pid, kerr := daemonSys.factoryKick(c.Layout)
	if kerr != nil {
		fmt.Fprintf(c.Stderr, "magnum %s: could not wake the factory daemon: %v (it picks request %d up on its next tick)\n", name, kerr, id)
	}
	if pid == 0 {
		fmt.Fprintf(c.Stderr, "magnum %s: queued as request %d, but no factory daemon runs: `magnum factory install` "+
			"loads it, and %s if it runs within 30 minutes (a request expires then)\n", name, id, does)
		return 0
	}
	q, err := waitFactoryRequest(ctx, reg, id)
	if err != nil {
		fmt.Fprintf(c.Stderr, "magnum %s: %v\n", name, err)
		return 1
	}
	switch q.State {
	case registry.RequestDone:
		fmt.Fprintln(c.Stdout, q.Result)
		return 0
	case registry.RequestFailed:
		fmt.Fprintf(c.Stderr, "magnum %s: %s\n", name, q.Result)
		return 1
	}
	fmt.Fprintf(c.Stdout, "the factory daemon (pid %d) has not answered request %d yet: `magnum factory status` shows the task\n", pid, id)
	return 0
}
