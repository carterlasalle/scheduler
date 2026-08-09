# Diagnostic Trail — coding-hermes-scheduler (dogfood 2026-08-04)

This is the "how it's built, what broke, the right way" record. It explains
the system and the errors encountered — from the project's own history AND
this run — so future agents can answer "does this thing actually work?" from
the repo, not from test colors.

## How it's built

- **Go 1.26+ daemon** (`cmd/schedulerd/`), `net/http` ServeMux, Go 1.22+
  method patterns. Single binary `bin/schedulerd`.
- **SQLite via modernc.org/sqlite** (pure Go, WAL mode). Live DB:
  `~/.hermes/coding-hermes/scheduler.db`. Schema: projects,
  namespaces, ticks, events, migrations. Board state (`.coding-hermes/board/`)
  is separate: DuckDB live store + JSONL git mirror (see INFRA-013).
- **Scheduler loop** (`internal/scheduler/`): every 60s evaluate → urgency =
  f(cooldown age, priority, decay_rate) → greedy weight packing into a 100-unit
  budget with multi-pool namespace mode → spawn via Hermes gateway HTTP API
  (`POST /v1/responses`, `require_approval:false`) with `--no-exec-fallback`
  exec spawn as fallback. Cooldown per project; timeout does NOT back off
  (deliberate); configurable auto-disable (SCHED-GAP-018, default off — `--auto-disable-failure-rate` > 0 to enable; the 10+ consecutive timeouts/24h safety net remains).
- **REST API** (`internal/api/`): 20 method/path ops under `/api/v1` — health,
  status, projects CRUD-ish (no delete), ticks, events, namespaces, evaluate,
  pause/resume. **MCP** (`internal/mcp/`) JSON-RPC on `/mcp` with fleet_* tools.
  **Dashboard** (`internal/dashboard/`): htmx + server-rendered HTML.
  **DuckBrain sync** (`internal/sync/`): pushes fleet state to :3000 every 5m.
- **Deploy verify**: host crontab runs `deploy/scheduler-verify.sh` every 2h →
  `./bin/schedulerd --test-verify 3` (self-contained E2E: temp DB, 7-project
  fixture fleet, N cycles, invariant checks). Logs to `deploy/verify-*.log`.

## Errors hit during this run (and their root causes)

### 1. POST /api/v1/projects — 400 on documented body, 500 on minimal body
- Symptom A: `{"name":"ch-alpha","repo_url":"x","workdir":"/tmp"}` →
  `400 {"error":"name, repo_url, workdir are required"}`.
  Cause: `internal/database/models.go` and `ProjectUpdates` have **no json
  tags**, so Go's decoder binds exported names (`Name`, `RepoURL`...) and the
  snake_case request keys bind to nothing → required-field check fires.
  Documented in S06 §1.1 as a known gap; never fixed. **The spec's own
  example body cannot create a project.**
- Symptom B: PascalCase minimal body → `500 create project "x": CHECK
  constraint failed: weight >= 1 AND weight <= 100`. Cause: handler fills no
  defaults for zero-valued fields (S06 §5.1 admits this), and the schema's
  CHECK rejects weight=0. The documented 409 duplicate path is unreachable —
  constraint errors fire before the duplicate check.
- Right way today: send `{"Name","RepoURL","Workdir","Weight":1-100,...}` all
  PascalCase. Fix direction: DOGFOOD-001 (json tags + defaults + 409).

### 2. deploy verify RED for 4+ days, board claims green
- Symptom: `deploy/verify-*.log` → `VERIFY FAILED: create beta: create project
  "beta": workdir "/tmp/scheduler-verify-<rand>" already registered by enabled
  project "alpha" (case-insensitive duplicate)`. 191/207 logs FAIL (0 PASS),
  continuous streak since 2026-07-31T08:00.
- Cause: `cmd/schedulerd/test_verify.go` registers 7 fixture projects **all
  with `Workdir: tmpDir`**. The duplicate-workdir guard (added `bc438e6`,
  `9f9d6a5` ~tick #184, to prevent ghost projects after the `heading` incident
  INFRA-009) rejects project #2 ("beta"). The guard is correct; the fixture is
  stale. The foreman's tick #185 audit said "dup-workdir check verified" —
  verified at the unit level, never ran `--test-verify` (L3). The foreman gate
  suite (`go test -p 1`, lint, govulncheck, gitreins guard) does not include
  the crontab verify, so the red gate was invisible to the board loop.
- Right way: each fixture project gets its own subdir under tmpDir; add
  `--test-verify` to the gate suite; NEVER-DONE audit greps
  `deploy/verify-*.log` for FAIL. Fix direction: DOGFOOD-002.

### 3. Mixed JSON dialects
- projects/ticks/events serialize PascalCase; namespaces serialize snake_case.
  Root cause: models.go lacks json tags (gap #1) while the namespace model has
  them. README `jq '.project_count'` is wrong (field is `active_projects`).
  Impact: any consumer following S06 gets nulls; the ecosystem scripts
  (stand-in, pickers) work only because they reverse-engineered PascalCase.
  Fix direction: DOGFOOD-003.

### 4. 13s read stalls
- `GET /api/v1/projects` twice took 13.07s/10s+ while `/status` p50 was 38ms.
  Working hypothesis: SQLite WAL checkpoint contention while 6 ticks write
  continuously. Not root-caused in this run (time-boxed) — measurement task
  DOGFOOD-006 with `busy_timeout`/checkpoint tuning candidates.

### 5. Open question: 69% tick failure rate
- `recent_outcomes`: 10,105 completed / 22,274 failed / 331 timeout.
  Escalation events keep firing ("project starved: my-project — last tick
  3h22m ago, cooldown 900s") — some enabled projects (e.g. `my-project` with a
  placeholder repo) appear to be spawned and fail repeatedly. Whether this is
  test-junk, config drift, or a real packer/failure-loop bug needs an audit
  (folded into DOGFOOD-006's observability ask; NEVER-DONE performance audit
  should own it).

## Historical context from the board (things already learned the hard way)

- **INFRA-012 (tick #190-191):** daemon restart marked live gateway ticks
  timeout → duplicate spawns. Fixed: only reap dead pid>0 ticks on startup.
- **REC-ZOMBIE-OUTCOME (tick #192):** `outcome=zombie_reaped` violated the
  ticks CHECK → every reap silently no-oped (SQLite rejects whole UPDATE).
  Fixed by dropping outcome from the UPDATE; also fixed a rows-open-during-
  UPDATE deadlock. Lesson: SQLite CHECK constraints silently kill multi-row
  UPDATEs — always test the reap path end-to-end.
- **INFRA-009/010 (tick #183):** ghost `heading` row deleted; dup-workdir and
  decay_rate=0 guards added at API level — the very guards that later broke
  --test-verify (this run) and that `fleet_set_decay` now enforces.
- **INFRA-003:** tick storms (cooldown < tick_timeout) — preemptively solved
  by config (min cooldown 900s > timeout 600s... note daemon now runs
  `--tick-timeout 7200s`, so re-check this invariant).

## The right way (summary)

1. Read the wire, not the spec, for field names (PascalCase; namespaces
   snake_case) — or fix the tags (DOGFOOD-001/003).
2. Never create throwaway projects (no delete endpoint) — DOGFOOD-005.
3. Treat `--test-verify` as the L3 gate; board "gates green" ≠ verified.
4. Writes to the board: append JSONL records to `.coding-hermes/board/tasks.jsonl`.
5. All control endpoints are idempotent and loopback-only — safe to probe;
   only POST /projects with a real intent (it persists forever).
