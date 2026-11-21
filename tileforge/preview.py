"""Published-version preview frames and offline/preview consistency checks."""

from __future__ import annotations

import base64
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import E_PREVIEW_MISMATCH, E_VERSION_STALE, error
from .model import TileBlock
from .store import Snapshot, VersionStore


def preview_frame(workspace: str | Path, expected_version_hash: str | None = None) -> dict[str, Any]:
    snapshot = VersionStore(workspace).load_current(expected_version_hash)
    tiles = []
    for block in snapshot.sequence.ordered_blocks():
        record = next(
            (item for item in snapshot.manifest.get("tile_records", []) if int(item["tile_id"]) == block.tile_id),
            {},
        )
        tiles.append(
            {
                "tile_id": block.tile_id,
                "x": block.x,
                "y": block.y,
                "width": block.width,
                "height": block.height,
                "exposure_stops": block.exposure,
                "content_hash": block.content_hash.hex(),
                "payload_hash": record.get("payload_hash"),
                "max_code_delta": record.get("max_code_delta"),
                "downgrade_stage": record.get("downgrade_stage", record.get("profile")),
                "reused": record.get("reused", False),
            }
        )
    return {
        "ok": True,
        "version_id": snapshot.version_id,
        "version_hash": snapshot.version_hash,
        "sync_marker": snapshot.manifest.get("sync_marker"),
        "observation": snapshot.manifest.get("observation"),
        "bit_depth": snapshot.sequence.header.bit_depth,
        "tile_count": len(tiles),
        "tiles": tiles,
    }


@dataclass
class PreviewCursor:
    workspace: str | Path
    expected_version_hash: str | None = None
    snapshot: Snapshot | None = None
    cursor: int = 0
    frames: list[dict[str, Any]] = field(default_factory=list)

    def refresh(self) -> dict[str, Any]:
        snapshot = VersionStore(self.workspace).load_current(self.expected_version_hash)
        if self.snapshot is None:
            self.snapshot = snapshot
            if self.expected_version_hash is None:
                self.expected_version_hash = snapshot.version_hash
        elif snapshot.version_hash != self.expected_version_hash:
            raise error(
                E_VERSION_STALE,
                "published version changed during a preview comparison",
                field="version_hash",
                actual=snapshot.version_hash,
                expected=self.expected_version_hash,
            )
        self.snapshot = snapshot
        frame = preview_frame(self.workspace, self.expected_version_hash)
        self.frames = frame["tiles"]
        return frame

    def advance(self) -> dict[str, Any] | None:
        if self.snapshot is None:
            self.refresh()
        if self.cursor >= len(self.frames):
            return None
        tile = self.frames[self.cursor]
        self.cursor += 1
        return tile

    def advance_to_end(self, max_steps: int | None = None) -> list[dict[str, Any]]:
        if self.snapshot is None:
            self.refresh()
        steps = 0
        advanced: list[dict[str, Any]] = []
        while self.cursor < len(self.frames):
            if max_steps is not None and steps >= max_steps:
                break
            tile = self.advance()
            if tile is not None:
                advanced.append(tile)
            steps += 1
        return advanced

    @property
    def at_end(self) -> bool:
        return self.snapshot is not None and self.cursor >= len(self.frames)

    def compare_offline(self, report: dict[str, Any]) -> dict[str, Any]:
        if self.snapshot is None:
            self.refresh()
        assert self.snapshot is not None
        if not self.at_end:
            raise error(
                E_PREVIEW_MISMATCH,
                "preview has not advanced to the end",
                field="cursor",
                cursor=self.cursor,
                tile_count=len(self.frames),
            )
        if report.get("kind") != "tileforge-offline-report":
            raise error(E_PREVIEW_MISMATCH, "offline report kind mismatch", field="kind", actual=report.get("kind"))
        if report.get("sync_marker") != self.snapshot.manifest.get("sync_marker"):
            raise error(
                E_PREVIEW_MISMATCH,
                "preview and offline sync markers differ",
                field="sync_marker",
                actual=self.snapshot.manifest.get("sync_marker"),
                expected=report.get("sync_marker"),
                tile_id=None,
            )
        if report.get("observation") != self.snapshot.manifest.get("observation"):
            raise error(
                E_PREVIEW_MISMATCH,
                "preview and offline observation windows differ",
                field="observation",
                actual=self.snapshot.manifest.get("observation"),
                expected=report.get("observation"),
                tile_id=None,
            )
        offline_tiles = {int(item["tile_id"]): item for item in report.get("tiles", [])}
        preview_ids = set(self.snapshot.sequence.tile_ids())
        if set(offline_tiles) != preview_ids:
            raise error(
                E_PREVIEW_MISMATCH,
                "offline report tile coverage differs from the preview",
                field="tile_id",
                missing=sorted(preview_ids - set(offline_tiles)),
                extra=sorted(set(offline_tiles) - preview_ids),
            )
        tolerance = int(report.get("observation", {}).get("tolerance_code", 0))
        compared = 0
        for block in self.snapshot.sequence.ordered_blocks():
            expected = offline_tiles.get(block.tile_id)
            if expected is None:
                raise error(
                    E_PREVIEW_MISMATCH,
                    "offline report is missing a tile",
                    field="tile_id",
                    tile_id=block.tile_id,
                )
            if block.content_hash.hex() != expected.get("content_hash"):
                try:
                    offline_payload = base64.b64decode(str(expected["payload_b64"]), validate=True)
                except (KeyError, ValueError, TypeError) as exc:
                    raise error(
                        E_PREVIEW_MISMATCH,
                        "offline tile payload is missing or invalid",
                        field="payload_b64",
                        tile_id=block.tile_id,
                    ) from exc
                if len(offline_payload) != len(block.data):
                    raise error(
                        E_PREVIEW_MISMATCH,
                        "preview and offline payload lengths differ",
                        field="payload_length",
                        tile_id=block.tile_id,
                        actual=len(block.data),
                        expected=len(offline_payload),
                    )
                bit_depth = self.snapshot.sequence.header.bit_depth
                bytes_per = 1 if bit_depth == 8 else 2
                max_delta = 0
                first_mismatch: dict[str, Any] | None = None
                for offset in range(0, len(block.data), bytes_per):
                    actual = int.from_bytes(block.data[offset : offset + bytes_per], "little")
                    expected_code = int.from_bytes(offline_payload[offset : offset + bytes_per], "little")
                    delta = abs(actual - expected_code)
                    max_delta = max(max_delta, delta)
                    if delta > tolerance and first_mismatch is None:
                        sample_index = offset // bytes_per
                        first_mismatch = {
                            "tile_id": block.tile_id,
                            "field": "sample_value",
                            "sample_index": sample_index,
                            "channel": sample_index % self.snapshot.sequence.header.channels,
                            "pixel_index": sample_index // self.snapshot.sequence.header.channels,
                            "offset": offset,
                            "actual": actual,
                            "expected": expected_code,
                            "delta": delta,
                            "tolerance": tolerance,
                        }
                if first_mismatch is not None:
                    raise error(
                        E_PREVIEW_MISMATCH,
                        "preview and offline tile exceed code tolerance",
                        **first_mismatch,
                    )
            try:
                expected_exposure = float(expected.get("exposure_stops"))
            except (TypeError, ValueError) as exc:
                raise error(
                    E_PREVIEW_MISMATCH,
                    "offline tile exposure is missing or invalid",
                    field="exposure_stops",
                    tile_id=block.tile_id,
                ) from exc
            if block.exposure != expected_exposure:
                raise error(
                    E_PREVIEW_MISMATCH,
                    "preview and offline exposure differ",
                    field="exposure_stops",
                    tile_id=block.tile_id,
                    actual=block.exposure,
                    expected=expected_exposure,
                )
            preview_record = next(
                (
                    item
                    for item in self.snapshot.manifest.get("tile_records", [])
                    if int(item["tile_id"]) == block.tile_id
                ),
                {},
            )
            if preview_record.get("max_code_delta") != expected.get("max_code_delta"):
                raise error(
                    E_PREVIEW_MISMATCH,
                    "preview and offline max code deviation differ",
                    field="max_code_delta",
                    tile_id=block.tile_id,
                    actual=preview_record.get("max_code_delta"),
                    expected=expected.get("max_code_delta"),
                )
            compared += 1
        return {
            "ok": True,
            "compared_tiles": compared,
            "version_id": self.snapshot.version_id,
            "version_hash": self.snapshot.version_hash,
            "offline_output_hash": report.get("output_hash"),
            "sync_marker": report.get("sync_marker"),
            "tolerance_code": tolerance,
        }


def load_offline_report(path: str | Path) -> dict[str, Any]:
    try:
        report = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError, UnicodeError) as exc:
        raise error(E_PREVIEW_MISMATCH, "cannot load offline report", field="report", path=str(path), reason=str(exc)) from exc
    if not isinstance(report, dict):
        raise error(E_PREVIEW_MISMATCH, "offline report must be an object", field="report")
    return report
