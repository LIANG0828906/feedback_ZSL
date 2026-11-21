from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .errors import E_RECOVERY_INVALID, PipelineError
from .storage import Store


@dataclass
class RecoveryResult:
    version: int | None
    content_hash: str | None
    manifest: dict[str, Any] | None
    resume_available: bool
    staging_base_version: int | None
    completed_batches: list[str] = field(default_factory=list)
    missing_batches: list[str] = field(default_factory=list)
    discarded_temp_files: list[str] = field(default_factory=list)
    discarded_chunks: list[str] = field(default_factory=list)
    discarded_manifest_files: list[str] = field(default_factory=list)
    discarded_staging: bool = False
    diagnostics: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "version": self.version,
            "content_hash": self.content_hash,
            "resume_available": self.resume_available,
            "staging_base_version": self.staging_base_version,
            "completed_batches": self.completed_batches,
            "missing_batches": self.missing_batches,
            "discarded_temp_files": self.discarded_temp_files,
            "discarded_chunks": self.discarded_chunks,
            "discarded_manifest_files": self.discarded_manifest_files,
            "discarded_staging": self.discarded_staging,
            "diagnostics": self.diagnostics,
        }


def _valid_staged_record(store: Store, record: Any, expected: set[str]) -> bool:
    if not isinstance(record, dict):
        return False
    batch_id = record.get("batch")
    if not isinstance(batch_id, str) or batch_id not in expected:
        return False
    try:
        document = store.load_chunk_document(record)
    except PipelineError:
        return False
    return (
        document.get("batch") == batch_id
        and document.get("layer") == record.get("layer")
        and document.get("factor_versions") == record.get("factor_versions")
        and len(document.get("strokes", [])) == record.get("stroke_count")
    )


def recover(
    store: Store,
    *,
    cleanup: bool = True,
    keep_staging: bool = True,
) -> RecoveryResult:
    manifest = store.current_manifest()
    version = None if manifest is None else int(manifest["version"])
    content_hash = None if manifest is None else str(manifest["content_hash"])
    state = store.read_staging()
    result = RecoveryResult(
        version=version,
        content_hash=content_hash,
        manifest=manifest,
        resume_available=False,
        staging_base_version=None,
    )
    expected: set[str] = set()
    completed_ids: list[str] = []
    keep_files: set[str] = set()
    rewritten_state: dict[str, Any] | None = None
    if state is not None:
        result.staging_base_version = state.get("base_version")
        base_ok = (
            state.get("base_version") == version
            and state.get("base_manifest_hash")
            == (None if manifest is None else manifest["manifest_hash"])
            and state.get("target_version")
            == (1 if version is None else version + 1)
        )
        raw_expected = state.get("expected_batches")
        raw_completed = state.get("completed")
        if (
            base_ok
            and isinstance(raw_expected, list)
            and all(isinstance(item, str) for item in raw_expected)
            and isinstance(raw_completed, list)
        ):
            expected = set(raw_expected)
            valid_completed: list[dict[str, Any]] = []
            for record in raw_completed:
                if _valid_staged_record(store, record, expected):
                    valid_completed.append(record)
                    completed_ids.append(str(record["batch"]))
                    keep_files.add(str(record["file"]))
                else:
                    result.diagnostics.append(
                        f"discarded_invalid_staged_batch:{record.get('batch', '?')}"
                    )
            valid_completed.sort(key=lambda item: str(item["batch"]))
            completed_ids.sort()
            result.completed_batches = completed_ids
            result.missing_batches = sorted(expected - set(completed_ids))
            result.resume_available = True
            rewritten_state = dict(state)
            rewritten_state["completed"] = valid_completed
        else:
            result.discarded_staging = True
            result.diagnostics.append("staging_base_or_shape_mismatch")
    if cleanup:
        if result.resume_available and keep_staging and rewritten_state is not None:
            store.write_staging(rewritten_state)
        else:
            store.clear_staging()
            keep_files.clear()
            result.resume_available = False
            result.discarded_staging = state is not None
        removed = store.cleanup_uncommitted(keep_files if result.resume_available else set())
        for relative in removed:
            if relative.startswith("chunks/"):
                result.discarded_chunks.append(relative)
            elif relative.startswith("manifests/"):
                result.discarded_manifest_files.append(relative)
            else:
                result.discarded_temp_files.append(relative)
        result.discarded_temp_files.sort()
        result.discarded_chunks.sort()
        result.discarded_manifest_files.sort()
    return result


__all__ = ["RecoveryResult", "recover"]
