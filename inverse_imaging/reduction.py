"""Parallel image-chunk gradients with deterministic coordinate-order reduction."""

from __future__ import annotations

import math
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from typing import Iterable, Sequence

import numpy as np

from .errors import E_REDUCE_DIVERGE, AppError
from .params import GROUPS, ParameterSet, canonical_json_bytes
from .renderer import (
    ChunkGradient,
    PixelChunk,
    gradient_chunk,
    gradient_content_hash,
    make_chunks,
)
from .scene import Scene


@dataclass(frozen=True)
class ReductionTolerance:
    atol: float = 1.0e-12
    rtol: float = 1.0e-10


@dataclass
class GradientResult:
    loss: float
    gradients: dict[str, np.ndarray]
    reduction_order: tuple[tuple[int, int, int, int], ...]
    chunk_hashes: tuple[str, ...]
    gradients_sha256: str
    chunk_size: int | tuple[int, int]
    parallel: int
    chunk_results: list[ChunkGradient] = field(repr=False)
    affected_nodes: tuple[str, ...] = ()
    reused_nodes: tuple[str, ...] = ()
    recomputed_nodes: tuple[str, ...] = ()
    degraded: bool = False
    downgrade_reason: str | None = None
    verification: dict[str, object] = field(default_factory=dict)

    def gradient_norms(self) -> dict[str, float]:
        return {
            group: float(np.linalg.norm(self.gradients[group])) for group in GROUPS
        }

    def gradient_bytes(self) -> bytes:
        payload = {
            group: self.gradients[group].tolist() for group in GROUPS
        }
        return canonical_json_bytes(payload)

    def to_publishable(self) -> dict[str, object]:
        return {
            "loss": self.loss,
            "gradients": {group: self.gradients[group].tolist() for group in GROUPS},
            "gradient_norms": self.gradient_norms(),
            "gradients_sha256": self.gradients_sha256,
            "reduction_order": [list(item) for item in self.reduction_order],
            "chunk_hashes": list(self.chunk_hashes),
            "chunk_size": (
                list(self.chunk_size)
                if isinstance(self.chunk_size, tuple)
                else self.chunk_size
            ),
            "parallel": self.parallel,
            "affected_nodes": list(self.affected_nodes),
            "reused_nodes": list(self.reused_nodes),
            "recomputed_nodes": list(self.recomputed_nodes),
            "degraded": self.degraded,
            "downgrade_reason": self.downgrade_reason,
            "verification": self.verification,
        }


def compute_chunk_gradient(
    params: ParameterSet,
    scene: Scene,
    chunk: PixelChunk,
) -> ChunkGradient:
    return gradient_chunk(params, scene, chunk)


def compute_gradients(
    params: ParameterSet,
    scene: Scene,
    chunk_size: int | tuple[int, int] = 16,
    parallel: int = 1,
    *,
    reference: GradientResult | None = None,
    tolerance: ReductionTolerance | None = None,
) -> GradientResult:
    if parallel < 1:
        raise ValueError("parallel must be at least one")
    chunks = make_chunks(scene.width, scene.height, chunk_size)
    if parallel == 1 or len(chunks) == 1:
        results = [compute_chunk_gradient(params, scene, chunk) for chunk in chunks]
    else:
        ordered: list[ChunkGradient | None] = [None] * len(chunks)
        with ThreadPoolExecutor(max_workers=min(parallel, len(chunks))) as executor:
            futures = {
                executor.submit(compute_chunk_gradient, params, scene, chunk): chunk
                for chunk in chunks
            }
            for future, chunk in futures.items():
                ordered[chunk.index] = future.result()
        results = [result for result in ordered if result is not None]
    if len(results) != len(chunks):
        raise RuntimeError("chunk workers did not return every chunk")
    return reduce_chunk_results(
        results,
        chunk_size=chunk_size,
        parallel=parallel,
        reference=reference,
        tolerance=tolerance,
    )


def reduce_chunk_results(
    results: Sequence[ChunkGradient],
    *,
    chunk_size: int | tuple[int, int],
    parallel: int,
    reference: GradientResult | None = None,
    tolerance: ReductionTolerance | None = None,
) -> GradientResult:
    tol = tolerance or ReductionTolerance()
    ordered = sorted(results, key=lambda result: result.chunk.key)
    for expected_index, result in enumerate(ordered):
        if result.chunk.index != expected_index:
            raise ValueError("chunk result indices are not contiguous")
    loss = math.fsum(result.loss for result in ordered)
    gradients: dict[str, np.ndarray] = {}
    for group in GROUPS:
        length = len(ordered[0].gradients[group]) if ordered else 0
        accumulator = np.zeros(length, dtype=np.float64)
        for result in ordered:
            accumulator += result.gradients[group]
        gradients[group] = accumulator
    digest = gradient_content_hash(gradients)
    reduced = GradientResult(
        loss=loss,
        gradients=gradients,
        reduction_order=tuple(result.chunk.key for result in ordered),
        chunk_hashes=tuple(result.gradients_sha256 for result in ordered),
        gradients_sha256=digest,
        chunk_size=chunk_size,
        parallel=parallel,
        chunk_results=list(ordered),
        affected_nodes=tuple(
            dict.fromkeys(node for result in ordered for node in result.affected_nodes)
        ),
        reused_nodes=tuple(
            dict.fromkeys(node for result in ordered for node in result.reused_nodes)
        ),
        recomputed_nodes=tuple(
            dict.fromkeys(node for result in ordered for node in result.recomputed_nodes)
        ),
    )
    if reference is not None:
        _check_reference(reduced, reference, tol)
    return reduced


def _check_reference(
    actual: GradientResult,
    reference: GradientResult,
    tolerance: ReductionTolerance,
) -> None:
    if not math.isclose(
        actual.loss,
        reference.loss,
        rel_tol=tolerance.rtol,
        abs_tol=tolerance.atol,
    ):
        raise AppError(
            E_REDUCE_DIVERGE,
            "reduced loss differs from reference",
            "loss",
            {"actual": actual.loss, "reference": reference.loss},
        )
    if len(actual.chunk_results) != len(reference.chunk_results):
        raise AppError(
            E_REDUCE_DIVERGE,
            "chunk layouts differ",
            "chunk_layout",
            {
                "actual_chunks": len(actual.chunk_results),
                "reference_chunks": len(reference.chunk_results),
            },
        )
    for actual_chunk, reference_chunk in zip(
        actual.chunk_results, reference.chunk_results, strict=True
    ):
        if actual_chunk.chunk.key != reference_chunk.chunk.key:
            raise AppError(
                E_REDUCE_DIVERGE,
                "chunk coordinate order differs",
                "chunk_layout",
                {
                    "chunk_index": actual_chunk.chunk.index,
                    "actual": list(actual_chunk.chunk.key),
                    "reference": list(reference_chunk.chunk.key),
                },
            )
        for group in GROUPS:
            left = actual_chunk.gradients[group]
            right = reference_chunk.gradients[group]
            if not np.allclose(
                left, right, rtol=tolerance.rtol, atol=tolerance.atol
            ):
                location = _first_difference(left, right, tolerance)
                raise AppError(
                    E_REDUCE_DIVERGE,
                    "chunk gradient differs from reference",
                    f"{group}[{location}]",
                    {
                        "chunk_index": actual_chunk.chunk.index,
                        "chunk_coordinate": list(actual_chunk.chunk.key),
                        "group": group,
                    },
                )
    for group in GROUPS:
        left = actual.gradients[group]
        right = reference.gradients[group]
        if not np.allclose(left, right, rtol=tolerance.rtol, atol=tolerance.atol):
            location = _first_difference(left, right, tolerance)
            raise AppError(
                E_REDUCE_DIVERGE,
                "reduced gradient differs from reference",
                f"{group}[{location}]",
                {"group": group, "chunk_index": 0},
            )
def _first_difference(left: np.ndarray, right: np.ndarray, tolerance: ReductionTolerance) -> int:
    absolute = np.abs(left - right)
    threshold = tolerance.atol + tolerance.rtol * np.abs(right)
    indices = np.nonzero(absolute > threshold)[0]
    return int(indices[0]) if len(indices) else 0


def results_equivalent(
    left: GradientResult,
    right: GradientResult,
    tolerance: ReductionTolerance | None = None,
) -> tuple[bool, str | None]:
    tol = tolerance or ReductionTolerance()
    if not math.isclose(left.loss, right.loss, rel_tol=tol.rtol, abs_tol=tol.atol):
        return False, "loss"
    for group in GROUPS:
        if not np.allclose(
            left.gradients[group], right.gradients[group], rtol=tol.rtol, atol=tol.atol
        ):
            return False, group
    return True, None


def gradient_byte_identity(left: GradientResult, right: GradientResult) -> bool:
    return left.gradient_bytes() == right.gradient_bytes()

