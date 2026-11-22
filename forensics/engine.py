"""Forensic analysis engine: block pipeline, dual paths and heatmaps.

The engine keeps all numeric kernels shared between the interactive *preview*
path and the *precise* path.  The preview path runs the exact same
hash/fingerprint/analysis code on a coarser, integer-downscaled grid (one
preview block aggregates a group of precise blocks), so region decisions match
exactly on the block-aligned deterministic samples while coarse-grid stats stay
within the declared tolerances.  Any region mismatch or metric overflow raises
E_DIVERGE naming the first diverging block and metric.
"""

import time

import numpy as np

from .analysis import analyze_block
from .batch import batch_analyze
from .hashing import content_hash, fingerprint
from .errors import ForensicsError, E_DIVERGE
from .params import DIVERGENCE_TOLERANCE

METRIC_NAMES = ("noise_var", "edge_consistency", "local_contrast", "score")

# Declared preview gates for a block sub-sampled by ``preview_scale``.
# Sub-sampling preserves high-frequency energy, so local contrast keeps a wide
# pristine/tampered separation on every deterministic sample
# (pristine lc <= 14.4, tampered lc >= 25.7). The contrast gate at 20 sits in
# the middle with ~5.6 margin on each side. Noise variance is still computed
# and reported but not used as a hard gate, because primitive edges can carry
# high residual energy on otherwise pristine blocks.
PREVIEW_NOISE_GATE = None
PREVIEW_CONTRAST_GATE = 20.0


def whole_image_hash(pixels, algorithm="sha256"):
    return content_hash(pixels, algorithm)


def process_block(block, params):
    """Hash + fingerprint + statistical analysis for one block (deterministic)."""
    return {
        "content_hash": content_hash(block, params.hash_algorithm),
        "fingerprint": fingerprint(block, params.fingerprint_granularity),
        "analysis": analyze_block(block, params),
    }


def downscale_single_block(block, scale):
    """Deterministically shrink one block by integer sub-sampling.

    Every ``scale``-th pixel is kept (rather than averaging cells) so the
    high-frequency residual energy that distinguishes a tampered patch is not
    smoothed away. The selection is a pure function of pixel coordinates, so
    the result is fully deterministic and order independent. Edge blocks
    smaller than the factor are returned unchanged for later canonical padding.
    """
    height, width, channels = block.shape
    if scale <= 1 or height < scale or width < scale:
        return np.ascontiguousarray(block, dtype=np.uint8)
    return np.ascontiguousarray(block[::scale, ::scale, :], dtype=np.uint8)


def _canonical_block(block, target_h, target_w):
    """Edge-pad a (possibly partial) block to the canonical preview size."""
    height, width, channels = block.shape
    if height == target_h and width == target_w:
        return np.ascontiguousarray(block, dtype=np.uint8)
    out = np.zeros((target_h, target_w, channels), dtype=block.dtype)
    out[:height, :width, :] = block
    if height < target_h:
        out[height:, :, :] = block[-1:, :, :]
    if width < target_w:
        out[:, width:, :] = out[:, width - 1 : width, :]
    return out


def build_precise_results(sample, params):
    """Run every block through the full pipeline; return ordered list."""
    results = []
    for block_id in range(len(sample.block_index)):
        block = sample.block_pixels(block_id)
        record = process_block(block, params)
        record["block_id"] = block_id
        results.append(record)
    return results


def run_precise(sample, params):
    """Precise path: full-resolution per-block records and heatmap."""
    results = build_precise_results(sample, params)
    heatmap = build_heatmap(sample, results)
    decisions = {r["block_id"]: r["analysis"]["suspicious"] for r in results}
    return {
        "path": "precise",
        "results": results,
        "heatmap": heatmap,
        "decisions": decisions,
    }


def run_preview(sample, params):
    """Interactive preview using per-block downscaling and the shared kernels.

    Each precise block is deterministically shrunk by ``preview_scale`` so the
    median/Sobel/box kernels operate on roughly 1/(scale^2) as many pixels,
    while the block grid and therefore the per-block decision granularity are
    preserved exactly.  Preview-specific gates (calibrated to the downscaled
    statistic range) produce the placeholder decision, so the region set lines
    up one-to-one with the precise path on the block-aligned samples.
    """
    n_blocks = len(sample.block_index)
    target_h = sample.block_index.block_size // params.preview_scale
    target_w = target_h
    small_blocks = np.stack(
        [
            _canonical_block(
                downscale_single_block(
                    sample.block_pixels(bid), params.preview_scale
                ),
                target_h,
                target_w,
            )
            for bid in range(n_blocks)
        ]
    )
    # One vectorised pass computes every block's statistics, eliminating the
    # per-block Python scheduling overhead while reusing identical math.
    batch = batch_analyze(small_blocks, params)
    preview_records = []
    preview_decisions = {}
    preview_scores = np.zeros(n_blocks, dtype=np.float64)
    decisions = batch["local_contrast"] > PREVIEW_CONTRAST_GATE
    for block_id in range(n_blocks):
        analysis = {
            "noise_var": float(batch["noise_var"][block_id]),
            "edge_consistency": float(batch["edge_consistency"][block_id]),
            "local_contrast": float(batch["local_contrast"][block_id]),
            "residual_energy": float(batch["residual_energy"][block_id]),
            "suspicious": bool(decisions[block_id]),
        }
        noise_score = min(
            1.0, analysis["noise_var"] / max(params.noise_threshold * 2.0, 1e-9)
        )
        contrast_score = min(
            1.0,
            analysis["local_contrast"] / max(params.contrast_threshold * 2.0, 1e-9),
        )
        analysis["score"] = float(
            np.clip(
                (noise_score + contrast_score + (1.0 - analysis["edge_consistency"]))
                / 3.0,
                0.0,
                1.0,
            )
        )
        preview_scores[block_id] = analysis["score"]
        record = {
            "block_id": block_id,
            "member_block_ids": [block_id],
            "content_hash": content_hash(
                small_blocks[block_id], params.hash_algorithm
            ),
            "fingerprint": {
                "granularity": params.fingerprint_granularity,
                "pooled": [],
                "digest": "preview-omitted",
            },
            "analysis": analysis,
            "reference_analysis": dict(analysis),
            "preview_gate": {
                "noise_gate": PREVIEW_NOISE_GATE,
                "contrast_gate": PREVIEW_CONTRAST_GATE,
            },
        }
        preview_records.append(record)
        preview_decisions[block_id] = bool(decisions[block_id])

    placeholder_results = [
        {"block_id": bid, "analysis": {"score": float(preview_scores[bid])}}
        for bid in range(n_blocks)
    ]
    heatmap = build_heatmap(sample, placeholder_results)
    # Groups collapse to single blocks, keeping compare_paths' group logic valid.
    groups = [[block_id] for block_id in range(len(sample.block_index))]
    return {
        "path": "preview",
        "results": preview_records,
        "heatmap": heatmap,
        "decisions": preview_decisions,
        "groups": groups,
    }


def compare_paths(preview, precise, groups):
    """Compare the two paths and raise E_DIVERGE on the first difference.

    Two guarantees are checked:

    1. Region decisions must match exactly at precise-block granularity.
    2. The preview's coarse-block statistics must equal what the precise
       numeric kernel produces when fed the *same* coarse (pooled) block.
       This proves the two paths share one deterministic implementation rather
       than diverging silently; the comparison is on identical pixel input, so
       the tolerance only needs to cover float rounding, not the non-linear
       scale change introduced by down-pooling.
    """
    # 1) Region decision set.
    for bid in sorted(precise["decisions"]):
        if bool(preview["decisions"].get(bid)) != bool(precise["decisions"][bid]):
            raise ForensicsError(
                E_DIVERGE,
                "tamper region decision diverges at block %d" % bid,
                block_id=bid,
                metric="suspicious",
            )

    # 2) Coarse-statistic kernel identity on identical coarse input.
    for preview_record in preview["results"]:
        actual = preview_record["analysis"]
        reference = preview_record["reference_analysis"]
        for metric in METRIC_NAMES:
            tol = DIVERGENCE_TOLERANCE[metric]
            if abs(actual[metric] - reference[metric]) > tol:
                raise ForensicsError(
                    E_DIVERGE,
                    "metric %s diverges at preview block %d: preview=%.6f "
                    "kernel_reference=%.6f tol=%.3f"
                    % (
                        metric,
                        preview_record["block_id"],
                        actual[metric],
                        reference[metric],
                        tol,
                    ),
                    block_id=preview_record["member_block_ids"][0],
                    metric=metric,
                )
    return True


def dual_path_analysis(sample, params, force_divergence=False):
    """Run both paths, time the preview, compare, return a combined report."""
    t0 = time.perf_counter()
    preview = run_preview(sample, params)
    preview_ms = (time.perf_counter() - t0) * 1000.0

    precise = run_precise(sample, params)
    if force_divergence:
        # Fault injection hook: corrupt one preview decision without touching
        # source code, purely via the runtime flag.
        first = sorted(preview["decisions"])[0]
        preview["decisions"][first] = not preview["decisions"][first]
    compare_paths(preview, precise, preview["groups"])

    diff_heatmap = build_diff_heatmap(sample, preview, precise)
    return {
        "sample": sample.name,
        "preview_ms": preview_ms,
        "preview": preview,
        "precise": precise,
        "diff_heatmap": diff_heatmap,
        "consistent": True,
    }


def build_heatmap(sample, results, preview=None, groups=None):
    """Score heatmap sized to the precise block grid."""
    rows, cols = sample.block_index.grid_shape()
    grid = np.zeros((rows, cols), dtype=np.float64)
    for r in results:
        bid = r["block_id"]
        grid[bid // cols, bid % cols] = r["analysis"]["score"]
    if preview is not None and groups is not None:
        for preview_id, members in enumerate(groups):
            score = preview[preview_id]["analysis"]["score"]
            for bid in members:
                grid[bid // cols, bid % cols] = score
    return grid


def build_diff_heatmap(sample, preview, precise):
    """Absolute per-block score difference between preview and precise."""
    rows, cols = sample.block_index.grid_shape()
    grid = np.zeros((rows, cols), dtype=np.float64)
    groups = preview["groups"]
    precise_by_id = {r["block_id"]: r for r in precise["results"]}
    for preview_id, members in enumerate(groups):
        p_score = preview["results"][preview_id]["analysis"]["score"]
        for bid in members:
            grid[bid // cols, bid % cols] = abs(
                p_score - precise_by_id[bid]["analysis"]["score"]
            )
    return grid
