---
name: scheduler-usage
description: >-
  How to USE the Coding Hermes Scheduler for real: REST API dialect (the #1
  trap), MCP tools, dashboards, project lifecycle, board format, and the
  verify harness. Load this before operating the scheduler or writing any
  integration/script against http://127.0.0.1:9090. Written from a live
  dogfood run 2026-08-04 (docs/dogfood/2026-08-04-integration.md).
version: 1.0.0
category: software-development
---

# Scheduler Usage — Operating the Fleet Scheduler for Real

The scheduler is a single Go daemon (`bin/schedulerd`, SQLite/WAL) that
dispatches foreman ticks for 40+ projects. It exposes REST on
`127.0.0.1:9090/api/v1`, MCP on `/mcp`, and an htmx dashboard at `/`.

## Golden rules (learned the hard way)

1. **The wire format is PascalCase for projects/ticks/events** (`Name`,
   `RepoURL`, `CooldownS`, `DecayRate`, `NamespaceID`, `CreatedAt`) — despite
   specs/S06-rest-api.md documenting snake_case. Namespaces ARE snake_case
   (`id`, `weight`, `hard_cap`). Match the endpoint, not the spec.
2. **Never create projects with the documented snake_case body** — it 400s
   ("name, repo_url, workdir are required"). Use
   `{"Name":...,"RepoURL":...,"Workdir":...,"Weight":10,"Priority":5,"CooldownS":900,"Enabled":true}`.
   `Weight` (1-100) is REQUIRED or you get a 500 CHECK-constraint error.
3. **There is no delete endpoint.** Do not create throwaway projects against
   the live DB — they live forever (16+ test dummies already pollute it).
4. **PUT to change cooldown/decay/enabled**: `curl -X PUT
   http://127.0.0.1:9090/api/v1/projects/<NAME> -d '{"CooldownS":900,"DecayRate":1.0}'`
   (PascalCase keys). This is how the stand-in cron wakes paused foremen.
5. **The board is JSONL in `.coding-hermes/board/`** (tasks.jsonl,
   events.jsonl, board.jsonl) — not tasks.md. Append one JSON object per line
   in the existing schema (id, title, status, priority, complexity,
   capability_tags, created_at, ...). Validate with `jq -c .` per line.

## Endpoint cheat sheet (all verified 2026-08-04)

| Route | Use |
|---|---|
| `GET /api/v1/health` | liveness + DB + active tick count |
| `GET /api/v1/status` | fleet summary; fields: `active_projects`, `active_ticks`, `budget_total`, `recent_outcomes`, `projects_failure_rates` (per-project failed/total/failure_rate over last `failure_window` ticks, default 100), `failure_window`, `duckbrain` |
| `GET /api/v1/projects` | list (PascalCase); filter nothing server-side |
| `GET /api/v1/projects/{name}` | detail + `latest_tick`; 404 `{"error":"project not found"}` |
| `PUT /api/v1/projects/{name}` | partial update (PascalCase keys) |
| `GET /api/v1/ticks?project=X&limit=N` | newest-first; bad limit → 50 |
| `GET /api/v1/ticks/{id}` | tick detail; 404 `tick not found` |
| `GET /api/v1/events?severity=CRITICAL&limit=N` | event feed (escalation, loop, sync) |
| `GET /api/v1/namespaces` | namespace weights (snake_case here!) |
| `POST /api/v1/evaluate` | force evaluation (safe, signal-only) |
| `POST /api/v1/pause` / `resume` | fleet-wide, process-local, idempotent |
| `POST /mcp` | JSON-RPC 2.0: initialize → tools/list → tools/call |

## MCP tools

`fleet_status`, `fleet_projects`, `fleet_project_detail(name)`,
`fleet_set_weight(name, weight)`, `fleet_set_priority(name, priority)`,
`fleet_set_cooldown(...)`, `fleet_set_decay(...)`, pause/resume variants —
get the exact list via `tools/list`. Call format:
`{"jsonrpc":"2.0","id":N,"method":"tools/call","params":{"name":"<tool>","arguments":{...}}}`.
Read-only tools are safe to use anytime.

## Diagnostics and verification

- `deploy/scheduler-verify.sh` → runs `./bin/schedulerd --test-verify 3`
  (crontab, every 2h, logs in `deploy/verify-*.log`). **As of 2026-08-04 this
  is RED** (see board task DOGFOOD-002) — the fixture shares one workdir across
  7 projects, tripping the dup-workdir check. Don't trust "gates green" board
  claims until this passes.
- Live signals: `/api/v1/events?severity=CRITICAL` shows escalation failures;
  `recent_outcomes` in /status shows the completed/failed/timeout split
  (69% failure rate as of 2026-08-04 — investigate before assuming health).
- Occasional 13s read stalls happen under load (WAL checkpoint contention).
  Retry once before debugging anything else.

## Pitfalls

- `.coding-hermes/tasks.md` is dead (`.bak`); the picker/stand-in scripts that
  look for tasks.md won't find this project's board — use tasks.jsonl.
- `/healthz` is NOT an endpoint; `/api/v1/health` is.
- No auth on anything — loopback only, treat as operator-only.
- The daemon binary at `bin/schedulerd` is what's running; rebuild + restart
  via systemd/supervisor after code changes (verify log shows the DB path
  `/home/kara/.hermes/coding-hermes/scheduler.db`).
