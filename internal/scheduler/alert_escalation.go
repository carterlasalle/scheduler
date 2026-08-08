package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// AlertEscalator checks scheduler health, project starvation, and failure
// patterns after each evaluation cycle and emits events at escalating severity.
type AlertEscalator struct {
	db     *sql.DB
	events *EventLogger
}

// NewAlertEscalator creates an escalator backed by db and events.
func NewAlertEscalator(db *sql.DB, events *EventLogger) *AlertEscalator {
	return &AlertEscalator{db: db, events: events}
}

// CheckSchedulerHealth emits CRITICAL if the scheduler has not evaluated in
// more than 10 minutes.
func (ae *AlertEscalator) CheckSchedulerHealth(ctx context.Context, lastEval time.Time) error {
	if lastEval.IsZero() {
		ae.events.Emit(ctx, SeverityCritical, "escalation", "scheduler not evaluating — never ran", map[string]any{
			"age_seconds": -1,
		})
		return nil
	}

	age := time.Since(lastEval)
	if age > 10*time.Minute {
		ae.events.Emit(ctx, SeverityCritical, "escalation",
			fmt.Sprintf("scheduler not evaluating — last eval %v ago", age.Round(time.Second)),
			map[string]any{
				"last_eval":   lastEval.Format(time.RFC3339),
				"age_seconds": age.Seconds(),
			})
	}
	return nil
}

// starvationThrottleWindow is the minimum spacing between consecutive
// starvation events for the same project (SCHED-GAP-014). The daemon
// evaluates roughly every 60s, so without throttling a starved project
// generates ~1 MEDIUM event/minute forever — 179 events in 33 minutes were
// observed across 20 projects. The escalator is constructed fresh on every
// evaluation cycle (tick_process.go), so the throttle survives by querying
// the events table for the last starvation event timestamp per project.
const starvationThrottleWindow = 30 * time.Minute

// lastStarvationEvent returns the created_at timestamp of the most recent
// MEDIUM starvation event for the given project, or ok=false if none exists.
// Uses json_extract for exact project-name matching (parameter-bound, no
// string interpolation) so similarly-named projects don't collide.
func (ae *AlertEscalator) lastStarvationEvent(ctx context.Context, project string) (time.Time, bool) {
	var ts string
	err := ae.db.QueryRowContext(ctx,
		`SELECT created_at FROM events
		 WHERE severity = ? AND component = ? AND json_extract(details, '$.project') = ?
		 ORDER BY created_at DESC LIMIT 1`,
		string(SeverityMedium), "escalation", project).Scan(&ts)
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// CheckStarvation emits MEDIUM for each enabled project that has not had a
// completed tick in more than 2× its configured maximum interval. Events are
// throttled per-project: a starvation event is emitted at most once per
// starvationThrottleWindow per project (SCHED-GAP-014).
func (ae *AlertEscalator) CheckStarvation(ctx context.Context) error {
	// Query enabled projects with their intervals.
	prows, err := ae.db.QueryContext(ctx,
		`SELECT name, cooldown_s FROM projects WHERE enabled = 1`)
	if err != nil {
		return fmt.Errorf("query projects: %w", err)
	}
	defer prows.Close()

	type projInfo struct {
		name     string
		cooldown int
	}
	var projects []projInfo
	for prows.Next() {
		var p projInfo
		if err := prows.Scan(&p.name, &p.cooldown); err != nil {
			log.Printf("ESCALATION: scan project: %v", err)
			continue
		}
		projects = append(projects, p)
	}
	if err := prows.Err(); err != nil {
		return fmt.Errorf("iter projects: %w", err)
	}

	// Get last completed tick per project.
	crows, err := ae.db.QueryContext(ctx,
		`SELECT project_name, MAX(completed_at) FROM ticks WHERE status = 'completed' GROUP BY project_name`)
	if err != nil {
		return fmt.Errorf("query completions: %w", err)
	}
	defer crows.Close()

	lastCompleted := make(map[string]time.Time)
	for crows.Next() {
		var name string
		var ts string
		if err := crows.Scan(&name, &ts); err != nil {
			log.Printf("ESCALATION: scan completion: %v", err)
			continue
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			log.Printf("ESCALATION: parse time %q: %v", ts, err)
			continue
		}
		lastCompleted[name] = t
	}
	if err := crows.Err(); err != nil {
		return fmt.Errorf("iter completions: %w", err)
	}

	// Check each project for starvation.
	now := time.Now()
	for _, proj := range projects {
		last, ok := lastCompleted[proj.name]
		if !ok {
			// Never completed a tick — not necessarily starved if it just started.
			continue
		}
		age := now.Sub(last)
		threshold := time.Duration(proj.cooldown) * time.Second * 2
		if age > threshold {
			// SCHED-GAP-014: throttle — only emit if no starvation event for
			// this project was recorded within the throttle window. This
			// prevents ~1 event/minute spam while still emitting once every
			// 30 minutes so the signal is not suppressed forever.
			if lastEmit, ok := ae.lastStarvationEvent(ctx, proj.name); ok {
				if now.Sub(lastEmit) < starvationThrottleWindow {
					continue
				}
			}
			ae.events.Emit(ctx, SeverityMedium, "escalation",
				fmt.Sprintf("project starved: %s — last tick %v ago, cooldown %ds",
					proj.name, age.Round(time.Second), proj.cooldown),
				map[string]any{
					"project":     proj.name,
					"last_tick":   last.Format(time.RFC3339),
					"cooldown":    proj.cooldown,
					"age_seconds": age.Seconds(),
				})
		}
	}
	return nil
}

// CheckConsecutiveFailures emits HIGH for any project with more than 3
// consecutive failed ticks (no completed tick interspersed).
func (ae *AlertEscalator) CheckConsecutiveFailures(ctx context.Context) error {
	// Get all enabled project names.
	prows, err := ae.db.QueryContext(ctx, `SELECT name FROM projects WHERE enabled = 1`)
	if err != nil {
		return fmt.Errorf("query projects: %w", err)
	}
	defer prows.Close()

	var names []string
	for prows.Next() {
		var name string
		if err := prows.Scan(&name); err != nil {
			log.Printf("ESCALATION: scan project name: %v", err)
			continue
		}
		names = append(names, name)
	}
	if err := prows.Err(); err != nil {
		return fmt.Errorf("iter project names: %w", err)
	}

	for _, name := range names {
		rows, err := ae.db.QueryContext(ctx,
			`SELECT id, status FROM ticks WHERE project_name = ? ORDER BY completed_at DESC LIMIT 5`,
			name)
		if err != nil {
			log.Printf("ESCALATION: query ticks for %s: %v", name, err)
			continue
		}

		var failures []string
		consecutive := 0
		for rows.Next() {
			var tickID, status string
			if err := rows.Scan(&tickID, &status); err != nil {
				log.Printf("ESCALATION: scan tick: %v", err)
				continue
			}
			if status == "failed" || status == "timeout" {
				consecutive++
				failures = append(failures, tickID)
			} else {
				break // completed/queued/running breaks the streak
			}
		}
		rows.Close()

		if consecutive > 3 {
			ae.events.Emit(ctx, SeverityHigh, "escalation",
				fmt.Sprintf("more than 3 consecutive failures: %s", name),
				map[string]any{
					"project":              name,
					"consecutive_failures": consecutive,
					"last_failure":         failures[0],
				})
		}
	}
	return nil
}

// CheckDuplicateWorkdirs emits HIGH for any pair of ENABLED projects sharing
// the same workdir. Two active foremen pointing at one directory split ticks
// unpredictably and can both commit to the same board (INFRA-009: ghost
// project `heading` duplicated `HEADING`'s workdir, spawned a tick, and
// committed into HEADING's repo). The create-path guard (database/projects.go)
// prevents new duplicates; this check flags any that already exist so the
// fleet audit surfaces them instead of letting them run silently.
func (ae *AlertEscalator) CheckDuplicateWorkdirs(ctx context.Context) error {
	prows, err := ae.db.QueryContext(ctx,
		`SELECT name, workdir FROM projects WHERE enabled = 1 AND workdir != '' ORDER BY workdir, name`)
	if err != nil {
		return fmt.Errorf("query enabled projects: %w", err)
	}
	defer prows.Close()

	byWorkdir := make(map[string][]string)
	for prows.Next() {
		var name, workdir string
		if err := prows.Scan(&name, &workdir); err != nil {
			log.Printf("ESCALATION: scan workdir project: %v", err)
			continue
		}
		key := strings.ToLower(workdir)
		byWorkdir[key] = append(byWorkdir[key], name)
	}
	if err := prows.Err(); err != nil {
		return fmt.Errorf("iter workdir projects: %w", err)
	}

	for wd, names := range byWorkdir {
		if len(names) < 2 {
			continue
		}
		ae.events.Emit(ctx, SeverityHigh, "escalation",
			fmt.Sprintf("duplicate workdir: %d enabled projects share %s", len(names), wd),
			map[string]any{
				"workdir":  wd,
				"projects": names,
			})
	}
	return nil
}

// RunAll executes all four escalation checks. Errors from individual checks
// are logged but not propagated — one check failing does not block the others.
func (ae *AlertEscalator) RunAll(ctx context.Context, lastEval time.Time) error {
	if err := ae.CheckSchedulerHealth(ctx, lastEval); err != nil {
		log.Printf("ESCALATION: health check error: %v", err)
	}
	if err := ae.CheckStarvation(ctx); err != nil {
		log.Printf("ESCALATION: starvation check error: %v", err)
	}
	if err := ae.CheckConsecutiveFailures(ctx); err != nil {
		log.Printf("ESCALATION: consecutive failures check error: %v", err)
	}
	if err := ae.CheckDuplicateWorkdirs(ctx); err != nil {
		log.Printf("ESCALATION: duplicate workdir check error: %v", err)
	}
	return nil
}
