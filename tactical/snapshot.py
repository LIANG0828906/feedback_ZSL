"""Immutable published cost-field snapshots and their hash chain."""

from __future__ import annotations

from collections.abc import Mapping
from dataclasses import dataclass
from types import MappingProxyType

from .cache import CacheKey, PathCacheEntry
from .costfield import CostField
from .grid import BattleGrid
from .hashing import ZERO_HASH, canonical_json_bytes, sha256_bytes


@dataclass(frozen=True)
class PublishedSnapshot:
    version: int
    batch_seq: int
    grid: BattleGrid
    fields: Mapping[int, CostField]
    path_cache: Mapping[CacheKey, PathCacheEntry]
    previous_hash: str
    payload_hash: str
    version_hash: str

    @classmethod
    def create(
        cls,
        version: int,
        batch_seq: int,
        grid: BattleGrid,
        fields: Mapping[int, CostField],
        path_cache: Mapping[CacheKey, PathCacheEntry],
        previous_hash: str,
    ) -> PublishedSnapshot:
        stable_fields = MappingProxyType(dict(sorted(fields.items())))
        stable_cache = MappingProxyType(dict(sorted(path_cache.items())))
        payload = snapshot_payload(version, batch_seq, grid, stable_fields)
        payload_hash = sha256_bytes(canonical_json_bytes(payload))
        version_hash = sha256_bytes(
            previous_hash.encode("ascii") + b"\0" + payload_hash.encode("ascii")
        )
        return cls(
            int(version),
            int(batch_seq),
            grid,
            stable_fields,
            stable_cache,
            str(previous_hash),
            payload_hash,
            version_hash,
        )

    @property
    def cache_dependency_count(self) -> int:
        return sum(len(entry.dependencies) for entry in self.path_cache.values())

    def array_nbytes(self) -> int:
        return self.grid.array_nbytes() + sum(
            field.array_nbytes() for field in self.fields.values()
        )

    def field_hashes(self) -> dict[int, str]:
        return {unit: field.field_hash for unit, field in self.fields.items()}


def snapshot_payload(
    version: int,
    batch_seq: int,
    grid: BattleGrid,
    fields: Mapping[int, CostField],
) -> dict[str, object]:
    return {
        "format": "published-snapshot-v1",
        "version": int(version),
        "batch_seq": int(batch_seq),
        "grid_hash": grid.content_hash(),
        "fields": [
            {
                "unit": int(unit),
                "target": [int(field.target[0]), int(field.target[1])],
                "field_hash": field.field_hash,
            }
            for unit, field in sorted(fields.items())
        ],
    }


def initial_chain_hash() -> str:
    return ZERO_HASH
