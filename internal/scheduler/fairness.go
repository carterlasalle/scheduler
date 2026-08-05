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

const (
	// starvationBoostUrgency is far above any organically reachable urgency
	// (live fleet tops out ~12k), so a starving project always sorts first.
	// Among several simultaneously starving projects the existing tie-breaks
	// apply (priority desc, oldest attempt first).
	starvationBoostUrgency = 1e12

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
