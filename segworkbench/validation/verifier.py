"""独立验证工具。

本模块刻意不导入引擎/生长实现，只依赖：
  * 检查点目录中的 npy/json 持久化产物；
  * 样本清单中的真值指纹与解析真值；
  * 独立实现的几何统计（与主链路同坐标约定，但代码路径独立）。

它重新拼接掩码、重算几何与偏差，并与主链路记录逐项比对；发现不一致
时返回首个不一致像素坐标、所属块、阶段与实测偏差。
"""
from __future__ import annotations

import json
import os
from dataclasses import dataclass
from typing import Any, Dict, List, Optional

import numpy as np

from segworkbench.core.errors import ErrorCode, fail
from segworkbench.core.versioning import ALGORITHM_VERSION, SNAPSHOT_FORMAT_VERSION
from segworkbench.persistence.checkpoint import PHASE_ARTIFACTS
from segworkbench.pipeline.blocks import block_rect
from segworkbench.pipeline.geometry import region_statistics
from segworkbench.utils.hashing import sha256_array, sha256_json
from segworkbench.utils.pngcodec import read_png


@dataclass
class Mismatch:
    kind: str
    block_id: str
    phase: str
    pixel_xy: Optional[List[int]]
    detail: Dict[str, Any]

    def to_dict(self) -> Dict[str, Any]:
        return {
            "kind": self.kind,
            "block_id": self.block_id,
            "phase": self.phase,
            "first_pixel_xy": self.pixel_xy,
            "detail": self.detail,
        }


class IndependentVerifier:
    def __init__(self, checkpoint_root: str, data_dir: str, sample_id: str) -> None:
        self.root = os.path.abspath(checkpoint_root)
        self.data_dir = data_dir
        self.sample_id = sample_id

    def _load_json(self, path: str) -> Dict[str, Any]:
        if not os.path.exists(path):
            raise fail(ErrorCode.E_DECODE, f"验证输入缺失: {path}", path=path)
        with open(path, "r", encoding="utf-8") as handle:
            return json.load(handle)

    def _load_phase(self, block_id: str, phase: str) -> Dict[str, Any]:
        npy_name, json_name = PHASE_ARTIFACTS[phase]
        block_dir = os.path.join(self.root, "blocks", block_id)
        record = self._load_json(os.path.join(block_dir, json_name))
        if npy_name is not None:
            array = np.load(os.path.join(block_dir, npy_name), allow_pickle=False)
            record["array"] = array
            record["array_hash_valid"] = sha256_array(array) == record["array_sha256"]
        if sha256_json(record["metadata"]) != record["metadata_sha256"]:
            raise fail(ErrorCode.E_HASH, "验证发现阶段元数据哈希不一致",
                       block_id=block_id, phase=phase)
        return record

    def verify(self) -> Dict[str, Any]:
        manifest = self._load_json(os.path.join(self.data_dir, f"{self.sample_id}.json"))
        header = self._load_json(os.path.join(self.root, "snapshot.json"))
        mismatches: List[Mismatch] = []

        # 1. 版本与指纹校验（恢复边界的独立复算）。
        image = read_png(os.path.join(self.data_dir, manifest["image"]["filename"]))
        image_hash = sha256_array(image)
        if image_hash != header["image_sha256"] or image_hash != manifest["image"]["sha256"]:
            mismatches.append(Mismatch("image_hash", "-", "-", None,
                                       {"snapshot": header["image_sha256"],
                                        "manifest": manifest["image"]["sha256"],
                                        "recomputed": image_hash}))
        if header["algorithm_version"] != ALGORITHM_VERSION:
            mismatches.append(Mismatch("algorithm_version", "-", "-", None,
                                       {"actual": header["algorithm_version"]}))
        if header["format_version"] != SNAPSHOT_FORMAT_VERSION:
            mismatches.append(Mismatch("format_version", "-", "-", None,
                                       {"actual": header["format_version"]}))
        truth_fingerprint = sha256_json(
            [{"region_id": r["region_id"], "block_id": r["block_id"], "truth": r["truth"]}
             for r in manifest["regions"]]
        )
        if truth_fingerprint != header["truth_fingerprint"]:
            mismatches.append(Mismatch("truth_fingerprint", "-", "-", None,
                                       {"recomputed": truth_fingerprint,
                                        "snapshot": header["truth_fingerprint"]}))

        block_ids = sorted(header.get("block_phases", {}).keys())
        expected_blocks = {f"b{idx:02d}" for idx in
                           range(manifest["block_grid"]["rows"] * manifest["block_grid"]["cols"])}
        if set(block_ids) != expected_blocks:
            mismatches.append(Mismatch("block_set", "-", "-", None,
                                       {"missing": sorted(expected_blocks - set(block_ids)),
                                        "extra": sorted(set(block_ids) - expected_blocks)}))

        truth_by_block = {region["block_id"]: region for region in manifest["regions"]}
        full_mask = np.zeros(image.shape[:2], dtype=np.uint8)
        recomputed_table: List[Dict[str, Any]] = []
        params = header["parameters"]

        for block_id in block_ids:
            # 2. grow -> clean 阶段数组逐块比对（首个不一致像素定位）。
            grow = self._load_phase(block_id, "grow")
            clean = self._load_phase(block_id, "clean")
            self._check_index_hash(block_id, "grow", grow, header, mismatches)
            self._check_index_hash(block_id, "clean", clean, header, mismatches)
            if clean.get("array_hash_valid") is False:
                diff_y, diff_x = np.argwhere(
                    self._independent_clean(grow["array"].astype(bool), params)
                    != clean["array"].astype(bool)
                )[0]
                x0, y0, _, _ = block_rect(block_id)
                mismatches.append(Mismatch(
                    "artifact_hash_break", block_id, "clean",
                    [int(x0 + diff_x), int(y0 + diff_y)],
                    {"local_pixel_xy": [int(diff_x), int(diff_y)],
                     "expected_sha256": clean["array_sha256"],
                     "actual_sha256": sha256_array(clean["array"])}))
                continue

            # 独立重算 clean：内联形态学（不导入主链路 postprocess），输入仅为持久化 grow 掩码。
            expected_clean = self._independent_clean(grow["array"].astype(bool), params)
            if not np.array_equal(expected_clean, clean["array"].astype(bool)):
                diff_y, diff_x = np.argwhere(expected_clean != clean["array"].astype(bool))[0]
                x0, y0, _, _ = block_rect(block_id)
                mismatches.append(Mismatch(
                    "clean_mask_pixel", block_id, "clean",
                    [int(x0 + diff_x), int(y0 + diff_y)],
                    {"local_pixel_xy": [int(diff_x), int(diff_y)],
                     "expected": int(expected_clean[diff_y, diff_x]),
                     "recorded": int(clean["array"][diff_y, diff_x])}))

            # 3. 独立重算几何（不读取主链路 stats 里的统计值作为输入）。
            final_mask = clean["array"].astype(bool)
            stats_list = region_statistics(final_mask)
            stats_record = self._load_phase(block_id, "stats")
            recorded = stats_record["metadata"]["statistics"]
            if len(stats_list) != 1:
                mismatches.append(Mismatch("region_count", block_id, "stats", None,
                                           {"recomputed": len(stats_list), "recorded": 1}))
                continue
            recomputed = stats_list[0]
            x0, y0, _, _ = block_rect(block_id)
            recomputed["centroid_xy"] = [recomputed["centroid_xy"][0] + x0,
                                         recomputed["centroid_xy"][1] + y0]
            bx0, by0, bx1, by1 = recomputed["bbox_xyxy"]
            recomputed["bbox_xyxy"] = [bx0 + x0, by0 + y0, bx1 + x0, by1 + y0]
            first_diff = self._compare_statistics(block_id, recorded, recomputed, mismatches)

            # 4. 独立比对真值偏差。
            truth = truth_by_block[block_id]["truth"]
            deviation = self._recompute_deviation(recomputed, truth)
            recorded_deviation = stats_record["metadata"]["deviation"]
            self._compare_deviation(block_id, deviation, recorded_deviation, mismatches)

            x0, y0, x1, y1 = block_rect(block_id)
            full_mask[y0:y1, x0:x1] = clean["array"]
            recomputed_table.append({"block_id": block_id, "deviation": deviation,
                                     "first_diff": first_diff})

        # 5. 全图拼接哈希：与逐块 clean 哈希集合的稳定摘要交叉校验。
        stitch_hash = sha256_array(full_mask)
        block_hash_payload = {
            block_id: self._load_phase(block_id, "clean")["array_sha256"]
            for block_id in block_ids
        }
        return {
            "sample_id": self.sample_id,
            "passed": not mismatches,
            "mismatch_count": len(mismatches),
            "first_mismatch": mismatches[0].to_dict() if mismatches else None,
            "mismatches": [item.to_dict() for item in mismatches],
            "recomputed_stitch_sha256": stitch_hash,
            "block_clean_hashes": block_hash_payload,
            "recomputed_deviation": recomputed_table,
        }

    def _check_index_hash(self, block_id: str, phase: str, record: Dict[str, Any],
                          header: Dict[str, Any], mismatches: List[Mismatch]) -> None:
        phase_index = header["block_phases"][block_id][phase]
        if phase_index.get("array_sha256") != record["array_sha256"]:
            mismatches.append(Mismatch("phase_index_hash", block_id, phase, None,
                                       {"index": phase_index.get("array_sha256"),
                                        "artifact": record["array_sha256"]}))

    @staticmethod
    def _independent_clean(mask: np.ndarray, params: Dict[str, Any]) -> np.ndarray:
        """验证器自带的形态学实现：闭运算 -> 开运算 -> 小连通域剔除。"""
        radius = int(params["struct_radius"])

        def shift(source: np.ndarray, dy: int, dx: int) -> np.ndarray:
            out = np.zeros_like(source)
            out[max(0, dy):source.shape[0] + min(0, dy),
                max(0, dx):source.shape[1] + min(0, dx)] = \
                source[max(0, -dy):source.shape[0] - max(0, dy),
                       max(0, -dx):source.shape[1] - max(0, dx)]
            return out

        def dilate(source: np.ndarray) -> np.ndarray:
            out = np.zeros_like(source)
            for dy in range(-radius, radius + 1):
                for dx in range(-radius, radius + 1):
                    out |= shift(source, dy, dx)
            return out

        def erode(source: np.ndarray) -> np.ndarray:
            out = np.ones_like(source)
            for dy in range(-radius, radius + 1):
                for dx in range(-radius, radius + 1):
                    out &= shift(source, dy, dx)
            return out

        closed = erode(dilate(mask))
        opened = dilate(erode(closed))
        # 连通域筛选（独立 BFS 实现）。
        min_pixels = int(params["min_region_pixels"])
        height, width = opened.shape
        kept = np.zeros_like(opened)
        visited = np.zeros_like(opened)
        for y in range(height):
            for x in range(width):
                if not opened[y, x] or visited[y, x]:
                    continue
                stack = [(y, x)]
                visited[y, x] = True
                component = []
                while stack:
                    cy, cx = stack.pop()
                    component.append((cy, cx))
                    for ny, nx in ((cy - 1, cx), (cy + 1, cx), (cy, cx - 1), (cy, cx + 1)):
                        if 0 <= ny < height and 0 <= nx < width and opened[ny, nx] and not visited[ny, nx]:
                            visited[ny, nx] = True
                            stack.append((ny, nx))
                if len(component) >= min_pixels:
                    for cy, cx in component:
                        kept[cy, cx] = True
        return kept.astype(np.uint8)

    def _compare_statistics(self, block_id: str, recorded: Dict[str, Any],
                            recomputed: Dict[str, Any], mismatches: List[Mismatch]) -> Optional[List[int]]:
        checks = (
            ("area_pixels", recorded["area_pixels"], recomputed["area_pixels"]),
            ("centroid_xy", recorded["centroid_xy"], recomputed["centroid_xy"]),
            ("bbox_xyxy", recorded["bbox_xyxy"], recomputed["bbox_xyxy"]),
        )
        for name, left, right in checks:
            if not np.allclose(np.array(left, dtype=float), np.array(right, dtype=float), atol=1e-9):
                pixel = [recomputed["bbox_xyxy"][0], recomputed["bbox_xyxy"][1]]
                mismatches.append(Mismatch(f"statistics.{name}", block_id, "stats", pixel,
                                           {"recorded": left, "recomputed": right}))
                return pixel
        return None

    def _recompute_deviation(self, stats: Dict[str, Any], truth: Dict[str, Any]) -> Dict[str, float]:
        area_rel = abs(stats["area_pixels"] - truth["mask_pixel_count"]) / truth["mask_pixel_count"]
        tx, ty = truth["centroid_xy"]
        mx, my = stats["centroid_xy"]
        tx0, ty0, tx1, ty1 = truth["bbox_xyxy"]
        mx0, my0, mx1, my1 = stats["bbox_xyxy"]
        return {
            "area_relative_error": float(area_rel),
            "area_truth": int(truth["mask_pixel_count"]),
            "area_measured": int(stats["area_pixels"]),
            "centroid_euclidean_error": float(np.hypot(mx - tx, my - ty)),
            "centroid_error_xy": [float(mx - tx), float(my - ty)],
            "bbox_error_xyxy": [mx0 - tx0, my0 - ty0, mx1 - tx1, my1 - ty1],
            "within_area_tolerance": bool(area_rel <= 0.10),
        }

    def _compare_deviation(self, block_id: str, recomputed: Dict[str, Any],
                           recorded: Dict[str, Any], mismatches: List[Mismatch]) -> None:
        for key, value in recomputed.items():
            recorded_value = recorded.get(key)
            if isinstance(value, float):
                if not np.isclose(value, recorded_value, atol=1e-9):
                    mismatches.append(Mismatch(f"deviation.{key}", block_id, "stats", None,
                                               {"recorded": recorded_value, "recomputed": value}))
            elif value != recorded_value:
                mismatches.append(Mismatch(f"deviation.{key}", block_id, "stats", None,
                                           {"recorded": recorded_value, "recomputed": value}))


def run_verification(checkpoint_root: str, data_dir: str, sample_id: str) -> Dict[str, Any]:
    return IndependentVerifier(checkpoint_root, data_dir, sample_id).verify()
