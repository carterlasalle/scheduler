package scheduler

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Create minimal schema for tests.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS projects (
			name TEXT PRIMARY KEY,
			enabled INTEGER DEFAULT 1,
			cooldown_s INTEGER DEFAULT 1800,
			workdir TEXT DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS ticks (
			id TEXT PRIMARY KEY,
			project_name TEXT,
			status TEXT DEFAULT 'queued',
			completed_at TEXT,
			started_at TEXT
		);
		CREATE TABLE IF NOT EXISTS events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			severity TEXT,
			component TEXT,
			message TEXT,
			details TEXT,
			created_at TEXT
		);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return db
}

func insertProject(t *testing.T, db *sql.DB, name string, cooldown int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO projects (name, enabled, cooldown_s) VALUES (?, 1, ?)`,
		name, cooldown)
	if err != nil {
		t.Fatalf("insert project %s: %v", name, err)
	}
}

func insertTick(t *testing.T, db *sql.DB, tickID, project, status string, completedAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO ticks (id, project_name, status, completed_at) VALUES (?, ?, ?, ?)`,
		tickID, project, status, completedAt.Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert tick %s: %v", tickID, err)
	}
}

func countEventsBySeverity(t *testing.T, db *sql.DB, severity string) int {
	t.Helper()
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE severity = ?`, severity).Scan(&count)
	if err != nil {
		t.Fatalf("count events: %v", err)
	}
	return count
}

// TestAlertEscalator_CheckSchedulerHealth_NotEvaluating emits CRITICAL when
// lastEval is zero (never ran).
func TestAlertEscalator_CheckSchedulerHealth_NotEvaluating(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	err := escalator.CheckSchedulerHealth(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("CheckSchedulerHealth: %v", err)
	}

	if n := countEventsBySeverity(t, db, "CRITICAL"); n != 1 {
		t.Errorf("expected 1 CRITICAL event, got %d", n)
	}
}

// TestAlertEscalator_CheckSchedulerHealth_Stale emits CRITICAL when lastEval
// is more than 10 minutes ago.
func TestAlertEscalator_CheckSchedulerHealth_Stale(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	stale := time.Now().Add(-15 * time.Minute)
	err := escalator.CheckSchedulerHealth(context.Background(), stale)
	if err != nil {
		t.Fatalf("CheckSchedulerHealth: %v", err)
	}

	if n := countEventsBySeverity(t, db, "CRITICAL"); n != 1 {
		t.Errorf("expected 1 CRITICAL event for stale eval, got %d", n)
	}
}

// TestAlertEscalator_CheckSchedulerHealth_Recent does NOT emit when lastEval
// is recent.
func TestAlertEscalator_CheckSchedulerHealth_Recent(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	recent := time.Now().Add(-1 * time.Minute)
	err := escalator.CheckSchedulerHealth(context.Background(), recent)
	if err != nil {
		t.Fatalf("CheckSchedulerHealth: %v", err)
	}

	if n := countEventsBySeverity(t, db, "CRITICAL"); n != 0 {
		t.Errorf("expected 0 CRITICAL events for recent eval, got %d", n)
	}
}

// TestAlertEscalator_CheckStarvation emits MEDIUM for a project with no tick
// in more than 2x its cooldown.
func TestAlertEscalator_CheckStarvation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "test-proj", 1800) // 1h max interval
	// Last tick was 3 hours ago (> 2x 3600 = 2h).
	oldTick := time.Now().Add(-3 * time.Hour)
	insertTick(t, db, "tick-001", "test-proj", "completed", oldTick)

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	err := escalator.CheckStarvation(context.Background())
	if err != nil {
		t.Fatalf("CheckStarvation: %v", err)
	}

	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 1 {
		t.Errorf("expected 1 MEDIUM event for starved project, got %d", n)
	}
}

// TestAlertEscalator_CheckStarvation_RecentTick does NOT emit when the project
// had a recent tick.
func TestAlertEscalator_CheckStarvation_RecentTick(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "active-proj", 1800)
	recent := time.Now().Add(-10 * time.Minute) // well within 2h threshold
	insertTick(t, db, "tick-002", "active-proj", "completed", recent)

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	err := escalator.CheckStarvation(context.Background())
	if err != nil {
		t.Fatalf("CheckStarvation: %v", err)
	}

	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 0 {
		t.Errorf("expected 0 MEDIUM events, got %d", n)
	}
}

// TestAlertEscalator_CheckStarvation_Throttle proves SCHED-GAP-014: consecutive
// CheckStarvation calls emit at most one MEDIUM event per starvationThrottleWindow
// per project, even though the escalator is constructed fresh each time (as it
// is in production at tick_process.go:134).
func TestAlertEscalator_CheckStarvation_Throttle(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "starved-proj", 1800) // 2x cooldown = 1h
	oldTick := time.Now().Add(-3 * time.Hour)  // well beyond 1h threshold
	insertTick(t, db, "tick-old", "starved-proj", "completed", oldTick)

	// First call — should emit (first crossing of the threshold).
	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)
	if err := escalator.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #1: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 1 {
		t.Fatalf("after first call: expected 1 MEDIUM event, got %d", n)
	}

	// Second call — fresh escalator (mirrors production), same project still
	// starved. Throttle must suppress: still 1 event.
	escalator2 := NewAlertEscalator(db, events)
	if err := escalator2.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #2: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 1 {
		t.Errorf("after second call (throttled): expected 1 MEDIUM event, got %d", n)
	}

	// Third call — still throttled.
	escalator3 := NewAlertEscalator(db, events)
	if err := escalator3.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #3: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 1 {
		t.Errorf("after third call (throttled): expected 1 MEDIUM event, got %d", n)
	}
}

// TestAlertEscalator_CheckStarvation_ThrottleExpires proves that after the
// throttle window passes, a new starvation event IS emitted (not suppressed
// forever). We simulate this by manually backdating the first event's
// created_at to before the throttle window.
func TestAlertEscalator_CheckStarvation_ThrottleExpires(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "expiring-proj", 1800) // 2x cooldown = 1h
	oldTick := time.Now().Add(-3 * time.Hour)
	insertTick(t, db, "tick-old", "expiring-proj", "completed", oldTick)

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)
	if err := escalator.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #1: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 1 {
		t.Fatalf("after first call: expected 1 MEDIUM event, got %d", n)
	}

	// Backdate the emitted event to 31 minutes ago — just past the 30-min window.
	_, err := db.Exec(`UPDATE events SET created_at = ? WHERE severity = 'MEDIUM' AND component = 'escalation'`,
		time.Now().Add(-31*time.Minute).UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("backdate event: %v", err)
	}

	// Second call — throttle has expired, should emit again.
	escalator2 := NewAlertEscalator(db, events)
	if err := escalator2.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #2: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 2 {
		t.Errorf("after throttle expires: expected 2 MEDIUM events, got %d", n)
	}
}

// TestAlertEscalator_CheckStarvation_ThrottleDistinctProjects proves the
// throttle is per-project: two starved projects each emit once on the first
// call, and neither emits again on the second call.
func TestAlertEscalator_CheckStarvation_ThrottleDistinctProjects(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "proj-a", 1800)
	insertProject(t, db, "proj-b", 1800)
	oldTick := time.Now().Add(-3 * time.Hour)
	insertTick(t, db, "tick-a", "proj-a", "completed", oldTick)
	insertTick(t, db, "tick-b", "proj-b", "completed", oldTick)

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)
	if err := escalator.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #1: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 2 {
		t.Fatalf("after first call: expected 2 MEDIUM events (one per project), got %d", n)
	}

	// Second call — both throttled.
	escalator2 := NewAlertEscalator(db, events)
	if err := escalator2.CheckStarvation(context.Background()); err != nil {
		t.Fatalf("CheckStarvation #2: %v", err)
	}
	if n := countEventsBySeverity(t, db, "MEDIUM"); n != 2 {
		t.Errorf("after second call (throttled): expected 2 MEDIUM events, got %d", n)
	}
}

// TestAlertEscalator_CheckConsecutiveFailures emits HIGH when a project
// has more than 3 consecutive failed ticks.
func TestAlertEscalator_CheckConsecutiveFailures(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "failing-proj", 1800)
	now := time.Now()
	for i := 0; i < 4; i++ {
		insertTick(t, db, "fail-"+string(rune('0'+i)), "failing-proj", "failed",
			now.Add(-time.Duration(4-i)*time.Minute))
	}

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	err := escalator.CheckConsecutiveFailures(context.Background())
	if err != nil {
		t.Fatalf("CheckConsecutiveFailures: %v", err)
	}

	if n := countEventsBySeverity(t, db, "HIGH"); n != 1 {
		t.Errorf("expected 1 HIGH event, got %d", n)
	}
}

// TestAlertEscalator_CheckConsecutiveFailures_BrokenStreak does NOT emit when
// a completed tick breaks the failure streak.
func TestAlertEscalator_CheckConsecutiveFailures_BrokenStreak(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	insertProject(t, db, "recovering-proj", 1800)
	now := time.Now()
	// 2 failures, then 1 success, then 2 more failures.
	insertTick(t, db, "f-1", "recovering-proj", "failed", now.Add(-5*time.Minute))
	insertTick(t, db, "f-2", "recovering-proj", "failed", now.Add(-4*time.Minute))
	insertTick(t, db, "ok-1", "recovering-proj", "completed", now.Add(-3*time.Minute))
	insertTick(t, db, "f-3", "recovering-proj", "failed", now.Add(-2*time.Minute))
	insertTick(t, db, "f-4", "recovering-proj", "failed", now.Add(-1*time.Minute))

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	err := escalator.CheckConsecutiveFailures(context.Background())
	if err != nil {
		t.Fatalf("CheckConsecutiveFailures: %v", err)
	}

	if n := countEventsBySeverity(t, db, "HIGH"); n != 0 {
		t.Errorf("expected 0 HIGH events (streak broken), got %d", n)
	}
}

// TestAlertEscalator_CheckDuplicateWorkdirs emits HIGH when two ENABLED
// projects share the same workdir (case-insensitive), and stays silent for
// unique or disabled duplicates.
func TestAlertEscalator_CheckDuplicateWorkdirs(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Two enabled projects sharing a workdir (different case).
	insertProject(t, db, "HEADING", 43200)
	insertProject(t, db, "heading", 900)
	_, err := db.Exec(`UPDATE projects SET workdir = ? WHERE name = ?`, "/home/kara/heading", "HEADING")
	if err != nil {
		t.Fatalf("set workdir: %v", err)
	}
	_, err = db.Exec(`UPDATE projects SET workdir = ? WHERE name = ?`, "/home/kara/heading", "heading")
	if err != nil {
		t.Fatalf("set workdir: %v", err)
	}

	// Unique workdir project — must NOT trigger.
	insertProject(t, db, "solo", 900)
	_, err = db.Exec(`UPDATE projects SET workdir = ? WHERE name = ?`, "/home/kara/solo", "solo")
	if err != nil {
		t.Fatalf("set workdir: %v", err)
	}

	// Disabled duplicate — must NOT trigger.
	insertProject(t, db, "archived", 900)
	_, err = db.Exec(`UPDATE projects SET workdir = ?, enabled = 0 WHERE name = ?`, "/home/kara/shared", "archived")
	if err != nil {
		t.Fatalf("set archived: %v", err)
	}

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	if err := escalator.CheckDuplicateWorkdirs(context.Background()); err != nil {
		t.Fatalf("CheckDuplicateWorkdirs: %v", err)
	}

	if n := countEventsBySeverity(t, db, "HIGH"); n != 1 {
		t.Errorf("expected 1 HIGH event for the duplicate pair, got %d", n)
	}
	var msg string
	err = db.QueryRow(`SELECT message FROM events WHERE severity = 'HIGH'`).Scan(&msg)
	if err != nil {
		t.Fatalf("read HIGH event: %v", err)
	}
	if !strings.Contains(msg, "duplicate workdir") {
		t.Errorf("event message = %q, want duplicate-workdir mention", msg)
	}
}

// TestAlertEscalator_RunAll runs all checks without error.
func TestAlertEscalator_RunAll(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	events := NewEventLogger(db)
	escalator := NewAlertEscalator(db, events)

	// Recent eval — should not emit CRITICAL.
	err := escalator.RunAll(context.Background(), time.Now().Add(-1*time.Minute))
	if err != nil {
		t.Fatalf("RunAll: %v", err)
	}
	// RunAll should not error even with no projects.
}
