/*
 * Copyright (c) 2026 Purushottam <purushottam@passiveintent.dev>
 * SPDX-License-Identifier: BUSL-1.1
 *
 * This source code is licensed under the BUSL-1.1 license found in the
 * LICENSE file in the root directory of this source tree.
 */

package main

import (
	"fmt"
	"math"
	"strings"
)

// renderDeadzoneSVG draws the dead-zone sweep as a self-contained SVG
// heatmap: columns are background-noise levels, rows are anomaly
// magnitudes (largest on top), cell fill is a single-hue sequential ramp
// over the recovery rate (the fraction of trials that produced an emitted
// deviation event), with the number printed in every cell. Overlay marks
// carry the two "why not" states so meaning never rides on color alone:
// a triangle where the storm breaker trips (the window ships a FALLBACK
// frame instead), a hollow square where FISTA found the anomaly but the
// max-residual gate suppressed it, and a dashed outline around the dead
// zone itself (neither path detects the anomaly in ≥50% of trials).
//
// The palette is the pre-validated reference set (single blue sequential
// ramp; the reserved critical red only marks the storm state, paired with
// its triangle glyph). The SVG carries its own light surface so it renders
// identically on light or dark pages.
func renderDeadzoneSVG(cfg deadzoneConfig, cells [][]*deadzoneCell) string {
	const (
		cellW, cellH = 72.0, 34.0
		gap          = 2.0
		marginL      = 92.0
		marginT      = 100.0
		marginR      = 20.0
	)
	cols := len(cells)
	rows := len(cells[0])
	gridW := float64(cols)*(cellW+gap) - gap
	gridH := float64(rows)*(cellH+gap) - gap
	gridBottom := marginT + gridH
	legendY := gridBottom + 64.0
	totalW := marginL + gridW + marginR
	totalH := legendY + 22.0 + 26.0

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" role="img" aria-label="Heatmap of FISTA recovery probability by anomaly magnitude and background noise">`+"\n", totalW, totalH, totalW, totalH)

	// Colorbar gradient from the same ramp the cells use.
	b.WriteString(`<defs><linearGradient id="dzramp" x1="0" y1="0" x2="1" y2="0">`)
	for i, hex := range deadzoneRamp {
		fmt.Fprintf(&b, `<stop offset="%.4f" stop-color="%s"/>`, float64(i)/float64(len(deadzoneRamp)-1), hex)
	}
	b.WriteString(`</linearGradient></defs>` + "\n")

	// Own surface: the figure must read the same on light and dark pages.
	fmt.Fprintf(&b, `<rect width="%.0f" height="%.0f" rx="8" fill="#fcfcfb"/>`+"\n", totalW, totalH)

	b.WriteString(`<g font-family="system-ui, -apple-system, 'Segoe UI', sans-serif">` + "\n")

	// Title + parameter subtitle (two lines: fleet, then detector knobs).
	fmt.Fprintf(&b, `<text x="%.0f" y="30" font-size="16" font-weight="600" fill="#0b0b0b">Layer-2 dead zone: FISTA recovery vs. storm fallback</text>`+"\n", marginL)
	fmt.Fprintf(&b, `<text x="%.0f" y="50" font-size="11.5" fill="#52514e">m=%d · d=%d · bits=%d · %d background series + 1 target · %d trials per cell</text>`+"\n",
		marginL, cfg.M, cfg.D, cfg.Bits, cfg.Background, cfg.Trials)
	fmt.Fprintf(&b, `<text x="%.0f" y="66" font-size="11.5" fill="#52514e">storm %g× median over %d windows · FISTA λ=%g, support threshold %g · residual gate %g</text>`+"\n",
		marginL, cfg.StormMultiplier, cfg.StormWindow, cfg.FISTALambda, cfg.FISTAThreshold, cfg.MaxResidual)

	// Axis titles.
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="12" fill="#52514e" text-anchor="middle">per-series background noise σ (residual units, per window)</text>`+"\n",
		marginL+gridW/2, gridBottom+44)
	fmt.Fprintf(&b, `<text x="24" y="%.1f" font-size="12" fill="#52514e" text-anchor="middle" transform="rotate(-90 24 %.1f)">injected anomaly magnitude ε (residual units)</text>`+"\n",
		marginT+gridH/2, marginT+gridH/2)

	// Cells. cells[ni][mi] has magnitudes ascending; draw largest on top.
	for ni := 0; ni < cols; ni++ {
		x := marginL + float64(ni)*(cellW+gap)
		for mi := 0; mi < rows; mi++ {
			c := cells[ni][mi]
			y := marginT + float64(rows-1-mi)*(cellH+gap)
			rate := c.RecoveryRate()
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.0f" height="%.0f" rx="3" fill="%s"/>`+"\n", x, y, cellW, cellH, rampColor(rate))

			ink := "#52514e"
			if rate >= 0.55 {
				ink = "#ffffff"
			}
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="11" fill="%s" text-anchor="middle" style="font-variant-numeric: tabular-nums">%d%%</text>`+"\n",
				x+cellW/2, y+cellH/2+4, ink, int(math.Round(rate*100)))

			if c.StormRate() >= 0.5 {
				fmt.Fprintf(&b, `<path d="M %.1f %.1f L %.1f %.1f L %.1f %.1f Z" fill="#d03b3b" stroke="#fcfcfb" stroke-width="1"/>`+"\n",
					x+cellW-15, y+11.5, x+cellW-5, y+11.5, x+cellW-10, y+3.5)
			}
			if c.GatedRate() >= 0.5 && c.SupportRate() >= 0.5 {
				fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="6" height="6" fill="none" stroke="%s" stroke-width="1.4"/>`+"\n", x+5, y+5, ink)
			}
			if c.DetectionRate() < 0.5 {
				fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" fill="none" stroke="#0b0b0b" stroke-width="1.5" stroke-dasharray="4 3"/>`+"\n",
					x+1.5, y+1.5, cellW-3, cellH-3)
			}
		}
	}

	// Tick labels: noise per column (below), magnitude per row (left).
	for ni := 0; ni < cols; ni++ {
		x := marginL + float64(ni)*(cellW+gap) + cellW/2
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="11" fill="#898781" text-anchor="middle" style="font-variant-numeric: tabular-nums">%s</text>`+"\n",
			x, gridBottom+18, trimFloat(cells[ni][0].Noise))
	}
	for mi := 0; mi < rows; mi++ {
		y := marginT + float64(rows-1-mi)*(cellH+gap) + cellH/2 + 4
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="11" fill="#898781" text-anchor="end" style="font-variant-numeric: tabular-nums">%s</text>`+"\n",
			marginL-10, y, trimFloat(cells[0][mi].Magnitude))
	}

	// Legend, two rows.
	fmt.Fprintf(&b, `<text x="%.0f" y="%.1f" font-size="11" fill="#898781" text-anchor="end" style="font-variant-numeric: tabular-nums">0%%</text>`+"\n", marginL+22.0, legendY+9)
	fmt.Fprintf(&b, `<rect x="%.0f" y="%.1f" width="140" height="10" rx="2" fill="url(#dzramp)"/>`+"\n", marginL+28.0, legendY)
	fmt.Fprintf(&b, `<text x="%.0f" y="%.1f" font-size="11" fill="#898781" style="font-variant-numeric: tabular-nums">100%%</text>`+"\n", marginL+174.0, legendY+9)
	fmt.Fprintf(&b, `<text x="%.0f" y="%.1f" font-size="11" fill="#52514e">trials where a deviation event actually emitted</text>`+"\n", marginL+210.0, legendY+9)

	row2 := legendY + 22.0
	fmt.Fprintf(&b, `<path d="M %.1f %.1f L %.1f %.1f L %.1f %.1f Z" fill="#d03b3b" stroke="#fcfcfb" stroke-width="1"/>`+"\n",
		marginL, row2+8.5, marginL+10.0, row2+8.5, marginL+5.0, row2+0.5)
	fmt.Fprintf(&b, `<text x="%.0f" y="%.1f" font-size="11" fill="#52514e">storm trips → FALLBACK frame</text>`+"\n", marginL+16.0, row2+9)
	fmt.Fprintf(&b, `<rect x="%.0f" y="%.1f" width="6" height="6" fill="none" stroke="#52514e" stroke-width="1.4"/>`+"\n", marginL+200.0, row2+2)
	fmt.Fprintf(&b, `<text x="%.0f" y="%.1f" font-size="11" fill="#52514e">FISTA found it, gate suppressed it</text>`+"\n", marginL+212.0, row2+9)
	fmt.Fprintf(&b, `<rect x="%.0f" y="%.1f" width="12" height="9" rx="2" fill="none" stroke="#0b0b0b" stroke-width="1.5" stroke-dasharray="4 3"/>`+"\n", marginL+416.0, row2)
	fmt.Fprintf(&b, `<text x="%.0f" y="%.1f" font-size="11" fill="#52514e">dead zone (undetected)</text>`+"\n", marginL+434.0, row2+9)

	b.WriteString("</g>\n</svg>\n")
	return b.String()
}

// deadzoneRamp is the reference sequential blue ramp (steps 100→700),
// lightest = 0% recovery (recedes toward the surface), darkest = 100%.
// Single-hue by construction, so it stays ordered under every common CVD.
var deadzoneRamp = []string{
	"#cde2fb", "#b7d3f6", "#9ec5f4", "#86b6ef", "#6da7ec", "#5598e7",
	"#3987e5", "#2a78d6", "#256abf", "#1c5cab", "#184f95", "#104281", "#0d366b",
}

// rampColor linearly interpolates deadzoneRamp at t ∈ [0, 1].
func rampColor(t float64) string {
	if t <= 0 {
		return deadzoneRamp[0]
	}
	if t >= 1 {
		return deadzoneRamp[len(deadzoneRamp)-1]
	}
	f := t * float64(len(deadzoneRamp)-1)
	i := int(f)
	frac := f - float64(i)
	r0, g0, b0 := hexRGB(deadzoneRamp[i])
	r1, g1, b1 := hexRGB(deadzoneRamp[i+1])
	lerp := func(a, b int) int { return int(math.Round(float64(a) + frac*float64(b-a))) }
	return fmt.Sprintf("#%02x%02x%02x", lerp(r0, r1), lerp(g0, g1), lerp(b0, b1))
}

func hexRGB(hex string) (r, g, b int) {
	fmt.Sscanf(hex, "#%02x%02x%02x", &r, &g, &b)
	return
}
