package scheduler

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/coding-herms/scheduler/internal/database"
)

// evaluate runs one evaluation cycle.
// Phase 1 (locked): state update, cleanup, pick projects.
// Phase 2 (lock-free): fire into slot pool, alert escalation.
func (l *Loop) evaluate() {
	l.mu.Lock()

	now := time.Now()
	l.lastEval = now

	if goroCount := runtime.NumGoroutine(); goroCount > 100 {
		log.Printf("WARN: goroutine count = %d (threshold: 100)", goroCount)
	}

	l.events.Emit(context.Background(), SeverityInfo, "loop", "evaluation started", map[string]any{
		"active_ticks": l.lifecycle.RunningCount(),
		"budget":       l.weightBudget,
	})

	// Cleanup stale ticks.
	cleaned, _ := l.lifecycle.CleanupStale(90 * time.Minute)
	if cleaned > 0 {
		log.Printf("EVAL: cleaned up %d stale tick(s)", cleaned)
	}

	// Pick projects.
	var packed []PackedProject
	if l.namespaceMode && l.multiPoolPacker != nil {
		ctx := context.Background()
		// Pass ALL namespaces (enabled + disabled). Pack() skips disabled
		// namespaces itself; seeing them lets it distinguish "project points
		// at a disabled namespace" (paused — leave alone) from "project points
		// at a namespace that doesn't exist" (dangling — flat-pack fallback).
		nss, _ := database.ListNamespaces(ctx, l.db, false)
		if len(nss) > 0 {
			projs, _ := database.ListProjects(ctx, l.db, false)
			running, lastComp := l.evalContext(ctx)
			result := l.multiPoolPacker.Pack(projs, nss, l.calculator, lastComp, running, now)
			packed = result.Projects
			tickGroup := now.Format("2006-01-02-15-04-05")
			for _, nt := range result.NamespaceTicks {
				_ = database.InsertNamespaceTick(ctx, l.db, &database.NamespaceTick{
					TickGroup: tickGroup, NamespaceID: nt.NamespaceID,
					Allocated: nt.Allocated, Used: nt.Used,
					Borrowed: nt.Borrowed, Lent: nt.Lent, JobCount: nt.JobCount,
				})
			}
		}
	}
	if len(packed) == 0 {
		var err error
		runningSet := l.spawner.RunningSet()
		if l.slotPool != nil {
			runningSet = l.slotPool.RunningSet()
		}
		packed, err = l.packer.Pick(now, runningSet)
		if err != nil {
			log.Printf("EVAL: packer error: %v", err)
			l.mu.Unlock()
			return
		}
	}

	if len(packed) == 0 {
		l.mu.Unlock()
		return
	}

	log.Printf("EVAL: %d project(s) selected, %d/%d budget used",
		len(packed), sumWeights(packed), l.weightBudget)

	// Snapshot before releasing lock.
	noDeliver := l.noDeliver

	l.mu.Unlock()
	// ---- Phase 2: spawn projects (lock-free, concurrent) ----

	// Lazy-init the slot pool if not already created (test_verify, tests).
	if l.slotPool == nil {
		l.slotPool = NewSlotPool(l.maxConcur, 2*time.Hour, l.spawner, l.lifecycle)
	}

	// Gateway liveness check: ping before spawning. If gateway is dead,
	// release all slots and skip this cycle. Retry next eval.
	if l.gatewayClient != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := l.gatewayClient.Ping(ctx)
		cancel()
		if err != nil {
			if !l.gatewayDead {
				log.Printf("GATEWAY DEAD — pausing spawns, will retry in 30s: %v", err)
				l.gatewayDead = true
				l.slotPool.ReleaseAll()
			}
			return
		}
		if l.gatewayDead {
			log.Printf("GATEWAY reconnected — resuming spawns")
			l.gatewayDead = false
		}
	}

	// Fire each project into the slot pool. The pool's semaphore limits
	// concurrency — projects acquire a slot, spawn via gateway in their
	// own goroutine, and release the slot on completion/timeout.
	// evaluate() returns immediately; the pool runs autonomously.
	//
	// Dedup: skip projects already occupying a slot to prevent
	// the timeout→re-spawn→duplicate processes problem.
	alreadyRunning := l.slotPool.RunningSet()
	for _, proj := range packed {
		if alreadyRunning[proj.Name] {
			log.Printf("DEDUP: skipping %s — already running", proj.Name)
			continue
		}
		l.slotPool.Spawn(proj, now, noDeliver, l.db)
	}

	// Alert escalation runs while pool processes ticks.
	if len(packed) > 0 {
		escalator := NewAlertEscalator(l.db, l.events)
		if err := escalator.RunAll(context.Background(), now); err != nil {
			log.Printf("EVAL: escalation check error: %v", err)
		}
	}
}
func (l *Loop) evalContext(ctx context.Context) ([]string, map[string]time.Time) {
	running := make([]string, 0)
	rrows, err := l.db.QueryContext(ctx, `SELECT DISTINCT project_name FROM ticks WHERE status = 'running'`)
	if err == nil {
		defer rrows.Close()
		for rrows.Next() {
			var name string
			if err := rrows.Scan(&name); err == nil {
				running = append(running, name)
			}
		}
	}

	lastCompleted := make(map[string]time.Time)
	crows, err := l.db.QueryContext(ctx,
		`SELECT project_name, MAX(completed_at) FROM ticks WHERE status != 'running' GROUP BY project_name`)
	if err == nil {
		defer crows.Close()
		for crows.Next() {
			var name string
			var ts string
			if err := crows.Scan(&name, &ts); err == nil {
				if t, err2 := time.Parse(time.RFC3339, ts); err2 == nil {
					lastCompleted[name] = t
				}
			}
		}
	}
	return running, lastCompleted
}

func sumWeights(packed []PackedProject) int {
	total := 0
	for _, p := range packed {
		total += p.Weight
	}
	return total
}

// cleanDanglingOnStartup reaps ONLY ticks whose recorded pid no longer
// exists (exec-fallback children die with the daemon). Ticks spawned via the
// Hermes gateway have pid=0 and their HTTP sessions SURVIVE a daemon restart —
// they must be left 'running' (CleanupStale(90m) in the eval loop handles
// true gateway zombies inside the stale window).
// Regression: INFRA-012 (2026-08-01) — restart marked live gateway ticks
// 'timeout' and the packer spawned duplicate ticks for in-flight projects.
func (l *Loop) cleanDanglingOnStartup() {
	ctx := context.Background()

	// Only ticks with a real pid can be checked against /proc. pid=0 rows are
	// gateway spawns whose sessions outlive this process — leave them alone.
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, project_name, pid FROM ticks WHERE status='running' AND pid > 0`)
	if err != nil {
		log.Printf("DANGLING: startup cleanup query failed: %v", err)
		return
	}

	type deadTick struct {
		id      string
		project string
		pid     int
	}
	var dead []deadTick
	for rows.Next() {
		var id, project string
		var pid int
		if err := rows.Scan(&id, &project, &pid); err != nil {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/stat", pid)); os.IsNotExist(err) {
			dead = append(dead, deadTick{id: id, project: project, pid: pid})
		}
	}
	rows.Close()

	if len(dead) == 0 {
		log.Printf("DANGLING: startup cleanup — no dead-pid ticks found (gateway ticks with pid=0 left running)")
		return
	}

	// Bump last_tick_completed ONLY for projects whose ticks were actually
	// reaped, so the packer uses actual last-tick time for urgency. Projects
	// with live pid=0 running ticks are untouched.
	projects := make(map[string]struct{}, len(dead))
	for _, dt := range dead {
		projects[dt.project] = struct{}{}
	}
	names := make([]string, 0, len(projects))
	for name := range projects {
		names = append(names, name)
	}
	placeholders := make([]string, len(names))
	args := make([]any, len(names))
	for i, name := range names {
		placeholders[i] = "?"
		args[i] = name
	}
	if _, err := l.db.ExecContext(ctx,
		`UPDATE projects SET last_tick_completed = strftime('%Y-%m-%dT%H:%M:%S', 'now')
		 WHERE name IN (`+strings.Join(placeholders, ",")+`)`, args...); err != nil {
		log.Printf("DANGLING: last_tick_completed update failed: %v", err)
	}

	var cleaned int
	for _, dt := range dead {
		// outcome stays unset — the CHECK constraint only allows
		// ('committed','dry_run','failed','timeout'); 'zombie_reaped' from
		// reapZombies predates that constraint and violates it here.
		if _, err := l.db.ExecContext(ctx,
			`UPDATE ticks SET status='timeout' WHERE id=?`, dt.id); err != nil {
			log.Printf("DANGLING: reaping tick %s (pid=%d): %v", dt.id, dt.pid, err)
			continue
		}
		cleaned++
	}
	if cleaned > 0 {
		log.Printf("DANGLING: cleaned %d dead-pid running tick(s) from previous process", cleaned)
	}
}

func (l *Loop) reapZombies() {
	ctx := context.Background()
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, pid FROM ticks WHERE status='running' AND pid > 0`)
	if err != nil {
		log.Printf("ZOMBIE: reaper query failed: %v", err)
		return
	}
	defer rows.Close()

	var reaped int
	for rows.Next() {
		var id string
		var pid int
		if err := rows.Scan(&id, &pid); err != nil {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/proc/%d/stat", pid)); os.IsNotExist(err) {
			if _, err := l.db.ExecContext(ctx,
				`UPDATE ticks SET status='timeout', outcome='zombie_reaped' WHERE id=?`, id); err != nil {
				log.Printf("ZOMBIE: reaping tick %s: %v", id, err)
				continue
			}
			reaped++
		}
	}
	if reaped > 0 {
		log.Printf("ZOMBIE: reaped %d ticks (process died)", reaped)
	}
}
