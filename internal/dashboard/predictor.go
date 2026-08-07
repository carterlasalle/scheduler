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

// typeEstimate picks the best duration estimate for a pending task of the given
// type: the learned per-type average if we have enough samples (minSamples),
// else the project-wide average, else a conservative default.
func typeEstimate(typ TaskType, learned map[TaskType]*durationSamples, projectAvg time.Duration, minSamples int) time.Duration {
	if ds, ok := learned[typ]; ok && ds.count >= minSamples && ds.avg > 0 {
		return ds.avg
	}
	if projectAvg > 0 {
		return projectAvg
	}
	return 15 * time.Minute // conservative floor when there is no signal
}

// predictETA returns the remaining-time estimate for the pending board steps
// plus a per-type breakdown (type → estimated total duration) and a per-type
// pending-step count. It uses per-type learned durations when available and
// falls back to the project-wide average otherwise. Done steps are ignored.
func predictETA(pending []BoardStep, learned map[TaskType]*durationSamples, projectAvg time.Duration, minSamples int) (total time.Duration, byType map[TaskType]time.Duration, counts map[TaskType]int) {
	byType = map[TaskType]time.Duration{}
	counts = map[TaskType]int{}
	for _, st := range pending {
		typ := classifyTaskType(st.Title)
		d := typeEstimate(typ, learned, projectAvg, minSamples)
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
func (g *Generator) learnedETA(ctx context.Context, project, workdir string, steps []BoardStep) (time.Duration, string, string) {
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

	total, byType, counts := predictETA(pending, learned, projectAvg, minLearnedSamples)
	if total <= 0 {
		return 0, "", ""
	}
	completionAt := time.Now().UTC().Add(total).Format(time.RFC3339)
	return total, completionAt, etaBreakdown(byType, counts)
}
