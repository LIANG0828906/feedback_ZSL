"""Reviewable quality-report export for completed pipeline snapshots."""

from __future__ import annotations

import hashlib
import json
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import numpy as np

from .errors import WorkbenchError
from .metrics import heatmap_png_array
from .pipeline import PipelineResult
from .pngio import write_png
from .util import array_content_hash, atomic_write_bytes

REPORT_VERSION = "imq-quality-report-v1"


def _json_safe(value: Any) -> Any:
    if isinstance(value, dict):
        return {str(key): _json_safe(item) for key, item in value.items()}
    if isinstance(value, (list, tuple)):
        return [_json_safe(item) for item in value]
    if isinstance(value, np.generic):
        return value.item()
    if isinstance(value, np.ndarray):
        return value.tolist()
    if isinstance(value, float) and not np.isfinite(value):
        return None
    return value


def export_quality_report(
    result: PipelineResult,
    output_dir: str | Path,
    *,
    sample_metadata: dict[str, Any] | None = None,
) -> dict[str, Any]:
    if not result.complete or result.metrics is None:
        raise WorkbenchError(
            "E_STATE",
            "a complete quality report cannot be produced before every block is committed",
            {
                "committed": result.progress["committed"],
                "total": result.progress["total"],
                "failed_blocks": result.failed_blocks,
                "snapshot_id": result.snapshot_id,
            },
        )

    root = Path(output_dir)
    root.mkdir(parents=True, exist_ok=True)
    heatmap_npy = root / "heatmap.npy"
    np.save(heatmap_npy, result.heatmap.astype(np.float32), allow_pickle=False)
    heatmap_png = root / "heatmap.png"
    write_png(heatmap_png, heatmap_png_array(result.heatmap))
    reconstruction_png = root / "reconstruction.png"
    write_png(reconstruction_png, result.reconstructed)
    ssim_map_npy = root / "ssim_map.npy"
    if result.ssim_map is not None:
        np.save(ssim_map_npy, result.ssim_map.astype(np.float32), allow_pickle=False)
    edge_map_npy = root / "edge_fidelity_map.npy"
    if result.edge_map is not None:
        np.save(edge_map_npy, result.edge_map.astype(np.float32), allow_pickle=False)

    blocks = {str(index): result.block_summaries[index] for index in sorted(result.block_summaries)}
    report = {
        "report_version": REPORT_VERSION,
        "generated_utc": datetime.now(timezone.utc).isoformat(),
        "status": "complete",
        "algorithm_version": result.algorithm_version,
        "mode": result.mode,
        "snapshot_id": result.snapshot_id,
        "image": {
            "key": result.image_key,
            "shape": list(result.image.shape),
            "dtype": str(result.image.dtype),
            "content_hash": result.image_hash,
            "baseline_export": "reconstruction.png is the decoded reconstruction; baseline remains in the sample manifest",
        },
        "sample_truth": sample_metadata or {},
        "config": result.config.to_dict(),
        "effective_parameters": {
            "quant_steps": result.metrics.get("quant_steps", [result.config.quant_step]),
            "evaluation_windows": result.metrics.get("evaluation_windows", [result.config.evaluation_window]),
            "frozen_parameters_mixed": result.metrics.get("frozen_parameters_mixed", False),
            "rule": "committed block parameters are frozen; only uncommitted blocks use the latest request",
        },
        "progress": result.progress,
        "complete": True,
        "metrics": result.metrics,
        "commit_order": result.commit_order,
        "block_states": result.block_states,
        "state_counts": {
            state: result.block_states.count(state)
            for state in sorted(set(result.block_states))
        },
        "error_propagation": result.error_propagation,
        "blocks": blocks,
        "artifacts": {
            "heatmap_npy": heatmap_npy.name,
            "heatmap_png": heatmap_png.name,
            "reconstruction_png": reconstruction_png.name,
            "ssim_map_npy": ssim_map_npy.name if result.ssim_map is not None else None,
            "edge_fidelity_map_npy": edge_map_npy.name if result.edge_map is not None else None,
        },
        "artifact_hashes": {
            "heatmap_npy": array_content_hash(result.heatmap),
            "heatmap_png_sha256": hashlib.sha256(heatmap_png.read_bytes()).hexdigest(),
            "reconstruction_png_sha256": hashlib.sha256(reconstruction_png.read_bytes()).hexdigest(),
        },
        "review_notes": [
            "Metrics are composed from exclusive block crops; each crop uses an evaluation-window-linked halo.",
            "Committed block parameters are frozen in SQLite checkpoints.",
            "An incomplete or interrupted run cannot export this report.",
        ],
    }
    safe_report = _json_safe(report)
    report_path = root / "quality_report.json"
    atomic_write_bytes(report_path, json.dumps(safe_report, ensure_ascii=False, indent=2, sort_keys=True, allow_nan=False).encode("utf-8") + b"\n")
    return {
        "status": "ok",
        "report_path": str(report_path.resolve()),
        "heatmap_npy": str(heatmap_npy.resolve()),
        "heatmap_png": str(heatmap_png.resolve()),
        "reconstruction_png": str(reconstruction_png.resolve()),
        "report_sha256": hashlib.sha256(report_path.read_bytes()).hexdigest(),
        "metrics": result.metrics,
    }
