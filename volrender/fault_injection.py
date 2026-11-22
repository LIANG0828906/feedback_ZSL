"""Input-boundary fault injection for validation-channel tests.

Nothing in the renderer or scheduler imports this module.  Mutations happen
after raw binary decoding and before semantic validation.
"""

from __future__ import annotations

from pathlib import Path

from .binary import decode_scene, validate_scene
from .errors import ConfigError, ErrorCode, SceneError
from .model import Scene


INJECTIONS = (
    "chunk_index",
    "missing_media",
    "camera_nonfinite",
    "density_truncated",
    "config_budget",
)


def load_scene_with_injection(
    source: bytes | bytearray | memoryview | str | Path,
    inject: str | None = None,
) -> Scene:
    if isinstance(source, (str, Path)):
        data = Path(source).read_bytes()
    else:
        data = bytes(source)
    if inject == "density_truncated":
        # Remove part of the final density payload, not metadata.  The decoder
        # reports E_DENSITY_TRUNCATED with the exact payload offset.
        data = data[:-max(1, min(8, len(data) // 100))]
    scene = decode_scene(data)
    if inject == "camera_nonfinite":
        scene.camera.intrinsics[0, 0] = float("nan")
    elif inject == "missing_media":
        if not scene.media:
            raise SceneError(
                "cannot remove a media entry from an empty scene",
                "media",
                -1,
                code=ErrorCode.MEDIA_MISSING,
                details={"injection": inject},
            )
        removed = scene.media.pop(0)
        raise SceneError(
            "declared media entry is missing",
            "media[0]",
            removed.source_offset,
            code=ErrorCode.MEDIA_MISSING,
            details={
                "declared": scene.declared_media_count,
                "actual": len(scene.media),
                "injection": inject,
            },
        )
    elif inject == "chunk_index":
        if not scene.density_fields:
            raise SceneError(
                "cannot corrupt a chunk index in a scene without density fields",
                "density_fields",
                -1,
                code=ErrorCode.CHUNK_INDEX,
            )
        field = scene.density_fields[sorted(scene.density_fields)[0]]
        if not field.chunks:
            raise SceneError(
                "cannot corrupt an empty chunk index",
                f"density_fields[{field.id}].chunks",
                field.source_offset,
                code=ErrorCode.CHUNK_INDEX,
            )
        first_key = sorted(field.chunks, key=lambda value: (value[2], value[1], value[0]))[0]
        chunk = field.chunks.pop(first_key)
        index_offset = field.chunk_offsets.pop(first_key, field.source_offset)
        bad_key = (field.chunk_count()[0] + 7, first_key[1], first_key[2])
        field.chunks[bad_key] = chunk
        field.chunk_offsets[bad_key] = index_offset
    elif inject not in (None, ""):
        raise SceneError(
            "unknown scene injection",
            "inject",
            -1,
            code=ErrorCode.SCENE_INVALID,
            details={"injection": inject},
        )
    validate_scene(scene)
    return scene


__all__ = ["INJECTIONS", "load_scene_with_injection"]
