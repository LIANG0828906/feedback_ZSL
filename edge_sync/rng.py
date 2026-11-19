"""Deterministic pseudo-random helpers.

SplitMix64 is used as a pure function of (seed, *labels) so that every loss
decision, shard size and jitter sample is reproducible across runs and does
not depend on thread interleaving or wall-clock timing.
"""

MASK64 = (1 << 64) - 1


def splitmix64(state: int) -> int:
    """Return the next 64-bit SplitMix64 value and the new state."""
    state = (state + 0x9E3779B97F4A7C15) & MASK64
    value = state
    value = ((value ^ (value >> 30)) * 0xBF58476D1CE4E5B9) & MASK64
    value = ((value ^ (value >> 27)) * 0x94D049BB133111EB) & MASK64
    return value ^ (value >> 31), state


def mix(*labels: int) -> int:
    """Fold arbitrary integer labels into one 64-bit value."""
    value = 0xCBF29CE484222325
    for label in labels:
        value ^= label & MASK64
        value = (value * 0x100000001B3) & MASK64
    return value


def prf(seed: int, *labels: int) -> int:
    """Pure pseudo-random function: same inputs always give the same output."""
    value, _ = splitmix64(mix(seed, *labels))
    return value


def uniform(seed: int, *labels: int) -> float:
    """Return a deterministic float in [0.0, 1.0)."""
    return prf(seed, *labels) / (1 << 64)


class ByteStream:
    """Generates deterministic payload blocks without storing them.

    A 180 MiB workload therefore costs only a few bytes of generator state
    instead of 180 MiB of temporary disk or memory.
    """

    def __init__(self, seed: int, task_id: int, shard_seq: int):
        self._state = mix(seed, task_id, shard_seq)

    def render(self, length: int) -> bytes:
        chunks = []
        remaining = length
        while remaining > 0:
            value, self._state = splitmix64(self._state)
            block = value.to_bytes(8, "little")
            chunks.append(block[: min(8, remaining)])
            remaining -= len(chunks[-1])
        return b"".join(chunks)
