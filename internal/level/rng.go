package level

// Deterministic PRNG (splitmix64). Generation must never read thread
// identity, completion order or wall-clock time; all randomness flows
// from these seeded streams.

func mix64(z uint64) uint64 {
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

type RNG struct {
	state uint64
}

func NewRNG(seed uint64) *RNG {
	return &RNG{state: seed}
}

func (r *RNG) Next() uint64 {
	r.state += 0x9E3779B97F4A7C15
	return mix64(r.state)
}

// Intn returns a value in [0, n). n must be > 0.
func (r *RNG) Intn(n int) int {
	return int(r.Next() % uint64(n))
}

// RangeInt returns a value in [lo, hi] inclusive. Requires lo <= hi.
func (r *RNG) RangeInt(lo, hi int) int {
	if lo >= hi {
		return lo
	}
	return lo + r.Intn(hi-lo+1)
}

// DeriveSubSeed is the declared sub-seed derivation rule: every partition
// seed is a pure function of the global seed, partition grid coordinate,
// the partition's salt parameter and the sub-seed version.
func DeriveSubSeed(globalSeed uint64, row, col int, salt uint64, version uint32) uint64 {
	h := mix64(globalSeed + uint64(version)*0xA24BAED4963EE407)
	h = mix64(h ^ uint64(uint32(row))*0x9FB21C651E98DF25)
	h = mix64(h ^ uint64(uint32(col))*0xC2B2AE3D27D4EB4F)
	h = mix64(h ^ salt)
	return h
}
