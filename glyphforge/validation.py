from __future__ import annotations

import threading
from dataclasses import replace
from pathlib import Path
from typing import Any

from .config import WorkbenchConfig
from .errors import ErrorCode, GlyphForgeError, RunCancelled
from .parser import load_font
from .pipeline import run_pipeline
from .scheduler import BudgetConfig
from .snapshot import load_snapshot
from .util import atomic_write_json, canonical_json, read_json


def _record(checks: list[dict[str, Any]], name: str, passed: bool, **details: Any) -> None:
    checks.append({"check": name, "passed": bool(passed), "details": details})


def _compare_tree_files(left: Path, right: Path, extensions: tuple[str, ...]) -> list[str]:
    differences: list[str] = []
    left_files = sorted(
        path.relative_to(left)
        for path in left.rglob("*")
        if path.is_file() and path.suffix.lower() in extensions
    )
    right_files = sorted(
        path.relative_to(right)
        for path in right.rglob("*")
        if path.is_file() and path.suffix.lower() in extensions
    )
    if left_files != right_files:
        return ["file_set"]
    for relative in left_files:
        if (left / relative).read_bytes() != (right / relative).read_bytes():
            differences.append(relative.as_posix())
    return differences


def _run_manifest_cases(
    samples_root: Path,
    output_root: Path,
    manifest: dict[str, Any],
    checks: list[dict[str, Any]],
) -> list[dict[str, Any]]:
    case_reports: list[dict[str, Any]] = []
    for index, case in enumerate(manifest.get("cases", [])):
        config = WorkbenchConfig.from_file(samples_root / case["config"])
        font = load_font(
            samples_root / case["font"],
            flatten_tolerance=config.curve_tolerance,
        )
        run_dir = output_root / "cases" / case["name"]
        repeat_dir = output_root / "determinism" / case["name"]
        try:
            result = run_pipeline(font, case["text"], config, output_dir=run_dir)
            run_pipeline(font, case["text"], config, output_dir=repeat_dir)
        except GlyphForgeError as exc:
            _record(
                checks,
                f"manifest_case:{case['name']}:run",
                False,
                error=exc.to_dict(),
            )
            continue
        baseline_by_glyph = {
            item["glyph"]: item for item in case.get("baselines", [])
        }
        mismatches: list[str] = []
        for glyph in result.glyphs:
            baseline = baseline_by_glyph.get(glyph.name)
            if baseline is None:
                mismatches.append(f"missing baseline:{glyph.name}")
                continue
            if glyph.status != baseline.get("status"):
                mismatches.append(f"status:{glyph.name}")
            if glyph.coverage_hash != baseline.get("coverage_hash"):
                mismatches.append(f"coverage_hash:{glyph.name}")
            if glyph.truth_hash != baseline.get("truth_hash"):
                mismatches.append(f"truth_hash:{glyph.name}")
            if glyph.coverage_hash != glyph.truth_hash:
                mismatches.append(f"tile_truth_diff:{glyph.name}")
        if result.atlas.alpha_hash != case.get("atlas_alpha_hash"):
            mismatches.append("atlas_alpha_hash")
        _record(
            checks,
            f"manifest_case:{case['name']}:baseline",
            not mismatches,
            mismatches=mismatches,
            glyphs=[
                {
                    "glyph": glyph.name,
                    "status": glyph.status,
                    "coverage_hash": glyph.coverage_hash,
                    "truth_hash": glyph.truth_hash,
                }
                for glyph in result.glyphs
            ],
        )
        file_differences = _compare_tree_files(
            run_dir, repeat_dir, (".jsonl", ".pgm", ".png", ".json")
        )
        committed_differences = [
            item for item in file_differences if item != "report.json"
        ]
        _record(
            checks,
            f"manifest_case:{case['name']}:deterministic",
            not committed_differences,
            differing_files=committed_differences,
        )
        case_reports.append(
            {
                "case": case["name"],
                "coverage_hashes": {
                    glyph.name: glyph.coverage_hash for glyph in result.glyphs
                },
                "atlas_alpha_hash": result.atlas.alpha_hash,
                "trace": str(
                    (run_dir / "trace.jsonl").relative_to(output_root)
                ),
            }
        )
    return case_reports


def _validate_budget_isolation(
    samples_root: Path,
    output_root: Path,
    checks: list[dict[str, Any]],
) -> dict[str, Any]:
    font = load_font(samples_root / "fonts" / "holes.json")
    config = WorkbenchConfig(
        pixel_size=12.0,
        tile_size=64,
        supersample=4,
        quant_step=1.0 / 255.0,
        fill_rule="nonzero",
        budget=BudgetConfig(
            parallelism=2,
            global_memory_bytes=4 * 1024 * 1024,
            max_task_samples=1000,
            max_task_memory_bytes=512 * 1024,
            sampling_batch_size=1024,
            min_sampling_batch_size=128,
        ),
    )
    result = run_pipeline(
        font,
        "O8",
        config,
        output_dir=output_root / "budget-isolation",
    )
    by_name = {glyph.name: glyph for glyph in result.glyphs}
    rejected = by_name.get("O")
    accepted = by_name.get("eight")
    passed = (
        rejected is not None
        and rejected.status == "budget_rejected"
        and rejected.error is not None
        and rejected.error.code == ErrorCode.E_BUDGET_EXCEEDED
        and accepted is not None
        and accepted.status == "ok"
        and [metric.name for metric in result.atlas.metrics] == ["eight"]
    )
    _record(
        checks,
        "budget_rejection_isolation",
        passed,
        requested="O8",
        rejected={
            "glyph": rejected.name if rejected else None,
            "error": rejected.error.to_dict() if rejected and rejected.error else None,
        },
        accepted={
            "glyph": accepted.name if accepted else None,
            "coverage_hash": accepted.coverage_hash if accepted else None,
        },
        atlas_glyphs=[metric.name for metric in result.atlas.metrics],
    )
    return {
        "rejected_glyph": rejected.name if rejected else None,
        "atlas_glyphs": [metric.name for metric in result.atlas.metrics],
        "report": "budget-isolation/report.json",
    }


def _validate_snapshot_recovery(
    samples_root: Path,
    output_root: Path,
    checks: list[dict[str, Any]],
) -> dict[str, Any]:
    font = load_font(samples_root / "fonts" / "holes.json")
    config = WorkbenchConfig(
        pixel_size=18.0,
        tile_size=4,
        supersample=4,
        quant_step=1.0 / 255.0,
        fill_rule="nonzero",
        budget=BudgetConfig(
            parallelism=3,
            global_memory_bytes=8 * 1024 * 1024,
            max_task_samples=1_000_000,
            max_task_memory_bytes=1024 * 1024,
            sampling_batch_size=2048,
            min_sampling_batch_size=256,
        ),
    )
    clean = run_pipeline(font, "8OB", config, output_dir=output_root / "snapshot-clean")
    snapshot_path = output_root / "snapshot-recovery" / "state.json"
    snapshot_path.parent.mkdir(parents=True, exist_ok=True)
    cancel_event = threading.Event()
    seen_tiles = 0

    def progress(event: dict[str, Any]) -> None:
        nonlocal seen_tiles
        if event.get("event") == "tile" and event.get("status") == "computed":
            seen_tiles += 1
            if seen_tiles >= 1:
                cancel_event.set()

    cancelled_cleanly = False
    try:
        run_pipeline(
            font,
            "8OB",
            config,
            snapshot_path=snapshot_path,
            cancel_event=cancel_event,
            progress=progress,
        )
    except RunCancelled:
        cancelled_cleanly = True
    snapshot = load_snapshot(snapshot_path)
    resumed = run_pipeline(
        font,
        "8OB",
        config,
        snapshot_path=snapshot_path,
        resume=True,
        output_dir=output_root / "snapshot-resumed",
    )
    hashes_match = all(
        clean_glyph.coverage_hash == resumed_glyph.coverage_hash
        for clean_glyph, resumed_glyph in zip(clean.glyphs, resumed.glyphs)
    )
    checksum_match = clean.atlas.alpha_hash == resumed.atlas.alpha_hash
    passed = (
        cancelled_cleanly
        and len(snapshot.tiles) > 0
        and resumed.reuse_count > 0
        and resumed.recompute_count > 0
        and hashes_match
        and checksum_match
    )
    _record(
        checks,
        "snapshot_recovery",
        passed,
        cancelled=cancelled_cleanly,
        snapshot_tiles=len(snapshot.tiles),
        snapshot_chain_hash=snapshot.chain_hash,
        reused_tiles=resumed.reuse_count,
        recomputed_tiles=resumed.recompute_count,
        hashes_match=hashes_match,
        atlas_hash_match=checksum_match,
    )

    tampered_path = output_root / "snapshot-recovery" / "tampered.json"
    document = read_json(snapshot_path)
    first_tile = document["tiles"][0]
    first_tile["content_hash"] = "0" * 64
    tampered_path.write_text(canonical_json(document) + "\n", encoding="utf-8")
    tamper_code: str | None = None
    try:
        load_snapshot(tampered_path)
    except GlyphForgeError as exc:
        tamper_code = exc.code.value
    _record(
        checks,
        "snapshot_tamper_detection",
        tamper_code == ErrorCode.E_SNAPSHOT_MISMATCH.value,
        code=tamper_code,
    )
    return {
        "snapshot_completed_tiles": len(snapshot.tiles),
        "reused_tiles": resumed.reuse_count,
        "recomputed_tiles": resumed.recompute_count,
        "coverage_hashes": {
            glyph.name: glyph.coverage_hash for glyph in resumed.glyphs
        },
        "trace": "snapshot-resumed/trace.jsonl",
        "tamper_code": tamper_code,
    }


def _validate_quantization_change(
    samples_root: Path,
    output_root: Path,
    checks: list[dict[str, Any]],
) -> dict[str, Any]:
    font = load_font(samples_root / "fonts" / "mixed.json")
    base = WorkbenchConfig(
        pixel_size=20.0,
        tile_size=5,
        supersample=4,
        quant_step=1.0 / 10.0,
        fill_rule="nonzero",
    )
    first = run_pipeline(font, "AOB", base)
    previous = {
        glyph.name: glyph.summary.to_dict()
        for glyph in first.glyphs
        if glyph.summary is not None
    }
    changed = replace(base, quant_step=1.0 / 255.0)
    second = run_pipeline(
        font,
        "AOB",
        changed,
        output_dir=output_root / "quantization-change",
        previous_summaries=previous,
    )
    comparisons = {
        glyph.name: glyph.comparison.to_dict()
        for glyph in second.glyphs
        if glyph.comparison is not None
    }
    passed = (
        len(comparisons) == len(second.glyphs)
        and all(
            comparison["current_step"] == changed.quant_step
            and comparison["previous_step"] == base.quant_step
            and comparison["abs_error_delta"] != 0.0
            for comparison in comparisons.values()
        )
    )
    _record(
        checks,
        "quantization_recompute_delta",
        passed,
        comparisons=comparisons,
    )
    return {
        "previous_step": base.quant_step,
        "current_step": changed.quant_step,
        "comparisons": comparisons,
        "trace": "quantization-change/trace.jsonl",
    }


def validate_samples(
    samples_root: str | Path,
    output_root: str | Path,
) -> dict[str, Any]:
    samples = Path(samples_root)
    output = Path(output_root)
    output.mkdir(parents=True, exist_ok=True)
    manifest = read_json(samples / "manifest.json")
    checks: list[dict[str, Any]] = []
    cases = _run_manifest_cases(samples, output, manifest, checks)
    budget = _validate_budget_isolation(samples, output, checks)
    snapshot = _validate_snapshot_recovery(samples, output, checks)
    quantization = _validate_quantization_change(samples, output, checks)
    passed = all(check["passed"] for check in checks)
    report = {
        "format": "glyphforge.validation-report/1",
        "passed": passed,
        "samples": str(samples),
        "checks": checks,
        "cases": cases,
        "budget_isolation": budget,
        "snapshot_recovery": snapshot,
        "quantization_change": quantization,
    }
    atomic_write_json(output / "validation-report.json", report, pretty=True)
    return report
