package scheduler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Regression tests for GAP-001 (2026-08-04): a Hermes gateway update stopped
// accepting per-foreman "fk-*" gateway keys (401 auth errors, 8208+ failed
// ticks fleet-wide). The fleet was restored by clearing projects.gateway_key
// for every project, so Spawn() falls back to the daemon's shared
// --gateway-key. The client-level key fallback is covered by
// gateway_client_test.go; these tests pin the SPAWN-level wiring:
// Spawn() must forward project.GatewayKey to the gateway, an empty key must
// resolve to the daemon shared key, and an auth failure must surface as an
// error — never a silently "completed" tick.

// gatewaySpawnOKHandler captures the Authorization header and replies with a
// minimal valid /v1/responses payload.
func gatewaySpawnOKHandler(capturedAuth *string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp_gap001",
			"status": "completed",
			"output": []map[string]any{},
			"usage":  map[string]int{},
		})
	}
}

// TestSpawn_ForwardsPerForemanGatewayKey proves a project with GatewayKey set
// authenticates to the gateway with ITS OWN key (not the daemon shared key).
func TestSpawn_ForwardsPerForemanGatewayKey(t *testing.T) {
	db := newTestDB(t)

	var capturedAuth string
	srv := httptest.NewServer(gatewaySpawnOKHandler(&capturedAuth))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	project := PackedProject{
		Name:       "gap001-per-foreman",
		Workdir:    t.TempDir(),
		GatewayKey: "fk-test-abc",
	}
	tick, err := spawner.Spawn(project, "gap001-per-foreman-2026-08-04-15-50-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
		return
	}

	if capturedAuth != "Bearer fk-test-abc" {
		t.Errorf("Authorization = %q, want per-foreman 'Bearer fk-test-abc' — "+
			"Spawn() is not forwarding project.GatewayKey", capturedAuth)
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s", outcome.Status, TickCompleted)
	}
	if tick.SessionID != "resp_gap001" {
		t.Errorf("SessionID = %q, want real gateway response id 'resp_gap001' (S-GAP-003: no more hardcoded 'gateway')", tick.SessionID)
	}
	httpCount, execCount := spawner.SpawnMethodCounts()
	if httpCount != 1 || execCount != 0 {
		t.Errorf("SpawnMethodCounts = (%d, %d), want (1, 0) — spawn must use the gateway, not exec", httpCount, execCount)
	}
}

// TestSpawn_EmptyGatewayKeyFallsBackToDaemonKey is THE regression guard for
// the 2026-08-04 outage: a project with GatewayKey == "" (the state the fleet
// was restored to) must authenticate with the daemon's shared --gateway-key.
// Re-populating fk-* keys, or breaking the empty-key fallback in either
// Spawn() or GatewayClient.setAuth(), fails this test.
func TestSpawn_EmptyGatewayKeyFallsBackToDaemonKey(t *testing.T) {
	db := newTestDB(t)

	var capturedAuth string
	srv := httptest.NewServer(gatewaySpawnOKHandler(&capturedAuth))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	project := PackedProject{
		Name:       "gap001-daemon-fallback",
		Workdir:    t.TempDir(),
		GatewayKey: "", // post-outage fleet state: no per-foreman key
	}
	tick, err := spawner.Spawn(project, "gap001-daemon-fallback-2026-08-04-15-50-00")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if tick == nil {
		t.Fatal("Spawn returned nil tick on gateway success")
		return
	}

	if capturedAuth != "Bearer sk-daemon-shared" {
		t.Errorf("Authorization = %q, want daemon shared 'Bearer sk-daemon-shared' — "+
			"empty project.GatewayKey must fall back to --gateway-key", capturedAuth)
	}

	outcome := tick.Wait()
	if outcome.Status != TickCompleted {
		t.Errorf("Wait() status = %s, want %s", outcome.Status, TickCompleted)
	}
	httpCount, execCount := spawner.SpawnMethodCounts()
	if httpCount != 1 || execCount != 0 {
		t.Errorf("SpawnMethodCounts = (%d, %d), want (1, 0)", httpCount, execCount)
	}
}

// TestSpawn_GatewayAuthFailureLoud proves a gateway 401 surfaces as a Spawn()
// error and does NOT mark the tick completed — during the outage, failed
// spawns must be visible, never silently swallowed. With noExecFallback set,
// the tick is dropped (no exec fallback) and its row keeps its prior status.
func TestSpawn_GatewayAuthFailureLoud(t *testing.T) {
	db := newTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"type":    "auth_error",
				"message": "Invalid gateway API key",
			},
		})
	}))
	defer srv.Close()

	spawner := NewSpawner(db, 4)
	spawner.SetGatewayClient(NewGatewayClient(srv.URL, "sk-daemon-shared", 5*time.Second))
	spawner.SetNoExecFallback(true)

	// Seed the rows a real tick would have so we can prove the failure path
	// never marks the tick completed. Helpers from tick_process_infra012_test.go.
	const (
		projectName = "gap001-auth-failure"
		tickID      = "gap001-auth-failure-2026-08-04-15-50-00"
	)
	mustCreateProjectINFRA012(t, db, projectName)
	insertRunningTick(t, db, tickID, projectName, 0) // pid 0 = gateway spawn

	project := PackedProject{
		Name:       projectName,
		Workdir:    t.TempDir(),
		GatewayKey: "fk-revoked-key", // the outage: gateway rejects this key
	}
	tick, err := spawner.Spawn(project, tickID)

	if err == nil {
		t.Fatal("Spawn returned nil error on gateway 401 — auth failure was silent")
	}
	if !strings.Contains(err.Error(), "auth_error") {
		t.Errorf("error = %q, want it to surface the gateway error type 'auth_error'", err.Error())
	}
	if !strings.Contains(err.Error(), "gateway") {
		t.Errorf("error = %q, want it to mention the gateway", err.Error())
	}
	if tick != nil {
		t.Error("Spawn returned a non-nil tick on auth failure — no tick should be produced")
	}

	if got := tickStatusOf(t, db, tickID); got != "running" {
		t.Errorf("tick status = %q after failed spawn, want 'running' — "+
			"a failed spawn must NOT mark the tick completed", got)
	}

	httpCount, execCount := spawner.SpawnMethodCounts()
	if httpCount != 0 || execCount != 0 {
		t.Errorf("SpawnMethodCounts = (%d, %d), want (0, 0) — no HTTP success, no exec fallback", httpCount, execCount)
	}
}
