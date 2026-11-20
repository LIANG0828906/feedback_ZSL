"""Atomic, hash-chained render snapshots and deterministic recovery."""

from __future__ import annotations

import copy
from dataclasses import dataclass
from pathlib import Path
from typing import Any


from .chunks import Chunk, build_chunk_table
from .config import RenderConfig
from .errors import E_RECOVERY_INVALID, DustRenderError
from .renderer import ChunkResult
from .scene import Scene
from .scheduler import content_hash
from .util import (
    atomic_write_json,
    canonical_json_bytes,
    decode_array,
    encode_array,
    sha256_bytes,
)

SNAPSHOT_FORMAT = "dustrender-snapshot"
SNAPSHOT_SCHEMA = 1


@dataclass
class SnapshotData:
    path: Path
    document: dict[str, Any]
    chunks: list[Chunk]
    results: dict[str, ChunkResult]
    completed_indices: list[int]
    snapshot_hash: str


def _result_chain_hash(previous: str, result: ChunkResult, index: int) -> str:
    return sha256_bytes(
        bytes.fromhex(previous)
        + bytes.fromhex(content_hash(result))
        + f":{index}:".encode("ascii")
    )


def build_snapshot(
    scene: Scene,
    config: RenderConfig,
    chunks: list[Chunk],
    results: dict[str, ChunkResult],
    statuses: dict[str, str] | None = None,
    *,
    effective_step: float | None = None,
) -> dict[str, Any]:
    statuses = statuses or {chunk.id: "completed" for chunk in chunks if chunk.id in results}
    completed = [
        chunk.index
        for chunk in chunks
        if chunk.id in results
        and statuses.get(chunk.id) in {"completed", "recovered", "reused"}
    ]
    chain: list[str] = []
    previous = "00" * 32
    records = []
    for chunk in chunks:
        result = results.get(chunk.id)
        if result is None or chunk.index not in completed:
            continue
        chain_hash = _result_chain_hash(previous, result, chunk.index)
        chain.append(chain_hash)
        previous = chain_hash
        records.append(
            {
                "index": chunk.index,
                "content_hash": content_hash(result),
                "chain_hash": chain_hash,
                "rgb_b64": encode_array(result.rgb),
                "transmittance_b64": encode_array(result.transmittance),
                "primary_samples": result.primary_samples,
                "samples": result.samples,
                "step": result.step if effective_step is None else effective_step,
            }
        )
    document: dict[str, Any] = {
        "format": SNAPSHOT_FORMAT,
        "schema_version": SNAPSHOT_SCHEMA,
        "scene_id": scene.scene_id,
        "scene_sha256": scene.scene_sha256,
        "payload_sha256": scene.payload_sha256,
        "density_version": scene.density_version,
        "precision": config.precision,
        "config": config.to_dict(),
        "config_fingerprint": config.fingerprint,
        "budget": {
            "parallelism": config.parallelism,
            "policy": config.budget_policy,
            "task_budget": config.task_budget,
        },
        "chunk_table": [chunk.to_dict() for chunk in chunks],
        "completed_indices": completed,
        "hash_chain": chain,
        "results": records,
    }
    document["snapshot_hash"] = sha256_bytes(canonical_json_bytes(document))
    return document


def save_snapshot(
    path: str | Path,
    scene: Scene,
    config: RenderConfig,
    chunks: list[Chunk],
    results: dict[str, ChunkResult],
    statuses: dict[str, str] | None = None,
    *,
    effective_step: float | None = None,
) -> dict[str, Any]:
    document = build_snapshot(
        scene,
        config,
        chunks,
        results,
        statuses,
        effective_step=effective_step,
    )
    atomic_write_json(path, document)
    return document


def load_snapshot(
    path: str | Path,
    scene: Scene,
    config: RenderConfig,
) -> SnapshotData:
    import json

    source = Path(path)
    try:
        document = json.loads(source.read_text(encoding="utf-8"))
    except OSError as exc:
        raise DustRenderError(
            E_RECOVERY_INVALID, f"cannot read snapshot: {exc}", "$snapshot"
        ) from exc
    except json.JSONDecodeError as exc:
        raise DustRenderError(
            E_RECOVERY_INVALID, "snapshot is not valid JSON", "$snapshot.json"
        ) from exc
    _require(document, "$", SNAPSHOT_FORMAT, "format")
    _require(document, "$", SNAPSHOT_SCHEMA, "schema_version")
    if _require(document, "$", SNAPSHOT_FORMAT, "format") != SNAPSHOT_FORMAT:
        raise DustRenderError(
            E_RECOVERY_INVALID, "unknown snapshot format", "$.format"
        )
    if _require(document, "$", SNAPSHOT_SCHEMA, "schema_version") != SNAPSHOT_SCHEMA:
        raise DustRenderError(
            E_RECOVERY_INVALID, "unsupported snapshot schema", "$.schema_version"
        )
    expected_hash = _require(document, "$", None, "snapshot_hash")
    body = copy.deepcopy(document)
    body.pop("snapshot_hash", None)
    actual_hash = sha256_bytes(canonical_json_bytes(body))
    if expected_hash != actual_hash:
        raise DustRenderError(
            E_RECOVERY_INVALID,
            "snapshot hash does not match its contents",
            "$.snapshot_hash",
            {"expected": expected_hash, "actual": actual_hash},
        )
    if document.get("scene_sha256") != scene.scene_sha256:
        raise DustRenderError(
            E_RECOVERY_INVALID,
            "snapshot belongs to a different scene version",
            "$.scene_sha256",
        )
    if document.get("density_version") != scene.density_version:
        raise DustRenderError(
            E_RECOVERY_INVALID,
            "snapshot density version does not match the scene",
            "$.density_version",
        )
    if document.get("config_fingerprint") != config.fingerprint:
        raise DustRenderError(
            E_RECOVERY_INVALID,
            "snapshot configuration fingerprint does not match",
            "$.config_fingerprint",
        )
    if document.get("precision") != config.precision:
        raise DustRenderError(
            E_RECOVERY_INVALID,
            "snapshot precision mode does not match",
            "$.precision",
        )
    chunks = build_chunk_table(
        scene.width, scene.height, config.chunk_width, config.chunk_height
    )
    expected_table = [chunk.to_dict() for chunk in chunks]
    if document.get("chunk_table") != expected_table:
        raise DustRenderError(
            E_RECOVERY_INVALID,
            "snapshot chunk table does not match the current configuration",
            "$.chunk_table",
        )
    raw_results = document.get("results")
    completed = document.get("completed_indices")
    chain = document.get("hash_chain")
    if not isinstance(raw_results, list) or not isinstance(completed, list) or not isinstance(chain, list):
        raise DustRenderError(
            E_RECOVERY_INVALID,
            "snapshot result, completion, or hash-chain field is missing",
            "$.results",
        )
    results: dict[str, ChunkResult] = {}
    previous = "00" * 32
    chain_index = 0
    by_index = {chunk.index: chunk for chunk in chunks}
    for position, record in enumerate(raw_results):
        path_prefix = f"$.results[{position}]"
        if not isinstance(record, dict):
            raise DustRenderError(
                E_RECOVERY_INVALID, "result record must be an object", path_prefix
            )
        index = record.get("index")
        if index not in by_index:
            raise DustRenderError(
                E_RECOVERY_INVALID, "result index is outside the chunk table", f"{path_prefix}.index"
            )
        chunk = by_index[index]
        rgb = decode_array(
            _require(record, path_prefix, None, "rgb_b64"),
            chunk.content_shape,
            f"{path_prefix}.rgb_b64",
        )
        trans = decode_array(
            _require(record, path_prefix, None, "transmittance_b64"),
            chunk.transmittance_shape,
            f"{path_prefix}.transmittance_b64",
        )
        primary = record.get("primary_samples")
        samples = record.get("samples")
        step = record.get("step")
        if (
            isinstance(primary, bool)
            or not isinstance(primary, int)
            or isinstance(samples, bool)
            or not isinstance(samples, int)
            or not isinstance(step, (int, float))
        ):
            raise DustRenderError(
                E_RECOVERY_INVALID,
                "result sample counters or step are invalid",
                f"{path_prefix}.samples",
            )
        result = ChunkResult(
            chunk=chunk,
            rgb=rgb,
            transmittance=trans,
            primary_samples=primary,
            shadow_samples=max(0, samples - primary),
            step=float(step),
            max_segments=0,
        )
        actual_content = content_hash(result)
        if record.get("content_hash") != actual_content:
            raise DustRenderError(
                E_RECOVERY_INVALID,
                "chunk content hash mismatch",
                f"{path_prefix}.content_hash",
            )
        expected_chain = _result_chain_hash(previous, result, index)
        if chain_index >= len(chain) or chain[chain_index] != expected_chain:
            raise DustRenderError(
                E_RECOVERY_INVALID,
                "snapshot hash chain is broken",
                f"{path_prefix}.chain_hash",
            )
        if record.get("chain_hash") != expected_chain:
            raise DustRenderError(
                E_RECOVERY_INVALID,
                "result chain hash does not match its content",
                f"{path_prefix}.chain_hash",
            )
        previous = expected_chain
        chain_index += 1
        results[chunk.id] = result
    if chain_index != len(chain):
        raise DustRenderError(
            E_RECOVERY_INVALID, "snapshot hash chain has trailing entries", "$.hash_chain"
        )
    if sorted(completed) != sorted(record["index"] for record in raw_results):
        raise DustRenderError(
            E_RECOVERY_INVALID,
            "completed index set does not match stored result records",
            "$.completed_indices",
        )
    return SnapshotData(
        path=source,
        document=document,
        chunks=chunks,
        results=results,
        completed_indices=sorted(completed),
        snapshot_hash=expected_hash,
    )


def find_latest_valid_snapshot(
    directory: str | Path,
    scene: Scene,
    config: RenderConfig,
) -> SnapshotData:
    candidates = sorted(
        Path(directory).glob("*.json"), key=lambda item: item.name, reverse=True
    )
    errors: list[dict[str, Any]] = []
    for candidate in candidates:
        try:
            return load_snapshot(candidate, scene, config)
        except DustRenderError as exc:
            errors.append({"path": str(candidate), "error": exc.to_dict()})
    raise DustRenderError(
        E_RECOVERY_INVALID,
        "no valid snapshot remains in the recovery directory",
        "$snapshot",
        {"candidates": errors},
    )


def _require(
    mapping: dict[str, Any],
    path: str,
    expected: Any,
    key: str,
) -> Any:
    if not isinstance(mapping, dict) or key not in mapping:
        raise DustRenderError(
            E_RECOVERY_INVALID,
            f"required snapshot field {key!r} is missing",
            f"{path}.{key}",
        )
    value = mapping[key]
    if expected is not None and value != expected:
        return value
    return value
