#!/usr/bin/env python3

import argparse
import copy
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


class TestFailure(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise TestFailure(message)


def run(binary, args, check=True, timeout=120):
    command = [str(binary)] + [str(arg) for arg in args]
    completed = subprocess.run(
        command,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        timeout=timeout,
    )
    if check and completed.returncode != 0:
        raise TestFailure(
            f"command failed ({completed.returncode}): {' '.join(command)}\n"
            f"stdout:\n{completed.stdout}\nstderr:\n{completed.stderr}"
        )
    return completed


def png_decode(path):
    data = path.read_bytes()
    require(data[:8] == b"\x89PNG\r\n\x1a\n", f"bad PNG signature: {path}")
    offset = 8
    idat = bytearray()
    width = height = None
    while offset + 12 <= len(data):
        length = struct.unpack(">I", data[offset : offset + 4])[0]
        chunk_type = data[offset + 4 : offset + 8]
        chunk_data = data[offset + 8 : offset + 8 + length]
        stored_crc = struct.unpack(">I", data[offset + 8 + length : offset + 12 + length])[0]
        require(
            zlib.crc32(chunk_type + chunk_data) & 0xFFFFFFFF == stored_crc,
            f"PNG chunk CRC mismatch: {chunk_type!r}",
        )
        if chunk_type == b"IHDR":
            width, height, depth, color_type = struct.unpack(">IIBB", chunk_data[:10])
            require(depth == 8 and color_type == 6, "expected 8-bit RGBA PNG")
        elif chunk_type == b"IDAT":
            idat.extend(chunk_data)
        elif chunk_type == b"IEND":
            break
        offset += 12 + length
    require(width is not None and height is not None, "missing IHDR")
    raw = zlib.decompress(bytes(idat))
    row_bytes = width * 4
    require(len(raw) == (row_bytes + 1) * height, "unexpected PNG scanline size")
    pixels = bytearray()
    for y in range(height):
        start = y * (row_bytes + 1)
        require(raw[start] == 0, "test decoder expects filter type 0")
        pixels.extend(raw[start + 1 : start + 1 + row_bytes])
    return width, height, bytes(pixels)


def load_report(directory):
    with (directory / "report.json").open("r", encoding="utf-8") as stream:
        return json.load(stream)


def sha256(path):
    import hashlib

    return hashlib.sha256(path.read_bytes()).hexdigest()


def assert_file_equal(a, b, label):
    require(a.read_bytes() == b.read_bytes(), f"{label} differs: {a} vs {b}")


def render(binary, scene, out, *extra):
    return run(binary, ["render", "--scene", scene, "--out", out, *extra])


def validate_render_outputs(directory, scene_data):
    report = load_report(directory)
    require(report["status"] == "ok", "render report status is not ok")
    image = report["image"]
    width = int(image["width"])
    height = int(image["height"])
    require(len(report["tiles"]) == int(image["tile_count"]), "tile report count mismatch")
    require(report["snapshot"]["snapshot_commit_count"] == image["tile_count"],
            "snapshot commit count is not deterministic")

    rgba = (directory / "render.rgba8").read_bytes()
    smoke = (directory / "smoke_premultiplied.rgba8").read_bytes()
    owner = (directory / "ownership.u32le").read_bytes()
    require(len(rgba) == width * height * 4, "final RGBA buffer size mismatch")
    require(len(smoke) == width * height * 4, "smoke RGBA buffer size mismatch")
    require(len(owner) == width * height * 4, "ownership buffer size mismatch")

    for _, _, png_pixels in [png_decode(directory / "render.png")]:
        require(png_pixels == rgba, "render.png does not reproduce render.rgba8")
    for _, _, png_pixels in [png_decode(directory / "smoke_premultiplied.png")]:
        require(png_pixels == smoke, "smoke PNG does not reproduce its raw RGBA")
    for _, _, png_pixels in [png_decode(directory / "ownership.png")]:
        require(png_pixels == owner, "ownership PNG does not encode raw uint32 pixels")

    valid_ids = {int(particle["id"]) for particle in scene_data["particles"]}
    ids = struct.unpack("<" + "I" * (len(owner) // 4), owner)
    no_particle = 0xFFFFFFFF
    require(all(value == no_particle or value in valid_ids for value in ids),
            "ownership map contains an unknown particle id")
    require(any(value != no_particle for value in ids), "ownership map is empty")

    for index in range(0, len(smoke), 4):
        alpha = smoke[index + 3]
        require(smoke[index] <= alpha and smoke[index + 1] <= alpha and
                smoke[index + 2] <= alpha, "smoke RGBA is not premultiplied")

    aggregate = report["aggregate"]
    require(aggregate["visible_particle_fragments"] > 0,
            "no visible particle fragments were counted")
    require(0.0 <= aggregate["average_transmittance"] <= 1.0,
            "average transmittance is outside [0,1]")

    shadow = report["shadow"]
    active = [item for item in shadow["shadow_landings"] if item["active_shadow"]]
    require(len(active) > 0, "no active shadow landings")
    for item in shadow["shadow_landings"]:
        if item["active_shadow"]:
            require(item["valid_projection"] and item["inside_ground"],
                    "active shadow landing is outside valid ground projection")
            require(item["penumbra_width"] > 0.0, "non-positive penumbra width")
        if item["inside_ground"]:
            require(item["valid_projection"], "inside-ground landing has invalid projection")

    trend = shadow["penumbra_trend"]
    require(trend["monotonic_in_layer_to_ground_distance"], "ground-distance trend failed")
    require(trend["monotonic_in_light_to_layer_distance"], "light-distance trend failed")
    controlled = trend["controlled_distance_samples"]
    require(len(controlled) >= 6, "missing controlled penumbra samples")
    ground_widths = [item["transition_width"] for item in controlled[:3]]
    light_widths = [item["transition_width"] for item in controlled[3:6]]
    require(all(b > a for a, b in zip(ground_widths, ground_widths[1:])),
            "penumbra width does not increase with ground distance")
    require(all(b > a for a, b in zip(light_widths, light_widths[1:])),
            "penumbra width does not increase with light distance")

    probes = report["shadow_probes"]
    require(len(probes) >= 5, "insufficient shadow probes")
    for probe in probes:
        require(abs(probe["share_sum"] - 1.0) <= 1e-6,
                "multi-light shares do not sum to one")
        require(abs(probe["superposition_excess"]) <= 1e-9,
                "multi-light superposition exceeds the sum of single-light contributions")
        shares = [light["share"] for light in probe["lights"]]
        require(all(-1e-12 <= share <= 1.0 + 1e-12 for share in shares),
                "light share outside [0,1]")

    profiles = report["penumbra_edge_profiles"]
    require(len(profiles) >= 1, "no penumbra edge profiles")
    for profile in profiles:
        samples = profile["samples"]
        require(profile["transition_width"] > 0.0, "edge profile has zero width")
        require(samples[0]["coverage"] >= 0.99 and samples[-1]["coverage"] <= 0.01,
                "edge profile does not span covered to uncovered")
        for a, b in zip(samples, samples[1:]):
            require(b["coverage"] <= a["coverage"] + 1e-9,
                    "coverage is not monotone across the soft edge")
        require(max(item["linear_luminance"] for item in samples) >
                min(item["linear_luminance"] for item in samples) + 1e-6,
                "edge profile does not sample a brightness transition")
    return report


def test_invalid_inputs(binary, base_scene, root):
    invalid_dir = root / "invalid"
    invalid_dir.mkdir(parents=True, exist_ok=True)

    def expect_error(name, mutate, code, path_fragment, render_mode=False, extra=None):
        data = copy.deepcopy(base_scene)
        mutate(data)
        case_dir = invalid_dir / name
        case_dir.mkdir(parents=True, exist_ok=True)
        scene_path = case_dir / "scene.json"
        scene_path.write_text(json.dumps(data), encoding="utf-8")
        args = ["render" if render_mode else "validate", "--scene", scene_path,
                "--out", case_dir / "report"]
        if extra:
            args.extend(extra)
        completed = run(binary, args, check=False)
        require(completed.returncode != 0, f"{name}: invalid scene was accepted")
        report_path = case_dir / "report" / "error_report.json"
        require(report_path.exists(), f"{name}: structured error report is missing")
        report = json.loads(report_path.read_text(encoding="utf-8"))
        require(report["status"] == "error", f"{name}: wrong error report status")
        matching = [error for error in report["errors"] if error["code"] == code]
        require(matching, f"{name}: business code {code} not found in {report['errors']}")
        require(any(path_fragment in error["path"] for error in matching),
                f"{name}: element path {path_fragment} not found")
        require(any("element" in error for error in matching),
                f"{name}: error has no element location")

    expect_error(
        "duplicate_id",
        lambda data: data["particles"][1].__setitem__("id", data["particles"][0]["id"]),
        "PARTICLE_ID_DUPLICATE",
        "particles[1].id",
    )
    expect_error(
        "degenerate_quad",
        lambda data: data["particles"][0].__setitem__("size", [0.0, 0.4]),
        "PARTICLE_QUAD_DEGENERATE",
        "particles[0].size",
    )
    expect_error(
        "transmittance_range",
        lambda data: data["particles"][0].__setitem__("transmittance", 1.25),
        "PARTICLE_TRANSMITTANCE_OUT_OF_RANGE",
        "particles[0].transmittance",
    )
    expect_error(
        "layer_range",
        lambda data: data["particles"][0].__setitem__("layer", 999),
        "LAYER_INDEX_OUT_OF_RANGE",
        "particles[0].layer",
    )

    def remove_layer(data):
        for particle in data["particles"]:
            if particle["layer"] == 3:
                particle["layer"] = 4

    expect_error(
        "missing_layer",
        remove_layer,
        "PARTICLE_LAYER_MISSING",
        "particles[*].layer",
    )
    expect_error(
        "timestamp_not_increasing",
        lambda data: data["camera_path"][1].__setitem__(
            "timestamp", data["camera_path"][0]["timestamp"]
        ),
        "CAMERA_TIMESTAMP_NOT_INCREASING",
        "camera_path[1].timestamp",
    )
    expect_error(
        "ground_normal",
        lambda data: data["ground"].__setitem__("normal", [0.0, 0.0, 0.0]),
        "GROUND_NORMAL_DEGENERATE",
        "ground.normal",
    )
    expect_error(
        "light_vector",
        lambda data: data["lights"][0].__setitem__("position", ["x", 1.0, 0.0]),
        "INVALID_VECTOR",
        "lights[0].position",
    )
    expect_error(
        "frame_range",
        lambda data: None,
        "CAMERA_FRAME_INDEX_OUT_OF_RANGE",
        "camera_path[999]",
        render_mode=True,
        extra=["--frame", "999"],
    )

    malformed = invalid_dir / "malformed"
    malformed.mkdir(parents=True, exist_ok=True)
    (malformed / "scene.json").write_text("{", encoding="utf-8")
    completed = run(binary, ["validate", "--scene", malformed / "scene.json",
                             "--out", malformed / "report"], check=False)
    require(completed.returncode != 0, "malformed JSON was accepted")
    report = json.loads((malformed / "report" / "error_report.json").read_text())
    require(any(error["code"] == "JSON_PARSE_ERROR" for error in report["errors"]),
            "JSON_PARSE_ERROR missing")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--binary", required=True)
    parser.add_argument("--work", required=True)
    args = parser.parse_args()

    binary = Path(args.binary).resolve()
    root = Path(args.work).resolve()
    if root.exists():
        shutil.rmtree(root)
    root.mkdir(parents=True)

    scene_path = root / "scene.json"
    run(binary, ["generate", "--out", scene_path])
    scene_data = json.loads(scene_path.read_text(encoding="utf-8"))
    validated = run(binary, ["validate", "--scene", scene_path, "--out", root / "validated"])
    require(json.loads(validated.stdout)["status"] == "ok", "validate did not report ok")

    full = root / "full"
    render(binary, scene_path, full)
    report = validate_render_outputs(full, scene_data)
    require(report["scene"]["content_digest"] == sha256(scene_path),
            "scene SHA-256 does not match the input file")
    import hashlib

    order_bytes = "".join(
        f"{particle['layer']}:{particle['id']}\n"
        for particle in sorted(
            scene_data["particles"], key=lambda item: (item["layer"], item["id"])
        )
    ).encode("utf-8")
    require(report["scene"]["composition_order_sha256"] ==
            hashlib.sha256(order_bytes).hexdigest(),
            "composition order digest is not reproducible")
    expected_rgba_hash = report["image"]["rgba_sha256"]

    orbited = root / "orbited"
    render(binary, scene_path, orbited, "--frame", "7")
    orbited_report = validate_render_outputs(orbited, scene_data)
    require(orbited_report["scene"]["composition_order_sha256"] ==
            report["scene"]["composition_order_sha256"],
            "camera orbit changed the particle composition order")
    require(orbited_report["camera"]["frame"] == 7,
            "camera frame override was not applied")
    require(orbited_report["image"]["rgba_sha256"] != expected_rgba_hash,
            "orbit render did not change with the camera")

    repeat = root / "repeat"
    render(binary, scene_path, repeat)
    assert_file_equal(full / "report.json", repeat / "report.json", "repeat report")
    assert_file_equal(full / "render.rgba8", repeat / "render.rgba8", "repeat RGBA")
    assert_file_equal(full / "ownership.u32le", repeat / "ownership.u32le",
                      "repeat ownership")
    assert_file_equal(full / "shadow_landings.json", repeat / "shadow_landings.json",
                      "repeat shadow landings")
    assert_file_equal(full / "render.png", repeat / "render.png", "repeat PNG")

    reordered_data = copy.deepcopy(scene_data)
    reordered_data["particles"].reverse()
    reordered_data["lights"].reverse()
    reordered_scene = root / "reordered.json"
    reordered_scene.write_text(json.dumps(reordered_data), encoding="utf-8")
    reordered = root / "reordered"
    render(binary, reordered_scene, reordered)
    for filename in ["render.rgba8", "smoke_premultiplied.rgba8", "ownership.u32le",
                     "shadow_landings.json"]:
        assert_file_equal(full / filename, reordered / filename,
                          f"input-order invariance: {filename}")

    zero_id_data = copy.deepcopy(scene_data)
    zero_id_data["particles"][0]["id"] = 0
    zero_id_data["render"]["shadow_samples"] = 8
    zero_id_scene = root / "zero_id_even_samples.json"
    zero_id_scene.write_text(json.dumps(zero_id_data), encoding="utf-8")
    zero_id_result = run(binary, ["validate", "--scene", zero_id_scene])
    require(json.loads(zero_id_result.stdout)["status"] == "ok",
            "particle id 0 or an even shadow sample density was rejected")
    zero_id_render = root / "zero_id_render"
    render(binary, zero_id_scene, zero_id_render)
    zero_owner = (zero_id_render / "ownership.u32le").read_bytes()
    zero_ids = struct.unpack("<" + "I" * (len(zero_owner) // 4), zero_owner)
    require(0 in zero_ids and 0xFFFFFFFF in zero_ids,
            "ownership sentinel does not distinguish particle id 0 from no particle")

    crash = root / "crash"
    crash_scene = crash / "scene.json"
    crash_scene.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(scene_path, crash_scene)
    crashed = run(binary, ["render", "--scene", crash_scene, "--out", crash,
                           "--crash-after-new-commits", "4"], check=False)
    require(crashed.returncode == 86, f"forced termination hook returned {crashed.returncode}")
    require((crash / "snapshot.bin").exists(), "forced termination left no committed snapshot")
    render(binary, crash_scene, crash)
    completed = run(binary, ["compare", "--reference", full, "--candidate", crash,
                             "--out", crash / "difference_report.json"])
    require(completed.returncode == 0, "resumed image does not match full render")
    assert_file_equal(full / "report.json", crash / "report.json", "resumed report")
    assert_file_equal(full / "shadow_landings.json", crash / "shadow_landings.json",
                      "resumed shadow landings")

    snapshot = crash / "snapshot.bin"
    snapshot_bytes = bytearray(snapshot.read_bytes())
    snapshot_bytes[len(snapshot_bytes) // 2] ^= 0x5A
    snapshot.write_bytes(snapshot_bytes)
    corrupted_resume = render(binary, crash_scene, crash)
    require("discarded snapshot" in corrupted_resume.stderr,
            "corrupted snapshot was not discarded")
    require(run(binary, ["compare", "--reference", full, "--candidate", crash,
                         "--out", crash / "difference_report.json"],
                check=False).returncode == 0,
            "corruption recovery did not reproduce the full render")

    mismatch = root / "mismatch"
    mismatch_scene = scene_path
    render(binary, mismatch_scene, mismatch)
    changed = copy.deepcopy(scene_data)
    changed["render"]["exposure"] = 1.36
    changed_scene = root / "changed.json"
    changed_scene.write_text(json.dumps(changed), encoding="utf-8")
    mismatch_run = render(binary, changed_scene, mismatch)
    require("discarded snapshot" in mismatch_run.stderr,
            "snapshot was reused after scene content changed")
    fresh_changed = root / "changed_full"
    render(binary, changed_scene, fresh_changed)
    require(run(binary, ["compare", "--reference", fresh_changed,
                         "--candidate", mismatch,
                         "--out", mismatch / "difference_report.json"],
                check=False).returncode == 0,
            "scene-change recomputation does not match a fresh render")

    if os.name != "nt":
        interrupted_dir = root / "interrupted"
        interrupted_scene = interrupted_dir / "scene.json"
        interrupted_scene.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(scene_path, interrupted_scene)
        process = subprocess.Popen(
            [str(binary), "render", "--scene", str(interrupted_scene),
             "--out", str(interrupted_dir), "--block-delay-ms", "100"],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        for _ in range(200):
            if (interrupted_dir / "snapshot.bin").exists():
                break
            time.sleep(0.02)
        time.sleep(0.12)
        process.send_signal(signal.SIGINT)
        stdout, stderr = process.communicate(timeout=30)
        require(process.returncode == 0,
                f"Ctrl+C path returned {process.returncode}: {stdout} {stderr}")
        recovery_path = interrupted_dir / "recovery_report.json"
        require(recovery_path.exists(), "Ctrl+C did not write a recovery report")
        recovery = json.loads(recovery_path.read_text())
        require(0 < recovery["completed_tile_count"] < recovery["total_tiles"],
                "Ctrl+C recovery report has an invalid completed tile count")
        render(binary, interrupted_scene, interrupted_dir)
        require(run(binary, ["compare", "--reference", full,
                             "--candidate", interrupted_dir,
                             "--out", interrupted_dir / "difference_report.json"],
                    check=False).returncode == 0,
                "Ctrl+C resume does not match a full render")

    difference = root / "difference"
    shutil.copytree(repeat, difference)
    rgba_path = difference / "render.rgba8"
    rgba = bytearray(rgba_path.read_bytes())
    rgba[(10 * int(report["image"]["width"]) + 11) * 4 + 1] ^= 0xFF
    rgba_path.write_bytes(rgba)
    owner_path = difference / "ownership.u32le"
    owner = bytearray(owner_path.read_bytes())
    owner[(20 * int(report["image"]["width"]) + 21) * 4] ^= 0x01
    owner_path.write_bytes(owner)
    compared = run(binary, ["compare", "--reference", full,
                            "--candidate", difference,
                            "--out", difference / "difference_report.json"],
                   check=False)
    require(compared.returncode == 3, "comparison did not return the difference exit code")
    diff_report = json.loads((difference / "difference_report.json").read_text())
    require(diff_report["status"] == "difference", "difference report status is wrong")
    require(diff_report["summary"]["differing_channels"] >= 2,
            "difference summary did not count channels")
    all_pixels = [
        pixel
        for block in diff_report["blocks"]
        for pixel in block["pixels"]
    ]
    require(any(pixel["x"] == 11 and pixel["y"] == 10 and "G" in pixel["channels"]
                for pixel in all_pixels),
            "structured difference lacks pixel coordinate or channel")
    require(any(pixel["x"] == 21 and pixel["y"] == 20 and "ID_B0" in pixel["channels"]
                for pixel in all_pixels),
            "structured ownership difference lacks channel")

    test_invalid_inputs(binary, scene_data, root)

    require(report["image"]["rgba_sha256"] == expected_rgba_hash,
            "final digest changed during validation")
    print("all acceptance checks passed")
    print(f"full_render_sha256={expected_rgba_hash}")
    print(f"workdir={root}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except TestFailure as error:
        print(f"TEST FAILURE: {error}", file=sys.stderr)
        sys.exit(1)
