package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
)

// redisRepos are three [[factory.repo]]s: talkable/talkable, whose copies
// take REDIS_DB_OFFSET on the Redis its REDIS_URL names (a password in it),
// 3 task copies and 2 fix copies; example/cache, whose REDIS_URL takes the
// copy's offset as {n*16} on REDIS_HOST and REDIS_PORT's Redis, 1 copy; and
// example/shop, which uses no Redis.
func redisRepos() []config.FactoryRepo {
	return []config.FactoryRepo{
		{Repo: "talkable/talkable", Copies: "/p/talkable.factory{n}", CopyName: "factory{n}", MaxCopies: 3, FixCopies: 2,
			Env: map[string]string{"REDIS_DB_OFFSET": "0", "REDIS_URL": "redis://:s3cret@127.0.0.1:6380/0"}},
		{Repo: "example/cache", Copies: "/p/cache{n}", CopyName: "cache{n}", MaxCopies: 1,
			Env: map[string]string{"REDIS_HOST": "127.0.0.1", "REDIS_PORT": "6381", "CACHE_DB": "{n*16}"}},
		{Repo: "example/shop", Copies: "/p/shop{n}", CopyName: "shop{n}", MaxCopies: 2, FixCopies: 1,
			Env: map[string]string{"PORT": "30{n}0"}},
	}
}

// Doctor reads how many databases the Redis of each [[factory.repo]] whose
// copies use Redis offsets has (CONFIG GET databases, through the runner),
// and fails when it is fewer than the highest copy's offset plus 16: copy
// n's 16 start at n × 16, and the fix copies are numbered past the tasks',
// so talkable's 3 task copies and 2 fix copies need 96, and a Redis with 64
// refuses the flush of copies 4 and 5. A Redis that does not answer is
// unknown, a warning, never a failure; a repository without Redis offsets
// gets no check, and no repository with them is a skip.
func TestDoctorSaysWhenRedisHasTooFewDatabasesForTheCopies(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	fake := d.Run.(*execx.Fake)
	base := fake.Rules
	cfg := *d.Config
	cfg.Factory.Repos = redisRepos()
	d.Config = &cfg
	redis := func(port, out string) execx.Rule {
		return execx.Rule{Prefix: []string{"redis-cli", "-h", "127.0.0.1", "-p", port}, Result: execx.Result{Stdout: []byte(out)}}
	}
	checks := func() map[string]doctorCheck {
		t.Helper()
		m := map[string]doctorCheck{}
		for _, c := range doctorRun(context.Background(), d) {
			if c.Name == "redis" || strings.HasPrefix(c.Name, "redis ") {
				m[c.Name] = c
			}
		}
		return m
	}

	fake.Rules = append([]execx.Rule{redis("6380", "databases\n64\n"), redis("6381", "databases\n64\n")}, base...)
	m := checks()
	if len(m) != 2 {
		t.Fatalf("redis checks = %+v, want one each for talkable/talkable and example/cache", m)
	}
	tk := m["redis talkable/talkable"]
	if tk.Status != doctorFail || !strings.Contains(tk.Detail, "talkable/talkable's copies need 96 Redis databases") ||
		!strings.Contains(tk.Detail, "the Redis at 127.0.0.1:6380 has 64") || !strings.Contains(tk.Fix, "databases 96") {
		t.Fatalf("talkable on a Redis with 64 = %+v, want a FAIL naming 96 needed and 64 there", tk)
	}
	if c := m["redis example/cache"]; c.Status != doctorPass || !strings.Contains(c.Detail, "example/cache's copies need 32 Redis databases") ||
		!strings.Contains(c.Detail, "127.0.0.1:6381 has 64") {
		t.Fatalf("example/cache on a Redis with 64 = %+v, want a PASS naming 32 needed and 64 there", c)
	}
	calls := fake.CallsWithPrefix("redis-cli", "-h", "127.0.0.1", "-p", "6380")
	if len(calls) != 1 || !slices.Equal(calls[0].Args[len(calls[0].Args)-3:], []string{"CONFIG", "GET", "databases"}) ||
		calls[0].Env["REDISCLI_AUTH"] != "s3cret" || slices.Contains(calls[0].Args, "s3cret") || calls[0].Mutates {
		t.Fatalf("the probe of talkable's Redis = %+v, want one read of CONFIG GET databases, its password in REDISCLI_AUTH only", calls)
	}

	fake.Rules = append([]execx.Rule{redis("6380", "databases\n96\n"), redis("6381", "databases\n16\n")}, base...)
	m = checks()
	if c := m["redis talkable/talkable"]; c.Status != doctorPass || !strings.Contains(c.Detail, "has 96") {
		t.Fatalf("talkable on a Redis with 96 = %+v, want a PASS", c)
	}
	if c := m["redis example/cache"]; c.Status != doctorFail || !strings.Contains(c.Detail, "need 32") || !strings.Contains(c.Detail, "has 16") {
		t.Fatalf("example/cache on a Redis with 16 = %+v, want a FAIL", c)
	}

	fake.Rules = append([]execx.Rule{
		{Prefix: []string{"redis-cli", "-h", "127.0.0.1", "-p", "6380"}, Result: execx.Result{Code: 1, Stderr: []byte("Could not connect to Redis at 127.0.0.1:6380: Connection refused\n")}},
		{Prefix: []string{"redis-cli", "-h", "127.0.0.1", "-p", "6381"}, Err: errors.New(`exec: "redis-cli": executable file not found in $PATH`)},
	}, base...)
	m = checks()
	for name, c := range m {
		if c.Status != doctorWarn || !strings.Contains(c.Detail, "need") || c.Status == doctorFail {
			t.Fatalf("%s with a Redis that does not answer = %+v, want a WARN (unknown), never a FAIL", name, c)
		}
	}
	if c := m["redis talkable/talkable"]; !strings.Contains(c.Detail, "96") || !strings.Contains(c.Detail, "Connection refused") {
		t.Fatalf("talkable with its Redis down = %+v, want the 96 it needs and why it is unknown", c)
	}

	cfg.Factory.Repos = redisRepos()[2:]
	before := len(fake.CallsWithPrefix("redis-cli"))
	m = checks()
	if c, ok := m["redis"]; len(m) != 1 || !ok || c.Status != doctorSkip {
		t.Fatalf("redis checks without a repository on Redis = %+v, want one SKIP", m)
	}
	if n := len(fake.CallsWithPrefix("redis-cli")); n != before {
		t.Fatalf("doctor ran redis-cli %d times with no repository on Redis", n-before)
	}
}

// `magnum config` names, for each [[factory.repo]] whose copies use Redis
// offsets, how many Redis databases its copies need: it cannot read Redis,
// so it states the number, (max_copies + fix_copies + 1) × 16.
func TestConfigCheckNamesTheRedisDatabasesTheCopiesNeed(t *testing.T) {
	f := newInspFixture(t)
	file := filepath.Join(f.Home, "config.toml") // its watches cover talkable/talkable and zhuravel/*
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, `
[[factory.repo]]
repo = "talkable/talkable"
main_clone = "/p/talkable"
copies = "/p/talkable.factory{n}"
max_copies = 3
fix_copies = 2
env = { REDIS_DB_OFFSET = "0", REDIS_URL = "redis://127.0.0.1:6380/0" }

[[factory.repo]]
repo = "zhuravel/cache"
main_clone = "/p/cache"
copies = "/p/cache{n}"
copy_name = "cache{n}"
max_copies = 1
env = { REDIS_HOST = "127.0.0.1", CACHE_DB = "{n*16}" }

[[factory.repo]]
repo = "zhuravel/shop"
main_clone = "/p/shop"
copies = "/p/shop{n}"
copy_name = "shop{n}"
max_copies = 2
`...)
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Context{Version: "test", Layout: f.Ctx.Layout, Stdout: f.Out, Stderr: f.Err}
	if code := execute(c, []string{"config"}); code != 0 {
		t.Fatalf("exit %d: %s", code, f.Err.String())
	}
	out := f.Out.String()
	for _, want := range []string{
		"redis:   talkable/talkable's copies need 96 Redis databases",
		"redis:   zhuravel/cache's copies need 32 Redis databases",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "zhuravel/shop") {
		t.Errorf("stdout names zhuravel/shop, whose copies use no Redis:\n%s", out)
	}
}
