"""End-to-end deterministic demo for the paper-cut lightbox renderer.

Run from the project root with ``python run_demo.py``.  It generates the
canonical five-layer sample plus a doubled-air-gap comparison scene, renders
both, exercises graceful interruption/resume, verifies byte-identical output
and writes every artifact under ``demo_out/``.  No network or GPU is used.
"""

from __future__ import annotations

import json
import os
import shutil
import sys

from paperlightbox.cli import main

ROOT = os.path.dirname(os.path.abspath(__file__))
WORK = os.path.join(ROOT, "demo_out")


def _run(argv: list[str]) -> int:
    print("$ python -m paperlightbox.cli", " ".join(argv))
    return main(argv)


def main_demo() -> int:
    shutil.rmtree(WORK, ignore_errors=True)
    os.makedirs(WORK, exist_ok=True)
    scene = os.path.join(WORK, "sample.json")
    gap_scene = os.path.join(WORK, "sample_gap2x.json")

    rc = _run(["generate", "--scene", scene,
               "--gap-variant", gap_scene, "--gap-scale", "2.0"])
    if rc:
        return rc

    ref_out, ref_snap = os.path.join(WORK, "ref", "out"), \
        os.path.join(WORK, "ref", "snap")
    if _run(["render", "--scene", scene, "--output", ref_out,
             "--snapshot", ref_snap]):
        return 1

    # Graceful interruption: the stop file makes the first run finish only
    # the current tile, then a second run resumes from the snapshot.
    res_out, res_snap = os.path.join(WORK, "resume", "out"), \
        os.path.join(WORK, "resume", "snap")
    stop_file = os.path.join(WORK, "stop.flag")
    with open(stop_file, "w", encoding="utf-8") as handle:
        handle.write("stop\n")
    if _run(["render", "--scene", scene, "--output", res_out,
             "--snapshot", res_snap, "--stop-file", stop_file]):
        return 1
    os.remove(stop_file)
    if _run(["render", "--scene", scene, "--output", res_out,
             "--snapshot", res_snap]):
        return 1

    diff_report = os.path.join(WORK, "resume_diff.json")
    rc = _run(["compare", "--scene", scene,
               "--reference-png", os.path.join(ref_out, "lightbox.png"),
               "--candidate-png", os.path.join(res_out, "lightbox.png"),
               "--reference-depth",
               os.path.join(ref_out, "optical_depth.npy"),
               "--candidate-depth",
               os.path.join(res_out, "optical_depth.npy"),
               "--diff-report", diff_report])
    if rc:
        return rc

    gap_out, gap_snap = os.path.join(WORK, "gap2x", "out"), \
        os.path.join(WORK, "gap2x", "snap")
    if _run(["render", "--scene", gap_scene, "--output", gap_out,
             "--snapshot", gap_snap]):
        return 1

    with open(diff_report, encoding="utf-8") as handle:
        diff = json.load(handle)
    assert diff["status"] == "identical", diff
    print("\nDemo finished. Artifacts:", sorted(os.listdir(WORK)))
    return 0


if __name__ == "__main__":
    raise SystemExit(main_demo())
