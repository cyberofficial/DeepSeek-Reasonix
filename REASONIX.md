# Reasonix project memory

DeepSeek-native AI coding agent for the terminal (Go), engineered around prefix-cache stability — leave it running.

This file is loaded into every session's system prompt (the cache-stable prefix),
so keep it concise and durable — it is the project's standing instructions to the
agent. It is the Reasonix analog of Claude Code's CLAUDE.md.

## Conventions

- Go kernel under `internal/`; each package owns one concern. A package's long
  explanation belongs in its `doc.go`, not spread across implementation files.
- One transport-agnostic `control.Controller` sits behind every frontend (chat
  TUI, HTTP/SSE serve, Wails desktop). Add behavior to the controller, not a
  frontend, so all three inherit it.
- Layering (enforced): utility packages import nothing under `reasonix/`; only
  the frontends `cli`, `serve`, `acp`, `bot`, `botruntime`, `boot` and the hosts
  `cmd/`, `desktop/` may import `control`; nothing below a frontend may import
  one. The declared sets live in `tools/repolint/layers.go`.
- Subagent delegation keeps five concepts apart: a profile says how a worker
  thinks, `TaskSpec` what this call wants, `CapabilityGrant` what it may touch,
  `ContextRequest` what it starts from, `SchedulerPolicy` when it runs. Put a
  field in whichever member decides its value — profiles carry ceilings, never
  per-call values. `internal/agent/profile_boundary_test.go` enforces it.
- Cache-first: the system-prompt prefix (base prompt + tools + memory) must stay
  byte-stable across turns so DeepSeek's automatic prefix cache stays warm. Never
  mutate it mid-session — ride the turn tail instead (see `control.Compose`).
- Performance features land with an effect test at their final boundary
  (`internal/boot/effect_test.go` pattern): assert what actually reaches the
  provider request, frontend sink, or trajectory through the real `boot.Build`
  assembly. Component correctness is not system effectiveness.
- A mutex- or atomic-guarded struct is ratcheted on its **scalar** field count
  (`struct-state`), not its total: independent flags multiply into states no
  type records as legal. Fixing a boundary case by adding one more `bool` is
  the move this blocks — group by lifetime into a named sub-state instead
  (`agent.perTurnState` is the pattern), which costs one field and removes the
  whole product.

## Fork focus

- This repo is a FORK of `esengine/DeepSeek-Reasonix` (original, default branch `main-v2`); our fork is `cyberofficial/DeepSeek-Reasonix`. Working branch: `master`; `origin` = fork, `upstream` = original repo.
- The fork's purpose is the scheduling/loop feature set built here: `/loop` + cron tools (`cron_create`/`cron_list`/`cron_delete`/`schedule_wakeup`), mid-turn steering, per-directory task persistence, `NEXT JOB` status indicator — pursued as a dedicated scheduler initiative (per maintainer guidance, scheduler + cache-first contracts should be their own effort, not CLI polish).
- Keep feature work scoped to this initiative; sync from `upstream/main-v2` regularly; do not push without the user's explicit approval.
- Workflow for ANY change: first pull in changes from the original repo (`git fetch upstream` + merge `upstream/main-v2` into the working branch), fix/repair anything that breaks our fork from the sync (conflicts, build/test regressions), then create the requested code changes on top. Never start feature edits without syncing first.
- README fork notice: after EVERY pull/merge from upstream, verify the fork notice is still present in `README.md` (it is delimited by `<!-- FORK-NOTICE-BEGIN -->` / `<!-- FORK-NOTICE-END -->`). If upstream's README changes overwrote it or a merge conflict resolved it away, re-apply it from the canonical copy below before doing anything else:
  `<!-- FORK-NOTICE-BEGIN -->` + `> [!NOTE]` + `> **This repository is a fork** (cyberofficial/DeepSeek-Reasonix) of [esengine/DeepSeek-Reasonix](https://github.com/esengine/DeepSeek-Reasonix) (upstream, main-v2), tracking upstream main-v2 continuously. It exists to develop the **scheduling / loop feature set** as a dedicated initiative — everything upstream offers remains available, and this fork adds:` + `> - /loop [interval] [prompt] — run a prompt on a cron schedule, or as a dynamic loop the agent re-arms with schedule_wakeup (--forever for an endless loop; tasks expire after 7 days by default)` + `> - /loopdelay [--action] <duration> <prompt|command> — one-shot countdown triggers that fire once after a delay, with --match/--no-ai options for command-based triggers` + `> - cron tools: cron_create, cron_create_action, cron_list, cron_delete, schedule_wakeup, trigger_delay` + `> - mid-turn steering of scheduled prompts, per-directory task persistence (<workspace>/.reasonix/scheduled-tasks.json), and the NEXT JOB status-bar indicator` + `>` + `> Full documentation: [docs/GUIDE.md](docs/GUIDE.md#slash-commands).` + `>` + `> **Latest builds:** grab the latest binaries from the build workflow: https://github.com/cyberofficial/DeepSeek-Reasonix/actions/workflows/build-matrix.yml` + `>` + `> [![Build CLI](https://github.com/cyberofficial/DeepSeek-Reasonix/actions/workflows/build-matrix.yml/badge.svg)](https://github.com/cyberofficial/DeepSeek-Reasonix/actions/workflows/build-matrix.yml)` + `<!-- FORK-NOTICE-END -->` (as blockquote lines like the README original).

## Commands

- Build: `go build ./...` (fast feature check: `go build ./internal/...`)
- Test: `go test ./...`; focused: `go test ./internal/<pkg>/`; suites covering our scheduling features: `./internal/scheduler/ ./internal/control/ ./internal/tool/ ./internal/tool/builtin/ ./internal/boot/ ./internal/store/`
- i18n: adding/renaming a UI string touches ALL THREE locale files (`internal/i18n/messages_en.go`, `messages_zh.go`, `messages_zh_tw.go`) — `go test ./internal/i18n/ -run TestMessagesDrift` enforces sync
- Format/lint: `gofmt -l .`, `go vet ./...`, `go run ./tools/repolint` (comment/size/layering ratchet; CI enforces it)
- Desktop frontend: `cd desktop/frontend && pnpm build` (lint + typecheck + CSS gates), `pnpm test:all`
- Docs/cache guards: `scripts/check-cache-impact.sh`, `scripts/check-docs-impact.sh`

## Architecture

- `internal/control` — the Controller: turns, scheduler binding, slash commands (`/loop`, `/looplist`, `/loopdel`, `/loopstatus`), session lifecycle; `port.go` defines the SessionAPI driving port every frontend uses.
- `internal/scheduler` — cron engine behind `/loop` and the cron_* tools. Tasks persist per working directory in `<workspace>/.reasonix/scheduled-tasks.json` — ONE file per launch directory, shared by chats in that folder, surviving `/new`/`/clear`; 7-day expiry unless `no_expire`.
- `internal/tool` + `internal/tool/builtin` — tool schemas incl. `cron_create`, `cron_list`, `cron_delete`, `schedule_wakeup`; `contract_test.go` gates every registration.
- `internal/boot` — production wiring (Options); `internal/store` — session sidecars; `internal/i18n` — en/zh/zh-TW strings.
- Thin frontends: `internal/cli` (TUI), `internal/serve` (HTTP/SSE), `desktop/` (Wails). Supporting: `internal/agent` (subagents), `internal/provider`, `internal/acp`, `internal/taskmonitor`, `internal/config`, `internal/memory`, `internal/skill`, `internal/command`.

## Comments

Default is none — the code is the truth. Write one only when the **why** is
non-obvious: a hidden constraint, a workaround anchored to something verifiable,
an invariant the type system cannot express, or an external-protocol quirk.

- Declaration doc: ≤15 lines. Package comment: ≤8 lines, or ≤40 in a `doc.go`.
- Every other comment: ≤3 lines. Struct-field and trailing `//`: 1 line.
- Never: restatements of the code, phase/stage narrative, incident or
  conversation history, section banners, commented-out code, `@param` lists.
- `TODO(#nnn):` and `HACK(#nnn):` need the issue anchor. `FIXME` is banned.
- One responsibility per file; 800 lines is the ceiling.

`go run ./tools/repolint` enforces all of it against a ratchet baseline: recorded
debt is tolerated, anything new fails CI. Never widen the baseline to land a
change — fix the code. `-update` exists for carrying debt through a rename or an
extraction, and that diff must be justified in the PR.

## Memory

- Standing instructions are hierarchical: committed/shared `REASONIX.md`,
  `AGENTS.md`, and `CLAUDE.md`; personal `*.local.md` variants; matching files in
  ancestor directories; and user-global files under the memory state root
  (`REASONIX_STATE_HOME`, otherwise `REASONIX_HOME`, otherwise `~/.reasonix` on
  macOS/Linux or `%APPDATA%\reasonix` on Windows). All distinct supported files
  in a directory load; `AGENTS.md` is not merely a fallback.
- `@path` on its own line imports another file's contents.
- `#<note>` in chat quick-adds an always-on instruction. The `remember` tool
  instead saves a fallible background fact (frontmatter file + `MEMORY.md`
  index). Fact `type` classifies content; independent `scope` controls whether it
  is project-only (the default) or explicitly global. The index loads into the
  stable prefix on the next session; global user/feedback bodies also load as
  lower-priority compatibility guidance. The current turn receives a tail note.

## Notes

<!-- Quick-add notes here. -->

## Pre-push CI simulation

Run these **before every commit** to catch the fastest CI failures locally:

```bash
gofmt -w .                          # catches gofmt (saves ~13s CI)
go vet ./...                        # catches vet warnings (saves ~52s CI/lint)
make lint                           # golangci-lint at CI's pin + repolint
go test ./internal/tool/builtin/ ./internal/boot/  # catches tool/boot test breaks
```

`make lint` runs both gates CI runs, at the version in `.golangci-version`;
`make lint-install` installs it. Do not skip it: a `modernize` finding never
shows up in `go vet`, and the CI round trip that catches it instead costs ten
minutes.

## Import cycle rule

Before importing a new internal package from a non-test file, verify the target package's **test files** aren't already importing back to you:

```
# BAD: agent(_test.go) → tool/builtin(sessions.go) → agent  → setup failed
```

Use `go test ./path/to/target/` to detect cycles **before** pushing. A `[setup failed]` message means a cycle exists.

## PR hygiene

- **One force-push per round of review feedback.** Multiple force-pushes destroy review history and confuse reviewers.
- **Keep the PR diff minimal.** Only the files relevant to the PR's purpose — no stray changes from other branches.
- **Amend, don't add commits, for review feedback** — keeps the commit history clean.

## PR metadata gates

Two CI guards read the PR body. The scripts are the source of truth and both
run locally: `scripts/check-cache-impact.sh`, `scripts/check-docs-impact.sh`.
Separators must be an ASCII `-` or `:` — an em dash fails the docs guard.

Cache-sensitive diffs (`internal/tool/`, `internal/provider/`,
`internal/boot/`, `internal/agent/agent.go`, and the rest of the list in the
script) require:

```
Cache-impact: <none|low|medium|high> - <reason>
Cache-guard: <focused guard test/command or existing guard rationale>
```

`none` is a legitimate impact when the provider-visible prefix stays
byte-identical; only an empty value, `todo`, or `tbd` is rejected. If the diff
also touches `internal/config/`, `internal/memory/`, `internal/outputstyle/`,
`internal/skill/`, or `internal/boot/`, add `System-prompt-review: <note>` —
that field additionally rejects `none` and `n/a`, so it must name a reviewer.

User-visible diffs (`cmd/reasonix/`, `desktop/`, `npm/`, and most of
`internal/`; tests and lockfiles are exempt) require one of these, chosen by
whether the same PR edited `docs/*.md`:

```
Documentation-impact: updated - <what changed>            # docs/*.md edited
Documentation-impact: none - <why the docs stay correct>  # not edited
```
