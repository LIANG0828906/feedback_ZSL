package generator

type splitMix struct {
	state uint64
}

func newSplitMix(seed uint64) *splitMix {
	return &splitMix{state: seed}
}

func (r *splitMix) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *splitMix) Float64() float64 {
	return float64(r.next()>>11) / float64(1<<53)
}

func (r *splitMix) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

func (r *splitMix) Int63n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(r.next() % uint64(n))
}

func (r *splitMix) rangeFloat(min, max float64) float64 {
	return min + (max-min)*r.Float64()
}
