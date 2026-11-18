#!/usr/bin/env python3
"""Deterministic end-to-end checks for the offline coffered-ceiling rasterizer."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import signal
import struct
import subprocess
import sys
import time
import zlib
from pathlib import Path


class Failure(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise Failure(message)


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


def decode_png_rgba(path: Path):
    data = path.read_bytes()
    pos = 8
    compressed = bytearray()
    width = height = 0
    bit_depth = color_type = interlace = 0
    while pos < len(data):
        length = struct.unpack(">I", data[pos : pos + 4])[0]
        kind = data[pos + 4 : pos + 8]
        payload = data[pos + 8 : pos + 8 + length]
        pos += 12 + length
        if kind == b"IHDR":
            width, height, bit_depth, color_type, _compression, _filter, interlace = (
                struct.unpack(">IIBBBBB", payload)
            )
        elif kind == b"IDAT":
            compressed.extend(payload)
        elif kind == b"IEND":
            break
    require(
        bit_depth == 8 and color_type == 6 and interlace == 0,
        "unexpected PNG format",
    )
    raw = zlib.decompress(bytes(compressed))
    stride = width * 4
    rows = []
    previous = bytearray(stride)
    cursor = 0
    for _y in range(height):
        filter_type = raw[cursor]
        cursor += 1
        row = bytearray(raw[cursor : cursor + stride])
        cursor += stride
        for x in range(stride):
            a = row[x - 4] if x >= 4 else 0
            b = previous[x]
            c = previous[x - 4] if x >= 4 else 0
            if filter_type == 1:
                row[x] = (row[x] + a) & 0xFF
            elif filter_type == 2:
                row[x] = (row[x] + b) & 0xFF
            elif filter_type == 3:
                row[x] = (row[x] + ((a + b) // 2)) & 0xFF
            elif filter_type == 4:
                p = a + b - c
                pa, pb, pc = abs(p - a), abs(p - b), abs(p - c)
                predictor = a if pa <= pb and pa <= pc else (b if pb <= pc else c)
                row[x] = (row[x] + predictor) & 0xFF
            elif filter_type != 0:
                raise Failure(f"unsupported PNG filter {filter_type}")
        rows.append(row)
        previous = row
    return width, height, rows


def read_owner_binary(path: Path):
    data = path.read_bytes()
    first = data.find(b"\n")
    second = data.find(b"\n", first + 1)
    require(first >= 0 and second >= 0 and data[:first] == b"COFFOWN1",
            "invalid owner.bin header")
    width, height = map(int, data[first + 1 : second].split())
    offset = second + 1
    count = width * height
    require(len(data) >= offset + count * 4, "owner.bin is truncated")
    owners = list(struct.unpack_from(f"<{count}i", data, offset))
    return width, height, owners


def check_rendered_image(image_path: Path, owner_path: Path, report_path: Path):
    width, height, rows = decode_png_rgba(image_path)
    owner_width, owner_height, owners = read_owner_binary(owner_path)
    require((width, height) == (owner_width, owner_height),
            "image and owner dimensions differ")
    report = json.loads(report_path.read_text(encoding="utf-8"))
    index = {item["code"]: item for item in report["ownershipIndex"]}
    by_component: dict[str, list[float]] = {}
    board_luminance = []
    bracket_luminance = []
    for y in range(height):
        for x in range(width):
            code = owners[y * width + x]
            info = index.get(code)
            if info is None:
                continue
            pixel = rows[y][x * 4 : x * 4 + 3]
            luminance = 0.2126 * pixel[0] + 0.7152 * pixel[1] + 0.0722 * pixel[2]
            if info["kind"] == "board":
                board_luminance.append(luminance)
                by_component.setdefault(info["componentId"], []).append(luminance)
            elif info["kind"] == "bracket":
                bracket_luminance.append(luminance)
    require(len(board_luminance) > 500, "too few board pixels")
    require(len(bracket_luminance) > 50, "too few bracket pixels")
    board_mean = sum(board_luminance) / len(board_luminance)
    board_variance = sum((value - board_mean) ** 2 for value in board_luminance) / len(
        board_luminance
    )
    require(board_variance > 9.0, "rendered board pixels lack visible wood-grain contrast")
    require(max(bracket_luminance) < 250.0, "a bracket pixel is overexposed to near-white")
    require(
        sum(bracket_luminance) / len(bracket_luminance) < 225.0,
        "bracket luminance is overexposed on average",
    )
    adjacent = [
        by_component.get(component_id, [])
        for component_id in ["B0N", "B0E", "B0S", "B0W"]
    ]
    require(all(len(values) > 20 for values in adjacent),
            "adjacent board components are missing from the ownership map")
    means = [sum(values) / len(values) for values in adjacent]
    require(max(means) - min(means) > 1.0,
            "same-color adjacent boards are not visually distinguishable")
    component_variances = {}
    for component_id, values in by_component.items():
        if len(values) < 20:
            continue
        mean = sum(values) / len(values)
        component_variances[component_id] = sum(
            (value - mean) ** 2 for value in values
        ) / len(values)
    require(
        max(component_variances.values()) > 9.0,
        "no single board component shows visible wood-grain variance",
    )
    return {
        "boardMeanLuminance": board_mean,
        "boardLuminanceVariance": board_variance,
        "bracketMaxLuminance": max(bracket_luminance),
        "bracketMeanLuminance": sum(bracket_luminance) / len(bracket_luminance),
        "adjacentBoardMeanLuminance": {
            component_id: value
            for component_id, value in zip(["B0N", "B0E", "B0S", "B0W"], means)
        },
        "maxBoardComponentVariance": max(component_variances.values()),
    }


def run(exe: Path, scene: Path, out: Path, *extra, timeout=120):
    cmd = [str(exe), "--scene", str(scene), "--out", str(out), *map(str, extra)]
    completed = subprocess.run(cmd, text=True, capture_output=True, timeout=timeout)
    return completed


def generate(generator: Path, out: Path, variant="clean"):
    subprocess.run(
        [sys.executable, str(generator), "--out", str(out), "--variant", variant],
        check=True,
        capture_output=True,
        text=True,
    )


def snapshot_count(path: Path) -> int:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
        return int(value["completedBlockCount"])
    except Exception:
        return 0


def wait_for_blocks(path: Path, count: int, process: subprocess.Popen, timeout=20):
    deadline = time.time() + timeout
    snap = path / "progress.snapshot.json"
    while time.time() < deadline:
        if process.poll() is not None:
            raise Failure(f"process exited before reaching {count} blocks: {process.returncode}")
        if snapshot_count(snap) >= count:
            return
        time.sleep(0.01)
    raise Failure(f"timed out waiting for snapshot block {count}")


def interrupt_run(exe: Path, scene: Path, out: Path, blocks=3):
    if os.name == "nt":
        completed = run(
            exe,
            scene,
            out,
            "--fresh",
            "--test-interrupt-after-block",
            str(blocks - 1),
        )
        require(completed.returncode == 0, f"simulated Ctrl+C exit={completed.returncode}")
        recovery_path = out / "recovery.json"
        require(recovery_path.exists(), "simulated Ctrl+C did not write recovery.json")
        recovery = json.loads(recovery_path.read_text(encoding="utf-8"))
        require(recovery["interrupted"] is True, "recovery report is not marked interrupted")
        require(recovery["exitCode"] == 0, "recovery exit code is not zero")
        completed_blocks = snapshot_count(out / "progress.snapshot.json")
        require(completed_blocks == blocks, "simulated interrupt stopped at the wrong block")
        return completed_blocks
    cmd = [
        str(exe),
        "--scene",
        str(scene),
        "--out",
        str(out),
        "--fresh",
        "--tile-delay-ms",
        "25",
    ]
    process = subprocess.Popen(cmd, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    wait_for_blocks(out, blocks, process)
    process.send_signal(signal.SIGINT)
    stdout, stderr = process.communicate(timeout=30)
    require(process.returncode == 0, f"Ctrl+C exit={process.returncode}\n{stderr}")
    recovery_path = out / "recovery.json"
    require(recovery_path.exists(), "Ctrl+C did not write recovery.json")
    recovery = json.loads(recovery_path.read_text(encoding="utf-8"))
    require(recovery["interrupted"] is True, "recovery report is not marked interrupted")
    require(recovery["exitCode"] == 0, "recovery exit code is not zero")
    completed = snapshot_count(out / "progress.snapshot.json")
    require(0 < completed, "interrupted snapshot is empty")
    return completed


def force_kill_run(exe: Path, scene: Path, out: Path, blocks=3):
    cmd = [
        str(exe),
        "--scene",
        str(scene),
        "--out",
        str(out),
        "--fresh",
        "--tile-delay-ms",
        "25",
    ]
    process = subprocess.Popen(cmd, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    wait_for_blocks(out, blocks, process)
    process.kill()
    process.communicate(timeout=30)
    require(process.returncode != 0, "force-killed process unexpectedly returned zero")
    require(not (out / "recovery.json").exists(), "force kill produced a recovery report")
    completed = snapshot_count(out / "progress.snapshot.json")
    require(0 < completed, "force-kill snapshot is empty")
    return completed


def compare_artifacts(left: Path, right: Path, names=None):
    names = names or ["rgba.png", "owner.bin", "projection.json"]
    for name in names:
        require((left / name).exists(), f"missing {left / name}")
        require((right / name).exists(), f"missing {right / name}")
        require(
            (left / name).read_bytes() == (right / name).read_bytes(),
            f"byte mismatch: {name}",
        )


def validate_error(
    exe: Path,
    generator: Path,
    work: Path,
    variant: str,
    expected_code: str,
    require_layer=True,
):
    scene = work / f"{variant}.json"
    generate(generator, scene, variant)
    out = work / f"validation-{variant}"
    completed = run(exe, scene, out)
    require(completed.returncode == 2, f"{variant}: exit={completed.returncode}")
    validation = json.loads((out / "validation.json").read_text(encoding="utf-8"))
    require(validation["ok"] is False, f"{variant}: validation ok flag")
    require(validation["warningCount"] >= 1, f"{variant}: no warnings")
    matching = [w for w in validation["warnings"] if w["code"] == expected_code]
    require(matching, f"{variant}: missing warning {expected_code}")
    for warning in matching:
        if require_layer:
            require("layer" in warning, f"{variant}: warning has no layer")
            require(isinstance(warning["layer"], int), f"{variant}: layer is not an integer")
            require("componentId" in warning, f"{variant}: warning has no componentId")
            require(
                isinstance(warning["componentId"], str) and warning["componentId"],
                f"{variant}: empty componentId",
            )


def check_projection_report(path: Path):
    projection = json.loads(path.read_text(encoding="utf-8"))
    landings = [item for light in projection["lights"] for item in light["landings"]]
    require(landings, "projection landing list is empty")
    require(
        any(item["treatedAsUnoccluded"] for item in landings),
        "sample does not exercise an out-of-bounds unoccluded landing",
    )
    for item in landings:
        if not item["inBounds"]:
            require(
                item["treatedAsUnoccluded"],
                f"out-of-bounds landing not marked unoccluded: {item}",
            )
    comparisons = projection["penumbraComparisons"]
    require(len(comparisons) >= 3, "too few penumbra comparisons")
    soft_edges = 0
    for comparison in comparisons:
        require(len(comparison["sampleOffsets"]) == 9, "wrong penumbra sample count")
        require(
            all(0.0 <= value <= 1.0 for value in comparison["sampleBrightness"]),
            "penumbra brightness outside [0,1]",
        )
        require(comparison["penumbraWidth"] > 0.0, "non-positive penumbra width")
        values = comparison["sampleBrightness"]
        if any(0.02 < value < 0.98 for value in values):
            soft_edges += 1
    require(soft_edges > 0, "no penumbra profile contains a soft intermediate edge")

    grouped: dict[tuple[str, int], list[dict]] = {}
    for comparison in comparisons:
        grouped.setdefault(
            (comparison["light"], comparison["occluderLayer"]), []
        ).append(comparison)
    strict_pairs = 0
    for group in grouped.values():
        for left in group:
            for right in group:
                if (
                    right["receiverDistance"] >= left["receiverDistance"]
                    and right["lightOccluderDistance"] >= left["lightOccluderDistance"]
                    and (
                        right["receiverDistance"] > left["receiverDistance"]
                        or right["lightOccluderDistance"] > left["lightOccluderDistance"]
                    )
                ):
                    require(
                        right["penumbraWidth"] >= left["penumbraWidth"],
                        "penumbra width is not monotone in distance",
                    )
                    if (
                        right["receiverDistance"] > left["receiverDistance"]
                        or right["lightOccluderDistance"] > left["lightOccluderDistance"]
                    ):
                        strict_pairs += 1
    require(strict_pairs > 0, "no strict penumbra monotonicity pair in sample")
    light_positions = {
        light["id"]: light["position"] for light in projection["lights"]
    }
    grouped_landings: dict[tuple[str, str], list[dict]] = {}
    for light in projection["lights"]:
        for item in light["landings"]:
            grouped_landings.setdefault(
                (light["id"], item["componentId"]), []
            ).append(item)
    checked_layer_offsets = 0
    for (light_id, _component_id), group in grouped_landings.items():
        by_z = sorted(group, key=lambda item: item["point"][2])
        for left, right in zip(by_z, by_z[1:]):
            if right["point"][2] <= left["point"][2] + 1e-9:
                continue
            lx, ly, _ = light_positions[light_id]
            left_distance = (
                (left["point"][0] - lx) ** 2 + (left["point"][1] - ly) ** 2
            ) ** 0.5
            right_distance = (
                (right["point"][0] - lx) ** 2 + (right["point"][1] - ly) ** 2
            ) ** 0.5
            require(
                right_distance > left_distance,
                "projection landing does not shift outward with receiver distance",
            )
            checked_layer_offsets += 1
    require(checked_layer_offsets > 0, "no projection layer-distance offset to verify")
    return projection


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--exe", type=Path, required=True)
    parser.add_argument("--texture-probe", type=Path)
    parser.add_argument("--work", type=Path, required=True)
    args = parser.parse_args()

    root = Path(__file__).resolve().parents[1]
    generator = root / "tools" / "make_sample.py"
    work = args.work.resolve()
    if work.exists():
        shutil.rmtree(work)
    work.mkdir(parents=True)
    exe = args.exe.resolve()
    require(exe.exists(), f"executable not found: {exe}")
    texture_probe = (
        args.texture_probe.resolve()
        if args.texture_probe
        else exe.with_name("coffered_texture_probe")
    )
    require(texture_probe.exists(), f"texture probe not found: {texture_probe}")

    scene = work / "coffered_scene.json"
    generate(generator, scene)
    baseline = work / "baseline"
    completed = run(exe, scene, baseline, "--fresh")
    require(completed.returncode == 0, f"baseline failed:\n{completed.stdout}\n{completed.stderr}")
    for name in ["rgba.png", "ownership.png", "owner.bin", "projection.json", "report.json",
                 "progress.snapshot.json"]:
        require((baseline / name).exists(), f"missing baseline artifact {name}")
    report = json.loads((baseline / "report.json").read_text(encoding="utf-8"))
    require(report["ok"] is True, "baseline report is not ok")
    require(report["completedBlocks"] == report["totalBlocks"], "baseline is incomplete")
    require(report["premultipliedAlpha"] is True, "RGBA is not marked premultiplied")
    require(len(report["woodTextures"]) == 12, "unexpected wood texture count")
    image_metrics = check_rendered_image(
        baseline / "rgba.png", baseline / "owner.bin", baseline / "report.json"
    )
    for layer in report["layerTransmissionReflection"]:
        total = layer["reflectedShare"] + layer["transmittedShare"]
        require(abs(total - 1.0) < 1e-9, "layer energy shares do not sum to one")
        require(layer["reflectedContribution"] > 0.0, "zero reflected contribution")
        require(layer["transmittedContribution"] > 0.0, "zero transmitted contribution")

    repeat = work / "repeat"
    completed = run(exe, scene, repeat, "--fresh")
    require(completed.returncode == 0, "repeat run failed")
    compare_artifacts(baseline, repeat)

    texture_a = work / "texture-a"
    texture_b = work / "texture-b"
    require(run(exe, scene, texture_a, "--fresh", "--dump-textures").returncode == 0,
            "texture dump A failed")
    require(run(exe, scene, texture_b, "--fresh", "--dump-textures").returncode == 0,
            "texture dump B failed")
    require(
        sha256(texture_a / "textures" / "wood_L1_B1N_seed2201_res64.bin")
        == sha256(texture_b / "textures" / "wood_L1_B1N_seed2201_res64.bin"),
        "wood texture table is not reproducible",
    )
    for file_a in sorted((texture_a / "textures").iterdir()):
        file_b = texture_b / "textures" / file_a.name
        require(file_b.exists() and file_a.read_bytes() == file_b.read_bytes(),
                f"texture dump mismatch: {file_a.name}")
    probe_table = work / "probe-wood.bin"
    probe = subprocess.run(
        [
            str(texture_probe),
            "--seed",
            "2201",
            "--id",
            "B1N",
            "--resolution",
            "64",
            "--color",
            "0.72",
            "0.415",
            "0.19",
            "--table",
            str(probe_table),
        ],
        text=True,
        capture_output=True,
        check=True,
    )
    probe_result = json.loads(probe.stdout)
    report_texture = next(
        item
        for item in report["woodTextures"]
        if item["componentId"] == "B1N" and item["layer"] == 1
    )
    require(
        probe_result["digest"] == report_texture["digest"],
        "independent texture probe digest differs from render texture",
    )
    require(
        probe_table.read_bytes()
        == (texture_a / "textures" / "wood_L1_B1N_seed2201_res64.bin").read_bytes(),
        "independent texture probe table differs from dump",
    )
    layer_zero_textures = {
        item["digest"] for item in report["woodTextures"] if item["layer"] == 0
    }
    require(
        len(layer_zero_textures) == 4,
        "same-color adjacent boards did not receive distinct textures",
    )

    triangle_order = work / "triangle-order.json"
    generate(generator, triangle_order, "triangle_order")
    ordered_out = work / "triangle-order"
    require(run(exe, triangle_order, ordered_out, "--fresh").returncode == 0,
            "triangle-order run failed")
    compare_artifacts(baseline, ordered_out)

    baseline_projection = check_projection_report(baseline / "projection.json")
    shifted_scene = work / "light-shift.json"
    generate(generator, shifted_scene, "light_shift")
    shifted_a = work / "light-shift-a"
    shifted_b = work / "light-shift-b"
    require(run(exe, shifted_scene, shifted_a, "--fresh").returncode == 0,
            "light-shift run failed")
    require(run(exe, shifted_scene, shifted_b, "--fresh").returncode == 0,
            "light-shift repeat failed")
    compare_artifacts(shifted_a, shifted_b)
    shifted_projection = json.loads(
        (shifted_a / "projection.json").read_text(encoding="utf-8")
    )
    baseline_landings = {
        (light["id"], item["target"], item["componentId"]): item["point"]
        for light in baseline_projection["lights"]
        for item in light["landings"]
    }
    shifted_landings = {
        (light["id"], item["target"], item["componentId"]): item["point"]
        for light in shifted_projection["lights"]
        for item in light["landings"]
    }
    require(
        baseline_landings[("L_WARM", "receiver", "K0N")]
        != shifted_landings[("L_WARM", "receiver", "K0N")],
        "moving a point light did not change projection landing coordinates",
    )

    for variant, code, require_layer in [
        ("index_oob", "mesh_index_out_of_range", True),
        ("degenerate", "degenerate_triangle", True),
        ("transmittance", "transmittance_out_of_range", True),
        ("reflectance", "reflectance_out_of_range", True),
        ("duplicate_id", "duplicate_component_id", True),
        ("missing_id", "missing_component_id", True),
        ("light_on_plane", "light_on_layer_plane", True),
        ("bad_light_color", "invalid_light_color", False),
        ("bad_camera", "invalid_camera", False),
    ]:
        validate_error(exe, generator, work, variant, code, require_layer)

    interrupted_out = work / "interrupted"
    interrupted_blocks = interrupt_run(exe, scene, interrupted_out)
    require(interrupted_blocks < report["totalBlocks"], "interrupt did not occur mid-render")
    completed = run(exe, scene, interrupted_out, "--verify-baseline", baseline)
    require(completed.returncode == 0,
            f"resume after Ctrl+C failed:\n{completed.stdout}\n{completed.stderr}")
    resumed_report = json.loads((interrupted_out / "report.json").read_text(encoding="utf-8"))
    require(resumed_report["resumedFromSnapshot"] is True, "Ctrl+C resume flag missing")
    require(resumed_report["resumedBlockCount"] == interrupted_blocks,
            "Ctrl+C resume did not use committed blocks")
    require(resumed_report["diffStatus"] == "match", "Ctrl+C resume diff mismatch")
    compare_artifacts(baseline, interrupted_out)

    killed_out = work / "force-killed"
    killed_blocks = force_kill_run(exe, scene, killed_out)
    require(killed_blocks < report["totalBlocks"], "force kill did not occur mid-render")
    completed = run(exe, scene, killed_out, "--verify-baseline", baseline)
    require(completed.returncode == 0,
            f"resume after force kill failed:\n{completed.stdout}\n{completed.stderr}")
    killed_report = json.loads((killed_out / "report.json").read_text(encoding="utf-8"))
    require(killed_report["resumedFromSnapshot"] is True, "force-kill resume flag missing")
    require(killed_report["resumedBlockCount"] == killed_blocks,
            "force-kill resume did not use committed blocks")
    require(killed_report["diffStatus"] == "match", "force-kill resume diff mismatch")
    compare_artifacts(baseline, killed_out)

    corrupt_out = work / "corrupt-snapshot"
    corrupt_blocks = force_kill_run(exe, scene, corrupt_out)
    snap_path = corrupt_out / "progress.snapshot.json"
    snap = json.loads(snap_path.read_text(encoding="utf-8"))
    snap["blocks"][0]["rgbaBase64"] = "AAAA"
    snap_path.write_text(json.dumps(snap, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    completed = run(exe, scene, corrupt_out, "--verify-baseline", baseline)
    require(completed.returncode == 0, "corrupt snapshot did not recover")
    corrupt_report = json.loads((corrupt_out / "report.json").read_text(encoding="utf-8"))
    require(corrupt_report["resumedFromSnapshot"] is False,
            "corrupt snapshot was reused")
    require(corrupt_report["resumedBlockCount"] == 0,
            "corrupt snapshot retained a block offset")
    require(corrupt_report["snapshotResetReason"], "corrupt snapshot reset reason missing")
    compare_artifacts(baseline, corrupt_out)

    seed_out = work / "seed-reset"
    force_kill_run(exe, scene, seed_out)
    seed_scene = work / "seed_changed.json"
    generate(generator, seed_scene, "seed_change")
    completed = run(exe, seed_scene, seed_out, "--verify-baseline", baseline)
    require(completed.returncode == 3, "seed change should produce structured diff")
    seed_report = json.loads((seed_out / "report.json").read_text(encoding="utf-8"))
    require(seed_report["resumedFromSnapshot"] is False,
            "snapshot with changed seed was reused")
    require("digest" in seed_report["snapshotResetReason"],
            "seed change reset did not mention scene digest")
    diff = json.loads((seed_out / "diff_report.json").read_text(encoding="utf-8"))
    require(diff["ok"] is False and diff["totalDifferences"] > 0,
            "structured diff did not report seed change")
    first = diff["differences"][0]
    for field in ["block", "x", "y", "channel"]:
        require(field in first, f"structured diff lacks {field}")

    seed_diff = work / "seed-diff"
    seed_diff_scene = work / "seed_diff.json"
    generate(generator, seed_diff_scene, "seed_change")
    completed = run(exe, seed_diff_scene, seed_diff, "--fresh", "--verify-baseline", baseline)
    require(completed.returncode == 3, "baseline mismatch must exit nonzero")
    diff = json.loads((seed_diff / "diff_report.json").read_text(encoding="utf-8"))
    require(diff["totalDifferences"] > 0, "mismatch diff is empty")
    for field in ["block", "x", "y", "channel"]:
        require(field in diff["differences"][0], f"diff entry lacks {field}")

    print(
        json.dumps(
            {
                "ok": True,
                "tests": [
                    "baseline",
                    "image-grain-and-exposure",
                    "repeat-byte-identity",
                    "texture-byte-identity",
                    "independent-texture-probe",
                    "adjacent-texture-distinction",
                    "triangle-order-identity",
                    "validation-warnings",
                    "projection-landings",
                    "projection-layer-offset",
                    "light-shift-determinism",
                    "penumbra-monotonicity",
                    "ctrl-c-resume",
                    "sigkill-resume",
                    "corrupt-snapshot-reset",
                    "seed-snapshot-reset",
                    "structured-diff",
                ],
                "baselineDigest": sha256(baseline / "rgba.png"),
                "interruptedBlocks": interrupted_blocks,
                "imageMetrics": image_metrics,
            },
            indent=2,
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Failure as error:
        print(f"TEST FAILURE: {error}", file=sys.stderr)
        raise SystemExit(1)
