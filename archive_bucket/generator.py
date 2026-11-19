"""Deterministic archive workload generation."""

from __future__ import annotations

import hashlib
from dataclasses import dataclass
from typing import Iterable

MiB = 1024 * 1024


@dataclass(frozen=True)
class PlannedObject:
    key: str
    size: int
    kind: str


@dataclass(frozen=True)
class WorkloadPlan:
    seed: int
    objects: tuple[PlannedObject, ...]
    total_bytes: int

    @property
    def object_count(self) -> int:
        return len(self.objects)


def deterministic_payload(seed: int, key: str, revision: int, size: int) -> bytes:
    """Build reproducible content without relying on process-randomized state."""
    if size < 0:
        raise ValueError("size must not be negative")
    material = f"archive-bucket-v1|{seed}|{key}|{revision}|{size}".encode("utf-8")
    return hashlib.shake_256(material).digest(size)


def _small_sizes(seed: int, count: int) -> list[int]:
    # SplitMix64 gives stable integer sizes independent of Python's random module.
    state = (seed * 0x9E3779B97F4A7C15 + 0xD1B54A32D192ED03) & ((1 << 64) - 1)

    def next_u64() -> int:
        nonlocal state
        state = (state + 0x9E3779B97F4A7C15) & ((1 << 64) - 1)
        value = state
        value = (value ^ (value >> 30)) * 0xBF58476D1CE4E5B9 & ((1 << 64) - 1)
        value = (value ^ (value >> 27)) * 0x94D049BB133111EB & ((1 << 64) - 1)
        return value ^ (value >> 31)

    return [4096 + next_u64() % (24576 - 4096 + 1) for _ in range(count)]


def build_plan(
    seed: int = 20261119,
    count: int = 200,
    target_bytes: int = 120 * MiB,
    prefix: str = "archive",
) -> WorkloadPlan:
    if count < 3:
        raise ValueError("count must be >= 3")
    if target_bytes < count * 4096:
        raise ValueError("target_bytes is too small for the requested object count")

    large_count = max(1, round(count * 0.01))
    medium_count = max(1, round(count * 0.04))
    if large_count + medium_count >= count:
        large_count = 1
        medium_count = 1
    small_count = count - large_count - medium_count

    small_sizes = _small_sizes(seed, small_count)
    # 3-7 MiB is the "tens of MB" middle tier; the final tier carries the bulk.
    medium_total = min(max(medium_count * 4 * MiB, target_bytes // 3), 48 * MiB)
    base = max(1 * MiB, medium_total // medium_count)
    medium_sizes = [base] * medium_count
    spend = sum(medium_sizes)
    for index in range(medium_count):
        if spend >= medium_total:
            break
        medium_sizes[index] += 1
        spend += 1
    medium_sizes[-1] += max(0, medium_total - sum(medium_sizes))

    remaining = target_bytes - sum(small_sizes) - sum(medium_sizes)
    minimum_large = 8 * MiB
    if remaining < large_count * minimum_large:
        raise ValueError("target_bytes cannot accommodate all object tiers")
    large_sizes = [remaining // large_count] * large_count
    large_sizes[-1] += remaining - sum(large_sizes)

    objects: list[PlannedObject] = []
    for index, size in enumerate(small_sizes):
        objects.append(PlannedObject(f"{prefix}/{index:04d}.bin", size, "small"))
    offset = len(objects)
    for index, size in enumerate(medium_sizes):
        objects.append(PlannedObject(f"{prefix}/{offset + index:04d}.bin", size, "medium"))
    offset = len(objects)
    for index, size in enumerate(large_sizes):
        objects.append(PlannedObject(f"{prefix}/{offset + index:04d}.bin", size, "large"))

    return WorkloadPlan(seed, tuple(objects), target_bytes)


def revision_size(planned_size: int, revision: int) -> int:
    if revision <= 1:
        return planned_size
    # Revisions remain deterministic while exercising both larger and smaller
    # historical versions.
    if revision % 3 == 0:
        return max(1024, planned_size // 2)
    if revision % 3 == 1:
        return max(1024, planned_size + min(planned_size // 5, 2 * MiB))
    return max(1024, planned_size - min(planned_size // 4, MiB))


def payload_for(seed: int, item: PlannedObject, revision: int) -> bytes:
    return deterministic_payload(seed, item.key, revision, revision_size(item.size, revision))


def iter_plan(plan: WorkloadPlan) -> Iterable[PlannedObject]:
    return iter(plan.objects)

