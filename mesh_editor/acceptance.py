from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Any, Callable

from .editing import Selection, simplify_edge_collapse, transform_selection
from .errors import (
    E_RECOVERY_INVALID,
    E_VERSION_LOSS,
    E_VERSION_STALE,
    RecoveryError,
    StaleVersionError,
    VersionLossError,
)
from .formats import (
    export_mesh,
    loss_report,
    parse_mesh,
    serialize_obj,
)
from .precision import compare_meshes, summarize_precision, write_precision_report
from .samplegen import generate_project_samples
from .topology import weld_vertices
from .versioning import VersionRepository, canonical_json


def _rel(project_root: Path, path: Path) -> str:
    try:
        return str(path.relative_to(project_root))
    except ValueError:
        return str(path)


def run_acceptance(project_root: str | Path) -> dict[str, Any]:
    root = Path(project_root).resolve()
    verification = root / "data" / "verification"
    logs = root / "logs"
    if verification.exists():
        shutil.rmtree(verification)
    verification.mkdir(parents=True, exist_ok=True)
    logs.mkdir(parents=True, exist_ok=True)

    checks: list[dict[str, Any]] = []

    def add(name: str, passed: bool, **details: Any) -> None:
        checks.append({"name": name, "status": "PASS" if passed else "FAIL", **details})

    try:
        sample_report = generate_project_samples(root)
        add(
            "deterministic_local_samples",
            all((root / "data/samples" / item["path"]).is_file() for item in sample_report["artifacts"].values()),
            artifacts={name: item["sha256"] for name, item in sample_report["artifacts"].items()},
        )
        sample_paths = {
            "obj_v2": root / "data/samples/part_v2.obj",
            "obj_v1": root / "data/samples/part_v1.obj",
            "stl_ascii": root / "data/samples/part_ascii.stl",
            "stl_binary": root / "data/samples/part_binary.stl",
        }
        parsed = {name: parse_mesh(path) for name, path in sample_paths.items()}
        add(
            "format_loading",
            len(parsed["obj_v2"].faces) == 14
            and len(parsed["obj_v1"].faces) == 14
            and len(parsed["stl_ascii"].faces) == 14
            and len(parsed["stl_binary"].faces) == 14,
            counts={name: len(mesh.faces) for name, mesh in parsed.items()},
        )
        upgraded_v1 = serialize_obj(parsed["obj_v1"], 2)
        add(
            "v1_to_v2_upgrade",
            b"# mesh_editor:format_version 2" in upgraded_v1,
            upgraded_bytes=len(upgraded_v1),
        )

        repo = VersionRepository(root / "data/workspace")
        baseline_mesh, baseline_entry = repo.load_current()
        truth_mesh = parsed["obj_v2"]
        add(
            "committed_baseline",
            baseline_entry["version"] == 1,
            version=1,
            content_hash=baseline_entry["content_hash"],
            metrics=baseline_entry["metrics_after"],
        )

        roundtrip_obj = verification / "baseline_roundtrip.obj"
        report_obj = repo.export_version(1, roundtrip_obj, "obj", 2)
        reloaded_obj = parse_mesh(roundtrip_obj)
        precision_obj = compare_meshes(truth_mesh, reloaded_obj, tolerance=1e-7)
        write_precision_report(precision_obj, verification / "precision_obj_roundtrip.json")
        add(
            "obj_v2_roundtrip",
            report_obj.code == "OK" and precision_obj["code"] == "OK",
            export=report_obj.to_dict(),
            precision=summarize_precision(precision_obj),
        )

        for variant, ascii_stl in (("ascii", True), ("binary", False)):
            target = verification / f"baseline_{variant}.stl"
            report_stl = repo.export_version(
                1, target, "stl", 2, ascii_stl=ascii_stl, allow_lossy=True
            )
            reloaded_stl = parse_mesh(target)
            precision_stl = compare_meshes(truth_mesh, reloaded_stl, tolerance=1e-6)
            write_precision_report(precision_stl, verification / f"precision_stl_{variant}.json")
            add(
                f"stl_{variant}_roundtrip_and_loss_report",
                report_stl.code == E_VERSION_LOSS
                and bool(report_stl.affected_faces)
                and precision_stl["code"] == "OK",
                export=report_stl.to_dict(),
                precision=summarize_precision(precision_stl),
            )

        v1_loss_path = verification / "must_not_exist_v1.obj"
        try:
            repo.export_version(1, v1_loss_path, "obj", 1)
            v1_loss = None
        except VersionLossError as exc:
            v1_loss = exc
        add(
            "obj_v1_loss_is_explicit",
            v1_loss is not None
            and v1_loss.code == E_VERSION_LOSS
            and bool(v1_loss.faces)
            and not v1_loss_path.exists(),
            faces=sorted(v1_loss.faces) if v1_loss else [],
        )

        first_weld = weld_vertices(parsed["obj_v2"], 1e-6)
        second_weld = weld_vertices(parsed["obj_v2"], 1e-6)
        add(
            "weld_determinism_and_mapping",
            first_weld.report() == second_weld.report()
            and first_weld.old_to_new[8] == 0
            and first_weld.merged_vertices == 1,
            mapping=first_weld.mapping_dict(),
            affected_faces=list(first_weld.affected_faces),
            topology=first_weld.topology.summary(),
        )

        current_version = 1
        transform_result = transform_selection(
            baseline_mesh,
            Selection(face_indices=(0, 1, 2)),
            translation=(0.1, 0.0, 0.0),
            rotation_degrees=(0.0, 0.0, 5.0),
            scale=0.98,
        )
        v2_entry = repo.commit_edit(
            current_version,
            transform_result.mesh,
            metrics_before=transform_result.metrics_before,
            metrics_after=transform_result.metrics_after,
            operation=transform_result.operation,
            weld_tolerance=1e-6,
        )
        current_version = int(v2_entry["version"])
        simplify_result = simplify_edge_collapse(transform_result.mesh, target_ratio=0.8)
        v3_entry = repo.commit_edit(
            current_version,
            simplify_result.mesh,
            metrics_before=simplify_result.metrics_before,
            metrics_after=simplify_result.metrics_after,
            operation=simplify_result.operation,
            weld_tolerance=1e-6,
        )
        current_version = int(v3_entry["version"])
        add(
            "two_edits_generate_monotonic_versions",
            (v2_entry["version"], v3_entry["version"]) == (2, 3),
            versions=[1, 2, 3],
            v2_metrics=v2_entry["metrics_after"],
            v3_metrics=v3_entry["metrics_after"],
        )

        undo_entry = repo.undo_last_commit()
        restored_mesh, restored_entry = repo.load_current()
        add(
            "single_step_undo_metrics_exact",
            undo_entry["version"] == 2
            and restored_entry["version"] == 2
            and restored_mesh.metrics() == v2_entry["metrics_after"]
            and restored_entry["metrics_after"] == v2_entry["metrics_after"],
            restored_version=restored_entry["version"],
            next_version=repo.read_manifest()["next_version"],
            metrics=restored_mesh.metrics(),
        )

        repo.rollback_to(1)
        rollback_mesh, _ = repo.load_current()
        post_rollback = transform_selection(
            rollback_mesh,
            Selection(face_indices=(3, 4)),
            translation=(0.0, 0.05, 0.0),
        )
        v4_entry = repo.commit_edit(
            1,
            post_rollback.mesh,
            metrics_before=post_rollback.metrics_before,
            metrics_after=post_rollback.metrics_after,
            operation=post_rollback.operation,
            weld_tolerance=1e-6,
        )
        current_version = int(v4_entry["version"])
        add(
            "rollback_recomputes_dependencies_and_keeps_monotonic_number",
            current_version == 4
            and v4_entry["parent"] == 1
            and v4_entry["dependencies"]["base_content_hash"] == baseline_entry["content_hash"]
            and v4_entry["dependencies"]["dependency_key"]
            != v3_entry["dependencies"]["dependency_key"],
            version=current_version,
            parent=v4_entry["parent"],
            dependency_key=v4_entry["dependencies"]["dependency_key"],
            invalidated_cache_entries=repo.cache.invalidations,
        )

        current_mesh, current_entry = repo.load_current()
        edited_stl = verification / "edited_current.stl"
        edited_export = repo.export_version(
            current_version,
            edited_stl,
            "stl",
            2,
            ascii_stl=False,
            allow_lossy=True,
        )
        edited_reload = parse_mesh(edited_stl)
        edited_precision = compare_meshes(
            truth_mesh, edited_reload, tolerance=1e-4, normal_tolerance_deg=2.0
        )
        write_precision_report(
            edited_precision, verification / "precision_edited_stl.json"
        )
        add(
            "edited_export_deviation_report",
            edited_export.code == E_VERSION_LOSS
            and edited_precision["code"] == "E_PRECISION_TOLERANCE"
            and bool(edited_precision["candidate_bad_faces"])
            and edited_precision["max_bidirectional_distance"] > 1e-4,
            export=edited_export.to_dict(),
            precision=summarize_precision(edited_precision),
            bad_candidate_faces=edited_precision["candidate_bad_faces"][:20],
        )

        stale_case = verification / "stale_case"
        shutil.copytree(root / "data/workspace", stale_case)
        stale_repo = VersionRepository(stale_case)
        stale_entry = stale_repo.current_entry()
        stale_snapshot = stale_repo.snapshots_dir / stale_entry["mesh_relpath"]
        stale_snapshot.write_bytes(stale_snapshot.read_bytes()[:-11])
        stale_code = None
        try:
            stale_repo.export_version(current_version, verification / "stale.obj", "obj", 2)
        except StaleVersionError as exc:
            stale_code = exc.code
        recover_code = None
        try:
            stale_repo.recover()
        except RecoveryError as exc:
            recover_code = exc.code
        add(
            "stale_export_and_truncated_recovery_rejected",
            stale_code == E_VERSION_STALE and recover_code == E_RECOVERY_INVALID,
            stale_code=stale_code,
            recovery_code=recover_code,
        )

        chain_case = verification / "chain_case"
        shutil.copytree(root / "data/workspace", chain_case)
        chain_manifest_path = chain_case / "manifest.json"
        chain_manifest = json.loads(chain_manifest_path.read_text(encoding="utf-8"))
        chain_manifest["versions"][0]["metrics_after"]["volume"] = 123456.0
        chain_manifest_path.write_text(
            canonical_json(chain_manifest) + "\n", encoding="utf-8"
        )
        chain_code = None
        try:
            VersionRepository(chain_case).recover()
        except RecoveryError as exc:
            chain_code = exc.code
        add(
            "hash_chain_tamper_rejected",
            chain_code == E_RECOVERY_INVALID,
            recovery_code=chain_code,
        )

        crash_case = verification / "crash_case"
        shutil.copytree(root / "data/workspace", crash_case)
        crash_repo = VersionRepository(crash_case)
        committed_before_crash = crash_repo.current_entry()["version"]
        script = f"""
import sys
sys.path.insert(0, {str(root)!r})
from mesh_editor.editing import Selection, transform_selection
from mesh_editor.versioning import VersionRepository
repo = VersionRepository({str(crash_case)!r})
mesh, entry = repo.load_current()
result = transform_selection(mesh, Selection(face_indices=(0,)), translation=(1.0, 0.0, 0.0))
repo.commit_edit(entry["version"], result.mesh, metrics_before=result.metrics_before,
    metrics_after=result.metrics_after, operation=result.operation, weld_tolerance=1e-6)
"""
        env = dict(os.environ)
        env.pop("MESH_EDITOR_CRASH_BEFORE_MANIFEST", None)
        env["MESH_EDITOR_CRASH_AFTER_STAGE"] = "1"
        process = subprocess.run(
            [sys.executable, "-c", script],
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )
        staging_before = len(list(crash_repo.snapshots_dir.glob(".staging-*")))
        recovery = crash_repo.recover()
        staging_after = len(list(crash_repo.snapshots_dir.glob(".staging-*")))
        add(
            "forced_termination_recovers_only_committed_state",
            process.returncode == 91
            and recovery.recovered_version == committed_before_crash
            and staging_before == 1
            and staging_after == 0,
            forced_exit=process.returncode,
            committed_before=committed_before_crash,
            recovered_version=recovery.recovered_version,
            discarded_staging=recovery.discarded_staging,
            interrupted_exit_report=recovery.to_dict()["exit_report_for_interrupted_edit"],
        )

        deterministic_pairs: dict[str, dict[str, str]] = {"a": {}, "b": {}}
        for label in ("a", "b"):
            det_root = verification / f"determinism_{label}"
            det_repo = VersionRepository(det_root)
            det_mesh = weld_vertices(parsed["obj_v2"], 1e-6).mesh
            det_repo.create_baseline(
                det_mesh,
                weld_tolerance=1e-6,
                operation={"type": "baseline", "source": "part_v2.obj"},
            )
            det_transform = transform_selection(
                det_mesh,
                Selection(face_indices=(0, 1)),
                translation=(0.125, -0.25, 0.5),
                rotation_degrees=(1.0, 2.0, 3.0),
                scale=0.95,
            )
            det_repo.commit_edit(
                1,
                det_transform.mesh,
                metrics_before=det_transform.metrics_before,
                metrics_after=det_transform.metrics_after,
                operation=det_transform.operation,
                weld_tolerance=1e-6,
            )
            det_repo.write_trace(det_root / "trace.jsonl")
            det_repo.export_version(2, det_root / "export.obj", "obj", 2)
            for name in ("manifest.json", "trace.jsonl", "export.obj", "snapshots/v00000002/mesh.obj"):
                deterministic_pairs[label][name] = (
                    det_root / name
                ).read_bytes().hex()
        add(
            "byte_deterministic_export_and_trace",
            deterministic_pairs["a"] == deterministic_pairs["b"],
            compared_files=sorted(deterministic_pairs["a"]),
        )

        ui_env = dict(os.environ)
        for variable in (
            "MESH_EDITOR_CRASH_AFTER_STAGE",
            "MESH_EDITOR_CRASH_BEFORE_MANIFEST",
            "MESH_EDITOR_FAIL_COMMIT",
        ):
            ui_env.pop(variable, None)
        ui_platform = "wayland" if ui_env.get("WAYLAND_DISPLAY") else "offscreen"
        ui_env["QT_QPA_PLATFORM"] = ui_platform
        ui_process = subprocess.run(
            [
                sys.executable,
                "-m",
                "mesh_editor",
                "gui",
                "--workspace",
                str(root / "data/workspace"),
                "--data",
                str(root / "data"),
                "--backend",
                "auto",
                "--smoke-test",
            ],
            cwd=root,
            env=ui_env,
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        ui_output = ui_process.stdout + ui_process.stderr
        add(
            "desktop_window_and_opengl_smoke",
            ui_process.returncode == 0
            and "UI_SMOKE_READY" in ui_output
            and "RENDER_BACKEND=" in ui_output
            and "Traceback" not in ui_output,
            platform=ui_platform,
            backend_line=next(
                (line for line in ui_output.splitlines() if line.startswith("RENDER_BACKEND=")),
                "",
            ),
            exit_code=ui_process.returncode,
        )

        passed = sum(1 for check in checks if check["status"] == "PASS")
        report = {
            "schema": "mesh-editor-acceptance-v1",
            "all_pass": passed == len(checks),
            "checks_passed": passed,
            "checks_total": len(checks),
            "checks": checks,
            "current_version": current_entry["version"],
            "current_content_hash": current_entry["content_hash"],
            "samples": {
                name: _rel(root, path) for name, path in sample_paths.items()
            },
            "runs_tests": "python -m unittest discover -s tests -v",
        }
    except Exception as exc:
        checks.append(
            {
                "name": "acceptance_runner",
                "status": "FAIL",
                "error": f"{type(exc).__name__}: {exc}",
            }
        )
        report = {
            "schema": "mesh-editor-acceptance-v1",
            "all_pass": False,
            "checks_passed": sum(1 for check in checks if check["status"] == "PASS"),
            "checks_total": len(checks),
            "checks": checks,
        }

    payload = json.dumps(report, sort_keys=True, indent=2, ensure_ascii=False) + "\n"
    (verification / "acceptance_report.json").write_text(payload, encoding="utf-8")
    (logs / "verification.log").write_text(payload, encoding="utf-8")
    return report
