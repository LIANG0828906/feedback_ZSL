package sample

// rng is a deterministic 64-bit linear congruential generator. Every process
// that starts from the same seed observes the exact same byte stream, which is
// what makes sample generation reproducible across machines and runs.
type rng struct {
	state uint64
}

const (
	lcgMul = 6364136223846793005
	lcgInc = 1442695040888963407
)

func newRNG(seed int64) *rng {
	return &rng{state: uint64(seed)}
}

// next returns a pseudo-random uint64.
func (r *rng) next() uint64 {
	r.state = r.state*lcgMul + lcgInc
	return r.state
}

// intn returns a deterministic value in [0,n).
func (r *rng) intn(n int) int {
	if n <= 0 {
		panic("rng.intn: non-positive bound")
	}
	return int(r.next() % uint64(n))
}

// byteRange returns a value in [lo,hi].
func (r *rng) byteRange(lo, hi int) int {
	if hi < lo {
		hi, lo = lo, hi
	}
	return lo + r.intn(hi-lo+1)
}
