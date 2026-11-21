"""Atomic interval commits and hash-chain recovery."""

from __future__ import annotations

import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Iterable, Mapping, Sequence

from .errors import CommitFailedError, RecoveryInvalidError
from .hashing import (
    canonical_json_bytes,
    frame_content_hash,
    pose_hash,
    sha256_bytes,
    sha256_json,
)
from .solver import FrameSolution, solutions_from_commit_records

SNAPSHOT_VERSION = 1
SNAPSHOT_SCHEMA = "pose-interval-v1"
CHAIN_SCHEMA = "pose-chain-link-v1"
GENESIS_HASH = sha256_json(
    {"schema": "pose-chain-genesis-v1", "snapshot_version": SNAPSHOT_VERSION}
)


@dataclass(frozen=True)
class RecoveryState:
    clip_id: str
    next_frame: int
    head_hash: str
    seq: int
    intervals: tuple[dict[str, Any], ...] = ()
    solutions: tuple[FrameSolution, ...] = ()
    parameter_version: str | None = None

    @property
    def has_commits(self) -> bool:
        return bool(self.intervals)


def _without_hashes(payload: Mapping[str, Any]) -> dict[str, Any]:
    result = dict(payload)
    result.pop("content_hash", None)
    result.pop("chain_hash", None)
    return result


class SnapshotStore:
    """One directory per clip; intervals are append-only and chain-linked."""

    def __init__(self, root: str | Path, clip_id: str) -> None:
        self.root = Path(root) / clip_id
        self.clip_id = clip_id
        self.root.mkdir(parents=True, exist_ok=True)
        self.fail_next_commit = False
        self.commit_hook: Callable[[str], None] | None = None

    @property
    def diagnostics_dir(self) -> Path:
        path = self.root / "diagnostics"
        path.mkdir(parents=True, exist_ok=True)
        return path

    def _interval_path(self, seq: int, start: int, end: int) -> Path:
        return self.root / f"interval_{seq:08d}_{start:08d}_{end:08d}.json"

    def _existing_intervals(self) -> list[Path]:
        return sorted(
            path
            for path in self.root.glob("interval_*.json")
            if path.is_file()
        )

    def _atomic_write(self, destination: Path, payload: Mapping[str, Any]) -> str:
        data = canonical_json_bytes(payload)
        digest = sha256_bytes(data)
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=f".{destination.name}.tmp-",
            dir=self.root,
        )
        temporary = Path(temporary_name)
        try:
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(data)
                handle.flush()
                os.fsync(handle.fileno())
            if self.fail_next_commit:
                self.fail_next_commit = False
                raise OSError("injected snapshot write failure")
            if self.commit_hook is not None:
                self.commit_hook(str(temporary))
            os.replace(temporary, destination)
            try:
                directory_fd = os.open(self.root, os.O_RDONLY)
                try:
                    os.fsync(directory_fd)
                finally:
                    os.close(directory_fd)
            except OSError:
                pass
        except Exception:
            try:
                temporary.unlink(missing_ok=True)
            except OSError:
                pass
            raise
        return digest

    def commit_range(
        self,
        solutions: Sequence[FrameSolution],
        *,
        parameter_version: str,
        document_hash: str = "",
        extra_dependencies: Mapping[str, str] | None = None,
        replay: bool = False,
    ) -> dict[str, Any]:
        if not solutions:
            raise ValueError("cannot commit an empty frame interval")
        frames = [solution.frame for solution in solutions]
        if frames != list(range(frames[0], frames[-1] + 1)):
            raise CommitFailedError(
                "interval frames must be contiguous and ascending",
                clip_id=self.clip_id,
                frame=frames[0] if frames else 0,
            )
        if any(solution.clip_id != self.clip_id for solution in solutions):
            raise CommitFailedError(
                "solution belongs to a different clip",
                clip_id=self.clip_id,
                frame=frames[0],
            )
        if any(
            solution.parameter_version != parameter_version
            for solution in solutions
        ):
            raise CommitFailedError(
                "solution parameter version does not match interval",
                clip_id=self.clip_id,
                frame=frames[0],
            )

        recovered = self.recover(
            expected_parameter_version=parameter_version if not replay else None,
            expected_document_hash=document_hash or None if not replay else None,
            require_parameter_match=not replay,
        )
        if frames[0] != recovered.next_frame:
            raise CommitFailedError(
                "interval does not start at the committed frontier",
                clip_id=self.clip_id,
                frame=frames[0],
                details={
                    "expected_start": recovered.next_frame,
                    "actual_start": frames[0],
                },
            )

        records: list[dict[str, Any]] = []
        for solution in solutions:
            record = solution.to_commit_record()
            record["transform_shape"] = list(solution.transforms.shape)
            records.append(record)

        seq = recovered.seq + 1
        interval = {
            "schema": SNAPSHOT_SCHEMA,
            "snapshot_version": SNAPSHOT_VERSION,
            "clip_id": self.clip_id,
            "parameter_version": parameter_version,
            "document_hash": document_hash,
            "extra_dependencies": dict(extra_dependencies or {}),
            "seq": seq,
            "prev_hash": recovered.head_hash,
            "start_frame": frames[0],
            "end_frame": frames[-1] + 1,
            "records": records,
        }
        content_hash = sha256_json(_without_hashes(interval))
        interval["content_hash"] = content_hash
        chain_hash = sha256_json(
            {
                "schema": CHAIN_SCHEMA,
                "prev_hash": recovered.head_hash,
                "seq": seq,
                "content_hash": content_hash,
            }
        )
        interval["chain_hash"] = chain_hash
        destination = self._interval_path(
            seq, frames[0], frames[-1] + 1
        )
        try:
            self._atomic_write(destination, interval)
        except Exception as exc:
            raise CommitFailedError(
                "atomic interval write failed; previous interval preserved",
                clip_id=self.clip_id,
                frame=frames[0],
                details={
                    "destination": str(destination),
                    "reason": str(exc),
                },
            ) from exc
        return interval

    def commit_range_without_replay(
        self,
        solutions: Sequence[FrameSolution],
        *,
        parameter_version: str,
        document_hash: str,
    ) -> dict[str, Any]:
        return self.commit_range(
            solutions,
            parameter_version=parameter_version,
            document_hash=document_hash,
        )

    def recover(
        self,
        *,
        expected_parameter_version: str | None = None,
        expected_document_hash: str | None = None,
        require_parameter_match: bool = True,
    ) -> RecoveryState:
        files = self._existing_intervals()
        if not files:
            return RecoveryState(
                clip_id=self.clip_id,
                next_frame=0,
                head_hash=GENESIS_HASH,
                seq=0,
            )
        intervals: list[dict[str, Any]] = []
        solutions: list[FrameSolution] = []
        expected_prev = GENESIS_HASH
        expected_seq = 1
        expected_frame = 0
        parameter_version: str | None = None

        for path in files:
            try:
                payload = json.loads(
                    path.read_text(encoding="utf-8")
                )
            except Exception as exc:
                raise RecoveryInvalidError(
                    "cannot read snapshot interval",
                    clip_id=self.clip_id,
                    frame=expected_frame,
                    details={
                        "breakpoint_file": str(path),
                        "reason": str(exc),
                    },
                ) from exc
            frame = int(payload.get("start_frame", expected_frame))
            if payload.get("schema") != SNAPSHOT_SCHEMA:
                raise RecoveryInvalidError(
                    "snapshot schema mismatch",
                    clip_id=self.clip_id,
                    frame=frame,
                    details={
                        "breakpoint_file": str(path),
                        "expected": SNAPSHOT_SCHEMA,
                        "actual": payload.get("schema"),
                    },
                )
            if payload.get("snapshot_version") != SNAPSHOT_VERSION:
                raise RecoveryInvalidError(
                    "snapshot version mismatch",
                    clip_id=self.clip_id,
                    frame=frame,
                    details={
                        "breakpoint_file": str(path),
                        "expected": SNAPSHOT_VERSION,
                        "actual": payload.get("snapshot_version"),
                    },
                )
            if payload.get("clip_id") != self.clip_id:
                raise RecoveryInvalidError(
                    "snapshot clip id mismatch",
                    clip_id=self.clip_id,
                    frame=frame,
                    details={"breakpoint_file": str(path)},
                )
            if payload.get("seq") != expected_seq:
                raise RecoveryInvalidError(
                    "snapshot sequence gap",
                    clip_id=self.clip_id,
                    frame=frame,
                    details={
                        "breakpoint_file": str(path),
                        "expected_seq": expected_seq,
                        "actual_seq": payload.get("seq"),
                    },
                )
            if payload.get("prev_hash") != expected_prev:
                raise RecoveryInvalidError(
                    "snapshot hash chain break",
                    clip_id=self.clip_id,
                    frame=frame,
                    details={
                        "breakpoint_file": str(path),
                        "expected_prev_hash": expected_prev,
                        "actual_prev_hash": payload.get("prev_hash"),
                    },
                )
            actual_content = sha256_json(_without_hashes(payload))
            if payload.get("content_hash") != actual_content:
                raise RecoveryInvalidError(
                    "snapshot content hash mismatch",
                    clip_id=self.clip_id,
                    frame=frame,
                    details={
                        "breakpoint_file": str(path),
                        "expected_content_hash": actual_content,
                        "actual_content_hash": payload.get("content_hash"),
                    },
                )
            actual_chain = sha256_json(
                {
                    "schema": CHAIN_SCHEMA,
                    "prev_hash": payload.get("prev_hash"),
                    "seq": payload.get("seq"),
                    "content_hash": payload.get("content_hash"),
                }
            )
            if payload.get("chain_hash") != actual_chain:
                raise RecoveryInvalidError(
                    "snapshot chain hash mismatch",
                    clip_id=self.clip_id,
                    frame=frame,
                    details={
                        "breakpoint_file": str(path),
                        "expected_chain_hash": actual_chain,
                        "actual_chain_hash": payload.get("chain_hash"),
                    },
                )
            start = int(payload["start_frame"])
            end = int(payload["end_frame"])
            if start != expected_frame or end <= start:
                raise RecoveryInvalidError(
                    "snapshot frame intervals are not contiguous",
                    clip_id=self.clip_id,
                    frame=start,
                    details={
                        "breakpoint_file": str(path),
                        "expected_start": expected_frame,
                        "actual_interval": [start, end],
                    },
                )
            if (
                require_parameter_match
                and expected_parameter_version is not None
                and payload.get("parameter_version")
                != expected_parameter_version
            ):
                raise RecoveryInvalidError(
                    "snapshot dependency parameter version mismatch",
                    clip_id=self.clip_id,
                    frame=start,
                    details={
                        "breakpoint_file": str(path),
                        "expected_parameter_version": expected_parameter_version,
                        "actual_parameter_version": payload.get(
                            "parameter_version"
                        ),
                    },
                )
            if (
                expected_document_hash is not None
                and payload.get("document_hash")
                not in {"", expected_document_hash}
            ):
                raise RecoveryInvalidError(
                    "snapshot document dependency mismatch",
                    clip_id=self.clip_id,
                    frame=start,
                    details={"breakpoint_file": str(path)},
                )
            records = payload.get("records")
            if not isinstance(records, list):
                raise RecoveryInvalidError(
                    "snapshot records are missing",
                    clip_id=self.clip_id,
                    frame=start,
                    details={"breakpoint_file": str(path)},
                )
            expected_records = list(range(start, end))
            actual_records = [int(record.get("frame", -1)) for record in records]
            if actual_records != expected_records:
                raise RecoveryInvalidError(
                    "snapshot records do not cover the interval",
                    clip_id=self.clip_id,
                    frame=start,
                    details={
                        "breakpoint_file": str(path),
                        "expected_frames": [start, end],
                    },
                )
            try:
                interval_solutions = solutions_from_commit_records(
                    clip_id=self.clip_id,
                    parameter_version=str(payload["parameter_version"]),
                    records=records,
                )
            except Exception as exc:
                raise RecoveryInvalidError(
                    "cannot reconstruct committed pose records",
                    clip_id=self.clip_id,
                    frame=start,
                    details={
                        "breakpoint_file": str(path),
                        "reason": str(exc),
                    },
                ) from exc
            for record, solution in zip(
                records, interval_solutions, strict=True
            ):
                actual_pose_hash = pose_hash(solution.transforms)
                if actual_pose_hash != record.get("pose_hash"):
                    raise RecoveryInvalidError(
                        "committed pose hash mismatch",
                        clip_id=self.clip_id,
                        frame=solution.frame,
                        details={"breakpoint_file": str(path)},
                    )
                actual_content_hash = frame_content_hash(
                    frame=solution.frame,
                    transforms=solution.transforms,
                    residuals=solution.residuals,
                    parameter_version=str(payload["parameter_version"]),
                )
                if actual_content_hash != record.get("content_hash"):
                    raise RecoveryInvalidError(
                        "committed frame content hash mismatch",
                        clip_id=self.clip_id,
                        frame=solution.frame,
                        details={"breakpoint_file": str(path)},
                    )
            intervals.append(payload)
            solutions.extend(interval_solutions)
            expected_prev = payload["chain_hash"]
            expected_seq += 1
            expected_frame = end
            parameter_version = str(payload["parameter_version"])

        return RecoveryState(
            clip_id=self.clip_id,
            next_frame=expected_frame,
            head_hash=expected_prev,
            seq=expected_seq - 1,
            intervals=tuple(intervals),
            solutions=tuple(solutions),
            parameter_version=parameter_version,
        )

    def write_diagnostic(self, diagnostic: Mapping[str, Any]) -> Path:
        frame = int(diagnostic["frame"])
        destination = self.diagnostics_dir / f"conflict_{frame:08d}.json"
        self._atomic_write(destination, diagnostic)
        return destination

    def write_trace(self, records: Iterable[Mapping[str, Any]]) -> Path:
        destination = self.root / "trace.jsonl"
        lines = [
            canonical_json_bytes(record).decode("utf-8")
            for record in records
        ]
        data = ("\n".join(lines) + ("\n" if lines else "")).encode("utf-8")
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=".trace.tmp-", dir=self.root
        )
        temporary = Path(temporary_name)
        try:
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(data)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, destination)
        except Exception:
            temporary.unlink(missing_ok=True)
            raise
        return destination

    def list_diagnostics(self) -> tuple[Path, ...]:
        return tuple(sorted(self.diagnostics_dir.glob("conflict_*.json")))
