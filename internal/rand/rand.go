// Package rand provides a fully deterministic, platform independent PRNG.
// No math/rand global source is used, so output is identical on every OS and
// run and does not depend on Go version seeding behavior.
package rand

// RNG is a SplitMix64 based deterministic stream.
type RNG struct {
	state uint64
}

// New creates a stream from a seed.
func New(seed uint64) *RNG { return &RNG{state: seed} }

// Uint64 returns the next 64 bit value.
func (r *RNG) Uint64() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Float64 returns a value in [0,1).
func (r *RNG) Float64() float64 {
	return float64(r.Uint64()>>11) / float64(uint64(1)<<53)
}

// Intn returns a value in [0,n); n must be positive.
func (r *RNG) Intn(n int) int {
	if n <= 0 {
		panic("Intn with non-positive bound")
	}
	return int(r.Uint64() % uint64(n))
}

// mix64 is the finalizer used to derive independent sub-streams.
func mix64(z uint64) uint64 {
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// Derive produces a child stream from a parent seed and a tag.
func Derive(parent uint64, tag uint64) *RNG {
	return New(mix64(parent ^ mix64(tag+0x9e3779b97f4a7c15)))
}

// Quantize rounds v to the nearest multiple of step using deterministic
// round-half-up semantics.
func Quantize(v, step float64) float64 {
	return float64(int64((v/step)+0.5)) * step
}
