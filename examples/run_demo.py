#!/usr/bin/env python3
"""Runnable end-to-end demo for the terrain geometry transform chain.

Usage::

    .venv/bin/python examples/run_demo.py                # non-interactive
    .venv/bin/python examples/run_demo.py --interactive  # REPL adjustments
    .venv/bin/python examples/run_demo.py --workers 1

Artifacts are written under ``./work`` (snapshots, logs, images).
"""

from __future__ import annotations

import argparse
import copy
import logging
import os
import sys

import numpy as np

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, ROOT)

from terrain_geo import SCHEMA_VERSION, ErrorCode, GeoError
from terrain_geo.factory import build_stages
from terrain_geo.io_utils import load_image, save_image
from terrain_geo.persistence import SnapshotStore
from terrain_geo.pipeline import Pipeline
from terrain_geo.resample import ResampleConfig
from terrain_geo.samplegen import generate
from terrain_geo.validation import validate_geometry
from terrain_geo.visualize import diff_heatmap, save_overlay


def setup_logging(workdir: str):
    os.makedirs(workdir, exist_ok=True)
    log_path = os.path.join(workdir, "demo.log")
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        handlers=[logging.FileHandler(log_path, mode="w"),
                  logging.StreamHandler(sys.stdout)],
    )
    return logging.getLogger("demo")


def stage_outputs(result):
    return {status.stage_id: status.output for status in result.statuses}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--workers", type=int, default=4)
    parser.add_argument("--order", type=int, default=2,
                        choices=[0, 1, 2])
    parser.add_argument("--tile", type=int, default=48)
    parser.add_argument("--interactive", action="store_true")
    parser.add_argument("--gray", action="store_true",
                        help="use the single-channel sample image")
    args = parser.parse_args()

    workdir = os.path.join(ROOT, "work")
    logger = setup_logging(workdir)
    bundle = generate(ROOT)
    image_path = bundle["gray_path"] if args.gray else bundle["rgb_path"]
    source = load_image(image_path)
    logger.info("loaded source %s shape=%s dtype=%s",
                os.path.basename(image_path), source.shape, source.dtype)

    cfg = ResampleConfig(tile_size=args.tile, order=args.order,
                         max_overlap=16, max_pixels_per_thread=4_000_000)
    specs = bundle["stage_specs"]
    truth = bundle["truth"]
    stages = build_stages(specs, cfg)
    store = SnapshotStore(workdir, SCHEMA_VERSION)
    store.reset()
    pipeline = Pipeline(stages, workdir, SCHEMA_VERSION,
                        max_workers=args.workers, store=store)

    # 1. Full cold-cache run.
    result = pipeline.run(source)
    logger.info("cold run commit sequence: %s", result.commit_sequence)
    for item in result.invalidation_listing():
        logger.info("  %s", item)
    outputs = stage_outputs(result)
    _write_stage_images(workdir, source, outputs, truth)

    # 2. Ground-truth geometric validation.
    report = validate_geometry(stages, truth, tolerance=0.35)
    logger.info("geometry validation:\n%s", report.summary())

    # 3. Incremental run after changing only the radial stage parameters.
    new_specs = copy.deepcopy(specs)
    new_specs[1]["params"]["k1"] = 0.22
    new_specs[1]["param_version"] = "r-1.1.0"
    pipeline.update_stage_params(
        "radial_correction",
        build_stages(new_specs, cfg)[1].params,
        "r-1.1.0")
    inc_result = pipeline.run(source)
    logger.info("incremental states: %s",
                inc_result.invalidation_listing())
    ok, _ = pipeline.assert_matches_full(source, inc_result)
    logger.info("incremental vs full recompute pixel-identical: %s", ok)

    # 4. Restart: a brand-new pipeline must restore the snapshot.
    restarted = Pipeline(build_stages(new_specs, cfg), workdir,
                         SCHEMA_VERSION, max_workers=args.workers)
    restart_result = restarted.run(source)
    logger.info("post-restart states: %s",
                restart_result.invalidation_listing())
    assert np.array_equal(restart_result.output, inc_result.output)

    # 5. Version mismatch invalidates the whole snapshot (no partial reuse).
    tampered = copy.deepcopy(new_specs)
    tampered[0]["param_version"] = "h-9.9.9"
    stale = Pipeline(build_stages(tampered, cfg), workdir,
                     SCHEMA_VERSION, max_workers=args.workers)
    stale_result = stale.run(source)
    states = {s["stage_id"]: s["state"]
              for s in stale_result.invalidation_listing()}
    logger.info("version-mismatch states (all recomputed): %s", states)
    assert all(v == "recomputed" for v in states.values())

    # 6. Difference heat map between parameter versions.
    before = outputs["affine"]
    after = inc_result.output
    heat = diff_heatmap(before, after)
    save_image(os.path.join(workdir, "diff_heatmap.png"), heat)
    logger.info("wrote diff_heatmap.png mismatch_pixels=%d",
                int(np.count_nonzero(heat.sum(axis=2))))

    # 7. Error-code demonstrations.
    _demo_error_codes(logger, pipeline, source, workdir, specs, cfg, truth)

    if args.interactive:
        _interactive(logger, pipeline, source, truth, specs, cfg)
    logger.info("demo finished successfully")


def _write_stage_images(workdir, source, outputs, truth):
    img_dir = os.path.join(workdir, "images")
    os.makedirs(img_dir, exist_ok=True)
    save_image(os.path.join(img_dir, "00_source.png"), source)
    save_overlay(os.path.join(img_dir, "01_source_points.png"),
                 source, np.asarray(truth["points_source"]))
    for idx, (stage_id, img) in enumerate(outputs.items(), start=2):
        save_image(os.path.join(img_dir, f"{idx:02d}_{stage_id}.png"), img)
    final_pts = np.asarray(truth["points_final"])
    save_overlay(os.path.join(img_dir, "99_final_points.png"),
                 outputs["affine"], final_pts)


def _demo_error_codes(logger, pipeline, source, workdir, specs, cfg, truth):
    from terrain_geo.errors import GeoError
    # E_PARA: out-of-range distortion coefficient.
    bad = copy.deepcopy(specs)
    bad[1]["params"]["k1"] = 5.0
    try:
        Pipeline(build_stages(bad, cfg), workdir, SCHEMA_VERSION).run(source)
    except GeoError as exc:
        logger.info("expected %s: %s", exc.code, exc)
        assert exc.code == ErrorCode.E_PARA
    # E_BAND: overlap cap smaller than required.
    tight_cfg = ResampleConfig(tile_size=32, order=2, max_overlap=0)
    try:
        Pipeline(build_stages(specs, tight_cfg), workdir,
                 SCHEMA_VERSION).run(source)
    except GeoError as exc:
        logger.info("expected %s: %s", exc.code, exc)
        assert exc.code == ErrorCode.E_BAND
    # E_DECODE: corrupt image file.
    corrupt = os.path.join(workdir, "corrupt.png")
    with open(corrupt, "wb") as fh:
        fh.write(b"\x89PNG\r\n\x1a\n garbage not an image")
    try:
        load_image(corrupt)
    except GeoError as exc:
        logger.info("expected %s: %s", exc.code, exc)
        assert exc.code == ErrorCode.E_DECODE
    # E_INTERNAL: isolated stage failure, retry succeeds, no cache pollution.
    probe = Pipeline(build_stages(specs, cfg), workdir, SCHEMA_VERSION,
                     store=SnapshotStore(os.path.join(workdir, "probe"),
                                         SCHEMA_VERSION))
    probe.set_fault("radial_correction",
                    GeoError(ErrorCode.E_INTERNAL, "injected fault",
                             stage="radial_correction"))
    try:
        probe.run(source)
    except GeoError as exc:
        logger.info("expected %s: %s", exc.code, exc)
        assert exc.code == ErrorCode.E_INTERNAL
    retry = probe.run(source)
    logger.info("retry after isolated fault: %s",
                retry.invalidation_listing())
    # E_GEOM: tolerance tighter than the Newton residual.
    try:
        validate_geometry(build_stages(specs, cfg), truth, tolerance=1e-9)
    except GeoError as exc:
        logger.info("expected %s: %s", exc.code, exc)
        assert exc.code == ErrorCode.E_GEOM


def _interactive(logger, pipeline, source, truth, specs, cfg):
    print("\nInteractive commands:\n"
          "  k1 <value>   update radial k1 (triggers incremental update)\n"
          "  scale <sx>   update affine x scale\n"
          "  run          re-run current pipeline\n"
          "  quit\n")
    current = copy.deepcopy(specs)
    while True:
        try:
            command = input("geo> ").strip()
        except EOFError:
            break
        if command in ("quit", "exit", ""):
            break
        parts = command.split()
        try:
            if parts[0] == "k1":
                value = float(parts[1])
                current[1]["params"]["k1"] = value
                current[1]["param_version"] = f"r-manual-{value:g}"
                stages = build_stages(current, cfg)
                pipeline.update_stage_params(
                    "radial_correction", stages[1].params,
                    stages[1].param_version)
                result = pipeline.run(source)
                logger.info("states: %s", result.invalidation_listing())
                pipeline.assert_matches_full(source, result)
            elif parts[0] == "scale":
                value = float(parts[1])
                current[2]["params"]["scale"][0] = value
                current[2]["param_version"] = f"a-manual-{value:g}"
                stages = build_stages(current, cfg)
                pipeline.update_stage_params(
                    "affine", stages[2].params, stages[2].param_version)
                result = pipeline.run(source)
                logger.info("states: %s", result.invalidation_listing())
                pipeline.assert_matches_full(source, result)
            elif parts[0] == "run":
                result = pipeline.run(source)
                logger.info("states: %s", result.invalidation_listing())
            else:
                print("unknown command")
        except GeoError as exc:
            logger.error("%s", exc)


if __name__ == "__main__":
    main()
