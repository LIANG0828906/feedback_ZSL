"""Independent reproducibility verifier.

Recomputes plane images and annotation readings using *only* the persisted
snapshot and the volume manifest/payload, then compares them item by item
against the snapshot record.  On the first mismatch it reports the plane
identifier plus the pixel coordinate, or the annotation id plus the
differing field name.

Usage: ``python -m volview.verify --snapshots DIR --samples DIR``
Exit code 0 = every snapshot verified; 1 = first mismatch reported.
"""

from __future__ import annotations

import argparse
import glob
import json
import os
import sys

import numpy as np

from .model import VolumeModel
from .resample import render_plane
from .slab import render_slab


def _recompute_plane(volume_dir: str, params: dict) -> np.ndarray:
    model = VolumeModel.load(volume_dir)
    thickness = float(params["thickness"])
    if thickness == 0.0:
        return render_plane(model, params["plane"],
                            float(params["plane_position"]),
                            float(params["window_width"]),
                            float(params["window_level"]),
                            int(params["interp_order"]))
    return render_slab(model, params["plane"],
                       float(params["plane_position"]), thickness,
                       float(params["window_width"]),
                       float(params["window_level"]),
                       int(params["interp_order"]))


def _recompute_reading(annotation: dict, spacing: dict) -> dict:
    axes = ("X", "Y", "Z")
    p0, p1 = annotation["endpoints_world"]
    delta = [p1[i] - p0[i] for i in range(3)]
    world_length = float(np.sqrt(sum(d * d for d in delta)))
    voxel_length = float(np.sqrt(sum(
        (d / spacing[a]) ** 2 for d, a in zip(delta, axes))))
    endpoints_voxel = [
        [float(c) / float(spacing[a]) for c, a in zip(point, axes)]
        for point in annotation["endpoints_world"]
    ]
    return {"world_length": world_length, "voxel_length": voxel_length,
            "endpoints_voxel": endpoints_voxel}


def verify_snapshot(snapshot_path: str, samples_dir: str) -> dict:
    with open(snapshot_path, "r", encoding="utf-8") as fh:
        snapshot = json.load(fh)
    volume_id = snapshot["volume_id"]
    volume_dir = os.path.join(samples_dir, volume_id)
    params = snapshot["params"]

    # --- plane image, pixel by pixel -----------------------------------
    expected = _recompute_plane(volume_dir, params)
    plane_file = snapshot["plane_image"]["file"]
    recorded = np.load(os.path.join(os.path.dirname(snapshot_path), plane_file),
                       allow_pickle=False)
    if recorded.shape != expected.shape:
        return {"ok": False, "snapshot": snapshot_path,
                "plane": params["plane"], "field": "shape",
                "recorded": list(recorded.shape),
                "recomputed": list(expected.shape)}
    diff = np.argwhere(recorded != expected)
    if diff.size:
        row, col = (int(v) for v in diff[0])
        return {"ok": False, "snapshot": snapshot_path,
                "plane": params["plane"], "pixel": [row, col],
                "field": "display_gray",
                "recorded": int(recorded[row, col]),
                "recomputed": int(expected[row, col])}

    # --- annotations, value by value -------------------------------------
    model = VolumeModel.load(volume_dir)
    spacing = model.manifest.spacing
    for annotation in snapshot.get("annotations", []):
        recomputed = _recompute_reading(annotation, spacing)
        for field in ("world_length", "voxel_length"):
            recorded_value = float(annotation[field])
            if abs(recorded_value - recomputed[field]) > 1e-9:
                return {"ok": False, "snapshot": snapshot_path,
                        "annotation_id": annotation["id"], "field": field,
                        "recorded": recorded_value,
                        "recomputed": recomputed[field]}
        recorded_voxel = np.asarray(annotation["endpoints_voxel"], dtype=float)
        if not np.allclose(recorded_voxel,
                           np.asarray(recomputed["endpoints_voxel"]),
                           rtol=0.0, atol=1e-9):
            return {"ok": False, "snapshot": snapshot_path,
                    "annotation_id": annotation["id"],
                    "field": "endpoints_voxel",
                    "recorded": recorded_voxel.tolist(),
                    "recomputed": recomputed["endpoints_voxel"]}
    return {"ok": True, "snapshot": snapshot_path,
            "plane": params["plane"],
            "annotations": len(snapshot.get("annotations", []))}


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="volview snapshot verifier")
    parser.add_argument("--snapshots", default="data/snapshots")
    parser.add_argument("--samples", default="data/samples")
    args = parser.parse_args(argv)
    paths = sorted(glob.glob(os.path.join(args.snapshots, "snapshot_*.json")))
    if not paths:
        print(json.dumps({"ok": False, "error": "no snapshots found",
                          "dir": args.snapshots}))
        return 1
    results = []
    for path in paths:
        result = verify_snapshot(path, args.samples)
        results.append(result)
        if not result["ok"]:
            print(json.dumps({"ok": False, "first_mismatch": result,
                              "verified": len(results) - 1},
                             indent=2, sort_keys=True))
            return 1
    print(json.dumps({"ok": True, "verified": len(results),
                      "snapshots": [r["snapshot"] for r in results]},
                     indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
