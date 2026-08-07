package dashboard

import (
	"fmt"
	"html/template"
	"strings"
)

// renderLineChart builds a hand-rolled SVG line chart from []SpeedCostPoint.
// mode "speed" plots tick duration in seconds (higher = slower, so a shorter
// bar is faster), "cost" plots cost_usd. Returns "" for empty/invalid input.
// No external chart library — the dashboard is deliberately no-CDN.
//
// Layout: fixed 640x160 viewBox. Y axis auto-scales to the max value with a
// 10% headroom; X axis is evenly spaced across the points. A subtle area fill
// under the line and a value label at the last point help readability.
func renderLineChart(pts []SpeedCostPoint, mode string) template.HTML {
	if len(pts) < 2 {
		return ""
	}
	const w, h = 640.0, 160.0
	const padL, padR, padT, padB = 8.0, 46.0, 12.0, 20.0

	plotW := w - padL - padR
	plotH := h - padT - padB

	// Pull the numeric series per mode.
	values := make([]float64, len(pts))
	maxV := 0.0
	for i, p := range pts {
		var v float64
		if mode == "cost" {
			v = p.Cost
		} else {
			v = float64(p.Duration)
		}
		values[i] = v
		if v > maxV {
			maxV = v
		}
	}
	if maxV <= 0 {
		return ""
	}
	// Add headroom so the tallest point isn't flush against the top.
	scale := maxV * 1.12
	if scale <= 0 {
		scale = 1
	}

	// Build the polyline + area path + last-point label.
	var line, area strings.Builder
	var lastX, lastY float64
	step := plotW / float64(len(pts)-1)
	for i, v := range values {
		x := padL + step*float64(i)
		y := padT + plotH*(1.0-v/scale)
		line.WriteString(fmt.Sprintf("%s%.1f,%.1f", comma(i), x, y))
		lastX, lastY = x, y
	}
	// Area: close the line down to the baseline.
	area.WriteString(fmt.Sprintf("M%.1f,%.1f", padL, padT+plotH))
	area.WriteString(line.String())
	area.WriteString(fmt.Sprintf("L%.1f,%.1fZ", lastX, padT+plotH))

	// Value label + axis labels (first/last timestamps).
	var label string
	if mode == "cost" {
		label = fmt.Sprintf("$%.3f", values[len(values)-1])
	} else {
		label = fmt.Sprintf("%ds", int(values[len(values)-1]))
	}
	color := "var(--live)"
	if mode == "cost" {
		color = "var(--signal)"
	}
	fillOpacity := "0.10"
	if mode == "cost" {
		fillOpacity = "0.08"
	}

	var xLabels strings.Builder
	xLabels.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="ax-label" text-anchor="start">%s</text>`, padL, h-4, esc(pts[0].Label)))
	xLabels.WriteString(fmt.Sprintf(`<text x="%.1f" y="%.1f" class="ax-label" text-anchor="end">%s</text>`, w-padR, h-4, esc(pts[len(pts)-1].Label)))

	grid := ""
	// A light top gridline at the max for a reference.
	grid = fmt.Sprintf(`<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" class="ax-grid"/>`, padL, padT, padL+plotW, padT)

	return template.HTML(fmt.Sprintf(
		`<svg class="chart" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s over time">
%s
<path d="%s" fill="%s" fill-opacity="%s"/>
<path d="%s" fill="none" stroke="%s" stroke-width="2" stroke-linejoin="round" stroke-linecap="round"/>
<circle cx="%.1f" cy="%.1f" r="3" fill="%s"/>
<text x="%.1f" y="%.1f" class="chart-val" text-anchor="end" fill="%s">%s</text>
%s
</svg>`,
		w, h, esc(modeTitle(mode)),
		grid, area.String(), color, fillOpacity,
		line.String(), color,
		lastX, lastY, color,
		w-padR+4, lastY-8, color, esc(label),
		xLabels.String(),
	))
}

func modeTitle(mode string) string {
	if mode == "cost" {
		return "cost"
	}
	return "speed"
}

func comma(i int) string {
	if i == 0 {
		return ""
	}
	return " "
}

func esc(s string) string {
	if s == "" {
		return "—"
	}
	return template.HTMLEscapeString(s)
}
