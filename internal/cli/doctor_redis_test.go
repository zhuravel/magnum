package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhuravel/magnum/internal/config"
	"github.com/zhuravel/magnum/internal/execx"
	"github.com/zhuravel/magnum/internal/store"
	"github.com/zhuravel/magnum/internal/store/storetest"
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
		{Prefix: []string{"redis-cli", "-h", "127.0.0.1", "-p", "6381"}, Err: &execx.RunError{Cmd: execx.Cmd{Name: "redis-cli"}, Err: context.DeadlineExceeded}},
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

// Doctor's Redis need counts the copies the factory already made, from the
// pool slots of its agents store, not the configuration alone: a lower
// max_copies or fix_copies retires no copy, and a task's claim takes one
// past the range, so talkable's copies 4 and 5, made while it had 2 fix
// copies, still need 96 databases once fix_copies is 0. It gave a PASS on
// 64 then, while copies 4 and 5 failed their flush at every claim (C8 on
// T3b). A per-PR checkout and another repository's copy do not count.
func TestDoctorCountsTheCopiesAlreadyMadeForTheRedisDatabases(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	fake := d.Run.(*execx.Fake)
	cfg := *d.Config
	cfg.Factory.Repos = redisRepos()[:2]
	cfg.Factory.Repos[0].FixCopies = 0 // lowered after copies 4 and 5 were made
	d.Config = &cfg
	fake.Rules = append([]execx.Rule{
		{Prefix: []string{"redis-cli", "-h", "127.0.0.1"}, Result: execx.Result{Stdout: []byte("databases\n64\n")}},
	}, fake.Rules...)
	st := storetest.OpenFile(t, d.Layout.FactoryAgentsDB())
	for _, s := range []store.Slot{
		{Name: "factory1", RepoFullName: "talkable/talkable", Kind: store.SlotKindPool, Path: "/p/talkable.factory1"},
		{Name: "factory4", RepoFullName: "talkable/talkable", Kind: store.SlotKindPool, Path: "/p/talkable.factory4"},
		{Name: "factory5", RepoFullName: "talkable/talkable", Kind: store.SlotKindPool, Path: "/p/talkable.factory5"},
		{Name: "fix-talkable/talkable#7", RepoFullName: "talkable/talkable", Kind: store.SlotKindPerPR, Path: "/p/checkouts/pr-7"},
		{Name: "cache9", RepoFullName: "example/other", Kind: store.SlotKindPool, Path: "/p/other9"},
	} {
		s.MainClone, s.State = "/p/main", store.SlotFree
		if _, err := st.CreateSlot(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	m := map[string]doctorCheck{}
	for _, c := range doctorRun(context.Background(), d) {
		m[c.Name] = c
	}
	tk := m["redis talkable/talkable"]
	if tk.Status != doctorFail || !strings.Contains(tk.Detail, "talkable/talkable's copies need 96 Redis databases") ||
		!strings.Contains(tk.Detail, "copies 4 to 5 exist past max_copies + fix_copies") || !strings.Contains(tk.Fix, "databases 96") ||
		strings.Contains(tk.Fix, "lower") {
		t.Fatalf("talkable with copies 4 and 5 made and fix_copies 0, on a Redis with 64 = %+v, want a FAIL naming 96 and the copies made, never lowering the keys as its fix", tk)
	}
	if c := m["redis example/cache"]; c.Status != doctorPass || !strings.Contains(c.Detail, "example/cache's copies need 32 Redis databases") {
		t.Fatalf("example/cache, whose cache9 is another repository's = %+v, want a PASS naming 32", c)
	}
}

// The Redis FAIL offers a lower max_copies or fix_copies only while no copy
// past the Redis's databases exists: talkable's copies 4 and 5, made while
// max_copies + fix_copies is still 5, stay whatever the keys say, so
// lowering them would give a PASS on the next run while their flush fails.
// It offered lowering whenever no copy was made past the keys (C3 of T3b's
// third check). With copies made only up to 3, lowering still helps.
func TestDoctorOffersLowerKeysOnlyWhileNoCopyPastTheRedisExists(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	fake := d.Run.(*execx.Fake)
	cfg := *d.Config
	cfg.Factory.Repos = redisRepos()[:1] // max_copies 3, fix_copies 2
	d.Config = &cfg
	fake.Rules = append([]execx.Rule{
		{Prefix: []string{"redis-cli", "-h", "127.0.0.1"}, Result: execx.Result{Stdout: []byte("databases\n64\n")}},
	}, fake.Rules...)
	st := storetest.OpenFile(t, d.Layout.FactoryAgentsDB())
	made := func(names ...string) {
		t.Helper()
		for _, n := range names {
			if _, err := st.CreateSlot(context.Background(), store.Slot{Name: n, RepoFullName: "talkable/talkable", Kind: store.SlotKindPool,
				Path: "/p/talkable." + n, MainClone: "/p/main", State: store.SlotFree}); err != nil {
				t.Fatal(err)
			}
		}
	}
	check := func() doctorCheck {
		t.Helper()
		for _, c := range doctorRun(context.Background(), d) {
			if c.Name == "redis talkable/talkable" {
				return c
			}
		}
		t.Fatal("no redis check of talkable/talkable")
		return doctorCheck{}
	}

	made("factory1", "factory2", "factory3")
	if c := check(); c.Status != doctorFail || !strings.Contains(c.Fix, "databases 96") || !strings.Contains(c.Fix, "or lower talkable/talkable's max_copies or fix_copies") {
		t.Fatalf("talkable with copies 1 to 3 made, on a Redis with 64 = %+v, want a FAIL that offers lowering the keys", c)
	}
	made("factory4", "factory5")
	c := check()
	if c.Status != doctorFail || !strings.Contains(c.Fix, "databases 96") || strings.Contains(c.Fix, "lower") ||
		!strings.Contains(c.Fix, "copies 4 to 5 exist already") {
		t.Fatalf("talkable with copies 4 and 5 made, on a Redis with 64 = %+v, want a FAIL naming the copies that exist, never lowering the keys", c)
	}
}

// The copies made count whatever the case of the repository's name: the
// factory claims a copy made as Talkable/Talkable once the key reads
// talkable/talkable (the store matches repo_full_name without case), so
// its Redis offset counts too. Doctor matched the name with case and gave
// a PASS on 64 databases while copies 4 and 5 failed their flush (C6 of
// T3b's third check).
func TestDoctorCountsTheCopiesMadeWhateverTheRepositorysCase(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	fake := d.Run.(*execx.Fake)
	cfg := *d.Config
	cfg.Factory.Repos = redisRepos()[:1]
	cfg.Factory.Repos[0].FixCopies = 0
	d.Config = &cfg
	fake.Rules = append([]execx.Rule{
		{Prefix: []string{"redis-cli", "-h", "127.0.0.1"}, Result: execx.Result{Stdout: []byte("databases\n64\n")}},
	}, fake.Rules...)
	st := storetest.OpenFile(t, d.Layout.FactoryAgentsDB())
	for _, n := range []string{"factory4", "factory5"} {
		if _, err := st.CreateSlot(context.Background(), store.Slot{Name: n, RepoFullName: "Talkable/Talkable", Kind: store.SlotKindPool,
			Path: "/p/talkable." + n, MainClone: "/p/main", State: store.SlotFree}); err != nil {
			t.Fatal(err)
		}
	}
	checks := doctorRun(context.Background(), d)
	i := slices.IndexFunc(checks, func(c doctorCheck) bool { return c.Name == "redis talkable/talkable" })
	if i < 0 {
		t.Fatal("no redis check of talkable/talkable")
	}
	if c := checks[i]; c.Status != doctorFail || !strings.Contains(c.Detail, "need 96") {
		t.Fatalf("talkable with copies 4 and 5 made as Talkable/Talkable, on a Redis with 64 = %+v, want a FAIL naming 96", c)
	}
}

// A redis-cli that is not on PATH is no Redis that does not answer: doctor
// names redis-cli and says how to install it, never to start a Redis that
// runs, and fails where the copies' claims flush their Redis databases with
// it (REDIS_DB_OFFSET), since each such claim then fails. It read as "the
// Redis does not answer", with "start the Redis" as the fix (C9 on T3b).
func TestDoctorNamesAMissingRedisCLIThatTheClaimsFlushNeedsToo(t *testing.T) {
	_, d, _, _ := doctorFixture(t)
	fake := d.Run.(*execx.Fake)
	cfg := *d.Config
	cfg.Factory.Repos = redisRepos()[:2]
	d.Config = &cfg
	missing := &execx.RunError{Cmd: execx.Cmd{Name: "redis-cli"}, Err: &exec.Error{Name: "redis-cli", Err: exec.ErrNotFound}}
	fake.Rules = append([]execx.Rule{{Prefix: []string{"redis-cli"}, Err: missing}}, fake.Rules...)
	m := map[string]doctorCheck{}
	for _, c := range doctorRun(context.Background(), d) {
		m[c.Name] = c
	}
	tk := m["redis talkable/talkable"]
	if tk.Status != doctorFail || !strings.Contains(tk.Detail, "redis-cli is not on PATH") || !strings.Contains(tk.Detail, "flush") ||
		!strings.Contains(tk.Detail, "need 96") || !strings.Contains(tk.Fix, "install redis-cli") || strings.Contains(tk.Fix, "start the Redis") {
		t.Fatalf("talkable, whose claims flush with redis-cli, without it = %+v, want a FAIL naming redis-cli and the flush, with its install as the fix", tk)
	}
	c := m["redis example/cache"]
	if c.Status != doctorWarn || !strings.Contains(c.Detail, "redis-cli is not on PATH") || strings.Contains(c.Detail, "flush") ||
		!strings.Contains(c.Fix, "install redis-cli") || strings.Contains(c.Fix, "start the Redis") {
		t.Fatalf("example/cache, whose claims flush nothing, without redis-cli = %+v, want a WARN naming redis-cli, with its install as the fix", c)
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
