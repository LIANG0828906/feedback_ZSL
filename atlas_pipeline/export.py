from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping

import numpy as np

from .codec import atomic_write_bytes, encode_atlas_bytes, read_atlas
from .errors import AtlasError, ErrorCode
from .hashing import hash_value, sha256_bytes
from .pipeline import AtlasPipeline


@dataclass(frozen=True)
class ExportResult:
    atlas_path: str
    report_path: str
    version: int
    atlas_sha256: str
    content_hash: str
    chain_hash: str
    report_sha256: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "atlas_path": self.atlas_path,
            "report_path": self.report_path,
            "version": self.version,
            "atlas_sha256": self.atlas_sha256,
            "content_hash": self.content_hash,
            "chain_hash": self.chain_hash,
            "report_sha256": self.report_sha256,
        }


def _report_bytes(report_without_hash: Mapping[str, Any]) -> tuple[bytes, str]:
    report_hash = hash_value(report_without_hash)
    report = dict(report_without_hash)
    report["report_sha256"] = report_hash
    payload = (
        json.dumps(
            report,
            indent=2,
            sort_keys=True,
            ensure_ascii=True,
            allow_nan=False,
        )
        + "\n"
    ).encode("ascii")
    return payload, report_hash


def export_published(
    pipeline: AtlasPipeline,
    atlas_path: str | Path,
    report_path: str | Path,
    *,
    version_number: int | None = None,
    expected_version: int | None = None,
    expected_content_hash: str | None = None,
    expected_manifest: Mapping[int, str] | None = None,
) -> ExportResult:
    preview = pipeline.capture_for_export(
        version_number=version_number,
        expected_version=expected_version,
        expected_content_hash=expected_content_hash,
        expected_manifest=expected_manifest,
    )
    version = preview.version
    atlas = pipeline.atlas
    payload = encode_atlas_bytes(
        preview.encoded,
        bit_depth=atlas.header.bit_depth,
        color_space=version.config.target_color_space,
        transfer=version.config.target_transfer,
        grid_cols=atlas.header.grid_cols,
        grid_rows=atlas.header.grid_rows,
        source_peak_nits=version.config.target_peak_nits,
        pipeline_version=version.number,
    )
    atlas_sha = sha256_bytes(payload)
    report_without_hash: dict[str, Any] = {
        "format": "atlas-error-report",
        "format_version": 1,
        "atlas_version": version.number,
        "atlas_sha256": atlas_sha,
        "content_hash": version.content_hash,
        "chain_hash": version.chain_hash,
        "transformation_id": version.transformation_id,
        "error_summary_hash": version.error_summary_hash,
        "chain_digests": dict(version.chain_digests),
        "header": {
            "width": atlas.header.width,
            "height": atlas.header.height,
            "bit_depth": atlas.header.bit_depth,
            "color_space": version.config.target_color_space,
            "transfer_function": version.config.target_transfer,
            "source_peak_nits": version.config.target_peak_nits,
            "grid_cols": atlas.header.grid_cols,
            "grid_rows": atlas.header.grid_rows,
            "pipeline_version": version.number,
        },
        "config": version.config.to_dict(),
        "tile_count": atlas.tile_count,
        "tile_params": [item.to_dict() for item in version.tile_params],
        "tiles": [
            {
                "index": index,
                "bbox": list(atlas.tiles[index].bbox),
                "params": version.tile_params[index].to_dict(),
                "tile_hash": version.output_tile_hashes[index],
                "max_code_deviation": version.tile_summaries[index].max_code_deviation,
                "mean_code_deviation": version.tile_summaries[index].mean_code_deviation,
                "rmse_code_deviation": version.tile_summaries[index].rmse_code_deviation,
                "p99_code_deviation": version.tile_summaries[index].p99_code_deviation,
                "clipped_fraction": version.tile_summaries[index].clipped_fraction,
                "summary_hash": version.tile_summaries[index].summary_hash,
            }
            for index in range(atlas.tile_count)
        ],
    }
    report_payload, report_hash = _report_bytes(report_without_hash)
    atomic_write_bytes(atlas_path, payload)
    atomic_write_bytes(report_path, report_payload)
    return ExportResult(
        atlas_path=str(atlas_path),
        report_path=str(report_path),
        version=version.number,
        atlas_sha256=atlas_sha,
        content_hash=version.content_hash,
        chain_hash=version.chain_hash,
        report_sha256=report_hash,
    )


def verify_export(atlas_path: str | Path, report_path: str | Path) -> dict[str, Any]:
    try:
        report = json.loads(Path(report_path).read_text(encoding="ascii"))
    except OSError as exc:
        raise AtlasError(
            ErrorCode.E_IO,
            f"cannot read export report: {exc}",
            field="report_path",
        ) from None
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise AtlasError(
            ErrorCode.E_EXPORT_INVALID,
            f"export report is malformed: {exc}",
            field="report",
        ) from None
    if not isinstance(report, dict):
        raise AtlasError(
            ErrorCode.E_EXPORT_INVALID,
            "export report root must be an object",
            field="report",
        )
    required = {
        "format",
        "format_version",
        "atlas_version",
        "atlas_sha256",
        "content_hash",
        "header",
        "tiles",
        "tile_params",
        "report_sha256",
    }
    missing = sorted(required - set(report))
    if missing:
        raise AtlasError(
            ErrorCode.E_EXPORT_INVALID,
            f"export report is missing field {missing[0]!r}",
            field=missing[0],
        )
    if report["format"] != "atlas-error-report" or int(report["format_version"]) != 1:
        raise AtlasError(
            ErrorCode.E_EXPORT_INVALID,
            "export report format is not recognized",
            field="format",
        )
    without_hash = dict(report)
    persisted_report_hash = str(without_hash.pop("report_sha256"))
    expected_report_hash = hash_value(without_hash)
    if persisted_report_hash != expected_report_hash:
        raise AtlasError(
            ErrorCode.E_VERSION_STALE,
            "export report hash does not match its contents",
            field="report_sha256",
            details={
                "expected": expected_report_hash,
                "actual": persisted_report_hash,
            },
        )
    try:
        exported = read_atlas(atlas_path)
    except AtlasError as exc:
        raise AtlasError(
            ErrorCode.E_EXPORT_INVALID,
            f"exported atlas cannot be reloaded: {exc.message}",
            field="atlas",
        ) from None
    atlas_hash = exported.source_hash
    if atlas_hash != report["atlas_sha256"]:
        raise AtlasError(
            ErrorCode.E_VERSION_STALE,
            "exported atlas SHA-256 does not match the report",
            field="atlas_sha256",
            details={"expected": report["atlas_sha256"], "actual": atlas_hash},
        )
    if exported.header.pipeline_version != int(report["atlas_version"]):
        raise AtlasError(
            ErrorCode.E_VERSION_STALE,
            "atlas header version does not match the report version",
            field="pipeline_version",
            details={
                "expected": int(report["atlas_version"]),
                "actual": exported.header.pipeline_version,
            },
        )
    header = report["header"]
    header_pairs = {
        "width": exported.header.width,
        "height": exported.header.height,
        "bit_depth": exported.header.bit_depth,
        "color_space": exported.header.color_space,
        "transfer_function": exported.header.transfer_function,
        "source_peak_nits": exported.header.source_peak_nits,
        "grid_cols": exported.header.grid_cols,
        "grid_rows": exported.header.grid_rows,
        "pipeline_version": exported.header.pipeline_version,
    }
    for field, actual in header_pairs.items():
        if field not in header or header[field] != actual:
            raise AtlasError(
                ErrorCode.E_VERSION_STALE,
                f"export header field {field!r} does not match the report",
                field=field,
            )
    tiles = report["tiles"]
    if not isinstance(tiles, list) or len(tiles) != exported.tile_count:
        raise AtlasError(
            ErrorCode.E_VERSION_STALE,
            "report tile count does not match the exported atlas",
            field="tiles",
        )
    tile_hashes: list[str] = []
    for index, spec in enumerate(exported.tiles):
        record = tiles[index]
        if not isinstance(record, dict) or int(record.get("index", -1)) != index:
            raise AtlasError(
                ErrorCode.E_VERSION_STALE,
                "report tiles are not in atlas tile order",
                field="tiles",
                tile=index,
            )
        tile = np.ascontiguousarray(exported.encoded[spec.y0 : spec.y1, spec.x0 : spec.x1, :])
        actual_hash = sha256_bytes(tile.tobytes(order="C"))
        if actual_hash != record.get("tile_hash"):
            raise AtlasError(
                ErrorCode.E_VERSION_STALE,
                "reloaded tile hash does not match the report",
                field="tiles.tile_hash",
                tile=index,
                details={"expected": record.get("tile_hash"), "actual": actual_hash},
            )
        if list(spec.bbox) != record.get("bbox"):
            raise AtlasError(
                ErrorCode.E_VERSION_STALE,
                "report tile bounding box does not match the exported atlas",
                field="bbox",
                tile=index,
            )
        tile_hashes.append(actual_hash)
    expected_content_hash = hash_value(
        {
            "tiles": [
                {"tile": index, "hash": digest}
                for index, digest in enumerate(tile_hashes)
            ]
        }
    )
    if expected_content_hash != report["content_hash"]:
        raise AtlasError(
            ErrorCode.E_VERSION_STALE,
            "reloaded tile content hash does not match the report",
            field="content_hash",
            details={
                "expected": report["content_hash"],
                "actual": expected_content_hash,
            },
        )
    return {
        "ok": True,
        "version": int(report["atlas_version"]),
        "content_hash": expected_content_hash,
        "atlas_sha256": atlas_hash,
        "report_sha256": persisted_report_hash,
        "tile_count": len(tile_hashes),
    }
