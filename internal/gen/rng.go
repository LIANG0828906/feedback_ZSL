package gen

// rng is a deterministic 64-bit linear congruential generator with fixed
// constants, so every run on every machine produces identical samples. It
// deliberately avoids math/rand to keep output stable across Go versions and
// seed policies.
type rng struct {
	state uint64
}

const (
	lcgMul  uint64 = 6364136223846793005
	lcgInc  uint64 = 1442695040888963407
	lcgMask uint64 = 1<<63 - 1
)

func newRNG(seed uint64) *rng { return &rng{state: seed} }

// next returns the next pseudo-random uint64.
func (r *rng) next() uint64 {
	r.state = (r.state*lcgMul + lcgInc) & lcgMask
	return r.state
}

// intn returns a deterministic value in [0, n).
func (r *rng) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

// pick chooses one element of xs.
func (r *rng) pick(xs []string) string {
	return xs[r.intn(len(xs))]
}

// chance returns true with probability pct/100.
func (r *rng) chance(pct int) bool { return r.intn(100) < pct }
