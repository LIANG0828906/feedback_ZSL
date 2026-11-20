from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import threading
from collections.abc import Callable, Iterable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .color import build_intermediate_summary, convert_frame
from .errors import ErrorCode, HDRError
from .formats import HDRHeader, HDRSequence
from .model import (
    ConversionParams,
    DatasetInfo,
    PreviewState,
    ToneMap,
    VersionDiff,
    VersionRecord,
    changed_frame_indices,
    compare_param_dicts,
    frames_in_intervals,
)
from .util import (
    append_json_line,
    atomic_write_bytes,
    atomic_write_json,
    canonical_bytes,
    fsync_dir,
    hash_file,
    intervals_from_indices,
    relative_to_workspace,
    sha256_hex,
)

SNAPSHOT_SCHEMA_VERSION = 1
_VERSION_DIR_RE = re.compile(r"^v(\d{6})$")
_COMMIT_KEYS = {
    "target_color_space",
    "target_transfer",
    "peak_luminance",
    "tone_map",
}


@dataclass(frozen=True, slots=True)
class RecoveryReport:
    restored_version: int
    recomputed_frames: dict[int, tuple[int, ...]]
    discarded_versions: tuple[int, ...]

    def to_dict(self) -> dict[str, Any]:
        return {
            "restored_version": self.restored_version,
            "recomputed_frames": {
                str(version): list(frames)
                for version, frames in sorted(self.recomputed_frames.items())
            },
            "discarded_versions": list(self.discarded_versions),
        }


class ConversionSession:
    """A single-source conversion session with immutable published versions."""

    def __init__(self, workspace: Path | str, source_path: Path | str | None = None) -> None:
        self.workspace = Path(workspace)
        self.workspace.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._busy = False
        self._active_cancel: threading.Event | None = None
        self._versions: list[VersionRecord] = []
        self._preview = PreviewState()
        self._recovery_report: RecoveryReport | None = None
        self.snapshot_path = self.workspace / "snapshots" / "current.json"
        self.trace_path = self.workspace / "trace" / "trace.jsonl"
        self.versions_dir = self.workspace / "versions"
        self.versions_dir.mkdir(parents=True, exist_ok=True)
        self._source_hint = Path(source_path) if source_path is not None else None
        self._loader: HDRSequence | None = None
        self.dataset: DatasetInfo | None = None

        if self.snapshot_path.exists():
            self._restore_from_snapshot()
        else:
            if source_path is None:
                raise HDRError(
                    ErrorCode.RECOVERY_INVALID,
                    "workspace has no snapshot and no source sequence was supplied",
                )
            self._initialize_new_workspace(Path(source_path))

    @property
    def header(self) -> HDRHeader:
        assert self._loader is not None
        return self._loader.header

    @property
    def source_path(self) -> Path:
        assert self._loader is not None
        return self._loader.path

    @property
    def recovery_report(self) -> RecoveryReport | None:
        return self._recovery_report

    def _initialize_new_workspace(self, source_path: Path) -> None:
        loader = HDRSequence(source_path)
        file_hash = hash_file(loader.path)
        relative = relative_to_workspace(loader.path, self.workspace)
        self.dataset = DatasetInfo(
            relative_path=relative,
            file_hash=file_hash,
            header_hash=loader.header.header_hash,
            width=loader.header.width,
            height=loader.header.height,
            frame_count=loader.header.frame_count,
            bit_depth=loader.header.bit_depth,
            color_space=loader.header.color_space,
            transfer=loader.header.transfer,
            peak_luminance=loader.header.peak_luminance,
        )
        self._loader = loader
        self._cleanup_staging()

    @classmethod
    def open(
        cls, workspace: Path | str, source_path: Path | str | None = None
    ) -> ConversionSession:
        return cls(workspace, source_path)

    def _recovery_error(self, message: str, **details: Any) -> HDRError:
        return HDRError(ErrorCode.RECOVERY_INVALID, message, details=details)

    def _restore_from_snapshot(self) -> None:
        try:
            raw = self.snapshot_path.read_bytes()
            snapshot = json.loads(raw.decode("utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise self._recovery_error(f"cannot decode snapshot: {exc}") from exc
        if not isinstance(snapshot, dict):
            raise self._recovery_error("snapshot root must be an object")
        required = {
            "schema_version",
            "dataset",
            "versions",
            "latest_version",
            "preview",
            "snapshot_hash",
        }
        missing = sorted(required - set(snapshot))
        if missing:
            raise self._recovery_error("snapshot fields are missing", missing=missing)
        if snapshot["schema_version"] != SNAPSHOT_SCHEMA_VERSION:
            raise self._recovery_error(
                "unsupported snapshot schema", schema_version=snapshot["schema_version"]
            )
        if not isinstance(snapshot["versions"], list) or not snapshot["versions"]:
            raise self._recovery_error("snapshot contains no published versions")
        try:
            dataset = DatasetInfo.from_dict(snapshot["dataset"])
            preview = PreviewState.from_dict(snapshot["preview"])
            versions = [VersionRecord.from_dict(item) for item in snapshot["versions"]]
        except (KeyError, TypeError, ValueError) as exc:
            raise self._recovery_error(f"snapshot field is invalid: {exc}") from exc

        snapshot_base = dict(snapshot)
        expected_snapshot_hash = snapshot_base.pop("snapshot_hash")
        actual_snapshot_hash = sha256_hex(canonical_bytes(snapshot_base))
        if expected_snapshot_hash != actual_snapshot_hash:
            raise self._recovery_error("snapshot hash mismatch")

        self.dataset = dataset
        if self._source_hint is not None:
            candidate = self._source_hint
        else:
            candidate = self.workspace / dataset.relative_path
            if not candidate.exists() and not Path(dataset.relative_path).is_absolute():
                candidate = Path(dataset.relative_path)
        try:
            loader = HDRSequence(candidate)
            actual_file_hash = hash_file(loader.path)
        except (HDRError, OSError) as exc:
            raise self._recovery_error(f"source sequence cannot be reopened: {exc}") from exc
        self._validate_dataset(loader, dataset, actual_file_hash)
        self._loader = loader
        self._validate_version_chain(versions, snapshot["latest_version"])
        self._validate_preview(preview, versions)

        recomputed: dict[int, tuple[int, ...]] = {}
        for record in versions:
            missing_frames = self._validate_output_files(record)
            if missing_frames:
                recovered_frames = self._recompute_missing_frames(record, missing_frames)
                recomputed[record.version] = tuple(recovered_frames)
        for record in versions:
            self._verify_output_hash(record)

        discarded = self._discard_unpublished_versions(versions[-1].version)
        self._cleanup_staging()
        self._versions = versions
        self._preview = preview
        self._recovery_report = RecoveryReport(
            restored_version=versions[-1].version,
            recomputed_frames=recomputed,
            discarded_versions=tuple(discarded),
        )

    def _validate_dataset(
        self, loader: HDRSequence, dataset: DatasetInfo, actual_file_hash: str
    ) -> None:
        header = loader.header
        actual = DatasetInfo(
            relative_path=dataset.relative_path,
            file_hash=actual_file_hash,
            header_hash=header.header_hash,
            width=header.width,
            height=header.height,
            frame_count=header.frame_count,
            bit_depth=header.bit_depth,
            color_space=header.color_space,
            transfer=header.transfer,
            peak_luminance=header.peak_luminance,
        )
        if actual != dataset:
            raise self._recovery_error(
                "source sequence does not match the snapshot dataset",
                expected=dataset.to_dict(),
                actual=actual.to_dict(),
            )

    def _validate_version_chain(
        self, versions: list[VersionRecord], latest_version: Any
    ) -> None:
        if not versions:
            raise self._recovery_error("version chain is empty")
        if int(latest_version) != versions[-1].version:
            raise self._recovery_error(
                "latest_version does not match the version chain",
                latest_version=latest_version,
                chain_tip=versions[-1].version,
            )
        previous: VersionRecord | None = None
        for expected_version, record in enumerate(versions, start=1):
            if record.version != expected_version:
                raise self._recovery_error(
                    "version numbers are not contiguous",
                    expected=expected_version,
                    actual=record.version,
                )
            if record.operation not in {"convert", "rollback"}:
                raise self._recovery_error(
                    "unknown version operation", version=record.version, operation=record.operation
                )
            if previous is None:
                if record.parent_version is not None or record.parent_record_hash is not None:
                    raise self._recovery_error("first version has an unexpected parent")
            else:
                if record.parent_version != previous.version:
                    raise self._recovery_error(
                        "version parent link is broken", version=record.version
                    )
                if record.parent_record_hash != previous.record_hash:
                    raise self._recovery_error(
                        "version record hash chain is broken", version=record.version
                    )
            if not record.verify_hash():
                raise self._recovery_error(
                    "version record hash mismatch", version=record.version
                )
            if len(record.frame_hashes) != self.header.frame_count:
                raise self._recovery_error(
                    "frame hash count does not match the sequence",
                    version=record.version,
                    expected=self.header.frame_count,
                    actual=len(record.frame_hashes),
                )
            if len(record.frame_summaries) != self.header.frame_count:
                raise self._recovery_error(
                    "frame summary count does not match the sequence",
                    version=record.version,
                )
            if len(record.output_hash) != 64:
                raise self._recovery_error(
                    "output hash has an invalid shape", version=record.version
                )
            try:
                record.params.normalized(self.header.frame_count)
            except ValueError as exc:
                raise self._recovery_error(
                    f"stored parameters are invalid: {exc}", version=record.version
                ) from exc
            affected_frames = frames_in_intervals(record.affected_intervals)
            if affected_frames and (
                affected_frames[0] < 0 or affected_frames[-1] >= self.header.frame_count
            ):
                raise self._recovery_error(
                    "affected interval is outside the sequence", version=record.version
                )
            if record.affected_count != len(affected_frames):
                raise self._recovery_error(
                    "affected_count does not match affected_intervals",
                    version=record.version,
                )
            if record.recomputed_count + record.reused_count != self.header.frame_count:
                raise self._recovery_error(
                    "recomputed/reused frame counts do not add up", version=record.version
                )
            previous = record

    def _validate_preview(
        self, preview: PreviewState, versions: list[VersionRecord]
    ) -> None:
        if preview.selected_version is None:
            return
        version_ids = {record.version for record in versions}
        if preview.selected_version not in version_ids:
            raise self._recovery_error(
                "preview references an unknown version",
                selected_version=preview.selected_version,
            )
        if preview.frame_index < 0 or preview.frame_index >= self.header.frame_count:
            raise self._recovery_error(
                "preview frame is outside the sequence", frame_index=preview.frame_index
            )

    def _validate_output_files(self, record: VersionRecord) -> list[int]:
        version_dir = self._version_dir(record.version)
        manifest_path = version_dir / "manifest.json"
        try:
            manifest = json.loads(manifest_path.read_text("utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise self._recovery_error(
                f"version manifest is missing or invalid: {exc}", version=record.version
            ) from exc
        if manifest != record.to_dict():
            raise self._recovery_error(
                "version manifest does not match the snapshot record", version=record.version
            )
        missing: list[int] = []
        for frame_index, expected_hash in enumerate(record.frame_hashes):
            frame_path = version_dir / "frames" / f"frame_{frame_index:06d}.bin"
            if not frame_path.exists():
                missing.append(frame_index)
                continue
            actual = hash_file(frame_path)
            if actual != expected_hash:
                raise self._recovery_error(
                    "published frame hash mismatch (possible corruption)",
                    version=record.version,
                    frame=frame_index,
                    expected=expected_hash,
                    actual=actual,
                )
        return missing

    def _recompute_missing_frames(
        self, record: VersionRecord, missing_frames: Iterable[int]
    ) -> list[int]:
        assert self._loader is not None
        version_dir = self._version_dir(record.version)
        recovered: list[int] = []
        for frame_index in missing_frames:
            try:
                source = self._loader.read_frame(frame_index)
                output, _ = convert_frame(source, self.header, record.params, frame_index)
                raw = self._array_to_bytes(output)
            except HDRError as exc:
                raise self._recovery_error(
                    f"cannot recompute missing published frame: {exc}",
                    version=record.version,
                    frame=frame_index,
                ) from exc
            actual_hash = sha256_hex(raw)
            if actual_hash != record.frame_hashes[frame_index]:
                raise self._recovery_error(
                    "recomputed missing frame does not match the published hash",
                    version=record.version,
                    frame=frame_index,
                )
            atomic_write_bytes(
                version_dir / "frames" / f"frame_{frame_index:06d}.bin", raw
            )
            recovered.append(frame_index)
        return recovered

    def _verify_output_hash(self, record: VersionRecord) -> None:
        digest = hashlib.sha256()
        for frame_index in range(self.header.frame_count):
            data = (self._version_dir(record.version) / "frames" / f"frame_{frame_index:06d}.bin").read_bytes()
            digest.update(data)
        if digest.hexdigest() != record.output_hash:
            raise self._recovery_error(
                "version content hash mismatch", version=record.version
            )

    def _discard_unpublished_versions(self, latest_version: int) -> list[int]:
        discarded: list[int] = []
        for path in sorted(self.versions_dir.iterdir()):
            if not path.is_dir():
                continue
            match = _VERSION_DIR_RE.match(path.name)
            if not match:
                continue
            version = int(match.group(1))
            if version > latest_version:
                shutil.rmtree(path)
                discarded.append(version)
        if discarded:
            fsync_dir(self.versions_dir)
        return discarded

    def _cleanup_staging(self) -> None:
        if not self.versions_dir.exists():
            return
        for path in self.versions_dir.iterdir():
            if path.is_dir() and path.name.startswith(".staging-"):
                shutil.rmtree(path)

    def _version_dir(self, version: int) -> Path:
        return self.versions_dir / f"v{version:06d}"

    def _frame_path(self, version: int, frame_index: int) -> Path:
        return self._version_dir(version) / "frames" / f"frame_{frame_index:06d}.bin"

    def _array_to_bytes(self, output: np.ndarray) -> bytes:
        if self.header.bit_depth <= 8:
            return output.astype(np.uint8).tobytes(order="C")
        return output.astype("<u2").tobytes(order="C")

    def _normalize_and_validate_params(
        self, params: ConversionParams
    ) -> ConversionParams:
        try:
            normalized = params.normalized(self.header.frame_count)
        except ValueError as exc:
            raise HDRError(
                ErrorCode.PARAM_CONFLICT,
                f"invalid exposure parameters: {exc}",
                conflict_params=["exposure_segments"],
            ) from exc
        for index, segment in enumerate(normalized.exposure_segments):
            if abs(segment.ev) > 64.0:
                raise HDRError(
                    ErrorCode.PARAM_CONFLICT,
                    "exposure EV magnitude exceeds the supported range",
                    conflict_params=[f"exposure_segments[{index}]"],
                    details={"ev": segment.ev, "supported_abs_max": 64.0},
                )
        if normalized.peak_luminance > 10000.0:
            raise HDRError(
                ErrorCode.PARAM_CONFLICT,
                "target peak luminance cannot exceed 10000 nits",
                conflict_params=["peak_luminance"],
                details={"peak_luminance": normalized.peak_luminance},
            )
        if (
            normalized.peak_luminance < self.header.peak_luminance
            and normalized.tone_map is ToneMap.NONE
        ):
            raise HDRError(
                ErrorCode.PARAM_CONFLICT,
                "target peak is lower than source peak and tone map 'none' cannot compress",
                conflict_params=["peak_luminance", "tone_map"],
                details={
                    "source_peak_luminance": self.header.peak_luminance,
                    "target_peak_luminance": normalized.peak_luminance,
                },
            )
        return normalized

    def submit(
        self,
        params: ConversionParams,
        *,
        cancel_event: threading.Event | None = None,
        progress: Callable[[int, int], None] | None = None,
    ) -> VersionRecord:
        if cancel_event is None:
            cancel_event = threading.Event()
        with self._lock:
            if self._busy:
                raise HDRError(ErrorCode.BUSY, "a conversion task is already running")
            self._busy = True
            self._active_cancel = cancel_event
            previous = self._versions[-1] if self._versions else None
        staging: Path | None = None
        version_dir: Path | None = None
        published = False
        try:
            normalized = self._normalize_and_validate_params(params)
            if previous is None:
                affected = list(range(self.header.frame_count))
                full_recompute = True
            else:
                changed_params = set(
                    compare_param_dicts(previous.params, normalized)
                )
                full_recompute = bool(changed_params & _COMMIT_KEYS)
                affected = changed_frame_indices(
                    previous.params,
                    normalized,
                    self.header.frame_count,
                    full_recompute=full_recompute,
                )
            if previous is None:
                affected_indices = list(range(self.header.frame_count))
            else:
                affected_indices = affected

            computed: dict[int, tuple[bytes, dict[str, Any]]] = {}
            total = len(affected_indices)
            for done, frame_index in enumerate(affected_indices, start=1):
                self._check_cancel(cancel_event)
                source = self._loader.read_frame(frame_index)  # type: ignore[union-attr]
                output, summary = convert_frame(
                    source, self.header, normalized, frame_index
                )
                computed[frame_index] = (self._array_to_bytes(output), summary)
                if progress is not None:
                    progress(done, total)
            self._check_cancel(cancel_event)
            self._reject_exposure_overrange(normalized, computed)

            version = 1 if previous is None else previous.version + 1
            staging, output_hash, frame_hashes, frame_summaries = self._stage_outputs(
                version=version,
                params=normalized,
                previous=previous,
                computed=computed,
                affected_indices=affected_indices,
            )
            record = self._make_record(
                version=version,
                parent=previous,
                params=normalized,
                affected_indices=affected_indices,
                computed=computed,
                frame_hashes=frame_hashes,
                frame_summaries=frame_summaries,
                output_hash=output_hash,
                operation="convert",
            )
            self._write_manifest_and_verify(staging, record)
            version_dir = self._version_dir(version)
            self._commit_version_dir(staging, version_dir)
            self._after_version_dir_visible()
            with self._lock:
                if self._latest_version != (0 if previous is None else previous.version):
                    raise HDRError(
                        ErrorCode.VERSION_MISMATCH,
                        "session changed while the conversion was running",
                    )
                preview = PreviewState(
                    selected_version=record.version, frame_index=0, playing=False
                )
                self._write_snapshot([*self._versions, record], preview)
                self._versions.append(record)
                self._preview = preview
                published = True
                self._trace_published(record)
            return record
        except HDRError as exc:
            self._trace_rejection(exc)
            raise
        finally:
            if not published and staging is not None:
                shutil.rmtree(staging, ignore_errors=True)
            if not published and version_dir is not None and version_dir.exists():
                shutil.rmtree(version_dir, ignore_errors=True)
            with self._lock:
                self._busy = False
                self._active_cancel = None

    def rollback(self, target_version: int) -> VersionRecord:
        with self._lock:
            if self._busy:
                raise HDRError(ErrorCode.BUSY, "a conversion task is already running")
            target = self._find_version(target_version)
            previous = self._versions[-1] if self._versions else None
            if previous is None:
                raise HDRError(ErrorCode.VERSION_MISMATCH, "there is no published version")
            self._busy = True
        staging: Path | None = None
        version_dir: Path | None = None
        published = False
        try:
            changed_params = set(compare_param_dicts(previous.params, target.params))
            affected = changed_frame_indices(
                previous.params,
                target.params,
                self.header.frame_count,
                full_recompute=bool(changed_params & _COMMIT_KEYS),
            )
            version = previous.version + 1
            staging, output_hash, frame_hashes, frame_summaries = self._stage_from_version(
                version, target
            )
            record = self._make_record(
                version=version,
                parent=previous,
                params=target.params,
                affected_indices=affected,
                computed=None,
                frame_hashes=frame_hashes,
                frame_summaries=frame_summaries,
                output_hash=output_hash,
                operation="rollback",
                rollback_target=target.version,
                recomputed_count=0,
                reused_count=self.header.frame_count,
                intermediate_summary=target.intermediate_summary,
            )
            self._write_manifest_and_verify(staging, record)
            version_dir = self._version_dir(version)
            self._commit_version_dir(staging, version_dir)
            self._after_version_dir_visible()
            with self._lock:
                preview = PreviewState(
                    selected_version=record.version, frame_index=0, playing=False
                )
                self._write_snapshot([*self._versions, record], preview)
                self._versions.append(record)
                self._preview = preview
                published = True
                self._trace_published(record)
            return record
        except HDRError as exc:
            self._trace_rejection(exc)
            raise
        finally:
            if not published and staging is not None:
                shutil.rmtree(staging, ignore_errors=True)
            if not published and version_dir is not None and version_dir.exists():
                shutil.rmtree(version_dir, ignore_errors=True)
            with self._lock:
                self._busy = False

    def _stage_outputs(
        self,
        *,
        version: int,
        params: ConversionParams,
        previous: VersionRecord | None,
        computed: dict[int, tuple[bytes, dict[str, Any]]],
        affected_indices: list[int],
    ) -> tuple[Path, str, tuple[str, ...], tuple[dict[str, Any], ...]]:
        staging = self.versions_dir / f".staging-v{version:06d}-{os.getpid()}"
        shutil.rmtree(staging, ignore_errors=True)
        (staging / "frames").mkdir(parents=True)
        digest = hashlib.sha256()
        frame_hashes: list[str] = []
        frame_summaries: list[dict[str, Any]] = []
        for frame_index in range(self.header.frame_count):
            destination = staging / "frames" / f"frame_{frame_index:06d}.bin"
            if frame_index in computed:
                raw, summary = computed[frame_index]
                frame_hash = sha256_hex(raw)
                with destination.open("wb") as handle:
                    handle.write(raw)
                    handle.flush()
                    os.fsync(handle.fileno())
                digest.update(raw)
                frame_hashes.append(frame_hash)
                frame_summaries.append(summary)
            else:
                if previous is None:
                    raise HDRError(
                        ErrorCode.VERSION_MISMATCH,
                        "cannot reuse a frame without a previous version",
                    )
                source = previous
                source_path = self._frame_path(source.version, frame_index)
                raw = source_path.read_bytes()
                expected_hash = source.frame_hashes[frame_index]
                if sha256_hex(raw) != expected_hash:
                    raise HDRError(
                        ErrorCode.VERSION_MISMATCH,
                        "reused frame hash does not match the previous version",
                        details={"version": source.version, "frame": frame_index},
                    )
                self._link_or_copy(source_path, destination)
                digest.update(raw)
                frame_hashes.append(expected_hash)
                frame_summaries.append(source.frame_summaries[frame_index])
        return staging, digest.hexdigest(), tuple(frame_hashes), tuple(frame_summaries)

    def _stage_from_version(
        self, version: int, source: VersionRecord
    ) -> tuple[Path, str, tuple[str, ...], tuple[dict[str, Any], ...]]:
        staging = self.versions_dir / f".staging-v{version:06d}-{os.getpid()}"
        shutil.rmtree(staging, ignore_errors=True)
        (staging / "frames").mkdir(parents=True)
        digest = hashlib.sha256()
        for frame_index in range(self.header.frame_count):
            source_path = self._frame_path(source.version, frame_index)
            raw = source_path.read_bytes()
            if sha256_hex(raw) != source.frame_hashes[frame_index]:
                raise HDRError(
                    ErrorCode.VERSION_MISMATCH,
                    "rollback source frame hash is invalid",
                    details={"version": source.version, "frame": frame_index},
                )
            digest.update(raw)
            self._link_or_copy(
                source_path,
                staging / "frames" / f"frame_{frame_index:06d}.bin",
            )
        return (
            staging,
            digest.hexdigest(),
            source.frame_hashes,
            source.frame_summaries,
        )

    def _link_or_copy(self, source: Path, destination: Path) -> None:
        try:
            os.link(source, destination)
        except OSError:
            shutil.copyfile(source, destination)

    def _make_record(
        self,
        *,
        version: int,
        parent: VersionRecord | None,
        params: ConversionParams,
        affected_indices: list[int],
        computed: dict[int, tuple[bytes, dict[str, Any]]] | None,
        frame_hashes: tuple[str, ...],
        frame_summaries: tuple[dict[str, Any], ...],
        output_hash: str,
        operation: str,
        rollback_target: int | None = None,
        recomputed_count: int | None = None,
        reused_count: int | None = None,
        intermediate_summary: dict[str, Any] | None = None,
    ) -> VersionRecord:
        affected_intervals = tuple(
            (item[0], item[1]) for item in intervals_from_indices(affected_indices)
        )
        if recomputed_count is None:
            recomputed_count = (
                len(computed) if computed is not None else len(affected_indices)
            )
        if reused_count is None:
            reused_count = self.header.frame_count - recomputed_count
        record = VersionRecord(
            version=version,
            parent_version=None if parent is None else parent.version,
            parent_record_hash=None if parent is None else parent.record_hash,
            operation=operation,
            params=params,
            affected_intervals=affected_intervals,
            affected_count=len(affected_indices),
            recomputed_count=recomputed_count,
            reused_count=reused_count,
            frame_hashes=frame_hashes,
            output_hash=output_hash,
            intermediate_summary=(
                intermediate_summary
                if intermediate_summary is not None
                else build_intermediate_summary(self.header, params)
            ),
            frame_summaries=frame_summaries,
            rollback_target=rollback_target,
        )
        return record.with_computed_hash()

    def _reject_exposure_overrange(
        self,
        params: ConversionParams,
        computed: dict[int, tuple[bytes, dict[str, Any]]],
    ) -> None:
        overrange_frames = [
            frame_index
            for frame_index, (_, summary) in computed.items()
            if summary["overrange"] and params.ev_for_frame(frame_index) > 0.0
        ]
        if not overrange_frames:
            return
        segments = [
            f"exposure_segments[{index}]"
            for index, segment in enumerate(params.exposure_segments)
            if any(segment.contains(frame) for frame in overrange_frames)
        ]
        max_code = max(
            computed[frame][1]["raw_max_code"] for frame in overrange_frames
        )
        raise HDRError(
            ErrorCode.PARAM_CONFLICT,
            "exposure adjustment pushes output codes outside the target bit depth",
            conflict_params=segments or ["exposure_segments"],
            details={
                "frame_indices": overrange_frames,
                "max_code": self.header.max_code,
                "max_raw_code": max_code,
            },
        )

    def _check_cancel(self, cancel_event: threading.Event | None) -> None:
        if cancel_event is not None and cancel_event.is_set():
            raise HDRError(ErrorCode.CANCELLED, "conversion task was cancelled")

    def _write_manifest_and_verify(self, staging: Path, record: VersionRecord) -> None:
        atomic_write_json(staging / "manifest.json", record.to_dict())
        for frame_index, expected_hash in enumerate(record.frame_hashes):
            path = staging / "frames" / f"frame_{frame_index:06d}.bin"
            if not path.exists() or hash_file(path) != expected_hash:
                raise HDRError(
                    ErrorCode.VERSION_MISMATCH,
                    "staged frame hash mismatch before publication",
                    details={"version": record.version, "frame": frame_index},
                )
        self._verify_staged_output_hash(staging, record)
        self._before_publish_commit(staging, record)
        self._verify_staged_output_hash(staging, record)
        for frame_index, expected_hash in enumerate(record.frame_hashes):
            path = staging / "frames" / f"frame_{frame_index:06d}.bin"
            if hash_file(path) != expected_hash:
                raise HDRError(
                    ErrorCode.VERSION_MISMATCH,
                    "staged frame hash changed during publication",
                    details={"version": record.version, "frame": frame_index},
                )

    def _verify_staged_output_hash(self, staging: Path, record: VersionRecord) -> None:
        digest = hashlib.sha256()
        for frame_index in range(self.header.frame_count):
            digest.update(
                (staging / "frames" / f"frame_{frame_index:06d}.bin").read_bytes()
            )
        if digest.hexdigest() != record.output_hash:
            raise HDRError(
                ErrorCode.VERSION_MISMATCH,
                "staged output content hash does not match",
                details={"version": record.version},
            )

    def _commit_version_dir(self, staging: Path, version_dir: Path) -> None:
        if version_dir.exists():
            raise HDRError(
                ErrorCode.VERSION_MISMATCH,
                "target version directory already exists",
                details={"version_dir": version_dir.name},
            )
        os.replace(staging, version_dir)
        fsync_dir(self.versions_dir)

    def _before_publish_commit(self, staging: Path, record: VersionRecord) -> None:
        """Test hook: runs after output hashes, immediately before directory commit."""

    def _after_version_dir_visible(self) -> None:
        """Test hook: runs between immutable directory promotion and snapshot commit."""

    def _snapshot_payload(
        self, versions: list[VersionRecord], preview: PreviewState
    ) -> dict[str, Any]:
        if self.dataset is None:
            raise HDRError(ErrorCode.RECOVERY_INVALID, "dataset information is unavailable")
        payload: dict[str, Any] = {
            "schema_version": SNAPSHOT_SCHEMA_VERSION,
            "dataset": self.dataset.to_dict(),
            "versions": [record.to_dict() for record in versions],
            "latest_version": versions[-1].version if versions else 0,
            "preview": preview.to_dict(),
        }
        payload["snapshot_hash"] = sha256_hex(canonical_bytes(payload))
        return payload

    def _write_snapshot(
        self, versions: list[VersionRecord], preview: PreviewState
    ) -> None:
        payload = self._snapshot_payload(versions, preview)
        snapshots_dir = self.snapshot_path.parent
        archive = snapshots_dir / f"snapshot-v{payload['latest_version']:06d}.json"
        atomic_write_json(archive, payload)
        atomic_write_json(self.snapshot_path, payload)

    def _trace_base(self, record: VersionRecord) -> dict[str, Any]:
        return {
            "version": record.version,
            "parent_version": record.parent_version,
            "operation": record.operation,
            "rollback_target": record.rollback_target,
            "params": record.params.to_dict(),
            "affected_intervals": [list(item) for item in record.affected_intervals],
            "affected_frames": record.affected_count,
            "recomputed_frames": record.recomputed_count,
            "reused_frames": record.reused_count,
            "frame_hashes": list(record.frame_hashes),
            "output_hash": record.output_hash,
            "record_hash": record.record_hash,
            "parent_record_hash": record.parent_record_hash,
        }

    def _trace_published(self, record: VersionRecord) -> None:
        event = {
            "event": "version_published",
            **self._trace_base(record),
            "preview": self._preview.to_dict(),
        }
        try:
            append_json_line(self.trace_path, event)
        except OSError:
            # The snapshot and output content hashes remain the source of truth.
            pass

    def _trace_rejection(self, error: HDRError) -> None:
        if error.code is ErrorCode.CANCELLED:
            event_name = "submission_cancelled"
        else:
            event_name = "submission_rejected"
        latest = self._versions[-1] if self._versions else None
        event = {
            "event": event_name,
            "code": error.code.value,
            "message": error.message,
            "conflict_params": error.conflict_params,
            "details": error.details,
            "published_version": None if latest is None else latest.version,
            "published_output_hash": None if latest is None else latest.output_hash,
            "published_record_hash": None if latest is None else latest.record_hash,
        }
        try:
            append_json_line(self.trace_path, event)
        except OSError:
            pass

    @property
    def _latest_version(self) -> int:
        return self._versions[-1].version if self._versions else 0

    def _find_version(self, version: int) -> VersionRecord:
        for record in self._versions:
            if record.version == version:
                return record
        raise HDRError(
            ErrorCode.VERSION_MISMATCH,
            "requested version does not exist",
            details={"version": version},
        )

    @property
    def is_busy(self) -> bool:
        with self._lock:
            return self._busy

    @property
    def versions(self) -> tuple[VersionRecord, ...]:
        with self._lock:
            return tuple(self._versions)

    @property
    def latest_record(self) -> VersionRecord | None:
        with self._lock:
            return self._versions[-1] if self._versions else None

    @property
    def preview_state(self) -> PreviewState:
        with self._lock:
            return self._preview

    def get_version(self, version: int) -> VersionRecord:
        with self._lock:
            return self._find_version(version)

    def cancel_active(self) -> bool:
        with self._lock:
            if self._active_cancel is None:
                return False
            self._active_cancel.set()
            return True

    def set_preview(
        self, *, selected_version: int | None = None, frame_index: int | None = None, playing: bool | None = None
    ) -> PreviewState:
        with self._lock:
            version = self._preview.selected_version if selected_version is None else selected_version
            frame = self._preview.frame_index if frame_index is None else frame_index
            is_playing = self._preview.playing if playing is None else playing
            if version is not None:
                self._find_version(version)
            if frame < 0 or frame >= self.header.frame_count:
                raise HDRError(
                    ErrorCode.VERSION_MISMATCH,
                    "preview frame is outside the sequence",
                    details={"frame_index": frame},
                )
            preview = PreviewState(
                selected_version=version, frame_index=frame, playing=is_playing
            )
            if self._versions:
                self._write_snapshot(self._versions, preview)
            self._preview = preview
            return preview

    def read_version_frame_bytes(self, version: int, frame_index: int) -> bytes:
        with self._lock:
            record = self._find_version(version)
            if frame_index < 0 or frame_index >= self.header.frame_count:
                raise HDRError(
                    ErrorCode.VERSION_MISMATCH,
                    "frame index is outside the sequence",
                    details={"frame_index": frame_index},
                )
            path = self._frame_path(version, frame_index)
            try:
                data = path.read_bytes()
            except OSError as exc:
                raise HDRError(
                    ErrorCode.VERSION_MISMATCH,
                    f"published frame is unavailable: {exc}",
                    details={"version": version, "frame": frame_index},
                ) from exc
            if sha256_hex(data) != record.frame_hashes[frame_index]:
                raise HDRError(
                    ErrorCode.VERSION_MISMATCH,
                    "published frame hash mismatch",
                    details={"version": version, "frame": frame_index},
                )
            return data

    def read_version_frame(self, version: int, frame_index: int) -> np.ndarray:
        data = self.read_version_frame_bytes(version, frame_index)
        dtype = np.uint8 if self.header.bit_depth <= 8 else np.dtype("<u2")
        values = np.frombuffer(data, dtype=dtype).copy()
        return values.reshape((self.header.height, self.header.width, 3))

    def read_source_frame(self, frame_index: int) -> np.ndarray:
        return self._loader.read_frame(frame_index)  # type: ignore[union-attr]

    def diff_versions(self, left_version: int, right_version: int) -> VersionDiff:
        with self._lock:
            left = self._find_version(left_version)
            right = self._find_version(right_version)
        changed_params = compare_param_dicts(left.params, right.params)
        changed_frames = tuple(
            index
            for index, (left_hash, right_hash) in enumerate(
                zip(left.frame_hashes, right.frame_hashes)
            )
            if left_hash != right_hash
        )
        return VersionDiff(
            left_version=left.version,
            right_version=right.version,
            changed_params=changed_params,
            changed_frames=changed_frames,
            left_output_hash=left.output_hash,
            right_output_hash=right.output_hash,
            same_content=left.output_hash == right.output_hash,
        )

    def close(self) -> None:
        """No persistent handles are held; retained for API symmetry."""

