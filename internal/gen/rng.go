package gen

// RNG is a deterministic splitmix64 stream. It has no global state and no
// platform dependency: the same seed always produces the same sequence on
// every architecture, which makes every generated sample reproducible.
type RNG struct {
	state uint64
}

// NewRNG seeds a stream.
func NewRNG(seed uint64) *RNG { return &RNG{state: seed} }

// Uint64 returns the next pseudo-random uint64.
func (r *RNG) Uint64() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Intn returns a uniform value in [0, n). n must be positive.
func (r *RNG) Intn(n int64) int64 {
	if n <= 0 {
		panic("gen: Intn bound must be positive")
	}
	return int64(r.Uint64() % uint64(n))
}

// Range returns a uniform value in [lo, hi].
func (r *RNG) Range(lo, hi int64) int64 {
	if hi < lo {
		panic("gen: inverted range")
	}
	return lo + r.Intn(hi-lo+1)
}

// Pick chooses one string from the list deterministically.
func (r *RNG) Pick(options []string) string {
	return options[r.Intn(int64(len(options)))]
}
