from __future__ import annotations

import fcntl
import json
import threading
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from types import TracebackType
from typing import Any, ClassVar

from .config import Config
from .errors import (
    E_BUSY,
    E_CANCELLED,
    E_MEMORY_BUDGET,
    E_RECOVERY_INVALID,
    AppError,
)
from .loudness import (
    LoudnessAnalyzer,
    SegmentAnalysis,
    default_downmix_weights,
    round_db,
)
from .metadata import (
    MetadataDocument,
    derive_boundaries,
)
from .trace import TraceLogger
from .util import (
    atomic_write_json,
    canonical_json_bytes,
    hash_without,
    read_json,
    sha256_bytes,
)
from .versions import DowngradeResult, UpgradeResult, downgrade_document, upgrade_to_v3
from .wav import WavInfo, estimated_analysis_memory

MANIFEST_SCHEMA = "loudness-archive-manifest"
MANIFEST_SCHEMA_VERSION = 1
SEGMENT_SCHEMA = "loudness-archive-segment"
GENESIS_CHAIN = "sha256:genesis"
CHAIN_ALGORITHM = (
    "sha256(prev_chain + '\\n' + content_sha256 + '\\n' + "
    "parameters_version + '\\n' + decimal_segment_index)"
)


class CancellationToken:
    def __init__(self) -> None:
        self._event = threading.Event()

    def cancel(self) -> None:
        self._event.set()

    @property
    def cancelled(self) -> bool:
        return self._event.is_set()

    def raise_if_cancelled(self, asset_id: str = "") -> None:
        if self.cancelled:
            raise AppError(
                E_CANCELLED,
                "operation was cooperatively cancelled",
                field="cancel",
                details={"asset_id": asset_id},
            )


class AssetLock:
    """Advisory process lock plus an in-process guard for the same asset."""

    _thread_lock = threading.Lock()
    _held: ClassVar[set[str]] = set()

    def __init__(self, workspace: str | Path, asset_id: str):
        self.asset_id = asset_id
        self.path = Path(workspace) / "locks" / f"{asset_id}.lock"
        self.handle: Any = None
        self.acquired = False

    def __enter__(self) -> AssetLock:  # noqa: PYI034 - Python 3.10 compatibility
        with self._thread_lock:
            if self.asset_id in self._held:
                raise AppError(
                    E_BUSY,
                    f"asset {self.asset_id!r} already has an active archival task",
                    field="asset_id",
                    details={"asset_id": self.asset_id},
                )
            self.path.parent.mkdir(parents=True, exist_ok=True)
            handle = open(self.path, "a+b")
            try:
                fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError as exc:
                handle.close()
                raise AppError(
                    E_BUSY,
                    f"asset {self.asset_id!r} is locked by another process",
                    field="asset_id",
                    details={"asset_id": self.asset_id},
                ) from exc
            self.handle = handle
            self._held.add(self.asset_id)
            self.acquired = True
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        with self._thread_lock:
            self._held.discard(self.asset_id)
            if self.handle is not None:
                try:
                    fcntl.flock(self.handle.fileno(), fcntl.LOCK_UN)
                finally:
                    self.handle.close()
            self.acquired = False


@dataclass(frozen=True)
class ArchiveRunResult:
    asset_id: str
    target_version: int
    parameters_version: str
    manifest: dict[str, Any]
    analyses: tuple[SegmentAnalysis, ...]
    upgrade: UpgradeResult
    downgrade: DowngradeResult | None
    recovery: dict[str, Any]
    field_mapping: tuple[dict[str, Any], ...]
    loss_manifest: dict[str, Any] | None
    estimated_memory_bytes: int
    manifest_path: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "ok": True,
            "operation": "archive",
            "asset_id": self.asset_id,
            "target_version": self.target_version,
            "parameters_version": self.parameters_version,
            "manifest_path": self.manifest_path,
            "manifest": self.manifest,
            "segments": [analysis.to_dict() for analysis in self.analyses],
            "field_mapping": list(self.field_mapping),
            "loss_manifest": self.loss_manifest,
            "recovery": self.recovery,
            "estimated_memory_bytes": self.estimated_memory_bytes,
        }


def manifest_file(workspace: str | Path, asset_id: str) -> Path:
    return Path(workspace) / "manifests" / f"{asset_id}.json"


def segment_file(workspace: str | Path, asset_id: str, index: int) -> Path:
    return Path(workspace) / "segments" / asset_id / f"seg-{index:06d}.json"


def _recovery_error(
    message: str,
    field: str,
    segment_index: int | None = None,
    **details: Any,
) -> AppError:
    payload = dict(details)
    if segment_index is not None:
        payload["segment_index"] = segment_index
    return AppError(E_RECOVERY_INVALID, message, field=field, details=payload)


def _manifest_hash(manifest: dict[str, Any]) -> str:
    return hash_without(manifest, "manifest_sha256")


def _chain_hash(
    previous: str,
    content_sha256: str,
    parameters_version: str,
    index: int,
) -> str:
    material = (
        f"{previous}\n{content_sha256}\n{parameters_version}\n{index}".encode()
    )
    return sha256_bytes(material)


def _new_manifest(
    asset_id: str,
    source_wav_sha256: str,
    parameters_version: str,
    metadata_version: int,
    boundaries: tuple[tuple[int, int], ...],
    boundary_source: str,
    metadata_checksum: str | None,
) -> dict[str, Any]:
    boundary_rows = [
        {"index": index, "start_sample": start, "end_sample": end}
        for index, (start, end) in enumerate(boundaries)
    ]
    manifest: dict[str, Any] = {
        "schema": MANIFEST_SCHEMA,
        "schema_version": MANIFEST_SCHEMA_VERSION,
        "manifest_revision": 0,
        "asset_id": asset_id,
        "source_wav_sha256": source_wav_sha256,
        "parameters_version": parameters_version,
        "metadata_version": metadata_version,
        "metadata_checksum": metadata_checksum,
        "boundary_source": boundary_source,
        "boundaries": boundary_rows,
        "boundaries_sha256": sha256_bytes(canonical_json_bytes(boundary_rows)),
        "chain_algorithm": CHAIN_ALGORITHM,
        "chain_head": GENESIS_CHAIN,
        "segments": [],
    }
    manifest["manifest_sha256"] = _manifest_hash(manifest)
    return manifest


def _load_and_validate_manifest(
    workspace: str | Path,
    asset_id: str,
    *,
    verify_segments: bool = True,
) -> dict[str, Any] | None:
    path = manifest_file(workspace, asset_id)
    if not path.exists():
        return None
    try:
        manifest = read_json(path)
    except (OSError, json.JSONDecodeError) as exc:
        raise _recovery_error(
            f"manifest is unreadable or truncated: {exc}",
            "manifest",
        ) from exc
    if not isinstance(manifest, dict):
        raise _recovery_error("manifest root must be an object", "manifest")
    if manifest.get("schema") != MANIFEST_SCHEMA:
        raise _recovery_error("unknown manifest schema", "manifest.schema")
    if manifest.get("schema_version") != MANIFEST_SCHEMA_VERSION:
        raise _recovery_error(
            f"unsupported manifest schema version {manifest.get('schema_version')!r}",
            "manifest.schema_version",
        )
    expected_manifest_hash = manifest.get("manifest_sha256")
    if not isinstance(expected_manifest_hash, str):
        raise _recovery_error("missing manifest checksum", "manifest.manifest_sha256")
    actual_manifest_hash = _manifest_hash(manifest)
    if expected_manifest_hash != actual_manifest_hash:
        raise _recovery_error(
            "manifest checksum mismatch",
            "manifest.manifest_sha256",
            expected=expected_manifest_hash,
            actual=actual_manifest_hash,
        )
    if manifest.get("asset_id") != asset_id:
        raise _recovery_error("manifest asset id mismatch", "manifest.asset_id")
    segments = manifest.get("segments")
    if not isinstance(segments, list):
        raise _recovery_error("manifest segments must be an array", "manifest.segments")
    if verify_segments:
        validate_manifest_chain(workspace, manifest)
    return manifest


def validate_manifest_chain(
    workspace: str | Path,
    manifest: dict[str, Any],
) -> tuple[dict[str, Any], ...]:
    entries = manifest.get("segments", [])
    previous = GENESIS_CHAIN
    seen: set[int] = set()
    for position, entry in enumerate(entries):
        base = f"segments[{position}]"
        if not isinstance(entry, dict):
            raise _recovery_error("segment entry must be an object", base, position)
        index = entry.get("index")
        if isinstance(index, bool) or not isinstance(index, int):
            raise _recovery_error("segment index must be an integer", f"{base}.index", position)
        if index in seen:
            raise _recovery_error("duplicate committed segment", f"{base}.index", index)
        if index != position:
            raise _recovery_error(
                f"expected contiguous segment index {position}, got {index}",
                f"{base}.index",
                index,
            )
        seen.add(index)
        relative = entry.get("file")
        if not isinstance(relative, str):
            raise _recovery_error("segment file path is missing", f"{base}.file", index)
        expected_path = segment_file(workspace, manifest["asset_id"], index)
        if Path(relative) != Path("segments") / manifest["asset_id"] / expected_path.name:
            raise _recovery_error(
                "segment file path does not match the ledger",
                f"{base}.file",
                index,
            )
        if not expected_path.exists():
            raise _recovery_error("segment file is missing", f"{base}.file", index)
        try:
            payload = read_json(expected_path)
        except (OSError, json.JSONDecodeError) as exc:
            raise _recovery_error(
                f"segment file is truncated or unreadable: {exc}",
                f"{base}.file",
                index,
            ) from exc
        if not isinstance(payload, dict):
            raise _recovery_error("segment payload must be an object", f"{base}.payload", index)
        if payload.get("schema") != SEGMENT_SCHEMA:
            raise _recovery_error("unknown segment schema", f"{base}.schema", index)
        if payload.get("asset_id") != manifest["asset_id"]:
            raise _recovery_error("segment asset id mismatch", f"{base}.asset_id", index)
        if payload.get("index") != index:
            raise _recovery_error("segment payload index mismatch", f"{base}.index", index)
        content_sha = payload.get("content_sha256")
        actual_content_sha = hash_without(payload, "content_sha256")
        if not isinstance(content_sha, str) or content_sha != actual_content_sha:
            raise _recovery_error(
                "segment content hash mismatch",
                f"{base}.content_sha256",
                index,
            )
        if entry.get("content_sha256") != content_sha:
            raise _recovery_error(
                "manifest content hash disagrees with segment file",
                f"{base}.content_sha256",
                index,
            )
        expected_chain = _chain_hash(
            previous,
            content_sha,
            str(entry.get("parameters_version", "")),
            index,
        )
        if entry.get("prev_chain") != previous:
            raise _recovery_error(
                "previous hash pointer is broken",
                f"{base}.prev_chain",
                index,
                expected=previous,
                actual=entry.get("prev_chain"),
            )
        if entry.get("chain_hash") != expected_chain:
            raise _recovery_error(
                "chain hash mismatch",
                f"{base}.chain_hash",
                index,
                expected=expected_chain,
                actual=entry.get("chain_hash"),
            )
        previous = str(entry["chain_hash"])
    if manifest.get("chain_head") != previous:
        raise _recovery_error(
            "manifest chain head mismatch",
            "manifest.chain_head",
            len(entries) - 1 if entries else None,
            expected=previous,
            actual=manifest.get("chain_head"),
        )
    return tuple(entries)


def _write_manifest(workspace: str | Path, manifest: dict[str, Any]) -> None:
    manifest["manifest_sha256"] = _manifest_hash(manifest)
    path = manifest_file(workspace, str(manifest["asset_id"]))
    atomic_write_json(path, manifest)


def _segment_payload(
    asset_id: str,
    analysis: SegmentAnalysis,
    target_version: int,
    parameters_version: str,
) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "schema": SEGMENT_SCHEMA,
        "asset_id": asset_id,
        "index": analysis.index,
        "analysis_version": 3,
        "metadata_version": target_version,
        "parameters_version": parameters_version,
        "start_sample": analysis.start_sample,
        "end_sample": analysis.end_sample,
        "short_term_lufs": round_db(analysis.short_term_lufs),
        "true_peak_dbtp": round_db(analysis.true_peak_dbtp),
        "true_peak_limit_dbtp": round_db(analysis.true_peak_limit_dbtp),
        "over_limit": analysis.over_limit,
        "excess_db": round_db(analysis.excess_db),
        "short_term_window_samples": analysis.short_term_window_samples,
        "boundary_source": analysis.boundary_source,
        "basis_fields": list(analysis.basis_fields),
    }
    payload["content_sha256"] = hash_without(payload, "content_sha256")
    return payload


def _commit_segment(
    workspace: str | Path,
    manifest: dict[str, Any],
    analysis: SegmentAnalysis,
    target_version: int,
    parameters_version: str,
) -> dict[str, Any]:
    asset_id = str(manifest["asset_id"])
    index = analysis.index
    payload = _segment_payload(asset_id, analysis, target_version, parameters_version)
    path = segment_file(workspace, asset_id, index)
    rel_path = Path("segments") / asset_id / path.name
    atomic_write_json(path, payload)

    previous = str(manifest.get("chain_head", GENESIS_CHAIN))
    content_sha = str(payload["content_sha256"])
    chain = _chain_hash(previous, content_sha, parameters_version, index)
    entry = {
        "index": index,
        "start_sample": analysis.start_sample,
        "end_sample": analysis.end_sample,
        "file": rel_path.as_posix(),
        "content_sha256": content_sha,
        "parameters_version": parameters_version,
        "metadata_version": target_version,
        "prev_chain": previous,
        "chain_hash": chain,
    }
    manifest.setdefault("segments", []).append(entry)
    manifest["chain_head"] = chain
    manifest["manifest_revision"] = int(manifest.get("manifest_revision", 0)) + 1
    manifest["parameters_version"] = parameters_version
    manifest["metadata_version"] = target_version
    _write_manifest(workspace, manifest)
    return entry


def _analysis_from_payload(payload: dict[str, Any]) -> SegmentAnalysis:
    return SegmentAnalysis(
        index=int(payload["index"]),
        start_sample=int(payload["start_sample"]),
        end_sample=int(payload["end_sample"]),
        short_term_lufs=float(payload["short_term_lufs"]),
        true_peak_dbtp=float(payload["true_peak_dbtp"]),
        true_peak_limit_dbtp=float(payload["true_peak_limit_dbtp"]),
        over_limit=bool(payload["over_limit"]),
        excess_db=float(payload["excess_db"]),
        delta_to_limit_db=float(payload["true_peak_dbtp"])
        - float(payload["true_peak_limit_dbtp"]),
        short_term_window_samples=int(payload.get("short_term_window_samples", 0)),
        boundary_source=str(payload.get("boundary_source", "metadata.explicit_boundaries")),
        basis_fields=(
            f"segments[{payload['index']}].true_peak_dbtp",
            f"segments[{payload['index']}].true_peak_limit_dbtp",
        ),
    )


def default_parameters_version(config: Config) -> str:
    return f"v3:{config.analysis_algorithm_version}:{config.fingerprint[7:23]}"


class ArchiveService:
    def __init__(
        self,
        workspace: str | Path,
        config: Config,
        trace: TraceLogger | None = None,
    ):
        self.workspace = Path(workspace)
        self.config = config
        self.trace = trace or TraceLogger(self.workspace / "trace.jsonl")
        self.workspace.mkdir(parents=True, exist_ok=True)

    def _record(self, event: str, **fields: Any) -> None:
        self.trace.record(event, **fields)

    def execute(
        self,
        wav: WavInfo,
        document: MetadataDocument,
        *,
        target_version: int = 3,
        parameters_version: str | None = None,
        cancel_token: CancellationToken | None = None,
        crash_after_commits: int | None = None,
        crash_callback: Callable[[], None] | None = None,
        progress_callback: Callable[[dict[str, Any]], None] | None = None,
    ) -> ArchiveRunResult:
        if target_version not in (1, 2, 3):
            raise AppError(
                "E_META_INVALID",
                f"unknown target version {target_version}",
                field="target_version",
            )
        estimated = estimated_analysis_memory(wav)
        if estimated > self.config.per_task_memory_bytes:
            raise AppError(
                E_MEMORY_BUDGET,
                "estimated task memory exceeds the configured per-task budget",
                field="per_task_memory_bytes",
                details={
                    "estimated_memory_bytes": estimated,
                    "per_task_memory_bytes": self.config.per_task_memory_bytes,
                },
            )
        parameter_version = parameters_version or default_parameters_version(self.config)
        with AssetLock(self.workspace, document.asset_id):
            self._record(
                "task_submit",
                asset_id=document.asset_id,
                submit_seq=0,
                memory_bytes=estimated,
                parameters_version=parameter_version,
                target_version=target_version,
                state="queued",
            )
            self._record(
                "task_start",
                asset_id=document.asset_id,
                submit_seq=0,
                memory_bytes=estimated,
                parameters_version=parameter_version,
                target_version=target_version,
                state="running",
            )
            try:
                result = self._execute_locked(
                    wav,
                    document,
                    target_version=target_version,
                    parameters_version=parameter_version,
                    cancel_token=cancel_token,
                    crash_after_commits=crash_after_commits,
                    crash_callback=crash_callback,
                    progress_callback=progress_callback,
                    estimated=estimated,
                )
            except AppError as exc:
                if exc.code == E_CANCELLED:
                    self._record(
                        "task_cancel",
                        asset_id=document.asset_id,
                        submit_seq=0,
                        code=exc.code,
                        state="cancelled",
                    )
                raise
            self._record(
                "task_complete",
                asset_id=document.asset_id,
                submit_seq=0,
                chain_head=result.manifest["chain_head"],
                manifest_revision=result.manifest["manifest_revision"],
                state="complete",
            )
            return result

    def _execute_locked(
        self,
        wav: WavInfo,
        document: MetadataDocument,
        *,
        target_version: int,
        parameters_version: str,
        cancel_token: CancellationToken | None,
        crash_after_commits: int | None,
        crash_callback: Callable[[], None] | None,
        progress_callback: Callable[[dict[str, Any]], None] | None,
        estimated: int,
    ) -> ArchiveRunResult:
        if cancel_token is not None:
            cancel_token.raise_if_cancelled(document.asset_id)

        def progress(event: str, **fields: Any) -> None:
            if progress_callback is not None:
                progress_callback({"event": event, **fields})

        upgrade = upgrade_to_v3(document, wav, self.config)
        v3 = upgrade.document
        downgrade: DowngradeResult | None = None
        loss_manifest = None
        mapping = list(upgrade.field_mapping)
        if target_version in (1, 2):
            downgrade = downgrade_document(
                v3,
                target_version,
                wav,
                self.config,
                allow_loss=True,
            )
            loss_manifest = downgrade.loss_manifest
            mapping.extend(downgrade.field_mapping)
        elif target_version == 3:
            downgrade = downgrade_document(v3, 3, wav, self.config, allow_loss=True)

        derived = derive_boundaries(v3, self.config)
        boundaries = derived.segments
        boundary_rows = [
            {"index": index, "start_sample": start, "end_sample": end}
            for index, (start, end) in enumerate(boundaries)
        ]
        boundaries_sha = sha256_bytes(canonical_json_bytes(boundary_rows))
        manifest = _load_and_validate_manifest(
            self.workspace,
            document.asset_id,
            verify_segments=True,
        )
        recovery_info: dict[str, Any] = {
            "manifest_created": manifest is None,
            "manifest_revision_before": 0,
            "manifest_revision_after": 0,
            "parameters_version_before": None,
            "parameters_version_after": parameters_version,
            "parameters_version_changed": False,
            "committed_before": [],
            "skipped_committed": [],
            "computed_uncommitted": [],
            "orphan_segment_files": [],
        }
        if manifest is None:
            manifest = _new_manifest(
                document.asset_id,
                wav.file_sha256,
                parameters_version,
                target_version,
                boundaries,
                derived.source,
                v3.archive_checksum,
            )
            _write_manifest(self.workspace, manifest)
            self._record(
                "manifest_update",
                asset_id=document.asset_id,
                manifest_revision=manifest["manifest_revision"],
                chain_head=manifest["chain_head"],
                phase="create",
            )
            progress(
                "manifest_created",
                manifest_revision=manifest["manifest_revision"],
                total_segments=len(boundaries),
            )
        else:
            recovery_info["manifest_revision_before"] = int(manifest["manifest_revision"])
            recovery_info["parameters_version_before"] = manifest.get("parameters_version")
            if manifest.get("source_wav_sha256") != wav.file_sha256:
                raise _recovery_error(
                    "source WAV hash differs from the archived manifest",
                    "manifest.source_wav_sha256",
                    expected=manifest.get("source_wav_sha256"),
                    actual=wav.file_sha256,
                )
            if manifest.get("boundaries_sha256") != boundaries_sha:
                raise _recovery_error(
                    "boundary table changed after committed segments exist",
                    "manifest.boundaries_sha256",
                    expected=manifest.get("boundaries_sha256"),
                    actual=boundaries_sha,
                )
            recovery_info["parameters_version_changed"] = (
                manifest.get("parameters_version") != parameters_version
            )
            manifest["metadata_version"] = target_version
            manifest["metadata_checksum"] = v3.archive_checksum
            self._record(
                "recover_scan",
                asset_id=document.asset_id,
                manifest_revision=int(manifest["manifest_revision"]),
                committed_count=len(manifest.get("segments", [])),
                parameters_version_changed=recovery_info["parameters_version_changed"],
            )
            progress(
                "recover_scan",
                manifest_revision=int(manifest["manifest_revision"]),
                committed=len(manifest.get("segments", [])),
                total_segments=len(boundaries),
            )

        committed_entries = tuple(manifest.get("segments", []))
        committed_indices = {int(entry["index"]): entry for entry in committed_entries}
        recovery_info["committed_before"] = sorted(committed_indices)
        for entry in committed_entries:
            recovery_info["skipped_committed"].append(
                {
                    "segment_index": int(entry["index"]),
                    "content_sha256": entry["content_sha256"],
                    "chain_hash": entry["chain_hash"],
                }
            )
            self._record(
                "recover_skip",
                asset_id=document.asset_id,
                segment_index=int(entry["index"]),
                hash=entry["chain_hash"],
                metadata_version=entry.get("metadata_version"),
                state="committed",
            )
            progress(
                "recover_skip",
                segment_index=int(entry["index"]),
                committed=len(committed_indices),
                total_segments=len(boundaries),
            )
        for index in range(len(boundaries)):
            path = segment_file(self.workspace, document.asset_id, index)
            if index not in committed_indices and path.exists():
                recovery_info["orphan_segment_files"].append(index)

        analyzer = LoudnessAnalyzer(
            wav,
            self.config,
            default_downmix_weights(wav.channel_layout, wav.channel_names),
        )
        uncommitted = [index for index in range(len(boundaries)) if index not in committed_indices]
        analyses_by_index: dict[int, SegmentAnalysis] = {}
        for index, entry in committed_indices.items():
            payload = read_json(segment_file(self.workspace, document.asset_id, index))
            analyses_by_index[index] = _analysis_from_payload(payload)

        workers = min(self.config.max_parallel, max(1, len(uncommitted)))
        commit_counter = 0
        if uncommitted:
            with ThreadPoolExecutor(
                max_workers=workers,
                thread_name_prefix=f"loudness-{document.asset_id}",
            ) as executor:
                futures = {}
                for index in uncommitted:
                    if cancel_token is not None:
                        cancel_token.raise_if_cancelled(document.asset_id)
                    start, end = boundaries[index]
                    self._record(
                        "segment_start",
                        asset_id=document.asset_id,
                        segment_index=index,
                        start_sample=start,
                        end_sample=end,
                        parameters_version=parameters_version,
                    )
                    future = executor.submit(
                        analyzer.analyze_segment,
                        index,
                        start,
                        end,
                        float(v3.limits()[index]),
                        derived.source,
                    )
                    futures[index] = future
                for index in uncommitted:
                    if cancel_token is not None:
                        cancel_token.raise_if_cancelled(document.asset_id)
                    analysis = futures[index].result()
                    analyses_by_index[index] = analysis
                    recovery_info["computed_uncommitted"].append(index)
                    self._record(
                        "recover_compute",
                        asset_id=document.asset_id,
                        segment_index=index,
                        start_sample=analysis.start_sample,
                        end_sample=analysis.end_sample,
                        short_term_lufs=round_db(analysis.short_term_lufs),
                        true_peak_dbtp=round_db(analysis.true_peak_dbtp),
                        over_limit=analysis.over_limit,
                        parameters_version=parameters_version,
                    )
                    progress(
                        "recover_compute",
                        segment_index=index,
                        committed=len(manifest.get("segments", [])),
                        total_segments=len(boundaries),
                    )
                    entry = _commit_segment(
                        self.workspace,
                        manifest,
                        analysis,
                        target_version,
                        parameters_version,
                    )
                    commit_counter += 1
                    self._record(
                        "segment_commit",
                        asset_id=document.asset_id,
                        segment_index=index,
                        hash=entry["chain_hash"],
                        content_sha256=entry["content_sha256"],
                        metadata_version=target_version,
                        parameters_version=parameters_version,
                        over_limit=analysis.over_limit,
                        state="committed",
                    )
                    self._record(
                        "manifest_update",
                        asset_id=document.asset_id,
                        segment_index=index,
                        hash=entry["chain_hash"],
                        manifest_revision=manifest["manifest_revision"],
                        chain_head=manifest["chain_head"],
                        phase="segment",
                    )
                    progress(
                        "segment_commit",
                        segment_index=index,
                        committed=len(manifest.get("segments", [])),
                        total_segments=len(boundaries),
                        chain_head=manifest["chain_head"],
                    )
                    if (
                        crash_after_commits is not None
                        and commit_counter >= crash_after_commits
                        and crash_callback is not None
                    ):
                        crash_callback()

        analyses = tuple(analyses_by_index[index] for index in range(len(boundaries)))
        recovery_info["manifest_revision_after"] = int(manifest["manifest_revision"])
        recovery_info["chain_head_after"] = manifest["chain_head"]
        progress(
            "complete",
            committed=len(manifest.get("segments", [])),
            total_segments=len(boundaries),
            chain_head=manifest["chain_head"],
        )
        return ArchiveRunResult(
            asset_id=document.asset_id,
            target_version=target_version,
            parameters_version=parameters_version,
            manifest=manifest,
            analyses=analyses,
            upgrade=upgrade,
            downgrade=downgrade,
            recovery=recovery_info,
            field_mapping=tuple(mapping),
            loss_manifest=loss_manifest,
            estimated_memory_bytes=estimated,
            manifest_path=str(manifest_file(self.workspace, document.asset_id)),
        )

