package scheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCountPending_JSONL(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := `{"id":"TASK-001","status":"pending","title":"fix bug"}` + "\n" +
		`{"id":"TASK-002","status":"pending","title":"add tests"}` + "\n" +
		`{"id":"TASK-003","status":"complete","title":"done"}` + "\n" +
		`{this is malformed JSON` + "\n"
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewPendingTaskCounter(60 * time.Second)
	got := c.CountPending(dir)
	if got != 2 {
		t.Errorf("CountPending(jsonl) = %d, want 2 (2 pending, 1 complete, 1 malformed)", got)
	}
}

func TestCountPending_MD_Fallback(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := "## [ ] GAP-001 - fix bug\n" +
		"## [ ] GAP-002 - add tests\n" +
		"## [ ] GAP-003 - update docs\n" +
		"## [x] GAP-000 - done task\n"
	path := filepath.Join(boardDir, "tasks.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewPendingTaskCounter(60 * time.Second)
	got := c.CountPending(dir)
	if got != 3 {
		t.Errorf("CountPending(md) = %d, want 3 (3 unchecked, 1 checked)", got)
	}
}

func TestCountPending_NoBoardFiles(t *testing.T) {
	dir := t.TempDir()
	c := NewPendingTaskCounter(60 * time.Second)
	got := c.CountPending(dir)
	if got != 0 {
		t.Errorf("CountPending(empty dir) = %d, want 0", got)
	}
}

func TestCountPending_EmptyWorkdir(t *testing.T) {
	c := NewPendingTaskCounter(60 * time.Second)
	got := c.CountPending("")
	if got != 0 {
		t.Errorf("CountPending(empty) = %d, want 0", got)
	}
}

func TestCountPending_MtimeReread(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(`{"status":"pending"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewPendingTaskCounter(60 * time.Second)
	got := c.CountPending(dir)
	if got != 1 {
		t.Fatalf("first CountPending = %d, want 1", got)
	}
	got = c.CountPending(dir)
	if got != 1 {
		t.Errorf("second CountPending (cached) = %d, want 1", got)
	}
	// Sleep so mtime granularity changes
	time.Sleep(1100 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"status":"pending"}`+"\n"+`{"status":"pending"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile 2: %v", err)
	}
	got = c.CountPending(dir)
	if got != 2 {
		t.Errorf("third CountPending (mtime changed) = %d, want 2", got)
	}
}

func TestCountPending_TTLExpiry(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(`{"status":"pending"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewPendingTaskCounter(100 * time.Millisecond)
	got := c.CountPending(dir)
	if got != 1 {
		t.Fatalf("first CountPending = %d, want 1", got)
	}
	got = c.CountPending(dir)
	if got != 1 {
		t.Errorf("cached CountPending = %d, want 1", got)
	}
	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"status":"pending"}`+"\n"+`{"status":"pending"}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile 2: %v", err)
	}
	got = c.CountPending(dir)
	if got != 2 {
		t.Errorf("post-TTL CountPending = %d, want 2", got)
	}
}

func TestCountPending_Concurrent(t *testing.T) {
	dir := t.TempDir()
	boardDir := filepath.Join(dir, ".coding-hermes", "board")
	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	content := `{"status":"pending"}` + "\n" + `{"status":"complete"}` + "\n"
	path := filepath.Join(boardDir, "tasks.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	c := NewPendingTaskCounter(time.Second)
	done := make(chan int, 20)
	for i := 0; i < 20; i++ {
		go func() {
			got := c.CountPending(dir)
			done <- got
		}()
	}
	for i := 0; i < 20; i++ {
		got := <-done
		if got != 1 {
			t.Errorf("goroutine CountPending = %d, want 1", got)
		}
	}
}

func TestPendingBoostUrgencyFor(t *testing.T) {
	cases := []struct {
		name    string
		pending int
		want    float64
	}{
		{"zero pending", 0, pendingBoostUrgency},
		{"one pending", 1, pendingBoostUrgency + 1},
		{"ten pending", 10, pendingBoostUrgency + 10},
		{"at cap (1000)", 1000, pendingBoostUrgency + 1000},
		{"above cap (1001)", 1001, pendingBoostUrgency + 1000},
		{"negative", -5, pendingBoostUrgency},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pendingBoostUrgencyFor(c.pending)
			if got != c.want {
				t.Errorf("pendingBoostUrgencyFor(%d) = %v, want %v", c.pending, got, c.want)
			}
			if got >= starvationBoostUrgency {
				t.Errorf("pendingBoostUrgencyFor(%d) = %v >= starvationBoostUrgency %v", c.pending, got, starvationBoostUrgency)
			}
		})
	}
}

func TestPendingBoost_BelowStarvation(t *testing.T) {
	maxPending := pendingBoostUrgencyFor(10000)
	minStarvation := starvationBoostUrgencyFor(0)
	if maxPending >= minStarvation {
		t.Errorf("max pending boost %v >= min starvation boost %v", maxPending, minStarvation)
	}
	if maxPending < 100000 {
		t.Errorf("max pending boost %v < 100000 - too close to organic urgency range", maxPending)
	}
}
