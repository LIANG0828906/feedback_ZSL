"""Deterministic sample generator.

Generates local JSON animation samples across joint counts, frame counts
and format versions, plus fault-injection variants (missing alias,
non-unit quaternion, dangling parent, mapping conflict, duplicate scale
scenario) and a baseline deviation summary. Everything derives from the
seed: identical seeds and operation sequences produce byte-identical
files. No timestamps or file sizes are used anywhere.
"""

import json
import math
import os
import random

from .util import content_hash, write_json

# Biped joint template, parents always precede children.
SOURCE_TEMPLATE = [
    ("Hips", None), ("Spine", "Hips"), ("Chest", "Spine"),
    ("Neck", "Chest"), ("Head", "Neck"),
    ("Shoulder_L", "Chest"), ("UpperArm_L", "Shoulder_L"),
    ("Forearm_L", "UpperArm_L"), ("Hand_L", "Forearm_L"),
    ("Shoulder_R", "Chest"), ("UpperArm_R", "Shoulder_R"),
    ("Forearm_R", "UpperArm_R"), ("Hand_R", "Forearm_R"),
    ("UpperLeg_L", "Hips"), ("LowerLeg_L", "UpperLeg_L"),
    ("Foot_L", "LowerLeg_L"),
    ("UpperLeg_R", "Hips"), ("LowerLeg_R", "UpperLeg_R"),
    ("Foot_R", "LowerLeg_R"),
]

# Target renames: some joints keep normalized-name equality (case /
# separators), some change completely and require the v3 alias table.
RENAME = {"Hips": "Pelvis", "Head": "Cranium",
          "Hand_L": "PalmLeft", "Hand_R": "PalmRight"}


def target_name(source_name):
    return RENAME.get(source_name, source_name.lower())


def _axis_angle_quat(ax, ay, az, angle):
    n = math.sqrt(ax * ax + ay * ay + az * az)
    s = math.sin(angle / 2.0) / n
    return [math.cos(angle / 2.0), ax * s, ay * s, az * s]


def _bind_matrix(rot, tx, ty, tz):
    return [rot[0], rot[1], rot[2], tx,
            rot[3], rot[4], rot[5], ty,
            rot[6], rot[7], rot[8], tz,
            0.0, 0.0, 0.0, 1.0]


def _build_skeleton_data(rng, n_joints):
    """Return (joints, hierarchy, local_offsets, bone_lengths)."""
    template = SOURCE_TEMPLATE[:n_joints]
    joints = [j for j, _ in template]
    hierarchy = {j: p for j, p in template}
    offsets, lengths = {}, {}
    for j, parent in template:
        if parent is None:
            offsets[j] = (0.0, 1.0, 0.0)
            lengths[j] = 0.0
        else:
            length = round(rng.uniform(0.10, 0.40), 6)
            dx = round(rng.uniform(-0.15, 0.15), 6)
            offsets[j] = (dx, -length if "Leg" in j or "Foot" in j
                          else length, round(rng.uniform(-0.05, 0.05), 6))
            lengths[j] = length
    return joints, hierarchy, offsets, lengths


def _global_bind(joints, hierarchy, offsets):
    """Compose global bind matrices (identity rotation) from offsets."""
    pos = {}

    def place(j):
        if j in pos:
            return pos[j]
        parent = hierarchy[j]
        if parent is None:
            pos[j] = offsets[j]
        else:
            pp = place(parent)
            off = offsets[j]
            pos[j] = (pp[0] + off[0], pp[1] + off[1], pp[2] + off[2])
        return pos[j]

    ident = [1.0, 0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 1.0]
    return {j: _bind_matrix(ident, *place(j)) for j in joints}


def gen_sample(seed, n_joints, n_frames, version, out_dir, fault=None):
    """Generate one sample directory with anim.json, target.json, meta.json."""
    rng = random.Random(seed)
    os.makedirs(out_dir, exist_ok=True)

    joints, hierarchy, offsets, lengths = _build_skeleton_data(rng, n_joints)
    if n_joints >= 12:
        # Extra source-only joint (unmatched-source demonstration).
        joints.append("Prop_L")
        hierarchy["Prop_L"] = "Hand_L"
        offsets["Prop_L"] = (0.05, 0.05, 0.0)
        lengths["Prop_L"] = 0.07

    # Deterministic animation tracks.
    axes = {j: (rng.uniform(-1, 1), rng.uniform(-1, 1), rng.uniform(-1, 1))
            for j in joints}
    amps = {j: rng.uniform(0.05, 0.45) for j in joints}
    freqs = {j: rng.choice([1, 2, 3]) for j in joints}
    phases = {j: rng.uniform(0, math.pi) for j in joints}
    rotations, translations = {}, {}
    for j in joints:
        rot_rows, tr_rows = [], []
        for f in range(n_frames):
            ang = amps[j] * math.sin(
                2 * math.pi * freqs[j] * f / n_frames + phases[j])
            rot_rows.append([round(c, 10)
                             for c in _axis_angle_quat(*axes[j], ang)])
            if hierarchy[j] is None:
                tr_rows.append([
                    round(0.002 * f, 6),
                    round(1.0 + 0.01 * math.sin(2 * math.pi * f / n_frames),
                          6),
                    0.0])
            else:
                tr_rows.append([round(v, 6) for v in offsets[j]])
        rotations[j] = rot_rows
        translations[j] = tr_rows

    # Fault: non-unit quaternion beyond the declared tolerance.
    quat_tol = 1e-3
    if fault == "nonunit_quat":
        bad_joint = "Spine" if "Spine" in joints else joints[0]
        bad_frame = min(7, n_frames - 1)
        rotations[bad_joint][bad_frame] = [
            c * (1.0 + 5e-3) for c in rotations[bad_joint][bad_frame]]

    anim = {
        "version": version,
        "name": os.path.basename(out_dir),
        "quat_norm_tolerance": quat_tol,
        "joints": joints,
        "tracks": {"rotations": rotations},
    }
    if version >= 2:
        anim["hierarchy"] = dict(hierarchy)
        anim["tracks"]["translations"] = translations
    if version >= 3:
        anim["bind_pose"] = _global_bind(joints, hierarchy, offsets)
        anim["bone_scales"] = {
            j: round(rng.uniform(0.9, 1.1), 6) for j in joints}
        anim["aliases"] = {s: target_name(s) for s in RENAME if s in joints}

    # Faults applied on top of the version-specific document.
    if fault == "dangling_parent" and version >= 2:
        victim = "Spine" if "Spine" in joints else joints[-1]
        anim["hierarchy"][victim] = "GhostJoint"
    if fault == "missing_field":
        del anim["tracks"]["rotations"][joints[-1]]
    if fault == "alias_miss" and version >= 3:
        anim["aliases"]["Head" if "Head" in joints else joints[0]] = \
            "NoSuchJoint"
    if fault == "map_conflict" and version >= 3:
        anim["aliases"]["Neck"] = anim["aliases"].get("Head", "Cranium")

    # Target skeleton: renamed joints, slightly different bone lengths,
    # plus a target-only joint that stays unmapped.
    tgt_joints = [target_name(j) for j in joints if j != "Prop_L"]
    tgt_joints.append("IKTarget")
    tgt_hierarchy = {}
    tgt_offsets = {}
    tgt_lengths = {}
    for j in joints:
        if j == "Prop_L":
            continue
        t = target_name(j)
        parent = hierarchy[j]
        tgt_hierarchy[t] = target_name(parent) if parent else None
        ratio = round(rng.uniform(0.9, 1.3), 6)
        off = offsets[j]
        tgt_offsets[t] = (off[0], round(off[1] * ratio, 6), off[2])
        tgt_lengths[t] = round(lengths[j] * ratio, 6)
    tgt_hierarchy["IKTarget"] = target_name("Hips")
    tgt_offsets["IKTarget"] = (0.3, 0.0, 0.0)
    tgt_lengths["IKTarget"] = 0.3
    target = {
        "name": "target_skeleton",
        "joints": tgt_joints,
        "hierarchy": tgt_hierarchy,
        "bind_pose": _global_bind(tgt_joints, tgt_hierarchy, tgt_offsets),
        "bone_lengths": tgt_lengths,
    }

    write_json(os.path.join(out_dir, "anim.json"), anim)
    write_json(os.path.join(out_dir, "target.json"), target)

    expect_error = {
        "nonunit_quat": "E_ANIM_INVALID",
        "dangling_parent": "E_ANIM_INVALID",
        "missing_field": "E_ANIM_INVALID",
        "map_conflict": "E_MAP_CONFLICT",
        "dup_scale": "E_SCALE_APPLIED",
    }.get(fault)
    meta = {
        "name": os.path.basename(out_dir),
        "seed": seed,
        "version": version,
        "n_joints": len(joints),
        "n_frames": n_frames,
        "fault": fault,
        "run_flags": (["--inject-scale-dup"] if fault == "dup_scale"
                      else []),
        "expect_error": expect_error,
        "anim_hash": content_hash(anim),
        "target_hash": content_hash(target),
    }
    write_json(os.path.join(out_dir, "meta.json"), meta)
    return meta


def gen_suite(seed, out_dir):
    """Generate the full deterministic verification suite."""
    os.makedirs(out_dir, exist_ok=True)
    metas = []
    combos = [
        (5, 24, 1), (5, 24, 2), (5, 24, 3),
        (12, 120, 1), (12, 120, 2), (12, 120, 3),
        (18, 240, 3),
    ]
    for i, (nj, nf, ver) in enumerate(combos):
        name = f"valid_v{ver}_j{nj}_f{nf}"
        metas.append(gen_sample(seed + i, nj, nf, ver,
                                os.path.join(out_dir, name)))
    faults = ["nonunit_quat", "dangling_parent", "missing_field",
              "alias_miss", "map_conflict", "dup_scale"]
    for i, fault in enumerate(faults):
        version = 3 if fault in ("alias_miss", "map_conflict",
                                 "dup_scale") else 2
        name = f"fault_{fault}"
        metas.append(gen_sample(seed + 100 + i, 12, 48, version,
                                os.path.join(out_dir, name), fault=fault))

    # Baseline deviation summary: predicted fixed-path orientation bound
    # (chain depth x per-node quantization angular bound) per sample.
    baseline = {"seed": seed, "samples": []}
    for meta in metas:
        anim_path = os.path.join(out_dir, meta["name"], "anim.json")
        with open(anim_path, "r", encoding="utf-8") as fh:
            anim = json.load(fh)
        hierarchy = anim.get("hierarchy",
                             {j: None for j in anim["joints"]})
        depth = 0
        for j in anim["joints"]:
            d, cur = 0, j
            while cur is not None:
                d += 1
                cur = hierarchy.get(cur)
            depth = max(depth, d)
        step = 1e-4  # default decimals=4
        predicted_deg = math.degrees(2 * step) * 2 * depth
        baseline["samples"].append({
            "name": meta["name"],
            "version": meta["version"],
            "joints": meta["n_joints"],
            "frames": meta["n_frames"],
            "fault": meta["fault"],
            "expect_error": meta["expect_error"],
            "max_chain_depth": depth,
            "quant_step": step,
            "predicted_fixed_max_deviation_deg": predicted_deg,
        })
    baseline["suite_hash"] = content_hash(baseline["samples"])
    write_json(os.path.join(out_dir, "baseline_summary.json"), baseline)
    return baseline
