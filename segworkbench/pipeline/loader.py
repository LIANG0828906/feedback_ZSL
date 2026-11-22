"""确定性样本加载、元数据校验与观测基线登记。"""
from __future__ import annotations

import json
import os
from dataclasses import dataclass
from typing import Any, Dict, List

import numpy as np

from segworkbench.core.errors import ErrorCode, fail
from segworkbench.utils.hashing import sha256_array, sha256_json
from segworkbench.utils.pngcodec import read_png


@dataclass(frozen=True)
class RegionTruth:
    region_id: str
    block_id: str
    shape: str
    color: List[int]
    truth: Dict[str, Any]
    low_contrast: bool


@dataclass(frozen=True)
class LoadedSample:
    sample_id: str
    image: np.ndarray
    manifest: Dict[str, Any]
    regions: List[RegionTruth]
    image_hash: str
    truth_fingerprint: str
    block_grid: Dict[str, int]


def _load_manifest(manifest_path: str) -> Dict[str, Any]:
    if not os.path.exists(manifest_path):
        raise fail(ErrorCode.E_DECODE, f"真值清单缺失: {manifest_path}", path=manifest_path)
    try:
        with open(manifest_path, "r", encoding="utf-8") as handle:
            manifest = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise fail(ErrorCode.E_DECODE, f"真值清单无法解析: {manifest_path}",
                   path=manifest_path, detail=str(exc)) from exc
    for field_name in ("image", "regions", "block_grid", "truth_fingerprint"):
        if field_name not in manifest:
            raise fail(ErrorCode.E_META, f"清单缺少字段: {field_name}", field=field_name, path=manifest_path)
    return manifest


def load_sample(data_dir: str, sample_id: str) -> LoadedSample:
    """读取 PNG + JSON 并完成全部加载期校验。

    校验顺序固定：文件可读 → 形状/通道 → 块网格 → 区域数量 → 哈希基线。
    任何真值缺失都会在进入分割前抛错。
    """
    manifest_path = os.path.join(data_dir, f"{sample_id}.json")
    manifest = _load_manifest(manifest_path)
    image_meta = manifest["image"]
    for field_name in ("width", "height", "channels", "filename", "sha256"):
        if field_name not in image_meta:
            raise fail(ErrorCode.E_META, f"image 缺少字段: {field_name}", field=f"image.{field_name}")

    image_path = os.path.join(data_dir, image_meta["filename"])
    image = read_png(image_path)

    height, width, channels = image.shape
    if width != int(image_meta["width"]) or height != int(image_meta["height"]):
        raise fail(
            ErrorCode.E_META,
            "图像尺寸与清单不一致",
            field="image.width/height",
            actual=[width, height],
            expected=[image_meta["width"], image_meta["height"]],
        )
    if channels != int(image_meta["channels"]):
        raise fail(ErrorCode.E_META, "通道数与清单不一致", field="image.channels",
                   actual=channels, expected=image_meta["channels"])

    grid = manifest["block_grid"]
    expected_blocks = int(grid["rows"]) * int(grid["cols"])
    if width != int(grid["cols"]) * int(grid["block_width"]) or height != int(grid["rows"]) * int(
        grid["block_height"]
    ):
        raise fail(ErrorCode.E_META, "块网格与图像尺寸不一致", field="block_grid")

    regions = manifest.get("regions")
    if not regions:
        raise fail(ErrorCode.E_META, "真值区域缺失，禁止继续分割", field="regions")
    block_ids = {f"b{idx:02d}" for idx in range(expected_blocks)}
    region_blocks = set()
    parsed: List[RegionTruth] = []
    for index, record in enumerate(regions):
        for field_name in ("region_id", "block_id", "truth"):
            if field_name not in record:
                raise fail(ErrorCode.E_META, f"区域 {index} 缺少字段: {field_name}",
                           field=f"regions[{index}].{field_name}")
        truth = record["truth"]
        for field_name in ("mask_pixel_count", "centroid_xy", "bbox_xyxy"):
            if field_name not in truth:
                raise fail(ErrorCode.E_META, f"区域 {index} 真值缺少字段: {field_name}",
                           field=f"regions[{index}].truth.{field_name}")
        region_blocks.add(record["block_id"])
        parsed.append(
            RegionTruth(
                region_id=record["region_id"],
                block_id=record["block_id"],
                shape=record.get("shape", "unknown"),
                color=list(record.get("color", [])),
                truth=truth,
                low_contrast=bool(record.get("low_contrast", False)),
            )
        )
    if region_blocks != block_ids:
        missing = sorted(block_ids - region_blocks)
        extra = sorted(region_blocks - block_ids)
        raise fail(ErrorCode.E_META, "真值区域数量/块归属与块网格不一致", field="regions[].block_id",
                   missing=missing, extra=extra, expected_count=expected_blocks,
                   actual_count=len(region_blocks))

    image_hash = sha256_array(image)
    if image_hash != image_meta["sha256"]:
        raise fail(ErrorCode.E_HASH, "图像内容哈希与清单登记不一致", field="image.sha256",
                   actual=image_hash, expected=image_meta["sha256"])
    recomputed_fingerprint = sha256_json(
        [{"region_id": r.region_id, "block_id": r.block_id, "truth": r.truth} for r in parsed]
    )
    if recomputed_fingerprint != manifest["truth_fingerprint"]:
        raise fail(ErrorCode.E_HASH, "真值指纹复算不一致", field="truth_fingerprint",
                   actual=recomputed_fingerprint, expected=manifest["truth_fingerprint"])

    return LoadedSample(
        sample_id=manifest["sample_id"],
        image=image,
        manifest=manifest,
        regions=parsed,
        image_hash=image_hash,
        truth_fingerprint=manifest["truth_fingerprint"],
        block_grid=grid,
    )


def observation_baseline(sample: LoadedSample) -> Dict[str, str]:
    """登记后续验证使用的观测基准（图像哈希 + 真值指纹）。"""
    return {
        "sample_id": sample.sample_id,
        "image_sha256": sample.image_hash,
        "truth_fingerprint": sample.truth_fingerprint,
        "baseline_fingerprint": sha256_json(
            {"image": sample.image_hash, "truth": sample.truth_fingerprint}
        ),
    }
