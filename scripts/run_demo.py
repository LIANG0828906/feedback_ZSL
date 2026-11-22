#!/usr/bin/env python
"""End-to-end demo of the volview component stack.

Generates the deterministic sample set, runs a reading session (window,
plane, slab thickness, annotations with truth checking), persists the
snapshot, reopens it in an independent session to prove pixel-/value-exact
reproduction, exercises the five anomaly-injection chains and the
independent verifier, and renders the three planes to offscreen PGM files.

Usage:  .venv/bin/python scripts/run_demo.py [work_dir]
"""

import json
import os
import sys

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))

import numpy as np

from volview import inject, verify
from volview.generator import generate_dataset
from volview.measure import accuracy_bound
from volview.model import VolumeRegistry
from volview.session import ReadingSession
from volview.snapshot import SnapshotStore


def write_pgm(path, image):
    """Offscreen rendering: dump the display-gray plane as a binary PGM."""
    image = np.ascontiguousarray(image)
    with open(path, "wb") as fh:
        fh.write(f"P5\n{image.shape[1]} {image.shape[0]}\n255\n".encode())
        fh.write(image.tobytes())


def main():
    work = sys.argv[1] if len(sys.argv) > 1 else "data"
    samples = os.path.join(work, "samples")
    snapshots = os.path.join(work, "snapshots")
    renders = os.path.join(work, "renders")
    os.makedirs(renders, exist_ok=True)

    print("== 1. generate deterministic samples ==")
    truths = generate_dataset(samples)
    for vid, truth in truths.items():
        print(f"  {vid}: {[s['kind'] for s in truth['structures']]}")

    print("== 2. reading session with annotations ==")
    store = SnapshotStore(snapshots)
    volume_dir = os.path.join(samples, "vol_a")
    session = ReadingSession.open(VolumeRegistry(), volume_dir, store, "demo")
    session.set_window(1500.0, 300.0)
    session.set_interp_order(1)
    sphere = truths["vol_a"]["structures"][0]
    cx, cy, cz = sphere["center"]
    r = sphere["radius"]
    session.set_plane("axial", cz)
    ann = session.add_annotation([cx - r, cy, cz], [cx + r, cy, cz],
                                 true_length=2.0 * r)
    report = session.check_accuracy(ann.id, 2.0 * r)
    print(f"  sphere diameter: measured={report['measured']:.3f} mm "
          f"truth={report['true_length']:.3f} mm "
          f"deviation={report['deviation']:.3f} <= bound={report['bound']:.3f}")
    reading = session.reading(ann.id)
    print(f"  dual readout: world={reading['world_length']:.3f} mm "
          f"voxel={reading['voxel_length']:.3f} vx")
    session.commit()

    print("== 3. offscreen plane rendering (PGM) ==")
    center = session.model.center_world()
    for plane, pos in (("axial", center[2]), ("coronal", center[1]),
                       ("sagittal", center[0])):
        session.set_plane(plane, pos)
        session.set_thickness(5.0)
        image = session.render()
        path = os.path.join(renders, f"plane_{plane}.pgm")
        write_pgm(path, image)
        print(f"  {path} {image.shape}")
    session.set_plane("axial", cz)
    session.set_thickness(0.0)
    session.commit()

    print("== 4. cross-session reproduction ==")
    reopened = ReadingSession.open(VolumeRegistry(), volume_dir,
                                   SnapshotStore(snapshots), "demo")
    same_pixels = np.array_equal(session.render(), reopened.render())
    same_readings = ([a.reading() for a in session.annotations]
                     == [a.reading() for a in reopened.annotations])
    print(f"  restored={reopened.restored} pixel-identical={same_pixels} "
          f"readings-identical={same_readings}")

    print("== 5. anomaly injection chains ==")
    for case in inject.CASES:
        result = inject.run_case(case, samples, os.path.join(work, "inject"))
        err = result.get("error", {})
        print(f"  {case:18s} -> {err.get('code')} field={err.get('field')} "
              f"ok={result['ok']}")

    print("== 6. independent verification ==")
    rc = verify.main(["--snapshots", snapshots, "--samples", samples])
    print(f"  verifier exit code: {rc}")
    return 0 if (same_pixels and same_readings and rc == 0) else 1


if __name__ == "__main__":
    sys.exit(main())
