"""Atomic resumable render snapshots."""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .render import iter_chunks
from .scene import Scene


SNAPSHOT_FORMAT = 1
SNAPSHOT_NAME = "render_snapshot.npz"


@dataclass
class SnapshotState:
    radiance: np.ndarray
    optical_depth: np.ndarray
    light_radiance: np.ndarray
    completed: np.ndarray
    recovery_count: int


@dataclass(frozen=True)
class SnapshotLoad:
    state: SnapshotState | None
    status: str
    detail: str | None = None


def _array_digest(arrays: dict[str, np.ndarray]) -> str:
    digest = hashlib.sha256()
    for name in sorted(arrays):
        array = np.ascontiguousarray(arrays[name])
        digest.update(name.encode("ascii"))
        digest.update(str(array.dtype).encode("ascii"))
        digest.update(str(array.shape).encode("ascii"))
        digest.update(array.tobytes())
    return digest.hexdigest()


def _manifest_digest(manifest: dict[str, Any]) -> str:
    core = {
        key: value
        for key, value in manifest.items()
        if key != "manifest_digest"
    }
    canonical = json.dumps(
        core,
        ensure_ascii=True,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
    return hashlib.sha256(canonical).hexdigest()


def _completed_ids(
    completed: np.ndarray, chunk_size: tuple[int, int], width: int, height: int
) -> list[str]:
    result: list[str] = []
    for chunk_id, chunk_y, chunk_x, *_ in iter_chunks(
        width, height, chunk_size
    ):
        if completed[chunk_y, chunk_x]:
            result.append(chunk_id)
    return result


def _chunk_summaries(
    state: SnapshotState,
    scene: Scene,
) -> dict[str, dict[str, Any]]:
    camera = scene.camera
    summaries: dict[str, dict[str, Any]] = {}
    luminance_weights = np.asarray([0.2126, 0.7152, 0.0722])
    for chunk_id, _, _, x0, x1, y0, y1 in iter_chunks(
        camera.width, camera.height, scene.render.chunk_size
    ):
        chunk_y = y0 // scene.render.chunk_size[1]
        chunk_x = x0 // scene.render.chunk_size[0]
        if not state.completed[chunk_y, chunk_x]:
            continue
        rgb = state.radiance[y0:y1, x0:x1, :]
        luminance = rgb @ luminance_weights
        summaries[chunk_id] = {
            "mean_rgb": [
                round(float(value), 9) for value in np.mean(rgb, axis=(0, 1))
            ],
            "mean_luminance": round(float(np.mean(luminance)), 9),
            "max_luminance": round(float(np.max(luminance)), 9),
        }
    return summaries


def write_snapshot(
    path: Path,
    state: SnapshotState,
    scene: Scene,
) -> None:
    completed_u8 = state.completed.astype(np.uint8)
    arrays = {
        "completed": completed_u8,
        "light_radiance": state.light_radiance,
        "optical_depth": state.optical_depth,
        "radiance": state.radiance,
    }
    completed_ids = _completed_ids(
        state.completed,
        scene.render.chunk_size,
        scene.camera.width,
        scene.camera.height,
    )
    manifest = {
        "format": SNAPSHOT_FORMAT,
        "scene_digest": scene.digest,
        "render_signature": scene.render_signature,
        "scene_id": scene.scene_id,
        "step": float(scene.render.step),
        "substeps": scene.render.substeps,
        "max_samples": scene.render.max_samples,
        "chunk_size": list(scene.render.chunk_size),
        "image_size": [scene.camera.width, scene.camera.height],
        "completed_chunk_ids": completed_ids,
        "completed_chunk_count": len(completed_ids),
        "chunk_color_summaries": _chunk_summaries(state, scene),
        "recovery_count": int(state.recovery_count),
        "buffer_digest": _array_digest(arrays),
    }
    manifest["manifest_digest"] = _manifest_digest(manifest)
    manifest_bytes = json.dumps(
        manifest,
        ensure_ascii=True,
        allow_nan=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
    arrays["manifest"] = np.frombuffer(manifest_bytes, dtype=np.uint8)

    temp = path.with_name(f".{path.name}.{os.getpid()}.tmp.npz")
    try:
        with temp.open("wb") as handle:
            np.savez_compressed(handle, **arrays)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp, path)
    finally:
        if temp.exists():
            temp.unlink()


def _discard(path: Path) -> None:
    try:
        path.unlink()
    except FileNotFoundError:
        pass


def load_snapshot(path: Path, scene: Scene) -> SnapshotLoad:
    if not path.exists():
        return SnapshotLoad(None, "absent")
    try:
        with np.load(path, allow_pickle=False) as archive:
            required = {
                "manifest",
                "completed",
                "radiance",
                "optical_depth",
                "light_radiance",
            }
            if not required.issubset(set(archive.files)):
                raise ValueError("快照缺少必需数组")
            manifest = json.loads(
                archive["manifest"].tobytes().decode("utf-8")
            )
            completed = archive["completed"]
            radiance = archive["radiance"]
            optical_depth = archive["optical_depth"]
            light_radiance = archive["light_radiance"]
    except Exception as exc:
        _discard(path)
        return SnapshotLoad(None, "corrupt", type(exc).__name__)

    try:
        if not isinstance(manifest, dict):
            raise ValueError("manifest 不是对象")
        if manifest.get("format") != SNAPSHOT_FORMAT:
            raise ValueError("快照格式版本不一致")
        if manifest.get("manifest_digest") != _manifest_digest(manifest):
            raise ValueError("manifest 校验和不一致")
        if manifest.get("scene_digest") != scene.digest:
            raise ValueError("场景摘要不一致")
        if manifest.get("render_signature") != scene.render_signature:
            raise ValueError("渲染参数或相机摘要不一致")
        if manifest.get("chunk_size") != list(scene.render.chunk_size):
            raise ValueError("分块尺寸不一致")
        if manifest.get("image_size") != [
            scene.camera.width,
            scene.camera.height,
        ]:
            raise ValueError("图像尺寸不一致")
        if float(manifest.get("step", -1.0)) != float(scene.render.step):
            raise ValueError("步长不一致")
        if int(manifest.get("substeps", -1)) != scene.render.substeps:
            raise ValueError("求积子步数不一致")
        if int(manifest.get("max_samples", -1)) != scene.render.max_samples:
            raise ValueError("采样上限不一致")

        width = scene.camera.width
        height = scene.camera.height
        chunk_width, chunk_height = scene.render.chunk_size
        grid_shape = (
            (height + chunk_height - 1) // chunk_height,
            (width + chunk_width - 1) // chunk_width,
        )
        expected_shapes = {
            "completed": grid_shape,
            "radiance": (height, width, 3),
            "optical_depth": (height, width),
            "light_radiance": (
                len(scene.lights),
                height,
                width,
                3,
            ),
        }
        arrays = {
            "completed": completed,
            "radiance": radiance,
            "optical_depth": optical_depth,
            "light_radiance": light_radiance,
        }
        for name, shape in expected_shapes.items():
            if arrays[name].shape != shape:
                raise ValueError(f"{name} 形状不一致")
        if completed.dtype != np.uint8:
            raise ValueError("completed 类型不一致")
        completed_bool = completed.astype(bool)
        flat = completed_bool.reshape(-1)
        if np.any(flat[1:] & ~flat[:-1]):
            raise ValueError("已完成块不是行优先前缀")
        for name in ("radiance", "optical_depth", "light_radiance"):
            if not np.all(np.isfinite(arrays[name])):
                raise ValueError(f"{name} 含非有限值")

        expected_completed_ids = _completed_ids(
            completed_bool, scene.render.chunk_size, width, height
        )
        if manifest.get("completed_chunk_ids") != expected_completed_ids:
            raise ValueError("已完成块编号不一致")
        if int(manifest.get("completed_chunk_count", -1)) != len(
            expected_completed_ids
        ):
            raise ValueError("已完成块计数不一致")
        recovery_count = int(manifest.get("recovery_count", -1))
        if recovery_count < 0:
            raise ValueError("恢复次数非法")
        digest = _array_digest(
            {
                name: arrays[name]
                for name in (
                    "completed",
                    "light_radiance",
                    "optical_depth",
                    "radiance",
                )
            }
        )
        if digest != manifest.get("buffer_digest"):
            raise ValueError("缓冲区校验和不一致")
    except Exception as exc:
        _discard(path)
        return SnapshotLoad(None, "invalid", str(exc))

    return SnapshotLoad(
        SnapshotState(
            radiance=np.array(radiance, dtype=np.float64, copy=True),
            optical_depth=np.array(
                optical_depth, dtype=np.float64, copy=True
            ),
            light_radiance=np.array(
                light_radiance, dtype=np.float64, copy=True
            ),
            completed=np.array(completed_bool, dtype=bool, copy=True),
            recovery_count=recovery_count,
        ),
        "loaded",
    )
