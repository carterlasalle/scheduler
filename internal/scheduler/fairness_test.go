package scheduler

import (
	"testing"
	"time"
)

// Unit tests for the S-GAP-001 selection-fairness and spawn-failure-backoff
// primitives. Pack-level regression tests live in sgap001_regression_test.go
// (external test package).

func TestFailureBackoff(t *testing.T) {
	cases := []struct {
		name     string
		base     time.Duration
		failures int
		want     time.Duration
	}{
		{"no failures → base", 900 * time.Second, 0, 900 * time.Second},
		{"first failure → base (tick already cost the cooldown)", 900 * time.Second, 1, 900 * time.Second},
		{"2 failures → 2x", 900 * time.Second, 2, 1800 * time.Second},
		{"3 failures → 4x", 900 * time.Second, 3, 3600 * time.Second},
		{"4 failures → 8x hits 2h cap", 900 * time.Second, 4, 7200 * time.Second},
		{"many failures → still capped, no overflow", 900 * time.Second, 50, 7200 * time.Second},
		{"base exactly at cap stays", 7200 * time.Second, 3, 7200 * time.Second},
		{"3600 base ×2 lands exactly on cap", 3600 * time.Second, 2, 7200 * time.Second},
		{"high-cooldown project never sped up", 43200 * time.Second, 5, 43200 * time.Second},
		{"zero cooldown floors at minBackoffBase", 0, 3, 120 * time.Second},
		{"zero cooldown, no failures → floored base", 0, 0, 30 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FailureBackoff(c.base, c.failures); got != c.want {
				t.Errorf("FailureBackoff(%v, %d) = %v, want %v", c.base, c.failures, got, c.want)
			}
		})
	}
}

func TestStarvationWindow(t *testing.T) {
	cases := []struct {
		cooldownS int
		want      time.Duration
	}{
		{0, time.Hour},                 // dynamic/no cooldown → 1h bucket
		{600, time.Hour},               // live starved cohort
		{900, time.Hour},               // AC (a): cooldown<=3600 → 60 min
		{3600, time.Hour},              // boundary: still the 1h bucket
		{3601, 3 * 3601 * time.Second}, // just above → 3× cooldown
		{43200, 36 * time.Hour},        // self-paused-class project → 36h
	}
	for _, c := range cases {
		if got := StarvationWindow(c.cooldownS); got != c.want {
			t.Errorf("StarvationWindow(%d) = %v, want %v", c.cooldownS, got, c.want)
		}
	}
}

func TestIsStarving(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time {
		tm := now.Add(-d)
		return &tm
	}

	cases := []struct {
		name      string
		cooldownS int
		failures  int
		lastAtt   *time.Time
		createdAt time.Time
		want      bool
	}{
		{"61min ago, cooldown 900 → starving", 900, 0, ago(61 * time.Minute), time.Time{}, true},
		{"59min ago, cooldown 900 → not yet", 900, 0, ago(59 * time.Minute), time.Time{}, false},
		{"inside backoff → no boost despite window", 900, 4, ago(61 * time.Minute), time.Time{}, false},
		{"backoff elapsed AND window elapsed → starving", 900, 4, ago(3 * time.Hour), time.Time{}, true},
		{"never ran, created 2h ago → starving", 900, 0, nil, now.Add(-2 * time.Hour), true},
		{"never ran, created 30min ago → normal urgency", 900, 0, nil, now.Add(-30 * time.Minute), false},
		{"no timestamps at all → never boost", 900, 0, nil, time.Time{}, false},
		{"future attempt (clock skew) → no boost", 900, 0, ago(-5 * time.Minute), time.Time{}, false},
		{"high-cooldown project inside 3x window → not yet", 43200, 0, ago(2 * time.Hour), time.Time{}, false},
		{"high-cooldown project past 3x window → starving", 43200, 0, ago(37 * time.Hour), time.Time{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isStarving(c.cooldownS, c.failures, c.lastAtt, c.createdAt, now); got != c.want {
				t.Errorf("isStarving(cooldown=%d, failures=%d) = %v, want %v",
					c.cooldownS, c.failures, got, c.want)
			}
		})
	}
}
