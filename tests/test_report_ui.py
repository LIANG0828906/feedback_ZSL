from __future__ import annotations

from balance_lab.budget import SimulationRequest
from balance_lab.report import build_report
from balance_lab.scheduler import run_validation


def test_report_contains_budget_truth_thresholds_and_markdown(alpha) -> None:
    project, candidates = alpha
    request = SimulationRequest(target_samples=5, min_samples=2, total_sample_budget=240)
    result = run_validation(project, candidates, request, mode="full")
    report = build_report(project, candidates, result)
    payload = report.to_dict()
    assert payload["summary"]["status"] == "COMPLETE"
    assert payload["budget"]["submitted_combos"] == len(result.combos)
    assert payload["candidates"]
    assert payload["candidates"][0]["scenarios"]
    assert payload["candidates"][0]["scenarios"][0]["thresholds"]
    assert payload["budget"]["decisions"]
    markdown = report.to_markdown()
    assert "战斗平衡验证报告" in markdown
    assert "预算与取舍" in markdown
    assert "阈值诊断" in markdown


def test_qt_window_smoke(alpha) -> None:
    from balance_lab.ui.app import create_application, create_main_window

    app = create_application(["pytest-ui"])
    window = create_main_window()
    window.show()
    app.processEvents()
    assert window.bundle is not None
    assert window.candidate_table.rowCount() == 7
    assert window.decisions_table.rowCount() > 0
    assert window.graph_filter.count() == 4
    assert window.graph_view is not None
    image = window.grab()
    assert not image.isNull()
    window.close()
    app.processEvents()

