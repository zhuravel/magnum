# R55 slice 1: the factory's visual QA stage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Before the ready card, the factory starts the change's app in the task's copy. An agent walks the changed pages in the states that the acceptance checks name. The factory checks the saved pages itself. If nothing visible is broken, the task goes on.

**Architecture:** A new stage type `qa` runs after `local_review`. Its runner (`runQA`, `internal/factory/qa.go`) holds one QA at a time on the Mac. It starts the copy's server and waits for it to answer. Then it starts its own headless Chrome and runs one `fx-qa` walker turn through the agents stage machinery (end fact `qa`). It always stops the browser and the server and proves that the port is free. Go detectors read the probe files the walker saved and decide the outcome. A clean outcome passes the stage. A defect becomes a writer fix turn (the local review's fix machinery), then the task goes back through the review and QA. QA that cannot run moves the task to NEEDS YOU, with the reason and the retry command.

**Tech Stack:** Go (latest) and the existing factory engine, registry and runtime. Prompts use `text/template`, and the probe uses `go:embed`. Headless Chrome starts through `os/exec`, with no CDP library. macOS `lsof` and `ps` find the processes.

**Spec:** `R55-design.md`, and items R55 and R56 of `ROADMAP.md`, in the audit folder outside the repository. Read section 4(b), 4(c), 5 and 7 (slice 1). The design's private parts never enter this plan or the repository.

## Global Constraints

- The repository is public. Use the repository `example/shop`, hosts `app.example.localhost` and `127.0.0.1`, and logins `zhuravel`, `alice` and `rev-ann` only. Name no private organization, repository, host or ticket in code, tests, fixtures, docs or commit messages. `make test` runs `check-private`.
- Tests are hermetic. Never run `bin/magnum`, a server, a browser, herdr, launchctl or mysql. Use fakes for the server (`fakeCopies`), the HTTP readiness probe (`Engine.qaProbe`) and the browser (`copySet.spawnBrowser`, `fakeCopies`). Also fake `lsof` and `dial` (copySet seams) and the agents (`fakeRuntime`). Build on `newHarness`, `newCodingRig` and `newReviewRig`. A test that swaps a package variable calls `storetest.Serial(t)`.
- Run every gate under the shared lock: `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- <cmd>`. Each task runs `go test -count=1 -race` on the packages it touched, then `make test`, then `make lint`. Fix only the issues in the lines you touched. When the exported API changed, run `mise exec -- make api-doc`. Never use `git stash`, never push, and never touch master.
- Commit messages follow the repository style: one sentence that says the behaviour, then a short body. Add no new dependency.
- Every destructive step (server start and stop, browser start and stop, stray kill) writes `begin`, `ok` and `fail` events: kind `qa` on the task, or the copy's `factory.server_*` / `factory.browser_*` events in the agents store. Events, logs and NEEDS YOU texts carry classes, page paths, counts and file names. They never carry page text, quotes, secrets or PR text. Quotes live only in the run folder's files (0600), clipped by `textx.Clip` and passed through `execx.Redact`.
- Values the design pins. Use them exactly:
  - `boot` 5m and `cap` 50m for one run.
  - The walker's role timeout is 35m.
  - `max_pages` is 12 (at most 30). At most 30 shots and one tab.
  - The viewport is 1280×800.
  - The probe's text is capped at 200,000 bytes and its HTML at 400,000.
  - The verdict's `version` is 1, and any other version is refused.
  - A page's host must be `localhost`, `*.localhost`, `127.0.0.1` or `::1`, on the copy's `PORT`.
  - A boot failure gets one retry. A bad verdict gets one nudge. Critical memory pressure gets one retry after it clears. `fixes` is 2 (from 1 to 5).
  - Ready means a 2xx or 3xx answer to `url + ready`.
- Severity: the walker writes P1 for Critical, P2 for Major and P3 for the rest. The Go detectors use P2 for `escaped_markup`, `raw_i18n_key` and `error_page`, and P3 for `console_error` and `broken_layout`.
- What blocks the stage:
  - any defect of class `escaped_markup`, `raw_i18n_key` or `error_page`, whoever found it.
  - any P1 or P2.
  - an invalid verdict.
  
  Slice 1 has no before pass, so `pre_existing` is read but never trusted: it never unblocks a defect. A writer can decline a defect only with its risk, as with a review finding.
- R56 is not in this plan. QA must stay correct without it: no QA decision reads the review's severities, and a review never clears a QA defect.
- Out of scope (slices 2 to 4): the before pass, `fx-qa-review`, attachments and the manifest hash in `y`. Also out of scope: the artifact-kinds migration, the skill's `auto` mode, the stats lines and the replay command. The operator's replay of the original case is a private opt-in step, never in the gate.
- Decisions this plan takes where the design left room:
  - A QA defect that comes back after its fix still blocks. It is named "came back after its fix", never moved to the doubts. A visible defect never ships quietly.
  - The QA run holds the factory's heavy slot and its own machine lock (`factory-qa.lock`). It does not hold the spec lock: a 50-minute hold starves every precheck. The memory hold guards the overlap.
  - `e` on a ready card opens the run's `summary.md` (its shots listed with absolute paths) in the pager that already opens the fixer's report. It does not open Finder.
- Docs: every new key goes in the comments of `config.defaults.toml` and `config.full.example.toml` in the task that adds it. The README factory section and one DECISIONS entry (appended at the END of `## The factory (2026-11-03)`, never in `## Learned rules`) go in Task 7.

## Review Focus

- **A page that shows escaped HTML on purpose** in an attribute (a Bootstrap `data-bs-title="&lt;b&gt;…"`) or in text such as "Press <Enter>". This must not block. Pinned by `TestDetectQAIgnoresEscapedHTMLInsideAttributes` and `TestDetectQAIgnoresAnglePlaceholdersThatAreNoHTMLTags` (Task 2).
- **A verdict whose page sits on another host or port** (a staging URL, the operator's own server on :3000). The verdict is invalid: the walker gets its nudge, and the run is never clean. Pinned by the `off-host` and `other-port` rows of `TestReadQAVerdictRefuses` (Task 2).
- **The copy's port answering before its server started** (another app holds it). Else QA walks the wrong app. Pinned by `TestQARefusesAPortThatAnswersBeforeTheServerStarts` (Task 4).
- **A `when` command that fails** (a missing script, a bad exit). This means "cannot tell", not "nothing user-facing": NEEDS YOU, never `not_needed`. Pinned by `TestAFailingWhenCommandNeedsYouNotNotNeeded` (Task 4).
- **The writer declining every QA defect with its risk** (no commit, the head unchanged). The stage must end without a loop, with the declines in KNOWN. Pinned by `TestADeclinedQADefectEndsTheStageWithoutALoop` (Task 6).

---

## File map

| File | Responsibility | Task |
|---|---|---|
| `internal/config/factory.go` | `[factory.repo.qa]` (`FactoryQA`), `[factory] browser`, stage type and end fact `qa`, role `fx-qa`, validation, then the default kinds | 1, 5 |
| `prompts/factory-qa.md`, `prompts/factory-qa-fix.md` | the walker's prompt, the writer's QA fix prompt | 1, 6 |
| `internal/factory/qa_verdict.go`, `qa_probe.js`, `testdata/qa/` | the verdict contract, the embedded probe, the Go detectors, the outcome | 2 |
| `internal/factory/server.go`, `browser.go`, `internal/paths/paths.go` | the server's stop with its proof, the no-tab environment, the factory's Chrome | 3 |
| `internal/factory/qa.go` | `runQA`: the run, the session, its teardown, the restart, the walk, the outcome | 4, 5 |
| `internal/factory/qa_fix.go` | the fix turn, the back-edge to the review, the `fixes` cap | 6 |
| `internal/factory/stage_ready.go`, `screen.go`, `internal/tui/*` | `readyProblem`, the card's QA line, the PR body's QA section, `e` | 5, 7 |

---

### Task 1: The QA configuration, the `fx-qa` role and its prompt

**Files:**
- Modify: `internal/config/factory.go`: `Factory`, `FactoryRepo`, the stage consts and lists, `DefaultFactoryRoles`, `expandFactory`, `validateStage`, `validateFactoryKinds`, `validateFactoryRepos`
- Modify: `config.defaults.toml`: the `fx-qa` role block, a `qa` line in the stage-type comment and `browser` under `[factory]`.
- Modify: `config.full.example.toml`: a commented `[factory.repo.qa]` block under the `[[factory.repo]]` example, and `browser`.
- Create: `prompts/factory-qa.md`
- Modify: `prompts/README.md` (the prompt table row and the PromptData rows of the QA fields)
- Modify: `internal/factory/stage_agents.go` (`PromptData` fields and `checkData`)
- Test: `internal/config/factory_test.go`, `internal/factory/stage_agents_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces (package `config`):
  ```go
  const FactoryStageQA = "qa"   // appended to FactoryStageTypes
  const FactoryEndQA   = "qa"   // appended to FactoryEndFacts
  const (
      DefaultQABoot     = 5 * time.Minute
      DefaultQACap      = 50 * time.Minute
      DefaultQAMaxPages = 12
      DefaultQAFixes    = 2
      DefaultQAReady    = "/"
      MaxQAPages        = 30
      MaxQAFixes        = 5
      MaxQACap          = 3 * time.Hour
  )
  var DefaultFactoryBrowsers = []string{
      "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
      "/Applications/Chromium.app/Contents/MacOS/Chromium",
  }
  type FactoryQA struct {
      When       string   `toml:"when"`        // required: reads the changed files on stdin, prints the user-facing ones
      URL        string   `toml:"url"`         // required: "http://app.example.localhost:{port}"
      Ready      string   `toml:"ready"`       // default "/"
      Skill      string   `toml:"skill"`       // optional, one word: the repository's QA skill (the walker reads it as reference in slice 1)
      SignIn     string   `toml:"sign_in"`     // optional, one line: how the walker signs in locally (never a password: name the env var)
      ServerStop string   `toml:"server_stop"` // optional: runs in the copy before the group kill (e.g. "overmind kill")
      Boot       Duration `toml:"boot"`
      Cap        Duration `toml:"cap"`
      MaxPages   int      `toml:"max_pages"`
      Fixes      int      `toml:"fixes"`
  }
  // FactoryRepo gains: QA *FactoryQA `toml:"qa"` (nil = no QA for the repository)
  // Factory gains:     Browser string `toml:"browser"` ("" = the first of DefaultFactoryBrowsers that exists)
  func (q FactoryQA) PageURL(port int, path string) string // URL with {port} rendered, then path
  func LocalHost(host string) bool // "localhost", "*.localhost", "127.0.0.1", "::1"; case-insensitive
  ```
  Role (Go and TOML alike): `{Name: "fx-qa", Kind: KindClaude, Effort: "high", Prompt: "factory-qa.md", Timeout: Duration{35 * time.Minute}, Why: "look at the changed pages in a browser, in the states the ticket names", Doing: "walking the pages"}`.
  Package `factory`, new `PromptData` fields: `QARun, QAURL, QACDP, QAProbe, QAVerdict, QAChanged, QASignIn, QASkill string; QAMaxPages, QAMaxShots int`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/config/factory_test.go
func TestARepoWithoutAQABlockHasNoQA(t *testing.T)            // a repo block with no [factory.repo.qa] → cfg.Factory.Repos[0].QA == nil
func TestTheQABlockTakesItsDefaults(t *testing.T) {
    // [factory.repo.qa] with only when = "bin/user-facing", url = "http://app.example.localhost:{port}"
    // want QA.Ready "/", Boot 5m, Cap 50m, MaxPages 12, Fixes 2
}
func TestTheQABlockIsValidated(t *testing.T) {
    // each row edits a valid repo (server = "bin/dev", env PORT = "31{n}0") and wants an error containing want:
    // {"no when", "qa.when"}, {"url off the Mac: http://staging.example.com:{port}", "qa.url"},
    // {"url without {port}", "{port}"}, {`ready = "up"`, "qa.ready"}, {`skill = "a b"`, "qa.skill"},
    // {"sign_in on two lines", "qa.sign_in"}, {`boot = "1h"` with cap 50m, "qa.boot"}, {`cap = "4h"`, "qa.cap"},
    // {`max_pages = 31`, "qa.max_pages"}, {`fixes = 0`, "qa.fixes"}, {"env without PORT", "PORT"},
    // {"no server", "server"}
}
func TestAQAStageNeedsRolesTheQAEndAndAReviewBeforeIt(t *testing.T) {
    // a kind with {qa type qa roles [fx-qa] end qa} BEFORE local_review → "must come after a review stage"
    // qa without roles → "needs roles"; qa with end "clean" → `end "clean" must be qa`; qa with prompt → "prompt is for agents stages only";
    // two qa stages → "at most one qa stage"; qa with when = "wide" → "when"
}
func TestFactoryBrowserMustBeAbsolute(t *testing.T) // [factory] browser = "chrome" → error naming factory.browser
func TestLocalHostTakesOnlyTheMacsOwnNames(t *testing.T) {
    // true: localhost, LOCALHOST, app.example.localhost, 127.0.0.1, ::1
    // false: example.com, localhost.example.com, 10.0.0.1, 127.0.0.2, ""
}
// The existing TestFactoryGoDefaultsEqualTheEmbeddedFile and TestFactoryRolesNameEmbeddedPrompts cover fx-qa once both files have it.

// internal/factory/stage_agents_test.go
func TestCheckPromptsRendersTheQAWalkersPrompt(t *testing.T) {
    cfg := config.Defaults()
    if err := CheckPrompts(cfg); err != nil { t.Fatal(err) }
    role, _ := cfg.FactoryRole("fx-qa")
    d := checkData(role)
    // render factory-qa.md with d: the text contains d.QACDP, d.QAVerdict, d.QAProbe, d.QAChanged, d.Acceptance, d.QASignIn, d.QASkill,
    // "12" (QAMaxPages), "30" (QAMaxShots), and the words "blocked" and "not_shown"
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -run 'QA|FactoryBrowser|LocalHost|FactoryGoDefaults|FactoryRolesName' ./internal/config/ ./internal/factory/`
Expected: FAIL to compile (`FactoryQA`, `LocalHost` and `QACDP` are undefined).

- [ ] **Step 3: Implement the config and the prompt**

- `validateStage`:
  - roles are allowed for `agents` and `qa` stages.
  - a `qa` stage needs roles that are known `[[factory.role]]`s and `end = "qa"`.
  - no `prompt` and no `when`.
- `validateFactoryKinds`: at most one `qa` stage per kind, and it comes after a `review` stage. The ready_push and publish ordering checks stay as they are.
- If `QA` is set, `validateFactoryRepos` checks it:
  - `when` is not blank.
  - `url` parses after `{port}` becomes `1`. Its scheme is http or https, its host passes `LocalHost`, and it contains `{port}`.
  - `ready` starts with `/`.
  - `skill` is one word, and `sign_in` is one line.
  - `0 < boot <= cap <= MaxQACap`.
  - `1 <= max_pages <= MaxQAPages` and `1 <= fixes <= MaxQAFixes`.
  - the repo has a `server`, and its `env` names `PORT`.
- If `Ready`, `Boot`, `Cap`, `MaxPages` or `Fixes` is zero, `expandFactory` sets its default. It also trims `Skill`.
- Do not change `DefaultFactoryKinds` yet. Task 5 adds the stage, once the walk exists.

`prompts/factory-qa.md` uses `{{template "task" .}}`, `{{template "inputs" .}}` and `{{template "rules" .}}`. In order, it says:
1. **Role.** You are the QA walker. The change is in `{{.Copy}}`. Its server answers at `{{.QAURL}}`.
2. **Inputs.** Read `{{.Acceptance}}` and `{{.QAChanged}}` (the user-facing files). Map the files to the routes they render.
3. **The browser.** Drive only the headless browser at `{{.QACDP}}`, over CDP, with the browser tool you have. Use one tab. Never start another browser, never run `open`, and never load a URL off `{{.QAURL}}`'s host and port.
4. **Signing in.** Use only the local sign-in `{{.QASignIn}}`. Never type a real password. Use local data only: change it only with local commands in the copy, each run as `{{.DBLockCommand}} <command>`.
5. **States.** Reach each state an acceptance check's `given` names before you capture it. Use the UI first. Else use local means: a record from a local command, or a TOTP from the local secret. For a passkey or a security key, use a CDP virtual authenticator. A state you cannot reach goes into `blocked` with why. Never claim it.
6. **Each page.** Save a full-page PNG to `{{.QARun}}/pages/NN-<page>-<state>-after.png`. After load, evaluate `{{.QAProbe}}` and save the JSON it returns, unchanged, beside the PNG as `.json`. Save the console messages, one per line with a `[level]` prefix, to `{{.QARun}}/console/NN.log`. Capture at most `{{.QAMaxPages}}` pages and `{{.QAMaxShots}}` shots.
7. **Defects.** The classes are `escaped_markup`, `raw_i18n_key`, `error_page`, `broken_layout`, `console_error` and `flow`. Severity: P1 for Critical, P2 for Major, P3 for the rest. Give each defect its page and the code paths you suspect.
8. **The skill.** `{{with .QASkill}}`Read the skill `{{.}}` for this repository's sign-in, test data and page conventions only. Never run its steps that ask questions, post anywhere or edit a PR.`{{end}}`
9. **Ask nothing.** Write no questions file: what you cannot do goes into `blocked`. If the server stops answering, write the verdict with what you have.
10. **The verdict, last.** Write `{{.QAVerdict}}` with the JSON contract of Task 2: `version` 1, `head`, `pages`, `defects`, `blocked`, and `not_shown` (an acceptance check that no page shows, with why). Every acceptance check id goes into a page's `checks`, `blocked[].check` or `not_shown`.

`checkData` sets:

```go
QARun: dir+"/qa/1-abc1234"
QAURL: "http://app.example.localhost:3000"
QACDP: "http://127.0.0.1:9222"
QAProbe: QARun+"/probe.js"
QAVerdict: QARun+"/verdict.json"
QAChanged: QARun+"/changed.txt"
QASignIn: "/dev/sign-in?as=qa@example.com"
QASkill: "example:qa"
QAMaxPages: 12
QAMaxShots: 30
```

- [ ] **Step 4: Run them to see them pass**

Run: the Step 2 command, then `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -race ./internal/config/ ./internal/factory/ ./prompts/`
Expected: PASS. `TestFactoryGoDefaultsEqualTheEmbeddedFile` and `TestEveryTomlBlockInTheDocsDecodesWithoutUnknownKeys` also pass.

- [ ] **Step 5: Gate, API doc, commit**

Run `make test`, `make lint` and `make api-doc` (all under the lock), then:
```bash
git add internal/config/factory.go internal/config/factory_test.go config.defaults.toml config.full.example.toml prompts/factory-qa.md prompts/README.md internal/factory/stage_agents.go internal/factory/stage_agents_test.go docs/API.md
git commit -m "A factory repository declares visual QA in [factory.repo.qa], and the fx-qa walker has its prompt"
```

---

### Task 2: The verdict contract, the embedded probe and the Go detectors

**Files:**
- Create: `internal/factory/qa_verdict.go`, `internal/factory/qa_probe.js`
- Create: `internal/factory/testdata/qa/escaped-tooltip.json`, `clean-tooltip.json`, `attribute-html.json`, `angle-placeholder.json`, `missing-translation.json`, `error-page.json`
- Test: `internal/factory/qa_verdict_test.go`

**Interfaces:**
- Consumes: `config.LocalHost` (Task 1), and `endProblem`, `readFresh` and `AcceptanceFile` (`endfacts.go`).
- Produces:
  ```go
  const QAVerdictVersion = 1
  const QAMaxShots = 30
  const (QAEscapedMarkup = "escaped_markup"; QARawI18nKey = "raw_i18n_key"; QAErrorPage = "error_page";
         QABrokenLayout = "broken_layout"; QAConsoleError = "console_error"; QAFlow = "flow")
  const (QAOutcomeClean = "clean"; QAOutcomeDefects = "defects"; QAOutcomeUnavailable = "unavailable";
         QAOutcomeNotNeeded = "not_needed"; QAOutcomePressure = "pressure"; QAOutcomeInterrupted = "interrupted")
  type QAVerdict struct {
      Version  int          `json:"version"`
      Head     string       `json:"head"`
      Base     string       `json:"base"`
      URL      string       `json:"url"`
      Walker   string       `json:"walker"`
      Pages    []QAPage     `json:"pages"`
      Defects  []QADefect   `json:"defects"`
      Blocked  []QABlocked  `json:"blocked"`
      NotShown []QANotShown `json:"not_shown"`
  }
  type QAPage struct { ID, Path, State, Side, Shot, Probe, Console string; Status int; Checks []string } // json: id path state side shot probe console status checks
  type QADefect struct { ID, Class, Severity, Page, Title, Quote, Source string; Paths []string; PreExisting bool; Fingerprint string } // json: …, pre_existing, source ("walker"|"factory"), fingerprint
  type QABlocked struct { What, Why, Check string }   // json: what why check
  type QANotShown struct { Check, Why string }        // json: check why
  type QAProbe struct { Nonce, URL, Title, Text, HTML string; Status int; OverflowX bool; Truncated bool } // json: nonce url title text html status overflow_x truncated
  type qaWant struct { Head, Nonce string; Port, MaxPages int; Checks []string }
  func readQAVerdict(taskDir, runRel string, since time.Time, want qaWant) (QAVerdict, *endProblem)
  func readQAProbe(runPath, rel string) (QAProbe, error)
  func detectQA(page QAPage, p QAProbe, console []string) []QADefect
  func qaOutcome(v QAVerdict, found []QADefect) (outcome string, blocking, known []QADefect)
  func qaFingerprint(d QADefect, pagePath string) string // sha256 hex of class|pagePath|first of Paths, first 16 chars
  func qaProbeScript(nonce string) []byte                // the embedded qa_probe.js with __MAGNUM_NONCE__ replaced
  ```

- [ ] **Step 1: Write the failing tests**

```go
func TestDetectQAFindsTheEscapedTooltipSpan(t *testing.T) {
    // testdata/qa/escaped-tooltip.json: text "Passkeys\nAdded <span data-bs-toggle=\"tooltip\" title=\"Nov 07, 2026 10:15\">Nov 07, 2026</span>\nnever used",
    // html with "Added &lt;span data-bs-toggle=&quot;tooltip&quot;…"
    ds := detectQA(QAPage{ID: "P1", Path: "/settings/security"}, probe, nil)
    // len(ds)==1, ds[0].Class==QAEscapedMarkup, Severity=="P2", Source=="factory", Page=="P1",
    // strings.Contains(ds[0].Quote, `<span data-bs-toggle`), Fingerprint != ""
}
func TestDetectQAPassesTheSamePageRenderedRight(t *testing.T)            // clean-tooltip.json: text "Added Nov 07, 2026", html has a real <span data-bs-toggle…> → no defect
func TestDetectQAIgnoresEscapedHTMLInsideAttributes(t *testing.T)        // attribute-html.json: html data-bs-title="&lt;b&gt;x&lt;/b&gt;", clean text → no defect
func TestDetectQAIgnoresAnglePlaceholdersThatAreNoHTMLTags(t *testing.T) // text "Press <Enter> to save, then <name>" → no defect
func TestDetectQAFindsADisplayedEntity(t *testing.T)                     // text "Tom &amp; Jerry" → escaped_markup P2
func TestDetectQAFindsAMissingTranslation(t *testing.T)                  // text "translation missing: en.users.title" → raw_i18n_key P2;
                                                                         // html `<span class="translation_missing" title="…">Title</span>` → raw_i18n_key P2
func TestDetectQAFindsAnErrorPage(t *testing.T)                          // status 500, status 404, status 200 with title "Action Controller: Exception caught"
                                                                         // → error_page P2 each
func TestDetectQANotesConsoleErrorsAndSidewaysScroll(t *testing.T)       // console "[error] Uncaught TypeError: x" → console_error P3;
                                                                         // OverflowX → broken_layout P3; "[warning] x" → nothing
func TestReadQAVerdictTakesAValidRun(t *testing.T)                       // a run dir in t.TempDir(): verdict v1 at head H, 2 pages, PNG shots (magic bytes),
                                                                         // probes with nonce "n0nce" on http://app.example.localhost:3001, checks A1 A2 covered
func TestReadQAVerdictRefuses(t *testing.T) {
    // each row breaks the valid run and wants p.why to contain:
    // {"stale", "older than this stage"}, {"version 2", "version 2"}, {"head", "head"}, {"shot ../x.png", "outside the run folder"},
    // {"missing shot", "missing"}, {"shot not a PNG", "not a PNG"}, {"probe nonce", "not the factory's probe"},
    // {"off-host: http://staging.example.com/x", "not on the copy's own server"}, {"other-port: :3000", "not on the copy's own server"},
    // {"13 pages with max 12", "13 pages"}, {"31 shots", "31 shots"}, {"defect class ugly", "class"}, {"severity P0", "severity"},
    // {"A2 uncovered", "A2"}
    // and p.file == "qa/1-abc1234/verdict.json" for every row
}
func TestQAOutcomeDecidesFromTheFilesNotTheWalker(t *testing.T) {
    // no pages → unavailable; a factory escaped_markup and a walker verdict with no defects → defects;
    // a walker P3 flow note → clean with it in known; a walker P2 flow → defects;
    // a walker escaped_markup with pre_existing true → still defects (no before pass in slice 1);
    // the walker's and the factory's defect with one fingerprint → counted once
}
func TestTheProbeScriptCarriesTheNonceAndSkipsCode(t *testing.T) // qaProbeScript("n0nce") contains "n0nce", "PRE", "CODE", "TEXTAREA",
                                                                 // "SCRIPT", "200000", "400000", "responseStatus", and no "__MAGNUM_NONCE__"
```

- [ ] **Step 2: Run them to see them fail**

Run: `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -run 'DetectQA|ReadQAVerdict|QAOutcome|ProbeScript' ./internal/factory/`
Expected: FAIL to compile (`detectQA` is undefined).

- [ ] **Step 3: Implement `qa_verdict.go` and `qa_probe.js`**

The probe (`qa_probe.js`, `//go:embed`) is an expression that returns `JSON.stringify` of an object with these fields:

```js
{
  nonce: "__MAGNUM_NONCE__",
  url: location.href,
  status: performance.getEntriesByType('navigation')[0]?.responseStatus || 0,
  title: document.title,
  overflow_x: document.documentElement.scrollWidth > innerWidth + 1,
  text,
  html,
  truncated,
}
```

- `text` holds the visible text nodes, from a TreeWalker over SHOW_TEXT. It skips nodes under PRE, CODE, TEXTAREA, SCRIPT, STYLE, NOSCRIPT and TEMPLATE, and it is capped at 200000.
- `html` is `documentElement.outerHTML` with the contents of script and style emptied, capped at 400000.

The detectors (exact rules):
- `escaped_markup` runs on `p.Text` only, never the HTML, because attributes hold escaped markup legally. It matches:
  - a tag `</?NAME(\s[^<>]*)?/?>`, or an opening `<NAME\s+[A-Za-z_:@][-\w:.@]*\s*=\s*["']`, where NAME is one of `htmlTags`, case-insensitive: `a abbr b br button code del div em form h1-h6 i img input ins label li mark ol option p pre s script section select small span strong style sub sup svg table td textarea th time tr u ul`.
  - or an entity `&(?:lt|gt|amp|quot|apos|nbsp|#\d{1,7}|#[xX][0-9A-Fa-f]{1,6});`.
- `raw_i18n_key`: `(?i)translation missing` in the text, or `class\s*=\s*["'][^"']*\btranslation_missing\b` in the HTML.
- `error_page`: any of these:
  - `Status >= 500` or `Status == 404`.
  - the title, text or HTML contains one of `errorPageSigns`: `Action Controller: Exception caught`, `We're sorry, but something went wrong`, `Internal Server Error`, `Traceback (most recent call last)`, `Application error: a client-side exception has occurred`, `Unhandled Runtime Error` or `ActionView::Template::Error`.
- `console_error`: at most one per page, for console lines that start with `[error]`. `broken_layout`: `OverflowX`.
- A quote is at most 120 runes around the match, passed through `execx.Redact`.

`readQAVerdict`:
- It reads `<taskDir>/<runRel>/verdict.json` the way `readFresh` does. Every problem's `file` is `<runRel>/verdict.json`.
- Shot and probe paths must be `filepath.IsLocal`, and they resolve inside the run folder.
- For a probe URL to pass, `config.LocalHost(host)` must hold and its port must equal `want.Port`.
- Each acceptance id in `want.Checks` is covered by a page, a blocked step or a `not_shown` entry.

`qaOutcome`:
- The blocking classes are `{escaped_markup, raw_i18n_key, error_page}`, plus every P1 or P2.
- It dedupes by `Fingerprint`, keeping the factory's copy.
- With no page, the outcome is `unavailable`.

- [ ] **Step 4: Run them to see them pass**

Run: the Step 2 command, then `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -race ./internal/factory/`
Expected: PASS.

- [ ] **Step 5: Gate, API doc, commit**

```bash
git add internal/factory/qa_verdict.go internal/factory/qa_probe.js internal/factory/qa_verdict_test.go internal/factory/testdata/qa docs/API.md
git commit -m "The factory reads a QA walker's verdict and finds escaped markup, missing translations and error pages itself"
```

---

### Task 3: The server's proven stop, no browser tab from a server, and the factory's Chrome

**Files:**
- Modify: `internal/factory/server.go`: `spawnServer`'s environment, `stopServer` with `server_stop` and the proof, `Reconcile` for browsers
- Create: `internal/factory/browser.go`: `StartBrowser`, `StopBrowser`, the browser records, the lsof parsers
- Modify: `internal/factory/engine.go` (the `Copies` interface), `internal/factory/copies.go` (the seams in `copySet` and `NewCopies`)
- Modify: `internal/paths/paths.go` (`FactoryShims`, `FactoryQALock`)
- Modify: `internal/factory/engine_test.go` (`fakeCopies` gains the browser methods and records `startedBrowsers`, `stoppedBrowsers`, and `stopCtxErr` for each stop)
- Test: `internal/factory/server_test.go`, `internal/factory/browser_test.go`

**Interfaces:**
- Consumes: `config.FactoryQA.ServerStop`, `config.Factory.Browser`, `config.DefaultFactoryBrowsers` (Task 1).
- Produces:
  ```go
  // Copies (engine.go) gains:
  StartBrowser(ctx context.Context, c Copy, dir string) (cdp string, err error) // profile <dir>/chrome; cdp "http://127.0.0.1:<port>"
  StopBrowser(ctx context.Context, c Copy) error                               // group kill, profile deleted, record forgotten
  // paths:
  func (l Layout) FactoryShims() string  // <State>/factory-shims (holds the `open` shim)
  func (l Layout) FactoryQALock() string // <State>/factory-qa.lock
  // copySet seams (tests replace them):
  listeners    func(ctx context.Context) ([]listener, error) // lsof -nP -iTCP -sTCP:LISTEN -Fpcn, then lsof -a -d cwd -p <pids> -Fpn
  dial         func(ctx context.Context, addr string) error  // nil error = something answers
  spawnBrowser func(ctx context.Context, bin string, args []string, log string) (pid int, err error)
  exists       func(path string) bool
  type listener struct { PID, Port int; Start, Command, Cwd string }
  func serverEnv(base []string, shims string, unset []string) []string // PATH=<shims>:<PATH>, BROWSER=true, unset dropped
  func parseLsofListeners(out []byte) []listener
  func parseLsofCwd(out []byte) map[int]string
  const (serverStopCap = 30 * time.Second; browserBoot = 30 * time.Second; browserKeyPrefix = "factory.browser.")
  ```

- [ ] **Step 1: Write the failing tests**

```go
func TestTheServersPathFindsTheOpenShimFirst(t *testing.T) {
    env := serverEnv([]string{"PATH=/usr/bin:/bin", "GH_TOKEN=x"}, "/state/factory-shims", []string{"GH_TOKEN"})
    // contains "PATH=/state/factory-shims:/usr/bin:/bin" and "BROWSER=true"; no GH_TOKEN
}
func TestAServerStartsWithNoWayToOpenABrowserTab(t *testing.T) {
    // StartServer on a copySet with a fake spawn: <layout>/state/factory-shims/open exists, mode 0o755;
    // running it with /bin/sh and args "http://x" exits 0 and its stderr contains "refused"
}
func TestAServerStopRunsServerStopBeforeTheGroupKill(t *testing.T)   // repo qa.server_stop = "bin/overmind-kill": the runner sees mise … bin/overmind-kill
                                                                     // (Timeout 30s) before kill(pgid, SIGTERM); events in that order
func TestAServerStopKillsAListenerLeftInTheCopy(t *testing.T)        // after the group is gone, listeners returns {PID 777, Cwd copy+"/tmp", Port 3110};
                                                                     // 777 (same start in the process table) gets SIGTERM; then the proof passes;
                                                                     // an event names "a listener the group left"
func TestAServerStopLeavesAListenerOutsideTheCopyAlone(t *testing.T) // {PID 888, Cwd "/elsewhere", Port 9999} is never signalled
func TestAServerStopThatCannotFreeThePortFails(t *testing.T)         // dial("127.0.0.1:3110") keeps answering → the error contains "listening on :3110";
                                                                     // a factory.server_stop fail event
func TestParseLsofListenersAndCwd(t *testing.T)                      // fixtures "p777\ncruby\nn*:3110\n", "p777\nfcwd\nn/copies/factory1\n"
func TestStartBrowserRecordsChromeAndReadsItsDevToolsPort(t *testing.T) {
    // the fake spawnBrowser writes <dir>/chrome/DevToolsActivePort "9333\n/devtools/browser/x\n"
    // cdp == "http://127.0.0.1:9333"; a factory.browser.factory1 record with pid and start;
    // args contain "--headless=new", "--user-data-dir="+dir+"/chrome", "--window-size=1280,800", "--remote-debugging-port=0",
    // "--remote-debugging-address=127.0.0.1", "--use-mock-keychain", "--no-first-run", "about:blank"
}
func TestStartBrowserWithNoChromeNamesTheKey(t *testing.T)                 // exists → false everywhere → the error contains "[factory] browser"
func TestStartBrowserThatNeverListensIsStoppedAndForgotten(t *testing.T)   // no DevToolsActivePort within browserBoot (fake sleep) → group killed,
                                                                           // no record, the error names the browser's log
func TestStopBrowserEndsItsGroupAndDeletesTheProfile(t *testing.T)
func TestReconcileKillsAStrayBrowserOnlyWhenPidAndStartMatch(t *testing.T) // mirrors TestReconcileKillsAStrayServerOnlyWhenPidAndStartTimeMatch
```

- [ ] **Step 2: Run them to see them fail**

Run: `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -run 'Shim|ServerStop|ServersPath|Lsof|Browser' ./internal/factory/`
Expected: FAIL to compile (`serverEnv`, `listener` and `StartBrowser` are undefined).

- [ ] **Step 3: Implement**

- `spawnServer` sets `cmd.Env = serverEnv(os.Environ(), c.cfg.Layout.FactoryShims(), c.unset())` and adds `BROWSER=true` to the env map that it passes to mise.
- `StartServer` writes the shim first, with `ensureOpenShim(dir string) error`, 0o755:
  ```sh
  #!/bin/sh
  printf 'magnum factory: refused `open %s`: a copy'\''s server opens no browser tab\n' "$*" >&2; exit 0
  ```
- `stopServer(ctx, name, why)` runs these steps in order. A failed step does not stop the next one (`errors.Join`):
  1. If the repo's `QA` sets `server_stop`, run it, bounded by `serverStopCap`. A failure is an event, and the stop goes on.
  2. `end(rec)` as today.
  3. `proveStopped(ctx, name)`:
     - It dials `127.0.0.1:<port>` and `[::1]:<port>`. It also lists the listeners that have their `Cwd` in the copy's path, or that listen on the copy's port.
     - It kills each of them only while its pid and start match the process table: SIGTERM, then SIGKILL after `serverStopGrace`.
     - Then it checks again. Anything still there is the error `the server of <copy> left pid <n> (<command>) listening on :<port>`.
  
  If no record is left, the proof still runs. That is the leader-gone case, where overmind's tmux children still listen.
- Chrome:
  - The binary is `cfg.Factory.Browser`, else the first of `DefaultFactoryBrowsers` that `exists`.
  - The args, pinned: `--headless=new --disable-gpu --no-first-run --no-default-browser-check --disable-extensions --disable-sync --disable-background-networking --disable-component-update --metrics-recording-only --password-store=basic --use-mock-keychain --renderer-process-limit=2 --window-size=1280,800 --remote-debugging-address=127.0.0.1 --remote-debugging-port=0 --user-data-dir=<dir>/chrome about:blank`.
  - It spawns with `Setpgid` and logs to `<Logs>/factory-browser-<copy>.log`.
  - The record is a `serverRecord` (`Path` is `dir`) under `browserKeyPrefix+copy`.
  - It polls `DevToolsActivePort` with `c.sleep` every 100ms, up to `browserBoot`.
- `Reconcile` stops every `factory.browser.*` record as it stops the servers.

- [ ] **Step 4: Run them to see them pass**

Run: the Step 2 command, then `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -race ./internal/factory/ ./internal/paths/`
Expected: PASS. The existing `server_test.go` and `copies_test.go` stay green.

- [ ] **Step 5: Gate, API doc, commit**

```bash
git add internal/factory/server.go internal/factory/browser.go internal/factory/copies.go internal/factory/engine.go internal/paths/paths.go internal/factory/*_test.go docs/API.md
git commit -m "A copy's server stops with proof that its port is free and opens no browser tab, and the factory runs its own headless Chrome"
```

---

### Task 4: The QA session: claim, boot, teardown on every path, and the restart

**Files:**
- Create: `internal/factory/qa.go`: `runQA` up to a ready session, the session, the teardown, `cutQA`
- Create: `internal/factory/qa_test.go`: `qaRig` on `newReviewRig`
- Modify: `internal/factory/engine.go`:
  - `New` registers `e.runners[config.FactoryStageQA] = e.runQA`.
  - the fields `qaSessions map[int64]*qaSession` and `qaProbe`.
  - `RunLocked` runs `defer e.stopQASessions(context.WithoutCancel(ctx))`.
  - `advance` moves through `e.nextStage`.
  - `Tick` calls `e.sweepQA`.
- Modify: `internal/factory/holds.go`:
  - `heavyStage` is true for `qa` too.
  - `adoptHeavy` skips qa attempts, as it skips review's.
  - `freeHeavy` calls `e.endQASession`.
- Modify: `internal/factory/recover.go` (`startup` calls `e.cutQA` after `Copies.Reconcile`, before `adoptHeavy`)
- Modify: `internal/factory/coding_test.go` (`codingRig.script` gains `case "bin/user-facing"`, answered by `r.userFacing func(stdin []byte) (string, int)`)

**Interfaces:**
- Consumes:
  - Task 1: `FactoryQA`, `FactoryStageQA`, `PageURL`.
  - Task 2: `QAOutcome*`, `qaProbeScript`.
  - Task 3: `Copies.StartBrowser`/`StopBrowser`, `Layout.FactoryQALock`.
  - The existing code: `e.startServer`, `e.stopServer`, `e.heavy`, `e.mayLaunch`, `engine.AcquireLock`, `e.toOperator` and `retryHint`.
  - Also existing: `e.claimCopy`, `e.branchHead`, `e.headMoved`, `e.d.Git.ChangedFiles`, `miseArgs`, `unsetEnv` and `writeJSON`.
- Produces:
  ```go
  const (qaRunFile = "qa-run.json"; qaSummaryFile = "qa-summary.json"; qaResultFile = "result.json"; qaVerdictFile = "verdict.json";
         qaProbeFile = "probe.js"; qaChangedFile = "changed.txt"; heavyQA = "qa"; waitQA = "qa";
         qaProbeEvery = 2 * time.Second; qaWhenCap = 2 * time.Minute)
  func qaRunDir(k int, head string) string // "qa/<k>-<sha7>", relative to the task's directory
  type qaRun struct {                      // qa-run.json: the newest run of the task
      N, Port, MaxPages, AttemptsBefore, BootTries int
      Head, Base, Dir, URL, Ready, CDP, Nonce string
      Changed []string
      Started time.Time
  }
  type QAResult struct {                   // <run>/result.json; qa-summary.json holds the one that passed the stage
      Version, Run, Pages, Shots int
      Dir, Head, Outcome, Why string
      Blocking, Known []QADefect
      Blocked []QABlocked
      Started, Ended time.Time
  }
  type qaSession struct { task int64; run qaRun; copy Copy; ready, browser bool; err error; cancel context.CancelFunc;
                          done chan struct{}; release, unlock func() }
  qaProbe func(ctx context.Context, url, host string) (status int, err error) // Engine seam; nil = HTTP GET dialing 127.0.0.1:<port>
                                                                             // (then [::1]), Host header = host, redirects not followed, 5s timeout
  func (e *Engine) runQA(ctx context.Context, t registry.Task, st config.FactoryStage) (bool, error)
  func (e *Engine) nextStage(t registry.Task, kind config.FactoryKind, i int) (name string, skipped []string)
  func (e *Engine) startQASession(ctx context.Context, t registry.Task, c Copy, run qaRun) error
  func (e *Engine) endQASession(ctx context.Context, t registry.Task, why string) error
  func (e *Engine) stopQASessions(ctx context.Context)
  func (e *Engine) sweepQA(ctx context.Context, tasks []registry.Task)
  func (e *Engine) cutQA(ctx context.Context, tasks []registry.Task)
  func (e *Engine) qaUnavailable(ctx context.Context, t registry.Task, run qaRun, why string) error
  func (e *Engine) qaTold(ctx context.Context, t registry.Task, res QAResult) (bool, error)
  func readQARun(dir string) (qaRun, bool)
  func readQASummary(dir string) (QAResult, bool) // qa-summary.json
  func (e *Engine) writeQAResult(ctx context.Context, t registry.Task, run qaRun, res QAResult) error // result.json, and CheckQA
  ```

`runQA`'s pass, in this order. These are the rules the tests pin.

1. **No QA block, or no runtime, copies or git.**
   - The repo has no `QA` block: write `qa-summary.json` with outcome `not_needed`, Why "the repository has no [factory.repo.qa]", at `t.HeadSHA`, and pass.
   - The build has no `Runtime`, `Copies` or `Git`: wait, with `false, nil`.
2. **Copy and head.** Run `claimCopy`, then `branchHead`. When the head is not `t.HeadSHA`, call `e.headMoved`.
3. **The newest run's result.** Read the run with `readQARun`. If it is at `t.HeadSHA` and has a `result.json`, act on its outcome:
   - `clean` or `not_needed`: pass (`qa-summary.json` was written already).
   - `unavailable`, or `defects` (from Task 5 on): `qaTold`. When you were not told, move the task to NEEDS YOU now. When you were told, this is your retry, and a new run starts.
   - `pressure`: wait on the memory hold while it stands. Then a new run starts, unless 2 pressure results exist at this head. Then it is `unavailable`.
   - `interrupted`: a new run starts, unless 2 interrupted results exist at this head. Then it is `unavailable`.
4. **A new run** (k = the last k + 1):
   1. Read the changed files: `ChangedFiles(base...head)`, every status.
   2. Run `when` through `e.run`: `mise` args in the copy, stdin is the files one per line, `Timeout: qaWhenCap`, `Unset: unsetEnv`.
      - A non-zero exit or a run error → `qaUnavailable`: "the user-facing check `<when>` exited N: <first stderr line, redacted, clipped 200>".
      - An empty stdout → `QAResult{Outcome: not_needed}`, written as the summary at the head. CheckQA `skipped`. Pass.
   3. Write `qa-run.json`, `<run>/changed.txt` and `<run>/probe.js`. The nonce is 16 random bytes in hex, and `AttemptsBefore` comes from `attemptsOf`.
5. **Start the session.** With no session for the task, `startQASession` does these steps in order:
   1. `mayLaunch`.
   2. `e.heavy(ctx, t, heavyQA)`.
   3. `engine.AcquireLock(FactoryQALock)`. While another holds it, the task waits `qa`, "another QA runs on this Mac".
   4. The port check: `qaProbe(url+ready)` answers before the start (any status) → `qaUnavailable` "port <p> already answers before the copy's server started: another server holds it".
   5. `e.startServer`.
   6. A boot goroutine on `bgCtx` (`e.bg`) probes `url+ready` every `qaProbeEvery` (through `e.after`) until 2xx/3xx or `Boot`. It has its own `recover`, which turns a panic into `sess.err`.
   
   Any failure or panic before the goroutine runs releases what was taken (a deferred cleanup on `ok == false`). The task waits `qa`, "the copy's server boots (up to <boot>)".
6. **A failed boot.** Stop the server (`e.stopServer`, so the proof runs). On the first failure, increment `run.BootTries` and start again. On the second, `qaUnavailable` "the copy's server did not answer <ready> within <boot>".
7. **The boot answered.** Run `mayLaunch` again, then `Copies.StartBrowser(ctx, c, <task dir>/<run dir>)`, and write `CDP` into `qa-run.json`. In this task the stage then waits `qa`, "ready for the walk". Task 5 continues from here.

The teardown:
- `endQASession` always uses `context.WithoutCancel`. It cancels and waits for the boot goroutine, then runs `StopBrowser`, `e.stopServer`, the unlock and the heavy release. A failed step does not stop the next one. It writes a `qa` event: ok, or fail with the errors.
- It is called from:
  - every end of a run.
  - `freeHeavy` (NEEDS YOU, stop, drop, done, a panic sent to you).
  - `sweepQA` (a live task no longer at the session's stage).
  - `stopQASessions`.

The rest:
- `qaUnavailable` writes `result.json` (outcome `unavailable`, Why) and a CheckQA `fail` with cause `env`. Then it runs `endQASession`, then `e.toOperator(ctx, t, "QA could not run for a user-facing change: "+why+retryHint(t), "QA could not run")`.
- `qaTold` mirrors `toldOfStop`: an `alert` event at or after `res.Ended`.
- `nextStage` skips a `qa` stage of a repo without a `QA` block, with the `stage` event "qa skipped: example/shop has no [factory.repo.qa]".
- `cutQA` handles each live or NEEDS YOU task at a qa-type stage whose newest run has no `result.json`. It writes the outcome `interrupted`, with the Why `the daemon stopped during the run: its server and browser were stopped`. It also fails the stage's open attempts with N > `AttemptsBefore`.

- [ ] **Step 1: Write the failing tests**

```go
// qaRig: newReviewRig + repo QA {When "bin/user-facing", URL "http://app.example.localhost:{port}", Ready "/up", Boot 5m, Cap 50m,
// MaxPages 12, Fixes 2}, repo Server "bin/dev"; a kind "bug" whose stages are the default plus {qa, qa, [fx-qa], qa} after local_review;
// qaProbe answers from a script; e.after returns a closed channel; each probe call moves the clock 1m.
func TestARepoWithoutAQABlockSkipsTheQAStage(t *testing.T)      // after a clean local review the stage is pr_body; event "qa skipped: example/shop has no [factory.repo.qa]"
func TestAChangeWithNoUserFacingFileNeedsNoQA(t *testing.T)     // userFacing → "" exit 0: qa-summary {Outcome not_needed, Head H}; no StartServer; stage pr_body;
                                                                // CheckQA skipped; userFacing saw "app/credit.rb\n" on stdin
func TestAFailingWhenCommandNeedsYouNotNotNeeded(t *testing.T)  // exit 2: NEEDS YOU, WaitsWhat contains "QA could not run" and
                                                                // fmt.Sprintf("magnum factory retry %d", id); no StartServer; no qa-summary.json
func TestQAStartsTheServerThenWaitsForItToAnswer(t *testing.T)  // probe: refused before the start, then 503, then 200 → StartServer 1, StartBrowser 1
                                                                // with dir <task>/qa/1-<sha7>; qa-run.json has CDP "http://127.0.0.1:9333", a
                                                                // 32-char nonce; probe.js contains it; changed.txt lists the user-facing files
func TestQARefusesAPortThatAnswersBeforeTheServerStarts(t *testing.T) // probe answers 200 before any StartServer → no StartServer; NEEDS YOU
                                                                      // "port 3001 already answers"
func TestAServerThatNeverAnswersNeedsYouWithTheRetryCommand(t *testing.T) {
    // probe refused always: StartServer 2, StopServer 2, StartBrowser 0; result.json Outcome unavailable, Why contains "did not answer /up within 5m0s";
    // NEEDS YOU with "magnum factory retry"; CheckQA fail cause env; engine.AcquireLock(FactoryQALock) succeeds after (lock free);
    // e.heavyHeld has no entry for the task
}
func TestAServerThatFailsToStartGetsOneRetry(t *testing.T)      // fakeCopies.StartServer errors twice → as above, Why names the start error
func TestOneQAAtATimeOnTheMac(t *testing.T)                     // the test holds FactoryQALock: the task waits "qa", "another QA runs on this Mac";
                                                                // no StartServer; released → the next tick starts
func TestQATeardownRunsWhenTheBootProbePanics(t *testing.T)     // qaProbe panics after the start: the next tick StopServer 1, lock free, heavy free,
                                                                // NEEDS YOU, Why contains "panic"
func TestQATeardownRunsWhenTheDaemonStops(t *testing.T)         // a ready session; e.stopQASessions(cancelled ctx) → StopBrowser 1, StopServer 1,
                                                                // each with stopCtxErr == nil
func TestQATeardownRunsWhenTheTaskGoesToYou(t *testing.T)       // a ready session; e.toOperator → StopBrowser 1, StopServer 1
func TestARestartDuringQARunsAFreshRun(t *testing.T)            // a ready session; r.restart(): qa/1-<sha7>/result.json interrupted;
                                                                // the next ticks start qa/2-<sha7>
func TestARetryAfterUnavailableStartsAFreshRun(t *testing.T)    // after the NEEDS YOU of the never-answers case: ReqRetry → run 2 starts;
                                                                // without the alert event (crash before the move) the next pass moves to you instead
```

- [ ] **Step 2: Run them to see them fail**

Run: `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -run 'QA|UserFacing|WhenCommand|ServerThat' ./internal/factory/`
Expected: FAIL to compile (`runQA` and `qaRun` are undefined).

- [ ] **Step 3: Implement `qa.go` and the hooks**, with the signatures and rules above.

- [ ] **Step 4: Run them to see them pass**

Run: the Step 2 command, then `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -race ./internal/factory/`
Expected: PASS. The existing review, recover and holds tests stay green: the default kinds have no qa stage yet.

- [ ] **Step 5: Gate, API doc, commit**

```bash
git add internal/factory/qa.go internal/factory/qa_test.go internal/factory/engine.go internal/factory/holds.go internal/factory/recover.go internal/factory/coding_test.go docs/API.md
git commit -m "The factory's QA stage starts the copy's server alone on the Mac, and stops it and its browser on every way out"
```

---

### Task 5: The walk and the gate: the walker's turn, the factory's verdict, and no card without QA at the head

**Files:**
- Modify: `internal/factory/qa.go`: the walk, `evaluateQA`, the cap, the pressure, `qaBlocks` (NEEDS YOU in this task)
- Modify: `internal/factory/stage_agents.go`:
  - `agentsEnds` gains `FactoryEndQA`.
  - `agentsTurn.qa *qaWalk`.
  - `runTurn` passes `again` for a qa walk.
  - For `st.End == FactoryEndQA`, `promptData` fills the QA fields from `qa-run.json`.
  - `outputs` returns the run's `verdict.json` for `FactoryEndQA`.
- Modify: `internal/factory/endfacts.go` (`checkEnd` case `FactoryEndQA` → `checkQAEnd`)
- Modify: `internal/factory/stage_gate.go` (for a qa walk, `run.turn.qa != nil`, `roleAsks` returns `askNone`)
- Modify: `internal/factory/stage_ready.go` (`readyProblem` adds `e.qaProblem(t)`)
- Modify: `internal/config/factory.go`: `DefaultFactoryKinds` gets `{Name: "qa", Type: FactoryStageQA, Roles: []string{"fx-qa"}, End: FactoryEndQA}` after `local_review` in every kind.
- Modify: `config.defaults.toml`: the same line in the 3 kinds, and this line in the stage-type comment: `qa  starts the copy's server and a headless Chrome; the roles walk the changed pages ([factory.repo.qa]); ends when the factory's checks of the saved pages find nothing that blocks`.
- Test: `internal/factory/qa_test.go`, `internal/factory/stage_ready_test.go`

**Interfaces:**
- Consumes:
  - Task 2: `readQAVerdict`, `readQAProbe`, `detectQA`, `qaOutcome`, `qaWant`.
  - Task 4: `qaRun`, `QAResult`, `readQARun`, `writeQAResult`, `endQASession`, `qaUnavailable`, `qaTold`, `qaSession`.
- Produces:
  ```go
  type qaWalk struct{ run qaRun }                        // agentsTurn.qa
  func checkQAEnd(dir string, since time.Time) *endProblem // qa-run.json → readQAVerdict with qaWant{Head, Nonce, Port, MaxPages, Checks from acceptance.json}
  func (e *Engine) walkDone(ctx context.Context, t registry.Task, st config.FactoryStage, run qaRun) (bool, error) // an attempt of st with N > AttemptsBefore verified
  func (e *Engine) evaluateQA(ctx context.Context, t registry.Task, run qaRun) (QAResult, error) // verdict + detectQA on every page + qaOutcome
  func (e *Engine) qaBlocks(ctx context.Context, t registry.Task, st config.FactoryStage, run qaRun, res QAResult) (bool, error) // Task 6 replaces its body
  func (e *Engine) qaProblem(t registry.Task) string     // "" or why no ready card
  ```

The walk's rules:
- A ready session whose walk has not ended runs `e.runTurn(ctx, t, st, agentsTurn{qa: &qaWalk{run}})`, with `stageAttempt(…, again: true)` for a qa walk.
- On each pass:
  - When `now - run.Started > Cap`: `qaUnavailable` "over its cap of <cap>".
  - When `e.holds[HoldMemory]` stands with a reason that starts with "critical": write a `pressure` result, fail the walker's open attempt, and run `endQASession`.
- When `walkDone` holds:
  1. `endQASession` first.
  2. `evaluateQA`.
  3. `writeQAResult`. CheckQA: pass for clean, `fail` with cause `code` for defects, `fail` with cause `env` for unavailable. `Source "factory"`, `Command "qa run <k>"`, `Took` = Ended − Started.
  4. Then, by outcome:
     - clean → write `qa-summary.json` and pass.
     - defects → `qaBlocks`.
     - unavailable (no page reached) → NEEDS YOU, as in Task 4, "QA reached no page: <first blocked why>".
- `qaBlocks` in this task:
  ```go
  e.toOperator(ctx, t, fmt.Sprintf("QA found %s that block%s: %s; see %s%s", plural(n, "defect"), s, classesAndPaths, <run>/result.json, retryHint(t)), "QA found defects")
  ```
  `classesAndPaths` is `escaped_markup on /settings/security`. It names no quote.
- `qaProblem` applies to a task whose repo has a `QA` block and whose kind has a qa stage. It wants `qa-summary.json` with outcome `clean` or `not_needed` and `Head == t.HeadSHA`. Else it says `QA did not pass at <sha7>`, or `QA looked at <sha7>, another head than <sha7>`.

- [ ] **Step 1: Write the failing tests**

```go
// the fake walker: r.walk(task, verdict QAVerdict, probes map[string]string) writes pages/NN-*.png (PNG magic), the probe JSONs with the
// run's nonce, verdict.json, then r.ended(task, "fx-qa")
func TestTheWalkerGetsTheBrowserTheRunAndTheAcceptanceChecks(t *testing.T) // fx-qa launched in the qa stage; its prompt contains the CDP URL,
                                                                           // <task>/qa/1-<sha7>/verdict.json, probe.js, acceptance.json; no other role
func TestACleanWalkPassesQAAndStopsEverything(t *testing.T) {
    // the clean-tooltip probe: StopBrowser 1 and StopServer 1 BEFORE the stage moves; qa-summary {Outcome clean, Head H, Pages 1};
    // stage pr_body; CheckQA pass at H; lock and heavy free; a questions-fx-qa.json the walker also wrote suspended nothing
}
func TestTheEscapedTooltipBlocksQAWhateverTheWalkerSays(t *testing.T) // verdict with no defects + the escaped-tooltip probe → result defects,
                                                                      // Blocking[0].Class escaped_markup Source factory; the stage is not pr_body;
                                                                      // no qa-summary.json; CheckQA fail code (all of it stays true after Task 6)
func TestQABlocksAVisibleDefectTheReviewLeftAsP3(t *testing.T)        // review-summary.json Leftovers [{Severity "P3", Title "dates show HTML"}] at H
                                                                      // + the escaped probe → result defects; review-summary.json unchanged
func TestQADefectsNeedYouUntilTheFixLoopLands(t *testing.T)           // the escaped probe → NEEDS YOU with "escaped_markup on /settings/security";
                                                                      // WaitsWhat has no "<span" (Task 6 replaces this test)
func TestAnInvalidVerdictGetsOneNudgeThenNeedsYou(t *testing.T)       // a page on http://staging.example.com: fx-qa's second prompt starts with nudgeLead
                                                                      // and names verdict.json; a second invalid one → NEEDS YOU "the fx-qa agent wrote
                                                                      // no valid qa/1-<sha7>/verdict.json"; StopServer 1
func TestAWalkThatReachesNoPageNeedsYou(t *testing.T)                 // pages [] blocked [{What "the security tab", Why "sign-in failed"}] → unavailable,
                                                                      // NEEDS YOU "QA reached no page: sign-in failed"
func TestQAOverItsCapNeedsYou(t *testing.T)                           // the clock moves 51m during the walk → NEEDS YOU "over its cap of 50m0s"; teardown ran
func TestCriticalPressureStopsTheWalkAndRetriesOnceAfterIt(t *testing.T) // a critical memory hold: teardown, result pressure, the walker's attempt failed;
                                                                         // the hold lifts → run 2; a second pressure → NEEDS YOU
func TestARestartMidWalkFailsTheWalkersAttempt(t *testing.T)          // restart: run 1 interrupted, its fx-qa attempt failed; run 2 begins a new attempt
func TestNoReadyCardWithoutQAAtTheHead(t *testing.T) {
    // repo with a QA block: no qa-summary → readyProblem says "QA did not pass at <sha7>"; a summary at an older head → names both heads;
    // a clean summary at H → ""; a repo without a QA block and no summary → ""
}
func TestTheDefaultKindsRunQAAfterTheLocalReview(t *testing.T)        // config.Defaults(): in bug, feature and chore, the stage after local_review is
                                                                      // {qa qa [fx-qa] qa}
```

- [ ] **Step 2: Run them to see them fail**

Run: `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -run 'Walk|Walker|QA|ReadyCardWithoutQA|DefaultKindsRunQA' ./internal/factory/ ./internal/config/`
Expected: FAIL. The walker is never launched, and the default kinds have no qa stage.

- [ ] **Step 3: Implement the walk, the end fact, the gate and the default kinds**, with the signatures and rules above. `TestFactoryGoDefaultsEqualTheEmbeddedFile` keeps the Go and TOML kinds in step.

- [ ] **Step 4: Run them to see them pass**

Run: the Step 2 command, then `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -race ./internal/factory/... ./internal/config/`
Expected: PASS. Some tests assert `Stage == "pr_body"` after a clean review (`review_left_test.go`, `recover_test.go`). They stay green: their repo has no QA block, so `nextStage` skips the stage.

- [ ] **Step 5: Gate, API doc, commit**

```bash
git add internal/factory/qa.go internal/factory/qa_test.go internal/factory/stage_agents.go internal/factory/endfacts.go internal/factory/stage_gate.go internal/factory/stage_ready.go internal/factory/stage_ready_test.go internal/config/factory.go internal/config/factory_test.go config.defaults.toml docs/API.md
git commit -m "The QA walker walks the changed pages, the factory's own checks of what it saved decide, and no ready card comes without QA at the head"
```

---

### Task 6: The QA fix loop: the writer's turn, the review again, the cap

**Files:**
- Create: `internal/factory/qa_fix.go`, `prompts/factory-qa-fix.md`
- Modify: `internal/factory/qa.go` (`qaBlocks` → `e.qaFixPhase`)
- Modify: `internal/factory/review.go`: `reviewTurn` gains `reply string`, and `func (rt *reviewTurn) replyFile() string { return cmp.Or(rt.reply, reviewReplyFile(rt.n)) }` replaces `reviewReplyFile(rt.n)` in `replyProblem` and `commitProblem`.
- Modify: `internal/factory/stage_agents.go`:
  - `isFixTurn(st)` replaces `st.Prompt != ReviewFixPrompt` in `passStage` and `promptData`.
  - `outputs` and `promptData` handle `QAFixPrompt` (`d.Review` is the defects file, `d.ReviewReply` the reply file).
  - `beforeLetGo` uses `run.turn.review.replyFile()`.
  - `CheckPrompts` and `PromptNames` add `QAFixPrompt`.
- Modify: `prompts/README.md` (the `factory-qa-fix.md` row)
- Test: `internal/factory/qa_fix_test.go`, `internal/factory/stage_agents_test.go`

**Interfaces:**
- Consumes:
  - Task 5: `qaBlocks`, `QAResult`, `qaRun`, `evaluateQA`.
  - The existing code: `ReviewFile`, `ReviewItem`, `ReviewReply`, `reviewTurn`, `e.fixDone`, `e.writerRoles`, `e.attemptsOf`, `e.runTurn`.
- Produces:
  ```go
  const QAFixPrompt = "factory-qa-fix.md"
  func qaDefectsFile(k int, head string) string // qaRunDir(k, head)+"/defects-<k>.json" (a ReviewFile: Round k, Head, AttemptsBefore, Findings)
  func qaReplyFile(k int, head string) string   // qaRunDir(k, head)+"/reply-<k>.json" (a ReviewReply)
  func isFixTurn(st config.FactoryStage) bool   // st.Prompt is ReviewFixPrompt or QAFixPrompt
  func newestQADefects(dir string) (k int, head string)
  func (e *Engine) qaFixPhase(ctx context.Context, t registry.Task, st config.FactoryStage, run qaRun, res QAResult) (bool, error)
  func (e *Engine) closeQAFix(ctx context.Context, t registry.Task, run qaRun, res QAResult, rf ReviewFile) (bool, error)
  func (e *Engine) backToReview(ctx context.Context, t registry.Task, because string) error // stage = the kind's review stage before qa, attempt 0
  func (e *Engine) qaFixTurns(t registry.Task) int // the defects-*.json files of the task's runs
  ```

The fix loop's rules:
- **The cap.** At `qaFixTurns(t) >= repo.QA.Fixes`, the task goes to NEEDS YOU with the text `QA still finds <n> defects that block after <fixes> fix turns (<class on path>, …)<; came back after its fix: …>; see <result.json><retryHint>`. No defects file is written.
- **The defects file.** Write `defects-<k>.json`. Its items are `ReviewItem{ID: "Q<i>", Fingerprint, Severity, Title: class+": "+title, Path: first of Paths, Body}`. The Body holds the page path, the state, the shot's absolute path and the quote. If an earlier run's defects file has the same fingerprint, the Body adds `came back after its fix`.
- **The fix turn.** It is `config.FactoryStage{Name: st.Name, Type: FactoryStageAgents, Roles: e.writerRoles(t), End: FactoryEndProof, Prompt: QAFixPrompt}`, run with `agentsTurn{review: &reviewTurn{n: k, reviewed: run.Head, live: ids, reply: qaReplyFile(k, run.Head)}}`, until an attempt N > AttemptsBefore is verified. That is `fixDone` on a `ReviewFile`. The proof and the precheck run at the new head through the existing proof end fact.
- **`closeQAFix`.** The reply's declines with a risk become `Known` items titled `declined: … The risk: …`.
  - Head unchanged (every defect declined with its risk): write `qa-summary.json` as `clean` at the head, with the declines in Known, and pass.
  - Head moved: call `backToReview` with "QA's fix at <sha7>: the local review and QA run again". The review loop starts afresh, since its last cycle reviewed another head, and QA runs next.

`prompts/factory-qa-fix.md` follows `factory-review-fix.md`, with these differences:
- `{{.Review}}` lists QA's defects, each with its class, page, state, shot and quote.
- The writer fixes a defect test first, with a request or view spec. The spec fails without the fix and asserts on the rendered body: no escaped markup and no `translation missing`.
- A decline needs its risk.
- The writer commits on `{{.Branch}}`, never pushes, and writes `{{.ReviewReply}}`, `{{.Proof}}` and `{{.Questions}}` last.

- [ ] **Step 1: Write the failing tests**

```go
// Replace Task 5's TestQADefectsNeedYouUntilTheFixLoopLands with this test:
func TestQADefectsGoToTheWriterAsAFixTurn(t *testing.T) {
    // the escaped probe with walker Paths ["app/views/settings/_keys.html.erb"]: defects-1.json Findings[0] == {ID "Q1", Severity "P2",
    // Title "escaped_markup: …", Path "app/views/settings/_keys.html.erb"}, Body contains "/settings/security" and the shot path;
    // fx-writer launched in the qa stage; its prompt contains <task>/qa/1-<sha7>/defects-1.json and reply-1.json
}
func TestAQAFixGoesBackThroughTheReviewThenQA(t *testing.T) {
    // the writer commits, writes proof.json and reply-1.json "fixed" → the proof and precheck scripts ran at the new head N; t.HeadSHA == N;
    // stage local_review; event "back to local_review"; a second review round with head N; after it is clean the stage is qa; run qa/2-<N7>;
    // a clean walk → pr_body; qa-summary.Head == N
}
func TestADeclinedQADefectEndsTheStageWithoutALoop(t *testing.T) // the reply declines Q1 with risk "the key is shared with the mailer", no commit →
                                                                 // stage pr_body; qa-summary clean, Known[0].Title starts "declined:"; the review
                                                                 // calls are unchanged
func TestAQADeclineWithoutARiskNeedsACommit(t *testing.T)        // declined without a risk and no commit → the writer's next prompt contains
                                                                 // "commit your fixes, or decline each finding with its risk"
func TestQAFixTurnsStopAtTheCap(t *testing.T)                    // fixes 2; defects in runs 1, 2 and 3 → after run 3, NEEDS YOU "after 2 fix turns" and
                                                                 // "came back after its fix"; no defects-3.json
func TestCheckPromptsRendersTheQAFixPrompt(t *testing.T)         // CheckPrompts(config.Defaults()) == nil; PromptNames contains QAFixPrompt
func TestAReviewFixTurnStillRepliesInItsOwnFile(t *testing.T)    // reviewTurn{n: 2}.replyFile() == "review-reply-2.json" (the generalization keeps the review's name)
```

- [ ] **Step 2: Run them to see them fail**

Run: `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -run 'QAFix|QADefects|QADecline|DeclinedQA|ReviewFixTurnStill' ./internal/factory/`
Expected: FAIL. `QAFixPrompt` is undefined, and the defects go to NEEDS YOU.

- [ ] **Step 3: Implement `qa_fix.go`, the prompt and the generalization**, with the signatures and rules above.

- [ ] **Step 4: Run them to see them pass**

Run: the Step 2 command, then `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -race ./internal/factory/... ./prompts/`
Expected: PASS. The whole of `review_test.go` stays green, because the review's reply file names are unchanged.

- [ ] **Step 5: Gate, API doc, commit**

```bash
git add internal/factory/qa_fix.go internal/factory/qa_fix_test.go internal/factory/qa.go internal/factory/review.go internal/factory/stage_agents.go internal/factory/stage_agents_test.go prompts/factory-qa-fix.md prompts/README.md docs/API.md
git commit -m "QA's defects go to the writer for a fix, then through the local review and QA again, at most fixes times"
```

---

### Task 7: The ready card and the PR body say what QA found, and the docs

**Files:**
- Modify: `internal/factory/stage_ready.go`:
  - `ReadyCardFile` gains `QA *QAResult json:"qa,omitempty"`.
  - `drawCard` reads `qa-summary.json` at the card's head and appends FLAGS lines.
  - `appendKnown` appends `qaMarker` and `qaSection(res)`.
- Modify: `internal/factory/qa.go` (`writeQAReport` writes `<run>/summary.md` at each result)
- Modify: `internal/factory/screen.go`:
  - `readyCard` sets `QA`, `Screenshots`, `ScreenshotsNote` and the QA lines in `Leftovers`.
  - `taskCard` sets `QAReport`.
  - `stageBlocker` and the doings name the qa stage.
  - `stageProof` gets the `FactoryEndQA` case.
- Modify the tui:
  - `internal/tui/factory_rows.go`: `ReadyCard.QA string` and `TaskCard.QAReport string`.
  - `internal/tui/factory_card_ready.go`: `field("QA", c.QA, false)` after PROOF.
  - `internal/tui/factory_report.go`: `reportOf` falls back to `QAReport`, named after its file.
  - `internal/tui/factory_keys.go`: with a `QAReport`, the ready card keeps its `e` hint.
- Modify: `README.md` (a `#### Visual QA before the ready card` subsection in the factory section), `docs/DECISIONS.md` (one entry at the END of `## The factory (2026-11-03)`)
- Test: `internal/factory/stage_ready_test.go`, `internal/factory/screen_test.go`, `internal/tui/factory_card_test.go` (new golden `ready-qa-140`)

**Interfaces:**
- Consumes: Task 4 and Task 5 (`QAResult`, `readQASummary`, `qaRunDir`), Task 6 (the declines in `Known`).
- Produces:
  ```go
  const qaMarker = "<!-- magnum factory: visual QA -->"
  const qaBlockedFlag = "QA did not reach: "
  func qaCardLine(res QAResult) string // "clean: 3 pages walked at abc1234, 1 note" | "not needed: no user-facing file changed"
  func qaSection(res QAResult) string  // "## Visual QA\n\n<details>\n<summary>…</summary>\n…</details>\n"; "" without QA
  func writeQAReport(path string, res QAResult, v QAVerdict) error // summary.md: run, head, URL, each page (path, state, absolute shot path), notes, blocked
  ```

The card and the PR body:
- `qaSection` lists pages (path and state), notes (class and title, never a quote), declines and blocked steps.
  - `not_needed` gives "No user-facing file changed: no visual QA ran."
  - `Why` "the repository has no [factory.repo.qa]" gives "".
- `appendKnown` cuts the body at `knownMarker` as today, so a second pass rewrites the Known list and the QA section once each.
- `ScreenshotsNote` is `the after side, from the copy's own server; e opens the QA report`.
- `Recommended`'s Why for a clean card adds "and QA found nothing that blocks".

- [ ] **Step 1: Write the failing tests**

```go
func TestTheReadyCardSaysWhatQAFound(t *testing.T) {
    // qa-summary clean at H: 3 pages, Known [{Class broken_layout, Severity P3, Title "the table scrolls sideways"}],
    // Blocked [{What "the passkey autofill list", Why "headless Chrome draws no autofill"}]:
    // card.QA.Pages == 3; card.Flags contains "QA did not reach: the passkey autofill list (headless Chrome draws no autofill)";
    // screen: c.QA == "clean: 3 pages walked at <sha7>, 1 note", c.Screenshots == 3, c.ScreenshotsNote as pinned,
    // c.Leftovers contains "qa: P3 broken_layout: the table scrolls sideways"
}
func TestTheReadyCardSaysQAWasNotNeeded(t *testing.T)        // not_needed → c.QA == "not needed: no user-facing file changed", c.Screenshots == 0
func TestThePRBodyNotesTheQAResult(t *testing.T)             // appendKnown twice: one knownMarker, one qaMarker, "## Visual QA", "3 pages",
                                                             // the blocked line; no "<span"
func TestThePRBodyHasNoQASectionForARepoWithoutQA(t *testing.T)
func TestEOpensTheQAReportOnTheReadyCard(t *testing.T)       // tui: a ReadyCard task with QAReport "/abs/qa/1-abc1234/summary.md": e runs the pager on it;
                                                             // the hints show "e summary.md"
func TestTheQAStageSaysWhatItDoes(t *testing.T)              // stageBlocker at a qa stage → "visual QA"; stageProof of FactoryEndQA → "QA found nothing that blocks"
// golden: ready-qa-140 (the QA row and SCREENSHOTS "3, the after side, …"); the existing ready goldens are unchanged (an empty QA draws no row)
```

- [ ] **Step 2: Run them to see them fail**

Run: `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -run 'QA|ReadyCard|PRBody' ./internal/factory/ ./internal/tui/`
Expected: FAIL (`card.QA` is undefined).

- [ ] **Step 3: Implement the card, the PR body section, the report and the e key**, with the signatures above. Write the golden with the package's `-update` flag. Read its diff before you commit.

- [ ] **Step 4: Write the docs**

- **README.** A `#### Visual QA before the ready card` subsection covers:
  - the `[factory.repo.qa]` keys with a TOML block (`example/shop`, `http://app.example.localhost:{port}`), which `TestEveryTomlBlockInTheDocsDecodesWithoutUnknownKeys` decodes.
  - `[factory] browser`.
  - the `qa` stage and the `fx-qa` role.
  - one QA at a time (`factory-qa.lock` in the state directory), the `open` shim (`factory-shims/`) and `BROWSER=true`.
  - what blocks.
  - the fix loop and `fixes`.
  - the NEEDS YOU texts and `magnum factory retry <id>`.
  - the card's QA line and `e`.
  - "slice 1 captures the after side only: the before pass and the PR attachments come later".
  - `make build && bin/magnum factory install` after an update.
- **DECISIONS** (the end of `## The factory (2026-11-03)`): "**The factory looks at the changed pages before the ready card (R55 slice 1)** (2026-11-07)". Cover:
  - the motive, an escaped tooltip that a P3 leftover let through.
  - the flow.
  - the gate's independence from the review's severities.
  - the three decisions under Global Constraints.
  
  Rejected:
  - QA before the review (QA binds to the reviewed head).
  - trusting the walker's "clean" (the Go detectors read the probes).
  - a Go CDP library: it is a new dependency, and the walker drives the browser that the factory starts.
  - `pre_existing` without a before pass.
  - the repository's QA skill in slice 1 (its unattended mode is slice 3).
  
  Name the tests of Tasks 2 to 7 that failed first.

- [ ] **Step 5: Run the tests and the gate**

Run: `/usr/bin/lockf -k /tmp/magnum-gate.lock mise exec -- go test -count=1 -race ./internal/factory/... ./internal/tui/ ./internal/config/`, then `make test`, `make lint` and `make api-doc` under the lock.
Expected: PASS. `check-private` passes, and the API doc is current.

- [ ] **Step 6: Commit**

```bash
git add internal/factory/stage_ready.go internal/factory/qa.go internal/factory/screen.go internal/factory/*_test.go internal/tui README.md docs/DECISIONS.md docs/API.md
git commit -m "The ready card and the PR body say what visual QA found, and e opens its report"
```
