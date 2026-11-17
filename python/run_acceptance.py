#!/usr/bin/env python3
"""End-to-end deterministic acceptance test for the mosaic validator."""

from __future__ import annotations

import copy
import hashlib
import json
import os
import pathlib
import shutil
import subprocess
import sys
import time
from collections import defaultdict
from dataclasses import dataclass

import numpy as np


ROOT = pathlib.Path(__file__).resolve().parents[1]
BUILD = ROOT / "build"
BINARY = BUILD / "mosaic-validator"
CORE_TEST = BUILD / "mosaic_core_tests"
SCENES = ROOT / "samples"
WORK = ROOT / "artifacts" / "acceptance"


def run(
    args: list[str],
    *,
    expect: int = 0,
    timeout: float = 60.0,
    check: bool = True,
) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(
        args,
        cwd=ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=timeout,
    )
    if check and result.returncode != expect:
        raise AssertionError(
            f"command failed ({result.returncode}, expected {expect}): {' '.join(args)}\n"
            f"stdout:\n{result.stdout}\nstderr:\n{result.stderr}"
        )
    return result


def render(scene: pathlib.Path, output: pathlib.Path, *extra: str, expect: int = 0) -> subprocess.CompletedProcess[str]:
    return run(
        [
            str(BINARY),
            "render",
            "--scene",
            str(scene.relative_to(ROOT)),
            "--out",
            str(output.relative_to(ROOT)),
            *extra,
        ],
        expect=expect,
    )


def verify(left: pathlib.Path, right: pathlib.Path, report: pathlib.Path, expect: int = 0) -> dict:
    result = run(
        [
            str(BINARY),
            "verify",
            "--a",
            str(left.relative_to(ROOT)),
            "--b",
            str(right.relative_to(ROOT)),
            "--report",
            str(report.relative_to(ROOT)),
        ],
        expect=expect,
        check=False,
    )
    if result.returncode != expect:
        raise AssertionError(
            f"verify returned {result.returncode}, expected {expect}\n"
            f"stdout:\n{result.stdout}\nstderr:\n{result.stderr}"
        )
    return json.loads(report.read_text(encoding="utf-8"))


def sha_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while chunk := stream.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def load_array(directory: pathlib.Path, name: str, dtype: str, shape: tuple[int, ...]) -> np.ndarray:
    return np.fromfile(directory / name, dtype=dtype).reshape(shape)


def owner_layer(key: int) -> int:
    if key == 0 or key == np.iinfo(np.uint64).max:
        return -1
    return int((int(key) >> 32) & 0x7FFFFFFF)


@dataclass
class CaseResult:
    name: str
    passed: bool
    detail: str


def validate_error_cases() -> list[CaseResult]:
    base = json.loads((SCENES / "church_floor.json").read_text(encoding="utf-8"))
    cases: list[tuple[str, dict, str, str]] = []

    scene = copy.deepcopy(base)
    scene["pieces"][0]["triangles"][0][1] = 999
    cases.append(
        (
            "mesh_index_out_of_range",
            scene,
            "MOSAIC_MESH_INDEX_OUT_OF_RANGE",
            "scene.pieces[0].triangles[0][1]",
        )
    )

    scene = copy.deepcopy(base)
    scene["pieces"][0]["triangles"][0] = [0, 0, 1]
    cases.append(
        (
            "degenerate_triangle",
            scene,
            "MOSAIC_TRIANGLE_DEGENERATE",
            "scene.pieces[0].triangles[0]",
        )
    )

    scene = copy.deepcopy(base)
    scene["pieces"][0]["transmission"] = [1.2, 0.5, 0.5]
    cases.append(
        (
            "transmission_out_of_range",
            scene,
            "MOSAIC_TRANSMISSION_OUT_OF_RANGE",
            "scene.pieces[0].transmission",
        )
    )

    scene = copy.deepcopy(base)
    scene["pieces"][0]["edgeIor"] = 0.9
    cases.append(
        (
            "ior_out_of_range",
            scene,
            "MOSAIC_IOR_OUT_OF_RANGE",
            "scene.pieces[0].edgeIor",
        )
    )

    scene = copy.deepcopy(base)
    duplicate = copy.deepcopy(scene["pieces"][0])
    scene["pieces"].append(duplicate)
    cases.append(
        (
            "duplicate_piece_key",
            scene,
            "MOSAIC_PIECE_KEY_DUPLICATE",
            f"scene.pieces[{len(scene['pieces']) - 1}]",
        )
    )

    scene = copy.deepcopy(base)
    del scene["pieces"][0]["layerIndex"]
    cases.append(
        (
            "missing_layer_index",
            scene,
            "MOSAIC_FIELD_MISSING",
            "scene.pieces[0].layerIndex",
        )
    )

    scene = copy.deepcopy(base)
    scene["lights"][0]["position"] = [0.0, -0.08, 0.0]
    cases.append(
        (
            "light_inside_floor",
            scene,
            "MOSAIC_LIGHT_INSIDE_FLOOR",
            "scene.lights[0].position",
        )
    )

    scene = copy.deepcopy(base)
    scene["camera"]["fovYDeg"] = 180.0
    cases.append(
        (
            "invalid_camera_fov",
            scene,
            "MOSAIC_CAMERA_INVALID",
            "scene.camera.fovYDeg",
        )
    )

    results: list[CaseResult] = []
    invalid_root = WORK / "invalid"
    invalid_root.mkdir(parents=True, exist_ok=True)
    for name, scene, code, path in cases:
        scene_path = invalid_root / f"{name}.json"
        out_path = invalid_root / f"{name}.out"
        shutil.rmtree(out_path, ignore_errors=True)
        scene_path.write_text(json.dumps(scene), encoding="utf-8")
        completed = render(scene_path, out_path, expect=20)
        errors_path = out_path / "validation_errors.json"
        if not errors_path.exists():
            results.append(CaseResult(name, False, "validation_errors.json was not written"))
            continue
        payload = json.loads(errors_path.read_text(encoding="utf-8"))
        found = any(
            error.get("code") == code and error.get("path") == path
            for error in payload.get("errors", [])
        )
        results.append(
            CaseResult(
                name,
                found and completed.returncode == 20,
                f"return={completed.returncode}, expected {code} at {path}",
            )
        )
    return results


def image_checks(full: pathlib.Path, scene_data: dict) -> tuple[list[CaseResult], dict]:
    width = scene_data["settings"]["image"]["width"]
    height = scene_data["settings"]["image"]["height"]
    image = load_array(full, "image.rgba", np.uint8, (height, width, 4))
    owners = load_array(full, "ownership.u64", "<u8", (height, width))
    gray = load_array(full, "caustic_gray.u16", "<u2", (height, width))
    report = json.loads((full / "report.json").read_text(encoding="utf-8"))
    caustics = json.loads((full / "caustics.json").read_text(encoding="utf-8"))
    results: list[CaseResult] = []

    alpha_ok = bool(np.all(image[..., 3] == 255))
    results.append(CaseResult("premultiplied_rgba_alpha", alpha_ok, f"alpha={np.unique(image[..., 3]).tolist()}"))

    white_fraction = float(np.mean(np.all(image[..., :3] >= 254, axis=-1)))
    results.append(
        CaseResult(
            "no_blank_white_overexposure",
            white_fraction == 0.0,
            f"near-white fraction={white_fraction:.8f}",
        )
    )

    layers = defaultdict(list)
    for key in np.unique(owners):
        layer = owner_layer(int(key))
        if layer >= 0:
            layers[layer].append(int(key))
    layers_visible = all(len(layers[layer]) >= 2 for layer in (0, 1, 2))
    results.append(
        CaseResult(
            "all_layers_own_pixels",
            layers_visible,
            " / ".join(f"L{layer}:{len(layers[layer])}" for layer in sorted(layers)),
        )
    )

    piece_means: dict[int, np.ndarray] = {}
    for key in np.unique(owners):
        if owner_layer(int(key)) < 0:
            continue
        mask = owners == key
        piece_means[int(key)] = image[..., :3][mask].mean(axis=0)

    scene_pieces = {}
    for piece in scene_data["pieces"]:
        key = (1 << 63) | (piece["layerIndex"] << 32) | piece["pieceId"]
        center = np.asarray(piece["vertices"], dtype=np.float64).mean(axis=0)
        scene_pieces[key] = (piece["layerIndex"], center, piece["transmission"])

    adjacent_differences = []
    for key, (layer, center, _) in scene_pieces.items():
        if key not in piece_means:
            continue
        for other, (other_layer, other_center, _) in scene_pieces.items():
            if other <= key or other_layer != layer or other not in piece_means:
                continue
            distance = float(np.linalg.norm(center[[0, 2]] - other_center[[0, 2]]))
            if distance <= 1.02:
                adjacent_differences.append(
                    float(np.sqrt(np.mean((piece_means[key] - piece_means[other]) ** 2)))
                )
    adjacent_ok = bool(adjacent_differences) and min(adjacent_differences) > 0.7
    results.append(
        CaseResult(
            "same_color_adjacent_pieces_distinct",
            adjacent_ok,
            f"min RMS={min(adjacent_differences, default=0.0):.3f}/255 across {len(adjacent_differences)} pairs",
        )
    )

    saturation_values = []
    peak_values = []
    for mean in piece_means.values():
        maximum = float(mean.max())
        minimum = float(mean.min())
        if maximum > 0.0:
            saturation_values.append((maximum - minimum) / maximum)
        peak_values.append(maximum)
    hue_ok = bool(saturation_values) and min(saturation_values) > 0.05 and max(peak_values) < 252.0
    results.append(
        CaseResult(
            "side_piece_hue_retained",
            hue_ok,
            f"min saturation={min(saturation_values, default=0.0):.4f}, max mean channel={max(peak_values, default=0.0):.1f}",
        )
    )

    layer_stats = {entry["layerIndex"]: entry for entry in report["layers"]}
    fragments_ok = all(layer_stats[layer]["visibleFragments"] > 0 for layer in (0, 1, 2))
    results.append(
        CaseResult(
            "layer_fragment_statistics",
            fragments_ok,
            ", ".join(
                f"L{layer}={layer_stats[layer]['visibleFragments']}"
                for layer in sorted(layer_stats)
            ),
        )
    )

    proportions_ok = all(
        layer_stats[layer]["transmissionRatio"] + layer_stats[layer]["reflectionRatio"] > 0.999
        for layer in layer_stats
    )
    results.append(
        CaseResult(
            "transmission_reflection_proportions",
            proportions_ok,
            "ratios sum to 1 per layer",
        )
    )

    widths = [sample["penumbraWidthPixels"] for sample in report["penumbraTrend"]["samples"]]
    widths_ok = len(widths) >= 2 and all(right > left for left, right in zip(widths, widths[1:]))
    results.append(
        CaseResult(
            "penumbra_width_trend",
            widths_ok,
            "widths=" + ",".join(f"{value:.6f}" for value in widths),
        )
    )

    stripe_ok = report["caustics"]["stripeCount"] >= 2 and int(gray.max()) > 1000
    results.append(
        CaseResult(
            "visible_caustic_stripes",
            stripe_ok,
            f"stripes={report['caustics']['stripeCount']}, maxGray={int(gray.max())}",
        )
    )

    fallback_records = [
        record
        for record in caustics["records"]
        if record["targetKind"] == "floor" and not record["outerGeometryHit"]
    ]
    fallback_ok = False
    lit_fallback = 0
    for record in fallback_records:
        x, y = record["screen"]
        if 0 <= x < width and 0 <= y < height:
            if int(gray[int(round(y)), int(round(x))]) > 0:
                lit_fallback += 1
    fallback_ok = len(fallback_records) > 0 and lit_fallback > 0
    results.append(
        CaseResult(
            "outer_miss_floor_fallback_is_lit",
            fallback_ok,
            f"fallback records={len(fallback_records)}, lit probes={lit_fallback}",
        )
    )

    report_text = (full / "report.json").read_text(encoding="utf-8")
    recovery_text = (full / "recovery.json").read_text(encoding="utf-8")
    path_free = (
        str(ROOT) not in report_text
        and str(ROOT) not in recovery_text
        and "timestamp" not in report_text.lower()
        and "timestamp" not in recovery_text.lower()
        and "2026-" not in report_text
        and "2026-" not in recovery_text
    )
    results.append(CaseResult("stable_report_no_time_or_abs_path", path_free, "checked report/recovery"))

    summary = {
        "imageSha256": sha_file(full / "image.rgba"),
        "ownershipSha256": sha_file(full / "ownership.u64"),
        "causticGraySha256": sha_file(full / "caustic_gray.u16"),
        "stripeCount": report["caustics"]["stripeCount"],
        "causticRecords": report["caustics"]["recordCount"],
        "penumbraWidths": widths,
        "layerVisibleFragments": {
            str(layer): layer_stats[layer]["visibleFragments"] for layer in sorted(layer_stats)
        },
        "whiteFraction": white_fraction,
        "minAdjacentRms": min(adjacent_differences, default=0.0),
    }
    return results, summary


def force_kill_test(scene: pathlib.Path) -> tuple[CaseResult, dict]:
    output = WORK / "kill"
    shutil.rmtree(output, ignore_errors=True)
    process = subprocess.Popen(
        [
            str(BINARY),
            "render",
            "--scene",
            str(scene.relative_to(ROOT)),
            "--out",
            str(output.relative_to(ROOT)),
            "--chunk-delay-ms",
            "160",
        ],
        cwd=ROOT,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    snapshot = output / "snapshot" / "snapshot.json"
    completed = 0
    deadline = time.time() + 20.0
    while time.time() < deadline:
        if snapshot.exists():
            try:
                payload = json.loads(snapshot.read_text(encoding="utf-8"))
                completed = int(payload.get("completedCount", 0))
                if completed >= 5:
                    break
            except (json.JSONDecodeError, OSError):
                pass
        if process.poll() is not None:
            break
        time.sleep(0.02)
    process.kill()
    process.wait(timeout=5.0)
    before = json.loads(snapshot.read_text(encoding="utf-8"))
    render(scene, output)
    recovery = json.loads((output / "recovery.json").read_text(encoding="utf-8"))
    passed = (
        completed >= 5
        and before["state"] == "in-progress"
        and recovery["snapshotLoaded"]
        and recovery["reusedChunks"] >= completed
        and recovery["resumeCount"] >= 1
    )
    return (
        CaseResult(
            "sigkill_resume",
            passed,
            f"committed={completed}, killed={process.returncode}, reused={recovery['reusedChunks']}",
        ),
        recovery,
    )


def main() -> int:
    if not BINARY.exists():
        run(["cmake", "-S", ".", "-B", "build", "-DCMAKE_BUILD_TYPE=Release"])
        run(["cmake", "--build", "build", "-j", "8"])
    run([str(CORE_TEST)])
    run([sys.executable, "python/generate_scene.py"])

    WORK.mkdir(parents=True, exist_ok=True)
    scene_path = SCENES / "church_floor.json"
    shuffled_path = SCENES / "church_floor_shuffled.json"
    full = WORK / "full"
    repeat = WORK / "repeat"
    shuffled = WORK / "shuffled"
    graceful = WORK / "graceful"
    corrupt = WORK / "corrupt"
    mismatch = WORK / "mismatch"
    thick = WORK / "thick"
    reversed_run = WORK / "reversed"
    for directory in (full, repeat, shuffled, graceful, corrupt, mismatch, thick, reversed_run):
        shutil.rmtree(directory, ignore_errors=True)

    cases: list[CaseResult] = []
    render(scene_path, full)
    render(scene_path, repeat)
    full_scene = json.loads(scene_path.read_text(encoding="utf-8"))
    image_cases, summary = image_checks(full, full_scene)
    cases.extend(image_cases)

    equal = verify(full, repeat, WORK / "full_vs_repeat.json")
    cases.append(CaseResult("clean_run_repeatability", equal["status"] == "equal", equal["status"]))
    report_equal = (full / "report.json").read_bytes() == (repeat / "report.json").read_bytes()
    cases.append(
        CaseResult(
            "stable_report_byte_order",
            report_equal,
            "report.json is byte-identical across clean runs",
        )
    )

    render(shuffled_path, shuffled)
    shuffled_equal = verify(full, shuffled, WORK / "full_vs_shuffled.json")
    texture_equal = (full / "texture_table.bin").read_bytes() == (
        shuffled / "texture_table.bin"
    ).read_bytes()
    cases.append(
        CaseResult(
            "piece_and_light_order_independence",
            shuffled_equal["status"] == "equal" and texture_equal,
            f"verify={shuffled_equal['status']}, textureEqual={texture_equal}",
        )
    )

    graceful_result = render(
        scene_path,
        graceful,
        "--interrupt-after-chunks",
        "5",
    )
    interrupted_report = json.loads((graceful / "recovery.json").read_text(encoding="utf-8"))
    snapshot_before = json.loads(
        (graceful / "snapshot" / "snapshot.json").read_text(encoding="utf-8")
    )
    render(scene_path, graceful)
    graceful_recovery = json.loads((graceful / "recovery.json").read_text(encoding="utf-8"))
    graceful_equal = verify(full, graceful, WORK / "full_vs_graceful.json")
    cases.append(
        CaseResult(
            "graceful_interrupt_resume",
            graceful_result.returncode == 0
            and interrupted_report["outcome"] == "interrupted"
            and snapshot_before["completedCount"] == 5
            and graceful_recovery["reusedChunks"] == 5
            and graceful_recovery["resumeCount"] == 1
            and graceful_equal["status"] == "equal",
            f"interruptExit={graceful_result.returncode}, resumedReuse={graceful_recovery['reusedChunks']}",
        )
    )

    kill_case, _ = force_kill_test(scene_path)
    cases.append(kill_case)
    kill_equal = verify(full, WORK / "kill", WORK / "full_vs_kill.json")
    cases.append(CaseResult("sigkill_final_equality", kill_equal["status"] == "equal", kill_equal["status"]))

    shutil.copytree(full, corrupt)
    chunk_path = corrupt / "snapshot" / "chunks" / "chunk_000004.bin"
    corrupted = bytearray(chunk_path.read_bytes())
    corrupted[40] ^= 0x5A
    chunk_path.write_bytes(corrupted)
    render(scene_path, corrupt)
    corrupt_recovery = json.loads((corrupt / "recovery.json").read_text(encoding="utf-8"))
    corrupt_equal = verify(full, corrupt, WORK / "full_vs_corrupt.json")
    cases.append(
        CaseResult(
            "corrupt_snapshot_full_restart",
            corrupt_recovery["snapshotDiscarded"]
            and corrupt_recovery["reusedChunks"] == 0
            and corrupt_recovery["recomputedChunks"] == 25
            and corrupt_recovery["resumeCount"] == 1
            and corrupt_equal["status"] == "equal",
            f"reason={corrupt_recovery['discardReason']}, reused={corrupt_recovery['reusedChunks']}",
        )
    )

    manifest_corrupt = WORK / "manifest_corrupt"
    shutil.rmtree(manifest_corrupt, ignore_errors=True)
    shutil.copytree(full, manifest_corrupt)
    (manifest_corrupt / "snapshot" / "snapshot.json").write_text(
        "{ this is not valid json",
        encoding="utf-8",
    )
    render(scene_path, manifest_corrupt)
    manifest_recovery = json.loads(
        (manifest_corrupt / "recovery.json").read_text(encoding="utf-8")
    )
    manifest_equal = verify(full, manifest_corrupt, WORK / "full_vs_manifest_corrupt.json")
    cases.append(
        CaseResult(
            "corrupt_manifest_full_restart",
            manifest_recovery["snapshotDiscarded"]
            and manifest_recovery["discardReason"] == "snapshot-parse-error"
            and manifest_recovery["reusedChunks"] == 0
            and manifest_equal["status"] == "equal",
            f"reason={manifest_recovery['discardReason']}, reused={manifest_recovery['reusedChunks']}",
        )
    )

    mismatched_scene = copy.deepcopy(full_scene)
    mismatched_scene["seed"] += 1
    mismatch_deep = copy.deepcopy(mismatched_scene)
    for piece in mismatch_deep["pieces"]:
        piece["transmission"] = [
            min(1.0, piece["transmission"][0] + 0.01),
            piece["transmission"][1],
            piece["transmission"][2],
        ]
    mismatch_scene_path = WORK / "mismatch_scene.json"
    mismatch_scene_path.write_text(json.dumps(mismatch_deep), encoding="utf-8")
    shutil.copytree(full, mismatch)
    render(mismatch_scene_path, mismatch)
    mismatch_recovery = json.loads((mismatch / "recovery.json").read_text(encoding="utf-8"))
    mismatch_reference = WORK / "mismatch_reference"
    shutil.rmtree(mismatch_reference, ignore_errors=True)
    render(mismatch_scene_path, mismatch_reference)
    mismatch_equal = verify(mismatch_reference, mismatch, WORK / "mismatch_equal.json")
    cases.append(
        CaseResult(
            "scene_digest_mismatch_discards",
            mismatch_recovery["discardReason"] == "scene-digest-mismatch"
            and mismatch_recovery["reusedChunks"] == 0
            and mismatch_recovery["resumeCount"] == 1
            and mismatch_equal["status"] == "equal",
            f"reason={mismatch_recovery['discardReason']}, reused={mismatch_recovery['reusedChunks']}",
        )
    )

    thick_scene = copy.deepcopy(full_scene)
    for piece in thick_scene["pieces"]:
        if piece["layerIndex"] == 0:
            piece["thickness"] = round(float(piece["thickness"]) + 0.01, 9)
    thick_scene_path = WORK / "thick_scene.json"
    thick_scene_path.write_text(json.dumps(thick_scene), encoding="utf-8")
    render(thick_scene_path, thick)
    base_caustics = json.loads((full / "caustics.json").read_text(encoding="utf-8"))["records"]
    thick_caustics = json.loads((thick / "caustics.json").read_text(encoding="utf-8"))["records"]
    displacement = 0.0
    for left, right in zip(base_caustics, thick_caustics):
        if left["sourceLayer"] == 0 and left["targetKind"] == "floor":
            displacement = max(
                displacement,
                float(np.linalg.norm(np.asarray(left["target"]) - np.asarray(right["target"]))),
            )
    cases.append(
        CaseResult(
            "thickness_shifts_projection",
            displacement > 1.0e-4,
            f"max projection shift={displacement:.6f}",
        )
    )

    reversed_scene = copy.deepcopy(full_scene)
    reversed_scene["settings"]["compositionOrder"] = ["reflection", "transmission"]
    reversed_path = WORK / "reversed_scene.json"
    reversed_path.write_text(json.dumps(reversed_scene), encoding="utf-8")
    render(reversed_path, reversed_run)
    reversed_difference = verify(full, reversed_run, WORK / "full_vs_reversed.json", expect=40)
    difference_entries = json.loads(
        (WORK / "full_vs_reversed.json").read_text(encoding="utf-8")
    )["differences"]
    structured_difference = bool(difference_entries) and all(
        {"chunk", "x", "y", "channel"} <= set(entry)
        for entry in difference_entries[:32]
    )
    reversed_image = load_array(
        reversed_run,
        "image.rgba",
        np.uint8,
        (full_scene["settings"]["image"]["height"], full_scene["settings"]["image"]["width"], 4),
    )
    cases.append(
        CaseResult(
            "declared_composition_order_changes_output",
            reversed_difference["status"] == "different"
            and reversed_difference["differenceCount"] > 0
            and structured_difference
            and bool(np.all(reversed_image[..., 3] == 255)),
            f"differences={reversed_difference['differenceCount']}",
        )
    )

    cases.extend(validate_error_cases())

    passed = all(case.passed for case in cases)
    result = {
        "format": "mosaic-acceptance-result",
        "version": 1,
        "passed": passed,
        "caseCount": len(cases),
        "passedCount": sum(case.passed for case in cases),
        "cases": [
            {"name": case.name, "passed": case.passed, "detail": case.detail}
            for case in cases
        ],
        "summary": summary,
    }
    (WORK / "acceptance_result.json").write_text(
        json.dumps(result, indent=2) + "\n",
        encoding="utf-8",
    )
    print(json.dumps(result, indent=2))
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
