"""Parsing, validation and version upgrade for v1/v2/v3 animation formats.

Format summary (JSON):
  v1: version, joints, tracks.rotations{joint: [[w,x,y,z], ...]}
  v2: + hierarchy{joint: parent|null}, tracks.translations{joint: [[x,y,z], ...]}
  v3: + bind_pose{joint: [16] row-major 4x4 global bind matrix},
      bone_scales{joint: float}, aliases{source_joint: target_joint}
  optional on all versions: quat_norm_tolerance (default 1e-3)

Any validation failure raises RetargetError(E_ANIM_INVALID) locating the
first offending joint and frame; no intermediate artifacts are produced.
"""

import numpy as np

from .errors import E_ANIM_INVALID, RetargetError
from .model import Animation, Skeleton
from .util import read_json

DEFAULT_QUAT_NORM_TOLERANCE = 1e-3


def _invalid(message, **details):
    raise RetargetError(E_ANIM_INVALID, message, details)


def _require(raw, key, version):
    if key not in raw:
        _invalid(f"missing required field '{key}' for format v{version}",
                 field=key, version=version)
    return raw[key]


def _check_joints(raw, version):
    joints = _require(raw, "joints", version)
    if not isinstance(joints, list) or not joints or \
            not all(isinstance(j, str) for j in joints):
        _invalid("'joints' must be a non-empty list of joint names",
                 field="joints", version=version)
    seen = set()
    for j in joints:
        if j in seen:
            _invalid("duplicate joint name", joint=j, version=version)
        seen.add(j)
    return joints


def _check_hierarchy(raw, joints, version):
    hierarchy = _require(raw, "hierarchy", version)
    if not isinstance(hierarchy, dict):
        _invalid("'hierarchy' must be an object mapping joint to parent",
                 field="hierarchy", version=version)
    joint_set = set(joints)
    for j in joints:
        if j not in hierarchy:
            _invalid("joint missing from hierarchy", joint=j,
                     field="hierarchy", version=version)
        parent = hierarchy[j]
        if parent is not None and parent not in joint_set:
            _invalid("dangling parent reference in hierarchy", joint=j,
                     parent=parent, version=version)
    for j in joints:
        seen = set()
        cur = j
        while cur is not None:
            if cur in seen:
                _invalid("cycle detected in hierarchy", joint=j, cycle_at=cur,
                         version=version)
            seen.add(cur)
            cur = hierarchy[cur]
    return hierarchy


def _check_track_table(raw, joints, version, kind, width):
    tracks = _require(raw, "tracks", version)
    if kind not in tracks:
        _invalid(f"missing track table '{kind}' for format v{version}",
                 field=f"tracks.{kind}", version=version)
    table = tracks[kind]
    result = {}
    frame_count = None
    for j in joints:
        if j not in table:
            _invalid(f"joint missing from tracks.{kind}", joint=j,
                     field=f"tracks.{kind}", version=version)
        rows = table[j]
        if frame_count is None:
            frame_count = len(rows)
            if frame_count == 0:
                _invalid("animation has zero frames", joint=j, version=version)
        elif len(rows) != frame_count:
            _invalid("track length mismatch across joints", joint=j,
                     expected_frames=frame_count, actual_frames=len(rows),
                     version=version)
        arr = np.array(rows, dtype=np.float64)
        if arr.shape != (frame_count, width):
            _invalid(f"track rows must have width {width}", joint=j,
                     field=f"tracks.{kind}", version=version)
        result[j] = arr
    return result, frame_count


def _check_quat_norms(rotations, joints, frame_count, tolerance, version):
    for j in joints:
        norms = np.linalg.norm(rotations[j], axis=1)
        dev = np.abs(norms - 1.0)
        bad = np.nonzero(dev > tolerance)[0]
        if bad.size:
            f = int(bad[0])
            _invalid("quaternion norm deviates from unit beyond declared "
                     "tolerance", joint=j, frame=f, norm=float(norms[f]),
                     deviation=float(dev[f]), tolerance=tolerance,
                     version=version)


def _check_bind_pose(raw, joints, version):
    bind = _require(raw, "bind_pose", version)
    result = {}
    for j in joints:
        if j not in bind:
            _invalid("joint missing from bind_pose", joint=j,
                     field="bind_pose", version=version)
        mat = np.array(bind[j], dtype=np.float64)
        if mat.shape != (16,):
            _invalid("bind_pose matrix must have 16 elements (row-major 4x4)",
                     joint=j, field="bind_pose", version=version)
        result[j] = mat.reshape(4, 4)
    return result


def parse_animation(path):
    """Parse and validate an animation file, upgrading to unified v3 model."""
    raw = read_json(path)
    if not isinstance(raw, dict):
        _invalid("animation file must contain a JSON object")
    version = raw.get("version")
    if version not in (1, 2, 3):
        _invalid("unsupported or missing format version",
                 field="version", version=version)

    joints = _check_joints(raw, version)
    tolerance = float(raw.get("quat_norm_tolerance",
                              DEFAULT_QUAT_NORM_TOLERANCE))

    rotations, frame_count = _check_track_table(raw, joints, version,
                                                "rotations", 4)
    _check_quat_norms(rotations, joints, frame_count, tolerance, version)

    if version >= 2:
        hierarchy = _check_hierarchy(raw, joints, version)
        translations, _ = _check_track_table(raw, joints, version,
                                             "translations", 3)
        has_translations = True
    else:
        # v1 upgrade: flat hierarchy under a virtual root, no translations.
        hierarchy = {j: None for j in joints}
        translations = None
        has_translations = False

    if version >= 3:
        bind_global = _check_bind_pose(raw, joints, version)
        bone_scales = _require(raw, "bone_scales", version)
        for j in joints:
            if j not in bone_scales:
                _invalid("joint missing from bone_scales", joint=j,
                         field="bone_scales", version=version)
        bone_scales = {j: float(bone_scales[j]) for j in joints}
        aliases = _require(raw, "aliases", version)
        if not isinstance(aliases, dict):
            _invalid("'aliases' must be an object mapping source joint to "
                     "target joint", field="aliases", version=version)
        aliases = {str(k): str(v) for k, v in aliases.items()}
        has_bind = True
    else:
        # v1/v2 upgrade: identity global bind pose, unit scales, no aliases.
        bind_global = {j: np.eye(4, dtype=np.float64) for j in joints}
        bone_scales = {}
        aliases = {}
        has_bind = False

    return Animation(
        name=str(raw.get("name", "animation")),
        version=version,
        joints=joints,
        hierarchy=hierarchy,
        rotations=rotations,
        translations=translations,
        bind_global=bind_global,
        bone_scales=bone_scales,
        aliases=aliases,
        frame_count=frame_count,
        quat_norm_tolerance=tolerance,
        has_translations=has_translations,
        has_bind_pose=has_bind,
    )


def load_target_skeleton(path):
    raw = read_json(path)
    joints = _check_joints(raw, 0)
    hierarchy = _check_hierarchy(raw, joints, 0)
    bind_global = _check_bind_pose(raw, joints, 0)
    bone_lengths = raw.get("bone_lengths", {})
    return Skeleton(
        name=str(raw.get("name", "target")),
        joints=joints,
        hierarchy=hierarchy,
        bind_global=bind_global,
        bone_lengths={str(k): float(v) for k, v in bone_lengths.items()},
    )


def topological_order(joints, hierarchy):
    """Deterministic parent-before-child ordering (Kahn, declared order)."""
    order = []
    placed = set()
    remaining = list(joints)
    while remaining:
        progressed = False
        for j in list(remaining):
            parent = hierarchy[j]
            if parent is None or parent in placed:
                order.append(j)
                placed.add(j)
                remaining.remove(j)
                progressed = True
        if not progressed:  # pragma: no cover - cycles rejected at parse time
            raise ValueError("hierarchy cycle")
    return order


def bind_local_matrices(joints, hierarchy, bind_global):
    """Derive local bind matrices from global bind matrices."""
    locals_ = {}
    for j in topological_order(joints, hierarchy):
        parent = hierarchy[j]
        g = bind_global[j]
        if parent is None:
            locals_[j] = np.array(g, dtype=np.float64)
        else:
            locals_[j] = np.linalg.inv(bind_global[parent]) @ g
    return locals_
