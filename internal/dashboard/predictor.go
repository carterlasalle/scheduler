package dashboard

import (
	"context"
	"sort"
	"strings"
	"time"
)

// TaskType buckets a board task (or a completed tick's work) into a coarse
// work category so ETA can learn per-type durations instead of assuming every
// step takes the same time. The key insight: "2 steps left" is a bad signal
// when both are heavy implementation tasks, and "10 steps left" is a bad
// signal when all ten are fast smoke tests.
type TaskType string

const (
	TaskSpec  TaskType = "spec"
	TaskCode  TaskType = "code"
	TaskTest  TaskType = "test"
	TaskDocs  TaskType = "docs"
	TaskChore TaskType = "chore"
	TaskOther TaskType = "other"
)

// typeKeywords maps each TaskType to the substrings that route a title to it.
// Keywords are checked by scoring, not first-match, so "Tests: assert every
// kill-shot" routes to test even though it also mentions code.
var typeKeywords = map[TaskType][]string{
	TaskSpec:  {"spec", "schema", "plan", "design", "architect", "adr", "requirement", "10-section", "requirements"},
	TaskTest:  {"test", "assert", "kill-shot", "integration", "e2e", "unit", "coverage", "verify", "smoke", "scenario"},
	TaskDocs:  {"doc", "readme", "guide", "runbook"},
	TaskChore: {"chore", "bootstrap", "config", "ci", "dependenc", "toolchain", "refactor", "lint", "cleanup", "migrat"},
	TaskCode:  {"implement", "daemon", "shim", "engine", "core", "feat", "add ", "wire", "worker", "adapter", "provider", "workflow", "controller", "api", "policy", "audit", "protocol", "skeleton", "exec", "socket", "plugin", "path", "hash", "env", "relay", "resolve", "canonical", "parse", "fingerprint", "atomic", "idempot", "queue", "health"},
}

// priority orders tie-breaks. spec and test are checked with higher weight
// than code because a task can legitimately mention both (e.g. a test task
// named "tests for the daemon"). other is last.
var typePriority = []TaskType{TaskSpec, TaskTest, TaskDocs, TaskChore, TaskCode, TaskOther}

// classifyTaskType routes a task title or commit-work string to a TaskType by
// keyword score. Empty/unknown input falls back to TaskOther.
func classifyTaskType(title string) TaskType {
	low := strings.ToLower(title)
	scores := map[TaskType]int{}
	for typ, kws := range typeKeywords {
		for _, kw := range kws {
			if strings.Contains(low, kw) {
				scores[typ]++
			}
		}
	}
	best := TaskOther
	bestScore := 0
	for _, typ := range typePriority {
		s := scores[typ]
		if s > bestScore {
			best = typ
			bestScore = s
		}
	}
	return best
}

// durationSamples accumulates completed-tick durations for one TaskType.
type durationSamples struct {
	count int
	total time.Duration
	avg   time.Duration // avg is 0 until finalize() is called
}

func (s *durationSamples) add(d time.Duration) {
	s.count++
	s.total += d
}

func (s *durationSamples) finalize() {
	if s.count > 0 {
		s.avg = s.total / time.Duration(s.count)
	}
}

// tickSample is one completed tick's duration plus the work it did (commit
// subject text), used to learn per-type durations.
type tickSample struct {
	dur  time.Duration
	work string
}

// learnTypeSamples buckets completed-tick samples by classified type and
// computes the per-type average duration. Returns nil-safe map.
func learnTypeSamples(samples []tickSample) map[TaskType]*durationSamples {
	learned := map[TaskType]*durationSamples{}
	for _, s := range samples {
		typ := classifyTaskType(s.work)
		ds, ok := learned[typ]
		if !ok {
			ds = &durationSamples{}
			learned[typ] = ds
		}
		ds.add(s.dur)
	}
	for _, ds := range learned {
		ds.finalize()
	}
	return learned
}

// fleetModel is the fleet-wide learned prior: per-task-type average durations
// aggregated across ALL projects, plus the fleet overall average. A new project
// starts from this prior and blends toward its own data as it accumulates.
type fleetModel struct {
	byType  map[TaskType]*durationSamples
	overall *durationSamples // fleet-wide average across all types
}

// fleetLearned aggregates completed-tick durations across every project,
// bucketed by task type, to form the fleet-wide prior. Returns an empty model
// on query error so callers degrade to project-only estimates.
func (g *Generator) fleetLearned(ctx context.Context) *fleetModel {
	m := &fleetModel{byType: map[TaskType]*durationSamples{}, overall: &durationSamples{}}

	// Map project → workdir once, so we can classify each tick's commit work.
	wd := map[string]string{}
	if rows, err := g.db.QueryContext(ctx, `SELECT name, COALESCE(workdir,'') FROM projects`); err == nil {
		for rows.Next() {
			var name, w string
			if rows.Scan(&name, &w) == nil {
				wd[name] = w
			}
		}
		_ = rows.Close()
	}

	rows, err := g.db.QueryContext(ctx, `
		SELECT project_name, spawned_at, completed_at FROM ticks
		WHERE status = 'completed' AND completed_at != ''
		ORDER BY spawned_at DESC LIMIT 200
	`)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var proj, sp, co string
		if rows.Scan(&proj, &sp, &co) != nil {
			continue
		}
		d := parseDuration(sp, co)
		if d <= 0 {
			continue
		}
		typ := classifyTaskType(tickWork(wd[proj], sp, co, 4))
		ds := m.byType[typ]
		if ds == nil {
			ds = &durationSamples{}
			m.byType[typ] = ds
		}
		ds.add(d)
		m.overall.add(d)
	}
	for _, ds := range m.byType {
		ds.finalize()
	}
	m.overall.finalize()
	return m
}

// typeEstimate picks the best duration estimate for a pending task of the given
// type, blending the project's own learned per-type average with the fleet-wide
// prior. A project with enough local samples dominates; a new project leans on
// the fleet prior; then the fleet overall; then project overall; then a floor.
func typeEstimate(typ TaskType, learned map[TaskType]*durationSamples, projectAvg time.Duration, fleet *fleetModel, minSamples int) time.Duration {
	if ds, ok := learned[typ]; ok && ds.count >= minSamples && ds.avg > 0 {
		return ds.avg
	}
	if fleet != nil {
		if fds, ok := fleet.byType[typ]; ok && fds.count >= minSamples && fds.avg > 0 {
			return fds.avg
		}
		if fleet.overall != nil && fleet.overall.count > 0 && fleet.overall.avg > 0 {
			return fleet.overall.avg
		}
	}
	if projectAvg > 0 {
		return projectAvg
	}
	return 15 * time.Minute // conservative floor when there is no signal
}

// predictETA returns the remaining-time estimate for the pending board steps
// plus a per-type breakdown (type → estimated total duration) and a per-type
// pending-step count. It blends per-type project-learned durations with the
// fleet-wide prior and falls back to the fleet/project overall average. Done
// steps are ignored.
func predictETA(pending []BoardStep, learned map[TaskType]*durationSamples, projectAvg time.Duration, fleet *fleetModel, minSamples int) (total time.Duration, byType map[TaskType]time.Duration, counts map[TaskType]int) {
	byType = map[TaskType]time.Duration{}
	counts = map[TaskType]int{}
	for _, st := range pending {
		typ := classifyTaskType(st.Title)
		d := typeEstimate(typ, learned, projectAvg, fleet, minSamples)
		total += d
		byType[typ] += d
		counts[typ]++
	}
	return total, byType, counts
}

// etaBreakdown renders a compact human-readable per-type breakdown for the ETA
// tooltip, e.g. "code 2·40m + test 5·5m". Ordered by contribution desc.
func etaBreakdown(byType map[TaskType]time.Duration, counts map[TaskType]int) string {
	type part struct {
		typ TaskType
		d   time.Duration
	}
	var parts []part
	for typ, d := range byType {
		if d > 0 {
			parts = append(parts, part{typ, d})
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].d > parts[j].d })
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			b.WriteString(" + ")
		}
		b.WriteString(string(p.typ))
		if n := counts[p.typ]; n > 1 {
			b.WriteString(" ×")
			b.WriteString(itoa(n))
		}
		b.WriteString(" ")
		b.WriteString(shortDur(p.d))
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func shortDur(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	if m < 60 {
		return itoa(m) + "m"
	}
	h := m / 60
	r := m % 60
	if r == 0 {
		return itoa(h) + "h"
	}
	return itoa(h) + "h" + itoa(r) + "m"
}

// minLearnedSamples is the minimum per-type completed-tick count before the
// learned per-type average is trusted over the project-wide fallback.
const minLearnedSamples = 2

// learnedETA computes the remaining-time estimate for a project from its board
// and tick history. It learns per-task-type durations from recent completed
// ticks (via their commit work) and predicts remaining time by summing the
// type-appropriate estimate for each pending board step, instead of the naive
// "average tick × remaining steps".
//
// Returns (eta, completionAtRFC3339, breakdown). eta is 0 when there is no
// signal (no board or no history) so callers can fall back to the old math.
func (g *Generator) learnedETA(ctx context.Context, project, workdir string, steps []BoardStep, fleet *fleetModel) (time.Duration, string, string) {
	if project == "" || len(steps) == 0 {
		return 0, "", ""
	}

	// Gather recent completed ticks + their work, for per-type learning.
	rows, err := g.db.QueryContext(ctx, `
		SELECT spawned_at, completed_at FROM ticks
		WHERE project_name = ? AND status = 'completed' AND completed_at != ''
		ORDER BY spawned_at DESC LIMIT 20
	`, project)
	var samples []tickSample
	var projTotal time.Duration
	var projCount int
	if err == nil {
		for rows.Next() {
			var sp, co string
			if rows.Scan(&sp, &co) == nil {
				if d := parseDuration(sp, co); d > 0 {
					samples = append(samples, tickSample{dur: d, work: tickWork(workdir, sp, co, 4)})
					projTotal += d
					projCount++
				}
			}
		}
		_ = rows.Close()
	}

	learned := learnTypeSamples(samples)
	var projectAvg time.Duration
	if projCount > 0 {
		projectAvg = projTotal / time.Duration(projCount)
	}

	// Pending steps are anything not done (active + pending).
	var pending []BoardStep
	for _, s := range steps {
		if s.Status != "done" {
			pending = append(pending, s)
		}
	}
	if len(pending) == 0 {
		return 0, "", ""
	}

	total, byType, counts := predictETA(pending, learned, projectAvg, fleet, minLearnedSamples)
	if total <= 0 {
		return 0, "", ""
	}
	completionAt := time.Now().UTC().Add(total).Format(time.RFC3339)
	return total, completionAt, etaBreakdown(byType, counts)
}
