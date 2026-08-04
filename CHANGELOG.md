# Changelog

All notable changes to the Coding Hermes Scheduler.

## [Unreleased] — 2026-08-04

### API Conformance (DOGFOOD-001/003/004)

- **snake_case wire format** — `Project`, `Tick`, `Event`, `ProjectUpdates` now carry S02/S06 `json` tags; responses emit `name`, `repo_url`, `cooldown_s`, `active_projects`, etc. per the OpenAPI contract
- **Backward-compatible request decoding** — custom `UnmarshalJSON` on `Project` and `ProjectUpdates` accepts snake_case AND legacy PascalCase keys (`Name`, `CooldownS`, `Enabled`, …); live fleet automation (fleet-auto-heal, stand-in gap-pusher) is unaffected
- **Create defaults** — `POST /api/v1/projects` fills `weight=10`, `priority=5`, `cooldown_s=900`, `decay_rate=1.0` when omitted; the documented minimal body `{name, repo_url, workdir}` now works; new projects stay disabled
- **Error mapping** — SQLite CHECK-constraint violations return `400` with an actionable range message (previously `500`); duplicate name/workdir still return `409`
- **Conformance tests** — `internal/api/conformance_test.go`: 11 tests covering both request spellings, 400/409 mapping, and exact response field names for `/projects`, `/status`, `/ticks`, `/events`
- **Spec S06 approved** — §1.1 conformance section rewritten to match verified behavior; status flipped Draft → Approved
- **README** — fixed `jq '.project_count'` → `jq '.active_projects'`; added "API wire format" note

### Verification Harness (DOGFOOD-002)

- **Fixed `--test-verify` red streak (50+ runs since 2026-07-31)** — fixture projects now get unique workdirs (`tmpDir/<name>`, created via `os.MkdirAll`) so the case-insensitive dup-workdir guard no longer rejects "beta"
- **Priority-ordering check fixed** — the per-eval spawn-order assertion was unsatisfiable: slot-pool goroutines start concurrently, so sub-second `spawned_at` order is scheduler-shuffled, not pack order. The check now asserts first-cycle set membership against the expected greedy-knapsack pack (urgency/priority ordering is still enforced)
- **CI gate** — `.github/workflows/ci.yml` build job now runs `./bin/schedulerd --test-verify 3` after "Build binaries" so the end-to-end verify can't silently rot again

## [1.0.0] — 2026-07-18

### Core Scheduler

- **Dynamic priority-weighted fleet scheduler** — single Go binary replaces 33+ static cron jobs
- **Urgency-based packing** — greedy knapsack fill with configurable weight budget and max concurrency
- **Geometric priority curve** — priority 1-10 maps to intervals from 20 minutes to 24 hours
- **Dynamic cooldown** — derived from priority when explicit cooldown is 0, preventing starvation
- **Auto-slowdown for idle projects** — doubles cooldown after consecutive idle ticks (capped at 4h), resets on first non-idle tick
- **Process-liveness zombie detection** — `/proc/pid/stat` instead of blind timeouts (30min cap rejected)

### HTTP API Spawn (FEAT-003)

- **Zero subprocess overhead** — `POST /v1/responses` to Hermes gateway instead of `exec.Command`
- **No MCP duplication** — duckbrain + gitreins loaded once by gateway, shared across ticks
- **~500MB → 0MB per tick** process overhead eliminated
- **Graceful fallback** — exec.Command when gateway is unreachable
- **Per-foreman MCP optimization** — HERMES_HOME with minimal config (duckbrain+gitreins only, no browser/chimera/flights)

### Dedicated Gateway (FEAT-004)

- **Cgroup isolation** — separate Hermes instance on :8643 with MemoryMax=16G
- **Independent restart cycle** — scheduler OOM doesn't kill main chat
- **Scheduler profile** — minimal MCPs, auto-approve mode, PAYG foreman provider

### API & Control Plane

- **REST API** — 15 endpoints (health, projects CRUD, ticks, events, evaluate)
- **MCP server** — 14 fleet_* tools at `/mcp` endpoint (status, projects, weight, priority, pause/resume, ticks, evaluate)
- **Dark theme dashboard** — HTML at `/` showing fleet status, project cards, tick history
- **Hermes plugin** — `/fleet` slash commands (status, weight, priority, pause, resume, ticks, evaluate)

### Configuration & Infrastructure

- **TOML fleet config** — `--config fleet.toml` for declarative project/namespace seeding
- **Cron migration tool** — `cmd/migrate/` imports Hermes cron jobs.json into SQLite
- **Multi-namespace DuckBrain** — separate namespaces per project with read-replica sync
- **Systemd deployment** — user units with MemoryMax, Restart=always, journal logging
- **Built-in verification** — `--test-verify N` with temp DB, 7-project fleet, 6 invariant checks

### Quality & Reliability

- **Goroutine leak fix** — context-cancellable stdout scanner, explicit pipe closure, tick timeout
- **Memory optimization** — per-chat MCP reduction (500MB → 175MB), MemoryMax=32G for 8 concurrent
- **pprof debugging** — net/http/pprof endpoint for production diagnostics
- **Alert escalation** — configurable thresholds with event emission
- **SQLite schema migrations** — versioned with automatic upgrade path
- **Built-in simulation** — `SimSpawner` for testing without real subprocesses

### Developer Experience

- **Makefile** — build, test, test-full, lint, fmt, migrate, deploy
- **Go 1.26** — latest stable toolchain
- **Vulnerability scanning** — govulncheck integration
- **Conventional commits** — feat/fix/docs/chore with co-author template
