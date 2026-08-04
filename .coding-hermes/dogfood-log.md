# Dogfood Log — coding-hermes-scheduler

## 2026-08-04 — 🟡 PROMISING-BUT-ROUGH

**Promise:** "A single Go binary that replaces dozens of static cron jobs" —
priority-weighted fleet scheduler that spawns foreman ticks, tracks outcomes,
exposes REST/MCP/dashboard/DuckBrain sync.

**Verdict evidence:** Core loop works live (44 projects, 1,427 HTTP spawns,
10,105 completed ticks, escalation events firing, dashboard + MCP functional,
warm p50 38ms). But: POST /projects fails on every documented request shape
(400 snake_case / 500 CHECK on minimal PascalCase; 409 unreachable); wire
format is PascalCase while the spec/README say snake_case (README jq example
returns null); the 2-hourly self-verify has been RED 50 consecutive runs
(2026-07-31T08:00 → now) while the board claims all gates green; no delete
endpoint; occasional 13s read stalls.

**Time-to-first-success:** ~2 min (first probe stalled >10s, then 40ms).

**Top 3 findings (task IDs):**
1. **DOGFOOD-001 (P0)** — create-project API broken per documented contract (400/500/409-unreachable).
2. **DOGFOOD-002 (P0)** — `--test-verify` red 4+ days, board green; fixture shares one workdir, dup-workdir guard rejects it; gate suite excludes the project's own verify.
3. **DOGFOOD-003 (P1)** — mixed JSON dialects vs S06 contract + wrong README jq example.
   (Also DOGFOOD-004 spec staleness, DOGFOOD-005 no delete, DOGFOOD-006 latency spikes/69% failure-rate question.)

**Artifacts left:** docs/dogfood/2026-08-04-integration.md,
skills/scheduler-usage/SKILL.md, docs/dogfood/diagnostics.md, board tasks
DOGFOOD-001..006 appended to .coding-hermes/board/tasks.jsonl.

**Foreman:** already at CooldownS=900 / Enabled=true, ticking normally —
no wake needed.
