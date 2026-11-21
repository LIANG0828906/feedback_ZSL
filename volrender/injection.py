"""Pre-render fault injection at scene/config loading boundaries."""

from __future__ import annotations

import json
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .binary import MAGIC, _PREFIX, decode_scene, encode_scene
from .config import RenderConfig, parse_config
from .errors import AppError, ErrorCode
from .scene import Scene


INJECTIONS = (
    "corrupt_tile_index",
    "missing_medium",
    "nonfinite_camera",
    "truncated_density",
    "budget_oversample",
)


@dataclass(frozen=True, slots=True)
class LoadedInputs:
    scene: Scene
    config: RenderConfig
    scene_bytes: bytes
    config_bytes: bytes


def _read_scene_container(raw: bytes) -> tuple[dict[str, Any], bytes]:
    if len(raw) < _PREFIX.size + 32:
        raise ValueError("scene container too short")
    magic, header_length, payload_length = _PREFIX.unpack_from(raw, 0)
    if magic != MAGIC:
        raise ValueError("bad magic")
    header_start = _PREFIX.size
    header_end = header_start + header_length
    payload_end = header_end + payload_length
    header = json.loads(raw[header_start:header_end].decode("utf-8"))
    payload = raw[header_end:payload_end]
    return header, payload


def _inject_scene(
    injection: str, raw: bytes, config_data: dict[str, Any]
) -> tuple[bytes, dict[str, Any], tuple[ErrorCode, str, str] | None]:
    header, payload = _read_scene_container(raw)
    if injection == "corrupt_tile_index":
        header["density_blocks"][0]["offset"] = 10**9
        mutated = encode_scene(header, payload)
        return (
            mutated,
            config_data,
            (
                ErrorCode.E_TILE_INDEX_CORRUPT,
                "density block index points outside payload",
                "density_blocks[0].offset",
            ),
        )
    if injection == "missing_medium":
        header["media"] = header["media"][1:]
        mutated = encode_scene(header, payload)
        return (
            mutated,
            config_data,
            (
                ErrorCode.E_MEDIUM_MISSING,
                "density block references a missing medium entry",
                "density_blocks[0].medium_id",
            ),
        )
    if injection == "nonfinite_camera":
        header["camera"]["K"][0][0] = float("nan")
        data = json.dumps(
            header,
            sort_keys=True,
            separators=(",", ":"),
            allow_nan=True,
        ).encode("utf-8")
        prefix = _PREFIX.pack(MAGIC, len(data), len(payload))
        import hashlib

        body = prefix + data + payload
        mutated = body + hashlib.sha256(body).digest()
        return (
            mutated,
            config_data,
            (
                ErrorCode.E_CAMERA_MATRIX_NONFINITE,
                "camera matrix contains NaN/Inf",
                "camera.K[0][0]",
            ),
        )
    if injection == "truncated_density":
        return (
            raw[:-17],
            config_data,
            (
                ErrorCode.E_DENSITY_TRUNCATED,
                "density payload is truncated",
                "binary.density_payload",
            ),
        )
    if injection == "budget_oversample":
        mutated_config = dict(config_data)
        mutated_config["sample_budget"] = 0
        return (
            raw,
            mutated_config,
            (
                ErrorCode.E_BUDGET_CONFIG_INVALID,
                "sample_budget injection is outside the accepted range",
                "config.sample_budget",
            ),
        )
    raise AppError(
        ErrorCode.E_USAGE,
        f"unknown injection: {injection}",
        field="injection",
    )


def _normal_validation_error(
    exc: AppError,
    *,
    injection: str,
    injection_point: str,
    message: str,
) -> AppError:
    mapping = {
        "corrupt_tile_index": ErrorCode.E_TILE_INDEX_CORRUPT,
        "missing_medium": ErrorCode.E_MEDIUM_MISSING,
        "nonfinite_camera": ErrorCode.E_CAMERA_MATRIX_NONFINITE,
        "truncated_density": ErrorCode.E_DENSITY_TRUNCATED,
        "budget_oversample": ErrorCode.E_BUDGET_CONFIG_INVALID,
    }
    return AppError(
        mapping[injection],
        message,
        field=exc.field,
        medium_id=exc.medium_id,
        injection_point=injection_point,
        details={"injected": injection, "validation_message": exc.message},
    )


def load_inputs(
    scene_path: str | Path,
    config_path: str | Path,
    *,
    injection: str | None = None,
) -> LoadedInputs:
    if injection is not None and injection not in INJECTIONS:
        raise AppError(
            ErrorCode.E_USAGE,
            f"unknown injection: {injection}",
            field="injection",
        )
    try:
        scene_bytes = Path(scene_path).read_bytes()
    except OSError as exc:
        raise AppError(
            ErrorCode.E_IO,
            f"cannot read scene binary: {exc}",
            field="scene.path",
        ) from exc
    try:
        config_bytes = Path(config_path).read_bytes()
    except OSError as exc:
        raise AppError(
            ErrorCode.E_IO,
            f"cannot read config: {exc}",
            field="config.path",
        ) from exc
    try:
        config_data = json.loads(config_bytes.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise AppError(
            ErrorCode.E_BUDGET_CONFIG_INVALID,
            f"configuration is not valid UTF-8 JSON: {exc}",
            field="config",
        ) from exc

    if injection is None:
        try:
            scene = decode_scene(scene_bytes)
        except AppError:
            raise
        config = parse_config(config_data)
        return LoadedInputs(
            scene=scene,
            config=config,
            scene_bytes=scene_bytes,
            config_bytes=config_bytes,
        )

    try:
        mutated_scene, mutated_config, expected = _inject_scene(
            injection, scene_bytes, config_data
        )
    except (ValueError, KeyError, IndexError, json.JSONDecodeError) as exc:
        raise AppError(
            ErrorCode.E_USAGE,
            f"injection {injection} could not be prepared: {exc}",
            field="injection",
            injection_point=injection,
        ) from exc
    assert expected is not None
    expected_code, expected_message, injection_point = expected
    try:
        scene = decode_scene(mutated_scene)
        config = parse_config(mutated_config)
    except AppError as exc:
        raise _normal_validation_error(
            exc,
            injection=injection,
            injection_point=injection_point,
            message=expected_message,
        ) from exc
    # A compatible validator may accept the mutated input in a future
    # format version; this wrapper still makes the injected fault explicit.
    raise AppError(
        expected_code,
        expected_message,
        field=injection_point,
        injection_point=injection_point,
        details={"injected": injection, "scene_id": scene.scene_id},
    )
