"""Atomic checkpoint manifests and append-only hash-chain validation."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any, Mapping

from .errors import BusinessError, ErrorCode
from .utils import canonical_json, sha256_bytes, sha256_file

SNAPSHOT_VERSION = 1
ENGINE_VERSION = "1.0.0"
CHAIN_SEED = sha256_bytes(b"GAME_AUDIO_RESTORER_CHAIN_V1")
MANIFEST_NAME = "snapshot.json"


def _recovery_error(
    message: str,
    work_dir: str | Path,
    index: int,
    field: str,
    **extra: Any,
) -> BusinessError:
    return BusinessError(
        ErrorCode.RECOVERY_INVALID,
        message,
        {
            "first_break_segment": int(index),
            "location": {
                "work_dir": os.fspath(work_dir),
                "segment": int(index),
                "field": field,
                **extra,
            },
        },
    )


def manifest_path(work_dir: str | Path) -> Path:
    return Path(work_dir) / MANIFEST_NAME


def write_manifest(work_dir: str | Path, manifest: Mapping[str, Any]) -> None:
    from .wav_io import atomic_write_bytes

    payload = json.dumps(
        manifest,
        ensure_ascii=False,
        sort_keys=True,
        indent=2,
        allow_nan=False,
    ).encode("utf-8") + b"\n"
    atomic_write_bytes(manifest_path(work_dir), payload)


def chain_hash(previous: str, entry: Mapping[str, Any]) -> str:
    payload = dict(entry)
    payload.pop("chain_hash", None)
    return sha256_bytes((previous + "\n" + canonical_json(payload)).encode("utf-8"))


def validate_manifest(
    work_dir: str | Path,
    manifest: Mapping[str, Any],
    *,
    expected_source_sha256: str | None = None,
    expected_recipe_hash: str | None = None,
) -> None:
    if manifest.get("snapshot_version") != SNAPSHOT_VERSION:
        raise _recovery_error(
            "快照版本缺失或不支持",
            work_dir,
            0,
            "snapshot_version",
            value=manifest.get("snapshot_version"),
        )
    source = manifest.get("source")
    recipe = manifest.get("recipe")
    segments = manifest.get("segments")
    if not isinstance(source, Mapping):
        raise _recovery_error("快照缺少 source", work_dir, 0, "source")
    if not isinstance(recipe, Mapping):
        raise _recovery_error("快照缺少 recipe", work_dir, 0, "recipe")
    if not isinstance(segments, list):
        raise _recovery_error("快照缺少 segments 列表", work_dir, 0, "segments")
    for field in ("sha256", "sample_rate", "channels", "frames", "segment_size", "total_segments"):
        if field not in source:
            raise _recovery_error("source 字段缺失", work_dir, 0, f"source.{field}")
    for field in ("version", "field_mapping", "lost_fields"):
        if field not in recipe:
            raise _recovery_error("recipe 字段缺失", work_dir, 0, f"recipe.{field}")
    output = manifest.get("output")
    if not isinstance(output, Mapping) or not {"sample_rate", "channels"} <= set(output):
        raise _recovery_error("快照缺少有效 output 配置", work_dir, 0, "output")
    if expected_source_sha256 and source.get("sha256") != expected_source_sha256:
        raise _recovery_error(
            "快照源 WAV 哈希与当前文件不一致",
            work_dir,
            0,
            "source.sha256",
            declared=source.get("sha256"),
            expected=expected_source_sha256,
        )
    if expected_recipe_hash and manifest.get("recipe_hash") != expected_recipe_hash:
        raise BusinessError(
            ErrorCode.VERSION_STALE,
            "当前配方版本/内容与已提交片段清单不一致",
            {
                "declared_recipe_hash": manifest.get("recipe_hash"),
                "expected_recipe_hash": expected_recipe_hash,
                "snapshot_recipe_version": recipe.get("version"),
                "first_break_segment": len(segments),
            },
        )

    source_frames = source.get("frames")
    if (
        not isinstance(source_frames, int)
        or isinstance(source_frames, bool)
        or source_frames < 0
    ):
        raise _recovery_error("source.frames 非法", work_dir, 0, "source.frames")
    for field in ("sample_rate", "channels", "segment_size", "total_segments"):
        value = source.get(field)
        if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
            raise _recovery_error(
                f"source.{field} 非法", work_dir, 0, f"source.{field}"
            )
    expected_total = (
        (source_frames + source["segment_size"] - 1) // source["segment_size"]
        if source_frames
        else 0
    )
    if source["total_segments"] != expected_total:
        raise _recovery_error(
            "快照声明片段总数与源帧数/片段大小不一致",
            work_dir,
            0,
            "source.total_segments",
            declared=source["total_segments"],
            expected=expected_total,
        )

    previous_chain = CHAIN_SEED
    previous_end = 0
    for index, entry in enumerate(segments):
        if not isinstance(entry, Mapping):
            raise _recovery_error("片段条目不是对象", work_dir, index, "segment")
        required = (
            "index",
            "start",
            "end",
            "path",
            "sha256",
            "metrics",
            "parameters",
            "recipe_hash",
            "recipe_version",
            "prev_chain_hash",
            "chain_hash",
        )
        for field in required:
            if field not in entry:
                raise _recovery_error("片段字段缺失", work_dir, index, field)
        if (
            not isinstance(entry["index"], int)
            or isinstance(entry["index"], bool)
            or entry["index"] != index
        ):
            raise _recovery_error(
                "片段序号不连续",
                work_dir,
                index,
                "index",
                value=entry["index"],
            )
        if (
            not isinstance(entry["start"], int)
            or isinstance(entry["start"], bool)
            or entry["start"] != previous_end
        ):
            raise _recovery_error(
                "片段起点与上一片段终点不连续",
                work_dir,
                index,
                "start",
                value=entry["start"],
                expected=previous_end,
            )
        if (
            not isinstance(entry["end"], int)
            or isinstance(entry["end"], bool)
            or entry["end"] <= entry["start"]
        ):
            raise _recovery_error("片段终点非法", work_dir, index, "end")
        if entry["end"] > source_frames:
            raise _recovery_error(
                "片段终点超过源帧数",
                work_dir,
                index,
                "end",
                value=entry["end"],
                source_frames=source_frames,
            )
        length = entry["end"] - entry["start"]
        if length > source["segment_size"]:
            raise _recovery_error(
                "片段长度超过分段尺寸",
                work_dir,
                index,
                "end",
                length=length,
                segment_size=source["segment_size"],
            )
        if (
            index < source["total_segments"] - 1
            and length != source["segment_size"]
        ):
            raise _recovery_error(
                "非末尾片段长度不等于分段尺寸",
                work_dir,
                index,
                "end",
                length=length,
                segment_size=source["segment_size"],
            )
        if entry["recipe_version"] != manifest.get("recipe_version"):
            raise BusinessError(
                ErrorCode.VERSION_STALE,
                "片段配方版本与当前快照不一致",
                {
                    "first_break_segment": index,
                    "segment_recipe_version": entry["recipe_version"],
                    "snapshot_recipe_version": manifest.get("recipe_version"),
                },
            )
        if entry["recipe_hash"] != manifest.get("recipe_hash"):
            raise BusinessError(
                ErrorCode.VERSION_STALE,
                "片段的配方哈希与当前快照不一致",
                {
                    "first_break_segment": index,
                    "segment_recipe_hash": entry["recipe_hash"],
                    "snapshot_recipe_hash": manifest.get("recipe_hash"),
                },
            )
        if entry["prev_chain_hash"] != previous_chain:
            raise _recovery_error(
                "片段哈希链前驱断裂",
                work_dir,
                index,
                "prev_chain_hash",
                value=entry["prev_chain_hash"],
                expected=previous_chain,
            )
        try:
            expected_chain = chain_hash(previous_chain, entry)
        except (TypeError, ValueError) as exc:
            raise _recovery_error(
                "片段哈希链字段不可序列化",
                work_dir,
                index,
                "chain_hash",
                error=str(exc),
            ) from exc
        if entry["chain_hash"] != expected_chain:
            raise _recovery_error(
                "片段哈希链校验失败",
                work_dir,
                index,
                "chain_hash",
                value=entry["chain_hash"],
                expected=expected_chain,
            )
        relative = Path(str(entry["path"]))
        if relative.is_absolute() or ".." in relative.parts:
            raise _recovery_error("片段路径越界", work_dir, index, "path")
        segment_path = Path(work_dir) / relative
        if not segment_path.is_file():
            raise _recovery_error("片段文件缺失", work_dir, index, "path", value=str(relative))
        try:
            actual = sha256_file(segment_path)
        except OSError as exc:
            raise _recovery_error(
                "片段文件读取失败",
                work_dir,
                index,
                "path",
                error=str(exc),
            ) from exc
        if actual != entry["sha256"]:
            raise _recovery_error(
                "片段内容哈希不匹配",
                work_dir,
                index,
                "sha256",
                declared=entry["sha256"],
                actual=actual,
            )
        previous_chain = expected_chain
        previous_end = int(entry["end"])
    if len(segments) > int(source["total_segments"]):
        raise _recovery_error(
            "已提交片段数超过快照声明总数",
            work_dir,
            int(source["total_segments"]),
            "segments",
        )
    if len(segments) == int(source["total_segments"]) and previous_end != source_frames:
        raise _recovery_error(
            "完整快照的最后片段未覆盖源 WAV 全部帧",
            work_dir,
            max(0, len(segments) - 1),
            "end",
            last_end=previous_end,
            source_frames=source_frames,
        )


def load_and_validate(
    work_dir: str | Path,
    *,
    expected_source_sha256: str | None = None,
    expected_recipe_hash: str | None = None,
) -> dict[str, Any]:
    path = manifest_path(work_dir)
    try:
        raw = path.read_bytes()
    except FileNotFoundError:
        raise
    except OSError as exc:
        raise _recovery_error(
            "快照读取失败",
            work_dir,
            0,
            "snapshot",
            error=str(exc),
        ) from exc
    try:
        manifest = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise _recovery_error(
            "快照 JSON 损坏",
            work_dir,
            0,
            "snapshot",
            error=str(exc),
        ) from exc
    if not isinstance(manifest, dict):
        raise _recovery_error("快照根节点不是对象", work_dir, 0, "snapshot")
    validate_manifest(
        work_dir,
        manifest,
        expected_source_sha256=expected_source_sha256,
        expected_recipe_hash=expected_recipe_hash,
    )
    return manifest


def append_segment_manifest(
    work_dir: str | Path,
    manifest: dict[str, Any],
    *,
    index: int,
    start: int,
    end: int,
    relative_path: str,
    file_sha256: str,
    metrics: Mapping[str, Any],
    parameters: Mapping[str, Any],
    recipe_hash: str,
    recipe_version: int,
) -> dict[str, Any]:
    segments = manifest["segments"]
    previous_chain = segments[-1]["chain_hash"] if segments else CHAIN_SEED
    entry: dict[str, Any] = {
        "index": index,
        "start": start,
        "end": end,
        "path": relative_path,
        "sha256": file_sha256,
        "metrics": dict(metrics),
        "parameters": dict(parameters),
        "recipe_hash": recipe_hash,
        "recipe_version": recipe_version,
        "prev_chain_hash": previous_chain,
    }
    entry["chain_hash"] = chain_hash(previous_chain, entry)
    updated = dict(manifest)
    updated["segments"] = [*segments, entry]
    return updated
