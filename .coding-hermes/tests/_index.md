# E2E Testing Index — coding-hermes-scheduler

Created: tick #188 (2026-08-01). Loaded per E2E-001 (coding-hermes-testing skill).

## What This Project Is

The Coding Hermes fleet scheduler: Go daemon (`bin/schedulerd`) with SQLite, htmx dashboard, REST API, MCP endpoint, and DuckBrain sync. Live system verified at http://127.0.0.1:9090.

## E2E Surface (live-verified, tick #188)

| Dimension | Coverage | Status |
|-----------|----------|--------|
| Daemon health | `GET /api/v1/health` → `{"status":"ok","db":"connected"}` | ✅ |
| Fleet status | `GET /api/v1/status` → 44 active projects, 4 active ticks, DuckBrain reachable | ✅ |
| Project list | `GET /api/v1/projects` → 66 projects, 44 enabled | ✅ |
| Tick history | `GET /api/v1/ticks?limit=2` → 200 | ✅ |
| Namespaces | `GET /api/v1/namespaces` → 200 | ✅ |
| Dashboard | `GET /` and `/health` → 200 | ✅ |
| MCP endpoint | `POST /mcp` (tools/list) → 200 | ✅ |
| HTTP spawn path | health shows `spawns_http=28, spawns_exec=0` — HTTP spawn live, exec fallback never used | ✅ |
| DuckBrain sync | `spooled_pending=0`, `consecutive_failures=0`, last_ok_at within 5m window | ✅ |
| Spool+replay | 3 spooled writes at 15:11:55 (transient :3000 timeout) replayed 15:15:48 — no loss | ✅ |
| Tick storm guard | 30/44 projects cooldown<7200s but 0 projects with >1 running tick (runningSet exclusion in packer_select.go) | ✅ |

## Go Test Battery (unit/integration)

`go test -short -p 1 ./...` → 9/9 packages, 453 test runs PASS. `golangci-lint run` → 0 issues. `govulncheck` → no vulnerabilities.

## Known Gaps

- No browser-level dashboard render verification (htmx dashboard) — covered by `internal/dashboard` tests + endpoint 200s; visual render check deferred (no browser surface in cron).
- 30 projects below the 7200s tick_timeout — acceptable: runningSet exclusion prevents overlap (INFRA-003 WATCH).

## Test State

See `test-state.toml` for counters.
