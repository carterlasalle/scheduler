package scheduler

import (
	"database/sql"
	"testing"
	"time"
)

// Regression tests for S-GAP-003 (2026-08-05): gateway-spawned ticks have
// pid=0, so both zombie reapers (reapZombies, cleanDanglingOnStartup) skipped
// them — orphaned gateway rows stayed 'running' for up to 90 minutes
// (CleanupStale), excluding their projects from packing and starving the GAP
// boards 31h+. The reapers must now reap pid=0 rows whose heartbeat is stale
// (older than gatewayZombieMaxAge = 15 min), or whose heartbeat was never
// written and spawned_at is older than 15 min. Rows younger than 15 min must
// be left alone (INFRA-012 no-regression: a restart must never kill live
// gateway ticks and spawn duplicates).

// insertGatewayTickState inserts a running pid=0 tick with explicit
// spawned_at and heartbeat_at (nil heartbeat = NULL, the pre-fix shape).
func insertGatewayTickState(t *testing.T, db *sql.DB, tickID, project string, spawnedAt time.Time, heartbeatAt *time.Time) {
	t.Helper()
	var hb interface{}
	if heartbeatAt != nil {
		hb = heartbeatAt.UTC().Format(time.RFC3339)
	}
	_, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, spawned_at, created_at, pid, session_id, heartbeat_at)
		 VALUES (?, ?, 'running', ?, ?, 0, ?, ?)`,
		tickID, project,
		spawnedAt.UTC().Format(time.RFC3339), spawnedAt.UTC().Format(time.RFC3339),
		tickID, hb)
	if err != nil {
		t.Fatalf("insert gateway tick %s: %v", tickID, err)
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// gatewayStaleMatrix seeds the four-row heartbeat/spawn age matrix and
// returns the tick ids in matrix order.
func gatewayStaleMatrix(t *testing.T, db *sql.DB, project string) (staleHb, freshHb, nullHbOldSpawn, nullHbFreshSpawn string) {
	t.Helper()
	now := time.Now()
	staleHb = project + "-stale-hb"
	freshHb = project + "-fresh-hb"
	nullHbOldSpawn = project + "-null-hb-old-spawn"
	nullHbFreshSpawn = project + "-null-hb-fresh-spawn"
	insertGatewayTickState(t, db, staleHb, project, now.Add(-30*time.Minute), timePtr(now.Add(-20*time.Minute)))
	insertGatewayTickState(t, db, freshHb, project, now.Add(-30*time.Minute), timePtr(now.Add(-1*time.Minute)))
	insertGatewayTickState(t, db, nullHbOldSpawn, project, now.Add(-20*time.Minute), nil)
	insertGatewayTickState(t, db, nullHbFreshSpawn, project, now.Add(-1*time.Minute), nil)
	return staleHb, freshHb, nullHbOldSpawn, nullHbFreshSpawn
}

// assertGatewayMatrix asserts the reaper outcome shared by both reapers:
// stale rows reaped as timeout with outcome left NULL and completed_at
// stamped (GAP-045), fresh rows untouched.
func assertGatewayMatrix(t *testing.T, db *sql.DB, staleHb, freshHb, nullHbOldSpawn, nullHbFreshSpawn string) {
	t.Helper()
	if got := tickStatusOf(t, db, staleHb); got != "timeout" {
		t.Errorf("stale-heartbeat gateway tick = %q, want timeout (orphaned heartbeat > 15 min)", got)
	}
	if outcome := tickOutcomeOf(t, db, staleHb); outcome.Valid {
		t.Errorf("reaped tick %s outcome = %q, want NULL (CHECK constraint rejects 'zombie_reaped')", staleHb, outcome.String)
	}
	if got := tickCompletedAtOf(t, db, staleHb); !got.Valid || got.String == "" {
		t.Errorf("reaped tick %s completed_at = %v, want stamped (GAP-045: timeout rows must be terminal)", staleHb, got)
	}
	if got := tickStatusOf(t, db, freshHb); got != "running" {
		t.Errorf("fresh-heartbeat gateway tick = %q, want running — younger than 15 min must never be reaped (INFRA-012)", got)
	}
	if got := tickStatusOf(t, db, nullHbOldSpawn); got != "timeout" {
		t.Errorf("NULL-heartbeat old-spawn gateway tick = %q, want timeout (pre-fix row shape, spawned > 15 min ago)", got)
	}
	if outcome := tickOutcomeOf(t, db, nullHbOldSpawn); outcome.Valid {
		t.Errorf("reaped tick %s outcome = %q, want NULL", nullHbOldSpawn, outcome.String)
	}
	if got := tickCompletedAtOf(t, db, nullHbOldSpawn); !got.Valid || got.String == "" {
		t.Errorf("reaped tick %s completed_at = %v, want stamped (GAP-045)", nullHbOldSpawn, got)
	}
	if got := tickStatusOf(t, db, nullHbFreshSpawn); got != "running" {
		t.Errorf("NULL-heartbeat fresh-spawn gateway tick = %q, want running — INFRA-012 no-regression", got)
	}
}

// TestReapZombies_GatewayStaleHeartbeat drives the matrix through the 60s
// zombie reaper. Red before the fix: reapZombies only looked at pid > 0.
func TestReapZombies_GatewayStaleHeartbeat(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "sgap003-reap")
	staleHb, freshHb, nullHbOldSpawn, nullHbFreshSpawn := gatewayStaleMatrix(t, db, "sgap003-reap")

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.reapZombies()

	assertGatewayMatrix(t, db, staleHb, freshHb, nullHbOldSpawn, nullHbFreshSpawn)
}

// TestCleanDanglingOnStartup_GatewayStale drives the same matrix through
// startup cleanup. Red before the fix: cleanDanglingOnStartup filtered
// pid > 0 only. The fresh rows pin the INFRA-012 regression guard.
func TestCleanDanglingOnStartup_GatewayStale(t *testing.T) {
	db := newTestDB(t)
	mustCreateProjectINFRA012(t, db, "sgap003-dangle")
	staleHb, freshHb, nullHbOldSpawn, nullHbFreshSpawn := gatewayStaleMatrix(t, db, "sgap003-dangle")

	loop := NewLoop(db, time.Minute, time.Hour, 10, 100, 5)
	loop.cleanDanglingOnStartup()

	assertGatewayMatrix(t, db, staleHb, freshHb, nullHbOldSpawn, nullHbFreshSpawn)

	// The reaped rows belong to the project, so its last_tick_completed must
	// be bumped exactly as for dead-pid reaps.
	var lastCompleted sql.NullString
	if err := db.QueryRow(`SELECT last_tick_completed FROM projects WHERE name = ?`, "sgap003-dangle").Scan(&lastCompleted); err != nil {
		t.Fatalf("query last_tick_completed: %v", err)
	}
	if !lastCompleted.Valid || lastCompleted.String == "" {
		t.Error("last_tick_completed not bumped after reaping stale gateway ticks — packer urgency distorted")
	}
}
