from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from dataclasses import replace
from pathlib import Path
from typing import Any

from .canonical import canonical_json_bytes, sha256_bytes, sha256_json
from .errors import E_COMMIT_FAILED, E_STALE_HANDLE, PipelineError
from .params import RenderParams
from .preview import PreviewService
from .recovery import recover
from .samples import generate_samples
from .scene import load_scene
from .storage import Store


def _cli_env() -> dict[str, str]:
    env = os.environ.copy()
    source_root = str(Path(__file__).resolve().parents[1])
    existing = env.get("PYTHONPATH")
    env["PYTHONPATH"] = (
        source_root if not existing else source_root + os.pathsep + existing
    )
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    return env


def _run_cli(
    output_root: Path,
    *arguments: str,
    expected_codes: tuple[int, ...] = (0,),
) -> tuple[int, dict[str, Any] | None]:
    command = [sys.executable, "-m", "npr_pipeline.cli", *arguments]
    completed = subprocess.run(
        command,
        env=_cli_env(),
        text=True,
        capture_output=True,
        check=False,
    )
    payload: dict[str, Any] | None = None
    raw = completed.stdout.strip() if completed.returncode == 0 else completed.stderr.strip()
    if raw:
        try:
            payload = json.loads(raw.splitlines()[-1])
        except json.JSONDecodeError:
            payload = {"raw": raw}
    if completed.returncode not in expected_codes:
        raise RuntimeError(
            f"CLI failed ({completed.returncode}): {' '.join(command)}\n"
            f"stdout={completed.stdout}\nstderr={completed.stderr}"
        )
    return completed.returncode, payload


def _params_json(params: RenderParams) -> dict[str, Any]:
    return params.to_dict()


def _write_params(path: Path, params: RenderParams) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(canonical_json_bytes(_params_json(params)) + b"\n")


def _trace_records(path: Path) -> list[dict[str, Any]]:
    return [
        json.loads(line)
        for line in path.read_text(encoding="utf-8").splitlines()
        if line.strip()
    ]


def _latest_version_record(records: list[dict[str, Any]]) -> dict[str, Any]:
    versions = [record for record in records if record.get("type") == "version"]
    if not versions:
        raise AssertionError("trace has no version record")
    return versions[-1]


def _layer_record(
    records: list[dict[str, Any]],
    version: int,
    layer: int,
) -> dict[str, Any]:
    matches = [
        record
        for record in records
        if record.get("type") == "layer"
        and int(record.get("version", -1)) == version
        and int(record.get("layer", -1)) == layer
    ]
    if not matches:
        raise AssertionError(f"trace has no layer {layer} record for v{version}")
    return matches[-1]


def _relative(path: Path, root: Path) -> str:
    return path.relative_to(root).as_posix()


def run_regression(root: str | Path, *, seed: int = 20261122) -> dict[str, Any]:
    output = Path(root)
    if output.exists():
        shutil.rmtree(output)
    output.mkdir(parents=True)
    samples_root = output / "samples"
    traces_root = output / "traces"
    runs_root = output / "runs"
    operations_root = output / "operations"
    traces_root.mkdir(parents=True)
    runs_root.mkdir(parents=True)
    operations_root.mkdir(parents=True)
    sample_manifest = generate_samples(samples_root, seed=seed)
    checks: list[dict[str, Any]] = []

    def check(name: str, condition: bool, detail: Any = None) -> None:
        if not condition:
            raise AssertionError(f"regression check failed: {name} ({detail!r})")
        checks.append({"name": name, "ok": True, "detail": detail})

    case_results: dict[str, Any] = {}
    baseline_by_name = {
        case["name"]: json.loads(
            (samples_root / case["baseline"]).read_text(encoding="utf-8")
        )
        for case in sample_manifest["cases"]
    }
    for case in sample_manifest["cases"]:
        name = case["name"]
        case_dir = samples_root / name
        workdir = runs_root / name
        trace_path = traces_root / f"{name}.jsonl"
        _code, initial = _run_cli(
            output,
            "render",
            "--scene",
            str(case_dir / "scene.json"),
            "--params",
            str(case_dir / "params.json"),
            "--workdir",
            str(workdir),
            "--trace",
            str(trace_path),
            "--operation",
            "initial",
        )
        assert initial is not None
        check(
            f"{name}: initial baseline hash",
            initial["content_hash"] == baseline_by_name[name]["version_hash"],
            initial["content_hash"],
        )
        check(f"{name}: initial version", initial["version"] == 1, initial["version"])
        _code, unchanged = _run_cli(
            output,
            "render",
            "--scene",
            str(case_dir / "scene.json"),
            "--params",
            str(case_dir / "params.json"),
            "--workdir",
            str(workdir),
            "--trace",
            str(trace_path),
            "--operation",
            "no-change",
        )
        assert unchanged is not None
        check(f"{name}: no-change reuses all", not unchanged["committed"], unchanged["version"])
        check(
            f"{name}: no-change reuse count",
            unchanged["affected_batches"] == 0 and unchanged["reused_batches"] > 0,
            unchanged["reused_batches"],
        )
        preview = PreviewService(Store(workdir)).refresh()
        check(
            f"{name}: preview hash",
            preview is not None and preview.content_hash == initial["content_hash"],
            None if preview is None else preview.content_hash,
        )
        if preview is not None:
            records = _trace_records(trace_path)
            check(
                f"{name}: trace preview hash",
                _latest_version_record(records)["content_hash"] == preview.content_hash,
            )
        case_results[name] = {
            "workdir": _relative(workdir, output),
            "trace": _relative(trace_path, output),
            "version_hash": initial["content_hash"],
            "batch_count": int(initial["affected_batches"]),
        }

    balanced_dir = samples_root / "balanced"
    balanced_workdir = runs_root / "balanced"
    balanced_trace = traces_root / "balanced.jsonl"
    scene = load_scene(balanced_dir / "scene.json")
    base_params = RenderParams.from_dict(
        json.loads((balanced_dir / "params.json").read_text(encoding="utf-8"))
    )
    params_v2 = replace(
        base_params,
        shadow=replace(
            base_params.shadow,
            spacing=round(base_params.shadow.spacing * 1.18, 6),
            jitter=round(base_params.shadow.jitter + 0.15, 6),
        ),
    )
    params_v3 = replace(
        params_v2,
        smear=replace(params_v2.smear, width=round(params_v2.smear.width + 0.85, 6)),
    )
    params_fault = replace(
        params_v3,
        outline=replace(params_v3.outline, color="#000000"),
    )
    params_crash = replace(
        params_v3,
        outline=replace(
            params_v3.outline,
            width=round(params_v3.outline.width + 0.27, 6),
        ),
    )
    v2_path = operations_root / "params_v2.json"
    v3_path = operations_root / "params_v3.json"
    fault_path = operations_root / "params_fault.json"
    crash_path = operations_root / "params_crash.json"
    _write_params(v2_path, params_v2)
    _write_params(v3_path, params_v3)
    _write_params(fault_path, params_fault)
    _write_params(crash_path, params_crash)

    _code, v2 = _run_cli(
        output,
        "render",
        "--scene",
        str(balanced_dir / "scene.json"),
        "--params",
        str(v2_path),
        "--workdir",
        str(balanced_workdir),
        "--trace",
        str(balanced_trace),
        "--operation",
        "shadow-density-change",
    )
    assert v2 is not None
    check("incremental: version 2", v2["version"] == 2, v2["version"])
    records = _trace_records(balanced_trace)
    check(
        "incremental: outline reused",
        _layer_record(records, 2, 0)["rewritten_batches"] == 0,
    )
    check(
        "incremental: shadow rewritten",
        _layer_record(records, 2, 1)["rewritten_batches"] > 0,
    )

    _code, v3 = _run_cli(
        output,
        "render",
        "--scene",
        str(balanced_dir / "scene.json"),
        "--params",
        str(v3_path),
        "--workdir",
        str(balanced_workdir),
        "--trace",
        str(balanced_trace),
        "--operation",
        "smear-width-change",
    )
    assert v3 is not None
    check("incremental: version 3", v3["version"] == 3, v3["version"])
    records = _trace_records(balanced_trace)
    check(
        "incremental: outline and shadow reused",
        _layer_record(records, 3, 0)["rewritten_batches"] == 0
        and _layer_record(records, 3, 1)["rewritten_batches"] == 0,
    )
    check(
        "incremental: smear rewritten",
        _layer_record(records, 3, 2)["rewritten_batches"] > 0,
    )

    crash_preview_service = PreviewService(Store(balanced_workdir))
    pre_crash_preview = crash_preview_service.refresh()
    check(
        "preview before crash",
        pre_crash_preview is not None and pre_crash_preview.version == 3,
    )

    fault_code, fault_payload = _run_cli(
        output,
        "render",
        "--scene",
        str(balanced_dir / "scene.json"),
        "--params",
        str(fault_path),
        "--workdir",
        str(balanced_workdir),
        "--trace",
        str(balanced_trace),
        "--operation",
        "injected-commit-failure",
        "--fail-after-batch",
        "1",
        expected_codes=(3,),
    )
    check("atomic failure: exit code", fault_code == 3, fault_code)
    check(
        "atomic failure: error code",
        fault_payload is not None and fault_payload.get("code") == E_COMMIT_FAILED,
        fault_payload,
    )
    current_after_fault = Store(balanced_workdir).current_manifest()
    check(
        "atomic failure: old version retained",
        current_after_fault is not None and current_after_fault["version"] == 3,
    )
    check(
        "atomic failure: no temp files",
        not list((balanced_workdir).rglob(".*.tmp.*")),
    )

    crash_code, crash_payload = _run_cli(
        output,
        "render",
        "--scene",
        str(balanced_dir / "scene.json"),
        "--params",
        str(crash_path),
        "--workdir",
        str(balanced_workdir),
        "--trace",
        str(balanced_trace),
        "--operation",
        "forced-termination",
        "--crash-after-batch",
        "3",
        expected_codes=(91,),
    )
    check("forced termination: exit code", crash_code == 91, crash_code)
    check("forced termination: no stdout result", crash_payload is None and not crash_payload)
    current_after_crash = Store(balanced_workdir).current_manifest()
    check(
        "forced termination: old version retained",
        current_after_crash is not None and current_after_crash["version"] == 3,
    )
    recovery_result = recover(Store(balanced_workdir), cleanup=True, keep_staging=True)
    check(
        "recovery: resume available",
        recovery_result.resume_available,
        recovery_result.to_dict(),
    )
    check(
        "recovery: completed batches identified",
        len(recovery_result.completed_batches) == 3,
        len(recovery_result.completed_batches),
    )
    check(
        "recovery: missing batches identified",
        len(recovery_result.missing_batches) > 0,
        len(recovery_result.missing_batches),
    )

    _code, resumed = _run_cli(
        output,
        "render",
        "--scene",
        str(balanced_dir / "scene.json"),
        "--params",
        str(crash_path),
        "--workdir",
        str(balanced_workdir),
        "--trace",
        str(balanced_trace),
        "--operation",
        "resume-after-forced-termination",
    )
    assert resumed is not None
    check("recovery: final version", resumed["version"] == 4, resumed["version"])
    check(
        "recovery: staged batches reused",
        resumed["resumed_batches"] >= 3,
        resumed["resumed_batches"],
    )
    records = _trace_records(balanced_trace)
    check(
        "recovery: trace resumed count",
        _latest_version_record(records)["resumed_batches"] >= 3,
    )

    final_preview = PreviewService(Store(balanced_workdir)).refresh()
    check("preview: final snapshot", final_preview is not None)
    assert final_preview is not None
    check(
        "preview: trace hash match",
        _latest_version_record(records)["content_hash"] == final_preview.content_hash,
    )
    check(
        "preview: result hash match",
        resumed["content_hash"] == final_preview.content_hash,
    )
    if pre_crash_preview is not None:
        crash_preview_service.refresh()
        try:
            crash_preview_service.resolve_handle(pre_crash_preview)
            stale_ok = False
        except PipelineError as exc:
            stale_ok = exc.code == E_STALE_HANDLE
        check("preview: stale handle rejected", stale_ok)

    trace_bytes = balanced_trace.read_bytes()
    baseline_bytes = (samples_root / "manifest.json").read_bytes()
    report = {
        "schema": "npr-regression-report-v1",
        "seed": seed,
        "cases": case_results,
        "incremental": {
            "v2_content_hash": v2["content_hash"],
            "v3_content_hash": v3["content_hash"],
            "final_version": resumed["version"],
            "final_content_hash": resumed["content_hash"],
            "fault_error": fault_payload,
            "crash_exit_code": crash_code,
            "recovery_completed_batches": len(recovery_result.completed_batches),
            "recovery_missing_batches": len(recovery_result.missing_batches),
            "resumed_batches": resumed["resumed_batches"],
            "preview_content_hash": final_preview.content_hash,
        },
        "trace_sha256": sha256_bytes(trace_bytes),
        "sample_manifest_sha256": sha256_bytes(baseline_bytes),
        "checks_sha256": sha256_json(checks),
        "checks": checks,
        "verified": True,
    }
    (output / "report.json").write_bytes(canonical_json_bytes(report) + b"\n")
    return report


__all__ = ["run_regression"]
