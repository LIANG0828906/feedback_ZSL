"""Scripted end-to-end local validation experiment."""

from __future__ import annotations

import json
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any

from .canonical import atomic_write_json, canonical_bytes, hash_object, parse_json_bytes
from .comparison import write_comparison_reports
from .engine import Engine, replay_trace
from .errors import KernelError
from .injection import INJECTIONS
from .precision import Precision
from .samples import generate_samples
from .scenario import Scenario, load_scenario


INJECTION_CODE = {
    "cycle": "E_INJECT_CYCLE",
    "precision-conflict": "E_INJECT_PRECISION_CONFLICT",
    "deep-chain": "E_INJECT_DEPTH_EXCEEDED",
    "nonfinite": "E_INJECT_NONFINITE_COEFFICIENT",
    "truncated": "E_INJECT_TRUNCATED",
}


def _scenario_precision(scenario: Scenario, mode: str) -> Precision:
    if mode == "fixed":
        return Precision(
            "fixed",
            scenario.precision.decimal_places,
            scenario.precision.rounding,
        )
    return Precision("float")


def _run_replay_check(
    scenario_path: Path,
    trace_path: Path,
) -> dict[str, Any]:
    try:
        result = replay_trace(scenario_path, trace_path)
        return {
            "ok": True,
            "step_count": len(result.steps),
            "final_state_hash": result.final_state_hash,
            "causal_chain_hash": result.causal_chain_hash,
        }
    except KernelError as error:
        return {"ok": False, "error": error.to_dict()}


def _run_injection_checks(
    scenario_path: Path,
    reports_dir: Path,
) -> dict[str, Any]:
    checks: dict[str, Any] = {}
    for injection in sorted(INJECTIONS):
        marker = reports_dir / "injection" / f"{injection}.trace.json"
        marker.parent.mkdir(parents=True, exist_ok=True)
        if marker.exists():
            marker.unlink()
        try:
            load_scenario(scenario_path, injection=injection)
            checks[injection] = {
                "ok": False,
                "error": "injection unexpectedly loaded",
            }
        except KernelError as error:
            expected = INJECTION_CODE[injection]
            checks[injection] = {
                "ok": error.code == expected and not marker.exists(),
                "expected_code": expected,
                "actual_code": error.code,
                "location": error.location,
                "trace_written": marker.exists(),
            }
    return checks


def _run_trace_checks(
    scenario_path: Path,
    valid_trace: Path,
    reports_dir: Path,
) -> dict[str, Any]:
    tampered_path = reports_dir / "trace_checks" / "tampered.trace.json"
    truncated_path = reports_dir / "trace_checks" / "truncated.trace.json"
    tampered_path.parent.mkdir(parents=True, exist_ok=True)
    trace = parse_json_bytes(valid_trace.read_bytes())
    trace["steps"][0]["state_after_hash"] = "0" * 64
    atomic_write_json(tampered_path, trace)
    raw = valid_trace.read_bytes()
    truncated_path.write_bytes(raw[: max(1, len(raw) // 2)])
    checks: dict[str, Any] = {}
    for name, path in (("tampered", tampered_path), ("truncated", truncated_path)):
        try:
            replay_trace(scenario_path, path)
            checks[name] = {"ok": False, "error": "invalid trace was accepted"}
        except KernelError as error:
            checks[name] = {
                "ok": error.code == "E_TRACE_INVALID",
                "code": error.code,
                "location": error.location,
            }
    return checks


def _run_snapshot_recovery(
    scenario_path: Path,
    reports_dir: Path,
    python_executable: str,
    root: Path,
) -> dict[str, Any]:
    snapshot_dir = reports_dir / "recovery_snapshots"
    if snapshot_dir.exists():
        shutil.rmtree(snapshot_dir)
    snapshot_dir.mkdir(parents=True, exist_ok=True)
    recovered_trace = reports_dir / "recovery.trace.json"
    uninterrupted_trace = reports_dir / "uninterrupted.trace.json"
    common = [
        python_executable,
        "-m",
        "causalkernel.cli",
        "run",
        "--scenario",
        str(scenario_path),
        "--mode",
        "fixed",
        "--places",
        "6",
        "--max-steps",
        "6",
        "--snapshot-dir",
        str(snapshot_dir),
    ]
    crash = subprocess.run(
        common + ["--crash-after", "3"],
        cwd=root,
        capture_output=True,
        text=True,
        check=False,
    )
    snapshot_files = sorted(snapshot_dir.glob("snapshot-*.json"))
    latest_snapshot_count = None
    if snapshot_files:
        latest_snapshot_count = parse_json_bytes(
            snapshot_files[-1].read_bytes()
        ).get("step_count")
    recovery = subprocess.run(
        common + ["--recover", "--trace", str(recovered_trace)],
        cwd=root,
        capture_output=True,
        text=True,
        check=False,
    )
    baseline = subprocess.run(
        common + ["--trace", str(uninterrupted_trace)],
        cwd=root,
        capture_output=True,
        text=True,
        check=False,
    )
    result: dict[str, Any] = {
        "crash_exit_code": crash.returncode,
        "crash_exit_expected": 17,
        "snapshot_files": len(snapshot_files),
        "latest_snapshot_step_count": latest_snapshot_count,
        "recovery_exit_code": recovery.returncode,
        "baseline_exit_code": baseline.returncode,
    }
    if recovery.returncode == 0 and baseline.returncode == 0:
        recovered = parse_json_bytes(recovered_trace.read_bytes())
        baseline_data = parse_json_bytes(uninterrupted_trace.read_bytes())
        byte_identical = recovered_trace.read_bytes() == uninterrupted_trace.read_bytes()
        result.update(
            {
                "ok": (
                    crash.returncode == 17
                    and latest_snapshot_count == 3
                    and recovered["steps"] == baseline_data["steps"]
                    and recovered["final_state_hash"] == baseline_data["final_state_hash"]
                    and recovered["causal_chain_hash"] == baseline_data["causal_chain_hash"]
                    and len(recovered["steps"]) > int(latest_snapshot_count or 0)
                    and byte_identical
                ),
                "recovered_step_count": len(recovered["steps"]),
                "no_duplicate_prefix": recovered["steps"][:3] == baseline_data["steps"][:3],
                "byte_identical_to_uninterrupted": byte_identical,
            }
        )
    else:
        result.update(
            {
                "ok": False,
                "recovery_stdout": recovery.stdout,
                "recovery_stderr": recovery.stderr,
                "baseline_stdout": baseline.stdout,
                "baseline_stderr": baseline.stderr,
            }
        )
    all_snapshots = sorted(snapshot_dir.glob("snapshot-*.json"))
    if all_snapshots:
        corrupt_path = all_snapshots[-1]
        original = parse_json_bytes(corrupt_path.read_bytes())
        scenario = load_scenario(scenario_path)
        hash_corrupt = json.loads(json.dumps(original))
        numeric_cell = next(
            cell
            for cell in hash_corrupt["state"].values()
            if cell.get("kind") == "number"
        )
        numeric_cell["value"] = {"format": "fixed", "value": "999999"}
        atomic_write_json(corrupt_path, hash_corrupt)
        try:
            Engine.recover(
                scenario,
                Precision("fixed", 6),
                snapshot_dir,
                max_steps=6,
            )
            result["hash_corruption_rejected"] = False
        except KernelError as error:
            result["hash_corruption_rejected"] = error.code == "E_RECOVERY_INVALID"
            result["hash_corruption_error"] = error.to_dict()

        version_corrupt = json.loads(json.dumps(original))
        version_corrupt["version"] = 999
        version_corrupt.pop("snapshot_hash", None)
        version_corrupt["snapshot_hash"] = hash_object(version_corrupt)
        atomic_write_json(corrupt_path, version_corrupt)
        try:
            Engine.recover(
                scenario,
                Precision("fixed", 6),
                snapshot_dir,
                max_steps=6,
            )
            result["version_corruption_rejected"] = False
        except KernelError as error:
            result["version_corruption_rejected"] = error.code == "E_RECOVERY_INVALID"
            result["version_corruption_error"] = error.to_dict()
        atomic_write_json(corrupt_path, original)
    else:
        result["hash_corruption_rejected"] = False
        result["version_corruption_rejected"] = False
    return result


def _run_determinism_check(
    scenario_path: Path,
    operations: list[Any],
    reports_dir: Path,
) -> dict[str, Any]:
    scenario = load_scenario(scenario_path)
    precision = _scenario_precision(scenario, "fixed")
    trace_a = Engine(
        scenario,
        precision,
        operations=operations,
        max_steps=12,
    ).run().build_trace()
    trace_b = Engine(
        scenario,
        precision,
        operations=operations,
        max_steps=12,
    ).run().build_trace()
    output = reports_dir / "determinism"
    output.mkdir(parents=True, exist_ok=True)
    path_a = output / "a.trace.json"
    path_b = output / "b.trace.json"
    atomic_write_json(path_a, trace_a)
    atomic_write_json(path_b, trace_b)
    bytes_a = path_a.read_bytes()
    bytes_b = path_b.read_bytes()
    return {
        "ok": bytes_a == bytes_b and trace_a["trace_hash"] == trace_b["trace_hash"],
        "byte_identical": bytes_a == bytes_b,
        "trace_hash_a": trace_a["trace_hash"],
        "trace_hash_b": trace_b["trace_hash"],
        "bytes": len(bytes_a),
    }


def _render_results_markdown(report: dict[str, Any]) -> str:
    lines = [
        "# Causal kernel local validation",
        "",
        f"- Overall: **{'PASS' if report['ok'] else 'FAIL'}**",
        f"- Seed: `{report['seed']}`",
        f"- NumPy: `{report['environment']['numpy']}`",
        f"- Python: `{report['environment']['python']}`",
        "",
        "## Scenario comparisons",
        "",
        "| scenario | fixed final | float final | truth diffs | branch diffs | uncertain | replay |",
        "|---|---|---|---:|---:|---:|---|",
    ]
    for name, item in report["scenarios"].items():
        comparison = item["comparison"]
        lines.append(
            "| "
            + " | ".join(
                [
                    name,
                    f"`{comparison['fixed']['final_state_hash'][:12]}`",
                    f"`{comparison['float']['final_state_hash'][:12]}`",
                    str(comparison["summary"]["truth_differences"]),
                    str(comparison["summary"]["branch_differences"]),
                    str(comparison["summary"]["uncertain_conditions"]),
                    "PASS" if item["replay"]["ok"] else "FAIL",
                ]
            )
            + " |"
        )
    lines.extend(
        [
            "",
            "## Fault injection",
            "",
            "| injection | code | trace written | result |",
            "|---|---|---|---|",
        ]
    )
    for name, item in report["injections"].items():
        lines.append(
            f"| {name} | `{item.get('actual_code')}` | "
            f"{item.get('trace_written')} | {'PASS' if item['ok'] else 'FAIL'} |"
        )
    lines.extend(
        [
            "",
            "## Recovery and integrity",
            "",
            f"- Forced termination recovery: {'PASS' if report['snapshot_recovery']['ok'] else 'FAIL'}",
            f"- Recovered trace byte-identical: {'PASS' if report['snapshot_recovery'].get('byte_identical_to_uninterrupted') else 'FAIL'}",
            f"- Snapshot state-hash rejection: {'PASS' if report['snapshot_recovery'].get('hash_corruption_rejected') else 'FAIL'}",
            f"- Snapshot version rejection: {'PASS' if report['snapshot_recovery'].get('version_corruption_rejected') else 'FAIL'}",
            f"- Byte-identical deterministic traces: {'PASS' if report['determinism']['ok'] else 'FAIL'}",
            f"- Tampered trace rejection: {'PASS' if report['trace_checks']['tampered']['ok'] else 'FAIL'}",
            f"- Truncated trace rejection: {'PASS' if report['trace_checks']['truncated']['ok'] else 'FAIL'}",
            "",
        ]
    )
    return "\n".join(lines)


def run_experiment(
    data_dir: str | Path = "data",
    reports_dir: str | Path = "reports",
    seed: int = 20261121,
) -> dict[str, Any]:
    data = Path(data_dir)
    reports = Path(reports_dir)
    data.mkdir(parents=True, exist_ok=True)
    reports.mkdir(parents=True, exist_ok=True)
    root = Path(__file__).resolve().parent.parent
    manifest = generate_samples(data, seed)
    operations_map = parse_json_bytes((data / "operations.json").read_bytes())
    scenario_results: dict[str, Any] = {}
    comparison_data: dict[str, Any] = {}
    all_scenarios_ok = True
    for name, metadata in manifest["profiles"].items():
        scenario_path = data / name
        scenario = load_scenario(scenario_path)
        operations = operations_map.get(scenario.scenario_id, [])
        fixed = Engine(
            scenario,
            _scenario_precision(scenario, "fixed"),
            operations=operations,
            max_steps=64,
        ).run()
        floating = Engine(
            scenario,
            Precision("float"),
            operations=operations,
            max_steps=64,
        ).run()
        paths = write_comparison_reports(
            reports / "scenarios",
            scenario,
            fixed,
            floating,
            prefix=scenario.scenario_id,
        )
        comparison = parse_json_bytes(paths["comparison"].read_bytes())
        replay = _run_replay_check(scenario_path, paths["fixed_trace"])
        scenario_results[name] = {
            "scenario_id": scenario.scenario_id,
            "scenario_hash": scenario.scenario_hash,
            "operations": operations,
            "fixed_step_count": len(fixed.steps),
            "float_step_count": len(floating.steps),
            "fixed_selected": [
                [step["selected_event"], step["selected_branch"]] for step in fixed.steps
            ],
            "float_selected": [
                [step["selected_event"], step["selected_branch"]] for step in floating.steps
            ],
            "comparison": comparison,
            "replay": replay,
            "reports": {key: str(path) for key, path in paths.items()},
        }
        comparison_data[name] = comparison
        if not replay["ok"]:
            all_scenarios_ok = False
    branch_flip = scenario_results["branch_flip.json"]
    branch_flip_ok = (
        branch_flip["comparison"]["summary"]["truth_differences"] >= 1
        and branch_flip["comparison"]["summary"]["branch_differences"] >= 1
        and ["edge", "b_edge"] in branch_flip["fixed_selected"]
        and ["fallback", "b_fallback"] in branch_flip["float_selected"]
    )
    injection_checks = _run_injection_checks(data / "branch_flip.json", reports)
    trace_checks = _run_trace_checks(
        data / "branch_flip.json",
        Path(branch_flip["reports"]["fixed_trace"]),
        reports,
    )
    recovery = _run_snapshot_recovery(
        data / "deterministic_medium.json",
        reports,
        sys.executable,
        root,
    )
    determinism = _run_determinism_check(
        data / "branch_flip.json",
        operations_map.get("branch_flip", []),
        reports,
    )
    atomic_path = reports / "atomic_roundtrip.json"
    atomic_payload = {"format": "atomic-check/1", "value": "durable"}
    atomic_write_json(atomic_path, atomic_payload)
    atomic_ok = parse_json_bytes(atomic_path.read_bytes()) == atomic_payload
    environment = {
        "python": sys.version.split()[0],
        "numpy": __import__("numpy").__version__,
    }
    ok = (
        all_scenarios_ok
        and branch_flip_ok
        and all(item["ok"] for item in injection_checks.values())
        and all(item["ok"] for item in trace_checks.values())
        and recovery["ok"]
        and recovery.get("hash_corruption_rejected", False)
        and recovery.get("version_corruption_rejected", False)
        and determinism["ok"]
        and atomic_ok
    )
    report: dict[str, Any] = {
        "format": "causal-validation/1",
        "seed": seed,
        "ok": ok,
        "environment": environment,
        "manifest": manifest,
        "scenarios": scenario_results,
        "branch_flip_expectation": {
            "ok": branch_flip_ok,
            "expected_fixed": ["edge", "b_edge"],
            "expected_float": ["fallback", "b_fallback"],
        },
        "injections": injection_checks,
        "trace_checks": trace_checks,
        "snapshot_recovery": recovery,
        "determinism": determinism,
        "atomic_roundtrip": {"ok": atomic_ok, "path": str(atomic_path)},
    }
    report["report_hash"] = hash_object(report)
    atomic_write_json(reports / "validation_report.json", report)
    (reports / "RESULTS.md").write_text(
        _render_results_markdown(report),
        encoding="utf-8",
    )
    consolidated = {
        "format": "causal-experiment/1",
        "seed": seed,
        "ok": ok,
        "comparisons": comparison_data,
        "validation_report_hash": report["report_hash"],
    }
    atomic_write_json(reports / "experiment_summary.json", consolidated)
    return report
