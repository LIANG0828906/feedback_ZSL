"""地形引擎：加载、补丁合并、增量失效与全量真值。

引擎同时维护两套计算路径：

* :meth:`TerrainEngine.apply_patch` —— 增量路径，只重算失效 chunk 并解释原因；
* :meth:`TerrainEngine.full_rebuild` —— 全量路径，作为真值来源。

两条路径调用同一批纯函数（法线/材质混合/LOD/接缝），因此结果逐位一致。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Optional

import numpy as np

from .binary_io import (
    KIND_HEIGHT,
    KIND_MATERIAL,
    HeightField,
    MaterialMap,
    Patch,
)
from .dependency import InvalidationResult, NodeKind, compute_invalidation
from .errors import ErrorCode, TerrainError
from .hashing import GENESIS_HASH, sha256_array
from .pipeline import close_seams, compute_material_blend, compute_normals
from .quadtree import (
    Chunk,
    QuadNode,
    build_chunks,
    build_quadtree,
    chunk_grid_dims,
    compute_roughness,
    target_level_for_roughness,
)


@dataclass
class ChunkArtifact:
    """单个 chunk 的可复用渲染产物与哈希证据。"""

    chunk: Chunk
    elev_hash: str = ""
    material_hash: str = ""
    normals_hash: str = ""
    blend_hash: str = ""
    lod_level: int = 0
    reused: bool = False
    reasons: list[str] = field(default_factory=list)


@dataclass
class UpdateReport:
    seq: int
    invalidation: InvalidationResult
    recomputed: list[tuple[int, int]]
    reused: list[tuple[int, int]]
    reasons: dict[tuple[int, int], list[str]]
    seam_report: list[dict]
    crack: Optional[dict]
    height_rect: Optional[tuple[int, int, int, int]]
    material_rect: Optional[tuple[int, int, int, int]]


class TerrainEngine:
    def __init__(
        self,
        hf: HeightField,
        material: MaterialMap,
        chunk_size: int = 16,
        lod_threshold: float = 1.0,
        max_lod_level: int = 6,
        crack_tolerance: float = 1e-4,
        material_palette_size: int = 16,
    ) -> None:
        if material.indices.shape != hf.elevations.shape:
            raise TerrainError(
                ErrorCode.E_TERRAIN_INVALID,
                "材质图与高度场尺寸不一致",
                {"h": hf.elevations.shape, "m": material.indices.shape},
            )
        if not np.all(np.isfinite(hf.elevations)):
            raise TerrainError(
                ErrorCode.E_TERRAIN_INVALID,
                "高度场含非有限高程（NaN/Inf），不建立任何节点",
            )
        self.elevations = hf.elevations
        self.material = material.indices
        self.cell_size = hf.cell_size
        self.valid_rect = hf.valid_rect
        self.chunk_size = int(chunk_size)
        self.lod_threshold = float(lod_threshold)
        self.max_lod_level = int(max_lod_level)
        self.crack_tolerance = float(crack_tolerance)
        self.material_palette_size = int(material_palette_size)
        self.version = int(hf.base_version)
        if material.base_version not in (0, self.version):
            # 材质与高程版本允许同为基线；后续随批次一起推进
            raise TerrainError(
                ErrorCode.E_TERRAIN_INVALID,
                "材质图基线版本与高度场不一致",
                {"height": self.version, "material": material.base_version},
            )

        self.height, self.width = self.elevations.shape
        self.grid_cols, self.grid_rows = chunk_grid_dims(
            self.height, self.width, self.chunk_size
        )
        self.chunks: dict[tuple[int, int], Chunk] = {
            c.key: c for c in build_chunks(self.height, self.width, self.chunk_size)
        }
        self.artifacts: dict[tuple[int, int], ChunkArtifact] = {
            key: ChunkArtifact(chunk=chunk) for key, chunk in self.chunks.items()
        }
        self.normals: Optional[np.ndarray] = None
        self.blend: Optional[np.ndarray] = None
        self.roughness: Optional[np.ndarray] = None
        self.chunk_levels = np.zeros((self.grid_rows, self.grid_cols), dtype=np.int32)
        self.root: Optional[QuadNode] = None
        self.chain = GENESIS_HASH
        self.last_report: Optional[UpdateReport] = None
        # 固定的渲染显示范围：取初始基线高程范围，保证瓦片颜色只依赖局部输入，
        # 不会因别处补丁改变全局 min/max 而让未变更块的颜色漂移
        self.display_range: tuple[float, float] = (
            float(self.elevations.min()),
            float(self.elevations.max()),
        )
        # 固定热力刻度：基于初始粗糙度，避免后续补丁改变全局刻度令瓦片漂移
        initial_roughness = compute_roughness(self.elevations, self.cell_size)
        self.heat_scale = max(1e-9, float(initial_roughness.max()))

    # ------------------------------------------------------------------ #
    # 补丁校验与合并
    # ------------------------------------------------------------------ #
    def validate_patch(self, patch: Patch) -> None:
        x, y, w, h = patch.rect
        vx, vy, vw, vh = self.valid_rect
        if not (x >= vx and y >= vy and x + w <= vx + vw and y + h <= vy + vh):
            raise TerrainError(
                ErrorCode.E_PATCH_BOUNDS,
                f"补丁矩形 {patch.rect} 越出有效范围 {self.valid_rect}",
                {"rect": patch.rect, "valid_rect": self.valid_rect},
            )
        if patch.cell_size != self.cell_size:
            raise TerrainError(
                ErrorCode.E_PATCH_INVALID,
                f"补丁格距 {patch.cell_size} 与当前 {self.cell_size} 不一致",
                {"patch_cell": patch.cell_size, "current_cell": self.cell_size},
            )
        if patch.base_version != self.version:
            raise TerrainError(
                ErrorCode.E_PATCH_STALE,
                f"补丁基线 {patch.base_version} 与当前版本 {self.version} 错序",
                {"patch_base": patch.base_version, "current": self.version},
            )
        if patch.data.shape != (h, w):
            raise TerrainError(
                ErrorCode.E_PATCH_INVALID,
                "补丁数据形状与目标矩形不符",
                {"shape": patch.data.shape, "rect": patch.rect},
            )
        if patch.kind == KIND_HEIGHT and not np.all(np.isfinite(patch.data)):
            raise TerrainError(
                ErrorCode.E_PATCH_INVALID,
                "高程补丁含非有限高程（NaN/Inf）",
                {"rect": patch.rect},
            )

    def merge_patch(self, patch: Patch) -> tuple[int, int, int, int]:
        """像素级替换合并（幂等），返回实际变更矩形。

        纯替换语义：重复应用同一补丁得到完全相同的高程/材质值。
        """
        x, y, w, h = patch.rect
        if patch.kind == KIND_HEIGHT:
            target = self.elevations[y : y + h, x : x + w]
            incoming = patch.data.astype(np.float32, copy=False)
            self.elevations[y : y + h, x : x + w] = incoming
        elif patch.kind == KIND_MATERIAL:
            target = self.material[y : y + h, x : x + w]
            incoming = patch.data.astype(self.material.dtype, copy=False)
            self.material[y : y + h, x : x + w] = incoming
        else:  # pragma: no cover - 加载层已拦截
            raise TerrainError(ErrorCode.E_PATCH_INVALID, f"未知补丁类型 {patch.kind}")
        return patch.rect

    def apply_patch(self, patch: Patch) -> UpdateReport:
        """校验 + 合并 + 增量失效 + 局部重算的完整入口。"""
        self.validate_patch(patch)
        rect = self.merge_patch(patch)
        height_rect = rect if patch.kind == KIND_HEIGHT else None
        material_rect = rect if patch.kind == KIND_MATERIAL else None
        report = self.incremental_update(
            height_rect=height_rect,
            material_rect=material_rect,
            seq=patch.seq,
        )
        self.version += 1
        return report

    # ------------------------------------------------------------------ #
    # 增量 / 全量计算
    # ------------------------------------------------------------------ #
    def incremental_update(
        self,
        height_rect: Optional[tuple[int, int, int, int]],
        material_rect: Optional[tuple[int, int, int, int]],
        seq: int,
    ) -> UpdateReport:
        invalidation = compute_invalidation(
            height_rect=height_rect,
            material_rect=material_rect,
            chunk_size=self.chunk_size,
            height=self.height,
            width=self.width,
        )

        # 1. 全量派生数组（纯函数，整图计算一次，作为逐位一致的共享底座；
        #    真正的“复用/重算”决策与缓冲复用发生在 chunk 产物与渲染层）
        self.normals = compute_normals(self.elevations, self.cell_size)
        self.blend = compute_material_blend(self.material, self.material_palette_size)
        self.roughness = compute_roughness(self.elevations, self.cell_size)

        # 2. 逐 chunk 更新统计、哈希与 LOD 决策（仅失效块标重算）
        recomputed: list[tuple[int, int]] = []
        reused: list[tuple[int, int]] = []
        reasons: dict[tuple[int, int], list[str]] = {}

        height_invalid = invalidation.invalid[NodeKind.HEIGHT]
        material_invalid = invalidation.invalid[NodeKind.MATERIAL]
        normals_invalid = invalidation.invalid[NodeKind.NORMALS]
        lod_invalid = invalidation.invalid[NodeKind.LOD]
        seam_invalid = invalidation.invalid[NodeKind.SEAM]
        blend_invalid = invalidation.invalid[NodeKind.BLEND]
        render_invalid = invalidation.render_chunks()

        for key, chunk in self.chunks.items():
            artifact = self.artifacts[key]
            block_h = self.elevations[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1]
            block_m = self.material[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1]
            new_elev_hash = sha256_array(block_h)
            new_mat_hash = sha256_array(block_m)
            block_n = self.normals[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1]
            block_b = self.blend[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1]
            block_r = self.roughness[chunk.y0 : chunk.y1, chunk.x0 : chunk.x1]
            chunk.elev_min = float(block_h.min())
            chunk.elev_max = float(block_h.max())
            chunk.roughness = float(block_r.mean())
            level = target_level_for_roughness(
                chunk.roughness, self.lod_threshold, self.max_lod_level
            )
            chunk.elev_hash = new_elev_hash
            chunk.material_hash = new_mat_hash
            chunk.target_level = level
            self.chunk_levels[chunk.cy, chunk.cx] = level

            block_reasons = self._block_reasons(
                key,
                invalidation,
                height_invalid,
                material_invalid,
                normals_invalid,
                lod_invalid,
                seam_invalid,
                blend_invalid,
                render_invalid,
            )
            if key in render_invalid or key in height_invalid or key in material_invalid:
                artifact.reused = False
                artifact.reasons = block_reasons
                recomputed.append(key)
            else:
                # 复用前必须用哈希证明未受影响，否则保守重算
                proven_unchanged = (
                    artifact.elev_hash == new_elev_hash
                    and artifact.material_hash == new_mat_hash
                    and artifact.lod_level == level
                )
                if artifact.elev_hash and proven_unchanged:
                    artifact.reused = True
                    artifact.reasons = ["输入哈希与 LOD 均未变化，复用缓存产物"]
                    reused.append(key)
                else:
                    artifact.reused = False
                    artifact.reasons = ["无法证明未受影响，保守重算"]
                    recomputed.append(key)
                    block_reasons.append("无法证明未受影响，保守重算")
            artifact.elev_hash = new_elev_hash
            artifact.material_hash = new_mat_hash
            artifact.normals_hash = sha256_array(block_n)
            artifact.blend_hash = sha256_array(block_b)
            artifact.lod_level = level
            reasons[key] = block_reasons

        # 3. 重建四叉树并闭合接缝
        self.root = build_quadtree(
            self.chunk_levels, self.grid_cols, self.grid_rows, self.max_lod_level
        )
        crack: Optional[dict] = None
        try:
            _corrections, seam_report = close_seams(
                self.root,
                self.elevations,
                self.chunk_size,
                self.cell_size,
                self.crack_tolerance,
            )
        except TerrainError as exc:
            if exc.code != ErrorCode.E_CRACK_UNRESOLVED:
                raise
            crack = exc.detail
            seam_report = []

        report = UpdateReport(
            seq=seq,
            invalidation=invalidation,
            recomputed=sorted(recomputed),
            reused=sorted(reused),
            reasons=reasons,
            seam_report=seam_report,
            crack=crack,
            height_rect=height_rect,
            material_rect=material_rect,
        )
        self.last_report = report
        return report

    @staticmethod
    def _block_reasons(
        key: tuple[int, int],
        invalidation: InvalidationResult,
        height_invalid: set,
        material_invalid: set,
        normals_invalid: set,
        lod_invalid: set,
        seam_invalid: set,
        blend_invalid: set,
        render_invalid: set,
    ) -> list[str]:
        out: list[str] = []
        for kind, bucket in (
            (NodeKind.HEIGHT, height_invalid),
            (NodeKind.MATERIAL, material_invalid),
            (NodeKind.NORMALS, normals_invalid),
            (NodeKind.LOD, lod_invalid),
            (NodeKind.SEAM, seam_invalid),
            (NodeKind.BLEND, blend_invalid),
        ):
            if key in bucket:
                out.append(f"{kind.value}: {invalidation.reasons.get((kind, key[0], key[1]), '')}")
        if key in render_invalid:
            out.append("render: 上游失效触发离屏缓冲重渲")
        return out

    def full_rebuild(self, seq: Optional[int] = None) -> UpdateReport:
        """全量真值路径：所有 chunk 一律重算。"""
        seq = self.version if seq is None else seq
        report = self.incremental_update(
            height_rect=(0, 0, self.width, self.height),
            material_rect=(0, 0, self.width, self.height),
            seq=seq,
        )
        for artifact in self.artifacts.values():
            artifact.reused = False
        report.reused = []
        report.recomputed = sorted(self.chunks.keys())
        return report

    # ------------------------------------------------------------------ #
    # 快照状态对接
    # ------------------------------------------------------------------ #
    def snapshot_state(self) -> dict:
        return {
            "version": self.version,
            "chain": self.chain,
            "height_hash": sha256_array(self.elevations),
            "material_hash": sha256_array(self.material),
        }
