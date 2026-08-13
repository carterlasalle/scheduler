package scheduler

import (
	"database/sql"
	"testing"
	"time"
)

// GAP-042 eval-stall watchdog tests. The event-driven loop has no periodic
// eval trigger, so a fully idle fleet (0 running ticks, everything in
// cooldown) silently stops scheduling — observed 66-min gap 2026-08-13
// 13:08-14:14 local, recovered only by manual POST /api/v1/evaluate.
// checkEvalStall runs from the 30s health ticker (always fires) and forces
// re-evaluation + a HIGH event when lastEval ages past evalStallThreshold
// with zero in-flight ticks.

func TestEvalStall_HealthyNoForce(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now()

	l.checkEvalStall(0)

	assertNoForcedEval(t, l)
	assertStallEventCount(t, db, 0)
}

func TestEvalStall_StaleWithRunningTicksNoForce(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)

	// Ticks in flight = the loop is busy; the slot-freed debounce will fire
	// an eval when a slot opens. Not a stall.
	l.checkEvalStall(2)

	assertNoForcedEval(t, l)
	assertStallEventCount(t, db, 0)
}

func TestEvalStall_NeverEvaluatedNoForce(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	// lastEval zero — startup window; the initial eval fires immediately.

	l.checkEvalStall(0)

	assertNoForcedEval(t, l)
	assertStallEventCount(t, db, 0)
}

func TestEvalStall_StaleIdleForcesEvalAndEmits(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)

	l.checkEvalStall(0)

	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)
}

func TestEvalStall_ThrottledWhilePersisting(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)

	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)

	// Still stalled a minute later: the forced re-evaluation continues on
	// every crossing, but the HIGH event is throttled until reEmitGap.
	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)
}

func TestEvalStall_ReEmitsAfterPersistGap(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)

	// A wedged loop (forced evals never run, lastEval never refreshes)
	// re-emits so the stall stays visible.
	l.lastStallEvent = time.Now().Add(-(evalStallReEmitGap + time.Minute))
	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 2)
}

func TestEvalStall_RecoveryWithinGapThrottlesNextOnset(t *testing.T) {
	db := newTestDB(t)
	l := NewLoop(db, 30*time.Second, 24*time.Hour, 10, 100, 4)
	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)

	// Loop recovers (lastEval refreshes), then a new stall episode starts
	// within the re-emit gap: the forced re-evaluation still fires, but the
	// HIGH event is throttled (one event per evalStallReEmitGap window —
	// same philosophy as the SCHED-GAP-014 starvation throttle). A fresh
	// episode after the gap re-emits immediately (covered by
	// TestEvalStall_ReEmitsAfterPersistGap).
	l.lastEval = time.Now()
	l.checkEvalStall(0)
	assertStallEventCount(t, db, 1)

	l.lastEval = time.Now().Add(-10 * time.Minute)
	l.checkEvalStall(0)
	assertForcedEval(t, l)
	assertStallEventCount(t, db, 1)
}

func assertForcedEval(t *testing.T, l *Loop) {
	t.Helper()
	select {
	case <-l.evalCh:
	case <-time.After(time.Second):
		t.Fatal("expected forced re-evaluation signal on evalCh")
	}
}

func assertNoForcedEval(t *testing.T, l *Loop) {
	t.Helper()
	select {
	case <-l.evalCh:
		t.Fatal("unexpected forced re-evaluation signal on evalCh")
	case <-time.After(50 * time.Millisecond):
	}
}

func assertStallEventCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE severity = 'HIGH' AND component = 'loop' AND message LIKE 'eval loop stalled%'`,
	).Scan(&n); err != nil {
		t.Fatalf("count stall events: %v", err)
	}
	if n != want {
		t.Fatalf("HIGH stall events = %d, want %d", n, want)
	}
}
