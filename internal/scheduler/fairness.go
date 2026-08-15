package scheduler

import "time"

// Selection-fairness and spawn-failure-backoff primitives (S-GAP-001,
// 2026-08-04).
//
// Problem 1 — starvation: urgency-greedy selection lets the prio-10 cohort
// (urgency 2500-11800) permanently monopolize all concurrency slots; 16
// enabled projects (urgency 118-2038) were NEVER picked despite being hours
// past cooldown. Fix: any eligible project whose last tick ATTEMPT is older
// than its starvation window gets a massive urgency boost, guaranteeing it a
// slot in the next pack.
//
// Problem 2 — retry storms: a broken gateway produced 2444 failed ticks for
// one project in a day (~160/min during the outage) because nothing slowed
// retries. Fix: consecutive spawn failures multiply the project's effective
// cooldown exponentially (base × 2^(failures-1)), capped at backoffCap.
//
// The two mechanisms compose: the starvation boost only fires once the
// backoff-adjusted cooldown has ALSO elapsed, so a persistently failing
// project is retried at most once per max(starvationWindow, backoff) — AC(a)
// (≥1 attempt per 60 min for cooldown<=3600s projects) holds whenever the
// project is not backed off, and AC(b) (>50 consecutive failures impossible)
// holds because backoff reaches the 2h cap after ~4 failures (≤12 attempts
// per day per project).
//
// Reopened 2026-08-05: the original boost was FLAT (1e12 for every starving
// project), so simultaneously starving projects tied on urgency and the
// priority-desc tie-break handed every slot to the prio-10 starved cohort —
// the prio-5 tier logged FAIRNESS every eval yet received zero spawn
// attempts for 25-34h live. The boost is now MONOTONIC IN STARVATION AGE
// (starvationBoostUrgencyFor): most-starved first, regardless of priority.

const (
	// starvationBoostUrgency is the BASE of the fairness boost — far above
	// any organically reachable urgency (live fleet tops out ~12k), so a
	// starving project always sorts ahead of every non-starving project.
	// Among several simultaneously starving projects the age term added by
	// starvationBoostUrgencyFor decides (most-starved first); the
	// priority-desc tie-break only fires for identical starvation ages.
	starvationBoostUrgency = 1e12

	// maxStarvationAge caps the starvation-age term added to the boost.
	// 1e6s ≈ 11.6 days: at the cap the boosted value (1e12 + 1e6) is still
	// >10^7× the highest organic urgency, and the float64 sum stays exact
	// (2^53 ≈ 9e15). Two projects both starved past the cap tie and fall
	// back to the standard tie-breaks — acceptable: with the age-monotonic
	// boost in place no project should ever accumulate that much age.
	maxStarvationAge = 1000000 * time.Second

	// backoffCap is the maximum effective cooldown produced by failure
	// backoff. 2h ⇒ at most 12 attempts/day/project at full backoff.
	backoffCap = 2 * time.Hour

	// minBackoffBase floors the backoff base when a project has no explicit
	// cooldown (cooldown_s = 0 ⇒ "no cooldown" in namespace mode). Without a
	// floor a failing cooldown-0 project would storm unthrottled.
	minBackoffBase = 30 * time.Second
)

// StarvationWindow returns how long an eligible enabled project may go
// without a tick attempt before selection fairness forces one:
//
//	cooldown <= 1h  → 1h   (acceptance criterion S-GAP-001a)
//	cooldown >  1h  → 3 × cooldown
//
// cooldown_s <= 0 (dynamic / no cooldown) falls in the 1h bucket.
func StarvationWindow(cooldownS int) time.Duration {
	cd := time.Duration(cooldownS) * time.Second
	if cd <= time.Hour {
		return time.Hour
	}
	return 3 * cd
}

// FailureBackoff returns the effective cooldown for a project with the given
// count of consecutive spawn failures:
//
//	eff = base × 2^(failures-1)   capped at max(backoffCap, base)
//
// failures <= 1 returns base unchanged (first failure costs nothing extra —
// the tick itself already consumed the base cooldown). The cap never goes
// below base, so high-cooldown projects (e.g. a self-paused 12h project) are
// never sped up. base <= 0 floors at minBackoffBase so cooldown-less
// projects still back off under repeated failure.
func FailureBackoff(base time.Duration, consecutiveFailures int) time.Duration {
	if base <= 0 {
		base = minBackoffBase
	}
	if consecutiveFailures <= 1 {
		return base
	}
	cap := backoffCap
	if base > cap {
		cap = base
	}
	shift := consecutiveFailures - 1
	// base >= 1s with shift >= 13 already exceeds the 2h cap — stop early
	// instead of overflowing the Duration on pathological counters.
	if shift >= 13 {
		return cap
	}
	eff := base << shift
	if eff <= 0 || eff > cap {
		return cap
	}
	return eff
}

// isStarving reports whether a project must receive the fairness boost on
// this evaluation: its last tick attempt (falling back to createdAt when it
// has never run) is older than its starvation window AND its backoff-adjusted
// cooldown has fully elapsed. The backoff gate is what keeps a persistently
// failing project from being boosted every eval — the guarantee is "at
// least one attempt per max(window, effectiveCooldown)", never a bypass of
// backoff. Running projects are excluded by the caller (running-set dedup);
// projects with no usable timestamp are not boosted (normal urgency rules).
func isStarving(cooldownS, consecutiveFailures int, lastAttempt *time.Time, createdAt, now time.Time) bool {
	ref := createdAt
	if lastAttempt != nil {
		ref = *lastAttempt
	}
	if ref.IsZero() {
		return false
	}
	elapsed := now.Sub(ref)
	if elapsed < 0 {
		return false
	}
	base := time.Duration(cooldownS) * time.Second
	if elapsed < FailureBackoff(base, consecutiveFailures) {
		return false
	}
	return elapsed > StarvationWindow(cooldownS)
}

// starvationAge returns how long a project has gone without a tick attempt,
// using the same reference clock as isStarving (last attempt, falling back
// to createdAt when it has never run). Floored at 0: a project with no
// usable timestamp or a future timestamp (clock skew) reports age 0.
func starvationAge(lastAttempt *time.Time, createdAt, now time.Time) time.Duration {
	ref := createdAt
	if lastAttempt != nil {
		ref = *lastAttempt
	}
	if ref.IsZero() {
		return 0
	}
	if d := now.Sub(ref); d > 0 {
		return d
	}
	return 0
}

// starvationBoostUrgencyFor returns the fairness-boosted urgency for a
// starving project: starvationBoostUrgency plus the starvation age in
// seconds, capped at maxStarvationAge. The result is monotonic
// non-decreasing in elapsed, so when several projects starve simultaneously
// the MOST-starved one sorts first regardless of priority — the prio-10
// starved cohort can no longer outvote the prio-5 tier on the priority-desc
// tie-break (S-GAP-001 reopen, 2026-08-05). Even at the cap the sum
// (1e12 + 1e6) remains >10^7× any organic urgency (~12k max live).
func starvationBoostUrgencyFor(elapsed time.Duration) float64 {
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > maxStarvationAge {
		elapsed = maxStarvationAge
	}
	return starvationBoostUrgency + elapsed.Seconds()
}
