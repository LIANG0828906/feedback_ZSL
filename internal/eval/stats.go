package eval

import "math"

type pairStats struct {
	n   int64
	sx  int64
	sy  int64
	sxx int64
	syy int64
	sxy int64
}

func (s *pairStats) add(x, y int64) {
	s.n++
	s.sx += x
	s.sy += y
	s.sxx += x * x
	s.syy += y * y
	s.sxy += x * y
}

type residualStats struct {
	n   int64
	sum int64
	sq  int64
}

func (s *residualStats) add(e int64) {
	s.n++
	s.sum += e
	s.sq += e * e
}

func fixedStd(s residualStats, scale int64) int64 {
	if s.n == 0 {
		return 0
	}
	varianceNumerator := float64(s.n*s.sq - s.sum*s.sum)
	variance := varianceNumerator / (float64(s.n) * float64(s.n))
	if variance < 0 && variance > -1e-6 {
		variance = 0
	}
	if variance < 0 {
		variance = 0
	}
	return int64(math.Round(math.Sqrt(variance) * float64(scale)))
}

func fixedDBFromEnergy(signalEnergy, errorEnergy int64, scale int64) (int64, bool) {
	if errorEnergy == 0 {
		return 0, true
	}
	db := 10.0 * math.Log10(float64(signalEnergy)/float64(errorEnergy))
	return roundFixed(db, scale), false
}

func fixedPSNR(peak int64, count int, errorEnergy int64, scale int64) (int64, bool) {
	if errorEnergy == 0 {
		return 0, true
	}
	mse := float64(errorEnergy) / float64(count)
	db := 10.0 * math.Log10(float64(peak*peak)/mse)
	return roundFixed(db, scale), false
}

func roundFixed(v float64, scale int64) int64 {
	if math.IsInf(v, 1) || v > 180 {
		return 180 * scale
	}
	if math.IsInf(v, -1) || v < -180 {
		return -180 * scale
	}
	if math.IsNaN(v) {
		return 0
	}
	return int64(math.Round(v * float64(scale)))
}

func fixedSSIM(s pairStats, peak int64, scale int64) int64 {
	if s.n == 0 {
		return 0
	}
	n := float64(s.n)
	mx := float64(s.sx) / n
	my := float64(s.sy) / n
	vxNum := float64(s.n*s.sxx - s.sx*s.sx)
	vyNum := float64(s.n*s.syy - s.sy*s.sy)
	covNum := float64(s.n*s.sxy - s.sx*s.sy)
	vx := vxNum / (n * n)
	vy := vyNum / (n * n)
	cov := covNum / (n * n)
	if vx < 0 {
		vx = 0
	}
	if vy < 0 {
		vy = 0
	}
	l := float64(peak)
	c1 := 0.01 * 0.01 * l * l
	c2 := 0.03 * 0.03 * l * l
	num := (2*mx*my + c1) * (2*cov + c2)
	den := (mx*mx + my*my + c1) * (vx + vy + c2)
	if den == 0 {
		return scale
	}
	v := num / den
	if v > 1 {
		v = 1
	}
	if v < -1 {
		v = -1
	}
	return int64(math.Round(v * float64(scale)))
}
