from __future__ import annotations

import json
import shutil
import sys
import threading
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Callable

import numpy as np

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from atlas_pipeline.codec import atomic_write_bytes, read_atlas
from atlas_pipeline.errors import AtlasError, ErrorCode
from atlas_pipeline.export import export_published, verify_export
from atlas_pipeline.gui import create_server
from atlas_pipeline.opengl_preview import check_opengl
from atlas_pipeline.pipeline import AtlasPipeline
from atlas_pipeline.render import render_preview_png
from atlas_pipeline.sample import generate_atlas, generate_suite


def main() -> int:
    target = ROOT / "artifacts" / "acceptance"
    if target.exists():
        shutil.rmtree(target)
    target.mkdir(parents=True)
    checks: list[dict[str, Any]] = []
    artifacts: dict[str, str] = {}

    def check(name: str, function: Callable[[], Any]) -> Any:
        try:
            detail = function()
            checks.append({"name": name, "status": "passed", "detail": detail})
            return detail
        except Exception as exc:
            checks.append(
                {
                    "name": name,
                    "status": "failed",
                    "detail": {
                        "error": f"{type(exc).__name__}: {exc}",
                    },
                }
            )
            raise

    try:
        suite = check(
            "deterministic sample suite",
            lambda: generate_suite(target / "suite", seed=4242),
        )
        artifacts["suite_manifest"] = str(target / "suite" / "suite-manifest.json")
        sample = generate_atlas(
            target / "production-sample.atlas",
            width=64,
            height=48,
            bit_depth=10,
            color_space="rec709",
            transfer="srgb",
            grid_cols=4,
            grid_rows=3,
            source_peak_nits=1000.0,
            seed=77,
        )
        pipeline = AtlasPipeline(sample)
        baseline = pipeline.current_version_record
        artifacts["sample"] = str(target / "production-sample.atlas")

        def incremental():
            version = pipeline.submit(
                tile_updates={1: {"exposure_ev": 0.5, "tone_curve": "reinhard"}},
                expected_version=1,
            )
            verification = pipeline.recompute_full_check(version.number)
            assert version.affected_tiles == (1,)
            assert version.reused_tiles == (0, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11)
            assert verification.exact_hash_match
            return {
                "version": version.number,
                "affected_tiles": list(version.affected_tiles),
                "reused_tiles": list(version.reused_tiles),
                "full_recompute_exact": verification.exact_hash_match,
                "maximum_summary_delta": verification.maximum_summary_delta,
            }

        check("partition conversion and incremental reuse", incremental)

        def shared_invalidation():
            version = pipeline.submit(
                config={
                    "target_color_space": "display-p3",
                    "target_transfer": "linear",
                }
            )
            assert len(version.affected_tiles) == sample.tile_count
            assert version.reused_tiles == ()
            return {
                "version": version.number,
                "affected_tiles": list(version.affected_tiles),
            }

        check("shared matrix and white-point-wide invalidation", shared_invalidation)

        def interrupted_recovery():
            job = pipeline.begin_submission(
                config={
                    "target_color_space": "rec2020",
                    "target_transfer": "pq",
                    "target_peak_nits": 1000.0,
                }
            )
            assert job.step_tile()
            assert job.step_tile()
            completed = tuple(job.completed_tiles)
            snapshot = target / "pending.snapshot.json"
            pipeline.save_snapshot(snapshot)
            recovered = AtlasPipeline.from_snapshot(snapshot)
            report = recovered.recovery_report
            assert report["resumed_pending"]
            assert report["reused_completed_tiles"] == list(completed)
            assert recovered.recompute_full_check().exact_hash_match
            artifacts["pending_snapshot"] = str(snapshot)
            return {
                "resumed_version": recovered.current_version_number,
                "completed_tiles_reused": report["reused_completed_tiles"],
                "tiles_recomputed_after_recovery": report["recomputed_tiles"],
            }

        recovered = check("forced interruption snapshot recovery", interrupted_recovery)
        pipeline = AtlasPipeline.from_snapshot(target / "pending.snapshot.json")

        def rollback():
            expected_rollback = pipeline.current_version_number + 1
            rolled = pipeline.rollback(2)
            assert rolled.rollback_of == 2
            assert rolled.number == expected_rollback
            followup = pipeline.submit(
                tile_updates={5: {"exposure_ev": 0.2, "tone_curve": "hable"}},
                expected_version=rolled.number,
            )
            assert followup.parent_version == rolled.number
            return {
                "rollback_version": rolled.number,
                "followup_version": followup.number,
                "followup_parent": followup.parent_version,
            }

        check("historical rollback and rebased submission", rollback)

        def rejection_flow():
            isolated = AtlasPipeline(sample)
            before = isolated.current_version_record
            target_peak_error = None
            exposure_error = None
            table_error = None
            try:
                isolated.submit(
                    config={"target_peak_nits": 500.0},
                    tile_updates={
                        index: {"tone_curve": "none"}
                        for index in range(sample.tile_count)
                    },
                )
            except AtlasError as exc:
                target_peak_error = exc.to_dict()
            try:
                isolated.submit(
                    tile_updates={
                        3: {"exposure_ev": 4.0, "tone_curve": "none"}
                    }
                )
            except AtlasError as exc:
                exposure_error = exc.to_dict()
            try:
                isolated.submit(tile_params=[{} for _ in range(sample.tile_count - 1)])
            except AtlasError as exc:
                table_error = exc.to_dict()
            assert isolated.current_version_number == 1
            assert isolated.get_preview().version.chain_hash == before.chain_hash
            assert target_peak_error["code"] == ErrorCode.E_PARAM_CONFLICT.value
            assert exposure_error["tile"] == 3
            assert exposure_error["field"] == "exposure_ev"
            assert table_error["field"] == "tile_params"
            return {
                "target_peak_rejection": target_peak_error,
                "exposure_rejection": exposure_error,
                "table_rejection": table_error,
                "previous_version_preserved": True,
            }

        check("diagnosable invalid-parameter rejection", rejection_flow)

        def export_flow():
            version = pipeline.current_version_record
            atlas_out = target / "exported.atlas"
            report_out = target / "exported.report.json"
            trace_out = target / "trace.json"
            result = export_published(
                pipeline,
                atlas_out,
                report_out,
                expected_version=version.number,
                expected_content_hash=version.content_hash,
            )
            verified = verify_export(atlas_out, report_out)
            pipeline.write_trace(trace_out)
            artifacts.update(
                {
                    "exported_atlas": str(atlas_out),
                    "error_report": str(report_out),
                    "trace": str(trace_out),
                }
            )
            return {
                "version": result.version,
                "atlas_sha256": result.atlas_sha256,
                "content_hash": result.content_hash,
                "report_sha256": result.report_sha256,
                "reload_verified": verified["ok"],
                "reloaded_version": verified["version"],
            }

        check("consistent export and reload verification", export_flow)

        def preview_flow():
            cpu_path = target / "preview-cpu.png"
            cpu_crc = render_preview_png(
                pipeline,
                cpu_path,
                scale=4,
                renderer="cpu",
            )
            opengl_crc = None
            opengl = check_opengl()
            if opengl.available:
                gl_path = target / "preview-opengl.png"
                opengl_crc = render_preview_png(
                    pipeline,
                    gl_path,
                    scale=4,
                    renderer="opengl",
                )
                assert gl_path.read_bytes() == cpu_path.read_bytes()
            artifacts["preview"] = str(cpu_path)
            return {
                "cpu_crc32": f"{cpu_crc:08x}",
                "opengl_available": opengl.available,
                "opengl_version": opengl.version,
                "opengl_renderer": opengl.renderer,
                "opengl_crc32": None if opengl_crc is None else f"{opengl_crc:08x}",
                "cpu_opengl_byte_identical": (
                    None if opengl_crc is None else cpu_crc == opengl_crc
                ),
            }

        check("preview rendering", preview_flow)

        def determinism():
            def run(root: Path):
                atlas = generate_atlas(
                    root / "seed.atlas",
                    width=40,
                    height=32,
                    bit_depth=12,
                    color_space="display-p3",
                    transfer="linear",
                    grid_cols=2,
                    grid_rows=2,
                    source_peak_nits=1000.0,
                    seed=8128,
                )
                local = AtlasPipeline(atlas)
                local.submit(
                    config={
                        "target_color_space": "rec2020",
                        "target_transfer": "pq",
                    },
                    tile_updates={
                        1: {"exposure_ev": -0.2, "tone_curve": "hable"},
                        3: {"exposure_ev": 0.3, "tone_curve": "reinhard"},
                    },
                    expected_version=1,
                )
                result = export_published(
                    local,
                    root / "out.atlas",
                    root / "report.json",
                )
                local.write_trace(root / "trace.json")
                local.save_snapshot(root / "snapshot.json")
                return result

            first = run(target / "det-a")
            second = run(target / "det-b")
            comparisons = {
                name: (
                    (target / "det-a" / name).read_bytes()
                    == (target / "det-b" / name).read_bytes()
                )
                for name in ("out.atlas", "report.json", "trace.json", "snapshot.json")
            }
            assert all(comparisons.values())
            assert first.atlas_sha256 == second.atlas_sha256
            return {
                "byte_identical_files": comparisons,
                "atlas_sha256": first.atlas_sha256,
                "report_sha256": first.report_sha256,
            }

        check("same seed and operation sequence determinism", determinism)

        def gui_flow():
            server = create_server(
                target / "production-sample.atlas",
                port=0,
                export_dir=target / "gui-export",
                renderer="auto",
            )
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            base = f"http://127.0.0.1:{server.server_port}"

            def request(
                path: str,
                method: str = "GET",
                payload: dict[str, Any] | None = None,
            ):
                raw = None if payload is None else json.dumps(payload).encode("utf-8")
                req = urllib.request.Request(
                    base + path,
                    data=raw,
                    method=method,
                    headers={"Content-Type": "application/json"},
                )
                try:
                    with urllib.request.urlopen(req, timeout=20) as response:
                        return response.status, response.read()
                except urllib.error.HTTPError as exc:
                    return exc.code, exc.read()

            try:
                index_status, index_body = request("/")
                state_status, state_body = request("/api/state")
                preview_status, preview_body = request(
                    "/api/preview.png?version=1&heatmap=1"
                )
                submit_status, submit_body = request(
                    "/api/submit",
                    "POST",
                    {
                        "base_version": 1,
                        "config": {
                            "target_color_space": "rec2020",
                            "target_transfer": "pq",
                            "target_peak_nits": 1000.0,
                            "target_white_point": "D65",
                        },
                        "tile_updates": {
                            "2": {
                                "exposure_ev": 0.15,
                                "tone_curve": "reinhard",
                                "tone_strength": 1.0,
                            }
                        },
                    },
                )
                if submit_status == 202:
                    for _ in range(100):
                        job_status, job_body = request("/api/job")
                        job = json.loads(job_body)["job"]
                        if job and not job["active"]:
                            break
                        threading.Event().wait(0.05)
                export_status, export_body = request("/api/export", "POST")
                assert index_status == 200
                assert state_status == 200
                assert preview_status == 200 and preview_body.startswith(b"\x89PNG")
                assert submit_status == 202
                assert export_status == 200
                exported = json.loads(export_body)
                report_status, _ = request(exported["report_url"])
                assert report_status == 200
                return {
                    "index_status": index_status,
                    "state_tiles": len(json.loads(state_body)["tiles"]),
                    "preview_bytes": len(preview_body),
                    "submit_status": submit_status,
                    "job_status": job["status"],
                    "export_status": export_status,
                    "report_download_status": report_status,
                }
            finally:
                server.shutdown()
                server.server_close()
                thread.join(timeout=3)

        check("graphical preview API and timeline controls", gui_flow)

        report = {
            "format": "atlas-pipeline-acceptance-report",
            "format_version": 1,
            "status": "passed",
            "checks": checks,
            "artifacts": artifacts,
            "baseline": {
                "version": baseline.number,
                "chain_hash": baseline.chain_hash,
                "content_hash": baseline.content_hash,
            },
            "suite_presets": len(suite),
            "recovered_pending_detail": recovered,
            "environment": {
                "python": sys.version.split()[0],
                "numpy": np.__version__,
            },
        }
    except Exception as exc:
        report = {
            "format": "atlas-pipeline-acceptance-report",
            "format_version": 1,
            "status": "failed",
            "checks": checks,
            "error": f"{type(exc).__name__}: {exc}",
        }
    payload = (
        json.dumps(report, indent=2, sort_keys=True, ensure_ascii=True, allow_nan=False)
        + "\n"
    ).encode("ascii")
    report_path = target / "acceptance-report.json"
    atomic_write_bytes(report_path, payload)
    print(payload.decode("ascii"))
    return 0 if report["status"] == "passed" else 1


if __name__ == "__main__":
    raise SystemExit(main())
