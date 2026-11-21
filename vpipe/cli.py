from __future__ import annotations

import argparse
import json
import math
import os
import shutil
import tempfile
from pathlib import Path
from typing import Any

from .boolean import OPERATIONS, BooleanOptions, boolean_paths
from .compat import compare_geometry
from .errors import (
    E_PREVIEW_STALE,
    E_PROJECT_MISMATCH,
    E_VERSION_LOSS,
    PipelineError,
)
from .intersect import compare_intersection_results, intersect_segments
from .io import export_path, load_path
from .metrics import ErrorThresholds
from .model import Point, Segment
from .project import load_project, save_project
from .raster import RasterFrame, rasterize_path
from .samplegen import ensure_samples
from .tasks import BooleanService


DEFAULT_SAMPLES = "samples"
DEFAULT_ARTIFACTS = "artifacts"


class TraceWriter:
    def __init__(self, path: Path) -> None:
        self.path = path
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.sequence = 0
        self._handle = self.path.open("w", encoding="utf-8", newline="\n")

    def emit(self, event: str, **data: Any) -> None:
        self.sequence += 1
        record = {
            "seq": self.sequence,
            "event": event,
            "data": data,
        }
        self._handle.write(
            json.dumps(
                record,
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
                allow_nan=False,
            )
            + "\n"
        )
        self._handle.flush()

    def close(self) -> None:
        self._handle.close()

    def __enter__(self) -> "TraceWriter":
        return self

    def __exit__(self, exc_type: Any, exc: Any, traceback: Any) -> None:
        self.close()


def _write_ppm(path: Path, frame: RasterFrame) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    header = f"P6\n{frame.width} {frame.height}\n255\n".encode("ascii")
    rgb = bytearray(frame.width * frame.height * 3)
    for index in range(frame.width * frame.height):
        source = index * 4
        target = index * 3
        rgb[target : target + 3] = frame.rgba[source : source + 3]
    _atomic_write_bytes(path, header + bytes(rgb))


def _atomic_write_bytes(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(
        prefix=f".{path.name}.",
        suffix=".tmp",
        dir=str(path.parent),
    )
    try:
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, path)
    except Exception:
        try:
            os.unlink(temporary)
        except OSError:
            pass
        raise


def _parse_thresholds(args: argparse.Namespace) -> ErrorThresholds:
    return ErrorThresholds(
        max_area_relative_error=args.max_area_error,
        max_boundary_error=args.max_boundary_error,
    )


def _summary(record: Any, exported: Any | None = None) -> dict[str, Any]:
    result = {
        "result_id": record.result_id,
        "result_hash": record.result_hash,
        "operation": record.operation,
        "tolerance": record.tolerance,
        "loop_count": record.boolean_report.get("loop_count"),
        "area": record.boolean_report.get("area"),
        "self_intersections": record.boolean_report.get("self_intersections"),
        "traceable_edges": all(
            segment.trace
            for contour in record.path.contours
            for segment in contour.segments
        ),
        "metrics": record.metrics.to_dict(),
        "exportable": record.exportable,
    }
    if exported is not None:
        payload, report = exported
        result["export"] = {
            "version": payload.get("version"),
            "compatibility": report.to_dict(),
        }
    return result


def command_generate_samples(args: argparse.Namespace) -> int:
    manifest = ensure_samples(args.root)
    print(json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


def command_run(args: argparse.Namespace) -> int:
    ensure_samples(args.samples)
    if args.tolerance is None:
        tolerance = None
    else:
        tolerance = float(args.tolerance)
    thresholds = _parse_thresholds(args)
    log_path = Path(args.log)
    with TraceWriter(log_path) as trace:
        trace.emit(
            "run_start",
            path_a=str(args.a),
            path_b=str(args.b),
            operation=args.operation,
            requested_tolerance=tolerance,
            thresholds=thresholds.to_dict(),
        )
        path_a = load_path(args.a)
        path_b = load_path(args.b)
        trace.emit(
            "paths_loaded",
            path_a_hash=path_a.content_hash(),
            path_b_hash=path_b.content_hash(),
            path_a_version=path_a.version,
            path_b_version=path_b.version,
            path_a_tolerance=path_a.tolerance,
            path_b_tolerance=path_b.tolerance,
        )
        service = BooleanService()
        record = service.run_sync(
            path_a,
            path_b,
            args.operation,
            tolerance,
            thresholds,
        )
        trace.emit("boolean_committed", **_summary(record))
        exported = None
        if args.export_version is not None:
            exported = service.export_result(
                record,
                args.export_version,
                args.out_result,
            )
            trace.emit(
                "result_exported",
                destination=str(args.out_result),
                version=args.export_version,
                compatibility=exported[1].to_dict(),
            )
        elif args.out_result:
            export_path(record.path, 2, args.out_result)
            trace.emit(
                "result_exported",
                destination=str(args.out_result),
                version=2,
            )
        _, frame = service.store.snapshot()
        if args.preview:
            _write_ppm(Path(args.preview), frame)
            trace.emit(
                "preview_written",
                destination=str(args.preview),
                raster_hash=frame.raster_hash,
                result_hash=frame.relevant_result_hash,
            )
        if args.project:
            document = save_project(
                args.project,
                path_a,
                path_b,
                args.operation,
                record.tolerance,
                {
                    "path_a_version": path_a.version,
                    "path_b_version": path_b.version,
                    "result_version": 2,
                },
                thresholds.to_dict(),
                record,
            )
            trace.emit(
                "project_saved",
                destination=str(args.project),
                project_content_hash=document["content_hash"],
            )
        summary = _summary(record, exported)
        summary["raster_hash"] = frame.raster_hash
        summary["preview_sequence"] = frame.sequence
        print(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True))
    return 0


def command_intersect(args: argparse.Namespace) -> int:
    tolerance = float(args.tolerance)
    first = Segment.line(Point(args.a1, args.a2), Point(args.b1, args.b2))
    second = Segment.line(Point(args.c1, args.c2), Point(args.d1, args.d2))
    result = intersect_segments(first, second, tolerance)
    print(json.dumps(result.to_dict(), ensure_ascii=False, indent=2, sort_keys=True))
    return 0


def command_acceptance(args: argparse.Namespace) -> int:
    sample_root = Path(args.samples)
    artifacts = Path(args.artifacts)
    artifacts.mkdir(parents=True, exist_ok=True)
    ensure_samples(sample_root)
    trace_path = artifacts / "trace.jsonl"
    report_path = artifacts / "acceptance_report.json"
    report: dict[str, Any] = {
        "sample_manifest": json.loads(
            (sample_root / "manifest.json").read_text(encoding="utf-8")
        ),
        "checks": {},
        "operations": {},
        "status": "running",
    }
    all_ok = True
    with TraceWriter(trace_path) as trace:
        trace.emit(
            "acceptance_start",
            sample_root=str(sample_root),
            artifact_root=str(artifacts),
        )
        compatibility: list[dict[str, Any]] = []
        for name in [
            "base_a",
            "base_b",
            "nested",
            "near_a",
            "near_b",
            "degenerate",
            "shared",
        ]:
            v1 = load_path(sample_root / "v1" / f"{name}.json")
            v2 = load_path(sample_root / "v2" / f"{name}.json")
            comparison = compare_geometry(v1, v2, max(v1.tolerance, v2.tolerance))
            _, downgrade = export_path(v2, 1)
            item = {
                "sample": name,
                "consistent": comparison["consistent"],
                "max_boundary_deviation": comparison["max_boundary_deviation"],
                "v1_directions": [
                    {
                        "id": contour.contour_id,
                        "direction": contour.direction,
                        "source": contour.direction_source,
                        "corrected": contour.orientation_corrected,
                    }
                    for contour in v1.contours
                ],
                "derived_share_count": len(v1.vertex_shares),
                "v2_share_count": len(v2.vertex_shares),
                "downgrade_code": downgrade.code,
                "downgrade_affected": downgrade.affected,
                "tolerance": comparison["tolerance"],
            }
            compatibility.append(item)
            all_ok = all_ok and comparison["consistent"]
            all_ok = all_ok and downgrade.code == E_VERSION_LOSS
            trace.emit("compatibility_checked", **item)
        report["checks"]["compatibility"] = compatibility

        old_intersection = intersect_segments(
            Segment.line(Point(20, 0), Point(20, 20)),
            Segment.line(Point(20.004, 0), Point(20.004, 20)),
            0.001,
        )
        new_intersection = intersect_segments(
            Segment.line(Point(20, 0), Point(20, 20)),
            Segment.line(Point(20.004, 0), Point(20.004, 20)),
            0.01,
        )
        difference = compare_intersection_results(
            old_intersection,
            new_intersection,
        )
        report["checks"]["tolerance_intersection"] = {
            "old": old_intersection.to_dict(),
            "new": new_intersection.to_dict(),
            "difference": difference,
        }
        trace.emit(
            "intersection_tolerance_compared",
            old_tolerance=0.001,
            new_tolerance=0.01,
            old_status=old_intersection.status,
            new_status=new_intersection.status,
            difference=difference,
        )
        all_ok = all_ok and difference["status_changed"]

        near_a = load_path(sample_root / "v1" / "near_a.json")
        near_b = load_path(sample_root / "v1" / "near_b.json")
        near_tight = boolean_paths(
            near_a,
            near_b,
            BooleanOptions("union", 0.001),
        )
        near_loose = boolean_paths(
            near_a,
            near_b,
            BooleanOptions("union", 0.01),
        )
        near_check = {
            "tight_tolerance": 0.001,
            "loose_tolerance": 0.01,
            "tight_loops": near_tight.report["loop_count"],
            "loose_loops": near_loose.report["loop_count"],
            "tight_area": near_tight.report["area"],
            "loose_area": near_loose.report["area"],
        }
        report["checks"]["tolerance_boolean"] = near_check
        all_ok = all_ok and (
            near_check["tight_loops"] == 2 and near_check["loose_loops"] == 1
        )
        trace.emit("boolean_tolerance_compared", **near_check)

        service = BooleanService()
        thresholds = ErrorThresholds()
        last_record = None
        last_inputs = None
        last_operation = None
        for version in ("v1", "v2"):
            path_a = load_path(sample_root / version / "base_a.json")
            path_b = load_path(sample_root / version / "base_b.json")
            # v1 has no declared tolerance; use the same operational value for
            # both versions so the comparison isolates interpretation changes.
            tolerance = 0.01
            for operation in sorted(OPERATIONS):
                try:
                    record = service.run_sync(
                        path_a,
                        path_b,
                        operation,
                        tolerance,
                        thresholds,
                    )
                    key = f"{version}:{operation}"
                    summary = _summary(record)
                    summary["preview_deterministic"] = _preview_deterministic(record)
                    report["operations"][key] = summary
                    last_record = record
                    last_inputs = (path_a, path_b)
                    last_operation = operation
                    trace.emit("boolean_verified", version=version, **summary)
                except PipelineError as exc:
                    all_ok = False
                    report["operations"][f"{version}:{operation}"] = {
                        "error": exc.to_dict()
                    }
                    trace.emit(
                        "boolean_failed",
                        version=version,
                        operation=operation,
                        error=exc.to_dict(),
                    )

        if last_record is not None:
            _, frame = service.store.snapshot()
            preview_path = artifacts / "preview.ppm"
            _write_ppm(preview_path, frame)
            trace.emit(
                "preview_persistence_checked",
                raster_hash=frame.raster_hash,
                result_hash=frame.relevant_result_hash,
                destination=str(preview_path),
            )
            project_path = artifacts / "acceptance.vproj"
            document = save_project(
                project_path,
                last_inputs[0],
                last_inputs[1],
                last_operation,
                last_record.tolerance,
                {"acceptance": True, "source_result": last_record.result_id},
                thresholds.to_dict(),
                last_record,
            )
            loaded = load_project(project_path)
            restored = BooleanService().restore_project(loaded)
            report["checks"]["project"] = {
                "saved": True,
                "content_hash": document["content_hash"],
                "loaded_operation": loaded.operation,
                "restored_result_hash": (
                    restored.result_hash if restored is not None else None
                ),
                "matches_last_committed": (
                    restored is not None
                    and restored.result_hash == last_record.result_hash
                ),
            }
            all_ok = all_ok and bool(
                restored and restored.result_hash == last_record.result_hash
            )
            trace.emit("project_restored", **report["checks"]["project"])
            _assert_project_tamper_detected(project_path, artifacts)
            report["checks"]["project_tamper"] = "E_PROJECT_MISMATCH"
            trace.emit("project_tamper_detected", code=E_PROJECT_MISMATCH)
            _assert_preview_stale(service, last_record.result_hash)
            report["checks"]["preview_stale"] = E_PREVIEW_STALE
            trace.emit("preview_stale_detected", code=E_PREVIEW_STALE)
        else:
            all_ok = False
        trace.emit("acceptance_finished", all_ok=all_ok)
    report["status"] = "ok" if all_ok else "failed"
    _atomic_write_bytes(
        report_path,
        (
            json.dumps(
                report,
                ensure_ascii=False,
                indent=2,
                sort_keys=True,
                allow_nan=False,
            )
            + "\n"
        ).encode("utf-8"),
    )
    print(
        json.dumps(
            {
                "status": report["status"],
                "trace": str(trace_path),
                "report": str(report_path),
            },
            ensure_ascii=False,
            indent=2,
            sort_keys=True,
        )
    )
    return 0 if all_ok else 1


def _preview_deterministic(record: Any) -> bool:
    from .raster import RasterSpec

    metrics_spec = record.metrics.raster_spec
    bounds_data = metrics_spec["bounds"]
    from .model import Bounds

    spec = RasterSpec(
        width=int(metrics_spec["width"]),
        height=int(metrics_spec["height"]),
        bounds=Bounds(
            float(bounds_data["min_x"]),
            float(bounds_data["min_y"]),
            float(bounds_data["max_x"]),
            float(bounds_data["max_y"]),
        ),
    )
    first = rasterize_path(record.path, spec, record.tolerance)
    second = rasterize_path(record.path, spec, record.tolerance)
    return first.rgba == second.rgba and first.raster_hash == second.raster_hash


def _assert_project_tamper_detected(project_path: Path, artifacts: Path) -> None:
    tampered = artifacts / "tampered.vproj"
    shutil.copyfile(project_path, tampered)
    document = json.loads(tampered.read_text(encoding="utf-8"))
    document["payload"]["tolerance"] = float(document["payload"]["tolerance"]) + 1.0
    _atomic_write_bytes(
        tampered,
        (json.dumps(document, ensure_ascii=False, sort_keys=True) + "\n").encode("utf-8"),
    )
    try:
        load_project(tampered)
    except PipelineError as exc:
        if exc.code == E_PROJECT_MISMATCH:
            return
        raise
    raise AssertionError("tampered project was accepted")


def _assert_preview_stale(service: BooleanService, result_hash: str) -> None:
    latest, _ = service.store.snapshot()
    if latest.result_hash != result_hash:
        from .errors import E_PREVIEW_STALE as _code

        raise AssertionError("unexpected latest result")
    # A committed result is readable, but an unrelated binding must be rejected.
    try:
        service.store.read_preview("sha256:not-in-committed-manifest")
    except PipelineError as exc:
        if exc.code == E_PREVIEW_STALE:
            return
        raise
    raise AssertionError("stale preview binding was accepted")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="vpipe",
        description="Deterministic vector-path boolean pipeline",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)

    samples = subparsers.add_parser("generate-samples")
    samples.add_argument("--root", default=DEFAULT_SAMPLES)
    samples.set_defaults(func=command_generate_samples)

    run = subparsers.add_parser("run")
    run.add_argument("--a", required=True)
    run.add_argument("--b", required=True)
    run.add_argument("--operation", required=True, choices=sorted(OPERATIONS))
    run.add_argument("--tolerance", type=float)
    run.add_argument("--samples", default=DEFAULT_SAMPLES)
    run.add_argument("--log", default=str(Path(DEFAULT_ARTIFACTS) / "run_trace.jsonl"))
    run.add_argument("--out-result")
    run.add_argument("--export-version", type=int, choices=(1, 2))
    run.add_argument("--project")
    run.add_argument("--preview")
    run.add_argument("--max-area-error", type=float, default=0.02)
    run.add_argument("--max-boundary-error", type=float, default=1.0)
    run.set_defaults(func=command_run)

    intersect = subparsers.add_parser("intersect")
    intersect.add_argument("--tolerance", type=float, required=True)
    intersect.add_argument("--a1", type=float, required=True)
    intersect.add_argument("--a2", type=float, required=True)
    intersect.add_argument("--b1", type=float, required=True)
    intersect.add_argument("--b2", type=float, required=True)
    intersect.add_argument("--c1", type=float, required=True)
    intersect.add_argument("--c2", type=float, required=True)
    intersect.add_argument("--d1", type=float, required=True)
    intersect.add_argument("--d2", type=float, required=True)
    intersect.set_defaults(func=command_intersect)

    acceptance = subparsers.add_parser("acceptance")
    acceptance.add_argument("--samples", default=DEFAULT_SAMPLES)
    acceptance.add_argument("--artifacts", default=DEFAULT_ARTIFACTS)
    acceptance.set_defaults(func=command_acceptance)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except PipelineError as exc:
        print(json.dumps(exc.to_dict(), ensure_ascii=False, indent=2, sort_keys=True))
        return 2


if __name__ == "__main__":
    raise SystemExit(main())

