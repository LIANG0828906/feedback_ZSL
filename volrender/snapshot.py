"""Atomic, hash-chained render snapshots and recovery."""

from __future__ import annotations

import base64
import hashlib
import json
import os
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .config import RenderConfig
from .errors import AppError, ErrorCode
from .render import TileResult, TileSpec
from .scene import Scene


SNAPSHOT_VERSION = 1
_GENESIS = "0" * 64


@dataclass(slots=True)
class SnapshotLoad:
    path: Path
    completed: dict[int, TileResult]
    document: dict[str, Any]


def _canonical_bytes(value: Any) -> bytes:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
        allow_nan=False,
    ).encode("utf-8")


def _tile_payload(result: TileResult) -> bytes:
    return (
        np.ascontiguousarray(result.color, dtype="<f8").tobytes()
        + np.ascontiguousarray(result.transmittance, dtype="<f8").tobytes()
    )


def _entry_metadata(
    result: TileResult, color_hash: str, transmittance_hash: str
) -> dict[str, Any]:
    tile = result.tile
    return {
        "tile_index": tile.index,
        "seq": tile.seq,
        "x": tile.x0,
        "y": tile.y0,
        "w": tile.width,
        "h": tile.height,
        "stage_index": result.stage_index,
        "stage": result.stage,
        "step_scale": result.step_scale,
        "estimated_samples": result.estimated_samples,
        "samples_used": result.samples_used,
        "color_hash": color_hash,
        "transmittance_hash": transmittance_hash,
    }


def build_snapshot_document(
    scene: Scene,
    config: RenderConfig,
    precision: str,
    results: dict[int, TileResult],
) -> dict[str, Any]:
    entries: list[dict[str, Any]] = []
    previous_hash = _GENESIS
    for tile_index in sorted(results):
        result = results[tile_index]
        payload = _tile_payload(result)
        color_bytes = np.ascontiguousarray(
            result.color, dtype="<f8"
        ).tobytes()
        transmittance_bytes = np.ascontiguousarray(
            result.transmittance, dtype="<f8"
        ).tobytes()
        color_hash = hashlib.sha256(color_bytes).hexdigest()
        transmittance_hash = hashlib.sha256(
            transmittance_bytes
        ).hexdigest()
        metadata = _entry_metadata(
            result, color_hash, transmittance_hash
        )
        chain_hash = hashlib.sha256(
            previous_hash.encode("ascii") + _canonical_bytes(metadata)
        ).hexdigest()
        entry = {
            **metadata,
            "payload_b64": base64.b64encode(payload).decode("ascii"),
            "prev_hash": previous_hash,
            "chain_hash": chain_hash,
        }
        entries.append(entry)
        previous_hash = chain_hash
    chunk_table = [
        {
            key: entry[key]
            for key in (
                "tile_index",
                "seq",
                "x",
                "y",
                "w",
                "h",
                "stage_index",
                "stage",
                "step_scale",
                "estimated_samples",
                "samples_used",
                "color_hash",
                "transmittance_hash",
                "prev_hash",
                "chain_hash",
            )
        }
        for entry in entries
    ]
    return {
        "snapshot_version": SNAPSHOT_VERSION,
        "scene_id": scene.scene_id,
        "scene_fingerprint": scene.fingerprint,
        "density_version": scene.density_version,
        "config_digest": config.digest(),
        "budget_config": config.to_dict(),
        "precision": precision,
        "chunk_table": chunk_table,
        "completed_chunk_indices": sorted(results),
        "chain_head": previous_hash,
        "entries": entries,
    }


def save_snapshot_atomic(
    path: str | Path,
    scene: Scene,
    config: RenderConfig,
    precision: str,
    results: dict[int, TileResult],
) -> Path:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    document = build_snapshot_document(
        scene, config, precision, results
    )
    raw = _canonical_bytes(document) + b"\n"
    fd, temporary_name = tempfile.mkstemp(
        prefix=destination.name + ".",
        suffix=".tmp",
        dir=destination.parent,
    )
    temporary = Path(temporary_name)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(raw)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
        directory_fd = os.open(destination.parent, os.O_RDONLY)
        try:
            os.fsync(directory_fd)
        finally:
            os.close(directory_fd)
    except Exception:
        temporary.unlink(missing_ok=True)
        raise
    return destination


class SnapshotStore:
    """Versioned snapshot directory with latest-file recovery."""

    def __init__(self, directory: str | Path):
        self.directory = Path(directory)

    def write(
        self,
        scene: Scene,
        config: RenderConfig,
        precision: str,
        results: dict[int, TileResult],
    ) -> Path:
        document = build_snapshot_document(
            scene, config, precision, results
        )
        name = (
            f"snapshot-{len(results):06d}-"
            f"{document['chain_head'][:12]}.snapshot.json"
        )
        return save_snapshot_atomic(
            self.directory / name,
            scene,
            config,
            precision,
            results,
        )

    def latest_path(self) -> Path | None:
        if not self.directory.exists():
            return None
        candidates = sorted(self.directory.glob("*.snapshot.json"))
        return candidates[-1] if candidates else None

    def load_latest(
        self,
        scene: Scene,
        config: RenderConfig,
        precision: str,
    ) -> SnapshotLoad | None:
        path = self.latest_path()
        if path is None:
            return None
        return load_snapshot(path, scene, config, precision)


class SnapshotCheckpoint:
    def __init__(
        self,
        store: SnapshotStore,
        scene: Scene,
        config: RenderConfig,
        precision: str,
    ):
        self.store = store
        self.scene = scene
        self.config = config
        self.precision = precision
        self.last_path: Path | None = None

    def __call__(
        self,
        results: dict[int, TileResult],
        trace: list[dict[str, Any]],
    ) -> None:
        _ = trace
        self.last_path = self.store.write(
            self.scene,
            self.config,
            self.precision,
            results,
        )


def _recovery_error(message: str, **details: Any) -> AppError:
    return AppError(
        ErrorCode.E_RECOVERY_INVALID,
        message,
        field="snapshot",
        details=details,
    )


def _required(container: dict[str, Any], key: str) -> Any:
    if key not in container:
        raise _recovery_error(
            f"snapshot field missing: {key}",
            missing_field=key,
        )
    return container[key]


def _decode_entry(
    entry: dict[str, Any],
    scene: Scene,
    config: RenderConfig,
) -> TileResult:
    tile_index = int(_required(entry, "tile_index"))
    seq = int(_required(entry, "seq"))
    x = int(_required(entry, "x"))
    y = int(_required(entry, "y"))
    w = int(_required(entry, "w"))
    h = int(_required(entry, "h"))
    if w <= 0 or h <= 0 or x < 0 or y < 0:
        raise _recovery_error(
            "invalid tile geometry in snapshot",
            tile_index=tile_index,
        )
    if x + w > scene.width or y + h > scene.height:
        raise _recovery_error(
            "tile geometry exceeds image bounds",
            tile_index=tile_index,
        )
    stage_index = int(_required(entry, "stage_index"))
    if not 0 <= stage_index < len(config.degradation_sequence):
        raise _recovery_error(
            "snapshot stage index is outside configuration",
            tile_index=tile_index,
        )
    stage = config.degradation_sequence[stage_index]
    if entry.get("stage") != stage.name:
        raise _recovery_error(
            "snapshot stage name does not match configuration",
            tile_index=tile_index,
        )
    if abs(float(entry.get("step_scale", -1)) - stage.step_scale) > 1e-15:
        raise _recovery_error(
            "snapshot step scale does not match configuration",
            tile_index=tile_index,
        )
    samples_used = int(_required(entry, "samples_used"))
    estimated = int(_required(entry, "estimated_samples"))
    if samples_used > config.sample_budget:
        raise _recovery_error(
            "snapshot tile exceeds configured sample budget",
            tile_index=tile_index,
            samples_used=samples_used,
            sample_budget=config.sample_budget,
        )
    if samples_used != estimated:
        raise _recovery_error(
            "snapshot sample usage does not match preflight estimate",
            tile_index=tile_index,
        )
    payload_b64 = _required(entry, "payload_b64")
    try:
        payload = base64.b64decode(payload_b64, validate=True)
    except Exception as exc:
        raise _recovery_error(
            "snapshot payload is not valid base64",
            tile_index=tile_index,
        ) from exc
    expected_size = w * h * 3 * 8 + w * h * 8
    if len(payload) != expected_size:
        raise _recovery_error(
            "snapshot payload size does not match tile geometry",
            tile_index=tile_index,
            expected=expected_size,
            actual=len(payload),
        )
    color_size = w * h * 3 * 8
    color = np.frombuffer(
        payload[:color_size], dtype="<f8"
    ).reshape(h, w, 3).copy()
    transmittance = np.frombuffer(
        payload[color_size:], dtype="<f8"
    ).reshape(h, w).copy()
    color_hash = hashlib.sha256(
        np.ascontiguousarray(color, dtype="<f8").tobytes()
    ).hexdigest()
    transmittance_hash = hashlib.sha256(
        np.ascontiguousarray(transmittance, dtype="<f8").tobytes()
    ).hexdigest()
    if color_hash != entry.get("color_hash"):
        raise _recovery_error(
            "snapshot color hash mismatch",
            tile_index=tile_index,
        )
    if transmittance_hash != entry.get("transmittance_hash"):
        raise _recovery_error(
            "snapshot transmittance hash mismatch",
            tile_index=tile_index,
        )
    result = TileResult(
        tile=TileSpec(
            index=tile_index,
            seq=seq,
            x0=x,
            y0=y,
            x1=x + w,
            y1=y + h,
        ),
        stage_index=stage_index,
        stage=stage.name,
        step_scale=stage.step_scale,
        estimated_samples=estimated,
        samples_used=samples_used,
        color=color,
        transmittance=transmittance,
        precision="snapshot",
    )
    return result


def load_snapshot(
    path: str | Path,
    scene: Scene,
    config: RenderConfig,
    precision: str,
) -> SnapshotLoad:
    snapshot_path = Path(path)
    try:
        raw = snapshot_path.read_bytes()
    except OSError as exc:
        raise AppError(
            ErrorCode.E_IO,
            f"cannot read snapshot: {exc}",
            field="snapshot.path",
        ) from exc
    try:
        document = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise _recovery_error(
            "snapshot is not valid UTF-8 JSON",
            path=str(snapshot_path),
        ) from exc
    if not isinstance(document, dict):
        raise _recovery_error("snapshot root must be an object")
    if _required(document, "snapshot_version") != SNAPSHOT_VERSION:
        raise _recovery_error(
            "unsupported snapshot version",
            expected=SNAPSHOT_VERSION,
            actual=document.get("snapshot_version"),
        )
    if document.get("scene_id") != scene.scene_id:
        raise _recovery_error(
            "snapshot scene id mismatch",
            expected=scene.scene_id,
            actual=document.get("scene_id"),
        )
    if document.get("scene_fingerprint") != scene.fingerprint:
        raise _recovery_error(
            "snapshot scene fingerprint mismatch",
            expected=scene.fingerprint,
            actual=document.get("scene_fingerprint"),
        )
    if document.get("density_version") != scene.density_version:
        raise _recovery_error(
            "snapshot density version mismatch",
            expected=scene.density_version,
            actual=document.get("density_version"),
        )
    if document.get("config_digest") != config.digest():
        raise _recovery_error("snapshot config digest mismatch")
    if document.get("budget_config") != config.to_dict():
        raise _recovery_error("snapshot budget configuration mismatch")
    if document.get("precision") != precision:
        raise _recovery_error(
            "snapshot precision mode mismatch",
            expected=precision,
            actual=document.get("precision"),
        )

    entries = _required(document, "entries")
    chunk_table = _required(document, "chunk_table")
    completed_indices = _required(document, "completed_chunk_indices")
    chain_head = _required(document, "chain_head")
    if not isinstance(entries, list) or not isinstance(chunk_table, list):
        raise _recovery_error("snapshot chunk structures must be arrays")
    if not isinstance(completed_indices, list):
        raise _recovery_error(
            "completed_chunk_indices must be an array"
        )
    if completed_indices != sorted(completed_indices):
        raise _recovery_error(
            "completed_chunk_indices must be sorted"
        )
    if len(set(completed_indices)) != len(completed_indices):
        raise _recovery_error(
            "completed_chunk_indices must be unique"
        )
    if completed_indices != [entry.get("tile_index") for entry in entries]:
        raise _recovery_error(
            "completed_chunk_indices do not match entries"
        )
    if len(chunk_table) != len(entries):
        raise _recovery_error(
            "chunk_table length does not match entries"
        )

    previous_hash = _GENESIS
    completed: dict[int, TileResult] = {}
    for index, entry in enumerate(entries):
        if not isinstance(entry, dict):
            raise _recovery_error(
                "snapshot entry must be an object", entry_index=index
            )
        for required_key in (
            "tile_index",
            "seq",
            "x",
            "y",
            "w",
            "h",
            "stage_index",
            "stage",
            "step_scale",
            "estimated_samples",
            "samples_used",
            "color_hash",
            "transmittance_hash",
            "payload_b64",
            "prev_hash",
            "chain_hash",
        ):
            _required(entry, required_key)
        if entry.get("prev_hash") != previous_hash:
            raise _recovery_error(
                "snapshot hash chain is broken",
                entry_index=index,
                expected_prev_hash=previous_hash,
                actual_prev_hash=entry.get("prev_hash"),
            )
        metadata = {
            key: entry[key]
            for key in (
                "tile_index",
                "seq",
                "x",
                "y",
                "w",
                "h",
                "stage_index",
                "stage",
                "step_scale",
                "estimated_samples",
                "samples_used",
                "color_hash",
                "transmittance_hash",
            )
            if key in entry
        }
        expected_chain = hashlib.sha256(
            previous_hash.encode("ascii") + _canonical_bytes(metadata)
        ).hexdigest()
        if entry.get("chain_hash") != expected_chain:
            raise _recovery_error(
                "snapshot chain hash mismatch",
                entry_index=index,
            )
        result = _decode_entry(entry, scene, config)
        completed[result.tile.index] = result
        previous_hash = expected_chain
    if chain_head != previous_hash:
        raise _recovery_error(
            "snapshot chain head mismatch",
            expected=previous_hash,
            actual=chain_head,
        )
    return SnapshotLoad(
        path=snapshot_path,
        completed=completed,
        document=document,
    )
