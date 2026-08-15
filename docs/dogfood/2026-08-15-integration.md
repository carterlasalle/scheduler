# Dogfood Integration Report — coding-hermes-scheduler (2026-08-15)

**Verdict: 🟡 PROMISING-BUT-ROUGH (up from the 08-04 run: all P0/P1 API bugs fixed; remaining friction is at the evaluation/simulation + docs surfaces)**
**Probed by:** Hermes dogfood cron (independent user, not the foreman)
**Targets:** live daemon `http://127.0.0.1:9090` (pid 2524416, up 11h+, `--namespace-mode --auto-disable-failure-rate 0.9`, HTTP-gateway spawns) + a scratch sim instance on `:19090` (own DB in /tmp) + CLI surfaces (`--schema`, `--show-config`, `--sim-count`, `--sim-setup`, `--test-verify 3`)

## The promise vs. reality

**Promise (README/AGENTS.md):** "A single Go binary that replaces dozens of
static cron jobs" — event-driven evaluation, greedy weight packing, foreman
spawns via the Hermes gateway HTTP API, tick tracking, REST API + MCP +
dashboard + DuckBrain sync.

**Reality:** The production promise holds — 44 enabled projects scheduling
via HTTP gateway (84 HTTP spawns this session), all 6 of the 08-04 P0/P1
findings verified FIXED live (create-per-spec, snake_case wire format,
DELETE endpoint + 409 guard, `--test-verify` green 6/6, 2-hourly verify logs
green, S06 spec Approved). The broken parts are the *evaluation* surfaces a
new user hits before trusting the daemon: simulation mode, the /fleet
plugin, and several doc mirrors that describe things that don't exist.

## Time-to-first-success: ~1s
First probe `GET /api/v1/health` → instant `{"status":"ok"}`. No latency
stalls this run (the 13s WAL stalls of 08-04 are gone; verify harness p99
16ms on /status at 29,100 synthetic rows, spec S10 <100ms).

## What WORKS (verified live, 2026-08-15)

| Endpoint / surface | Result |
|---|---|
| `GET /api/v1/health` | 200 `{status:ok, db:connected, active_ticks:N, evaluation_age_seconds}` |
| `GET /api/v1/status` | 200 — `active_projects`, `auto_disable{enabled,threshold,window,min_ticks}`, `projects_failure_rates` (per-project), `recent_outcomes`, `zero_select_*` diagnostics |
| `GET /api/v1/projects` | 200 `{"projects":[...]}` — **snake_case now** (name, repo_url, workdir, cooldown_s, decay_rate, disabled_at/by/reason, consecutive_failures…) |
| `GET /api/v1/projects/{name}` | 200 `{"project":{...},"latest_tick":...}`; 404 `{"error":"project not found"}` |
| `POST /api/v1/projects` | **201 with minimal snake_case body** — defaults weight 10 / priority 5 / cooldown_s 900 / decay_rate 1.0, created disabled. Dup name → **409** |
| `PUT /api/v1/projects/{name}` | 200 partial update, snake_case keys |
| `DELETE /api/v1/projects/{name}?confirm=true` | 409 while enabled (clear message: "pause it first…"); 200 when disabled — **but soft-delete, row persists (see traps)** |
| `GET /api/v1/ticks?project=&limit=` | 200 `{count,ticks}` newest-first; rich rows (tokens, cost_usd, commits, exit_code, error) |
| `POST /api/v1/evaluate` | 200 `{"status":"evaluation triggered"}`; `evaluation_age_seconds` resets |
| MCP `POST /mcp` | tools/list → **14 tools, all `fleet_*`**; `fleet_status` works |
| Dashboard `/`, `/queue`, `/ticks`, `/health`, `/dashboard/partial`, `/namespaces/{id}`, `/projects/{name}` | all 200 HTML, 2–200ms |
| `--test-verify 3` | **6 checks, 0 failures, ✅ SCHEDULER VERIFIED** (+ perf audit + failure-rate breakdown) |
| `--sim-setup --sim-ticks 5` | works: 13-project fixture, 25 simulated ticks, report printed |
| Env config | `SCHEDULER_AUTO_DISABLE_FAILURE_RATE=0.5` honored at boot ("AUTO-DISABLE: enabled — rate=0.50") |

## What's BROKEN / traps for integrators (2026-08-15)

1. **`--simulate` does not simulate (P1, DOGFOOD-007).** The daemon-mode flag
   only sets a log string. The eval loop always uses the real spawner; in a
   scratch env without gateway credentials every tick fails
   `no gateway client and exec fallback disabled for <proj>`; with
   credentials it would spawn REAL foremen. README's "dry-run/simulation
   mode (no real spawning)" is false.
2. **`--sim-count N` crashes (P1, DOGFOOD-007).** `FATAL: simulation: spawn:
   sim spawn sim-<proj>-<HHMMSS>: constraint failed: UNIQUE constraint
   failed: ticks.id` — the 500ms ticker regenerates the same 1s-granularity
   tick ID twice within one second.
3. **`/fleet` plugin dead (P1, DOGFOOD-008).** `~/.hermes/plugins/coding-hermes`
   → `/home/kara/coding-herms-scheduler/plugin` (typo'd, missing `-hermes-`),
   target does not exist. All `/fleet …` slash commands silently gone.
4. **DELETE is an undocumented soft delete (P2, DOGFOOD-009).** 200 OK but the
   row stays in list + detail (stamped `disabled_by="api-delete"`). Test rows
   can never be purged via API; hard-deleted projects' ticks still show in
   `projects_failure_rates` (eduos-e2e: rate 1.0, armed=true, row gone).
5. **README §MCP second tool table is fiction (P2, DOGFOOD-011).**
   `list_projects` etc. → `{"error":{"message":"unknown tool: list_projects"}}`.
   Real names: the 14 `fleet_*` tools.
6. **`--schema`/`--show-config` over-claim (P2, DOGFOOD-012).** Schema
   describes a root `schedulerd.toml` three-layer config the daemon never
   loads; `--show-config` prints "CLI flags only" and hides applied env
   overrides (just comments them).
7. **Stale mirrors (P3, DOGFOOD-013).** repo `fleet.toml` (20 projects) vs
   live (94 entries); `docs/fleet.md` generated 08-07.

## Working integration patterns (the "right way", verified today)

- **Read:** `GET /api/v1/projects` → `{"projects":[…]}` snake_case; detail →
  `{"project":…,"latest_tick":…}`. `jq '.projects[] | {name, cooldown_s, enabled}'`.
- **Create (spec body works now):**
  `curl -X POST http://127.0.0.1:9090/api/v1/projects -d '{"name":"X","repo_url":"…","workdir":"/abs/path"}'`
  → 201, disabled, defaults filled. Then `PUT … -d '{"enabled":true}'`.
- **Lifecycle test safely:** create → PUT weight/priority → dashboard page →
  PUT `{"enabled":false}` → DELETE `?confirm=true`. Remember: the row stays
  in the list (soft-delete) — budget your test rows accordingly.
- **Wake a paused foreman** (stand-in cron pattern):
  `curl -X PUT http://127.0.0.1:9090/api/v1/projects/<NAME> -d '{"CooldownS":900,"DecayRate":1.0}'`
  (PascalCase still accepted on decode; snake_case also fine).
- **Simulate for real:** use `--sim-setup --sim-ticks N` on a scratch DB, or
  `--test-verify 3` (self-contained E2E + perf audit). Avoid `--simulate` and
  `--sim-count` until DOGFOOD-007 lands.
- **MCP:** initialize → tools/list → tools/call with `fleet_*` names only.

## Errors hit this run (and the fix)

- `no gateway client and exec fallback disabled for X` — scratch daemon
  without API_SERVER_KEY; expected in sim mode, which is exactly why
  `--simulate` pretending to be dry-run is a trap (DOGFOOD-007).
- `UNIQUE constraint failed: ticks.id` — `--sim-count` same-second ID
  collision (DOGFOOD-007).
- `unknown tool: list_projects` — README fiction (DOGFOOD-011).
- `{"error":"project is enabled — pause it first …"}` — DELETE guard, working
  as designed; it is the only clear signal that DELETE is soft.
- DuckDB board mirror `INSERT OR IGNORE` failed ("no UNIQUE constraints") —
  board.db tasks table has no PK; mirror with existence-check + plain INSERT
  (used here; foreman's append pattern is "lockstep pre-append").
