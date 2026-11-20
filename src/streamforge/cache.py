from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import ErrorCode, fail
from .generator import atomic_write, parse_slice_file, tier_fingerprint
from .models import (
    GENESIS_HASH,
    SliceArtifact,
    SourceTimeline,
    TierSpec,
    canonical_json,
)


@dataclass
class TierCacheState:
    artifacts: list[SliceArtifact] = field(default_factory=list)
    status: str = "pending"
    error: dict[str, Any] | None = None
    reused: bool = False

    @property
    def next_index(self) -> int:
        return len(self.artifacts)

    @property
    def prev_hash(self) -> str:
        return self.artifacts[-1].content_hash if self.artifacts else GENESIS_HASH

    @property
    def complete(self) -> bool:
        return self.status == "complete"


class TierCache:
    def __init__(self, output_root: Path, tier: TierSpec, timeline: SourceTimeline) -> None:
        self.tier = tier
        self.timeline = timeline
        self.directory = output_root / tier.tier_id
        self.directory.mkdir(parents=True, exist_ok=True)
        self.manifest_path = self.directory / "manifest.json"
        self.fingerprint = tier_fingerprint(tier, timeline)

    def clear(self) -> None:
        for path in self.directory.glob("*.fslice"):
            path.unlink(missing_ok=True)
        for path in self.directory.glob(".*.tmp"):
            path.unlink(missing_ok=True)
        self.manifest_path.unlink(missing_ok=True)

    def load(self) -> TierCacheState:
        if not self.manifest_path.exists():
            return TierCacheState()
        try:
            manifest = json.loads(self.manifest_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            raise fail(
                ErrorCode.RECOVERY_INVALID,
                "cannot read tier manifest",
                tier=self.tier.tier_id,
                error=str(exc),
            ) from exc
        if not isinstance(manifest, dict):
            raise fail(
                ErrorCode.RECOVERY_INVALID,
                "tier manifest is not an object",
                tier=self.tier.tier_id,
            )
        if (
            manifest.get("fingerprint") != self.fingerprint
            or manifest.get("timeline_digest") != self.timeline.digest
        ):
            self.clear()
            return TierCacheState()
        completed = manifest.get("completed")
        if not isinstance(completed, list):
            raise fail(
                ErrorCode.RECOVERY_INVALID,
                "tier manifest completed list is missing",
                tier=self.tier.tier_id,
            )
        state = TierCacheState(
            status=str(manifest.get("status", "pending")),
            error=manifest.get("error"),
            reused=True,
        )
        previous = GENESIS_HASH
        for expected_index, item in enumerate(completed):
            if not isinstance(item, dict):
                raise fail(
                    ErrorCode.RECOVERY_INVALID,
                    "invalid completed slice entry",
                    tier=self.tier.tier_id,
                    index=expected_index,
                )
            index = int(item.get("index", -1))
            if index != expected_index or expected_index >= len(self.timeline.boundaries):
                raise fail(
                    ErrorCode.RECOVERY_INVALID,
                    "tier cache chain is not contiguous",
                    tier=self.tier.tier_id,
                    expected_index=expected_index,
                    actual_index=index,
                )
            path = self.directory / f"slice-{index:05d}.fslice"
            artifact = parse_slice_file(path, verify=True)
            boundary = self.timeline.boundaries[index]
            if (
                artifact.tier_id != self.tier.tier_id
                or artifact.index != index
                or artifact.prev_hash != previous
                or artifact.start_ns != boundary.start_ns
                or artifact.end_ns != boundary.end_ns
                or artifact.content_hash != item.get("content_hash")
            ):
                raise fail(
                    ErrorCode.RECOVERY_INVALID,
                    "tier cache hash chain or boundary mismatch",
                    tier=self.tier.tier_id,
                    index=index,
                )
            state.artifacts.append(artifact)
            previous = artifact.content_hash
        if state.status == "complete" and len(state.artifacts) != len(
            self.timeline.boundaries
        ):
            raise fail(
                ErrorCode.RECOVERY_INVALID,
                "complete tier cache has wrong slice count",
                tier=self.tier.tier_id,
            )
        return state

    def save(self, state: TierCacheState, config_version: str) -> None:
        manifest = {
            "completed": [
                {
                    "content_hash": artifact.content_hash,
                    "end_ns": artifact.end_ns,
                    "index": artifact.index,
                    "start_ns": artifact.start_ns,
                }
                for artifact in state.artifacts
            ],
            "config_version": config_version,
            "error": state.error,
            "fingerprint": self.fingerprint,
            "schema": "streamforge.tier-manifest",
            "status": state.status,
            "tier": self.tier.to_dict(),
            "timeline_digest": self.timeline.digest,
            "version": 1,
        }
        atomic_write(self.manifest_path, canonical_json(manifest) + b"\n")
