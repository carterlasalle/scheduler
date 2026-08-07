package dashboard

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// nowRFC3339Offset returns time.Now() shifted by the given nanosecond offset,
// formatted as RFC3339 — used to build last-tick-completed values relative to
// the present for nextTickIn assertions.
func nowRFC3339Offset(ns int64) string {
	return time.Now().Add(time.Duration(ns)).UTC().Format(time.RFC3339)
}

func TestReadBoardProgress(t *testing.T) {
	// Board mirroring the ozzgraph model-router matrix format:
	// Active table rows are pending, Completed rows are done, and the
	// perpetual NEVER-DONE audit is a heading (not counted).
	board := `# Test Project — Task Board

## Active

| ID | Task | Pri |
|----|------|-----|
| T03 | PR3: heartbeat | High |
| T04 | PR4: logging | High |

## Completed

| ID | Task | Pri | Commit |
|----|------|-----|--------|
| T00 | Bootstrap | Trivial | — |
| T01 | PR1: CI | Critical | abc123 |
| T02 | PR2: runtime | Critical | def456 |

## [ ] NEVER-DONE — Run 12-point audit

Never counted.
`
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.md")
	if err := os.WriteFile(path, []byte(board), 0o644); err != nil {
		t.Fatal(err)
	}

	done, total := readBoardProgress(path)
	if done != 3 {
		t.Errorf("expected 3 done, got %d", done)
	}
	if total != 5 {
		t.Errorf("expected 5 total (3 done + 2 active, NEVER-DONE excluded), got %d", total)
	}
}

func TestReadBoardProgress_MissingFile(t *testing.T) {
	done, total := readBoardProgress(filepath.Join(t.TempDir(), "nope", "tasks.md"))
	if done != 0 || total != 0 {
		t.Errorf("expected (0,0) for missing board, got (%d,%d)", done, total)
	}
}

func TestNextTickIn(t *testing.T) {
	if got := nextTickIn(true, "2026-08-06T00:00:00Z", 900); got != "running" {
		t.Errorf("running -> expected 'running', got %q", got)
	}
	if got := nextTickIn(false, "", 900); got != "—" {
		t.Errorf("no last tick -> expected '—', got %q", got)
	}
	// Last tick completed 10s ago, 900s cooldown → ~14m49-50s to next.
	// (Tolerant: a second may elapse between building the timestamp and the
	// call, so assert the minute prefix rather than an exact second.)
	if got := nextTickIn(false, nowRFC3339Offset(-10*1e9), 900); got != "in 14m 50s" && got != "in 14m 49s" {
		t.Errorf("countdown mismatch, got %q", got)
	}
	// Last tick completed 20 minutes ago, 900s cooldown → past due.
	if got := nextTickIn(false, nowRFC3339Offset(-20*60*1e9), 900); got != "due now" {
		t.Errorf("overdue -> expected 'due now', got %q", got)
	}
}
