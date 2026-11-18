#!/usr/bin/env python3
"""End-to-end acceptance tests for the deterministic caisson renderer."""

from __future__ import annotations

import argparse
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


REPO_ROOT = Path(__file__).resolve().parents[1]
GENERATOR = REPO_ROOT / "tools" / "generate_samples.py"
COMPARE_FILES = [
    "image.png",
    "premultiplied.png",
    "ownership.png",
    "shadow_landings.csv",
    "render_report.json",
    "textures/manifest.json",
    "textures/layer_001_wood_rgb8.bin",
    "textures/layer_001_gauze_u8.bin",
    "textures/layer_002_wood_rgb8.bin",
    "textures/layer_002_gauze_u8.bin",
    "textures/layer_003_wood_rgb8.bin",
    "textures/layer_003_gauze_u8.bin",
]


def fail(message: str) -> None:
    raise AssertionError(message)


def run_renderer(
    exe: Path,
    scene: Path,
    output: Path,
    *extra: str,
    expect: int = 0,
) -> subprocess.CompletedProcess:
    command = [str(exe), "--scene", str(scene), "--output", str(output), *extra]
    completed = subprocess.run(
        command,
        text=True,
        capture_output=True,
        timeout=180,
        check=False,
    )
    if completed.returncode != expect:
        fail(
            f"unexpected exit {completed.returncode} for {' '.join(command)}\n"
            f"stdout:\n{completed.stdout}\nstderr:\n{completed.stderr}"
        )
    return completed


def read_json(path: Path) -> dict:
    return json.loads(path.read_text(encoding="utf-8"))


def assert_same_files(reference: Path, actual: Path) -> None:
    for relative in COMPARE_FILES:
        left = reference / relative
        right = actual / relative
        if not left.is_file() or not right.is_file():
            fail(f"missing comparison file: {relative}")
        if left.read_bytes() != right.read_bytes():
            fail(f"byte mismatch: {relative}")


def snapshot_block_count(output: Path) -> int:
    snapshot = output / "snapshot.json"
    if not snapshot.is_file():
        return 0
    try:
        data = read_json(snapshot)
    except Exception:
        return 0
    return len(data.get("completed_blocks", []))


def wait_for_blocks(output: Path, minimum: int, process: subprocess.Popen, timeout: float = 30.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            fail(f"renderer exited before hard-kill checkpoint: {process.returncode}")
        if snapshot_block_count(output) >= minimum:
            return
        time.sleep(0.02)
    fail("timed out waiting for snapshot progress")


def png_chunks(data: bytes):
    position = 8
    while position + 12 <= len(data):
        length = struct.unpack(">I", data[position : position + 4])[0]
        kind = data[position + 4 : position + 8]
        payload = data[position + 8 : position + 8 + length]
        yield kind, payload
        position += 12 + length
        if kind == b"IEND":
            break


def mutate_png_pixel(path: Path, x: int, y: int, channel: int) -> None:
    data = path.read_bytes()
    width = height = None
    idat = bytearray()
    for kind, payload in png_chunks(data):
        if kind == b"IHDR":
            width, height = struct.unpack(">II", payload[:8])
        elif kind == b"IDAT":
            idat.extend(payload)
    if width is None or height is None:
        fail("invalid PNG fixture")
    raw = bytearray(zlib.decompress(bytes(idat)))
    stride = width * 4 + 1
    offset = y * stride + 1 + x * 4 + channel
    raw[offset] = (raw[offset] + 1) % 256
    compressed = zlib.compress(bytes(raw), 9)

    def chunk(kind: bytes, payload: bytes) -> bytes:
        return (
            struct.pack(">I", len(payload))
            + kind
            + payload
            + struct.pack(">I", zlib.crc32(kind + payload) & 0xFFFFFFFF)
        )

    output = bytearray(b"\x89PNG\r\n\x1a\n")
    output.extend(chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 6, 0, 0, 0)))
    output.extend(chunk(b"IDAT", compressed))
    output.extend(chunk(b"IEND", b""))
    path.write_bytes(output)


def read_png_rgba(path: Path) -> tuple[int, int, bytes]:
    data = path.read_bytes()
    width = height = None
    idat = bytearray()
    for kind, payload in png_chunks(data):
        if kind == b"IHDR":
            width, height = struct.unpack(">II", payload[:8])
        elif kind == b"IDAT":
            idat.extend(payload)
    if width is None or height is None:
        fail(f"invalid PNG: {path}")
    raw = zlib.decompress(bytes(idat))
    stride = width * 4 + 1
    if any(raw[y * stride] != 0 for y in range(height)):
        fail("test decoder expects the renderer's deterministic filter-none PNG")
    pixels = bytearray()
    for y in range(height):
        start = y * stride + 1
        pixels.extend(raw[start : start + width * 4])
    return width, height, bytes(pixels)


def check_report(report: dict) -> None:
    if report["report_version"] != 1:
        fail("unexpected report version")
    stats = report["color_stats"]
    if stats["saturated_pixels"] != 0:
        fail("display image contains saturated pixels")
    if stats["saturated_channel_values"] != 0:
        fail("display image contains saturated channel values")
    if stats["max_channel"] >= 255:
        fail("display channel reached 255")
    if stats["non_gray_pixels"] < 1000:
        fail("not enough chromatic pixels for side-hue validation")
    if stats["coverage_pixels"] < 10000:
        fail("caisson coverage is unexpectedly small")

    layers = report["layers"]
    if len(layers) != 3:
        fail("expected three layers in report")
    for layer in layers:
        share = layer["transmit_share_ppm"] + layer["reflect_share_ppm"]
        if not 999999 <= share <= 1000001:
            fail(f"layer {layer['layer_id']} shares do not sum to 100%")
        if layer["transmitted_energy_u64"] == 0 or layer["reflected_energy_u64"] == 0:
            fail("transmission or reflection energy is missing")

    soft = report["soft_shadow"]
    if soft["samples_per_axis"] < 2:
        fail("invalid soft-shadow sample density")
    if len(soft["profiles"]) < 9:
        fail("too few soft-shadow profiles")
    crossed = [profile for profile in soft["profiles"] if profile["has_shadow"]]
    if len(crossed) < 4:
        fail("too few measurable shadow edges")
    if not any(
        min(point["gray_0_255"] for point in profile["samples"]) < 32
        and max(point["gray_0_255"] for point in profile["samples"]) > 223
        for profile in crossed
    ):
        fail("shadow grayscale lacks a full soft transition")
    trend = soft["depth_trend"]
    if len(trend["points"]) < 2:
        fail("depth trend requires at least two occluder depths")
    if not trend["monotonic_non_decreasing"]:
        fail("soft-shadow transition width does not grow with occluder depth")
    if len(report["projection_landings"]) != 144:
        fail("projection landing coordinate count is unstable")


def check_invalid_scene(
    exe: Path,
    scene: Path,
    output: Path,
    expected_code: str,
    expected_layer: int | None,
) -> None:
    completed = run_renderer(exe, scene, output, expect=2)
    try:
        error = json.loads(completed.stderr.strip())["error"]
    except Exception as failure:
        fail(f"invalid scene did not emit structured JSON: {failure}\n{completed.stderr}")
    if error["code"] != expected_code:
        fail(f"expected {expected_code}, got {error['code']}")
    if expected_layer is not None and error.get("layer_id") != expected_layer:
        fail(f"error is missing layer_id {expected_layer}: {error}")
    if (output / "image.png").exists():
        fail("invalid scene emitted a partial image")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--exe", type=Path, required=True)
    parser.add_argument("--work-dir", type=Path, required=True)
    args = parser.parse_args()
    exe = args.exe.resolve()
    work = args.work_dir.resolve()
    if work.exists():
        shutil.rmtree(work)
    work.mkdir(parents=True)
    scenes = work / "scenes"
    subprocess.run(
        [
            sys.executable,
            str(GENERATOR),
            "--output-dir",
            str(scenes),
        ],
        check=True,
        timeout=60,
    )
    scene = scenes / "scene.json"

    baseline = work / "baseline"
    run_renderer(exe, scene, baseline)
    report = read_json(baseline / "render_report.json")
    check_report(report)
    report_text = (baseline / "render_report.json").read_text(encoding="utf-8")
    if "/home/" in report_text or "timestamp" in report_text or "duration" in report_text:
        fail("render report contains a non-stable or absolute field")
    premul_width, premul_height, premul = read_png_rgba(baseline / "premultiplied.png")
    if (premul_width, premul_height) != (480, 360):
        fail("unexpected premultiplied image dimensions")
    for index in range(0, len(premul), 4):
        alpha = premul[index + 3]
        if any(premul[index + channel] > alpha for channel in range(3)):
            fail("premultiplied RGB exceeds alpha")
    image_width, image_height, display = read_png_rgba(baseline / "image.png")
    _, _, ownership = read_png_rgba(baseline / "ownership.png")
    if (image_width, image_height) != (480, 360):
        fail("unexpected display image dimensions")
    covered_luminance = []
    for pixel in range(image_width * image_height):
        if ownership[pixel * 4 + 3] == 0:
            continue
        r, g, b = display[pixel * 4 : pixel * 4 + 3]
        covered_luminance.append((0.2126 * r + 0.7152 * g + 0.0722 * b, r, g, b))
    covered_luminance.sort()
    bright = covered_luminance[-1]
    dark_decile = covered_luminance[len(covered_luminance) // 10]
    if bright[0] < 180.0 or max(bright[1:]) - min(bright[1:]) < 8:
        fail("lamp-facing texture lost chromaticity or dynamic range")
    if dark_decile[0] < 20.0 or max(dark_decile[1:]) - min(dark_decile[1:]) < 8:
        fail("side-facing texture became effectively black/gray")
    landing_lines = (baseline / "shadow_landings.csv").read_text(encoding="utf-8").splitlines()
    if len(landing_lines) != 145:
        fail("projection landing CSV row count is unstable")
    for layer_id in (1, 2, 3):
        wood = (baseline / "textures" / f"layer_{layer_id:03d}_wood_rgb8.bin").read_bytes()
        gauze = (baseline / "textures" / f"layer_{layer_id:03d}_gauze_u8.bin").read_bytes()
        if len(wood) != 32 * 32 * 3 or len(gauze) != 32 * 32:
            fail("texture sampling table has the wrong size")
        if len(set(wood[index : index + 3] for index in range(0, len(wood), 3))) < 64:
            fail("wood sampling table does not provide enough cell variation")
        if max(gauze) - min(gauze) < 8:
            fail("gauze sampling table does not modulate transmittance")
    fingerprint = report["soft_shadow"]["distribution_fingerprint"]

    repeat = work / "repeat"
    run_renderer(exe, scene, repeat)
    assert_same_files(baseline, repeat)
    if read_json(repeat / "render_report.json")["soft_shadow"]["distribution_fingerprint"] != fingerprint:
        fail("soft-shadow distribution changed across identical runs")

    reordered = work / "reordered"
    run_renderer(exe, scenes / "scene_reordered.json", reordered)
    assert_same_files(baseline, reordered)

    invalid_cases = [
        ("invalid_index.json", "E_GEOMETRY_INDEX", 1),
        ("invalid_degenerate.json", "E_TRIANGLE_DEGENERATE", 1),
        ("invalid_transmittance.json", "E_TRANSMITTANCE_RANGE", 1),
        ("invalid_reflectance.json", "E_REFLECTANCE_RANGE", 2),
        ("invalid_duplicate_layer.json", "E_LAYER_DUPLICATE", 1),
        ("invalid_lamp_inside.json", "E_LAMP_INSIDE_LAYER", 1),
    ]
    for filename, code, layer in invalid_cases:
        check_invalid_scene(
            exe,
            scenes / filename,
            work / ("invalid_" + Path(filename).stem),
            code,
            layer,
        )

    graceful = work / "graceful"
    graceful_run = run_renderer(
        exe, scene, graceful, "--stop-after-blocks", "17"
    )
    recovery = read_json(graceful / "recovery_report.json")
    if recovery["completed_blocks"] != 17 or recovery["status"] != "interrupted":
        fail("graceful recovery report is inconsistent")
    snapshot = read_json(graceful / "snapshot.json")
    if len(snapshot["completed_blocks"]) != 17:
        fail("snapshot does not record the completed block count")
    if (graceful / "image.png").exists():
        fail("graceful interruption emitted a partial final image")
    if "interrupted_blocks=17" not in graceful_run.stdout:
        fail("graceful interruption exit was not reported")
    resumed = run_renderer(
        exe, scene, graceful, "--verify-against", str(baseline)
    )
    if "snapshot_resumed_blocks=17" not in resumed.stdout:
        fail("resume did not use the committed snapshot")
    assert_same_files(baseline, graceful)
    if (graceful / "snapshot.json").exists() or (graceful / "recovery_report.json").exists():
        fail("completed run left recovery state behind")

    hard_kill = work / "hard_kill"
    command = [
        str(exe),
        "--scene",
        str(scene),
        "--output",
        str(hard_kill),
        "--block-delay-ms",
        "25",
    ]
    process = subprocess.Popen(
        command,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        wait_for_blocks(hard_kill, 3, process)
        if os.name == "nt":
            process.kill()
        else:
            os.kill(process.pid, signal.SIGKILL)
        process.wait(timeout=10)
    finally:
        if process.poll() is None:
            process.kill()
    killed_blocks = snapshot_block_count(hard_kill)
    if killed_blocks < 3:
        fail("hard kill did not leave a committed snapshot")
    hard_resume = run_renderer(
        exe, scene, hard_kill, "--verify-against", str(baseline)
    )
    if f"snapshot_resumed_blocks={killed_blocks}" not in hard_resume.stdout:
        fail("hard-kill restart ignored the committed block count")
    assert_same_files(baseline, hard_kill)

    signal_output = work / "signal_interrupt"
    signal_process = subprocess.Popen(
        [
            str(exe),
            "--scene",
            str(scene),
            "--output",
            str(signal_output),
            "--block-delay-ms",
            "20",
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    wait_for_blocks(signal_output, 3, signal_process)
    signal_process.send_signal(signal.SIGINT)
    signal_stdout, signal_stderr = signal_process.communicate(timeout=15)
    if signal_process.returncode != 0:
        fail(
            "SIGINT did not exit with code 0\n"
            f"stdout:\n{signal_stdout}\nstderr:\n{signal_stderr}"
        )
    if "interrupted_blocks=" not in signal_stdout:
        fail("SIGINT did not write the graceful interruption report")
    if not (signal_output / "recovery_report.json").is_file():
        fail("SIGINT recovery report is missing")
    signal_blocks = snapshot_block_count(signal_output)
    signal_resume = run_renderer(
        exe, scene, signal_output, "--verify-against", str(baseline)
    )
    if f"snapshot_resumed_blocks={signal_blocks}" not in signal_resume.stdout:
        fail("SIGINT resume did not continue from the committed snapshot")
    assert_same_files(baseline, signal_output)

    corrupt = work / "corrupt"
    run_renderer(exe, scene, corrupt, "--stop-after-blocks", "24")
    block_file = corrupt / "blocks" / "block_000010.bin"
    data = bytearray(block_file.read_bytes())
    data[len(data) // 2] ^= 0x5A
    block_file.write_bytes(data)
    corrupt_result = run_renderer(
        exe, scene, corrupt, "--verify-against", str(baseline)
    )
    if "snapshot_discarded=1" not in corrupt_result.stdout:
        fail("corrupt snapshot was not discarded")
    assert_same_files(baseline, corrupt)

    changed_scene = scenes / "scene_changed_seed.json"
    changed_data = read_json(scene)
    changed_data["seed"] += 1
    changed_scene.write_text(
        json.dumps(changed_data, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    digest_output = work / "digest_change"
    run_renderer(exe, scene, digest_output, "--stop-after-blocks", "12")
    digest_result = run_renderer(
        exe,
        changed_scene,
        digest_output,
        "--stop-after-blocks",
        "4",
    )
    if "snapshot_discarded=1" not in digest_result.stdout:
        fail("snapshot with a different scene digest was not discarded")
    if snapshot_block_count(digest_output) != 4:
        fail("scene-digest restart reused old block numbering")

    bad_reference = work / "bad_reference"
    shutil.copytree(baseline, bad_reference)
    mutate_png_pixel(bad_reference / "image.png", 200, 190, 1)
    verify_failure = run_renderer(
        exe,
        scene,
        work / "verify_failure",
        "--verify-against",
        str(bad_reference),
        expect=4,
    )
    try:
        difference = json.loads(verify_failure.stderr.strip())["error"]
    except Exception as failure:
        fail(f"pixel difference was not structured JSON: {failure}\n{verify_failure.stderr}")
    if difference["code"] != "E_RECOVERY_PIXEL_MISMATCH":
        fail("wrong verification failure code")
    entries = difference.get("differences", [])
    if not entries:
        fail("pixel mismatch lacks per-pixel differences")
    first = entries[0]
    for field in ("block_index", "x", "y", "channel"):
        if field not in first:
            fail(f"pixel difference lacks {field}")

    print(
        "PASS: deterministic render, ordered compositing, validation codes, soft-shadow "
        "trend, SIGINT resume, hard-kill resume, digest/corruption discard, premultiplied "
        "bounds, and structured pixel diff"
    )
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except AssertionError as error:
        print(f"FAIL: {error}", file=sys.stderr)
        sys.exit(1)

