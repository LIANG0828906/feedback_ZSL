from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .cache import TierCacheState
from .errors import ErrorCode, fail
from .generator import atomic_write, parse_slice_file
from .models import (
    GENESIS_HASH,
    ProjectConfig,
    SliceArtifact,
    SourceTimeline,
    canonical_json,
    sha256_json,
)

SNAPSHOT_SCHEMA = "streamforge.snapshot"
SNAPSHOT_VERSION = 1


@dataclass
class RestoreResult:
    states: dict[str, TierCacheState]
    dirty_tiers: set[str]
    timeline_changed: bool
    config_version: str
    memory: dict[str, Any]
    waiting_queue: list[dict[str, Any]]
    snapshot: dict[str, Any] = field(default_factory=dict)


def _artifact_file(output_root: Path, artifact: SliceArtifact) -> str:
    try:
        return str(artifact.path.relative_to(output_root))
    except ValueError:
        return str(artifact.path)


def build_snapshot(
    *,
    project: ProjectConfig,
    timeline: SourceTimeline,
    states: dict[str, TierCacheState],
    memory_samples: list[dict[str, int]],
    peak_bytes: int,
    waiting_queue: list[dict[str, Any]],
    output_root: Path,
    next_submit_seq: int,
) -> dict[str, Any]:
    tier_documents: dict[str, Any] = {}
    for tier in project.tiers:
        state = states[tier.tier_id]
        completed = []
        previous = GENESIS_HASH
        for artifact in state.artifacts:
            if artifact.prev_hash != previous:
                raise fail(
                    ErrorCode.RECOVERY_INVALID,
                    "cannot snapshot a broken hash chain",
                    tier=tier.tier_id,
                    index=artifact.index,
                )
            completed.append(
                {
                    "content_hash": artifact.content_hash,
                    "end_ns": artifact.end_ns,
                    "file": _artifact_file(output_root, artifact),
                    "index": artifact.index,
                    "prev_hash": artifact.prev_hash,
                    "start_ns": artifact.start_ns,
                }
            )
            previous = artifact.content_hash
        tier_documents[tier.tier_id] = {
            "chain_head": previous,
            "completed": completed,
            "config": tier.to_dict(),
            "error": state.error,
            "status": state.status,
        }
    document: dict[str, Any] = {
        "config_version": project.config_version,
        "memory": {
            "current_bytes": memory_samples[-1]["allocated_bytes"]
            if memory_samples
            else 0,
            "peak_bytes": peak_bytes,
            "samples": memory_samples,
        },
        "scheduler": {
            "next_submit_seq": next_submit_seq,
            "waiting_queue": waiting_queue,
        },
        "schema": SNAPSHOT_SCHEMA,
        "tiers": tier_documents,
        "timeline": {
            "boundaries": timeline.boundary_dicts(),
            "digest": timeline.digest,
            "slice_duration_ns": timeline.slice_duration_ns,
            "total_ns": timeline.total_ns,
        },
        "version": SNAPSHOT_VERSION,
    }
    document["snapshot_hash"] = sha256_json(document)
    return document


def save_snapshot(path: Path, document: dict[str, Any]) -> None:
    atomic_write(path, canonical_json(document) + b"\n")


def _require(mapping: dict[str, Any], key: str, label: str) -> Any:
    if key not in mapping:
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            f"snapshot missing {label}",
            field=key,
        )
    return mapping[key]


def load_snapshot(
    path: Path,
    *,
    project: ProjectConfig,
    timeline: SourceTimeline,
    output_root: Path,
) -> RestoreResult:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "snapshot file not found",
            path=str(path),
        ) from exc
    except (OSError, json.JSONDecodeError) as exc:
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "cannot parse snapshot",
            path=str(path),
            error=str(exc),
        ) from exc
    if not isinstance(document, dict):
        raise fail(ErrorCode.RECOVERY_INVALID, "snapshot must be an object")
    if document.get("schema") != SNAPSHOT_SCHEMA:
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "snapshot schema mismatch",
            expected=SNAPSHOT_SCHEMA,
            actual=document.get("schema"),
        )
    if document.get("version") != SNAPSHOT_VERSION:
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "snapshot version mismatch",
            expected=SNAPSHOT_VERSION,
            actual=document.get("version"),
        )
    expected_snapshot_hash = document.get("snapshot_hash")
    unsigned = dict(document)
    unsigned.pop("snapshot_hash", None)
    if expected_snapshot_hash != sha256_json(unsigned):
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "snapshot integrity hash mismatch",
        )

    timeline_doc = _require(document, "timeline", "timeline")
    tiers_doc = _require(document, "tiers", "tiers")
    memory = _require(document, "memory", "memory")
    scheduler = _require(document, "scheduler", "scheduler")
    config_version = _require(document, "config_version", "config_version")
    if not isinstance(timeline_doc, dict) or not isinstance(tiers_doc, dict):
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "snapshot timeline and tiers must be objects",
        )
    if not isinstance(memory, dict) or not isinstance(scheduler, dict):
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "snapshot memory and scheduler must be objects",
        )
    _require(memory, "peak_bytes", "memory.peak_bytes")
    _require(memory, "current_bytes", "memory.current_bytes")
    _require(memory, "samples", "memory.samples")
    _require(scheduler, "waiting_queue", "scheduler.waiting_queue")
    _require(scheduler, "next_submit_seq", "scheduler.next_submit_seq")
    _require(timeline_doc, "boundaries", "timeline.boundaries")
    _require(timeline_doc, "digest", "timeline.digest")
    if not isinstance(scheduler["waiting_queue"], list):
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "snapshot waiting queue must be an array",
        )

    snapshot_boundaries = timeline_doc["boundaries"]
    if not isinstance(snapshot_boundaries, list):
        raise fail(
            ErrorCode.RECOVERY_INVALID,
            "snapshot boundaries must be an array",
        )
    snapshot_boundary_by_index = {
        int(item["index"]): item
        for item in snapshot_boundaries
        if isinstance(item, dict) and "index" in item
    }
    timeline_changed = (
        timeline_doc.get("digest") != timeline.digest
        or snapshot_boundaries != timeline.boundary_dicts()
    )
    dirty_tiers: set[str] = set()
    states: dict[str, TierCacheState] = {}
    for tier in project.tiers:
        tier_doc = tiers_doc.get(tier.tier_id)
        if not isinstance(tier_doc, dict):
            dirty_tiers.add(tier.tier_id)
            states[tier.tier_id] = TierCacheState()
            continue
        if tier_doc.get("config") != tier.to_dict() or timeline_changed:
            dirty_tiers.add(tier.tier_id)
        completed = _require(tier_doc, "completed", f"tiers.{tier.tier_id}.completed")
        chain_head = _require(
            tier_doc, "chain_head", f"tiers.{tier.tier_id}.chain_head"
        )
        if not isinstance(completed, list):
            raise fail(
                ErrorCode.RECOVERY_INVALID,
                "tier completed field must be an array",
                tier=tier.tier_id,
            )
        state = TierCacheState(
            status=str(tier_doc.get("status", "pending")),
            error=tier_doc.get("error"),
        )
        previous = GENESIS_HASH
        for expected_index, item in enumerate(completed):
            if not isinstance(item, dict):
                raise fail(
                    ErrorCode.RECOVERY_INVALID,
                    "invalid completed slice record",
                    tier=tier.tier_id,
                    index=expected_index,
                )
            index = int(item.get("index", -1))
            if index != expected_index:
                raise fail(
                    ErrorCode.RECOVERY_INVALID,
                    "snapshot slice indexes are not contiguous",
                    tier=tier.tier_id,
                    expected_index=expected_index,
                    actual_index=index,
                )
            relative_file = item.get("file")
            if not isinstance(relative_file, str):
                raise fail(
                    ErrorCode.RECOVERY_INVALID,
                    "snapshot slice file is missing",
                    tier=tier.tier_id,
                    index=index,
                )
            file_path = Path(relative_file)
            if not file_path.is_absolute():
                file_path = output_root / file_path
            artifact = parse_slice_file(file_path, verify=True)
            boundary = snapshot_boundary_by_index.get(index)
            if boundary is None:
                raise fail(
                    ErrorCode.RECOVERY_INVALID,
                    "snapshot boundary for completed slice is missing",
                    tier=tier.tier_id,
                    index=index,
                )
            if (
                artifact.tier_id != tier.tier_id
                or artifact.index != index
                or artifact.prev_hash != previous
                or artifact.content_hash != item.get("content_hash")
                or artifact.start_ns != int(boundary["start_ns"])
                or artifact.end_ns != int(boundary["end_ns"])
            ):
                raise fail(
                    ErrorCode.RECOVERY_INVALID,
                    "snapshot tier hash chain is broken",
                    tier=tier.tier_id,
                    index=index,
                )
            previous = artifact.content_hash
            state.artifacts.append(artifact)
        if previous != chain_head:
            raise fail(
                ErrorCode.RECOVERY_INVALID,
                "snapshot tier chain head mismatch",
                tier=tier.tier_id,
                expected=chain_head,
                actual=previous,
            )
        if (
            state.status == "complete"
            and len(state.artifacts) != len(snapshot_boundaries)
        ):
            raise fail(
                ErrorCode.RECOVERY_INVALID,
                "complete snapshot tier has an inconsistent slice count",
                tier=tier.tier_id,
                expected=len(snapshot_boundaries),
                actual=len(state.artifacts),
            )
        if state.status == "complete" and timeline_changed:
            dirty_tiers.add(tier.tier_id)
        states[tier.tier_id] = state

    for tier_id in tiers_doc:
        if tier_id not in {tier.tier_id for tier in project.tiers}:
            continue
    return RestoreResult(
        states=states,
        dirty_tiers=dirty_tiers,
        timeline_changed=timeline_changed,
        config_version=str(config_version),
        memory=memory,
        waiting_queue=list(scheduler["waiting_queue"]),
        snapshot=document,
    )
