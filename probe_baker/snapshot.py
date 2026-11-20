"""Chunked, hash-chained probe snapshots with crash recovery."""

from __future__ import annotations

import json
import math
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

import numpy as np

from .canonical import (
    canonical_json,
    cleanup_temporary_files,
    hash_arrays,
    hash_object,
    read_json,
    sha256_hex,
    write_json_atomic,
)
from .errors import ErrorCode, ProbeError
from .grid import build_probe_grid
from .publish import IrradianceSnapshot
from .scene import parse_scene
from .strategy import BakeConfig


SNAPSHOT_SCHEMA = "light-probe-snapshot-v1"
CHUNK_SCHEMA = "light-probe-chunk-v1"
_REQUIRED_BEGIN = {
    "schema",
    "snapshot_version",
    "total_probes",
    "chunk_size",
    "grid_structure_hash",
    "content_hash",
    "scene",
    "config",
    "directions",
    "occluder_ids",
    "strategy",
    "sh_order",
}
_REQUIRED_CHUNK_META = {
    "schema",
    "snapshot_version",
    "chunk_index",
    "start",
    "count",
    "total_probes",
    "chain_prev",
    "arrays_hash",
    "chain_hash",
    "factor_versions",
    "probe_hashes",
}


def _version_seed(
    version: int,
    grid_hash: str,
    total_probes: int,
    chunk_size: int,
    content_hash: str,
) -> str:
    return hash_object(
        {
            "schema": SNAPSHOT_SCHEMA,
            "snapshot_version": version,
            "grid_structure_hash": grid_hash,
            "total_probes": total_probes,
            "chunk_size": chunk_size,
            "content_hash": content_hash,
        }
    )


def _chunk_arrays(
    raw: np.ndarray,
    coefficients: np.ndarray,
    truncation: np.ndarray,
    visibility: np.ndarray,
    sampled_mask: np.ndarray,
    hits: np.ndarray,
) -> list[tuple[str, np.ndarray]]:
    return [
        ("raw_coefficients", raw),
        ("coefficients", coefficients),
        ("truncation_error", truncation),
        ("visibility_mean", visibility),
        ("sampled_mask", sampled_mask),
        ("occluder_hits", hits),
    ]


def _atomic_npz(path: Path, payload: dict[str, np.ndarray]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.tmp-", dir=path.parent
    )
    os.close(descriptor)
    try:
        with open(temporary_name, "wb") as handle:
            np.savez_compressed(handle, **payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_name, path)
        directory_fd = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except BaseException:
        try:
            os.unlink(temporary_name)
        except FileNotFoundError:
            pass
        raise


def _load_chunk(path: Path) -> dict[str, np.ndarray]:
    try:
        with np.load(path, allow_pickle=False) as archive:
            return {name: np.array(archive[name], copy=True) for name in archive.files}
    except (OSError, ValueError, KeyError) as exc:
        raise ProbeError(
            ErrorCode.RECOVERY_INVALID,
            f"cannot read snapshot chunk {path}: {exc}",
        ) from exc


@dataclass(frozen=True)
class RecoveryReport:
    snapshot_dir: Path
    snapshot_version: int
    status: str
    clean_exit: bool
    published: bool
    total_probes: int
    committed_probe_ids: np.ndarray
    pending_probe_ids: np.ndarray
    chunk_hashes: tuple[str, ...]
    begin: dict[str, Any]
    arrays: dict[str, np.ndarray] | None = None
    factor_versions: tuple[str, ...] | None = None
    probe_hashes: tuple[str, ...] | None = None
    snapshot: IrradianceSnapshot | None = None

    @property
    def committed_count(self) -> int:
        return int(self.committed_probe_ids.size)

    @property
    def pending_count(self) -> int:
        return int(self.pending_probe_ids.size)

    def to_dict(self) -> dict[str, Any]:
        return {
            "snapshot_dir": str(self.snapshot_dir),
            "snapshot_version": self.snapshot_version,
            "status": self.status,
            "clean_exit": self.clean_exit,
            "published": self.published,
            "total_probes": self.total_probes,
            "committed_count": self.committed_count,
            "pending_count": self.pending_count,
            "committed_probe_ids": self.committed_probe_ids.tolist(),
            "pending_probe_ids": self.pending_probe_ids.tolist(),
            "chunk_hashes": list(self.chunk_hashes),
            "forced_termination_report": False,
        }


class SnapshotStore:
    """Write complete, atomically published snapshots and recover interrupted ones."""

    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self.root.mkdir(parents=True, exist_ok=True)

    def _version_dir(self, version: int) -> Path:
        return self.root / f"v{int(version):08d}"

    def commit_state(
        self,
        state: Any,
        *,
        version: int,
        forced_terminate_after_chunks: int | None = None,
        terminate_fn: Callable[[], None] | None = None,
        overwrite: bool = False,
    ) -> Path:
        snapshot_dir = self._version_dir(version)
        if snapshot_dir.exists() and any(snapshot_dir.iterdir()) and not overwrite:
            raise ProbeError(
                ErrorCode.IO,
                f"snapshot version directory already exists: {snapshot_dir}",
            )
        snapshot_dir.mkdir(parents=True, exist_ok=True)
        cleanup_temporary_files(snapshot_dir)
        if overwrite:
            for path in snapshot_dir.glob("chunk_*.npz"):
                path.unlink()
            for name in ("begin.json", "manifest.json"):
                try:
                    (snapshot_dir / name).unlink()
                except FileNotFoundError:
                    pass
        config = state.config
        config_payload = config.to_dict()
        config_payload["directions"] = config.resolved_directions().tolist()
        begin = {
            "schema": SNAPSHOT_SCHEMA,
            "snapshot_version": int(version),
            "total_probes": state.grid.count,
            "chunk_size": int(config.chunk_size),
            "grid_structure_hash": state.grid.structure_hash,
            "content_hash": state.content_hash,
            "scene": state.scene.to_dict(),
            "config": config_payload,
            "directions": config.resolved_directions().tolist(),
            "occluder_ids": list(state.artifacts.occluder_ids),
            "strategy": state.decision.strategy,
            "sh_order": int(config.sh_order),
        }
        write_json_atomic(snapshot_dir / "begin.json", begin)
        seed = _version_seed(
            int(version),
            state.grid.structure_hash,
            state.grid.count,
            int(config.chunk_size),
            state.content_hash,
        )
        chain_prev = seed
        chunk_metadata: list[dict[str, Any]] = []
        chunks_written = 0
        artifacts = state.artifacts
        for chunk_index, start in enumerate(range(0, state.grid.count, config.chunk_size)):
            stop = min(start + config.chunk_size, state.grid.count)
            raw = artifacts.raw_coefficients[start:stop]
            coefficients = artifacts.coefficients[start:stop]
            truncation = artifacts.truncation_error[start:stop]
            visibility = artifacts.visibility_mean[start:stop]
            sampled_mask = artifacts.sampled_mask[start:stop]
            hits = artifacts.occluder_hits[start:stop]
            arrays = _chunk_arrays(
                raw, coefficients, truncation, visibility, sampled_mask, hits
            )
            arrays_hash = hash_arrays(arrays)
            metadata = {
                "schema": CHUNK_SCHEMA,
                "snapshot_version": int(version),
                "chunk_index": chunk_index,
                "start": start,
                "count": stop - start,
                "total_probes": state.grid.count,
                "chain_prev": chain_prev,
                "arrays_hash": arrays_hash,
                "factor_versions": list(artifacts.factor_versions[start:stop]),
                "probe_hashes": list(artifacts.probe_hashes[start:stop]),
            }
            metadata["chain_hash"] = hash_object(metadata)
            chain_prev = metadata["chain_hash"]
            payload = dict(arrays)
            payload["meta_json"] = np.asarray(canonical_json(metadata))
            chunk_path = snapshot_dir / f"chunk_{chunk_index:08d}.npz"
            _atomic_npz(chunk_path, payload)
            chunk_metadata.append(metadata)
            chunks_written += 1
            if (
                forced_terminate_after_chunks is not None
                and chunks_written >= int(forced_terminate_after_chunks)
            ):
                if terminate_fn is None:
                    os._exit(23)
                terminate_fn()
                raise RuntimeError("terminate_fn returned; forced termination must not continue")

        manifest = {
            "schema": SNAPSHOT_SCHEMA,
            "snapshot_version": int(version),
            "total_probes": state.grid.count,
            "chunk_size": int(config.chunk_size),
            "grid_structure_hash": state.grid.structure_hash,
            "content_hash": state.content_hash,
            "begin_hash": hash_object(begin),
            "chunk_count": len(chunk_metadata),
            "chunks": chunk_metadata,
            "final_chain_hash": chain_prev,
            "complete": True,
        }
        write_json_atomic(snapshot_dir / "manifest.json", manifest)
        return snapshot_dir

    def _validate_begin(self, begin: Any) -> dict[str, Any]:
        if not isinstance(begin, dict):
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "snapshot begin metadata is not an object")
        missing = sorted(_REQUIRED_BEGIN - set(begin))
        if missing:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                f"snapshot begin metadata is missing fields: {missing}",
            )
        if begin["schema"] != SNAPSHOT_SCHEMA:
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "unsupported snapshot schema")
        total = int(begin["total_probes"])
        chunk_size = int(begin["chunk_size"])
        if total <= 0 or chunk_size <= 0:
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "snapshot probe/chunk count is invalid")
        return begin

    def _read_valid_chunk(
        self,
        path: Path,
        begin: dict[str, Any],
        expected_index: int,
        expected_start: int,
        expected_prev: str,
    ) -> tuple[dict[str, np.ndarray], dict[str, Any]]:
        payload = _load_chunk(path)
        required_arrays = {
            "raw_coefficients",
            "coefficients",
            "truncation_error",
            "visibility_mean",
            "sampled_mask",
            "occluder_hits",
            "meta_json",
        }
        missing_arrays = sorted(required_arrays - set(payload))
        if missing_arrays:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                f"snapshot chunk {path.name} is missing arrays: {missing_arrays}",
            )
        try:
            metadata = json.loads(str(payload["meta_json"].item()))
        except (ValueError, json.JSONDecodeError) as exc:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                f"snapshot chunk {path.name} metadata is invalid",
            ) from exc
        missing_meta = sorted(_REQUIRED_CHUNK_META - set(metadata))
        if missing_meta:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                f"snapshot chunk {path.name} metadata is missing fields: {missing_meta}",
            )
        if metadata["schema"] != CHUNK_SCHEMA:
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "unsupported chunk schema")
        if int(metadata["snapshot_version"]) != int(begin["snapshot_version"]):
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "chunk version does not match begin metadata")
        if int(metadata["chunk_index"]) != expected_index:
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "chunk index is out of sequence")
        if int(metadata["start"]) != expected_start:
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "chunk probe range is not contiguous")
        if int(metadata["total_probes"]) != int(begin["total_probes"]):
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "chunk total probe count differs")
        if metadata["chain_prev"] != expected_prev:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                f"snapshot hash chain is broken at chunk {expected_index}",
            )
        start = int(metadata["start"])
        count = int(metadata["count"])
        coefficient_count = (int(begin["sh_order"]) + 1) ** 2
        expected_shapes = {
            "raw_coefficients": (count, coefficient_count, 3),
            "coefficients": (count, coefficient_count, 3),
            "truncation_error": (count,),
            "visibility_mean": (count,),
            "sampled_mask": (count,),
            "occluder_hits": (count, len(begin["occluder_ids"])),
        }
        for name, shape in expected_shapes.items():
            if payload[name].shape != shape:
                raise ProbeError(
                    ErrorCode.RECOVERY_INVALID,
                    f"snapshot chunk {path.name} array {name} has shape {payload[name].shape}, expected {shape}",
                )
        if len(metadata["factor_versions"]) != count or len(metadata["probe_hashes"]) != count:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                f"snapshot chunk {path.name} factor/hash metadata has the wrong length",
            )
        if not np.isfinite(payload["raw_coefficients"]).all() or not np.isfinite(
            payload["coefficients"]
        ).all():
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "snapshot chunk contains non-finite coefficients")
        arrays = _chunk_arrays(
            payload["raw_coefficients"],
            payload["coefficients"],
            payload["truncation_error"],
            payload["visibility_mean"],
            payload["sampled_mask"],
            payload["occluder_hits"],
        )
        arrays_hash = hash_arrays(arrays)
        if arrays_hash != metadata["arrays_hash"]:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                f"snapshot chunk {path.name} array hash mismatch",
            )
        chain_payload = dict(metadata)
        expected_chain = chain_payload.pop("chain_hash")
        if hash_object(chain_payload) != expected_chain:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                f"snapshot chunk {path.name} chain hash mismatch",
            )
        return payload, metadata

    def recover(self, snapshot_dir: str | Path | None = None) -> RecoveryReport:
        directory = self._version_dir(self._latest_version_on_disk()) if snapshot_dir is None else Path(snapshot_dir)
        if not directory.exists():
            raise ProbeError(ErrorCode.RECOVERY_INVALID, f"snapshot directory does not exist: {directory}")
        cleanup_temporary_files(directory)
        begin_path = directory / "begin.json"
        chunk_paths = sorted(directory.glob("chunk_*.npz"))
        if not begin_path.exists():
            if chunk_paths:
                raise ProbeError(
                    ErrorCode.RECOVERY_INVALID,
                    "snapshot chunks exist without begin metadata",
                )
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "snapshot begin metadata is missing")
        begin = self._validate_begin(read_json(begin_path))
        version = int(begin["snapshot_version"])
        total = int(begin["total_probes"])
        chunk_size = int(begin["chunk_size"])
        expected_chunks = int(math.ceil(total / chunk_size))
        existing_indices: set[int] = set()
        for path in chunk_paths:
            try:
                existing_indices.add(int(path.stem.split("_")[-1]))
            except ValueError as exc:
                raise ProbeError(ErrorCode.RECOVERY_INVALID, f"invalid chunk filename {path.name}") from exc
        unexpected = sorted(index for index in existing_indices if index >= expected_chunks)
        if unexpected:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                f"snapshot has chunks beyond the declared total: {unexpected}",
            )

        chain_prev = _version_seed(
            version,
            begin["grid_structure_hash"],
            total,
            chunk_size,
            begin["content_hash"],
        )
        coefficient_count = (int(begin["sh_order"]) + 1) ** 2
        arrays: dict[str, np.ndarray] = {
            "raw_coefficients": np.zeros((total, coefficient_count, 3), dtype=np.float64),
            "coefficients": np.zeros((total, coefficient_count, 3), dtype=np.float64),
            "truncation_error": np.zeros(total, dtype=np.float64),
            "visibility_mean": np.zeros(total, dtype=np.float64),
            "sampled_mask": np.zeros(total, dtype=bool),
            "occluder_hits": np.zeros((total, len(begin["occluder_ids"])), dtype=bool),
        }
        factors: list[str] = [""] * total
        hashes: list[str] = [""] * total
        chunk_hashes: list[str] = []
        committed_end = 0
        missing_at: int | None = None
        for chunk_index in range(expected_chunks):
            path = directory / f"chunk_{chunk_index:08d}.npz"
            if not path.exists():
                if missing_at is None:
                    missing_at = chunk_index
                continue
            if missing_at is not None:
                raise ProbeError(
                    ErrorCode.RECOVERY_INVALID,
                    f"snapshot has a gap before chunk {chunk_index}",
                )
            payload, metadata = self._read_valid_chunk(
                path,
                begin,
                chunk_index,
                committed_end,
                chain_prev,
            )
            start = int(metadata["start"])
            stop = start + int(metadata["count"])
            for name in arrays:
                arrays[name][start:stop] = payload[name]
            factors[start:stop] = metadata["factor_versions"]
            hashes[start:stop] = metadata["probe_hashes"]
            chain_prev = metadata["chain_hash"]
            chunk_hashes.append(chain_prev)
            committed_end = stop

        manifest_path = directory / "manifest.json"
        manifest: dict[str, Any] | None = None
        if manifest_path.exists():
            try:
                manifest = read_json(manifest_path)
            except ProbeError as exc:
                raise ProbeError(
                    ErrorCode.RECOVERY_INVALID,
                    f"snapshot manifest is invalid: {exc.message}",
                ) from exc
            required_manifest = {
                "schema",
                "snapshot_version",
                "total_probes",
                "chunk_size",
                "content_hash",
                "begin_hash",
                "chunk_count",
                "chunks",
                "final_chain_hash",
                "complete",
            }
            if not isinstance(manifest, dict) or required_manifest - set(manifest):
                raise ProbeError(ErrorCode.RECOVERY_INVALID, "snapshot manifest is missing fields")
            if manifest["schema"] != SNAPSHOT_SCHEMA or manifest["complete"] is not True:
                raise ProbeError(ErrorCode.RECOVERY_INVALID, "snapshot manifest is not a complete commit")
            if manifest["begin_hash"] != hash_object(begin):
                raise ProbeError(ErrorCode.RECOVERY_INVALID, "snapshot begin metadata hash mismatch")
            if (
                int(manifest["snapshot_version"]) != version
                or int(manifest["total_probes"]) != total
                or int(manifest["chunk_size"]) != chunk_size
                or manifest["content_hash"] != begin["content_hash"]
            ):
                raise ProbeError(ErrorCode.RECOVERY_INVALID, "snapshot manifest metadata disagrees with begin")
            if int(manifest["chunk_count"]) != expected_chunks or len(manifest["chunks"]) != expected_chunks:
                raise ProbeError(ErrorCode.RECOVERY_INVALID, "snapshot manifest chunk count is incomplete")
            if manifest["final_chain_hash"] != chain_prev:
                raise ProbeError(ErrorCode.RECOVERY_INVALID, "snapshot manifest final hash chain mismatch")

        committed_ids = np.arange(committed_end, dtype=np.int64)
        pending_ids = np.arange(committed_end, total, dtype=np.int64)
        published = manifest is not None and pending_ids.size == 0
        status = (
            "complete"
            if published
            else ("uncommitted_complete" if pending_ids.size == 0 else "partial")
        )
        snapshot = None
        if published:
            snapshot = self._build_snapshot(begin, arrays, tuple(factors), tuple(hashes))
            if snapshot.content_hash != begin["content_hash"]:
                raise ProbeError(
                    ErrorCode.RECOVERY_INVALID,
                    "reconstructed snapshot content hash does not match begin metadata",
                )
        report = RecoveryReport(
            snapshot_dir=directory,
            snapshot_version=version,
            status=status,
            clean_exit=published,
            published=published,
            total_probes=total,
            committed_probe_ids=committed_ids,
            pending_probe_ids=pending_ids,
            chunk_hashes=tuple(chunk_hashes),
            begin=begin,
            arrays=arrays,
            factor_versions=tuple(factors),
            probe_hashes=tuple(hashes),
            snapshot=snapshot,
        )
        return report

    def _build_snapshot(
        self,
        begin: dict[str, Any],
        arrays: dict[str, np.ndarray],
        factors: tuple[str, ...],
        hashes: tuple[str, ...],
    ) -> IrradianceSnapshot:
        scene = parse_scene(begin["scene"])
        config_fields = dict(begin["config"])
        config_fields["directions"] = tuple(tuple(value) for value in begin["directions"])
        config = BakeConfig(**config_fields)
        config.validate()
        grid_result = build_probe_grid(scene, config.resolved_spacing(scene))
        if grid_result.structure_hash != begin["grid_structure_hash"]:
            raise ProbeError(
                ErrorCode.RECOVERY_INVALID,
                "reconstructed grid structure hash does not match snapshot metadata",
            )
        return IrradianceSnapshot(
            version=int(begin["snapshot_version"]),
            content_hash=begin["content_hash"],
            scene_id=scene.scene_id,
            grid=grid_result.grid,
            coefficients=np.asarray(arrays["coefficients"]),
            directions=config.resolved_directions(),
            sh_order=int(begin["sh_order"]),
            strategy=begin["strategy"],
            visibility_mean=np.asarray(arrays["visibility_mean"]),
            sampled_mask=np.asarray(arrays["sampled_mask"]),
            factor_versions=factors,
            probe_hashes=hashes,
        )

    def _latest_version_on_disk(self) -> int:
        versions: list[int] = []
        for path in self.root.glob("v*"):
            try:
                versions.append(int(path.name[1:]))
            except ValueError:
                continue
        if not versions:
            raise ProbeError(ErrorCode.RECOVERY_INVALID, "no snapshot versions exist")
        return max(versions)

    def snapshot_dirs(self) -> tuple[Path, ...]:
        return tuple(sorted(self.root.glob("v*")))

