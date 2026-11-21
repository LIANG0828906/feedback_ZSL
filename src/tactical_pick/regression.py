"""Scripted deterministic regression and concurrency verification."""

from __future__ import annotations

import json
import shutil
import threading
import time
from pathlib import Path
from typing import Any

from .camera import Camera
from .editor import SceneEditor
from .errors import ToolError, fail
from .generator import CASES, _build_initial_scene, load_manifest
from .injection import injection_selfcheck
from .query import PickRequest, Picker
from .region import PlacementRequest, RegionJudge
from .scene import read_scene
from .snapshot import SceneManager
from .trace import TraceLog, replay_trace
from .util import canonical_json, sha256_hex, write_json


def _load_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail("E_SCENE_INVALID", f"cannot load {path.name}: {exc}", f"samples.{path.name}")
    if not isinstance(value, dict):
        fail("E_SCENE_INVALID", f"{path.name} must be an object", f"samples.{path.name}")
    return value


def _request_from_document(document: dict[str, Any]) -> PickRequest:
    return PickRequest(
        screen=(float(document["screen"][0]), float(document["screen"][1])),
        camera=Camera.from_dict(document["camera"]),
        requested_version=document.get("requested_version"),
    )


def _region_from_document(document: dict[str, Any]) -> PlacementRequest:
    return PlacementRequest(
        polygon=tuple((float(point[0]), float(point[1])) for point in document["polygon"]),
        requested_version=document.get("requested_version"),
        height=float(document.get("height", 2.0)),
    )


def _expect_error(callable_: Any, expected_code: str, point: str) -> str:
    try:
        callable_()
    except ToolError as exc:
        if exc.code != expected_code:
            fail(
                "E_CHECK_FAILED",
                f"expected {expected_code}, got {exc.code}",
                point,
                first_conflict=exc.to_dict(),
            )
        return exc.code
    fail("E_CHECK_FAILED", f"expected {expected_code}, but no error was raised", point)


def _assert_equal(actual: Any, expected: Any, point: str) -> None:
    if isinstance(actual, bytes) or isinstance(expected, bytes):
        if actual == expected:
            return
        fail(
            "E_CHECK_FAILED",
            "regression byte stream does not match generated baseline",
            point,
            first_conflict={
                "expected_sha256": sha256_hex(expected if isinstance(expected, bytes) else canonical_json(expected).encode()),
                "actual_sha256": sha256_hex(actual if isinstance(actual, bytes) else canonical_json(actual).encode()),
            },
        )
    actual_doc = canonical_json(actual)
    expected_doc = canonical_json(expected)
    if actual_doc != expected_doc:
        fail(
            "E_CHECK_FAILED",
            "regression result does not match generated baseline",
            point,
            first_conflict={"expected": json.loads(expected_doc), "actual": json.loads(actual_doc)},
        )


def _run_case(
    sample_root: Path,
    case_entry: dict[str, Any],
) -> tuple[dict[str, Any], bytes]:
    case_dir = sample_root / case_entry["directory"]
    case_document = _load_json(sample_root / case_entry["case_file"])
    baseline_document = _load_json(sample_root / case_entry["baseline_file"])
    initial = read_scene(case_dir / case_document["initial_file"])
    if initial.compute_hash() != case_document["queries"][0]["hash"]:
        fail(
            "E_CHECK_FAILED",
            "initial scene file hash differs from sample metadata",
            f"case.{case_document['name']}.initial_scene",
        )

    manager = SceneManager(initial)
    editor = SceneEditor(manager)
    picker = Picker(manager)
    judge = RegionJudge(manager)
    trace = TraceLog()
    versions_report: list[dict[str, Any]] = []
    isolation_checks: list[dict[str, Any]] = []
    total_picks = 0
    total_regions = 0

    queries = case_document["queries"]
    baselines = baseline_document["versions"]
    if len(queries) != len(baselines):
        fail("E_CHECK_FAILED", "sample query and baseline version counts differ", f"case.{case_document['name']}")

    for index, query_version in enumerate(queries):
        if index > 0:
            editor.apply_batch(case_document["batches"][index - 1])
        snapshot = manager.current
        version = snapshot.version
        if version != query_version["version"]:
            fail(
                "E_CHECK_FAILED",
                "published version sequence differs from sample input",
                f"case.{case_document['name']}.version",
                first_conflict={"expected": query_version["version"], "actual": version},
            )
        if snapshot.content_hash != query_version["hash"]:
            fail(
                "E_CHECK_FAILED",
                "published content hash differs from sample input",
                f"case.{case_document['name']}.hash",
                first_conflict={"expected": query_version["hash"], "actual": snapshot.content_hash},
            )

        scene_entry = next(item for item in case_document["scene_files"] if item["version"] == version)
        saved_scene = read_scene(case_dir / scene_entry["file"])
        if saved_scene.compute_hash() != snapshot.content_hash:
            fail(
                "E_CHECK_FAILED",
                "editor-produced scene does not match its persisted binary",
                f"case.{case_document['name']}.scene_v{version}",
            )

        baseline_version = baselines[index]
        if baseline_version["version"] != version or baseline_version["hash"] != snapshot.content_hash:
            fail("E_CHECK_FAILED", "baseline version attribution mismatch", f"case.{case_document['name']}.baseline")

        pick_count = 0
        for definition, expected in zip(query_version["picks"], baseline_version["picks"], strict=True):
            request = _request_from_document(definition["request"])
            result = picker.pick(request)
            _assert_equal(result.to_dict(), expected["expected"], f"case.{case_document['name']}.v{version}.{definition['name']}")
            trace.append_pick(request, result)
            pick_count += 1
            total_picks += 1

        region_count = 0
        for definition, expected in zip(query_version["regions"], baseline_version["regions"], strict=True):
            request = _region_from_document(definition["request"])
            verdict = judge.judge(request)
            _assert_equal(verdict.to_dict(), expected["expected"], f"case.{case_document['name']}.v{version}.{definition['name']}")
            trace.append_region(request, verdict)
            region_count += 1
            total_regions += 1

        if index > 0:
            old_version = queries[index - 1]["version"]
            previous_request = _request_from_document(query_version["picks"][0]["request"])
            previous_request = PickRequest(
                screen=previous_request.screen,
                camera=previous_request.camera,
                requested_version=old_version,
            )
            code = _expect_error(
                lambda request=previous_request: picker.pick(request),
                "E_VERSION_STALE",
                f"case.{case_document['name']}.v{version}.stale_query",
            )
            isolation_checks.append(
                {"check": "stale_query", "current_version": version, "requested_version": old_version, "code": code}
            )

        handle = manager.acquire(version)
        handle.release()
        code = _expect_error(
            handle.snapshot,
            "E_STALE_HANDLE",
            f"case.{case_document['name']}.v{version}.stale_handle",
        )
        isolation_checks.append({"check": "released_handle", "version": version, "code": code})

        versions_report.append(
            {
                "version": version,
                "content_hash": snapshot.content_hash,
                "picks": pick_count,
                "regions": region_count,
            }
        )

    trace_bytes = trace.to_bytes()
    parsed_trace = TraceLog.from_bytes(trace_bytes, f"{case_document['name']}.jsonl")
    replay = replay_trace(parsed_trace, manager)
    _assert_equal(parsed_trace.to_bytes(), trace_bytes, f"case.{case_document['name']}.trace_roundtrip")

    report = {
        "name": case_document["name"],
        "seed": case_document["seed"],
        "versions": versions_report,
        "picks": total_picks,
        "regions": total_regions,
        "isolation": isolation_checks,
        "trace_sha256": sha256_hex(trace_bytes),
        "trace_records": len(parsed_trace.records),
        "replay": replay,
    }
    return report, trace_bytes


def concurrency_check(seed: int = 20261122) -> dict[str, Any]:
    spec = CASES[0]
    scene = _build_initial_scene(spec, seed + spec.seed_offset)
    manager = SceneManager(scene)
    editor = SceneEditor(manager)
    picker = Picker(manager)

    initial_unit = scene.units[0]
    lower, upper = initial_unit.bounds()
    target = (
        (lower[0] + upper[0]) * 0.5,
        (lower[1] + upper[1]) * 0.5,
        (lower[2] + upper[2]) * 0.5,
    )
    camera = Camera.look_at(
        (target[0] - 1.0, target[1] + 7.0, target[2] + 6.0),
        target,
        width=960.0,
        height=540.0,
    )
    request = PickRequest((camera.width * 0.5, camera.height * 0.5), camera)
    results: list[tuple[int, str]] = []
    errors: list[BaseException] = []
    writer_versions: list[int] = []
    stop = threading.Event()
    start_barrier = threading.Barrier(2)

    def writer() -> None:
        try:
            start_barrier.wait()
            for step in range(60):
                delta = [0.0, 0.0, 0.0]
                delta[2] = 0.012 if step % 2 == 0 else -0.012
                snapshot = editor.apply_batch(
                    [{"op": "move_unit", "unit_id": 101, "delta": delta}]
                )
                writer_versions.append(snapshot.version)
                time.sleep(0.001)
        except BaseException as exc:  # pragma: no cover - surfaced below
            errors.append(exc)
        finally:
            stop.set()

    def reader() -> None:
        try:
            start_barrier.wait()
            while not stop.is_set() or len(results) < 240:
                result = picker.pick(request)
                results.append((result.scene_version, result.scene_hash))
                if len(results) >= 1200:
                    break
        except BaseException as exc:  # pragma: no cover - surfaced below
            errors.append(exc)

    writer_thread = threading.Thread(target=writer, name="scene-writer")
    reader_thread = threading.Thread(target=reader, name="scene-reader")
    writer_thread.start()
    reader_thread.start()
    writer_thread.join()
    reader_thread.join()
    if errors:
        raise errors[0]

    archive = {item["version"]: item["content_hash"] for item in manager.archive_metadata()}
    torn = 0
    versions_seen: set[int] = set()
    for version, content_hash in results:
        versions_seen.add(version)
        if archive.get(version) != content_hash:
            torn += 1
    if torn:
        fail(
            "E_CHECK_FAILED",
            "concurrent readers observed a torn version/hash pair",
            "concurrency",
            first_conflict={"torn_reads": torn},
        )
    if len(versions_seen) < 2:
        fail(
            "E_CHECK_FAILED",
            "concurrency test did not observe overlapping versions",
            "concurrency.coverage",
            first_conflict={"versions_seen": sorted(versions_seen)},
        )
    return {
        "writer_versions": len(writer_versions),
        "reader_queries": len(results),
        "old_and_new_observed": min(versions_seen) < max(versions_seen),
        "torn_reads": torn,
        "final_version": manager.current.version,
    }


def run_regression(
    samples_root: str | Path,
    output_root: str | Path,
    seed: int = 20261122,
) -> dict[str, Any]:
    sample_root, manifest = load_manifest(samples_root, seed)
    injections = injection_selfcheck(seed)
    if not injections["ok"]:
        fail(
            "E_CHECK_FAILED",
            "input-entry fault injection self-check failed",
            "injection.selfcheck",
            first_conflict=injections,
        )
    case_reports: list[dict[str, Any]] = []
    traces: dict[str, bytes] = {}
    for case_entry in manifest["cases"]:
        report, trace_bytes = _run_case(sample_root, case_entry)
        case_reports.append(report)
        traces[report["name"]] = trace_bytes

    concurrency = concurrency_check(seed)
    report = {
        "schema": 1,
        "suite": "tactical_pick_local_regression",
        "seed": seed,
        "status": "pass",
        "cases": case_reports,
        "totals": {
            "cases": len(case_reports),
            "versions": sum(len(case["versions"]) for case in case_reports),
            "picks": sum(case["picks"] for case in case_reports),
            "regions": sum(case["regions"] for case in case_reports),
            "replayed_records": sum(case["replay"]["records"] for case in case_reports),
        },
        "concurrency": concurrency,
        "injections": injections,
        "artifacts": {},
    }

    output_path = Path(output_root) / f"seed_{seed}"
    if output_path.exists():
        shutil.rmtree(output_path)
    trace_dir = output_path / "traces"
    trace_dir.mkdir(parents=True)
    for name, data in traces.items():
        path = trace_dir / f"{name}.jsonl"
        path.write_bytes(data)
        report["artifacts"][f"traces/{name}.jsonl"] = sha256_hex(data)
    report_path = output_path / "report.json"
    write_json(report_path, report)
    report["artifacts"]["report.json"] = sha256_hex(report_path.read_bytes())
    # The report itself cannot contain its own hash without becoming unstable.
    report["artifacts"].pop("report.json")
    return report

