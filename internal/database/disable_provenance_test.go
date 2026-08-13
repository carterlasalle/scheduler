package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
)

// GAP-044 disable-provenance tests. Disabled projects previously had no
// record of how/when/why they were disabled (ch-delta: enabled=false,
// no reason, no event). Every disable path must now stamp
// disabled_at/disabled_by/disabled_reason and the API must surface them.

func TestUpdateProject_DisableStampsProvenance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	p := sampleProject("proj-a")
	if err := CreateProject(ctx, db, p); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := UpdateProject(ctx, db, "proj-a", ProjectUpdates{Enabled: BoolPtr(false)}); err != nil {
		t.Fatalf("disable project: %v", err)
	}

	got, err := GetProject(ctx, db, "proj-a")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	if got.Enabled {
		t.Fatal("project still enabled")
	}
	if got.DisabledBy != "api" {
		t.Fatalf("DisabledBy = %q, want \"api\"", got.DisabledBy)
	}
	if got.DisabledAt == "" {
		t.Fatal("DisabledAt empty — must be stamped on disable")
	}
	if !strings.Contains(got.DisabledReason, "API") {
		t.Fatalf("DisabledReason = %q, want default API reason", got.DisabledReason)
	}
}

func TestUpdateProject_DisableHonorsExplicitProvenance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("proj-b")); err != nil {
		t.Fatalf("create project: %v", err)
	}

	by := "api-pause"
	reason := "paused via POST /pause"
	if err := UpdateProject(ctx, db, "proj-b", ProjectUpdates{
		Enabled:        BoolPtr(false),
		DisabledBy:     &by,
		DisabledReason: &reason,
	}); err != nil {
		t.Fatalf("pause project: %v", err)
	}

	got, _ := GetProject(ctx, db, "proj-b")
	if got.DisabledBy != "api-pause" {
		t.Fatalf("DisabledBy = %q, want \"api-pause\"", got.DisabledBy)
	}
	if got.DisabledReason != "paused via POST /pause" {
		t.Fatalf("DisabledReason = %q, want explicit reason", got.DisabledReason)
	}
	if got.DisabledAt == "" {
		t.Fatal("DisabledAt empty")
	}
}

func TestUpdateProject_ResumeClearsProvenance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("proj-c")); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := UpdateProject(ctx, db, "proj-c", ProjectUpdates{Enabled: BoolPtr(false)}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if err := UpdateProject(ctx, db, "proj-c", ProjectUpdates{Enabled: BoolPtr(true)}); err != nil {
		t.Fatalf("resume: %v", err)
	}

	got, _ := GetProject(ctx, db, "proj-c")
	if !got.Enabled {
		t.Fatal("project not re-enabled")
	}
	if got.DisabledBy != "" || got.DisabledAt != "" || got.DisabledReason != "" {
		t.Fatalf("provenance not cleared on resume: by=%q at=%q reason=%q",
			got.DisabledBy, got.DisabledAt, got.DisabledReason)
	}
}

func TestUpdateProject_NonEnabledUpdateLeavesProvenanceUntouched(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("proj-d")); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := UpdateProject(ctx, db, "proj-d", ProjectUpdates{Enabled: BoolPtr(false)}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	before, _ := GetProject(ctx, db, "proj-d")

	cd := 7200
	if err := UpdateProject(ctx, db, "proj-d", ProjectUpdates{CooldownS: &cd}); err != nil {
		t.Fatalf("cooldown update: %v", err)
	}

	after, _ := GetProject(ctx, db, "proj-d")
	if after.DisabledBy != before.DisabledBy || after.DisabledAt != before.DisabledAt || after.DisabledReason != before.DisabledReason {
		t.Fatalf("non-enabled update changed provenance: %+v -> %+v", before, after)
	}
}

func TestDeleteProject_StampsProvenance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("proj-e")); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// The API only allows DELETE on already-disabled projects (409
	// otherwise) — simulate the pre-disabled legacy state, including a
	// row disabled BEFORE provenance columns existed (NULLs).
	if _, err := db.ExecContext(ctx, `UPDATE projects SET enabled = 0 WHERE name = 'proj-e'`); err != nil {
		t.Fatalf("legacy disable: %v", err)
	}

	if err := DeleteProject(ctx, db, "proj-e"); err != nil {
		t.Fatalf("delete project: %v", err)
	}

	got, _ := GetProject(ctx, db, "proj-e")
	if got.Enabled {
		t.Fatal("project still enabled after delete")
	}
	if got.DisabledBy != "api-delete" {
		t.Fatalf("DisabledBy = %q, want \"api-delete\" (legacy backfill)", got.DisabledBy)
	}
	if got.DisabledAt == "" {
		t.Fatal("DisabledAt empty — legacy backfill must stamp it")
	}
	if !strings.Contains(got.DisabledReason, "DELETE") {
		t.Fatalf("DisabledReason = %q, want api-delete reason", got.DisabledReason)
	}
}

func TestDeleteProject_KeepsExistingProvenance(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	if err := CreateProject(ctx, db, sampleProject("proj-f")); err != nil {
		t.Fatalf("create project: %v", err)
	}
	by := "api-pause"
	reason := "paused earlier"
	if err := UpdateProject(ctx, db, "proj-f", ProjectUpdates{
		Enabled:        BoolPtr(false),
		DisabledBy:     &by,
		DisabledReason: &reason,
	}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	before, _ := GetProject(ctx, db, "proj-f")

	if err := DeleteProject(ctx, db, "proj-f"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	after, _ := GetProject(ctx, db, "proj-f")
	if after.DisabledBy != "api-pause" || after.DisabledReason != "paused earlier" {
		t.Fatalf("delete overwrote existing provenance: by=%q reason=%q", after.DisabledBy, after.DisabledReason)
	}
	if after.DisabledAt != before.DisabledAt {
		t.Fatal("delete changed existing DisabledAt")
	}
}

func TestProjectJSON_ExposesDisableProvenance(t *testing.T) {
	// The wire format must carry the provenance fields so /api/v1/projects
	// and /api/v1/projects/{name} surface them.
	p := Project{
		Name:           "proj-g",
		Enabled:        false,
		DisabledAt:     "2026-08-13T21:30:00Z",
		DisabledBy:     "auto-disable",
		DisabledReason: "failure rate 95.0% (19/20 ticks)",
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"disabled_at"`, `"disabled_by"`, `"disabled_reason"`} {
		if !strings.Contains(string(out), key) {
			t.Fatalf("JSON missing %s: %s", key, out)
		}
	}
}

func TestMigration12_AddsProvenanceColumns(t *testing.T) {
	db := newTestDB(t) // InitDB runs all migrations incl. v12
	ctx := context.Background()
	cols, err := db.QueryContext(ctx, `PRAGMA table_info(projects)`)
	if err != nil {
		t.Fatalf("pragma: %v", err)
	}
	defer cols.Close()
	seen := map[string]bool{}
	for cols.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		seen[name] = true
	}
	for _, want := range []string{"disabled_at", "disabled_by", "disabled_reason"} {
		if !seen[want] {
			t.Fatalf("migration v12 missing column %s", want)
		}
	}
}
