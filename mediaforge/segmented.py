from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from . import ALGORITHM_VERSION
from .checkpoint import CheckpointManager, checkpoint_signature
from .errors import (
    E_BUFFER,
    E_CORRUPT,
    E_IO,
    E_PARAM,
    E_TRACK,
    LossyWarning,
    WorkbenchError,
    fail,
)
from .injection import InjectionConfig
from .jsonutil import atomic_write_bytes, sha256_bytes
from .metadata import normalize_target_version, warnings_as_dicts
from .model import ContainerModel, validate_model
from .package import (
    PackageResult,
    build_package_from_segments,
    parse_segment_bytes,
    serialize_segment,
)
from .validator import ValidationResult, validate_artifact


MAX_ENGINE_WORKERS = 4


@dataclass(frozen=True)
class SegmentFailure:
    track_id: str
    index: int
    code: str
    field_path: str
    message: str
    measured: Any = None
    declared: Any = None
    details: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        value = {
            "track_id": self.track_id,
            "index": self.index,
            "code": self.code,
            "field_path": self.field_path,
            "message": self.message,
        }
        if self.measured is not None:
            value["measured"] = self.measured
        if self.declared is not None:
            value["declared"] = self.declared
        if self.details:
            value["details"] = self.details
        return value


@dataclass
class SegmentedResult:
    ok: bool
    snapshot_id: str
    target_version: str
    artifact: bytes | None
    artifact_sha256: str
    warnings: tuple[LossyWarning, ...]
    trace: tuple[dict[str, Any], ...]
    declaration: dict[str, Any] | None
    failures: tuple[SegmentFailure, ...]
    reused_tracks: tuple[str, ...]
    built_tracks: tuple[str, ...]
    invalidated_snapshot: bool
    effective_workers: int
    validation: ValidationResult | None
    snapshot_state: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": self.ok,
            "snapshot_id": self.snapshot_id,
            "target_version": self.target_version,
            "artifact_sha256": self.artifact_sha256,
            "warnings": warnings_as_dicts(self.warnings),
            "trace": list(self.trace),
            "failures": [failure.to_dict() for failure in self.failures],
            "reused_tracks": list(self.reused_tracks),
            "built_tracks": list(self.built_tracks),
            "invalidated_snapshot": self.invalidated_snapshot,
            "effective_workers": self.effective_workers,
            "validation": self.validation.to_dict() if self.validation else None,
            "snapshot_state": self.snapshot_state,
        }


def _segment_bytes_for(
    model: ContainerModel,
    track_id: str,
    target: str,
    parameter_version: str,
    algorithm_version: str,
    per_thread_buffer_limit: int,
    injection: InjectionConfig | None,
) -> bytes:
    track = model.track_by_id(track_id)
    if track is None:
        fail(E_TRACK, f"$.tracks.{track_id}", "track is missing from the model")
    data = serialize_segment(
        track,
        model,
        target,
        parameter_version,
        algorithm_version=algorithm_version,
    )
    if len(data) > per_thread_buffer_limit:
        fail(
            E_BUFFER,
            f"$.segments.{track_id}.buffer",
            "segment exceeds the per-thread buffer limit",
            measured=len(data),
            declared=per_thread_buffer_limit,
        )
    if injection and injection.corrupt_track == track_id:
        if not data:
            fail(E_CORRUPT, f"$.segments.{track_id}", "segment is empty")
        corrupted = bytearray(data)
        position = injection.corrupt_offset % len(corrupted)
        corrupted[position] ^= 0x5A
        corrupted_bytes = bytes(corrupted)
        try:
            parse_segment_bytes(
                corrupted_bytes,
                expected_track_id=track_id,
                expected_metadata_version=target,
                expected_source_version=model.source_version,
                expected_parameter_version=parameter_version,
                expected_algorithm_version=algorithm_version,
            )
        except WorkbenchError:
            raise
        fail(
            E_CORRUPT,
            f"$.segments.{track_id}",
            "injected segment corruption was not detected",
        )
    parse_segment_bytes(
        data,
        expected_track_id=track_id,
        expected_metadata_version=target,
        expected_source_version=model.source_version,
        expected_parameter_version=parameter_version,
        expected_algorithm_version=algorithm_version,
    )
    return data


def pack_segmented(
    model: ContainerModel,
    target_version: str,
    checkpoint_root: str | Path,
    *,
    workers: int = 2,
    per_thread_buffer_limit: int = 4 * 1024 * 1024,
    parameter_version: str = "params-default",
    algorithm_version: str = ALGORITHM_VERSION,
    injection: InjectionConfig | None = None,
    artifact_path: str | Path | None = None,
    truth_path: str | Path | None = None,
) -> SegmentedResult:
    target = normalize_target_version(target_version)
    validate_model(model, require_payload=True)
    if artifact_path is not None:
        try:
            Path(artifact_path).unlink(missing_ok=True)
        except OSError as exc:
            fail(E_IO, "$.artifact", f"cannot clear a stale artifact: {exc}")
    if isinstance(workers, bool) or not isinstance(workers, int) or workers <= 0:
        fail(E_PARAM, "$.workers", "worker count must be a positive integer", measured=workers)
    if (
        isinstance(per_thread_buffer_limit, bool)
        or not isinstance(per_thread_buffer_limit, int)
        or per_thread_buffer_limit <= 0
    ):
        fail(
            E_BUFFER,
            "$.per_thread_buffer_limit",
            "per-thread buffer limit must be positive",
            measured=per_thread_buffer_limit,
        )
    effective_workers = min(workers, MAX_ENGINE_WORKERS)
    signature = checkpoint_signature(
        model,
        target,
        parameter_version,
        algorithm_version,
        per_thread_buffer_limit,
    )
    manager = CheckpointManager(
        checkpoint_root,
        request_signature=signature,
        metadata_version=target,
        source_version=model.source_version,
        parameter_version=parameter_version,
        algorithm_version=algorithm_version,
        per_thread_buffer_limit=per_thread_buffer_limit,
        track_ids=[track.track_id for track in model.tracks],
    )
    if injection and injection.corrupt_track:
        manager.force_pending(injection.corrupt_track)
    existing = manager.completed_segments()
    reused = tuple(sorted(existing))
    pending = [
        track.track_id
        for track in sorted(model.tracks, key=lambda item: item.track_id)
        if track.track_id not in existing
    ]
    built: list[str] = []
    failures: list[SegmentFailure] = []

    def worker(track_id: str) -> tuple[str, bytes | None, WorkbenchError | None]:
        try:
            return (
                track_id,
                _segment_bytes_for(
                    model,
                    track_id,
                    target,
                    parameter_version,
                    algorithm_version,
                    per_thread_buffer_limit,
                    injection,
                ),
                None,
            )
        except WorkbenchError as exc:
            return track_id, None, exc
        except Exception as exc:
            error = WorkbenchError(
                code=E_CORRUPT,
                field_path=f"$.segments.{track_id}",
                message=f"unexpected segment failure: {exc}",
            )
            return track_id, None, error

    if pending:
        with ThreadPoolExecutor(max_workers=min(effective_workers, len(pending))) as executor:
            future_map = {executor.submit(worker, track_id): track_id for track_id in pending}
            for future in as_completed(future_map):
                track_id, data, error = future.result()
                if error is not None:
                    manager.record_failure(track_id, error)
                    failures.append(
                        SegmentFailure(
                            track_id=track_id,
                            index=manager.snapshot()["segments"][track_id]["index"],
                            code=error.code,
                            field_path=error.field_path,
                            message=error.message,
                            measured=error.measured,
                            declared=error.declared,
                            details=dict(error.details),
                        )
                    )
                    continue
                assert data is not None
                manager.record_success(track_id, data)
                built.append(track_id)

    snapshot = manager.snapshot()
    failures.sort(key=lambda item: item.index)
    rebuilt_segments = manager.completed_segments() if not failures else {}
    metadata_export = None
    artifact: bytes | None = None
    artifact_sha256 = ""
    declaration: dict[str, Any] | None = None
    validation: ValidationResult | None = None
    warnings: tuple[LossyWarning, ...] = ()
    trace: tuple[dict[str, Any], ...] = ()
    if not failures:
        package: PackageResult = build_package_from_segments(
            model,
            target,
            rebuilt_segments,
            algorithm_version=algorithm_version,
        )
        artifact = package.artifact
        artifact_sha256 = package.artifact_sha256
        declaration = package.declaration
        warnings = package.warnings
        trace = package.trace
        if artifact_path is not None:
            try:
                atomic_write_bytes(artifact_path, artifact)
            except OSError as exc:
                fail(E_IO, "$.artifact", f"cannot write package: {exc}")
            if truth_path is not None:
                validation = validate_artifact(artifact_path, truth_path)
                if not validation.ok:
                    Path(artifact_path).unlink(missing_ok=True)
                    fail(
                        E_CORRUPT,
                        validation.error.get("field_path", "$") if validation.error else "$",
                        "independent validation failed after writing the package",
                        measured=validation.error.get("measured") if validation.error else None,
                        declared=validation.error.get("declared") if validation.error else None,
                    )
        elif truth_path is not None:
            validation = _validate_bytes(artifact, truth_path)
            if not validation.ok:
                fail(
                    E_CORRUPT,
                    validation.error.get("field_path", "$") if validation.error else "$",
                    "independent validation failed",
                )
    snapshot = manager.snapshot()
    return SegmentedResult(
        ok=not failures,
        snapshot_id=manager.snapshot_id,
        target_version=target,
        artifact=artifact,
        artifact_sha256=artifact_sha256,
        warnings=warnings,
        trace=trace,
        declaration=declaration,
        failures=tuple(failures),
        reused_tracks=reused,
        built_tracks=tuple(sorted(built)),
        invalidated_snapshot=manager.invalidated,
        effective_workers=effective_workers,
        validation=validation,
        snapshot_state=str(snapshot.get("state", "unknown")),
    )


def _validate_bytes(artifact: bytes, truth_path: str | Path) -> ValidationResult:
    import json
    import tempfile

    with tempfile.NamedTemporaryFile(prefix="mfz-validate-", suffix=".mfz", delete=False) as handle:
        handle.write(artifact)
        temp_path = handle.name
    try:
        return validate_artifact(temp_path, truth_path)
    finally:
        Path(temp_path).unlink(missing_ok=True)
