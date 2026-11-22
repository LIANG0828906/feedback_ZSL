from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from .bake import ARTIFACT_ORDER
from .cache import BLOCK_MANIFEST_NAME
from .errors import E_DIFF, PipelineError
from .util import read_json, sha256_file


def compare_snapshots(
    left_cache: str | Path,
    right_cache: str | Path,
    *,
    raise_on_diff: bool = True,
) -> dict[str, Any]:
    left_root = Path(left_cache).resolve()
    right_root = Path(right_cache).resolve()
    left_path = left_root / "snapshot.json"
    right_path = right_root / "snapshot.json"
    if not left_path.is_file() or not right_path.is_file():
        missing = "left" if not left_path.is_file() else "right"
        return _difference(
            {
                "equal": False,
                "tile_id": None,
                "artifact_type": "snapshot.json",
                "detail": f"{missing} snapshot is missing",
            },
            raise_on_diff,
        )
    try:
        left = read_json(left_path)
        right = read_json(right_path)
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        return _difference(
            {
                "equal": False,
                "tile_id": None,
                "artifact_type": "snapshot.json",
                "detail": f"snapshot decode failure: {type(exc).__name__}",
            },
            raise_on_diff,
        )
    if not isinstance(left, dict) or not isinstance(right, dict):
        return _difference(
            {
                "equal": False,
                "tile_id": None,
                "artifact_type": "snapshot.json",
                "detail": "snapshot root is not an object",
            },
            raise_on_diff,
        )
    for field in ("format", "project_hash", "bake_version", "grid_tiles"):
        if left.get(field) != right.get(field):
            return _difference(
                {
                    "equal": False,
                    "tile_id": None,
                    "artifact_type": "snapshot.json",
                    "detail": f"snapshot field differs: {field}",
                    "left": left.get(field),
                    "right": right.get(field),
                },
                raise_on_diff,
            )

    left_tiles = {
        entry.get("tile_id"): entry for entry in left.get("tiles", []) if isinstance(entry, dict)
    }
    right_tiles = {
        entry.get("tile_id"): entry for entry in right.get("tiles", []) if isinstance(entry, dict)
    }
    tile_ids = sorted(set(left_tiles) | set(right_tiles), key=lambda value: str(value))
    for tile_id in tile_ids:
        left_entry = left_tiles.get(tile_id)
        right_entry = right_tiles.get(tile_id)
        if left_entry is None or right_entry is None:
            return _difference(
                {
                    "equal": False,
                    "tile_id": tile_id,
                    "artifact_type": "snapshot.json",
                    "detail": "tile missing from one snapshot",
                },
                raise_on_diff,
            )
        left_key = left_entry.get("cache_key")
        right_key = right_entry.get("cache_key")
        if not isinstance(left_key, str) or not isinstance(right_key, str):
            return _difference(
                {
                    "equal": False,
                    "tile_id": tile_id,
                    "artifact_type": "snapshot.json",
                    "detail": "invalid cache key in snapshot",
                },
                raise_on_diff,
            )
        left_block = left_root / "blocks" / left_key
        right_block = right_root / "blocks" / right_key
        artifact_names = (BLOCK_MANIFEST_NAME,) + ARTIFACT_ORDER
        for artifact_name in artifact_names:
            left_file = left_block / artifact_name
            right_file = right_block / artifact_name
            if not left_file.is_file() or not right_file.is_file():
                return _difference(
                    {
                        "equal": False,
                        "tile_id": tile_id,
                        "artifact_type": artifact_name,
                        "detail": "artifact missing",
                    },
                    raise_on_diff,
                )
            if left_file.read_bytes() != right_file.read_bytes():
                return _difference(
                    {
                        "equal": False,
                        "tile_id": tile_id,
                        "artifact_type": artifact_name,
                        "detail": "artifact bytes differ",
                        "left_sha256": sha256_file(left_file),
                        "right_sha256": sha256_file(right_file),
                    },
                    raise_on_diff,
                )
    if left_path.read_bytes() != right_path.read_bytes():
        return _difference(
            {
                "equal": False,
                "tile_id": None,
                "artifact_type": "snapshot.json",
                "detail": "all block artifacts match but snapshot metadata bytes differ",
                "left_sha256": sha256_file(left_path),
                "right_sha256": sha256_file(right_path),
            },
            raise_on_diff,
        )
    return {
        "equal": True,
        "tiles_compared": len(tile_ids),
        "artifacts_per_tile": [BLOCK_MANIFEST_NAME, *ARTIFACT_ORDER],
    }


def _difference(info: dict[str, Any], raise_on_diff: bool) -> dict[str, Any]:
    if raise_on_diff and not info["equal"]:
        raise PipelineError(
            E_DIFF,
            "render snapshots differ",
            tile_id=info.get("tile_id"),
            field=info.get("artifact_type"),
            details=info,
        )
    return info
