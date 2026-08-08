package dashboard

import (
	"testing"
	"time"
)

func TestClassifyTaskType(t *testing.T) {
	cases := []struct {
		title string
		want  TaskType
	}{
		{"T01 SPEC phase: write 10-section specs for daemon", TaskSpec},
		{"T02 Daemon skeleton: root systemd service, framed socket protocol", TaskCode},
		{"T03 Canonical-path + byte-hash resolution; clean env", TaskCode},
		{"T10 Tests: assert every kill-shot from red-team R1-R19", TaskTest},
		{"Integration scenario coverage", TaskTest},
		{"docs: add a deploy-an-application guide", TaskDocs},
		{"chore: bootstrap sudobroker repo (Go gates, hilo, board)", TaskChore},
		{"fix: ci toolchain pinning", TaskChore},
		{"random unclear title", TaskOther},
	}
	for _, c := range cases {
		if got := classifyTaskType(c.title); got != c.want {
			t.Errorf("classifyTaskType(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestLearnTypeSamplesAndPredict(t *testing.T) {
	// History: spec tasks took ~30m, test tasks took ~5m, code took ~20m.
	samples := []tickSample{
		{20 * time.Minute, "feat(daemon): T02 daemon skeleton"},
		{30 * time.Minute, "feat(specs): T01 10-section specs"},
		{29 * time.Minute, "feat(specs): T05 requirements schema"},
		{5 * time.Minute, "test: T10 kill-shot test suite"},
		{21 * time.Minute, "feat(engine): T03 safe exec"},
		{6 * time.Minute, "test: T10 e2e scenario"},
	}
	learned := learnTypeSamples(samples)

	// Pending board: 1 spec + 1 code + 5 tests.
	pending := []BoardStep{
		{ID: "T05", Title: "Spec: requirements doc", Status: "pending"},
		{ID: "T06", Title: "Implement shim daemon core", Status: "pending"},
		{ID: "T07", Title: "Test scenario A", Status: "pending"},
		{ID: "T08", Title: "Test scenario B", Status: "pending"},
		{ID: "T09", Title: "Test scenario C", Status: "pending"},
		{ID: "T10", Title: "Test scenario D", Status: "pending"},
		{ID: "T11", Title: "Test scenario E", Status: "pending"},
	}

	total, byType, counts := predictETA(pending, learned, 0, nil, 2)
	// spec: learned avg ~29.5m. code: learned avg ~20.5m. tests: avg ~5.5m.
	// expected ≈ 29.5m + 20.5m + 5×5.5m ≈ 78m.
	if total < 60*time.Minute || total > 100*time.Minute {
		t.Errorf("predictETA total = %v, want ~78m", total)
	}
	if counts[TaskSpec] != 1 || counts[TaskCode] != 1 || counts[TaskTest] != 5 {
		t.Errorf("counts = %v, want spec=1 code=1 test=5", counts)
	}
	if byType[TaskSpec] < 25*time.Minute || byType[TaskSpec] > 34*time.Minute {
		t.Errorf("byType spec=%v, want ~29.5m", byType[TaskSpec])
	}
}

func TestPredictFallsBackToFleetPrior(t *testing.T) {
	// Project has NO samples for "code" → blends to the fleet prior, then
	// fleet overall, never the naive floor. This is the "new project starts
	// from everything the fleet has learned" behavior.
	samples := []tickSample{
		{30 * time.Minute, "feat(specs): T01 spec"},
	}
	learned := learnTypeSamples(samples)
	fleet := &fleetModel{
		byType: map[TaskType]*durationSamples{
			TaskCode: {count: 5, avg: 22 * time.Minute},
		},
		overall: &durationSamples{count: 20, avg: 12 * time.Minute},
	}

	pending := []BoardStep{{ID: "T01", Title: "Implement the daemon executor", Status: "pending"}}
	total, _, _ := predictETA(pending, learned, 0, fleet, 2)
	if total != 22*time.Minute {
		t.Errorf("fleet-prior fallback total = %v, want 22m", total)
	}

	// Unknown type not in fleet.byType → fleet overall.
	pending2 := []BoardStep{{ID: "T02", Title: "Something with no keywords at all", Status: "pending"}}
	total2, _, _ := predictETA(pending2, learned, 0, fleet, 2)
	if total2 != 12*time.Minute {
		t.Errorf("fleet-overall fallback total = %v, want 12m", total2)
	}
}

func TestPredictFallsBackToProjectAvg(t *testing.T) {
	// No samples for "code" type → falls back to project-wide avg.
	samples := []tickSample{
		{30 * time.Minute, "feat(specs): T01 spec"},
		{6 * time.Minute, "test: scenario"},
	}
	learned := learnTypeSamples(samples)
	projectAvg := 12 * time.Minute

	pending := []BoardStep{{ID: "T01", Title: "Implement the daemon executor", Status: "pending"}}
	total, _, _ := predictETA(pending, learned, projectAvg, nil, 2)
	if total != 12*time.Minute {
		t.Errorf("fallback total = %v, want 12m", total)
	}
}

func TestEtaBreakdown(t *testing.T) {
	byType := map[TaskType]time.Duration{
		TaskTest: 5 * time.Minute,
		TaskCode: 40 * time.Minute,
	}
	counts := map[TaskType]int{TaskTest: 5, TaskCode: 1}
	s := etaBreakdown(byType, counts)
	if s == "" {
		t.Error("etaBreakdown returned empty")
	}
	if got := shortDur(75 * time.Minute); got != "1h15m" {
		t.Errorf("shortDur(75m) = %q, want 1h15m", got)
	}
}
