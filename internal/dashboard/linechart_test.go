package dashboard

import (
	"strings"
	"testing"
)

func TestRenderLineChart(t *testing.T) {
	// Empty / single point → empty output.
	if got := string(renderLineChart(nil, "speed")); got != "" {
		t.Errorf("nil -> expected '', got %q", got)
	}
	if got := string(renderLineChart([]SpeedCostPoint{{Label: "a", Duration: 10}}, "speed")); got != "" {
		t.Errorf("single point -> expected '', got %q", got)
	}

	pts := []SpeedCostPoint{
		{Label: "14:00", Duration: 1800, Cost: 0.032},
		{Label: "15:00", Duration: 900, Cost: 0.032},
		{Label: "16:00", Duration: 600, Cost: 0.064},
	}
	svg := string(renderLineChart(pts, "speed"))
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "polyline points") {
		// Note: we emit <path d=...>, not polyline; just check for path + svg.
		if !strings.Contains(svg, `<path d="`) {
			t.Fatalf("speed chart missing line path: %q", svg)
		}
	}
	if !strings.Contains(svg, "role=\"img\"") {
		t.Errorf("chart missing role=img for a11y")
	}
	if !strings.Contains(svg, "aria-label=\"speed over time\"") {
		t.Errorf("chart missing aria-label: %q", svg)
	}
	// Last value label should appear.
	if !strings.Contains(svg, "600s") {
		t.Errorf("speed chart missing last-value label '600s': %q", svg)
	}
	// Axis timestamps.
	if !strings.Contains(svg, "14:00") || !strings.Contains(svg, "16:00") {
		t.Errorf("chart missing x-axis labels: %q", svg)
	}

	costSvg := string(renderLineChart(pts, "cost"))
	if !strings.Contains(costSvg, "aria-label=\"cost over time\"") {
		t.Errorf("cost chart missing aria-label: %q", costSvg)
	}
	if !strings.Contains(costSvg, "$0.064") {
		t.Errorf("cost chart missing last-value label '$0.064': %q", costSvg)
	}
}
