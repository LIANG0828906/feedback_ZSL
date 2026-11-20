"""Atomic interval snapshots with a verifiable pose-hash chain."""

from __future__ import annotations

import hashlib
import json
import os
import struct
import threading
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import numpy as np

from .bvh import BvhDocument
from .constraints import ConstraintSet
from .errors import E_IO, E_RECOVERY_INVALID, BusinessError
from .math3d import (
    canonical_json_bytes,
    hash_pose,
    record_to_transform,
    transform_to_record,
)
from .solver import FrameResult, SolverConfig

SNAPSHOT_FORMAT = "mocap-snapshot-v1"
SNAPSHOT_MAGIC = b"MOCAPSNAP1\n"
MANIFEST_NAME = "run.json"


def _recovery_fail(message: str, **details: object) -> None:
    raise BusinessError(E_RECOVERY_INVALID, message, details)


def _chain_next(previous: str, pose_hash: str) -> str:
    return hashlib.sha256(f"{previous}|{pose_hash}".encode("ascii")).hexdigest()


@dataclass(frozen=True)
class SnapshotIdentity:
    bvh_sha256: str
    skeleton_fingerprint: str
    frame_count: int
    joint_count: int
    joint_names: tuple[str, ...]
    joint_parents: tuple[int | None, ...]
    constraint_revision: int
    constraint_hash: str
    solver_config: Mapping[str, Any]
    solver_config_hash: str

    @classmethod
    def from_inputs(
        cls,
        document: BvhDocument,
        constraints: ConstraintSet,
        config: SolverConfig,
    ) -> SnapshotIdentity:
        return cls(
            bvh_sha256=document.source_sha256,
            skeleton_fingerprint=document.skeleton_fingerprint(),
            frame_count=document.frame_count,
            joint_count=document.joint_count,
            joint_names=document.joint_names,
            joint_parents=tuple(joint.parent for joint in document.joints),
            constraint_revision=int(constraints.revision),
            constraint_hash=constraints.semantic_hash(),
            solver_config=config.to_dict(),
            solver_config_hash=config.execution_signature(),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "bvh_sha256": self.bvh_sha256,
            "skeleton_fingerprint": self.skeleton_fingerprint,
            "frame_count": self.frame_count,
            "joint_count": self.joint_count,
            "joint_names": list(self.joint_names),
            "joint_parents": list(self.joint_parents),
            "constraint_revision": self.constraint_revision,
            "constraint_hash": self.constraint_hash,
            "solver_config": dict(self.solver_config),
            "solver_config_hash": self.solver_config_hash,
        }

    @property
    def genesis_chain(self) -> str:
        payload = canonical_json_bytes(self.to_dict())
        return hashlib.sha256(b"mocap-chain-v1\x00" + payload).hexdigest()


@dataclass
class RecoveredFrame:
    frame: int
    transforms: np.ndarray
    residuals: np.ndarray
    constraint_ids: tuple[str, ...]
    pose_hash: str
    conflict_report: dict[str, Any] | None = None


@dataclass
class RecoveryState:
    valid: bool
    frames: dict[int, RecoveredFrame] = field(default_factory=dict)
    last_end: int = -1
    next_frame: int = 0
    committed_ranges: tuple[tuple[int, int], ...] = ()
    conflict_reports: tuple[dict[str, Any], ...] = ()
    chain_head: str = ""
    snapshot_count: int = 0

    @property
    def committed_frames(self) -> tuple[int, ...]:
        return tuple(sorted(self.frames))

    def summary(self) -> dict[str, Any]:
        return {
            "valid": self.valid,
            "last_end": self.last_end,
            "next_frame": self.next_frame,
            "committed_frames": len(self.frames),
            "committed_ranges": [list(item) for item in self.committed_ranges],
            "conflicts": len(self.conflict_reports),
            "chain_head": self.chain_head,
            "snapshot_count": self.snapshot_count,
        }


def _ranges_from_frames(frames: Sequence[int]) -> tuple[tuple[int, int], ...]:
    if not frames:
        return ()
    ordered = sorted({int(frame) for frame in frames})
    out: list[tuple[int, int]] = []
    start = previous = ordered[0]
    for frame in ordered[1:]:
        if frame == previous + 1:
            previous = frame
            continue
        out.append((start, previous))
        start = previous = frame
    out.append((start, previous))
    return tuple(out)


class SnapshotStore:
    """Atomic, deterministic snapshot writer and validator.

    Each snapshot interval contains only committed (non-conflict) frames.
    Conflict frames can therefore fall inside the processing interval while
    remaining absent from ``committed_frames`` and from the pose hash chain.
    """

    def __init__(
        self,
        directory: str | Path,
        document: BvhDocument,
        constraints: ConstraintSet,
        config: SolverConfig,
    ):
        self.directory = Path(directory)
        self.document = document
        self.constraints = constraints
        self.config = config
        self.identity = SnapshotIdentity.from_inputs(document, constraints, config)
        self._lock = threading.RLock()
        self._chain = self.identity.genesis_chain
        self._last_end = -1
        self._initialized = False

    @property
    def manifest_path(self) -> Path:
        return self.directory / MANIFEST_NAME

    def open(self) -> None:
        self.directory.mkdir(parents=True, exist_ok=True)
        if self.manifest_path.exists():
            self._validate_manifest()
        else:
            snapshots = list(self.directory.glob("*.snap"))
            if snapshots:
                _recovery_fail(
                    "snapshot files exist without a run manifest",
                    directory=str(self.directory),
                )
            self._write_manifest()
        self._initialized = True

    def reset(self) -> None:
        """Remove only files owned by this snapshot format.

        Fresh CLI/UI runs call this explicitly.  Resume paths never do.
        """

        self.directory.mkdir(parents=True, exist_ok=True)
        for pattern in (MANIFEST_NAME, "*.snap", ".run.json.tmp.*", ".*.snap.tmp.*"):
            for path in self.directory.glob(pattern):
                try:
                    path.unlink()
                except OSError as exc:
                    raise BusinessError(
                        E_IO,
                        "unable to reset snapshot directory",
                        {"path": str(path), "error": str(exc)},
                    ) from exc
        self._chain = self.identity.genesis_chain
        self._last_end = -1
        self._initialized = False

    def _manifest_payload(self) -> dict[str, Any]:
        return {
            "format": SNAPSHOT_FORMAT,
            "format_version": 1,
            "identity": self.identity.to_dict(),
            "genesis_chain": self.identity.genesis_chain,
            "joint_names": list(self.identity.joint_names),
        }

    def _write_manifest(self) -> None:
        payload = self._manifest_payload()
        self._atomic_write(
            self.manifest_path,
            canonical_json_bytes(payload) + b"\n",
        )

    def _validate_manifest(self) -> None:
        try:
            raw = json.loads(self.manifest_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            _recovery_fail("unable to read run manifest", error=str(exc))
        required = {"format", "format_version", "identity", "genesis_chain", "joint_names"}
        if not isinstance(raw, Mapping) or not required.issubset(raw):
            _recovery_fail("run manifest is missing required fields")
        if raw.get("format") != SNAPSHOT_FORMAT or int(raw.get("format_version", -1)) != 1:
            _recovery_fail("run manifest format/version is unsupported")
        actual = raw.get("identity")
        if not isinstance(actual, Mapping):
            _recovery_fail("run manifest identity is missing")
        expected = self.identity.to_dict()
        for key in (
            "bvh_sha256",
            "skeleton_fingerprint",
            "frame_count",
            "joint_count",
            "constraint_revision",
            "constraint_hash",
            "solver_config_hash",
        ):
            if actual.get(key) != expected.get(key):
                _recovery_fail(
                    "run manifest does not match the current input or constraint/solver version",
                    field=key,
                    expected=expected.get(key),
                    actual=actual.get(key),
                )
        if list(actual.get("joint_names", [])) != list(expected["joint_names"]):
            _recovery_fail("run manifest joint names do not match the BVH")

    def _atomic_write(self, target: Path, payload: bytes) -> None:
        temp = target.with_name(
            f".{target.name}.tmp.{os.getpid()}.{threading.get_ident()}"
        )
        try:
            with open(temp, "wb") as handle:
                handle.write(payload)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temp, target)
            try:
                directory_fd = os.open(target.parent, os.O_RDONLY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
            except OSError:
                pass
        except OSError as exc:
            try:
                temp.unlink(missing_ok=True)
            except OSError:
                pass
            raise BusinessError(
                E_IO,
                "unable to atomically write snapshot",
                {"path": str(target), "error": str(exc)},
            ) from exc

    def _snapshot_name(self, start: int, end: int) -> str:
        return f"frame_{start:08d}_{end:08d}.snap"

    def commit_interval(
        self,
        start_frame: int,
        end_frame: int,
        results: Sequence[FrameResult],
        conflict_reports: Sequence[Mapping[str, Any]] = (),
    ) -> Path:
        if not self._initialized:
            self.open()
        with self._lock:
            if start_frame != self._last_end + 1:
                _recovery_fail(
                    "snapshot commit is not contiguous",
                    expected_start=self._last_end + 1,
                    actual_start=start_frame,
                )
            if end_frame < start_frame:
                _recovery_fail("snapshot end precedes snapshot start")
            committed = [item for item in results if 0 <= item.frame < self.document.frame_count]
            frames = [item.frame for item in committed]
            if not frames:
                # A pure conflict interval still advances the processing
                # boundary and records evidence, but commits no pose frames.
                pass
            if len(set(frames)) != len(frames):
                _recovery_fail("snapshot contains duplicate committed frames")
            for frame in frames:
                if frame < start_frame or frame > end_frame:
                    _recovery_fail(
                        "committed frame is outside the declared interval",
                        frame=frame,
                        start=start_frame,
                        end=end_frame,
                    )
            committed.sort(key=lambda item: item.frame)
            previous_chain = self._chain
            chain = previous_chain
            pose_hashes: list[str] = []
            transforms = np.empty(
                (len(committed), self.document.joint_count, 4, 4), dtype=np.float64
            )
            for index, result in enumerate(committed):
                recomputed = hash_pose(self.document.joint_names, result.pose.world)
                if recomputed != result.pose_hash:
                    _recovery_fail(
                        "result pose hash is inconsistent before commit",
                        frame=result.frame,
                    )
                pose_hashes.append(recomputed)
                transforms[index] = result.pose.world
                chain = _chain_next(chain, recomputed)
            constraint_ids = tuple(item.id for item in self.constraints.constraints)
            residual_values = np.empty(
                (len(committed), len(constraint_ids)), dtype=np.float64
            )
            for row, result in enumerate(committed):
                by_id = {item.constraint_id: item for item in result.residuals}
                for column, constraint_id in enumerate(constraint_ids):
                    residual_values[row, column] = float(
                        by_id[constraint_id].residual if constraint_id in by_id else 0.0
                    )
            header = {
                "format": SNAPSHOT_FORMAT,
                "format_version": 1,
                "start_frame": int(start_frame),
                "end_frame": int(end_frame),
                "committed_frames": [int(frame) for frame in frames],
                "pose_hashes": pose_hashes,
                "chain_start": previous_chain,
                "chain_end": chain,
                "constraint_revision": int(self.constraints.revision),
                "constraint_hash": self.constraints.semantic_hash(),
                "solver_config_hash": self.config.execution_signature(),
                "solver_config": self.config.to_dict(),
                "bvh_sha256": self.document.source_sha256,
                "skeleton_fingerprint": self.document.skeleton_fingerprint(),
                "constraint_ids": list(constraint_ids),
                "conflicts": list(conflict_reports),
                "transforms_shape": list(transforms.shape),
                "residuals_shape": list(residual_values.shape),
                "transforms_dtype": "<f8",
                "residuals_dtype": "<f8",
            }
            header_bytes = canonical_json_bytes(header)
            payload = (
                SNAPSHOT_MAGIC
                + struct.pack("<Q", len(header_bytes))
                + header_bytes
                + np.ascontiguousarray(transforms, dtype="<f8").tobytes(order="C")
                + np.ascontiguousarray(residual_values, dtype="<f8").tobytes(order="C")
            )
            target = self.directory / self._snapshot_name(start_frame, end_frame)
            self._atomic_write(target, payload)
            self._last_end = int(end_frame)
            self._chain = chain
            return target

    def _read_snapshot(
        self, path: Path
    ) -> tuple[dict[str, Any], np.ndarray, np.ndarray]:
        try:
            raw = path.read_bytes()
        except OSError as exc:
            _recovery_fail("unable to read snapshot", path=str(path), error=str(exc))
        if len(raw) < len(SNAPSHOT_MAGIC) + 8 or not raw.startswith(SNAPSHOT_MAGIC):
            _recovery_fail("snapshot has an invalid magic header", path=str(path))
        offset = len(SNAPSHOT_MAGIC)
        header_len = struct.unpack("<Q", raw[offset : offset + 8])[0]
        offset += 8
        if header_len <= 0 or offset + header_len > len(raw):
            _recovery_fail("snapshot header length is invalid", path=str(path))
        header_bytes = raw[offset : offset + header_len]
        offset += header_len
        try:
            header = json.loads(header_bytes.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            _recovery_fail("snapshot header is not valid JSON", path=str(path), error=str(exc))
        if not isinstance(header, Mapping):
            _recovery_fail("snapshot header must be an object", path=str(path))
        required = {
            "format",
            "format_version",
            "start_frame",
            "end_frame",
            "committed_frames",
            "pose_hashes",
            "chain_start",
            "chain_end",
            "constraint_revision",
            "constraint_hash",
            "solver_config_hash",
            "solver_config",
            "bvh_sha256",
            "skeleton_fingerprint",
            "constraint_ids",
            "transforms_shape",
            "residuals_shape",
            "transforms_dtype",
            "residuals_dtype",
        }
        missing = sorted(required - set(header))
        if missing:
            _recovery_fail(
                "snapshot is missing required fields",
                path=str(path),
                missing=missing,
            )
        if header.get("format") != SNAPSHOT_FORMAT or int(header.get("format_version", -1)) != 1:
            _recovery_fail("snapshot format/version is unsupported", path=str(path))
        try:
            t_shape = tuple(int(x) for x in header["transforms_shape"])
            r_shape = tuple(int(x) for x in header["residuals_shape"])
        except (TypeError, ValueError):
            _recovery_fail("snapshot array shapes are invalid", path=str(path))
        if len(t_shape) != 4 or len(r_shape) != 2:
            _recovery_fail("snapshot array shapes have the wrong rank", path=str(path))
        if t_shape[1:] != (self.document.joint_count, 4, 4):
            _recovery_fail("snapshot joint dimension does not match the BVH", path=str(path))
        if r_shape[1] != len(self.constraints.constraints):
            _recovery_fail("snapshot constraint dimension does not match the constraints", path=str(path))
        t_count = int(np.prod(t_shape))
        r_count = int(np.prod(r_shape))
        expected_bytes = t_count * 8 + r_count * 8
        if len(raw) - offset != expected_bytes:
            _recovery_fail(
                "snapshot payload length does not match its header",
                path=str(path),
                expected=expected_bytes,
                actual=len(raw) - offset,
            )
        transforms = np.frombuffer(
            raw, dtype="<f8", count=t_count, offset=offset
        ).reshape(t_shape).copy()
        offset += t_count * 8
        residuals = np.frombuffer(
            raw, dtype="<f8", count=r_count, offset=offset
        ).reshape(r_shape).copy()
        if not np.all(np.isfinite(transforms)) or not np.all(np.isfinite(residuals)):
            _recovery_fail("snapshot contains non-finite arrays", path=str(path))
        return dict(header), transforms, residuals

    def recover(self, *, require_manifest: bool = False) -> RecoveryState:
        self.directory.mkdir(parents=True, exist_ok=True)
        if not self.manifest_path.exists():
            if require_manifest:
                _recovery_fail("run manifest is missing", directory=str(self.directory))
            snapshots = list(self.directory.glob("*.snap"))
            if snapshots:
                _recovery_fail("snapshot files exist without a run manifest")
            return RecoveryState(valid=True, chain_head=self.identity.genesis_chain)
        self._validate_manifest()
        files = sorted(self.directory.glob("*.snap"))
        chain = self.identity.genesis_chain
        frames: dict[int, RecoveredFrame] = {}
        conflicts: list[dict[str, Any]] = []
        intervals: list[tuple[int, int]] = []
        previous_interval_end = -1
        for path in files:
            header, transforms, residuals = self._read_snapshot(path)
            start = int(header["start_frame"])
            end = int(header["end_frame"])
            if start != previous_interval_end + 1:
                _recovery_fail(
                    "snapshot intervals are not contiguous",
                    path=str(path),
                    expected_start=previous_interval_end + 1,
                    actual_start=start,
                )
            if end < start:
                _recovery_fail("snapshot end precedes start", path=str(path))
            for field_name in (
                "constraint_revision",
                "constraint_hash",
                "solver_config_hash",
                "bvh_sha256",
                "skeleton_fingerprint",
            ):
                expected_value = getattr(
                    self.identity,
                    field_name,
                )
                if header.get(field_name) != expected_value:
                    _recovery_fail(
                        "snapshot does not match the requested constraint/solver version",
                        field=field_name,
                        path=str(path),
                    )
            if header.get("solver_config") != self.config.to_dict():
                _recovery_fail(
                    "snapshot solver configuration does not match the current run",
                    path=str(path),
                )
            if list(header.get("constraint_ids", [])) != [
                item.id for item in self.constraints.constraints
            ]:
                _recovery_fail("snapshot constraint identifiers changed", path=str(path))
            committed = header.get("committed_frames")
            pose_hashes = header.get("pose_hashes")
            if not isinstance(committed, list) or not isinstance(pose_hashes, list):
                _recovery_fail("snapshot committed frame/hash fields are malformed", path=str(path))
            if len(committed) != len(pose_hashes) or len(committed) != transforms.shape[0]:
                _recovery_fail("snapshot committed frame/hash/array lengths differ", path=str(path))
            if committed != sorted({int(x) for x in committed}):
                _recovery_fail("snapshot committed frames are not sorted and unique", path=str(path))
            if any(int(frame) < start or int(frame) > end for frame in committed):
                _recovery_fail("snapshot committed frame is outside its interval", path=str(path))
            if header.get("chain_start") != chain:
                _recovery_fail(
                    "snapshot hash chain is broken",
                    path=str(path),
                    expected=chain,
                    actual=header.get("chain_start"),
                )
            local_chain = chain
            for row, frame in enumerate(committed):
                recomputed = hash_pose(self.document.joint_names, transforms[row])
                if recomputed != pose_hashes[row]:
                    _recovery_fail(
                        "snapshot pose hash mismatch",
                        path=str(path),
                        frame=int(frame),
                    )
                local_chain = _chain_next(local_chain, recomputed)
            if local_chain != header.get("chain_end"):
                _recovery_fail("snapshot end-of-chain hash is invalid", path=str(path))
            conflict_reports = header.get("conflicts", [])
            if not isinstance(conflict_reports, list):
                _recovery_fail("snapshot conflict report is malformed", path=str(path))
            report_by_frame = {
                int(item["frame"]): item
                for item in conflict_reports
                if isinstance(item, Mapping) and "frame" in item
            }
            constraint_ids = tuple(str(x) for x in header.get("constraint_ids", []))
            for row, frame in enumerate(committed):
                frame_int = int(frame)
                if frame_int in frames:
                    _recovery_fail("committed frame appears in multiple snapshots", frame=frame_int)
                frames[frame_int] = RecoveredFrame(
                    frame=frame_int,
                    transforms=transforms[row].copy(),
                    residuals=residuals[row].copy(),
                    constraint_ids=constraint_ids,
                    pose_hash=str(pose_hashes[row]),
                    conflict_report=report_by_frame.get(frame_int),
                )
            conflicts.extend(conflict_reports)
            intervals.append((start, end))
            chain = local_chain
            previous_interval_end = end
        recovery = RecoveryState(
            valid=True,
            frames=frames,
            last_end=previous_interval_end,
            next_frame=previous_interval_end + 1,
            committed_ranges=_ranges_from_frames(list(frames)),
            conflict_reports=tuple(conflicts),
            chain_head=chain,
            snapshot_count=len(files),
        )
        self._chain = chain
        self._last_end = previous_interval_end
        self._initialized = True
        return recovery

    @property
    def chain_head(self) -> str:
        return self._chain

    @property
    def last_end(self) -> int:
        return self._last_end


def recovered_transforms_to_records(transforms: np.ndarray) -> np.ndarray:
    return np.stack(
        [
            np.stack([transform_to_record(matrix) for matrix in frame])
            for frame in np.asarray(transforms)
        ]
    )


def records_to_transforms(records: np.ndarray) -> np.ndarray:
    return np.stack(
        [
            np.stack([record_to_transform(record) for record in frame])
            for frame in np.asarray(records)
        ]
    )

