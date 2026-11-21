from __future__ import annotations

import copy
import json
import os
import platform
import tempfile
import threading
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Iterator, Sequence

from . import __version__
from .bvh import BVHData
from .constraints import ConstraintSet
from .errors import (
    E_BUSY,
    E_COMMIT_FAILED,
    E_RECOVERY_INVALID,
    WorkbenchError,
)
from .hashing import canonical_json, chain_hash, content_hash, sha256_bytes
from .solver import (
    ConflictDiagnostic,
    FrameSolution,
    PoseFrame,
    Residual,
    frame_solution_hash,
)

GENESIS_HASH = "GENESIS"
MANIFEST_SCHEMA = "motion-workbench/manifest/v1"
INTERVAL_SCHEMA = "motion-workbench/interval/v1"
PARAMETER_SCHEMA = "motion-workbench/parameters/v1"
CONFLICT_SCHEMA = "motion-workbench/conflict/v1"
ENGINE_VERSION = f"motion-workbench/{__version__}"

_registry_guard = threading.Lock()
_registry_paths: set[str] = set()


def _recovery_error(message: str, first_break_frame: int, **extra):
    details = {"first_break_frame": first_break_frame}
    details.update(extra)
    return WorkbenchError(E_RECOVERY_INVALID, message, details)


def _commit_error(message: str, stage: str, path: Path, **extra):
    details = {"stage": stage, "path": str(path)}
    details.update(extra)
    return WorkbenchError(E_COMMIT_FAILED, message, details)


def _fsync_directory(path: Path) -> None:
    if os.name == "nt":
        return
    try:
        descriptor = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(descriptor)
    except OSError:
        pass
    finally:
        os.close(descriptor)


class SolveLock:
    """Process-wide and OS-level exclusive lock for one BVH session."""

    def __init__(self, root: Path):
        self.root = Path(root)
        self.path = self.root / ".solve.lock"
        self._handle = None
        self._registry_key = str(self.path.resolve())
        self._acquired = False

    def acquire(self) -> "SolveLock":
        self.root.mkdir(parents=True, exist_ok=True)
        with _registry_guard:
            if self._registry_key in _registry_paths:
                raise WorkbenchError(
                    E_BUSY,
                    "A solve task is already active for this BVH session",
                    {"lock_path": str(self.path)},
                )
            _registry_paths.add(self._registry_key)
        try:
            handle = open(self.path, "a+b")
            if os.name == "nt":
                import msvcrt

                handle.seek(0, os.SEEK_END)
                if handle.tell() == 0:
                    handle.write(b" ")
                    handle.flush()
                handle.seek(0)
                try:
                    msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
                except OSError as exc:
                    raise WorkbenchError(
                        E_BUSY,
                        "A solve task is already active for this BVH session",
                        {"lock_path": str(self.path)},
                    ) from exc
            else:
                import fcntl

                try:
                    fcntl.flock(
                        handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB
                    )
                except OSError as exc:
                    raise WorkbenchError(
                        E_BUSY,
                        "A solve task is already active for this BVH session",
                        {"lock_path": str(self.path)},
                    ) from exc
            handle.seek(0)
            handle.write(
                canonical_json(
                    {
                        "pid": os.getpid(),
                        "engine": ENGINE_VERSION,
                        "platform": platform.platform(),
                    }
                ).encode("utf-8")
                + b"\n"
            )
            handle.flush()
            os.fsync(handle.fileno())
            self._handle = handle
            self._acquired = True
            return self
        except BaseException:
            with _registry_guard:
                _registry_paths.discard(self._registry_key)
            if "handle" in locals():
                handle.close()
            raise

    def release(self) -> None:
        if not self._acquired:
            return
        handle = self._handle
        try:
            if handle is not None:
                if os.name == "nt":
                    import msvcrt

                    try:
                        handle.seek(0)
                        msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
                    except OSError:
                        pass
                else:
                    import fcntl

                    try:
                        fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
                    except OSError:
                        pass
                handle.close()
        finally:
            self._handle = None
            self._acquired = False
            with _registry_guard:
                _registry_paths.discard(self._registry_key)

    def __enter__(self) -> "SolveLock":
        return self.acquire()

    def __exit__(self, exc_type, exc, traceback) -> None:
        self.release()


@dataclass(frozen=True)
class IntervalRecord:
    start: int
    end: int
    sequence: int
    table_version: int
    file: str
    content_hash: str
    previous_hash: str
    chain_hash: str
    parameters_version: str
    frames: tuple[FrameSolution, ...]


@dataclass(frozen=True)
class CommittedState:
    manifest: dict[str, Any]
    parameters: ConstraintSet
    parameters_version: str
    frame_map: dict[int, IntervalRecord]
    stale_frames: tuple[int, ...]
    committed_ranges: tuple[tuple[int, int], ...]
    next_frame: int
    missing_ranges: tuple[tuple[int, int], ...]
    chain_head: str
    table_version: int

    @property
    def committed_frames(self) -> set[int]:
        return set(self.frame_map)

    def frame_solution(self, frame: int) -> FrameSolution | None:
        record = self.frame_map.get(frame)
        if record is None:
            return None
        for solution in record.frames:
            if solution.frame == frame:
                return solution
        return None

    def to_summary(self) -> dict[str, Any]:
        return {
            "parameters_version": self.parameters_version,
            "table_version": self.table_version,
            "chain_head": self.chain_head,
            "committed_ranges": [list(pair) for pair in self.committed_ranges],
            "missing_ranges": [list(pair) for pair in self.missing_ranges],
            "next_frame": self.next_frame,
            "committed_frame_count": len(self.frame_map),
            "stale_frames": list(self.stale_frames[:50]),
            "stale_frame_count": len(self.stale_frames),
        }


def _ranges(frames: Sequence[int]) -> tuple[tuple[int, int], ...]:
    values = sorted(set(frames))
    if not values:
        return ()
    result: list[tuple[int, int]] = []
    start = previous = values[0]
    for frame in values[1:]:
        if frame == previous + 1:
            previous = frame
            continue
        result.append((start, previous))
        start = previous = frame
    result.append((start, previous))
    return tuple(result)


class AtomicStore:
    def __init__(
        self,
        root: str | Path,
        *,
        fault_hook: Callable[[str, Path, dict[str, Any] | None], None] | None = None,
        post_commit_hook: Callable[[dict[str, Any]], None] | None = None,
    ):
        self.root = Path(root)
        self.intervals_dir = self.root / "intervals"
        self.parameters_dir = self.root / "parameters"
        self.conflicts_dir = self.root / "conflicts"
        self.manifest_path = self.root / "manifest.json"
        self.source_path = self.root / "source.bvh"
        self.trace_path = self.root / "trace.jsonl"
        self.fault_hook = fault_hook
        self.post_commit_hook = post_commit_hook
        self._ensure_directories()

    def _ensure_directories(self) -> None:
        for path in (
            self.root,
            self.intervals_dir,
            self.parameters_dir,
            self.conflicts_dir,
        ):
            path.mkdir(parents=True, exist_ok=True)

    @property
    def lock_path(self) -> Path:
        return self.root / ".solve.lock"

    def solve_lock(self) -> SolveLock:
        return SolveLock(self.root)

    def _atomic_write_bytes(
        self, path: Path, data: bytes, stage: str, payload: dict[str, Any] | None = None
    ) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        temporary_name: str | None = None
        try:
            descriptor, temporary_name = tempfile.mkstemp(
                prefix=f".{path.name}.", suffix=".tmp", dir=str(path.parent)
            )
            with os.fdopen(descriptor, "wb") as handle:
                handle.write(data)
                handle.flush()
                os.fsync(handle.fileno())
            if self.fault_hook is not None:
                self.fault_hook(stage, path, payload)
            os.replace(temporary_name, path)
            temporary_name = None
            _fsync_directory(path.parent)
        except BaseException as exc:
            if temporary_name is not None:
                try:
                    os.unlink(temporary_name)
                except OSError:
                    pass
            if isinstance(exc, (KeyboardInterrupt, SystemExit)):
                raise
            if isinstance(exc, WorkbenchError) and exc.code == E_COMMIT_FAILED:
                raise
            raise _commit_error(
                "Atomic write failed; previous committed interval is unchanged",
                stage,
                path,
                reason=str(exc),
            ) from exc

    def _atomic_write_json(
        self, path: Path, value: dict[str, Any], stage: str
    ) -> None:
        data = (canonical_json(value) + "\n").encode("utf-8")
        self._atomic_write_bytes(path, data, stage, payload=value)

    def _read_json(self, path: Path, first_break_frame: int = 0) -> dict[str, Any]:
        try:
            return json.loads(path.read_text(encoding="utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise _recovery_error(
                "Committed JSON state is unreadable",
                first_break_frame,
                path=str(path),
                reason=str(exc),
            ) from exc

    def _parameters_version(
        self, bvh: BVHData, constraints: ConstraintSet
    ) -> str:
        return content_hash(
            {
                "engine": ENGINE_VERSION,
                "bvh_hash": bvh.content_digest,
                "constraints": constraints.to_dict(),
            }
        )

    def _parameter_payload(
        self, bvh: BVHData, constraints: ConstraintSet, version: str
    ) -> dict[str, Any]:
        return {
            "schema": PARAMETER_SCHEMA,
            "engine_version": ENGINE_VERSION,
            "version": version,
            "bvh_hash": bvh.content_digest,
            "constraints": constraints.to_dict(),
        }

    def ensure_source(self, bvh_bytes: bytes, bvh: BVHData) -> None:
        if self.source_path.exists():
            current = self.source_path.read_bytes()
            if sha256_bytes(current) == sha256_bytes(bvh_bytes):
                return
            from .bvh import load_bvh

            try:
                current_data = load_bvh(self.source_path)
            except WorkbenchError as exc:
                raise _recovery_error(
                    "Stored BVH is invalid",
                    0,
                    reason=exc.message,
                ) from exc
            if current_data.content_digest != bvh.content_digest:
                raise _recovery_error(
                    "Session BVH differs from the requested BVH",
                    0,
                    requested_hash=bvh.content_digest,
                    stored_hash=current_data.content_digest,
                )
            return
        self._atomic_write_bytes(
            self.source_path, bvh_bytes, "source_bvh"
        )

    def initialize(
        self, bvh_bytes: bytes, bvh: BVHData, constraints: ConstraintSet
    ) -> tuple[dict[str, Any], str]:
        self.ensure_source(bvh_bytes, bvh)
        version = self._parameters_version(bvh, constraints)
        payload = self._parameter_payload(bvh, constraints, version)
        recomputed = content_hash(
            {
                "engine": ENGINE_VERSION,
                "bvh_hash": bvh.content_digest,
                "constraints": constraints.to_dict(),
            }
        )
        if recomputed != version:
            raise _recovery_error("Parameter version self-check failed", 0)
        parameter_path = self.parameters_dir / f"{version}.json"
        if parameter_path.exists():
            existing = self._read_json(parameter_path)
            if existing != payload:
                raise _recovery_error(
                    "Parameter version collision", 0, path=str(parameter_path)
                )
        else:
            self._atomic_write_json(
                parameter_path, payload, "parameters"
            )

        if self.manifest_path.exists():
            manifest = self._read_json(self.manifest_path)
            if manifest.get("bvh_hash") != bvh.content_digest:
                raise _recovery_error(
                    "Manifest BVH hash does not match session",
                    0,
                )
            return manifest, version

        session_id = content_hash(
            {
                "schema": "motion-workbench/session/v1",
                "engine": ENGINE_VERSION,
                "bvh_hash": bvh.content_digest,
            }
        )[:24]
        manifest = {
            "schema": MANIFEST_SCHEMA,
            "session_id": session_id,
            "engine_version": ENGINE_VERSION,
            "bvh_hash": bvh.content_digest,
            "parameters_version": version,
            "parameters_file": f"parameters/{version}.json",
            "table_version": 1,
            "next_sequence": 1,
            "chain_head": GENESIS_HASH,
            "parameter_updates": [
                {
                    "table_version": 1,
                    "from_version": None,
                    "to_version": version,
                }
            ],
            "commits": [],
            "barriers": [],
        }
        self._atomic_write_json(self.manifest_path, manifest, "manifest")
        self._post_commit(manifest)
        return manifest, version

    def read_manifest(self) -> dict[str, Any] | None:
        if not self.manifest_path.exists():
            return None
        return self._read_json(self.manifest_path)

    def load_parameters(
        self, bvh: BVHData, manifest: dict[str, Any] | None = None
    ) -> tuple[ConstraintSet, str]:
        from .constraints import parse_constraints

        manifest = manifest or self.read_manifest()
        if manifest is None:
            raise _recovery_error("No committed manifest exists", 0)
        if not isinstance(manifest, dict):
            raise _recovery_error("Manifest root must be an object", 0)
        version = manifest.get("parameters_version")
        relative = manifest.get("parameters_file")
        if not isinstance(version, str) or not isinstance(relative, str):
            raise _recovery_error(
                "Manifest lacks parameter snapshot fields", 0
            )
        path = self.root / relative
        payload = self._read_json(path)
        if not isinstance(payload, dict):
            raise _recovery_error(
                "Parameter snapshot root must be an object", 0, path=str(path)
            )
        if payload.get("schema") != PARAMETER_SCHEMA:
            raise _recovery_error(
                "Parameter snapshot schema is invalid", 0, path=str(path)
            )
        if payload.get("version") != version:
            raise _recovery_error(
                "Parameter snapshot version does not match manifest",
                0,
                path=str(path),
            )
        if payload.get("engine_version") != ENGINE_VERSION:
            raise _recovery_error(
                "Parameter snapshot engine version does not match",
                0,
                path=str(path),
            )
        if payload.get("bvh_hash") != bvh.content_digest:
            raise _recovery_error(
                "Parameter snapshot BVH hash does not match", 0
            )
        recomputed = content_hash(
            {
                "engine": payload.get("engine_version"),
                "bvh_hash": payload.get("bvh_hash"),
                "constraints": payload.get("constraints"),
            }
        )
        if recomputed != version:
            raise _recovery_error(
                "Parameter snapshot content hash is invalid", 0
            )
        try:
            constraints = parse_constraints(payload["constraints"], bvh)
        except WorkbenchError as exc:
            raise _recovery_error(
                "Committed parameter snapshot is invalid",
                0,
                constraint_error=exc.to_dict(),
            ) from exc
        return constraints, version

    def set_parameters(
        self, bvh: BVHData, constraints: ConstraintSet
    ) -> tuple[dict[str, Any], str]:
        manifest = self.read_manifest()
        if manifest is None:
            raise _recovery_error("No committed manifest exists", 0)
        version = self._parameters_version(bvh, constraints)
        if version == manifest.get("parameters_version"):
            return manifest, version
        payload = self._parameter_payload(bvh, constraints, version)
        parameter_path = self.parameters_dir / f"{version}.json"
        if parameter_path.exists():
            existing = self._read_json(parameter_path)
            if existing != payload:
                raise _recovery_error(
                    "Parameter version collision", 0, path=str(parameter_path)
                )
        else:
            self._atomic_write_json(parameter_path, payload, "parameters")
        updated = copy.deepcopy(manifest)
        next_table_version = int(manifest["table_version"]) + 1
        updated["table_version"] = next_table_version
        updated["parameters_version"] = version
        updated["parameters_file"] = f"parameters/{version}.json"
        updated["parameter_updates"].append(
            {
                "table_version": next_table_version,
                "from_version": manifest.get("parameters_version"),
                "to_version": version,
            }
        )
        self._atomic_write_json(self.manifest_path, updated, "manifest")
        self._post_commit(updated)
        return updated, version

    def _post_commit(self, manifest: dict[str, Any]) -> None:
        if self.post_commit_hook is not None:
            self.post_commit_hook(manifest)

    def commit_interval(
        self,
        frames: Sequence[FrameSolution],
        bvh: BVHData,
        parameters_version: str,
    ) -> IntervalRecord:
        if not frames:
            raise ValueError("Cannot commit an empty interval")
        ordered = tuple(sorted(frames, key=lambda item: item.frame))
        start = ordered[0].frame
        end = ordered[-1].frame
        if [item.frame for item in ordered] != list(range(start, end + 1)):
            raise ValueError("Committed interval frames must be contiguous")
        if any(item.conflict is not None or not item.converged for item in ordered):
            raise ValueError("Conflicted or unconverged frames cannot be committed")
        manifest = self.read_manifest()
        if manifest is None:
            raise _recovery_error("No committed manifest exists", start)
        if manifest.get("parameters_version") != parameters_version:
            raise _recovery_error(
                "Solve parameters do not match the manifest",
                start,
            )
        if manifest.get("bvh_hash") != bvh.content_digest:
            raise _recovery_error("BVH hash does not match the manifest", start)
        sequence = int(manifest.get("next_sequence", len(manifest["commits"]) + 1))
        table_version = int(manifest["table_version"]) + 1
        base_payload = {
            "schema": INTERVAL_SCHEMA,
            "interval_id": f"v{sequence:06d}_f{start:06d}-f{end:06d}",
            "sequence": sequence,
            "table_version": table_version,
            "start": start,
            "end": end,
            "parameters_version": parameters_version,
            "bvh_hash": bvh.content_digest,
            "frames": [item.to_dict() for item in ordered],
        }
        interval_content = content_hash(base_payload)
        filename = (
            f"v{sequence:06d}_f{start:06d}-f{end:06d}_"
            f"{interval_content[:12]}.json"
        )
        relative = f"intervals/{filename}"
        path = self.root / relative
        previous_hash = str(manifest["chain_head"])
        interval_chain = chain_hash(
            previous_hash,
            interval_content,
            parameters_version,
            table_version,
            start,
            end,
        )
        payload = dict(base_payload)
        payload["content_hash"] = interval_content
        payload["previous_hash"] = previous_hash
        payload["chain_hash"] = interval_chain
        if path.exists():
            existing = self._read_json(path, start)
            if existing.get("content_hash") != interval_content:
                raise _commit_error(
                    "An immutable interval file with this version already exists",
                    "interval",
                    path,
                )
            if existing.get("chain_hash") != interval_chain:
                raise _commit_error(
                    "An orphan interval with the same version has a different chain",
                    "interval",
                    path,
                )
        else:
            self._atomic_write_json(path, payload, "interval")

        updated = copy.deepcopy(manifest)
        updated["table_version"] = table_version
        updated["next_sequence"] = sequence + 1
        updated["chain_head"] = interval_chain
        updated["commits"].append(
            {
                "sequence": sequence,
                "table_version": table_version,
                "start": start,
                "end": end,
                "file": relative,
                "content_hash": interval_content,
                "previous_hash": previous_hash,
                "chain_hash": interval_chain,
                "parameters_version": parameters_version,
                "bvh_hash": bvh.content_digest,
            }
        )
        self._atomic_write_json(self.manifest_path, updated, "manifest")
        self._post_commit(updated)
        record = IntervalRecord(
            start=start,
            end=end,
            sequence=sequence,
            table_version=table_version,
            file=relative,
            content_hash=interval_content,
            previous_hash=previous_hash,
            chain_hash=interval_chain,
            parameters_version=parameters_version,
            frames=ordered,
        )
        return record

    def record_conflict(
        self,
        conflict: ConflictDiagnostic,
        table_version_after: int | None = None,
    ) -> dict[str, Any]:
        manifest = self.read_manifest()
        if manifest is None:
            raise _recovery_error("No committed manifest exists", conflict.frame)
        payload = conflict.to_dict()
        payload["report_hash"] = content_hash(payload)
        filename = (
            f"frame_{conflict.frame:06d}_{payload['report_hash'][:12]}.json"
        )
        relative = f"conflicts/{filename}"
        path = self.root / relative
        if not path.exists():
            self._atomic_write_json(path, payload, "conflict_report")
        next_table_version = (
            int(manifest["table_version"]) + 1
            if table_version_after is None
            else int(table_version_after)
        )
        updated = copy.deepcopy(manifest)
        updated["table_version"] = next_table_version
        updated["barriers"].append(
            {
                "table_version": next_table_version,
                "frame": conflict.frame,
                "report_file": relative,
                "report_hash": payload["report_hash"],
                "reason": "constraint_conflict",
            }
        )
        self._atomic_write_json(self.manifest_path, updated, "manifest")
        self._post_commit(updated)
        return {
            "report_file": relative,
            "report_hash": payload["report_hash"],
            "barrier_frame": conflict.frame,
            "table_version": next_table_version,
        }

    def load_committed(
        self,
        bvh: BVHData,
        *,
        constraints: ConstraintSet | None = None,
        expected_parameters_version: str | None = None,
        expected_table_version: int | None = None,
    ) -> CommittedState:
        manifest = self.read_manifest()
        if manifest is None:
            raise _recovery_error("No committed manifest exists", 0)
        first_active = 0
        if not isinstance(manifest, dict):
            raise _recovery_error("Manifest root must be an object", first_active)
        for field_name in ("parameter_updates", "commits", "barriers"):
            if not isinstance(manifest.get(field_name, []), list):
                raise _recovery_error(
                    f"Manifest field {field_name} must be a list",
                    first_active,
                )
        if manifest.get("schema") != MANIFEST_SCHEMA:
            raise _recovery_error(
                "Manifest schema is invalid", first_active
            )
        if manifest.get("engine_version") != ENGINE_VERSION:
            raise _recovery_error(
                "Manifest engine version does not match",
                first_active,
            )
        if manifest.get("bvh_hash") != bvh.content_digest:
            raise _recovery_error(
                "Manifest BVH hash does not match the source BVH",
                first_active,
            )
        if (
            expected_parameters_version is not None
            and manifest.get("parameters_version") != expected_parameters_version
        ):
            raise _recovery_error(
                "Snapshot parameter version does not match manifest",
                first_active,
                expected_parameters_version=expected_parameters_version,
                actual_parameters_version=manifest.get("parameters_version"),
            )
        try:
            manifest_table_version = int(manifest.get("table_version", -1))
        except (TypeError, ValueError) as exc:
            raise _recovery_error(
                "Manifest table version is invalid", first_active
            ) from exc
        if (
            expected_table_version is not None
            and manifest_table_version != expected_table_version
        ):
            raise _recovery_error(
                "Snapshot interval table version does not match manifest",
                first_active,
                expected_table_version=expected_table_version,
                actual_table_version=manifest.get("table_version"),
            )
        committed, parameters_version = self.load_parameters(bvh, manifest)
        if constraints is not None and constraints.version != committed.version:
            # This is not an error for incremental edits; the caller will install
            # a new parameter version before solving. The committed snapshot is
            # still required to be self-consistent.
            pass

        events: list[tuple[int, int, str]] = []
        parameter_versions: set[str] = set()
        for event in manifest.get("parameter_updates", []):
            try:
                version = int(event["table_version"])
            except (KeyError, TypeError, ValueError) as exc:
                raise _recovery_error(
                    "Parameter update table version is invalid", first_active
                ) from exc
            events.append((version, 0, "parameter"))
            if isinstance(event.get("to_version"), str):
                parameter_versions.add(event["to_version"])
        for commit in manifest.get("commits", []):
            try:
                version = int(commit["table_version"])
                start = int(commit["start"])
            except (KeyError, TypeError, ValueError) as exc:
                raise _recovery_error(
                    "Commit table version is invalid", first_active
                ) from exc
            events.append((version, start, "commit"))
            if isinstance(commit.get("parameters_version"), str):
                parameter_versions.add(commit["parameters_version"])
        for barrier in manifest.get("barriers", []):
            try:
                version = int(barrier["table_version"])
                frame = int(barrier["frame"])
            except (KeyError, TypeError, ValueError) as exc:
                raise _recovery_error(
                    "Barrier table version is invalid", first_active
                ) from exc
            events.append((version, frame, "barrier"))
            relative = barrier.get("report_file")
            if not isinstance(relative, str):
                raise _recovery_error(
                    "Conflict barrier lacks a report file", frame
                )
            report = self._read_json(self.root / relative, frame)
            if not isinstance(report, dict):
                raise _recovery_error(
                    "Conflict report root must be an object", frame
                )
            report_body = {
                key: value
                for key, value in report.items()
                if key != "report_hash"
            }
            recomputed_report_hash = content_hash(report_body)
            if recomputed_report_hash != report.get("report_hash"):
                raise _recovery_error(
                    "Conflict report hash mismatch", frame
                )
            if report.get("report_hash") != barrier.get("report_hash"):
                raise _recovery_error(
                    "Conflict report hash does not match barrier", frame
                )
            if report.get("frame") != frame:
                raise _recovery_error(
                    "Conflict report frame does not match barrier", frame
                )
        table_version = manifest_table_version
        events.sort(key=lambda item: item[0])
        if not events:
            raise _recovery_error("Manifest has no table events", first_active)
        seen_table_versions: set[int] = set()
        for expected, (version, frame, kind) in enumerate(events, start=1):
            if version in seen_table_versions:
                raise _recovery_error(
                    "Duplicate interval table version",
                    frame if kind != "parameter" else first_active,
                    table_version=version,
                )
            seen_table_versions.add(version)
            if version != expected:
                raise _recovery_error(
                    "Interval table version sequence is broken",
                    frame if kind != "parameter" else first_active,
                    expected_table_version=expected,
                    actual_table_version=version,
                )
        if table_version != max(seen_table_versions):
            raise _recovery_error(
                "Manifest interval table version does not match its events",
                first_active,
                manifest_table_version=table_version,
                event_table_version=max(seen_table_versions),
            )

        try:
            commits = sorted(
                manifest.get("commits", []),
                key=lambda item: (
                    int(item["table_version"]),
                    int(item["sequence"]),
                ),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise _recovery_error(
                "Commit ordering metadata is invalid",
                first_active,
            ) from exc
        running_hash = GENESIS_HASH
        records: list[tuple[dict[str, Any], IntervalRecord]] = []
        expected_sequence = 1
        for commit in commits:
            start = int(commit["start"])
            end = int(commit["end"])
            if end < start or start < 0 or end >= bvh.frame_count:
                raise _recovery_error(
                    "Committed interval frame range is invalid",
                    start,
                    start=start,
                    end=end,
                    frame_count=bvh.frame_count,
                )
            try:
                commit_sequence = int(commit["sequence"])
            except (KeyError, TypeError, ValueError) as exc:
                raise _recovery_error(
                    "Interval sequence is invalid", start
                ) from exc
            if commit_sequence != expected_sequence:
                raise _recovery_error(
                    "Interval sequence is broken",
                    start,
                    expected_sequence=expected_sequence,
                    actual_sequence=commit["sequence"],
                )
            expected_sequence += 1
            if commit.get("previous_hash") != running_hash:
                raise _recovery_error(
                    "Hash chain predecessor mismatch",
                    start,
                    expected_previous_hash=running_hash,
                    actual_previous_hash=commit.get("previous_hash"),
                )
            relative = str(commit.get("file", ""))
            path = self.root / relative
            payload = self._read_json(path, start)
            if not isinstance(payload, dict):
                raise _recovery_error(
                    "Interval root must be an object", start, path=str(path)
                )
            base_payload = {
                key: payload.get(key)
                for key in (
                    "schema",
                    "interval_id",
                    "sequence",
                    "table_version",
                    "start",
                    "end",
                    "parameters_version",
                    "bvh_hash",
                    "frames",
                )
            }
            if base_payload.get("schema") != INTERVAL_SCHEMA:
                raise _recovery_error(
                    "Interval schema is invalid", start, path=str(path)
                )
            interval_content = content_hash(base_payload)
            if interval_content != payload.get("content_hash"):
                raise _recovery_error(
                    "Interval content hash mismatch", start, path=str(path)
                )
            if interval_content != commit.get("content_hash"):
                raise _recovery_error(
                    "Interval content hash does not match manifest",
                    start,
                    path=str(path),
                )
            expected_chain = chain_hash(
                running_hash,
                interval_content,
                str(commit.get("parameters_version")),
                int(commit["table_version"]),
                start,
                end,
            )
            if expected_chain != payload.get("chain_hash"):
                raise _recovery_error(
                    "Interval hash chain mismatch", start, path=str(path)
                )
            if payload.get("previous_hash") != running_hash:
                raise _recovery_error(
                    "Interval predecessor hash mismatch",
                    start,
                    path=str(path),
                )
            if expected_chain != commit.get("chain_hash"):
                raise _recovery_error(
                    "Interval chain hash does not match manifest",
                    start,
                    path=str(path),
                )
            for field in (
                "sequence",
                "table_version",
                "start",
                "end",
                "parameters_version",
                "bvh_hash",
            ):
                if payload.get(field) != commit.get(field):
                    raise _recovery_error(
                        f"Interval field {field} does not match manifest",
                        start,
                        field=field,
                    )
            if payload.get("bvh_hash") != bvh.content_digest:
                raise _recovery_error(
                    "Interval BVH hash does not match source", start
                )
            if payload.get("parameters_version") not in parameter_versions:
                raise _recovery_error(
                    "Interval parameter version is absent from history",
                    start,
                    parameters_version=payload.get("parameters_version"),
                )
            raw_frames = payload.get("frames")
            if not isinstance(raw_frames, list) or len(raw_frames) != end - start + 1:
                raise _recovery_error(
                    "Interval frame payload is incomplete", start
                )
            parsed_frames: list[FrameSolution] = []
            for offset, raw_frame in enumerate(raw_frames):
                frame_number = start + offset
                try:
                    solution = FrameSolution.from_dict(raw_frame)
                except (KeyError, TypeError, ValueError) as exc:
                    raise _recovery_error(
                        "Interval frame payload is malformed",
                        frame_number,
                        reason=str(exc),
                    ) from exc
                if solution.frame != frame_number:
                    raise _recovery_error(
                        "Interval frame number is out of sequence",
                        frame_number,
                        stored_frame=solution.frame,
                    )
                if solution.conflict is not None or not solution.converged:
                    raise _recovery_error(
                        "Conflicted or unconverged frame is present in a commit",
                        frame_number,
                    )
                recomputed_frame_hash = frame_solution_hash(solution)
                if recomputed_frame_hash != solution.frame_hash:
                    raise _recovery_error(
                        "Frame content hash mismatch",
                        frame_number,
                    )
                parsed_frames.append(solution)
            record = IntervalRecord(
                start=start,
                end=end,
                sequence=int(commit["sequence"]),
                table_version=int(commit["table_version"]),
                file=relative,
                content_hash=interval_content,
                previous_hash=str(commit["previous_hash"]),
                chain_hash=expected_chain,
                parameters_version=str(commit["parameters_version"]),
                frames=tuple(parsed_frames),
            )
            records.append((commit, record))
            running_hash = expected_chain
        if manifest.get("chain_head") != running_hash:
            raise _recovery_error(
                "Manifest chain head mismatch",
                commits[-1]["start"] if commits else 0,
                expected_chain_head=running_hash,
                manifest_chain_head=manifest.get("chain_head"),
            )
        expected_next_sequence = expected_sequence
        if "next_sequence" in manifest and int(manifest["next_sequence"]) != expected_next_sequence:
            raise _recovery_error(
                "Manifest next interval sequence is inconsistent",
                commits[-1]["start"] if commits else 0,
                expected_next_sequence=expected_next_sequence,
                manifest_next_sequence=manifest["next_sequence"],
            )

        try:
            barriers = sorted(
                (
                    (int(item["table_version"]), int(item["frame"]), item)
                    for item in manifest.get("barriers", [])
                ),
                key=lambda item: item[0],
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise _recovery_error(
                "Barrier metadata is invalid", first_active
            ) from exc
        frame_map: dict[int, IntervalRecord] = {}
        for commit, record in records:
            commit_tv = record.table_version
            for frame in range(record.start, record.end + 1):
                invalidated = any(
                    barrier_tv > commit_tv and frame >= barrier_frame
                    for barrier_tv, barrier_frame, _ in barriers
                )
                if not invalidated:
                    frame_map[frame] = record

        dependency_constraints = constraints or committed
        stale_frames: list[int] = []
        for frame, record in sorted(frame_map.items()):
            solution = next(
                (item for item in record.frames if item.frame == frame), None
            )
            if solution is None:
                stale_frames.append(frame)
                continue
            expected_dependency = dependency_constraints.frame_dependency_hash(
                bvh, frame
            )
            if solution.dependency_hash != expected_dependency:
                stale_frames.append(frame)

        committed_ranges = _ranges(list(frame_map))
        all_frames = set(range(bvh.frame_count))
        missing = all_frames - set(frame_map)
        next_frame = min(missing) if missing else bvh.frame_count
        return CommittedState(
            manifest=manifest,
            parameters=committed,
            parameters_version=parameters_version,
            frame_map=frame_map,
            stale_frames=tuple(stale_frames),
            committed_ranges=committed_ranges,
            next_frame=next_frame,
            missing_ranges=_ranges(list(missing)),
            chain_head=running_hash,
            table_version=table_version,
        )

    def append_trace(self, events: Sequence[dict[str, Any]]) -> None:
        if not events:
            return
        lines = [canonical_json(event) for event in events]
        data = ("\n".join(lines) + "\n").encode("utf-8")
        with open(self.trace_path, "ab") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())

    def read_trace(self) -> list[dict[str, Any]]:
        if not self.trace_path.exists():
            return []
        result: list[dict[str, Any]] = []
        for line in self.trace_path.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            try:
                result.append(json.loads(line))
            except json.JSONDecodeError:
                break
        return result

    def read_source(self) -> bytes:
        try:
            return self.source_path.read_bytes()
        except OSError as exc:
            raise _recovery_error(
                "Committed source BVH is missing",
                0,
                path=str(self.source_path),
                reason=str(exc),
            ) from exc
