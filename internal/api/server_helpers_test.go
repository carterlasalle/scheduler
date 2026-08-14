package api

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
)

// insertHelperTestTick inserts a completed tick row directly into the DB.
// The owning project must already exist (ticks.project_name is a FK).
func insertHelperTestTick(t *testing.T, db *sql.DB, id, project, status string, spawnedAt time.Time) {
	t.Helper()
	ts := spawnedAt.Format(time.RFC3339)
	if _, err := db.Exec(
		`INSERT INTO ticks (id, project_name, status, completed_at, spawned_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, project, status, ts, ts, ts); err != nil {
		t.Fatalf("insert tick %s: %v", id, err)
	}
}

func mustCreateHelperTestProject(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	if err := database.CreateProject(context.Background(), db, &database.Project{
		Name:      name,
		RepoURL:   "https://example.com/" + name,
		Workdir:   "/tmp/" + name,
		Weight:    10,
		Priority:  5,
		CooldownS: 900,
		DecayRate: 1.0,
		Model:     "test",
		Provider:  "test",
		Enabled:   true,
	}); err != nil {
		t.Fatalf("CreateProject %s: %v", name, err)
	}
}

// TestComputeProjectFailureRates_AutoDisableArmed (GAP-047) verifies the
// armed-state computation mirrors the exact auto-disable condition in
// internal/scheduler/alert_escalation.go CheckFailureRateAutoDisable:
// armed = threshold > 0 && total >= minTicks && rate >= threshold.
func TestComputeProjectFailureRates_AutoDisableArmed(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	mustCreateHelperTestProject(t, db, "heavy") // 8 failed + 2 completed → rate 0.8, total 10
	mustCreateHelperTestProject(t, db, "small") // 4 completed → total 4
	mustCreateHelperTestProject(t, db, "clean") // 5 completed → rate 0.0

	now := time.Now()
	for i := 0; i < 8; i++ {
		insertHelperTestTick(t, db, "heavy-fail-"+string(rune('a'+i)), "heavy", "failed",
			now.Add(-time.Duration(20-i)*time.Minute))
	}
	for i := 0; i < 2; i++ {
		insertHelperTestTick(t, db, "heavy-ok-"+string(rune('a'+i)), "heavy", "completed",
			now.Add(-time.Duration(5-i)*time.Minute))
	}
	for i := 0; i < 4; i++ {
		insertHelperTestTick(t, db, "small-ok-"+string(rune('a'+i)), "small", "completed",
			now.Add(-time.Duration(8-i)*time.Minute))
	}
	for i := 0; i < 5; i++ {
		insertHelperTestTick(t, db, "clean-ok-"+string(rune('a'+i)), "clean", "completed",
			now.Add(-time.Duration(6-i)*time.Minute))
	}

	// Case 1: threshold == 0 (feature off) → never armed, even at 80% failure.
	rates := computeProjectFailureRates(ctx, db, 100, 0, 5)
	if r, ok := rates["heavy"]; !ok || r.AutoDisableArmed {
		t.Errorf("threshold=0: heavy armed = %+v, want false (feature off)", rates["heavy"])
	}

	// Case 2: threshold 0.5, minTicks 5 → heavy (0.8 >= 0.5, 10 >= 5) armed;
	// small (total 4 < 5) not armed; clean (0.0 < 0.5) not armed.
	rates = computeProjectFailureRates(ctx, db, 100, 0.5, 5)
	if r, ok := rates["heavy"]; !ok || !r.AutoDisableArmed {
		t.Errorf("threshold=0.5: heavy = %+v, want armed=true", rates["heavy"])
	}
	if r, ok := rates["small"]; !ok || r.AutoDisableArmed {
		t.Errorf("threshold=0.5: small = %+v, want armed=false (total 4 < minTicks 5)", rates["small"])
	}
	if r, ok := rates["clean"]; !ok || r.AutoDisableArmed {
		t.Errorf("threshold=0.5: clean = %+v, want armed=false (rate 0 < 0.5)", rates["clean"])
	}

	// Case 3: threshold 0.9 → heavy (0.8 < 0.9) not armed.
	rates = computeProjectFailureRates(ctx, db, 100, 0.9, 5)
	if r, ok := rates["heavy"]; !ok || r.AutoDisableArmed {
		t.Errorf("threshold=0.9: heavy = %+v, want armed=false (0.8 < 0.9)", rates["heavy"])
	}

	// Case 4: minTicks 50 → heavy (total 10 < 50) not armed despite rate.
	rates = computeProjectFailureRates(ctx, db, 100, 0.5, 50)
	if r, ok := rates["heavy"]; !ok || r.AutoDisableArmed {
		t.Errorf("minTicks=50: heavy = %+v, want armed=false (10 < 50)", rates["heavy"])
	}
}

// TestComputeProjectFailureRates_AutoDisableArmedAtThreshold verifies the
// boundary semantics: rate exactly equal to the threshold arms, and the
// unrounded ratio is used (mirroring CheckFailureRateAutoDisable).
func TestComputeProjectFailureRates_AutoDisableArmedAtThreshold(t *testing.T) {
	db, err := database.InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// "exact": 5 failed + 5 completed → rate exactly 0.5.
	mustCreateHelperTestProject(t, db, "exact")
	now := time.Now()
	for i := 0; i < 5; i++ {
		insertHelperTestTick(t, db, "exact-fail-"+string(rune('a'+i)), "exact", "failed",
			now.Add(-time.Duration(10-i)*time.Minute))
		insertHelperTestTick(t, db, "exact-ok-"+string(rune('a'+i)), "exact", "completed",
			now.Add(-time.Duration(9-i)*time.Minute))
	}

	// rate == threshold → armed (>= comparison).
	rates := computeProjectFailureRates(ctx, db, 100, 0.5, 5)
	if r, ok := rates["exact"]; !ok || !r.AutoDisableArmed {
		t.Errorf("exact-threshold: exact = %+v, want armed=true (0.5 >= 0.5)", rates["exact"])
	}

	// One tick's worth below the threshold → not armed.
	rates = computeProjectFailureRates(ctx, db, 100, 0.5001, 5)
	if r, ok := rates["exact"]; !ok || r.AutoDisableArmed {
		t.Errorf("just-below: exact = %+v, want armed=false (0.5 < 0.5001)", rates["exact"])
	}
}
