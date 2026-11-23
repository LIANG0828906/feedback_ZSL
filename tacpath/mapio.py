"""Tactical map loading and validation.

Validation failures raise E_SCHEMA (with the offending field name) or
E_POINT (invalid start/goal/probe). Corrupted files raise E_DECODE.
A failed load never mutates the repository's already-loaded maps.
"""

import hashlib
import json
import os

import numpy as np

from . import fp
from .errors import E_DECODE, E_POINT, E_SCHEMA, TacError
from .mapgen import TERRAIN_WATER

_REQUIRED_MAP_FIELDS = ["id", "width", "height", "costs", "terrain",
                        "pairs", "probes", "truth"]
_REQUIRED_TRUTH_FIELDS = ["cost_fp", "path_len", "corners", "probes"]


def _schema(field, message):
    return TacError(E_SCHEMA, f"{message} (field={field})",
                    {"field": field})


class MapData:
    def __init__(self, raw, source_path):
        self.raw = raw
        self.source_path = source_path
        self.id = raw["id"]
        self.width = raw["width"]
        self.height = raw["height"]
        self.costs = np.asarray(raw["costs"], dtype=np.int64)
        self.terrain = raw["terrain"]
        self.pairs = [(tuple(p["start"]), tuple(p["goal"]))
                      for p in raw["pairs"]]
        self.probes = [tuple(p) for p in raw["probes"]]
        self.truth = raw["truth"]
        self.neighborhood = raw.get("neighborhood", 8)
        self.max_cell_cost = int(self.costs.max())
        self.content_hash = hashlib.sha256(
            json.dumps({"costs": raw["costs"], "pairs": raw["pairs"],
                        "probes": raw["probes"]},
                       sort_keys=True).encode()).hexdigest()


def _validate_int_grid(values, width, height, field):
    if not isinstance(values, list) or len(values) != height:
        raise _schema(field, "grid row count does not match height")
    for y, row in enumerate(values):
        if not isinstance(row, list) or len(row) != width:
            raise _schema(f"{field}[{y}]",
                          "grid row length does not match width")
        for x, v in enumerate(row):
            if isinstance(v, bool) or not isinstance(v, int):
                raise _schema(f"{field}[{y}][{x}]",
                              "cell value must be an integer")


def _validate_point(point, width, height, costs, role):
    if (not isinstance(point, (list, tuple)) or len(point) != 2
            or any(isinstance(c, bool) or not isinstance(c, int)
                   for c in point)):
        raise TacError(E_POINT, f"{role} must be an [x, y] integer pair",
                       {"role": role, "point": point})
    x, y = point
    if not (0 <= x < width and 0 <= y < height):
        raise TacError(E_POINT, f"{role} out of bounds",
                       {"role": role, "point": list(point)})
    if costs[y][x] == 0:
        raise TacError(E_POINT, f"{role} on impassable cell",
                       {"role": role, "point": list(point)})


def validate_map(raw):
    """Validate a decoded map document; raises E_SCHEMA / E_POINT."""
    for field in _REQUIRED_MAP_FIELDS:
        if field not in raw:
            raise _schema(field, "missing required field")
    width, height = raw["width"], raw["height"]
    for name, v in (("width", width), ("height", height)):
        if isinstance(v, bool) or not isinstance(v, int) or v <= 0:
            raise _schema(name, "dimension must be a positive integer")

    _validate_int_grid(raw["costs"], width, height, "costs")
    _validate_int_grid(raw["terrain"], width, height, "terrain")

    costs = raw["costs"]
    terrain = raw["terrain"]
    for y in range(height):
        for x in range(width):
            c = costs[y][x]
            if not 0 <= c <= fp.MAX_CELL_COST:
                raise _schema(f"costs[{y}][{x}]",
                              f"cost {c} outside [0, {fp.MAX_CELL_COST}]")
            t = terrain[y][x]
            if not 0 <= t <= 3:
                raise _schema(f"terrain[{y}][{x}]",
                              f"unknown terrain class {t}")
            if (c == 0) != (t == TERRAIN_WATER):
                raise _schema(
                    f"costs[{y}][{x}]",
                    "impassable marker inconsistent: cost==0 must match "
                    "terrain==WATER")

    pairs = raw["pairs"]
    if not isinstance(pairs, list) or not pairs:
        raise _schema("pairs", "need a non-empty list of start/goal pairs")
    for i, pair in enumerate(pairs):
        if not isinstance(pair, dict) \
                or "start" not in pair or "goal" not in pair:
            raise _schema(f"pairs[{i}]", "pair needs start and goal")
        _validate_point(pair["start"], width, height, costs,
                        f"pairs[{i}].start")
        _validate_point(pair["goal"], width, height, costs,
                        f"pairs[{i}].goal")

    for i, probe in enumerate(raw["probes"]):
        _validate_point(probe, width, height, costs, f"probes[{i}]")

    truth = raw["truth"]
    if not isinstance(truth, dict):
        raise _schema("truth", "truth must be an object keyed by pair index")
    for i in range(len(pairs)):
        if str(i) not in truth:
            raise _schema(f"truth.{i}", "missing truth for pair")
        for field in _REQUIRED_TRUTH_FIELDS:
            if field not in truth[str(i)]:
                raise _schema(f"truth.{i}.{field}", "missing truth field")


def load_map(path):
    """Load and validate one map file; E_DECODE on corruption."""
    try:
        with open(path, "r") as fh:
            raw = json.load(fh)
    except (OSError, json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise TacError(E_DECODE, f"cannot decode map file: {exc}",
                       {"path": path})
    if not isinstance(raw, dict):
        raise TacError(E_DECODE, "map document is not an object",
                       {"path": path})
    validate_map(raw)
    return MapData(raw, path)


def load_manifest(path):
    try:
        with open(path, "r") as fh:
            manifest = json.load(fh)
    except (OSError, json.JSONDecodeError, UnicodeDecodeError) as exc:
        raise TacError(E_DECODE, f"cannot decode manifest: {exc}",
                       {"path": path})
    if not isinstance(manifest, dict) or "maps" not in manifest:
        raise _schema("maps", "manifest missing map list")
    for i, entry in enumerate(manifest["maps"]):
        for field in ("file", "width", "height"):
            if field not in entry:
                raise _schema(f"maps[{i}].{field}", "missing manifest field")
    return manifest


class MapRepository:
    """Loads maps listed in a manifest and cross-checks dimensions."""

    def __init__(self):
        self.maps = {}

    def load_all(self, manifest_path):
        manifest = load_manifest(manifest_path)
        base = os.path.dirname(os.path.abspath(manifest_path))
        staged = {}
        for entry in manifest["maps"]:
            m = load_map(os.path.join(base, entry["file"]))
            if m.width != entry["width"] or m.height != entry["height"]:
                raise _schema(
                    f"{entry['file']}.width/height",
                    "grid dimensions do not match the manifest")
            staged[m.id] = m
        self.maps.update(staged)  # only fully validated maps are added
        return staged
