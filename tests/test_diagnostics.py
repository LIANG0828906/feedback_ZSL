from __future__ import annotations

from diffrender.diagnostics import export_diagnostics
from diffrender.optimize import OptimizationRunner, RunConfig


def test_diagnostics_export_contains_dashboard_and_curves(dataset, tmp_path):
    task = dataset.task("single")
    runner = OptimizationRunner(
        task,
        RunConfig(
            iterations=1,
            learning_rate=0.006,
            mode="exact",
            checkpoint_every=0,
        ),
    )
    runner.run_to(1)
    output = tmp_path / "diagnostics"
    summary = export_diagnostics(runner, output)
    assert (output / "dashboard.html").is_file()
    assert (output / "comparison.png").is_file()
    assert (output / "loss_curve.png").is_file()
    assert (output / "gradient_curve.png").is_file()
    assert (output / "block_times.png").is_file()
    assert (output / "parameter_table.png").is_file()
    assert summary["dashboard"].endswith("dashboard.html")
    html = (output / "dashboard.html").read_text(encoding="utf-8")
    assert "Parameter truth comparison" in html
    assert "Absolute difference" in html

