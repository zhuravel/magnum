# magnum build helpers. `make install` loads the launchd agent for bin/magnum (see `magnum install`);
# `make release VERSION=vX.Y.Z` tags, publishes and updates the Homebrew tap (scripts/release.sh).

GO            ?= go
GOLANGCI_LINT ?= golangci-lint
BIN           ?= bin/magnum
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
INSTALL_FLAGS ?= --plugin
# SIGN_IDENTITY: a code-signing identity in your keychain that `make build` signs bin/magnum with, so macOS
# keeps the privacy permissions you gave it (removable volumes, files and folders) across rebuilds. Without
# one the build keeps Go's ad-hoc signature, which macOS treats as a new app at every build.
SIGN_IDENTITY ?= magnum local signing

# GATE_LOCK: the lock every gate on this machine takes (make test, make test-race, make lint-ratchet), so two checkouts
# never run their tests at once. lockf(1) takes it where it exists (macOS), flock(1) elsewhere (Linux); with neither
# the gate runs unlocked. A gate that finds it taken says so and waits up to an hour, then its recipe fails with
# status 75 (make reports "Error 75" and exits 2). It is in /tmp, not $TMPDIR (agent shells get a TMPDIR of their
# own), and it is not /tmp/magnum-gate.lock, which older `lockf -k /tmp/magnum-gate.lock make test` wrappers hold: the
# same lock taken again inside them would wait for its own parent. MAGNUM_GATE_LOCKED=1 marks a process that already
# holds it.
GATE_LOCK ?= /tmp/magnum-make-test.lock
# TEST_P: how many test binaries one gate runs at once (go test -p); 4 keeps one gate's memory and CPU low on a
# laptop; TEST_P= uses Go's default (one per CPU)
TEST_P    ?= 4
TEST_P_FLAG = $(if $(TEST_P),-p $(TEST_P))
# TEST_TZ: the time zone the gate's tests run in (make test, make test-race), here and on CI: neither UTC nor a whole
# hour off it (Asia/Kathmandu, UTC+5:45), so a test that assumes UTC, or the zone of the machine it was written on,
# fails everywhere instead of only on the machine in another zone. go test's cache does not key on TZ (the time package
# reads it past the test log), so a pass cached by a run by hand in another zone would pass make test too: the gate adds
# -skip TZ=$(TEST_TZ), which skips no test (no test name holds "=") and puts the zone into the cache's key
TEST_TZ   ?= Asia/Kathmandu
# TEST_PKGS: the packages the gate tests (default every one). CI's test job passes every package but internal/engine,
# which its engine job tests under make test-race in two halves (-run ^TestA and -run ^Test[B-Z]), as you do here
TEST_PKGS ?= ./...
# RACE_GCFLAGS: test-race's -gcflags. -race turns on checkptr, which checks every unsafe.Pointer conversion, and
# modernc.org/sqlite and modernc.org/libc (Go transpiled from C) make so many that the checks took about 30% of the
# engine's CPU under -race. They go off only there: magnum's own packages keep them, and the race detector stays on
# everywhere. CI's GOFLAGS carry the same flag into its race gate
RACE_GCFLAGS ?= -gcflags=modernc.org/...=-d=checkptr=0
gate_locked = if [ -n "$$MAGNUM_GATE_LOCKED" ]; then $(1); \
  elif command -v lockf >/dev/null 2>&1; then \
    lockf -k -s -t 0 "$(GATE_LOCK)" true || echo "make: waiting for $(GATE_LOCK): another checkout's gate runs"; \
    env MAGNUM_GATE_LOCKED=1 lockf -k -t 3600 "$(GATE_LOCK)" $(1); st=$$?; \
    if [ $$st -eq 75 ]; then echo "make: gave up after an hour waiting for $(GATE_LOCK) (lockf exit 75): is make test running inside lockf on it?"; fi; exit $$st; \
  elif command -v flock >/dev/null 2>&1; then \
    flock -n "$(GATE_LOCK)" true || echo "make: waiting for $(GATE_LOCK): another checkout's gate runs"; \
    env MAGNUM_GATE_LOCKED=1 flock -w 3600 -E 75 "$(GATE_LOCK)" $(1); st=$$?; \
    if [ $$st -eq 75 ]; then echo "make: gave up after an hour waiting for $(GATE_LOCK) (flock exit 75)"; fi; exit $$st; \
  else $(1); fi

.PHONY: build test gate test-race check-private lint lint-ratchet lint-fix vuln install api-doc api-doc-check release link unlink

## build: compile bin/magnum (the binary launchd and the herdr plugin run), signed with SIGN_IDENTITY when your
## keychain has it, and moved into place only then, so a daemon that restarts on a new binary never runs it unsigned
build:
	$(GO) build -ldflags "-X main.version=$(VERSION)" -o $(BIN).new ./cmd/magnum
	@if command -v codesign >/dev/null 2>&1 && security find-certificate -c "$(SIGN_IDENTITY)" >/dev/null 2>&1; then \
		codesign --force --sign "$(SIGN_IDENTITY)" --identifier zhuravel.magnum $(BIN).new >/dev/null 2>&1 \
			|| echo "build: could not sign with \"$(SIGN_IDENTITY)\"; macOS will ask for its permissions again"; \
	fi
	@mv -f $(BIN).new $(BIN)

## test: the gate (below) under GATE_LOCK: it waits while another checkout's gate runs
test:
	@$(call gate_locked,$(MAKE) --no-print-directory gate)

## gate: the private-name guard, the complexity ratchet (lint-ratchet), gofmt check (every tracked .go file, and new
## ones git does not ignore), go.sum verified and go.mod tidy, vet, and the hermetic unit tests of TEST_PKGS (TEST_P
## binaries at once, in TEST_TZ); run it as make test, which holds the machine's gate lock
gate: check-private lint-ratchet
	@out="$$(git ls-files -z --cached --others --exclude-standard '*.go' | xargs -0 gofmt -l)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) mod verify
	$(GO) mod tidy -diff
	$(GO) vet ./...
	TZ=$(TEST_TZ) $(GO) test $(TEST_P_FLAG) -skip 'TZ=$(TEST_TZ)' $(TEST_PKGS)

## test-race: go test -count=1 -race -timeout 30m with RACE_GCFLAGS on PKGS (the packages you touched; default ./...)
## in TEST_TZ under GATE_LOCK, TEST_P binaries at once (the engine, split in two with TESTFLAGS="-run ^TestA" and
## "-run ^Test[B-Z]", takes about 3 minutes a half on a busy laptop); TESTFLAGS adds flags and overrides these (-run,
## -count=3 -cpu 1,3 -shuffle=on, -timeout 45m)
PKGS ?= ./...
test-race:
	@$(call gate_locked,env TZ=$(TEST_TZ) $(GO) test -count=1 -race $(RACE_GCFLAGS) -timeout 30m $(TEST_P_FLAG) $(TESTFLAGS) $(PKGS))

## check-private: this repo is public; fail when a tracked file mentions a name listed in MAGNUM_PRIVATE_PATTERNS
## (a grep -E pattern set in the gitignored mise.local.toml, never in a tracked file), or a person's GitHub
## login from the local registry (scripts/check-logins.sh; MAGNUM_OWN_LOGINS and MAGNUM_LOGIN_ALLOW exempt)
check-private:
	@main="$$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)/.."; \
	if [ -z "$$MAGNUM_PRIVATE_PATTERNS" ] && [ ! -f mise.local.toml ] && [ -f "$$main/mise.local.toml" ]; then \
	  eval "$$(mise -C "$$main" env -s bash 2>/dev/null | grep -E '^export MAGNUM_(PRIVATE_PATTERNS|OWN_LOGINS|LOGIN_ALLOW)=')"; \
	  [ -n "$$MAGNUM_PRIVATE_PATTERNS" ] || { echo "check-private: $$main/mise.local.toml sets no MAGNUM_PRIVATE_PATTERNS for this worktree"; exit 1; }; \
	fi; \
	if [ -n "$$MAGNUM_PRIVATE_PATTERNS" ] && git grep -qiIE -e "$$MAGNUM_PRIVATE_PATTERNS" -- . ':!config.local.toml'; then \
	  echo "tracked files mention a private name:"; git grep -niIE -e "$$MAGNUM_PRIVATE_PATTERNS" -- . ':!config.local.toml'; exit 1; fi; \
	scripts/check-logins.sh

## lint: golangci-lint with .golangci.yml (the version mise.toml pins); not part of `test` until the backlog is cleared
## (its complexity linters are, through lint-ratchet)
lint:
	$(GOLANGCI_LINT) run ./...

## lint-ratchet: the complexity ratchet (gocyclo and nestif, with .golangci.yml's thresholds and exclusions) over the
## whole tree, under GATE_LOCK, part of the gate (about a second: both read syntax only); it fails on any hit. CI's
## lint and --new-from-rev report such a hit only where a change touched its func or if line, so a function grew past
## a threshold unseen
# a variable: the comma of the linters' list would split call's argument
LINT_RATCHET = $(GOLANGCI_LINT) run --enable-only gocyclo,nestif ./...
lint-ratchet:
	@$(call gate_locked,$(LINT_RATCHET))

## lint-fix: apply only the safe automatic fixes (modernize, intrange), then the formatters; never a blanket --fix
## (errorlint's fix for internal/pipeline/restart.go breaks the build). The fix pass fails while it leaves hits it
## cannot fix (a range over a method's result), so its status is ignored and the formatters still run.
lint-fix:
	-$(GOLANGCI_LINT) run --fix --enable-only modernize,intrange ./...
	$(GOLANGCI_LINT) fmt ./...

## vuln: govulncheck: known vulnerabilities the code reaches, in its dependencies and in the Go release it builds with
vuln:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

## install: build, then load the launchd agent and link the herdr plugin (INSTALL_FLAGS="" skips the plugin)
install: build
	$(BIN) install $(INSTALL_FLAGS)

## api-doc: regenerate the package sections of docs/API.md from `go doc -all` (the hand-written text above the marker is kept;
## API_DOC_OUT=<file> writes the result there instead, which api-doc-check uses)
API_MARKER  := <!-- generated by `make api-doc` below this line; edit only the text above -->
API_DOC_OUT  = docs/API.md
api-doc:
	@grep -qF '$(API_MARKER)' docs/API.md || { echo "docs/API.md: marker line missing"; exit 1; }
	@{ sed -n '1,/^<!-- generated by `make api-doc`/p' docs/API.md; \
	  echo; echo '| Package | Synopsis |'; echo '|---|---|'; \
	  $(GO) list -f '| [{{.Name}}](#{{.Name}}) | {{.Doc}} |' ./internal/...; \
	  for d in $$($(GO) list -f '{{.ImportPath}}' ./internal/...); do \
	    echo; echo "## $${d##*/}"; echo; echo '```text'; $(GO) doc -all $$d || exit 1; echo '```'; \
	  done; } > $(API_DOC_OUT).tmp && mv $(API_DOC_OUT).tmp $(API_DOC_OUT)
	@[ "$(API_DOC_OUT)" != docs/API.md ] || echo "docs/API.md regenerated"

## api-doc-check: fail when docs/API.md differs from what `make api-doc` writes (generated into a temporary file, diffed)
api-doc-check:
	@tmp="$$(mktemp -d)" && trap 'rm -rf "$$tmp"' EXIT && \
	  $(MAKE) --no-print-directory -s api-doc API_DOC_OUT="$$tmp/API.md" && \
	  { diff -u docs/API.md "$$tmp/API.md" || { echo "docs/API.md is stale: run make api-doc and commit it"; exit 1; }; }

## release: tag the pushed master as $(VERSION) (vX.Y.Z), publish the GitHub release, update the Homebrew tap
release:
	scripts/release.sh $(VERSION)

## link: put this checkout's build on PATH as ~/.local/bin/magnum (a symlink, so every `make build` updates it)
link: build
	@mkdir -p $(HOME)/.local/bin
	@ln -sfn $(CURDIR)/$(BIN) $(HOME)/.local/bin/magnum
	@echo "~/.local/bin/magnum -> $(CURDIR)/$(BIN)"

## unlink: remove ~/.local/bin/magnum when it is this checkout's link
unlink:
	@if [ "$$(readlink $(HOME)/.local/bin/magnum)" = "$(CURDIR)/$(BIN)" ]; then rm $(HOME)/.local/bin/magnum && echo "removed ~/.local/bin/magnum"; \
	else echo "~/.local/bin/magnum is not this checkout's link; left alone"; fi
