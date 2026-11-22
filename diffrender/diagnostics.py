"""Reconstruction diagnostics, plots and parameter truth comparisons."""

from __future__ import annotations

import csv
import html
import json
import os
from pathlib import Path
from typing import Any

import matplotlib

matplotlib.use("Agg")
import matplotlib.pyplot as plt  # noqa: E402
import numpy as np  # noqa: E402

from .optimize import OptimizationRunner  # noqa: E402


def _write_json(path: Path, value: Any) -> None:
    path.write_text(
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def export_diagnostics(
    runner: OptimizationRunner,
    output_dir: str | os.PathLike[str],
) -> dict[str, Any]:
    root = Path(output_dir)
    root.mkdir(parents=True, exist_ok=True)
    report = runner.report()
    images = runner.render_final_images()
    targets = runner.target_tensors
    target_plots: list[Path] = []
    reconstruction_plots: list[Path] = []
    difference_plots: list[Path] = []
    for index, (target, image) in enumerate(zip(targets, images)):
        target_np = target.detach().cpu().numpy()
        image_np = image.detach().cpu().numpy()
        difference_np = np.abs(target_np - image_np)
        target_path = root / f"target_{index:02d}.png"
        reconstruction_path = root / f"reconstruction_{index:02d}.png"
        difference_path = root / f"difference_{index:02d}.png"
        plt.imsave(target_path, np.clip(target_np, 0.0, 1.0))
        plt.imsave(reconstruction_path, np.clip(image_np, 0.0, 1.0))
        plt.imsave(difference_path, difference_np.mean(axis=2), cmap="magma")
        target_plots.append(target_path)
        reconstruction_plots.append(reconstruction_path)
        difference_plots.append(difference_path)

    image_count = len(images)
    figure, axes = plt.subplots(
        3,
        image_count,
        figsize=(4.1 * image_count, 8.2),
        squeeze=False,
    )
    for index, (target, image) in enumerate(zip(targets, images)):
        target_np = target.detach().cpu().numpy()
        image_np = image.detach().cpu().numpy()
        difference_np = np.abs(target_np - image_np).mean(axis=2)
        axes[0, index].imshow(np.clip(target_np, 0.0, 1.0))
        axes[0, index].set_title(f"target {index}")
        axes[1, index].imshow(np.clip(image_np, 0.0, 1.0))
        axes[1, index].set_title(f"reconstruction {index}")
        image_handle = axes[2, index].imshow(difference_np, cmap="magma")
        axes[2, index].set_title(f"|difference| {index}")
        figure.colorbar(image_handle, ax=axes[2, index], fraction=0.046)
        for row in range(3):
            axes[row, index].axis("off")
    figure.tight_layout()
    comparison_path = root / "comparison.png"
    figure.savefig(comparison_path, dpi=130)
    plt.close(figure)

    figure, axis = plt.subplots(figsize=(8.4, 4.4))
    axis.plot(range(1, len(report.loss_history) + 1), report.loss_history, marker="o")
    axis.set_xlabel("iteration completed")
    axis.set_ylabel("photometric loss")
    axis.set_title("Photometric loss")
    axis.grid(alpha=0.25)
    loss_path = root / "loss_curve.png"
    figure.tight_layout()
    figure.savefig(loss_path, dpi=140)
    plt.close(figure)

    global_norms = [
        entry["global_norm_preclip"] for entry in report.gradient_history
    ]
    figure, axis = plt.subplots(figsize=(8.4, 4.4))
    axis.semilogy(
        range(1, len(global_norms) + 1),
        np.maximum(global_norms, 1.0e-16),
        marker="o",
        label="global pre-clip norm",
    )
    clipping_rounds = [
        index + 1
        for index, entry in enumerate(report.gradient_history)
        if entry.get("clipping_triggered")
    ]
    if clipping_rounds:
        axis.scatter(
            clipping_rounds,
            [
                max(global_norms[index - 1], 1.0e-16)
                for index in clipping_rounds
            ],
            color="tab:red",
            label="clipping triggered",
            zorder=3,
        )
    axis.set_xlabel("iteration completed")
    axis.set_ylabel("gradient norm (log scale)")
    axis.set_title("Gradient norm history")
    axis.grid(alpha=0.25)
    axis.legend()
    gradient_path = root / "gradient_curve.png"
    figure.tight_layout()
    figure.savefig(gradient_path, dpi=140)
    plt.close(figure)

    per_block = report.block_statistics.get("per_block", {})
    block_ids = list(per_block)
    mean_times = [
        per_block[block_id]["total_ms"] / max(1.0, per_block[block_id]["count"])
        for block_id in block_ids
    ]
    figure, axis = plt.subplots(
        figsize=(max(8.4, 0.25 * len(block_ids)), 4.4)
    )
    if block_ids:
        axis.bar(range(len(block_ids)), mean_times)
        axis.set_xticks(range(len(block_ids)))
        axis.set_xticklabels(block_ids, rotation=90, fontsize=6)
    axis.set_ylabel("mean block time (ms)")
    axis.set_title("Block-level timing")
    axis.grid(axis="y", alpha=0.25)
    block_path = root / "block_times.png"
    figure.tight_layout()
    figure.savefig(block_path, dpi=140)
    plt.close(figure)

    rows = []
    for label, error in report.parameter_errors.items():
        rows.append(
            {
                "parameter": label,
                "estimate": error["estimate"],
                "truth": error["truth"],
                "absolute_error": error["absolute_error"],
                "relative_error": error["relative_error"],
            }
        )
    error_json = root / "parameter_errors.json"
    _write_json(error_json, rows)
    error_csv = root / "parameter_errors.csv"
    with error_csv.open("w", encoding="utf-8", newline="") as handle:
        writer = csv.DictWriter(handle, fieldnames=list(rows[0]) if rows else [])
        writer.writeheader()
        writer.writerows(rows)

    display_rows = rows[:34]
    figure_height = max(4.0, 0.26 * len(display_rows) + 1.5)
    figure, axis = plt.subplots(figsize=(11.0, figure_height))
    axis.axis("off")
    table_data = [
        [
            row["parameter"],
            f"{row['estimate']:.6f}",
            f"{row['truth']:.6f}",
            f"{row['absolute_error']:.6f}",
            (
                f"{row['relative_error']:.3%}"
                if abs(row["truth"]) >= 1.0e-6
                else "n/a (truth near zero)"
            ),
        ]
        for row in display_rows
    ]
    table = axis.table(
        cellText=table_data,
        colLabels=["parameter", "estimate", "truth", "abs error", "rel error"],
        loc="center",
        cellLoc="left",
    )
    table.auto_set_font_size(False)
    table.set_fontsize(7.5)
    table.scale(1.0, 1.15)
    axis.set_title("Parameter truth comparison", fontsize=12, pad=12)
    table_path = root / "parameter_table.png"
    figure.tight_layout()
    figure.savefig(table_path, dpi=150)
    plt.close(figure)

    dashboard_path = root / "dashboard.html"
    image_cards = []
    for index in range(image_count):
        image_cards.append(
            "<section class='card'>"
            f"<h3>View {index}</h3>"
            "<div class='views'>"
            f"<figure><img src='target_{index:02d}.png'><figcaption>Target</figcaption></figure>"
            f"<figure><img src='reconstruction_{index:02d}.png'><figcaption>Reconstruction</figcaption></figure>"
            f"<figure><img src='difference_{index:02d}.png'><figcaption>Absolute difference</figcaption></figure>"
            "</div></section>"
        )
    table_rows = "".join(
        "<tr>"
        f"<td>{html.escape(row['parameter'])}</td>"
        f"<td>{row['estimate']:.8f}</td>"
        f"<td>{row['truth']:.8f}</td>"
        f"<td>{row['absolute_error']:.8f}</td>"
        f"<td>{row['relative_error']:.4%}</td>"
        "</tr>"
        for row in rows
    )
    dashboard_path.write_text(
        f"""<!doctype html>
<html lang="zh-CN">
<meta charset="utf-8">
<title>Inverse rendering diagnostics</title>
<style>
body {{ font-family: system-ui, sans-serif; margin: 24px; background: #f5f6f8; color: #20242b; }}
h1 {{ margin-top: 0; }}
.grid {{ display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 16px; }}
.card {{ background: white; border-radius: 10px; padding: 14px; box-shadow: 0 1px 5px #0001; margin-bottom: 16px; }}
.views {{ display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; }}
figure {{ margin: 0; }} img {{ width: 100%; image-rendering: auto; border-radius: 5px; }}
figcaption {{ font-size: 12px; color: #57606a; margin-top: 4px; }}
table {{ border-collapse: collapse; width: 100%; background: white; }}
th, td {{ border-bottom: 1px solid #ddd; padding: 5px 7px; text-align: left; font-size: 13px; }}
th {{ background: #eceff3; position: sticky; top: 0; }}
.chart {{ width: 100%; background: white; border-radius: 10px; }}
code {{ background: #e9edf2; padding: 2px 4px; border-radius: 4px; }}
</style>
<h1>Inverse rendering diagnostics</h1>
<p>Task <code>{html.escape(runner.task.scene.scene_id)}</code> ·
mode <code>{html.escape(report.strategy['mode'])}</code> ·
final loss <code>{report.final_loss:.8f}</code></p>
<div class="grid">
  <div class="card"><h3>Photometric loss</h3><img class="chart" src="loss_curve.png"></div>
  <div class="card"><h3>Gradient norms</h3><img class="chart" src="gradient_curve.png"></div>
  <div class="card"><h3>Block timings</h3><img class="chart" src="block_times.png"></div>
</div>
{''.join(image_cards)}
<section class="card"><h3>Parameter truth comparison</h3>
<table><thead><tr><th>Parameter</th><th>Estimate</th><th>Truth</th><th>Abs error</th><th>Rel error</th></tr></thead>
<tbody>{table_rows}</tbody></table></section>
</html>
""",
        encoding="utf-8",
    )

    summary = {
        "ok": True,
        "diagnostics_dir": str(root),
        "comparison": str(comparison_path),
        "loss_curve": str(loss_path),
        "gradient_curve": str(gradient_path),
        "block_times": str(block_path),
        "parameter_table": str(table_path),
        "dashboard": str(dashboard_path),
        "parameter_errors_json": str(error_json),
        "parameter_errors_csv": str(error_csv),
        "target_images": [str(path) for path in target_plots],
        "reconstruction_images": [str(path) for path in reconstruction_plots],
        "difference_heatmaps": [str(path) for path in difference_plots],
        "final_loss": report.final_loss,
        "loss_start": report.loss_history[0] if report.loss_history else None,
        "loss_end": report.loss_history[-1] if report.loss_history else None,
        "mean_absolute_parameter_error": (
            sum(row["absolute_error"] for row in rows) / len(rows) if rows else 0.0
        ),
        "mean_relative_parameter_error": (
            sum(
                row["relative_error"]
                for row in rows
                if abs(row["truth"]) >= 1.0e-6
            )
            / max(
                1,
                sum(1 for row in rows if abs(row["truth"]) >= 1.0e-6),
            )
            if rows
            else 0.0
        ),
        "block_statistics": report.block_statistics,
        "strategy": report.strategy,
    }
    _write_json(root / "diagnostics_summary.json", summary)
    return summary

