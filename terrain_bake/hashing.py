"""Self-contained deterministic 64-bit integer hashing (SplitMix64)."""

MASK64 = (1 << 64) - 1


def mix64(z):
    """SplitMix64 finalizer: a bijective 64-bit integer mixer."""
    z = (z + 0x9E3779B97F4A7C15) & MASK64
    z = ((z ^ (z >> 30)) * 0xBF58476D1CE4E5B9) & MASK64
    z = ((z ^ (z >> 27)) * 0x94D049BB133111EB) & MASK64
    return z ^ (z >> 31)


def lattice_hash(seed, ix, iy):
    """Hash a 2D integer lattice coordinate with a seed -> uint64."""
    z = mix64((seed & MASK64) ^ ((ix & MASK64) * 0x9E3779B97F4A7C15 & MASK64))
    z = mix64(z ^ ((iy & MASK64) * 0xBF58476D1CE4E5B9 & MASK64))
    return z


def hash_to_unit(h):
    """Map a uint64 hash deterministically to a float in [0, 1)."""
    return (h >> 11) * (1.0 / (1 << 53))
