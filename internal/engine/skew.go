package engine

// The skew guard: a prompt edited for a newer build used to reach a daemon
// that could not render it (14 runs on 5 PRs failed that way, in two
// waves). CheckPrompts renders every prompt the configuration names with
// this binary's template data before the daemon starts, again on the
// snapshot the daemon renders rounds from (loadPrompts), and before
// daemon-restart or install hand the configuration to it.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/zhuravel/magnum/internal/agents"
	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/pipeline"
	"github.com/zhuravel/magnum/internal/store"
)

// CheckPrompts renders, with representative data and this binary's
// renderer, every prompt file the configured roles name (judges with
// agents.JudgeData, other session roles with agents.RoleData in each mode,
// shell roles' command or full-line template through agents.ShellLine), the
// model-fallback prompt and the triage prompt. Each template is rendered twice, once with
// every field set and once with the optional ones empty, so both sides of
// an {{if}} run. It returns how many renders passed and every failure,
// joined: a template field this binary's data lacks (a prompt edited for a
// newer build) fails here instead of in a round.
func CheckPrompts(cfg *config.Config) (int, error) {
	var errs []error
	ok := 0
	render := func(what string, p config.Prompt, data any) {
		if _, err := agents.RenderPrompt(p, data); err != nil {
			errs = append(errs, fmt.Errorf("%s: %s: %w", what, promptSource(p), err))
			return
		}
		ok++
	}
	for _, r := range cfg.Roles {
		if r.IsShell() {
			n, err := checkShellRole(cfg, r)
			ok += n
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		seen := map[string]bool{}
		for _, kind := range config.PromptKinds {
			name := r.PromptFile(kind)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			p, err := cfg.RolePrompt(r, kind)
			if err != nil {
				errs = append(errs, fmt.Errorf("role %s: %s prompt: %w", r.Name, kind, err))
				continue
			}
			what := "role " + r.Name + " " + kind + " prompt"
			if r.Judge {
				render(what, p, sampleData[agents.JudgeData](true))
				render(what, p, agents.JudgeData{})
				continue
			}
			for _, mode := range []string{agents.ModeInitial, agents.ModeRereview, agents.ModeRestart} {
				d := sampleData[agents.RoleData](true)
				d.Mode = mode
				render(what, p, d)
			}
			render(what, p, agents.RoleData{})
		}
	}
	if p, err := cfg.ResolvePrompt(agents.FallbackPromptName); err != nil {
		errs = append(errs, fmt.Errorf("model-fallback prompt: %w", err))
	} else {
		render("model-fallback prompt", p, sampleData[agents.FallbackData](true))
		render("model-fallback prompt", p, agents.FallbackData{})
	}
	if p, err := cfg.ResolvePrompt(cfg.Triage.Prompt); err != nil {
		errs = append(errs, fmt.Errorf("triage prompt: %w", err))
	} else {
		d := sampleData[triageData](true)
		for _, kind := range []string{pipeline.KindInitial, pipeline.KindRereview} {
			d.Kind = kind
			render("triage prompt", p, d)
		}
		render("triage prompt", p, triageData{})
	}
	return ok, errors.Join(errs...)
}

// checkShellRole renders a shell role's line (its command, or its
// full-line template) with representative values.
func checkShellRole(cfg *config.Config, r config.Role) (int, error) {
	d := agents.ShellData{
		Title: "PR #1 " + r.Name + " - repo", ReportPath: "/tmp/magnum-check/" + r.ReportFile(),
		Marker: agents.DoneMarker("check"), BaseRef: "origin/main",
		HeadSHA: "0123456789abcdef0123456789abcdef01234567", URL: "https://github.com/example/repo/pull/1",
	}
	what := "role " + r.Name + " command"
	if r.Command == "" && r.Prompt != "" {
		p, err := cfg.RolePrompt(r, config.PromptInitial)
		if err != nil {
			return 0, fmt.Errorf("role %s: initial prompt: %w", r.Name, err)
		}
		d.Template = &p
		what = "role " + r.Name + " initial prompt: " + promptSource(p)
	}
	if _, err := agents.ShellLine(r, d); err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	return 1, nil
}

// promptSource names where a prompt was read from.
func promptSource(p config.Prompt) string {
	if p.Embedded || p.Path == "" {
		return p.Name + " (embedded)"
	}
	return p.Path
}

// sampleData is a T with every exported field set: strings "sample",
// numbers 1, booleans truth, slices two elements (the first with booleans
// true, the second false, so both sides of an {{if}} inside a {{range}}
// run), pointers to such values.
func sampleData[T any](truth bool) T {
	var v T
	fillSample(reflect.ValueOf(&v).Elem(), truth, 0)
	return v
}

func fillSample(v reflect.Value, truth bool, depth int) {
	if depth > 4 || !v.CanSet() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("sample")
	case reflect.Bool:
		v.SetBool(truth)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillSample(v.Field(i), truth, depth+1)
			}
		}
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		fillSample(s.Index(0), true, depth+1)
		fillSample(s.Index(1), false, depth+1)
		v.Set(s)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fillSample(p.Elem(), truth, depth+1)
		v.Set(p)
	}
}

// ClearModelLimits deletes the per-model limits recorded for kind
// (agents.KVModelLimited rows indexed by agents.KVModelLimits), so new
// sessions start on their roles' models again and live ones switch back
// before their next prompt. It returns the models that were limited.
// `magnum resume --tool` runs it (through the daemon, or directly when no
// daemon runs).
func ClearModelLimits(ctx context.Context, st *store.Store, kind string) ([]string, error) {
	idx, ok, err := st.GetKV(ctx, agents.KVModelLimits(kind))
	if err != nil || !ok {
		return nil, err
	}
	var models []string
	for m := range strings.SplitSeq(idx, ",") {
		if m = strings.TrimSpace(m); m == "" || slices.Contains(models, m) {
			continue
		}
		if err := st.DeleteKV(ctx, agents.KVModelLimited(kind, m)); err != nil {
			return models, err
		}
		models = append(models, m)
	}
	return models, st.DeleteKV(ctx, agents.KVModelLimits(kind))
}
