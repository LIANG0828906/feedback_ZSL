"""Atomic snapshot persistence and strict hash-chain recovery."""

from __future__ import annotations

import json
import os
import re
import tempfile
from pathlib import Path

import numpy as np

from .cache import PathCacheEntry, path_index_dependencies
from .costfield import CostField, compute_full_field, reconstruct_path
from .errors import E_RECOVERY_INVALID, ServiceError
from .grid import BattleGrid
from .hashing import path_hash, sha256_bytes
from .snapshot import PublishedSnapshot, snapshot_payload

FILE_FORMAT = "tactical-snapshot-file-v1"
FILE_RE = re.compile(r"^snapshot-(\d{12})\.snap$")


class SnapshotStore:
    def __init__(self, root: str | os.PathLike[str]) -> None:
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)

    def _path_for(self, batch_seq: int) -> Path:
        return self.root / f"snapshot-{int(batch_seq):012d}.snap"

    def save(self, snapshot: PublishedSnapshot) -> Path:
        destination = self._path_for(snapshot.batch_seq)
        fd, temporary_name = tempfile.mkstemp(
            prefix=".snapshot-", suffix=".tmp", dir=self.root
        )
        temporary = Path(temporary_name)
        try:
            manifest = self._manifest(snapshot)
            arrays: dict[str, np.ndarray] = {
                "manifest": np.frombuffer(manifest, dtype=np.uint8),
                "grid_base": snapshot.grid.base_cost,
                "grid_mask": snapshot.grid.pass_mask,
                "grid_danger": snapshot.grid.danger,
            }
            for unit, field in sorted(snapshot.fields.items()):
                arrays[f"field_{unit}"] = field.distance
            with os.fdopen(fd, "wb") as handle:
                fd = -1
                np.savez(handle, **arrays)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, destination)
            self._fsync_directory()
            return destination
        except Exception:
            if fd >= 0:
                os.close(fd)
            try:
                temporary.unlink()
            except FileNotFoundError:
                pass
            raise

    def _manifest(self, snapshot: PublishedSnapshot) -> bytes:
        cache_records = []
        for key, entry in sorted(snapshot.path_cache.items()):
            cache_records.append(
                {
                    "unit": int(entry.unit_id),
                    "target": [int(entry.target[0]), int(entry.target[1])],
                    "source": [int(entry.source[0]), int(entry.source[1])],
                    "version": int(entry.version),
                    "path": [[int(row), int(col)] for row, col in entry.path],
                    "path_hash": entry.path_hash,
                    "dependencies": sorted(int(value) for value in entry.dependencies),
                    "cost": int(entry.cost),
                    "reachable": bool(entry.reachable),
                }
            )
        value = {
            "format": FILE_FORMAT,
            "version": int(snapshot.version),
            "batch_seq": int(snapshot.batch_seq),
            "previous_hash": snapshot.previous_hash,
            "payload_hash": snapshot.payload_hash,
            "version_hash": snapshot.version_hash,
            "grid_hash": snapshot.grid.content_hash(),
            "grid": {
                "shape": [snapshot.grid.height, snapshot.grid.width],
                "unit_count": int(snapshot.grid.unit_count),
                "danger_penalty": int(snapshot.grid.danger_penalty),
            },
            "field_units": [
                {
                    "unit": int(unit),
                    "target": [int(field.target[0]), int(field.target[1])],
                    "field_hash": field.field_hash,
                    "array": f"field_{unit}",
                }
                for unit, field in sorted(snapshot.fields.items())
            ],
            "cache": cache_records,
        }
        return json.dumps(
            value,
            ensure_ascii=True,
            allow_nan=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("utf-8")

    def recover_latest(self) -> PublishedSnapshot:
        entries: list[tuple[int, Path]] = []
        for path in self.root.iterdir():
            matched = FILE_RE.match(path.name)
            if matched and path.is_file():
                entries.append((int(matched.group(1)), path))
        entries.sort()
        if not entries:
            raise ServiceError(
                E_RECOVERY_INVALID, f"no published snapshot in {self.root}"
            )

        previous: PublishedSnapshot | None = None
        for sequence, path in entries:
            snapshot = self._load_one(path)
            if snapshot.batch_seq != sequence:
                raise ServiceError(
                    E_RECOVERY_INVALID,
                    f"filename sequence {sequence} != payload {snapshot.batch_seq}",
                )
            if previous is None:
                if snapshot.batch_seq != 0 or snapshot.version != 0:
                    raise ServiceError(
                        E_RECOVERY_INVALID,
                        "snapshot chain does not start at version 0",
                    )
            else:
                if snapshot.batch_seq != previous.batch_seq + 1:
                    raise ServiceError(
                        E_RECOVERY_INVALID,
                        f"missing snapshot batch after {previous.batch_seq}",
                    )
                if snapshot.version != previous.version + 1:
                    raise ServiceError(
                        E_RECOVERY_INVALID,
                        f"missing snapshot version after {previous.version}",
                    )
                if snapshot.previous_hash != previous.version_hash:
                    raise ServiceError(
                        E_RECOVERY_INVALID,
                        f"broken hash chain at batch {snapshot.batch_seq}",
                    )
            previous = snapshot
        assert previous is not None
        return previous

    def _load_one(self, path: Path) -> PublishedSnapshot:
        try:
            with np.load(path, allow_pickle=False) as archive:
                if "manifest" not in archive:
                    raise ValueError("manifest array is missing")
                manifest = json.loads(bytes(archive["manifest"]).decode("utf-8"))
                if manifest.get("format") != FILE_FORMAT:
                    raise ValueError("snapshot file format mismatch")
                required = {
                    "version",
                    "batch_seq",
                    "previous_hash",
                    "payload_hash",
                    "version_hash",
                    "grid_hash",
                    "grid",
                    "field_units",
                    "cache",
                }
                missing = required.difference(manifest)
                if missing:
                    raise ValueError(f"manifest fields missing: {sorted(missing)}")
                grid_record = manifest["grid"]
                base = np.asarray(archive["grid_base"], dtype=np.int32)
                mask = np.asarray(archive["grid_mask"], dtype=np.uint64)
                danger = np.asarray(archive["grid_danger"], dtype=np.uint8)
                shape = (int(grid_record["shape"][0]), int(grid_record["shape"][1]))
                if base.shape != shape or mask.shape != shape or danger.shape != shape:
                    raise ValueError("grid array shape mismatch")
                grid = BattleGrid(
                    base,
                    mask,
                    danger,
                    int(grid_record["unit_count"]),
                    int(grid_record["danger_penalty"]),
                )
                if grid.content_hash() != manifest["grid_hash"]:
                    raise ValueError("grid content hash mismatch")

                fields: dict[int, CostField] = {}
                for record in manifest["field_units"]:
                    unit = int(record["unit"])
                    target = (int(record["target"][0]), int(record["target"][1]))
                    key = str(record["array"])
                    if key not in archive:
                        raise ValueError(f"field array missing: {key}")
                    distance = np.asarray(archive[key], dtype=np.int64)
                    field = CostField.create(unit, target, distance)
                    if field.field_hash != record["field_hash"]:
                        raise ValueError(f"field hash mismatch for unit {unit}")
                    fields[unit] = field
                expected_units = set(range(grid.unit_count))
                if set(fields) != expected_units:
                    raise ValueError("published field units are incomplete")

                snapshot_payload_value = snapshot_payload(
                    int(manifest["version"]),
                    int(manifest["batch_seq"]),
                    grid,
                    fields,
                )
                from .hashing import canonical_json_bytes

                payload_hash = sha256_bytes(
                    canonical_json_bytes(snapshot_payload_value)
                )
                if payload_hash != manifest["payload_hash"]:
                    raise ValueError("snapshot payload hash mismatch")
                version_hash = sha256_bytes(
                    str(manifest["previous_hash"]).encode("ascii")
                    + b"\0"
                    + payload_hash.encode("ascii")
                )
                if version_hash != manifest["version_hash"]:
                    raise ValueError("snapshot version hash mismatch")

                cache_entries: dict[
                    tuple[int, tuple[int, int], tuple[int, int]], PathCacheEntry
                ] = {}
                width = grid.width
                for record in manifest["cache"]:
                    unit = int(record["unit"])
                    target = (int(record["target"][0]), int(record["target"][1]))
                    source = (int(record["source"][0]), int(record["source"][1]))
                    path = tuple(
                        (int(point[0]), int(point[1])) for point in record["path"]
                    )
                    dependencies = frozenset(
                        int(value) for value in record["dependencies"]
                    )
                    expected_dependencies = path_index_dependencies(
                        path, grid.height, width
                    )
                    if dependencies != expected_dependencies:
                        raise ValueError("cache dependency set mismatch")
                    if path_hash(path) != record["path_hash"]:
                        raise ValueError("cache path hash mismatch")
                    entry = PathCacheEntry.create(
                        unit,
                        target,
                        source,
                        int(record["version"]),
                        path,
                        dependencies,
                        int(record["cost"]),
                        bool(record["reachable"]),
                    )
                    if entry.version != int(manifest["version"]):
                        raise ValueError("cache entry version mismatch")
                    if entry.key in cache_entries:
                        raise ValueError("duplicate cache entry")
                    field = fields[entry.unit_id]
                    weights = grid.cell_cost(entry.unit_id)
                    if field.target == entry.target:
                        cache_field = field
                    else:
                        cache_field = CostField.create(
                            entry.unit_id,
                            entry.target,
                            compute_full_field(weights, entry.target),
                        )
                    expected_path, expected_cost, expected_reachable = reconstruct_path(
                        cache_field, weights, entry.source
                    )
                    if (
                        entry.path != expected_path
                        or entry.cost != expected_cost
                        or entry.reachable != expected_reachable
                    ):
                        raise ValueError("cache path is not the stored shortest path")
                    cache_entries[entry.key] = entry

                return PublishedSnapshot.create(
                    int(manifest["version"]),
                    int(manifest["batch_seq"]),
                    grid,
                    fields,
                    cache_entries,
                    str(manifest["previous_hash"]),
                )
        except ServiceError:
            raise
        except Exception as exc:
            raise ServiceError(
                E_RECOVERY_INVALID, f"invalid snapshot {path.name}: {exc}"
            ) from exc

    def _fsync_directory(self) -> None:
        if os.name == "nt":
            return
        try:
            descriptor = os.open(self.root, os.O_RDONLY)
        except OSError:
            return
        try:
            os.fsync(descriptor)
        except OSError:
            pass
        finally:
            os.close(descriptor)


def load_latest(root: str | os.PathLike[str]) -> PublishedSnapshot:
    return SnapshotStore(root).recover_latest()
