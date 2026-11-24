package core

import (
	"math"
	"sort"
)

func AnalyzeGeometry(mask Image, truth Truth) Geometry {
	stats := Geometry{Coverage: coverage(mask.Pix, mask.PixelCount())}
	search := math.Max(5, float64(min(mask.Width, mask.Height))*0.012)
	for _, line := range truth.Lines {
		obs := LineObservation{
			Channel: line.Channel,
			TruthX1: line.X1, TruthY1: line.Y1,
			TruthX2: line.X2, TruthY2: line.Y2,
		}
		dx, dy := line.X2-line.X1, line.Y2-line.Y1
		len2 := dx*dx + dy*dy
		if len2 > 0 {
			minX := max(0, int(math.Floor(math.Min(line.X1, line.X2)-search)))
			maxX := min(mask.Width-1, int(math.Ceil(math.Max(line.X1, line.X2)+search)))
			minY := max(0, int(math.Floor(math.Min(line.Y1, line.Y2)-search)))
			maxY := min(mask.Height-1, int(math.Ceil(math.Max(line.Y1, line.Y2)+search)))
			bestStart, bestEnd := 1.0, 0.0
			found := false
			support := 0
			for y := minY; y <= maxY; y++ {
				for x := minX; x <= maxX; x++ {
					if mask.At(x, y, line.Channel) == 0 {
						continue
					}
					px, py := float64(x)+0.5, float64(y)+0.5
					t := ((px-line.X1)*dx + (py-line.Y1)*dy) / len2
					if t < -0.08 || t > 1.08 {
						continue
					}
					qx, qy := line.X1+t*dx, line.Y1+t*dy
					if math.Hypot(px-qx, py-qy) > search {
						continue
					}
					if !found {
						bestStart, bestEnd = t, t
						found = true
					} else {
						bestStart = math.Min(bestStart, t)
						bestEnd = math.Max(bestEnd, t)
					}
					support++
				}
			}
			minSupport := max(6, int(math.Ceil(math.Sqrt(len2)*0.08)))
			if found && support >= minSupport && bestEnd-bestStart >= 0.10 {
				obs.X1 = line.X1 + bestStart*dx
				obs.Y1 = line.Y1 + bestStart*dy
				obs.X2 = line.X1 + bestEnd*dx
				obs.Y2 = line.Y1 + bestEnd*dy
			} else if found {
				obs.X1, obs.Y1, obs.X2, obs.Y2 = 0, 0, 0, 0
			}
		}
		obs.EndpointErr = math.Max(math.Hypot(obs.X1-obs.TruthX1, obs.Y1-obs.TruthY1), math.Hypot(obs.X2-obs.TruthX2, obs.Y2-obs.TruthY2))
		if obs.X1 == 0 && obs.X2 == 0 && obs.Y1 == 0 && obs.Y2 == 0 {
			obs.EndpointErr = math.Hypot(float64(mask.Width), float64(mask.Height))
		}
		stats.Lines = append(stats.Lines, obs)
	}
	for _, circ := range truth.Circles {
		obs := CircleObservation{
			Channel: circ.Channel,
			TruthCX: circ.CX, TruthCY: circ.CY, TruthDiameter: circ.Diameter,
		}
		radius := circ.Diameter / 2
		minX := max(0, int(math.Floor(circ.CX-radius-search)))
		maxX := min(mask.Width-1, int(math.Ceil(circ.CX+radius+search)))
		minY := max(0, int(math.Floor(circ.CY-radius-search)))
		maxY := min(mask.Height-1, int(math.Ceil(circ.CY+radius+search)))
		var sx, sy float64
		var samples []float64
		for y := minY; y <= maxY; y++ {
			for x := minX; x <= maxX; x++ {
				if mask.At(x, y, circ.Channel) == 0 {
					continue
				}
				px, py := float64(x)+0.5, float64(y)+0.5
				d := math.Hypot(px-circ.CX, py-circ.CY)
				if math.Abs(d-radius) <= search {
					sx += px
					sy += py
					samples = append(samples, d)
				}
			}
		}
		if len(samples) > 0 {
			minSupport := max(8, int(math.Ceil(2*math.Pi*radius*0.08)))
			if len(samples) < minSupport {
				samples = nil
			}
		}
		if len(samples) > 0 {
			obs.CX = sx / float64(len(samples))
			obs.CY = sy / float64(len(samples))
			sum := 0.0
			for y := minY; y <= maxY; y++ {
				for x := minX; x <= maxX; x++ {
					if mask.At(x, y, circ.Channel) == 0 {
						continue
					}
					px, py := float64(x)+0.5, float64(y)+0.5
					d := math.Hypot(px-obs.CX, py-obs.CY)
					if math.Abs(d-radius) <= search {
						sum += d
					}
				}
			}
			obs.Diameter = 2 * sum / float64(len(samples))
		}
		obs.CenterErr = math.Hypot(obs.CX-circ.CX, obs.CY-circ.CY)
		obs.DiameterErr = math.Abs(obs.Diameter - circ.Diameter)
		if len(samples) == 0 {
			obs.CenterErr = math.Hypot(float64(mask.Width), float64(mask.Height))
			obs.DiameterErr = circ.Diameter
		}
		stats.Circles = append(stats.Circles, obs)
	}
	return stats
}

func GeometryDeviation(a, b Geometry) float64 {
	worst := 0.0
	if len(a.Lines) != len(b.Lines) || len(a.Circles) != len(b.Circles) {
		return math.Inf(1)
	}
	for i := range a.Lines {
		l, r := a.Lines[i], b.Lines[i]
		worst = math.Max(worst, math.Hypot(l.X1-r.X1, l.Y1-r.Y1))
		worst = math.Max(worst, math.Hypot(l.X2-r.X2, l.Y2-r.Y2))
	}
	for i := range a.Circles {
		c, d := a.Circles[i], b.Circles[i]
		worst = math.Max(worst, math.Hypot(c.CX-d.CX, c.CY-d.CY))
		worst = math.Max(worst, math.Abs(c.Diameter-d.Diameter))
	}
	return worst
}

func GeometryTrueDeviation(stats Geometry) float64 {
	worst := 0.0
	for _, line := range stats.Lines {
		worst = math.Max(worst, line.EndpointErr)
	}
	for _, circ := range stats.Circles {
		worst = math.Max(worst, circ.CenterErr)
		worst = math.Max(worst, circ.DiameterErr)
	}
	return worst
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
