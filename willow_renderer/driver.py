"""Render driver: tiling, snapshot resume, interrupts, verification."""

import json
import os
import signal
import time

import numpy as np
from PIL import Image

from . import projection, raster, report, snapshot, texture
from .errors import SnapshotCorruptError, VerifyMismatchError
from .scene import scene_digest

RGBA_PNG = "rgba.png"
OWNER_PNG = "owner.png"
OWNER_PREVIEW_PNG = "owner_preview.png"
LANDINGS_JSON = "landings.json"
REPORT_JSON = "report.json"
REPORT_TXT = "report.txt"
RECOVERY_JSON = "recovery_report.json"

OWNER_PALETTE = np.array([
    [222, 119, 41], [206, 92, 60], [187, 70, 84], [160, 55, 110],
    [120, 48, 130], [70, 50, 140], [40, 60, 150], [30, 80, 150],
], dtype=np.uint8)


class _InterruptState:
    def __init__(self):
        self.received = None

    def handler(self, signum, _frame):
        if self.received is None:
            self.received = signal.Signals(signum).name


def _save_png(array, path, mode):
    Image.fromarray(array, mode=mode).save(path)


def _owner_preview(owner):
    preview = np.zeros((*owner.shape, 3), dtype=np.uint8)
    layer_mask = owner < 254
    preview[layer_mask] = OWNER_PALETTE[owner[layer_mask] % len(OWNER_PALETTE)]
    preview[owner == 254] = np.array([90, 90, 90], dtype=np.uint8)
    preview[owner == 255] = np.array([0, 0, 0], dtype=np.uint8)
    return preview


def _diff_images(expected, actual, tile_size, width, height, channels):
    diffs = []
    ey, ex = np.nonzero(np.any(expected != actual, axis=2)) if channels > 1 \
        else np.nonzero(expected != actual)
    for y, x in zip(ey.tolist(), ex.tolist()):
        tile_id = (y // tile_size) * ((width + tile_size - 1) // tile_size) + x // tile_size
        if channels > 1:
            for ch in range(channels):
                e = int(expected[y, x, ch])
                a = int(actual[y, x, ch])
                if e != a:
                    diffs.append({"tile_id": tile_id, "x": x, "y": y,
                                  "channel": ch, "expected": e, "actual": a})
        else:
            diffs.append({"tile_id": tile_id, "x": x, "y": y,
                          "channel": 0,
                          "expected": int(expected[y, x]),
                          "actual": int(actual[y, x])})
        if len(diffs) >= 64:
            break
    return diffs


def _verify(outdir, expect_rgba, expect_owner, expect_landings, tile_size,
            width, height):
    if expect_rgba:
        expected = np.asarray(Image.open(expect_rgba).convert("RGBA"))
        actual = np.asarray(Image.open(os.path.join(outdir, RGBA_PNG)).convert("RGBA"))
        if expected.shape != actual.shape or not np.array_equal(expected, actual):
            diffs = _diff_images(expected, actual, tile_size, width, height, 4)
            raise VerifyMismatchError(
                f"rgba output differs from {expect_rgba} ({len(diffs)} sampled diffs)",
                diff={"file": RGBA_PNG, "pixels": diffs})
    if expect_owner:
        expected = np.asarray(Image.open(expect_owner).convert("L"))
        actual = np.asarray(Image.open(os.path.join(outdir, OWNER_PNG)).convert("L"))
        if expected.shape != actual.shape or not np.array_equal(expected, actual):
            diffs = _diff_images(expected[:, :, None], actual[:, :, None],
                                 tile_size, width, height, 1)
            raise VerifyMismatchError(
                f"owner output differs from {expect_owner} ({len(diffs)} sampled diffs)",
                diff={"file": OWNER_PNG, "pixels": diffs})
    if expect_landings:
        with open(expect_landings, "r", encoding="utf-8") as fh:
            expected = json.load(fh)
        with open(os.path.join(outdir, LANDINGS_JSON), "r", encoding="utf-8") as fh:
            actual = json.load(fh)
        if expected != actual:
            raise VerifyMismatchError(
                f"landings output differs from {expect_landings}",
                diff={"file": LANDINGS_JSON,
                      "expected_count": len(expected) if isinstance(expected, list) else None,
                      "actual_count": len(actual) if isinstance(actual, list) else None})


def render(scene, outdir, *, strict_snapshot=False, expect_rgba=None,
           expect_owner=None, expect_landings=None, tile_delay_ms=0):
    os.makedirs(outdir, exist_ok=True)
    started = time.monotonic()
    digest = scene_digest(scene)
    width = scene["image"]["width"]
    height = scene["image"]["height"]
    tile_size = scene["tile"]["size"]
    tiles = raster.iter_tiles(width, height, tile_size)

    # Deterministic pre-pass: texture tables and hole projection fields are
    # always recomputed from the scene, never reused from an old snapshot.
    tables = texture.build_all_tables(scene)
    proj_result = projection.compute_light_fields(scene)
    ctx = raster.ShadingContext(scene, tables, proj_result)
    camera = raster.Camera(scene["camera"], width, height)
    layers = sorted(scene["layers"], key=lambda item: item["plane_z"])

    snap = snapshot.SnapshotManager(outdir, digest, width, height, tile_size)
    resumed_from = []
    discard_reason = None
    if snap.exists():
        restored, discard_reason = snap.try_restore(tiles)
        if discard_reason is not None:
            if strict_snapshot:
                raise SnapshotCorruptError(
                    f"snapshot discarded: {discard_reason}",
                    field=snap.json_path)
            snap.discard()
        else:
            resumed_from = restored

    interrupt = _InterruptState()
    previous_handlers = {}
    for sig in (signal.SIGINT, signal.SIGTERM):
        previous_handlers[sig] = signal.signal(sig, interrupt.handler)

    interrupted = False
    try:
        for tile in tiles:
            tile_id, tx, ty, tw, th = tile
            if str(tile_id) in snap.tile_digests:
                continue
            rgba_tile, owner_tile = raster.render_tile(ctx, scene, camera, layers,
                                                       (tile_id, tx, ty, tw, th))
            snap.commit_tile(tile_id, tx, ty, rgba_tile, owner_tile)
            if tile_delay_ms > 0:
                time.sleep(tile_delay_ms / 1000.0)
            if interrupt.received is not None:
                # Graceful interrupt: current tile is committed; persist a
                # recovery report and exit successfully.
                interrupted = True
                recovery = {
                    "reason": "interrupted",
                    "signal": interrupt.received,
                    "scene_digest": digest,
                    "completed_tiles": list(snap.completed),
                    "next_tile": tile_id + 1,
                    "total_tiles": len(tiles),
                }
                report.dump_json(recovery, os.path.join(outdir, RECOVERY_JSON))
                break
    finally:
        for sig, handler in previous_handlers.items():
            signal.signal(sig, handler)

    result = {
        "interrupted": interrupted,
        "resumed_from_tiles": resumed_from,
        "discarded_snapshot_reason": discard_reason,
        "completed_tiles": len(snap.completed),
        "total_tiles": len(tiles),
    }
    if interrupted:
        return result

    rgba_path = os.path.join(outdir, RGBA_PNG)
    owner_path = os.path.join(outdir, OWNER_PNG)
    _save_png(snap.rgba, rgba_path, "RGBA")
    _save_png(snap.owner, owner_path, "L")
    _save_png(_owner_preview(snap.owner),
              os.path.join(outdir, OWNER_PREVIEW_PNG), "RGB")
    report.dump_json(proj_result.landings, os.path.join(outdir, LANDINGS_JSON))

    output_digests = {
        RGBA_PNG: report.file_sha256(rgba_path),
        OWNER_PNG: report.file_sha256(owner_path),
        LANDINGS_JSON: report.file_sha256(os.path.join(outdir, LANDINGS_JSON)),
    }
    elapsed = time.monotonic() - started
    rep = report.build_report(
        scene, digest, proj_result, ctx.layer_stats, tiles, output_digests,
        resumed_from, discard_reason, elapsed)
    report.dump_json(rep, os.path.join(outdir, REPORT_JSON))
    report.write_text_summary(rep, os.path.join(outdir, REPORT_TXT))

    snap.finish()

    _verify(outdir, expect_rgba, expect_owner, expect_landings,
            tile_size, width, height)
    return result
