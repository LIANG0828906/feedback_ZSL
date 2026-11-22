"""Anomaly injection tool.

Injects one of five anomaly classes and reports which error chain fires:

* ``missing-intercept``   -> E_CAL    (manifest field ``intercept``)
* ``window-width-zero``   -> E_WIN    (parameter ``window_width``)
* ``thickness-overflow``  -> E_THICK  (parameter ``thickness``, with range)
* ``annotation-oob``      -> E_RANGE  (offending endpoint index)
* ``truncated-volume``    -> E_DECODE (payload file)

Usage: ``python -m volview.inject <case> [--samples DIR] [--work DIR]``
Prints a JSON record; exit code 0 when the expected error chain fired.
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sys

from .errors import ErrorCode, VolViewError
from .model import VolumeModel, VolumeRegistry
from .session import ReadingSession
from .snapshot import SnapshotStore

CASES = ("missing-intercept", "window-width-zero", "thickness-overflow",
         "annotation-oob", "truncated-volume")

EXPECTED = {
    "missing-intercept": (ErrorCode.E_CAL, "intercept"),
    "window-width-zero": (ErrorCode.E_WIN, "window_width"),
    "thickness-overflow": (ErrorCode.E_THICK, "thickness"),
    "annotation-oob": (ErrorCode.E_RANGE, "annotation[0].endpoint[1]"),
    "truncated-volume": (ErrorCode.E_DECODE, "volume.npy"),
}


def _copy_volume(samples_dir: str, work_dir: str, volume_id: str = "vol_a") -> str:
    src = os.path.join(samples_dir, volume_id)
    dst = os.path.join(work_dir, volume_id)
    if os.path.exists(dst):
        shutil.rmtree(dst)
    shutil.copytree(src, dst)
    return dst


def run_case(case: str, samples_dir: str, work_dir: str) -> dict:
    os.makedirs(work_dir, exist_ok=True)
    try:
        if case == "missing-intercept":
            volume_dir = _copy_volume(samples_dir, work_dir)
            manifest_path = os.path.join(volume_dir, "manifest.json")
            with open(manifest_path, "r", encoding="utf-8") as fh:
                manifest = json.load(fh)
            del manifest["intercept"]
            with open(manifest_path, "w", encoding="utf-8") as fh:
                json.dump(manifest, fh, indent=2, sort_keys=True)
            VolumeModel.load(volume_dir)

        elif case == "truncated-volume":
            volume_dir = _copy_volume(samples_dir, work_dir)
            payload = os.path.join(volume_dir, "volume.npy")
            with open(payload, "r+b") as fh:
                size = os.path.getsize(payload)
                fh.truncate(size // 2)
            VolumeModel.load(volume_dir)

        elif case == "window-width-zero":
            volume_dir = _copy_volume(samples_dir, work_dir)
            store = SnapshotStore(os.path.join(work_dir, "snapshots"))
            session = ReadingSession.open(VolumeRegistry(), volume_dir, store,
                                          "inject-window")
            session.set_window(0.0, 300.0)

        elif case == "thickness-overflow":
            volume_dir = _copy_volume(samples_dir, work_dir)
            store = SnapshotStore(os.path.join(work_dir, "snapshots"))
            session = ReadingSession.open(VolumeRegistry(), volume_dir, store,
                                          "inject-thickness")
            model = session.model
            session.set_thickness(model.extent("Z") + 100.0)

        elif case == "annotation-oob":
            volume_dir = _copy_volume(samples_dir, work_dir)
            store = SnapshotStore(os.path.join(work_dir, "snapshots"))
            session = ReadingSession.open(VolumeRegistry(), volume_dir, store,
                                          "inject-annotation")
            z = session.plane_position
            session.add_annotation([0.0, 0.0, z], [1.0e6, 0.0, z])

        else:
            return {"case": case, "ok": False, "error": "unknown case"}
    except VolViewError as exc:
        expected_code, expected_field = EXPECTED[case]
        ok = exc.code == expected_code and exc.field == expected_field
        return {"case": case, "ok": ok, "error": exc.to_dict(),
                "expected": {"code": expected_code, "field": expected_field}}
    return {"case": case, "ok": False,
            "error": "no error raised; expected " + EXPECTED[case][0]}


def main(argv=None) -> int:
    parser = argparse.ArgumentParser(description="volview anomaly injection")
    parser.add_argument("case", choices=CASES)
    parser.add_argument("--samples", default="data/samples")
    parser.add_argument("--work", default="data/inject")
    args = parser.parse_args(argv)
    result = run_case(args.case, args.samples, args.work)
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0 if result["ok"] else 1


if __name__ == "__main__":
    sys.exit(main())
