"""Byte-level comparison of two bake outputs (e.g. one-shot vs resumed)."""

import json
import os

import numpy as np

from .pipeline import CHUNK_FIELDS, _chunk_dir, _file_digest

_FIELD_FILES = {
    "positions": "positions.npy",
    "normals": "normals.npy",
    "uv": "uv.npy",
    "texture": "texture.png",
}


def _compare_field(path_a, path_b):
    with open(path_a, "rb") as fh:
        bytes_a = fh.read()
    with open(path_b, "rb") as fh:
        bytes_b = fh.read()
    if bytes_a == bytes_b:
        return None
    detail = {"byte_equal": False,
              "size_a": len(bytes_a), "size_b": len(bytes_b)}
    if path_a.endswith(".npy"):
        try:
            arr_a = np.load(path_a)
            arr_b = np.load(path_b)
            if arr_a.shape == arr_b.shape:
                detail["max_abs_diff"] = float(
                    np.max(np.abs(arr_a - arr_b))) if arr_a.size else 0.0
            else:
                detail["shape_a"] = list(arr_a.shape)
                detail["shape_b"] = list(arr_b.shape)
        except Exception as exc:  # corrupt file: report as-is
            detail["load_error"] = str(exc)
    return detail


def compare_bakes(dir_a, dir_b):
    """Compare every artifact; returns a structured mismatch report."""
    mismatches = []

    manifest_path_a = os.path.join(dir_a, "chunks.json")
    manifest_path_b = os.path.join(dir_b, "chunks.json")
    with open(manifest_path_a, "r", encoding="utf-8") as fh:
        manifest_a = json.load(fh)
    with open(manifest_path_b, "r", encoding="utf-8") as fh:
        manifest_b = json.load(fh)
    if manifest_a["grid"] != manifest_b["grid"]:
        mismatches.append({"chunk": None, "field": "grid",
                           "detail": {"a": manifest_a["grid"],
                                      "b": manifest_b["grid"]}})
        return {"ok": False, "mismatches": mismatches}

    for entry in manifest_a["chunks"]:
        cx, cy = entry["coord"]
        cdir_a = _chunk_dir(dir_a, cx, cy)
        cdir_b = _chunk_dir(dir_b, cx, cy)
        for field in CHUNK_FIELDS:
            path_a = os.path.join(cdir_a, _FIELD_FILES[field])
            path_b = os.path.join(cdir_b, _FIELD_FILES[field])
            if not (os.path.exists(path_a) and os.path.exists(path_b)):
                mismatches.append({"chunk": [cx, cy], "field": field,
                                   "detail": {"missing": True}})
                continue
            detail = _compare_field(path_a, path_b)
            if detail is not None:
                mismatches.append({"chunk": [cx, cy], "field": field,
                                   "detail": detail})

    return {"ok": not mismatches, "mismatches": mismatches}
