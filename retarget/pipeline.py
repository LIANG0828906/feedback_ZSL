"""Dual-path (float64 / fixed-point) retargeting computation.

Pipeline stages: bind pose alignment -> joint mapping -> per-frame global
transform conversion -> bone-length scale compensation (applied exactly
once per joint) -> local rotation recovery. The fixed-point path
quantizes local rotation quaternion components and scale coefficients to
a configurable number of decimals; every quantization records pre/post
values and the rounding direction so float/fixed divergence is traceable
per joint per frame.

Error propagation model (orientation, radians): for each frame and each
mapped target joint, the global orientation error bound is the sum of
per-node contributions along the source chain (quaternion normalization
error of the input tracks) and the target chain (fixed-point
quantization step of local rotations and of bind-alignment quats).
Rotation composition worst-case amplification per node is 1.0. A joint
frame whose bound exceeds the configured tolerance is flagged
untrusted; attribution goes to the node/channel with the largest
contribution, or E_ERROR_ATTR_UNKNOWN with per-candidate shares when no
node dominates (share < 0.5).
"""

import math
from dataclasses import dataclass
from decimal import (Decimal, ROUND_DOWN, ROUND_FLOOR, ROUND_HALF_EVEN,
                     ROUND_HALF_UP)

import numpy as np

from .errors import E_ERROR_ATTR_UNKNOWN, E_SCALE_APPLIED, RetargetError
from .formats import bind_local_matrices, topological_order
from .quat import qconj, qfrom_matrix, qmul, qnormalize, qrotate
from .util import to_py

ROUNDING_MODES = {
    "half_even": ROUND_HALF_EVEN,
    "half_up": ROUND_HALF_UP,
    "down": ROUND_DOWN,      # toward zero
    "floor": ROUND_FLOOR,    # toward -inf
}


@dataclass
class Config:
    mode: str = "both"               # float | fixed | both
    decimals: int = 4                # fixed-point fractional decimals
    rounding: str = "half_even"      # rounding direction mode
    tolerance_deg: float = 0.5       # untrusted / consistency tolerance
    scale_tolerance: float = 1e-6    # scale application check threshold
    interval_frames: int = 16        # checkpoint interval length
    interval_delay_ms: int = 0       # test hook: delay between intervals
    inject_scale_dup: bool = False   # test hook: apply scale twice

    def paths(self):
        return ["float", "fixed"] if self.mode == "both" else [self.mode]

    def to_dict(self):
        return {
            "mode": self.mode,
            "decimals": self.decimals,
            "rounding": self.rounding,
            "tolerance_deg": self.tolerance_deg,
            "scale_tolerance": self.scale_tolerance,
            "interval_frames": self.interval_frames,
            "inject_scale_dup": self.inject_scale_dup,
        }

    @staticmethod
    def from_dict(d):
        return Config(
            mode=d["mode"], decimals=d["decimals"], rounding=d["rounding"],
            tolerance_deg=d["tolerance_deg"],
            scale_tolerance=d["scale_tolerance"],
            interval_frames=d["interval_frames"],
            inject_scale_dup=d.get("inject_scale_dup", False),
        )


def quantize(value, decimals, mode):
    """Quantize to `decimals` fractional decimals.

    Returns (quantized_value, direction) where direction is the actual
    rounding direction taken: 'up', 'down' or 'none'.
    """
    step = Decimal(1).scaleb(-decimals)
    quantized = Decimal(repr(float(value))).quantize(
        step, rounding=ROUNDING_MODES[mode])
    out = float(quantized)
    direction = "up" if out > value else ("down" if out < value else "none")
    return out, direction


def _ancestors_incl(joint, hierarchy):
    chain = []
    cur = joint
    while cur is not None:
        chain.append(cur)
        cur = hierarchy[cur]
    chain.reverse()
    return chain


class Precompute:
    """Shared per-run intermediates (bind alignment inputs, bone lengths)."""

    def __init__(self, anim, target, mapping):
        self.anim = anim
        self.target = target
        self.mapping = mapping
        self.topo_src = topological_order(anim.joints, anim.hierarchy)
        self.topo_tgt = topological_order(target.joints, target.hierarchy)
        self.tgt_to_src = {m["target"]: m["source"]
                           for m in mapping["mappings"]}
        self.method_of = {m["target"]: m["method"]
                          for m in mapping["mappings"]}

        # Normalized source rotations and per-frame normalization error.
        self.src_quats = {}
        self.norm_err = {}
        for j in anim.joints:
            raw = anim.rotations[j]
            norms = np.linalg.norm(raw, axis=1)
            self.norm_err[j] = np.abs(norms - 1.0)
            self.src_quats[j] = raw / norms[:, None]

        # Bind pose rotations (global) as quaternions.
        self.qb_src = {j: qfrom_matrix(anim.bind_global[j][:3, :3])
                       for j in anim.joints}
        self.qb_tgt = {t: qfrom_matrix(target.bind_global[t][:3, :3])
                       for t in target.joints}

        # Target bind locals (rotation quat + translation).
        tgt_bind_local = bind_local_matrices(target.joints, target.hierarchy,
                                             target.bind_global)
        self.tgt_bind_local_rot = {t: qfrom_matrix(m[:3, :3])
                                   for t, m in tgt_bind_local.items()}
        self.tgt_bind_local_trans = {t: np.array(m[:3, 3], dtype=np.float64)
                                     for t, m in tgt_bind_local.items()}
        self.len_tgt = {t: float(np.linalg.norm(v))
                        for t, v in self.tgt_bind_local_trans.items()}

        # Source bone lengths for length-ratio compensation.
        if anim.has_bind_pose:
            src_bind_local = bind_local_matrices(anim.joints, anim.hierarchy,
                                                 anim.bind_global)
            self.len_src = {j: float(np.linalg.norm(m[:3, 3]))
                            for j, m in src_bind_local.items()}
        elif anim.has_translations:
            self.len_src = {}
            for j in anim.joints:
                mean_len = float(np.mean(np.linalg.norm(
                    anim.translations[j], axis=1)))
                self.len_src[j] = mean_len
        else:
            self.len_src = {j: 0.0 for j in anim.joints}

        # Per-target-joint length ratio (bone-length compensation factor
        # applied to source translations; not part of the scale channel).
        self.len_ratio = {}
        for t, s in self.tgt_to_src.items():
            ls = self.len_src.get(s, 0.0)
            lt = self.len_tgt.get(t, 0.0)
            self.len_ratio[t] = lt / ls if ls > 1e-12 else 1.0

        # Declared (theoretical) scale per target joint.
        self.scale_declared = {}
        for t in target.joints:
            s = self.tgt_to_src.get(t)
            self.scale_declared[t] = (anim.bone_scales.get(s, 1.0)
                                      if s is not None else 1.0)


def compute_bind_alignment(pre, config):
    """Alignment quats per mapped target joint, per path.

    A = conj(qb_src) * qb_tgt. The fixed path quantizes components and
    records each quantization event (frame -1 = bind stage).
    """
    align = {"float": {}, "fixed": {}}
    events = []
    for t, s in pre.tgt_to_src.items():
        a = qnormalize(qmul(qconj(pre.qb_src[s]), pre.qb_tgt[t]))
        align["float"][t] = a
        if "fixed" in config.paths():
            comps = []
            for idx, comp in enumerate("wxyz"):
                qv, direction = quantize(a[idx], config.decimals,
                                         config.rounding)
                events.append({
                    "frame": -1, "joint": t, "stage": "bind_alignment",
                    "component": comp, "pre": a[idx], "post": qv,
                    "direction": direction, "rounding": config.rounding,
                })
                comps.append(qv)
            align["fixed"][t] = np.array(comps, dtype=np.float64)
        else:
            align["fixed"][t] = a
    return align, events


def _source_fk(pre, frame):
    g = {}
    for j in pre.topo_src:
        local = pre.src_quats[j][frame]
        parent = pre.anim.hierarchy[j]
        g[j] = local if parent is None else qnormalize(qmul(g[parent], local))
    return g


def compute_interval(pre, config, align, frame_start, frame_end):
    """Compute one checkpoint interval. Returns the commit payload."""
    paths = config.paths()
    step = 10.0 ** (-config.decimals)
    quant_bound = 2.0 * step  # angular bound per quantized quat (radians)

    poses = {p: [] for p in paths}
    quant_events = []
    error_records = {p: [] for p in paths}
    scale_records = {p: [] for p in paths}

    anim, target = pre.anim, pre.target

    for f in range(frame_start, frame_end):
        g_src = _source_fk(pre, f)
        truth = {}
        for t, s in pre.tgt_to_src.items():
            truth[t] = qnormalize(qmul(g_src[s], align["float"][t]))

        for path in paths:
            g_tgt, pos = {}, {}
            frame_pose = {
                "frame": f,
                "local_rot": {}, "global_rot": {}, "global_pos": {},
                "local_trans": {}, "scale": {},
                "truth_global_rot": {t: to_py(q) for t, q in truth.items()},
            }
            for t in pre.topo_tgt:
                parent = target.hierarchy[t]
                g_parent = g_tgt[parent] if parent is not None else None
                s = pre.tgt_to_src.get(t)
                if s is not None:
                    a = align[path][t]
                    g_des = qnormalize(qmul(g_src[s], a))
                    if g_parent is None:
                        local = g_des
                    else:
                        local = qnormalize(qmul(qconj(g_parent), g_des))
                    if path == "fixed":
                        comps = []
                        for idx, comp in enumerate("wxyz"):
                            qv, direction = quantize(
                                local[idx], config.decimals, config.rounding)
                            quant_events.append({
                                "frame": f, "joint": t, "stage": "local_rot",
                                "component": comp, "pre": float(local[idx]),
                                "post": qv, "direction": direction,
                                "rounding": config.rounding,
                            })
                            comps.append(qv)
                        local = np.array(comps, dtype=np.float64)
                else:
                    local = pre.tgt_bind_local_rot[t]

                g_t = (qnormalize(qmul(g_parent, local))
                       if g_parent is not None else qnormalize(local))
                g_tgt[t] = g_t

                # Bone-length scale: applied exactly once to this joint's
                # own bone (its local translation), never accumulated down
                # the parent chain.
                if s is not None and anim.has_translations:
                    base = anim.translations[s][f] * pre.len_ratio[t]
                else:
                    base = pre.tgt_bind_local_trans[t]
                declared = pre.scale_declared[t]
                if path == "fixed":
                    intended, direction = quantize(declared, config.decimals,
                                                   config.rounding)
                    quant_events.append({
                        "frame": f, "joint": t, "stage": "scale",
                        "component": "scale", "pre": declared,
                        "post": intended, "direction": direction,
                        "rounding": config.rounding,
                    })
                else:
                    intended = declared
                applied = intended * intended if config.inject_scale_dup \
                    else intended
                scaled = base * applied

                base_norm = float(np.linalg.norm(base))
                actual = (float(np.linalg.norm(scaled)) / base_norm
                          if base_norm > 1e-12 else intended)
                apply_diff = abs(actual - intended)
                scale_rec = {
                    "frame": f, "joint": t, "path": path,
                    "declared": declared, "intended": intended,
                    "actual": actual, "apply_diff": apply_diff,
                    "declared_diff": abs(actual - declared),
                }
                scale_records[path].append(scale_rec)
                if apply_diff > config.scale_tolerance:
                    raise RetargetError(
                        E_SCALE_APPLIED,
                        "applied bone scale deviates from theoretical scale",
                        {"joint": t, "frame": f, "path": path,
                         "actual": actual, "theoretical": intended,
                         "diff": apply_diff})

                if g_parent is None:
                    pos[t] = np.array(scaled, dtype=np.float64)
                else:
                    pos[t] = pos[parent] + qrotate(g_parent, scaled)

                frame_pose["local_rot"][t] = to_py(local)
                frame_pose["global_rot"][t] = to_py(g_t)
                frame_pose["global_pos"][t] = to_py(pos[t])
                frame_pose["local_trans"][t] = to_py(scaled)
                frame_pose["scale"][t] = intended
            poses[path].append(frame_pose)

        # Error propagation per mapped target joint (both paths share the
        # source-chain normalization channel; fixed adds quantization).
        for path in paths:
            for t, s in pre.tgt_to_src.items():
                entries = []  # (node, channel, contribution)
                for u in _ancestors_incl(s, anim.hierarchy):
                    entries.append((f"src:{u}", "quat_normalization",
                                    float(pre.norm_err[u][f])))
                if path == "fixed":
                    for u in _ancestors_incl(t, target.hierarchy):
                        if u in pre.tgt_to_src:
                            entries.append((f"tgt:{u}", "quantization",
                                            quant_bound))
                            entries.append((f"tgt:{u}", "bind_alignment",
                                            quant_bound))
                total = sum(c for _, _, c in entries)
                bound_deg = math.degrees(total)
                untrusted = bound_deg > config.tolerance_deg
                channels = {}
                for _, channel, contrib in entries:
                    channels[channel] = channels.get(channel, 0.0) + contrib
                for rec in scale_records[path]:
                    if rec["frame"] == f and rec["joint"] == t:
                        channels["scale"] = rec["declared_diff"]
                if total > 0.0:
                    ranked = sorted(entries, key=lambda e: e[2],
                                    reverse=True)
                    top_node, top_channel, top_contrib = ranked[0]
                    if top_contrib / total >= 0.5:
                        attribution = {"status": "ok", "node": top_node,
                                       "channel": top_channel,
                                       "share": top_contrib / total}
                    else:
                        attribution = {
                            "status": "unknown",
                            "code": E_ERROR_ATTR_UNKNOWN,
                            "candidates": [
                                {"node": n, "channel": c,
                                 "share": v / total}
                                for n, c, v in ranked[:3]
                            ],
                        }
                else:
                    attribution = {"status": "ok", "node": None,
                                   "channel": None, "share": 0.0}
                error_records[path].append({
                    "frame": f, "joint": t,
                    "mapping": pre.method_of[t],
                    "bound_deg": bound_deg, "untrusted": untrusted,
                    "attribution": attribution, "channels": channels,
                })

    return {
        "frame_start": frame_start,
        "frame_end": frame_end - 1,
        "poses": poses,
        "quant_events": quant_events,
        "error_records": error_records,
        "scale_records": scale_records,
    }
