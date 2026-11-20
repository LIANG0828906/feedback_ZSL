from __future__ import annotations

import base64
import json
import zlib
from pathlib import Path
from typing import Any, Mapping

import numpy as np

from .codec import atomic_write_bytes, decode_atlas_bytes
from .errors import AtlasError, ErrorCode
from .hashing import hash_value, sha256_bytes
from .model import CachedTile, TileErrorSummary, TileParams, TransformConfig, Version
from .pipeline import AtlasPipeline, ConversionJob
from .serialization import version_from_dict, version_to_dict

SNAPSHOT_FORMAT = "atlas-pipeline-snapshot"
SNAPSHOT_FORMAT_VERSION = 1


def _recovery_error(field: str, message: str, **details: Any) -> AtlasError:
    return AtlasError(
        ErrorCode.E_RECOVERY_INVALID,
        message,
        field=field,
        details=details or None,
    )


def _required(mapping: Mapping[str, Any], key: str, *, field: str) -> Any:
    if key not in mapping:
        raise _recovery_error(field, f"snapshot is missing required field {key!r}")
    return mapping[key]


def _encode_array(array: np.ndarray) -> str:
    raw = np.ascontiguousarray(array, dtype="<f2").tobytes(order="C")
    return base64.b64encode(zlib.compress(raw, 9)).decode("ascii")


def _decode_array(value: str, shape: tuple[int, int, int], *, field: str) -> np.ndarray:
    try:
        raw = zlib.decompress(base64.b64decode(value.encode("ascii"), validate=True))
    except Exception as exc:
        raise _recovery_error(field, f"cannot decode compressed tile data: {exc}") from None
    expected = int(np.prod(shape)) * 2
    if len(raw) != expected:
        raise _recovery_error(
            field,
            "decoded tile byte length does not match the atlas tile shape",
            actual=len(raw),
            expected=expected,
        )
    return np.frombuffer(raw, dtype="<f2").reshape(shape).copy()


def _version_cache_keys(pipeline: AtlasPipeline, version: Version) -> tuple[str, ...]:
    chain = pipeline._make_chain(version.config)
    return tuple(
        pipeline._tile_cache_key(index, chain, params)
        for index, params in enumerate(version.tile_params)
    )


def _cache_to_dict(cached: CachedTile, shape: tuple[int, int, int]) -> dict[str, Any]:
    return {
        "tile": cached.tile,
        "shape": list(shape),
        "tile_hash": cached.tile_hash,
        "summary": cached.summary.to_dict(),
        "encoding": "zlib+base64+float16le",
        "data": _encode_array(cached.encoded),
    }


def save_snapshot(pipeline: AtlasPipeline, path: str | Path) -> str:
    with pipeline._snapshot_lock:
        with pipeline._publish_lock:
            versions = pipeline._versions
            current = pipeline._current_version
            active = pipeline._active_job
            pending_progress: dict[str, Any] | None = None
            pending_cache: dict[str, CachedTile] = {}
            if active is not None:
                pending_progress, pending_cache = active.snapshot_state()
            cache_entries: dict[str, CachedTile] = {}
            for version in versions:
                for index, key in enumerate(_version_cache_keys(pipeline, version)):
                    cached = pipeline._cache.get(key)
                    if cached is None:
                        raise _recovery_error(
                            f"versions[{version.number}].tile_hashes",
                            "published tile output is missing from the in-memory cache",
                            tile=index,
                        )
                    cache_entries[key] = cached
            cache_entries.update(pending_cache)
            serialized_cache: dict[str, Any] = {}
            for key, cached in sorted(cache_entries.items()):
                shape = tuple(pipeline._atlas.tiles[cached.tile].shape)
                serialized_cache[key] = _cache_to_dict(cached, shape)
            pending_document: dict[str, Any] | None = None
            if pending_progress is not None:
                pending_document = dict(pending_progress)
                pending_document["cache_keys"] = sorted(pending_cache)
            document = {
                "format": SNAPSHOT_FORMAT,
                "format_version": SNAPSHOT_FORMAT_VERSION,
                "atlas": {
                    "encoding": "zlib+base64",
                    "sha256": pipeline._atlas.source_hash,
                    "data": base64.b64encode(
                        zlib.compress(pipeline._atlas.raw_bytes, 9)
                    ).decode("ascii"),
                },
                "atlas_fingerprint": pipeline._atlas.fingerprint,
                "versions": [version_to_dict(version) for version in versions],
                "current_version": current,
                "preview_state": {"version": current},
                "cache": serialized_cache,
                "pending": pending_document,
                "trace": pipeline._trace,
            }
    encoded = (
        json.dumps(
            document,
            indent=2,
            sort_keys=True,
            ensure_ascii=True,
            allow_nan=False,
        )
        + "\n"
    ).encode("ascii")
    atomic_write_bytes(path, encoded)
    return sha256_bytes(encoded)


def _validate_version_against_atlas(
    pipeline: AtlasPipeline,
    version: Version,
) -> None:
    if version.atlas_fingerprint != pipeline._atlas.fingerprint:
        raise _recovery_error(
            f"versions[{version.number}].atlas_fingerprint",
            "version was produced from a different atlas snapshot",
        )
    if len(version.tile_params) != pipeline._atlas.tile_count:
        raise _recovery_error(
            f"versions[{version.number}].tile_params",
            "version tile parameter count does not match the atlas grid",
        )
    if len(version.output_tile_hashes) != pipeline._atlas.tile_count:
        raise _recovery_error(
            f"versions[{version.number}].output_tile_hashes",
            "version tile hash count does not match the atlas grid",
        )
    if len(version.tile_summaries) != pipeline._atlas.tile_count:
        raise _recovery_error(
            f"versions[{version.number}].tile_summaries",
            "version error summary count does not match the atlas grid",
        )
    try:
        pipeline._validate_params(version.config, version.tile_params)
    except AtlasError as exc:
        raise _recovery_error(
            f"versions[{version.number}].params",
            f"persisted parameters are invalid: {exc.message}",
            tile=exc.tile,
        ) from None
    expected_content = hash_value(
        {
            "tiles": [
                {"tile": index, "hash": digest}
                for index, digest in enumerate(version.output_tile_hashes)
            ]
        }
    )
    if expected_content != version.content_hash:
        raise _recovery_error(
            f"versions[{version.number}].content_hash",
            "version content hash does not match the persisted tile hash manifest",
        )
    expected_transformation = hash_value(
        {
            "atlas_fingerprint": pipeline._atlas.fingerprint,
            "config": version.config.to_dict(),
            "tile_params": [item.to_dict() for item in version.tile_params],
            "bit_depth": pipeline._atlas.header.bit_depth,
        }
    )
    if expected_transformation != version.transformation_id:
        raise _recovery_error(
            f"versions[{version.number}].transformation_id",
            "version transformation digest does not match its parameters",
        )
    expected_summary_hash = hash_value(
        [summary.to_dict() for summary in version.tile_summaries]
    )
    if expected_summary_hash != version.error_summary_hash:
        raise _recovery_error(
            f"versions[{version.number}].error_summary_hash",
            "version error summary digest does not match persisted summaries",
        )
    if version.chain_digests.get("tile_content") != version.content_hash:
        raise _recovery_error(
            f"versions[{version.number}].chain_digests.tile_content",
            "intermediate chain digest is inconsistent with tile content",
        )
    for index, summary in enumerate(version.tile_summaries):
        if summary.tile != index:
            raise _recovery_error(
                f"versions[{version.number}].tile_summaries[{index}].tile",
                "tile error summary is associated with the wrong tile",
            )


def _decode_cache_entry(
    key: str,
    value: Mapping[str, Any],
    pipeline: AtlasPipeline,
    expected_tile: int,
) -> CachedTile:
    try:
        if _required(value, "encoding", field=f"cache.{key}.encoding") != "zlib+base64+float16le":
            raise _recovery_error(
                f"cache.{key}.encoding",
                "unsupported tile cache encoding",
            )
        tile = int(_required(value, "tile", field=f"cache.{key}.tile"))
        if tile != expected_tile:
            raise _recovery_error(
                f"cache.{key}.tile",
                "cache entry is associated with the wrong tile",
                actual=tile,
                expected=expected_tile,
            )
        spec = pipeline._atlas.tiles[tile]
        encoded = _decode_array(
            str(_required(value, "data", field=f"cache.{key}.data")),
            spec.shape,
            field=f"cache.{key}.data",
        )
        summary = TileErrorSummary.from_dict(
            _required(value, "summary", field=f"cache.{key}.summary")
        )
        if summary.tile != tile:
            raise _recovery_error(
                f"cache.{key}.summary.tile",
                "cache error summary is associated with the wrong tile",
            )
        expected_summary_hash = hash_value(
            {
                "tile": summary.tile,
                "pixel_count": int(spec.width * spec.height),
                "max_code_deviation": summary.max_code_deviation,
                "mean_code_deviation": summary.mean_code_deviation,
                "rmse_code_deviation": summary.rmse_code_deviation,
                "p99_code_deviation": summary.p99_code_deviation,
                "clipped_fraction": summary.clipped_fraction,
            }
        )
        if expected_summary_hash != summary.summary_hash:
            raise _recovery_error(
                f"cache.{key}.summary.summary_hash",
                "cache error summary digest is invalid",
            )
        cached = CachedTile.build(
            key=key,
            tile=tile,
            encoded=encoded,
            summary=summary,
        )
        persisted_hash = str(
            _required(value, "tile_hash", field=f"cache.{key}.tile_hash")
        )
        if cached.tile_hash != persisted_hash:
            raise _recovery_error(
                f"cache.{key}.tile_hash",
                "tile content hash does not match persisted channel bytes",
            )
        return cached
    except AtlasError:
        raise
    except (KeyError, TypeError, ValueError) as exc:
        raise _recovery_error(
            f"cache.{key}",
            f"malformed tile cache entry: {exc}",
        ) from None


def _restore_job(
    pipeline: AtlasPipeline,
    pending: Mapping[str, Any],
    snapshot_cache: Mapping[str, CachedTile],
) -> tuple[ConversionJob, tuple[int, ...], tuple[int, ...]]:
    try:
        base_number = int(_required(pending, "base_version", field="pending.base_version"))
        version_number = int(
            _required(pending, "version_number", field="pending.version_number")
        )
        config = TransformConfig.from_mapping(
            _required(pending, "config", field="pending.config")
        )
        raw_params = _required(pending, "tile_params", field="pending.tile_params")
        params = tuple(TileParams.from_mapping(item) for item in raw_params)
        persisted_affected = tuple(
            int(item)
            for item in _required(
                pending, "affected_tiles", field="pending.affected_tiles"
            )
        )
        completed = tuple(
            int(item)
            for item in _required(
                pending, "completed_tiles", field="pending.completed_tiles"
            )
        )
        reason = str(_required(pending, "reason", field="pending.reason"))
        rollback = _required(pending, "rollback_of", field="pending.rollback_of")
    except AtlasError:
        raise
    except (TypeError, ValueError) as exc:
        raise _recovery_error(
            "pending",
            f"malformed pending conversion record: {exc}",
        ) from None
    if version_number != len(pipeline._versions) + 1:
        raise _recovery_error(
            "pending.version_number",
            "pending conversion version is not the next monotonic version",
        )
    try:
        base = pipeline.get_version(base_number)
    except AtlasError:
        raise _recovery_error(
            "pending.base_version",
            "pending conversion references a missing base version",
        ) from None
    if base_number != pipeline._current_version:
        raise _recovery_error(
            "pending.base_version",
            "pending conversion does not descend from the current published version",
        )
    try:
        config = pipeline._validate_params(config, params)
    except AtlasError as exc:
        raise _recovery_error(
            "pending.params",
            f"pending conversion parameters are invalid: {exc.message}",
            tile=exc.tile,
        ) from None
    expected_affected = pipeline._affected_tiles(base, config, params)
    if persisted_affected != expected_affected:
        raise _recovery_error(
            "pending.affected_tiles",
            "pending affected-tile set does not match its parameter changes",
        )
    if len(set(completed)) != len(completed) or not set(completed) <= set(expected_affected):
        raise _recovery_error(
            "pending.completed_tiles",
            "pending completed-tile set is invalid",
        )
    chain = pipeline._make_chain(config)
    keys = tuple(
        pipeline._tile_cache_key(index, chain, item)
        for index, item in enumerate(params)
    )
    for tile in range(pipeline._atlas.tile_count):
        if tile in expected_affected and tile not in completed:
            continue
        if keys[tile] not in snapshot_cache:
            raise _recovery_error(
                "pending.cache_keys",
                "a reused or completed pending tile is missing from the snapshot cache",
                tile=tile,
            )
    local_cache = {
        key: snapshot_cache[key] for key in keys if key in snapshot_cache
    }
    job = ConversionJob(
        pipeline,
        base_version=base,
        config=config,
        tile_params=params,
        affected_tiles=expected_affected,
        reason=reason,
        version_number=version_number,
        rollback_of=None if rollback is None else int(rollback),
        local_cache=local_cache,
        completed_tiles=completed,
        owns_guard=False,
    )
    reused = tuple(
        tile for tile in range(pipeline._atlas.tile_count) if tile not in expected_affected
    )
    recomputed = tuple(tile for tile in expected_affected if tile not in set(completed))
    return job, reused, recomputed


def load_snapshot(
    path: str | Path,
    *,
    resume: bool = True,
) -> AtlasPipeline:
    try:
        raw = Path(path).read_bytes()
    except OSError as exc:
        raise AtlasError(
            ErrorCode.E_IO,
            f"cannot read snapshot: {exc}",
            field="path",
            details={"path": str(path)},
        ) from None
    try:
        document = json.loads(raw.decode("ascii"))
    except Exception as exc:
        raise _recovery_error("snapshot", f"snapshot JSON is invalid: {exc}") from None
    if not isinstance(document, dict):
        raise _recovery_error("snapshot", "snapshot root must be an object")
    if _required(document, "format", field="format") != SNAPSHOT_FORMAT:
        raise _recovery_error("format", "snapshot format marker is not recognized")
    if int(_required(document, "format_version", field="format_version")) != SNAPSHOT_FORMAT_VERSION:
        raise _recovery_error("format_version", "unsupported snapshot format version")
    atlas_doc = _required(document, "atlas", field="atlas")
    if not isinstance(atlas_doc, dict):
        raise _recovery_error("atlas", "snapshot atlas record must be an object")
    if _required(atlas_doc, "encoding", field="atlas.encoding") != "zlib+base64":
        raise _recovery_error("atlas.encoding", "unsupported atlas payload encoding")
    try:
        atlas_raw = zlib.decompress(
            base64.b64decode(
                str(_required(atlas_doc, "data", field="atlas.data")).encode("ascii"),
                validate=True,
            )
        )
    except Exception as exc:
        raise _recovery_error("atlas.data", f"cannot decode atlas payload: {exc}") from None
    expected_atlas_hash = str(
        _required(atlas_doc, "sha256", field="atlas.sha256")
    )
    if sha256_bytes(atlas_raw) != expected_atlas_hash:
        raise _recovery_error("atlas.sha256", "embedded atlas hash does not match its bytes")
    try:
        atlas = decode_atlas_bytes(atlas_raw, source_name=str(path))
    except AtlasError as exc:
        raise _recovery_error(
            "atlas",
            f"embedded atlas is invalid: {exc.message}",
        ) from None
    pipeline = AtlasPipeline(atlas, initialize=False)
    fingerprint = str(
        _required(document, "atlas_fingerprint", field="atlas_fingerprint")
    )
    if fingerprint != atlas.fingerprint:
        raise _recovery_error(
            "atlas_fingerprint",
            "snapshot atlas fingerprint does not match embedded atlas bytes",
        )
    raw_versions = _required(document, "versions", field="versions")
    if not isinstance(raw_versions, list) or not raw_versions:
        raise _recovery_error("versions", "snapshot must contain at least one version")
    versions: list[Version] = []
    previous: Version | None = None
    for index, item in enumerate(raw_versions):
        if not isinstance(item, dict):
            raise _recovery_error(
                f"versions[{index}]",
                "version record must be an object",
            )
        version = version_from_dict(item, where=f"versions[{index}]")
        if version.number != index + 1:
            raise _recovery_error(
                f"versions[{index}].number",
                "version numbers must be contiguous and monotonic",
            )
        if previous is None:
            if version.parent_version is not None or version.parent_chain_hash is not None:
                raise _recovery_error(
                    f"versions[{index}].parent_version",
                    "initial version must not have a parent",
                )
        else:
            if version.parent_version != previous.number:
                raise _recovery_error(
                    f"versions[{index}].parent_version",
                    "version parent pointer is not the previous version",
                )
            if version.parent_chain_hash != previous.chain_hash:
                raise _recovery_error(
                    f"versions[{index}].parent_chain_hash",
                    "version hash chain is broken",
                )
        _validate_version_against_atlas(pipeline, version)
        versions.append(version)
        previous = version
    pipeline._versions = tuple(versions)
    current_version = int(
        _required(document, "current_version", field="current_version")
    )
    if current_version != versions[-1].number:
        raise _recovery_error(
            "current_version",
            "current published version must be the newest version in the chain",
        )
    pipeline._current_version = current_version
    preview_state = _required(document, "preview_state", field="preview_state")
    if not isinstance(preview_state, dict):
        raise _recovery_error("preview_state", "preview_state must be an object")
    preview_version = int(
        _required(preview_state, "version", field="preview_state.version")
    )
    if not 1 <= preview_version <= current_version:
        raise _recovery_error(
            "preview_state.version",
            "preview version is outside the persisted version range",
        )
    raw_cache = _required(document, "cache", field="cache")
    if not isinstance(raw_cache, dict):
        raise _recovery_error("cache", "snapshot cache must be an object")
    cache: dict[str, CachedTile] = {}
    for version in versions:
        keys = _version_cache_keys(pipeline, version)
        for index, key in enumerate(keys):
            if key not in raw_cache:
                raise _recovery_error(
                    f"versions[{version.number}].tile_hashes",
                    "snapshot cache is missing a published tile",
                    tile=index,
                )
            if key not in cache:
                cache[key] = _decode_cache_entry(key, raw_cache[key], pipeline, index)
            if cache[key].tile_hash != version.output_tile_hashes[index]:
                raise _recovery_error(
                    f"versions[{version.number}].output_tile_hashes[{index}]",
                    "persisted tile hash does not match its cached channel bytes",
                )
            if cache[key].summary != version.tile_summaries[index]:
                raise _recovery_error(
                    f"versions[{version.number}].tile_summaries[{index}]",
                    "persisted error summary does not match cached tile metadata",
                )
    pipeline._cache = cache
    for key, value in raw_cache.items():
        if key in cache:
            continue
        if not isinstance(value, dict) or "tile" not in value:
            raise _recovery_error(
                f"cache.{key}",
                "cache entry lacks a tile index",
            )
        tile = int(value["tile"])
        if tile < 0 or tile >= pipeline._atlas.tile_count:
            raise _recovery_error(
                f"cache.{key}.tile",
                "cache entry tile index is outside the atlas grid",
            )
        cache[key] = _decode_cache_entry(key, value, pipeline, tile)
    pipeline._cache = cache
    trace = document.get("trace", [])
    if not isinstance(trace, list):
        raise _recovery_error("trace", "trace must be a list")
    pipeline._trace = list(trace)
    report: dict[str, Any] = {
        "snapshot_path": str(path),
        "recovered_versions": len(versions),
        "current_version": current_version,
        "preview_version": preview_version,
        "resumed_pending": False,
        "reused_completed_tiles": [],
        "recomputed_tiles": [],
    }
    pending = document.get("pending")
    if pending is not None and resume:
        if not isinstance(pending, dict):
            raise _recovery_error("pending", "pending conversion must be an object")
        job, reused, recomputed = _restore_job(pipeline, pending, cache)
        pipeline._active_job = job
        try:
            job.run_to_completion()
        except AtlasError as exc:
            raise _recovery_error(
                "pending",
                f"pending conversion could not be completed: {exc.message}",
                tile=exc.tile,
            ) from None
        report["resumed_pending"] = True
        report["reused_completed_tiles"] = [
            tile for tile in job.affected_tiles if tile not in recomputed
        ]
        report["recomputed_tiles"] = list(recomputed)
        report["reused_unaffected_tiles"] = list(reused)
        report["current_version"] = pipeline._current_version
    elif pending is not None:
        if not isinstance(pending, dict):
            raise _recovery_error("pending", "pending conversion must be an object")
        report["pending_not_resumed"] = True
    pipeline.recovery_report = report
    return pipeline
