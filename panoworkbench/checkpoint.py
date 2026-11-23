"""Atomic, versioned, serialized checkpoint storage."""

from __future__ import annotations

import contextlib
import json
import os
import threading
import uuid
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterator

from .config import ALGORITHM_VERSION
from .dataset import Dataset, pair_content_hash
from .errors import ErrorCode, WorkbenchError
from .geometry import normalize_homography
from .state import PairResult

try:  # pragma: no cover - platform-specific
    import fcntl
except ImportError:  # pragma: no cover - Windows
    fcntl = None

try:  # pragma: no cover - platform-specific
    import msvcrt
except ImportError:  # pragma: no cover - POSIX
    msvcrt = None


CHECKPOINT_SCHEMA_VERSION = 1


@dataclass
class CheckpointLoad:
    valid: bool
    existed: bool
    snapshot: dict[str, Any] | None
    restored_pairs: dict[str, PairResult]
    invalid_reason: str | None = None


def _point_tuple(value: Any) -> tuple[tuple[float, float], ...]:
    if not isinstance(value, list):
        raise ValueError("points must be a list")
    points: list[tuple[float, float]] = []
    for item in value:
        if not isinstance(item, list) or len(item) != 2:
            raise ValueError("point must be [x, y]")
        x, y = float(item[0]), float(item[1])
        if not (float("-inf") < x < float("inf") and float("-inf") < y < float("inf")):
            raise ValueError("point must be finite")
        points.append((x, y))
    return tuple(points)


def _result_from_dict(payload: dict[str, Any]) -> PairResult:
    homography = tuple(float(value) for value in payload["homography_a_to_b"])
    if len(homography) != 9:
        raise ValueError("homography must have nine values")
    normalized = normalize_homography(
        [homography[0:3], homography[3:6], homography[6:9]]
    )
    result = PairResult(
        pair_id=str(payload["pair_id"]),
        image_a=int(payload["image_a"]),
        image_b=int(payload["image_b"]),
        homography_a_to_b=tuple(float(value) for value in normalized.reshape(-1)),
        matches=int(payload["matches"]),
        inliers=int(payload["inliers"]),
        inlier_ratio=float(payload["inlier_ratio"]),
        residual_median=float(payload["residual_median"]),
        residual_p95=float(payload["residual_p95"]),
        covariance_condition=float(payload["covariance_condition"]),
        content_hash=str(payload["content_hash"]),
        parameter_version=str(payload["parameter_version"]),
        algorithm_version=str(payload["algorithm_version"]),
        truth_corner_error=(
            None
            if payload.get("truth_corner_error") is None
            else float(payload["truth_corner_error"])
        ),
        truth_ok=payload.get("truth_ok"),
        inlier_points_a=_point_tuple(payload.get("inlier_points_a", [])),
        inlier_points_b=_point_tuple(payload.get("inlier_points_b", [])),
        inlier_residuals=tuple(
            float(value) for value in payload.get("inlier_residuals", [])
        ),
    )
    numeric = [
        *result.homography_a_to_b,
        result.inlier_ratio,
        result.residual_median,
        result.residual_p95,
        result.covariance_condition,
    ]
    if not all(float("-inf") < value < float("inf") for value in numeric):
        raise ValueError("checkpoint contains non-finite registration metrics")
    if result.matches <= 0 or result.inliers < 4 or result.inliers > result.matches:
        raise ValueError("checkpoint contains inconsistent inlier counts")
    if len(result.inlier_points_a) != len(result.inlier_points_b):
        raise ValueError("checkpoint inlier point arrays differ in length")
    if result.inlier_residuals and len(result.inlier_residuals) != len(
        result.inlier_points_a
    ):
        raise ValueError("checkpoint residual array differs in length")
    return result


class CheckpointStore:
    """A single versioned snapshot rewritten atomically after each commit.

    Committed pair writes are serialized with a process lock and an advisory
    file lock. A kill can leave a stale temporary file, but never a partially
    interleaved snapshot.
    """

    def __init__(
        self,
        path: str | Path,
        *,
        dataset: Dataset,
        parameter_version: str,
        plan_fingerprint: dict[str, Any],
    ) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.lock_path = self.path.with_suffix(self.path.suffix + ".lock")
        self.dataset = dataset
        self.parameter_version = parameter_version
        self.plan_fingerprint = plan_fingerprint
        self.algorithm_version = ALGORITHM_VERSION
        self._pairs: dict[str, PairResult] = {}
        self._panorama: dict[str, Any] | None = None
        self._sequence = 0
        self._thread_lock = threading.RLock()

    @contextlib.contextmanager
    def _file_lock(self) -> Iterator[None]:
        with self._thread_lock:
            handle = open(self.lock_path, "a+b")
            try:
                if fcntl is not None:
                    fcntl.flock(handle.fileno(), fcntl.LOCK_EX)
                elif msvcrt is not None:  # pragma: no cover - Windows
                    if handle.tell() < 1:
                        handle.write(b"\0")
                        handle.flush()
                    handle.seek(0)
                    msvcrt.locking(handle.fileno(), msvcrt.LK_LOCK, 1)
                yield
            finally:
                if fcntl is not None:
                    fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
                elif msvcrt is not None:  # pragma: no cover - Windows
                    handle.seek(0)
                    msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
                handle.close()

    def _check_header(self, payload: dict[str, Any]) -> str | None:
        if payload.get("schema_version") != CHECKPOINT_SCHEMA_VERSION:
            return "checkpoint schema version mismatch"
        if payload.get("algorithm_version") != self.algorithm_version:
            return "algorithm version mismatch"
        if payload.get("parameter_version") != self.parameter_version:
            return "parameter version mismatch"
        if payload.get("dataset_hash") != self.dataset.content_hash:
            return "dataset content hash mismatch"
        if payload.get("plan_fingerprint") != self.plan_fingerprint:
            return "strategy plan fingerprint mismatch"
        return None

    def load(self) -> CheckpointLoad:
        if not self.path.is_file():
            return CheckpointLoad(False, False, None, {})
        try:
            payload = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            return CheckpointLoad(
                False, True, None, {}, f"checkpoint cannot be decoded: {exc}"
            )
        if not isinstance(payload, dict):
            return CheckpointLoad(False, True, None, {}, "checkpoint root must be an object")
        header_error = self._check_header(payload)
        if header_error is not None:
            return CheckpointLoad(False, True, payload, {}, header_error)
        entries = payload.get("pairs")
        if not isinstance(entries, list):
            return CheckpointLoad(False, True, payload, {}, "checkpoint pairs must be a list")
        restored: dict[str, PairResult] = {}
        known_pair_ids = {
            f"P{position:04d}_{image_a}_{image_b}"
            for position, (image_a, image_b) in enumerate(
                sorted(
                    (min(pair[0], pair[1]), max(pair[0], pair[1]))
                    for pair in self.plan_fingerprint["pairs"]
                )
            )
        }
        try:
            for entry in entries:
                if not isinstance(entry, dict):
                    raise ValueError("pair entry must be an object")
                result = _result_from_dict(entry)
                if result.pair_id in restored:
                    raise ValueError(f"duplicate pair {result.pair_id}")
                if result.pair_id not in known_pair_ids:
                    raise ValueError(f"pair {result.pair_id} is not in the current plan")
                if result.parameter_version != self.parameter_version:
                    raise ValueError(f"pair {result.pair_id} parameter version mismatch")
                if result.algorithm_version != self.algorithm_version:
                    raise ValueError(f"pair {result.pair_id} algorithm version mismatch")
                expected_hash = pair_content_hash(
                    self.dataset,
                    result.image_a,
                    result.image_b,
                    self.parameter_version,
                )
                if result.content_hash != expected_hash:
                    raise ValueError(f"pair {result.pair_id} content hash mismatch")
                restored[result.pair_id] = result
        except (KeyError, TypeError, ValueError, WorkbenchError) as exc:
            return CheckpointLoad(
                False,
                True,
                payload,
                {},
                f"checkpoint pair validation failed: {exc}",
            )
        panorama = payload.get("panorama")
        if panorama is not None and not isinstance(panorama, dict):
            return CheckpointLoad(
                False, True, payload, {}, "checkpoint panorama must be an object"
            )
        self._pairs = dict(restored)
        self._panorama = panorama
        try:
            self._sequence = int(payload.get("sequence", len(entries)))
        except (TypeError, ValueError):
            self._sequence = len(entries)
        return CheckpointLoad(True, True, payload, restored)

    def invalidate(self, reason: str) -> None:
        with self._file_lock():
            try:
                self.path.unlink(missing_ok=True)
            except OSError as exc:
                raise WorkbenchError(
                    ErrorCode.IO,
                    f"failed to invalidate checkpoint: {exc}",
                    filename=str(self.path),
                ) from exc
            marker = self.path.with_suffix(".invalid.json")
            marker.write_text(
                json.dumps({"reason": reason}, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
        self._pairs.clear()
        self._panorama = None
        self._sequence = 0

    def _snapshot(self) -> dict[str, Any]:
        return {
            "schema_version": CHECKPOINT_SCHEMA_VERSION,
            "algorithm_version": self.algorithm_version,
            "parameter_version": self.parameter_version,
            "dataset_hash": self.dataset.content_hash,
            "plan_fingerprint": self.plan_fingerprint,
            "sequence": self._sequence,
            "pairs": [
                self._pairs[pair_id].to_dict()
                for pair_id in sorted(self._pairs)
            ],
            "panorama": self._panorama,
        }

    def _atomic_write(self, payload: dict[str, Any]) -> None:
        data = (
            json.dumps(payload, sort_keys=True, separators=(",", ":"), allow_nan=False)
            + "\n"
        ).encode("utf-8")
        self.path.parent.mkdir(parents=True, exist_ok=True)
        temporary = self.path.parent / f".{self.path.name}.{uuid.uuid4().hex}.tmp"
        try:
            with open(temporary, "wb") as handle:
                handle.write(data)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, self.path)
            if os.name != "nt":
                directory_fd = os.open(self.path.parent, os.O_RDONLY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
        except OSError as exc:
            with contextlib.suppress(OSError):
                temporary.unlink()
            raise WorkbenchError(
                ErrorCode.IO,
                f"atomic checkpoint write failed: {exc}",
                filename=str(self.path),
            ) from exc

    def commit_pair(self, result: PairResult) -> None:
        with self._file_lock():
            self._pairs[result.pair_id] = result
            self._sequence += 1
            self._atomic_write(self._snapshot())

    def commit_panorama(self, result: dict[str, Any]) -> None:
        with self._file_lock():
            self._panorama = result
            self._sequence += 1
            self._atomic_write(self._snapshot())

    @property
    def committed_pairs(self) -> dict[str, PairResult]:
        return dict(self._pairs)

    @property
    def panorama_snapshot(self) -> dict[str, Any] | None:
        return dict(self._panorama) if self._panorama is not None else None
