package scheduler

import (
	"bytes"
	"database/sql"
	"log"
	"strings"
)

// autoSlowdown detects IDLE signals in tick output and adjusts the project's cooldown.
// Uses the structured VERDICT: line from the foreman (e.g. "VERDICT: productively — IDLE").
// Cooldown caps at 24h (86400s). On any non-idle productive tick, resets to base 600s.
//
// IMPORTANT: the PRODUCTIVE reset never applies to cooldowns above autoSlowdownMaxCD (1h).
// Those are OPERATOR-SET (foreman self-pause via the PUT API, e.g. 43200s = 12h) and must
// survive the next tick's verdict — this is the "cooldown drift" fix. IDLE escalation is
// always allowed: it only increases cooldown, so it can never clobber operator intent.
const autoSlowdownMaxCD = 3600 // 1h — above this, the productive reset is skipped

func autoSlowdown(db *sql.DB, project string, output *bytes.Buffer) {
	if output == nil || output.Len() == 0 {
		return
	}

	text := output.String()

	// Detect idle: "VERDICT: ... — IDLE" or explicit "IDLE TICK" marker.
	isIdle := strings.Contains(text, "IDLE TICK") ||
		strings.Contains(text, "SLOWDOWN REQUESTED") ||
		(strings.Contains(text, "VERDICT:") && strings.Contains(text, "IDLE"))

	// Detect productive non-idle: "VERDICT: ... — PRODUCTIVE" or "FIXED"/"FIXED" keywords.
	isProductive := !isIdle && (strings.Contains(text, "VERDICT:") &&
		(strings.Contains(text, "PRODUCTIVE") || strings.Contains(text, "productively")))

	if isIdle {
		var currentCD int
		if err := db.QueryRow("SELECT cooldown_s FROM projects WHERE name = ?", project).Scan(&currentCD); err != nil {
			return
		}
		if currentCD == 0 {
			currentCD = 600
		}
		// Operator-set cooldown: never escalate either. Same guard as the
		// productive branch — 3-speed policy (Bane 08-07) pins 7200/43200 and
		// idle escalation (×1.5) was silently drifting them to 10800/64800.
		if currentCD >= autoSlowdownMaxCD {
			log.Printf("SLOWDOWN: %s cooldown %ds is operator-set (>=%ds) — idle escalation skipped", project, currentCD, autoSlowdownMaxCD)
			return
		}
		// Multiply by 1.5x instead of 2x — gentler escalation.
		newCD := currentCD + currentCD/2
		if newCD > 86400 {
			newCD = 86400
		}
		if newCD != currentCD {
			db.Exec("UPDATE projects SET cooldown_s = ? WHERE name = ?", newCD, project)
			log.Printf("SLOWDOWN: %s idle → cooldown %ds → %ds (%dm)", project, currentCD, newCD, newCD/60)
		}
	} else if isProductive {
		// Productive non-idle tick: reset cooldown to base if elevated.
		var currentCD int
		if err := db.QueryRow("SELECT cooldown_s FROM projects WHERE name = ?", project).Scan(&currentCD); err != nil {
			return
		}
		// Operator-set cooldown: never reset downward. This is the self-pause path
		// (foreman sets 43200s = 12h via PUT /api/v1/projects). Without this guard,
		// the next productive tick would silently clobber it back to 600s.
		if currentCD >= autoSlowdownMaxCD {
			log.Printf("SLOWDOWN: %s cooldown %ds is operator-set (>=%ds) — productive reset skipped", project, currentCD, autoSlowdownMaxCD)
			return
		}
		if currentCD > 600 {
			db.Exec("UPDATE projects SET cooldown_s = 600 WHERE name = ?", project)
			log.Printf("SLOWDOWN: %s productive → cooldown reset %ds → 600s", project, currentCD)
		}
	}
}
