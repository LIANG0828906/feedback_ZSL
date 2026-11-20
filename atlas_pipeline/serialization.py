from __future__ import annotations

from typing import Any, Mapping

from .errors import AtlasError, ErrorCode
from .hashing import hash_value
from .model import TileErrorSummary, TileParams, TransformConfig, Version


def version_hash_payload(version: Version) -> dict[str, Any]:
    return {
        "number": version.number,
        "parent_version": version.parent_version,
        "parent_chain_hash": version.parent_chain_hash,
        "atlas_fingerprint": version.atlas_fingerprint,
        "reason": version.reason,
        "rollback_of": version.rollback_of,
        "config": version.config.to_dict(),
        "tile_params": [params.to_dict() for params in version.tile_params],
        "affected_tiles": list(version.affected_tiles),
        "reused_tiles": list(version.reused_tiles),
        "output_tile_hashes": list(version.output_tile_hashes),
        "content_hash": version.content_hash,
        "transformation_id": version.transformation_id,
        "chain_digests": dict(version.chain_digests),
        "tile_summaries": [summary.to_dict() for summary in version.tile_summaries],
        "error_summary_hash": version.error_summary_hash,
    }


def compute_chain_hash(version: Version) -> str:
    return hash_value(version_hash_payload(version))


def version_to_dict(version: Version) -> dict[str, Any]:
    payload = version_hash_payload(version)
    payload["chain_hash"] = version.chain_hash
    return payload


def _required(mapping: Mapping[str, Any], key: str, *, where: str) -> Any:
    if key not in mapping:
        raise AtlasError(
            ErrorCode.E_RECOVERY_INVALID,
            f"snapshot version record is missing required field {key!r}",
            field=f"{where}.{key}",
        )
    return mapping[key]


def version_from_dict(value: Mapping[str, Any], *, where: str = "version") -> Version:
    try:
        config_value = _required(value, "config", where=where)
        tile_params_value = _required(value, "tile_params", where=where)
        summaries_value = _required(value, "tile_summaries", where=where)
        version = Version(
            number=int(_required(value, "number", where=where)),
            parent_version=(
                None
                if _required(value, "parent_version", where=where) is None
                else int(value["parent_version"])
            ),
            parent_chain_hash=(
                None
                if _required(value, "parent_chain_hash", where=where) is None
                else str(value["parent_chain_hash"])
            ),
            chain_hash=str(_required(value, "chain_hash", where=where)),
            atlas_fingerprint=str(_required(value, "atlas_fingerprint", where=where)),
            reason=str(_required(value, "reason", where=where)),
            rollback_of=(
                None
                if _required(value, "rollback_of", where=where) is None
                else int(value["rollback_of"])
            ),
            config=TransformConfig.from_mapping(config_value),
            tile_params=tuple(
                TileParams.from_mapping(item) for item in tile_params_value
            ),
            affected_tiles=tuple(int(item) for item in value["affected_tiles"]),
            reused_tiles=tuple(int(item) for item in value["reused_tiles"]),
            output_tile_hashes=tuple(str(item) for item in value["output_tile_hashes"]),
            content_hash=str(_required(value, "content_hash", where=where)),
            transformation_id=str(_required(value, "transformation_id", where=where)),
            chain_digests={
                str(key): str(item)
                for key, item in _required(value, "chain_digests", where=where).items()
            },
            tile_summaries=tuple(
                TileErrorSummary.from_dict(item) for item in summaries_value
            ),
            error_summary_hash=str(_required(value, "error_summary_hash", where=where)),
        )
    except AtlasError as exc:
        if exc.code == ErrorCode.E_RECOVERY_INVALID:
            raise
        raise AtlasError(
            ErrorCode.E_RECOVERY_INVALID,
            f"invalid persisted version data: {exc.message}",
            field=exc.field or where,
            tile=exc.tile,
            details=exc.details,
        ) from None
    except (KeyError, TypeError, ValueError) as exc:
        raise AtlasError(
            ErrorCode.E_RECOVERY_INVALID,
            f"malformed snapshot version record: {exc}",
            field=where,
        ) from None
    expected = compute_chain_hash(version)
    if version.chain_hash != expected:
        raise AtlasError(
            ErrorCode.E_RECOVERY_INVALID,
            "version hash chain digest does not match the persisted record",
            field=f"{where}.chain_hash",
            details={"expected": expected, "actual": version.chain_hash},
        )
    return version
