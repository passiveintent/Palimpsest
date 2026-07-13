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

// renderMonthSVG draws the month ledger as a daily bytes-on-wire line
// chart on a log scale: Palimpsest total vs the raw remote-write baseline
// under snappy and zstd, with the three scripted incidents marked. Series
// identity rides fixed categorical hues plus direct end-of-line labels
// (ink text with a color swatch), never color alone; the log axis is the
// only honest way to put lines ~40× apart on one chart, and it is labeled
// as such.
func renderMonthSVG(cfg monthConfig, res *monthResult) string {
	const (
		w, h                      = 780.0, 420.0
		marginL, marginR, marginT = 84.0, 172.0, 84.0
		marginB                   = 56.0
	)
	plotW := w - marginL - marginR
	plotH := h - marginT - marginB

	// Series, in fixed slot order (identity → hue, never re-assigned).
	type series struct {
		name  string
		color string
		vals  []float64
	}
	var days []int
	for d, day := range res.Days {
		if day.BaseRaw > 0 || day.PLTotal > 0 {
			days = append(days, d)
		}
	}
	ss := []series{
		{name: "Palimpsest (total)", color: "#2a78d6"},
		{name: "remote-write, snappy", color: "#1baf7a"},
		{name: "remote-write, zstd", color: "#eda100"},
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, d := range days {
		day := res.Days[d]
		for i, v := range [...]int64{day.PLTotal, day.BaseSnappy, day.BaseZstd} {
			fv := float64(v)
			if fv <= 0 {
				fv = 1
			}
			ss[i].vals = append(ss[i].vals, fv)
			lo = math.Min(lo, fv)
			hi = math.Max(hi, fv)
		}
	}
	if len(days) == 0 {
		return "<svg xmlns=\"http://www.w3.org/2000/svg\"/>"
	}
	logLo := math.Floor(math.Log10(lo))
	logHi := math.Ceil(math.Log10(hi))
	if logHi <= logLo {
		logHi = logLo + 1
	}

	xAt := func(i int) float64 {
		if len(days) == 1 {
			return marginL + plotW/2
		}
		return marginL + plotW*float64(i)/float64(len(days)-1)
	}
	yAt := func(v float64) float64 {
		return marginT + plotH*(1-(math.Log10(v)-logLo)/(logHi-logLo))
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%.0f" height="%.0f" viewBox="0 0 %.0f %.0f" role="img" aria-label="Daily bytes on wire, Palimpsest vs raw remote-write, log scale">`+"\n", w, h, w, h)
	fmt.Fprintf(&b, `<rect width="%.0f" height="%.0f" rx="8" fill="#fcfcfb"/>`+"\n", w, h)
	b.WriteString(`<g font-family="system-ui, -apple-system, 'Segoe UI', sans-serif">` + "\n")

	fmt.Fprintf(&b, `<text x="%.0f" y="30" font-size="16" font-weight="600" fill="#0b0b0b">Bytes on wire per day — %d raw series, %d simulated days</text>`+"\n",
		marginL, res.RawSeries, len(days))
	fmt.Fprintf(&b, `<text x="%.0f" y="50" font-size="11.5" fill="#52514e">flush %s · keyframe every %d windows · %d emitters · three scripted incidents · log scale</text>`+"\n",
		marginL, cfg.Interval, cfg.KeyframeEvery, cfg.Emitters)

	// Decade gridlines + tick labels (decimal decades, so SI units).
	for e := logLo; e <= logHi; e++ {
		y := yAt(math.Pow(10, e))
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#e1e0d9" stroke-width="1"/>`+"\n", marginL, y, marginL+plotW, y)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="11" fill="#898781" text-anchor="end" style="font-variant-numeric: tabular-nums">%s</text>`+"\n",
			marginL-8, y+4, humanBytesSI(math.Pow(10, e)))
	}

	// Incident markers, on a continuous [0, days] scale clamped to the plot.
	for _, inc := range cfg.Incidents {
		frac := float64(inc.StartWin) / float64(cfg.WindowsPerDay) / float64(len(days))
		if frac > 1 {
			frac = 1
		}
		x := marginL + plotW*frac
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#c3c2b7" stroke-width="1" stroke-dasharray="4 3"/>`+"\n", x, marginT-6, x, marginT+plotH)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="10.5" fill="#898781" text-anchor="middle">%s</text>`+"\n", x, marginT-12, inc.Name)
	}

	// Lines + end labels (ink text, color swatch carries identity).
	for _, s := range ss {
		var pts strings.Builder
		for i, v := range s.vals {
			fmt.Fprintf(&pts, "%.1f,%.1f ", xAt(i), yAt(v))
		}
		fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="2" stroke-linejoin="round"/>`+"\n", strings.TrimSpace(pts.String()), s.color)
		yEnd := yAt(s.vals[len(s.vals)-1])
		fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="3"/>`+"\n", marginL+plotW+6, yEnd, marginL+plotW+18, yEnd, s.color)
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="11" fill="#52514e">%s</text>`+"\n", marginL+plotW+24, yEnd+4, s.name)
	}

	// X axis: day ticks.
	step := len(days) / 10
	if step < 1 {
		step = 1
	}
	for i := 0; i < len(days); i += step {
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="11" fill="#898781" text-anchor="middle" style="font-variant-numeric: tabular-nums">%d</text>`+"\n",
			xAt(i), marginT+plotH+18, days[i])
	}
	fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" font-size="12" fill="#52514e" text-anchor="middle">simulated day</text>`+"\n", marginL+plotW/2, marginT+plotH+40)
	fmt.Fprintf(&b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="#c3c2b7" stroke-width="1"/>`+"\n", marginL, marginT+plotH, marginL+plotW, marginT+plotH)

	b.WriteString("</g>\n</svg>\n")
	return b.String()
}

func humanBytesSI(v float64) string {
	switch {
	case v >= 1e12:
		return fmt.Sprintf("%g TB", v/1e12)
	case v >= 1e9:
		return fmt.Sprintf("%g GB", v/1e9)
	case v >= 1e6:
		return fmt.Sprintf("%g MB", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%g KB", v/1e3)
	default:
		return fmt.Sprintf("%g B", v)
	}
}
