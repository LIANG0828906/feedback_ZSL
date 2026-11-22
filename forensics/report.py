"""Forensic reporting: ground-truth agreement, heatmap and JSON export."""

import json
import os

import numpy as np
from PIL import Image

from .params import DIVERGENCE_TOLERANCE


def confusion_against_truth(sample, decisions):
    """Compare per-block tamper decisions with the ground-truth mask."""
    truth = sample.ground_truth_mask()
    pred = np.array([bool(decisions.get(i)) for i in range(len(truth))])
    tp = int(np.sum(pred & truth))
    tn = int(np.sum(~pred & ~truth))
    fp = int(np.sum(pred & ~truth))
    fn = int(np.sum(~pred & truth))
    total = len(truth)
    agreement = (tp + tn) / total if total else 0.0
    return {
        "total": total,
        "true_positive": tp,
        "true_negative": tn,
        "false_positive": fp,
        "false_negative": fn,
        "block_agreement": agreement,
        "within_tolerance": fp == 0 and fn == 0,
        "tolerance_rule": "exact per-block match (declared tolerance 0 blocks)",
    }


def block_metrics_table(results):
    """Ordered per-block metric rows for export."""
    rows = []
    for record in sorted(results, key=lambda r: r["block_id"]):
        a = record["analysis"]
        rows.append(
            {
                "block_id": record["block_id"],
                "noise_var": a["noise_var"],
                "edge_consistency": a["edge_consistency"],
                "local_contrast": a["local_contrast"],
                "score": a["score"],
                "suspicious": a["suspicious"],
                "content_hash": record["content_hash"],
                "fingerprint_digest": record["fingerprint"]["digest"],
            }
        )
    return rows


def heatmap_to_rgb(grid):
    """Map a [0,1] score grid to an RGB uint8 image (blue -> red)."""
    g = np.clip(grid, 0.0, 1.0)
    rows, cols = g.shape
    rgb = np.zeros((rows, cols, 3), dtype=np.uint8)
    rgb[:, :, 0] = (255 * g).astype(np.uint8)
    rgb[:, :, 2] = (255 * (1.0 - g)).astype(np.uint8)
    rgb[:, :, 1] = (255 * (1.0 - np.abs(2 * g - 1))).astype(np.uint8)
    return rgb


def save_heatmap_png(grid, path, scale=32):
    rgb = heatmap_to_rgb(grid)
    image = Image.fromarray(rgb, mode="RGB").resize(
        (rgb.shape[1] * scale, rgb.shape[0] * scale), Image.NEAREST
    )
    image.save(path, format="PNG")
    return path


def build_report(sample, dual, snapshot, out_dir):
    """Assemble and persist the full forensic report for one sample."""
    os.makedirs(out_dir, exist_ok=True)
    precise = dual["precise"]
    confusion = confusion_against_truth(sample, precise["decisions"])
    report = {
        "sample": sample.name,
        "width": sample.width,
        "height": sample.height,
        "channels": sample.channels,
        "parameter_version": snapshot.version,
        "parameter_signature": snapshot.signature,
        "algorithm_version": snapshot.algorithm_version,
        "divergence_tolerance": DIVERGENCE_TOLERANCE,
        "preview_latency_ms": dual["preview_ms"],
        "dual_path_consistent": dual["consistent"],
        "ground_truth": confusion,
        "metrics": block_metrics_table(precise["results"]),
        "primitives": sample.manifest.get("primitives", []),
    }
    report_path = os.path.join(out_dir, sample.name + ".report.json")
    with open(report_path, "w", encoding="utf-8") as fh:
        json.dump(report, fh, indent=2)

    save_heatmap_png(
        precise["heatmap"], os.path.join(out_dir, sample.name + ".precise.png")
    )
    save_heatmap_png(
        dual["preview"]["heatmap"],
        os.path.join(out_dir, sample.name + ".preview.png"),
    )
    save_heatmap_png(
        dual["diff_heatmap"], os.path.join(out_dir, sample.name + ".diff.png")
    )
    report["heatmaps"] = {
        "precise": sample.name + ".precise.png",
        "preview": sample.name + ".preview.png",
        "diff": sample.name + ".diff.png",
    }
    return report


class LatencyTracker:
    """Records input-event -> preview-frame latencies and reports quantiles."""

    def __init__(self):
        self._samples_ms = []

    def record(self, elapsed_ms):
        self._samples_ms.append(float(elapsed_ms))

    def quantiles(self):
        if not self._samples_ms:
            return {"count": 0}
        arr = np.asarray(self._samples_ms)
        return {
            "count": int(arr.size),
            "min_ms": float(np.min(arr)),
            "p50_ms": float(np.percentile(arr, 50)),
            "p95_ms": float(np.percentile(arr, 95)),
            "p99_ms": float(np.percentile(arr, 99)),
            "max_ms": float(np.max(arr)),
            "within_100ms_p95": bool(np.percentile(arr, 95) <= 100.0),
        }
