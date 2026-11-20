"""Fixed-point versus float64 comparison and attribution reports."""

from __future__ import annotations

from pathlib import Path
from typing import Any

from .canonical import atomic_write_json, canonical_json, hash_object
from .engine import RunResult
from .scenario import Scenario


def _condition_key(step: dict[str, Any], record: dict[str, Any]) -> tuple[Any, ...]:
    return (
        step.get("index"),
        record.get("event_id"),
        record.get("condition_id"),
        record.get("purpose"),
        record.get("branch_id"),
    )


def _condition_index(
    result: RunResult,
) -> dict[tuple[Any, ...], tuple[dict[str, Any], dict[str, Any]]]:
    index: dict[tuple[Any, ...], tuple[dict[str, Any], dict[str, Any]]] = {}
    for step in result.steps:
        for record in step.get("evaluated_conditions", []):
            index[_condition_key(step, record)] = (step, record)
    return index


def _condition_view(
    step: dict[str, Any],
    record: dict[str, Any],
) -> dict[str, Any]:
    attribution = record.get("attribution") or {}
    earliest = attribution.get("earliest_factor") if attribution else None
    return {
        "step_index": step.get("index"),
        "selected_event": step.get("selected_event"),
        "selected_branch": step.get("selected_branch"),
        "event_id": record.get("event_id"),
        "condition_id": record.get("condition_id"),
        "purpose": record.get("purpose"),
        "branch_id": record.get("branch_id"),
        "operator": record.get("operator"),
        "point_value": record.get("point_value"),
        "lower_bound": record.get("lower_bound"),
        "upper_bound": record.get("upper_bound"),
        "cumulative_error_bound": record.get("cumulative_error_bound"),
        "threshold": record.get("threshold"),
        "truth": record.get("truth"),
        "truth_lower_bound": record.get("truth_lower_bound"),
        "truth_upper_bound": record.get("truth_upper_bound"),
        "uncertain_branch": record.get("uncertain_branch"),
        "attribution_status": attribution.get("status"),
        "earliest_factor": earliest,
        "evaluation_chain_hash": hash_object(record.get("evaluation_chain", [])),
    }


def build_comparison(
    scenario: Scenario,
    fixed_result: RunResult,
    float_result: RunResult,
) -> dict[str, Any]:
    fixed_index = _condition_index(fixed_result)
    float_index = _condition_index(float_result)
    keys = sorted(
        set(fixed_index) | set(float_index),
        key=lambda key: tuple("" if value is None else str(value) for value in key),
    )
    rows: list[dict[str, Any]] = []
    truth_differences: list[dict[str, Any]] = []
    uncertain_conditions: list[dict[str, Any]] = []
    for key in keys:
        fixed_pair = fixed_index.get(key)
        float_pair = float_index.get(key)
        fixed_view = _condition_view(*fixed_pair) if fixed_pair else None
        float_view = _condition_view(*float_pair) if float_pair else None
        truth_diff = bool(
            fixed_view is not None
            and float_view is not None
            and fixed_view["truth"] != float_view["truth"]
        )
        row = {
            "key": {
                "step_index": key[0],
                "event_id": key[1],
                "condition_id": key[2],
                "purpose": key[3],
                "branch_id": key[4],
            },
            "fixed": fixed_view,
            "float": float_view,
            "truth_difference": truth_diff,
        }
        rows.append(row)
        if truth_diff:
            truth_differences.append(
                {
                    "step_index": key[0],
                    "event_id": key[1],
                    "condition_id": key[2],
                    "purpose": key[3],
                    "branch_id": key[4],
                    "fixed": {
                        "truth": fixed_view["truth"],
                        "actual_value": fixed_view["point_value"],
                        "cumulative_error_bound": fixed_view["cumulative_error_bound"],
                        "lower_bound": fixed_view["lower_bound"],
                        "upper_bound": fixed_view["upper_bound"],
                        "uncertain_branch": fixed_view["uncertain_branch"],
                        "earliest_factor": fixed_view["earliest_factor"],
                    },
                    "float": {
                        "truth": float_view["truth"],
                        "actual_value": float_view["point_value"],
                        "cumulative_error_bound": float_view["cumulative_error_bound"],
                        "lower_bound": float_view["lower_bound"],
                        "upper_bound": float_view["upper_bound"],
                        "uncertain_branch": float_view["uncertain_branch"],
                        "earliest_factor": float_view["earliest_factor"],
                    },
                }
            )
        for mode, view in (("fixed", fixed_view), ("float", float_view)):
            if view is not None and view["uncertain_branch"]:
                uncertain_conditions.append({"mode": mode, **view})

    branch_differences: list[dict[str, Any]] = []
    max_steps = max(len(fixed_result.steps), len(float_result.steps))
    for index in range(max_steps):
        fixed_step = fixed_result.steps[index] if index < len(fixed_result.steps) else None
        float_step = float_result.steps[index] if index < len(float_result.steps) else None
        fixed_selection = (
            (fixed_step.get("selected_event"), fixed_step.get("selected_branch"))
            if fixed_step
            else (None, None)
        )
        float_selection = (
            (float_step.get("selected_event"), float_step.get("selected_branch"))
            if float_step
            else (None, None)
        )
        if fixed_selection != float_selection:
            branch_differences.append(
                {
                    "step_index": index,
                    "fixed": {
                        "selected_event": fixed_selection[0],
                        "selected_branch": fixed_selection[1],
                    },
                    "float": {
                        "selected_event": float_selection[0],
                        "selected_branch": float_selection[1],
                    },
                }
            )
    return {
        "format": "causal-comparison/1",
        "scenario_id": scenario.scenario_id,
        "scenario_hash": scenario.scenario_hash,
        "fixed": {
            "precision": fixed_result.precision.to_dict(),
            "trace_hash": fixed_result.build_trace()["trace_hash"],
            "causal_chain_hash": fixed_result.causal_chain_hash,
            "final_state_hash": fixed_result.final_state_hash,
            "terminated_reason": fixed_result.terminated_reason,
            "selected_events": [
                {
                    "step_index": step.get("index"),
                    "event_id": step.get("selected_event"),
                    "branch_id": step.get("selected_branch"),
                }
                for step in fixed_result.steps
            ],
        },
        "float": {
            "precision": float_result.precision.to_dict(),
            "trace_hash": float_result.build_trace()["trace_hash"],
            "causal_chain_hash": float_result.causal_chain_hash,
            "final_state_hash": float_result.final_state_hash,
            "terminated_reason": float_result.terminated_reason,
            "selected_events": [
                {
                    "step_index": step.get("index"),
                    "event_id": step.get("selected_event"),
                    "branch_id": step.get("selected_branch"),
                }
                for step in float_result.steps
            ],
        },
        "summary": {
            "condition_rows": len(rows),
            "truth_differences": len(truth_differences),
            "branch_differences": len(branch_differences),
            "uncertain_conditions": len(uncertain_conditions),
            "final_state_different": fixed_result.final_state_hash != float_result.final_state_hash,
        },
        "truth_differences": truth_differences,
        "branch_differences": branch_differences,
        "uncertain_conditions": uncertain_conditions,
        "conditions": rows,
    }


def build_attribution_report(
    scenario: Scenario,
    results: list[tuple[str, RunResult]],
) -> dict[str, Any]:
    entries: list[dict[str, Any]] = []
    for mode, result in results:
        for step in result.steps:
            for record in step.get("evaluated_conditions", []):
                if not record.get("uncertain_branch"):
                    continue
                entries.append(
                    {
                        "mode": mode,
                        "step_index": step.get("index"),
                        "selected_event": step.get("selected_event"),
                        "selected_branch": step.get("selected_branch"),
                        "event_id": record.get("event_id"),
                        "condition_id": record.get("condition_id"),
                        "purpose": record.get("purpose"),
                        "operator": record.get("operator"),
                        "actual_value": record.get("point_value"),
                        "lower_bound": record.get("lower_bound"),
                        "upper_bound": record.get("upper_bound"),
                        "cumulative_error_bound": record.get("cumulative_error_bound"),
                        "threshold": record.get("threshold"),
                        "truth": record.get("truth"),
                        "attribution": record.get("attribution"),
                        "evaluation_chain": record.get("evaluation_chain"),
                    }
                )
    return {
        "format": "causal-attribution/1",
        "scenario_id": scenario.scenario_id,
        "scenario_hash": scenario.scenario_hash,
        "summary": {
            "uncertain_conditions": len(entries),
            "attributed": sum(
                1
                for entry in entries
                if (entry.get("attribution") or {}).get("status") == "attributed"
            ),
        },
        "entries": entries,
        "report_hash": hash_object(entries),
    }


def _markdown_cell(value: Any) -> str:
    text = canonical_json(value) if not isinstance(value, str) else value
    return text.replace("|", "\\|").replace("\n", " ")


def render_comparison_markdown(comparison: dict[str, Any]) -> str:
    lines = [
        f"# Fixed vs float comparison: {comparison['scenario_id']}",
        "",
        (
            f"- Fixed final state: `{comparison['fixed']['final_state_hash']}`"
        ),
        (
            f"- Float final state: `{comparison['float']['final_state_hash']}`"
        ),
        f"- Truth differences: {comparison['summary']['truth_differences']}",
        f"- Branch differences: {comparison['summary']['branch_differences']}",
        f"- Uncertain conditions: {comparison['summary']['uncertain_conditions']}",
        "",
    ]
    if comparison["truth_differences"]:
        lines.extend(
            [
                "## Explicit truth differences",
                "",
                "| step | event | condition | fixed truth | fixed value | fixed error bound | float truth | float value | float error bound | earliest factor |",
                "|---:|---|---|---|---:|---|---|---|---|---|",
            ]
        )
        for item in comparison["truth_differences"]:
            fixed = item["fixed"]
            floating = item["float"]
            factor = (fixed.get("earliest_factor") or {}).get("node_id")
            lines.append(
                "| "
                + " | ".join(
                    [
                        str(item["step_index"]),
                        _markdown_cell(item["event_id"]),
                        _markdown_cell(item["condition_id"]),
                        str(fixed["truth"]),
                        _markdown_cell(fixed["actual_value"]),
                        _markdown_cell(fixed["cumulative_error_bound"]),
                        str(floating["truth"]),
                        _markdown_cell(floating["actual_value"]),
                        _markdown_cell(floating["cumulative_error_bound"]),
                        _markdown_cell(factor or "none"),
                    ]
                )
                + " |"
            )
        lines.append("")
    if comparison["branch_differences"]:
        lines.extend(["## Branch differences", ""])
        for difference in comparison["branch_differences"]:
            lines.append(
                f"- Step {difference['step_index']}: fixed "
                f"`{difference['fixed']['selected_branch']}` -> "
                f"`{difference['fixed']['selected_event']}`, float "
                f"`{difference['float']['selected_branch']}` -> "
                f"`{difference['float']['selected_event']}`."
            )
        lines.append("")
    return "\n".join(lines)


def render_attribution_markdown(report: dict[str, Any]) -> str:
    lines = [
        f"# Attribution report: {report['scenario_id']}",
        "",
        f"- Uncertain conditions: {report['summary']['uncertain_conditions']}",
        f"- Attributed conditions: {report['summary']['attributed']}",
        "",
        "| mode | step | event | condition | value | lower | upper | threshold | earliest factor | local error | sensitivity |",
        "|---|---:|---|---|---|---|---|---|---|---:|---:|",
    ]
    for entry in report["entries"]:
        attribution = entry.get("attribution") or {}
        earliest = attribution.get("earliest_factor") or {}
        lines.append(
            "| "
            + " | ".join(
                [
                    entry["mode"],
                    str(entry["step_index"]),
                    _markdown_cell(entry["event_id"]),
                    _markdown_cell(entry["condition_id"]),
                    _markdown_cell(entry["actual_value"]),
                    _markdown_cell(entry["lower_bound"]),
                    _markdown_cell(entry["upper_bound"]),
                    _markdown_cell(entry["threshold"]["value"]),
                    _markdown_cell(earliest.get("node_id", "none")),
                    _markdown_cell(earliest.get("local_error", "none")),
                    _markdown_cell(earliest.get("sensitivity", "none")),
                ]
            )
            + " |"
        )
    lines.append("")
    return "\n".join(lines)


def write_comparison_reports(
    output_dir: str | Path,
    scenario: Scenario,
    fixed_result: RunResult,
    float_result: RunResult,
    *,
    prefix: str | None = None,
) -> dict[str, Path]:
    output = Path(output_dir)
    output.mkdir(parents=True, exist_ok=True)
    stem = prefix or scenario.scenario_id
    fixed_trace = output / f"{stem}.fixed.trace.json"
    float_trace = output / f"{stem}.float.trace.json"
    fixed_result.write_trace(fixed_trace)
    float_result.write_trace(float_trace)
    comparison = build_comparison(scenario, fixed_result, float_result)
    attribution = build_attribution_report(
        scenario,
        [("fixed", fixed_result), ("float", float_result)],
    )
    comparison_path = output / f"{stem}.comparison.json"
    attribution_path = output / f"{stem}.attribution.json"
    comparison_md = output / f"{stem}.comparison.md"
    attribution_md = output / f"{stem}.attribution.md"
    atomic_write_json(comparison_path, comparison)
    atomic_write_json(attribution_path, attribution)
    comparison_md.write_text(render_comparison_markdown(comparison), encoding="utf-8")
    attribution_md.write_text(render_attribution_markdown(attribution), encoding="utf-8")
    return {
        "fixed_trace": fixed_trace,
        "float_trace": float_trace,
        "comparison": comparison_path,
        "attribution": attribution_path,
        "comparison_md": comparison_md,
        "attribution_md": attribution_md,
    }
