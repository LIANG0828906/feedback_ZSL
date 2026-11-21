"""Source-to-target joint mapping.

Priority: v3 alias table > normalized joint name > normalized hierarchy
path. Every mapping records its method and confidence level. An alias
entry that misses the target skeleton is reported as unmatched (with
nearest mapped ancestor candidates) and never silently falls back.
Multiple source joints claiming the same target joint abort with
E_MAP_CONFLICT.
"""

import re

from .errors import E_MAP_CONFLICT, RetargetError

CONFIDENCE = {"alias": "high", "name": "medium", "path": "low", "none": "none"}


def normalize_name(name):
    return re.sub(r"[^a-z0-9]", "", name.lower())


def _hierarchy_path(joint, hierarchy):
    path = []
    cur = joint
    while cur is not None:
        path.append(cur)
        cur = hierarchy[cur]
    path.reverse()
    return tuple(normalize_name(j) for j in path)


def _ancestors(joint, hierarchy):
    cur = hierarchy[joint]
    while cur is not None:
        yield cur
        cur = hierarchy[cur]


def build_mapping(anim, target):
    """Return a mapping report dict; raise E_MAP_CONFLICT on collisions."""
    target_by_norm = {}
    for t in target.joints:
        target_by_norm.setdefault(normalize_name(t), []).append(t)
    target_by_path = {}
    for t in target.joints:
        target_by_path.setdefault(_hierarchy_path(t, target.hierarchy),
                                  []).append(t)

    mappings = []
    unmatched = []
    mapped_source = {}

    for s in anim.joints:
        method = None
        tgt = None
        if anim.aliases and s in anim.aliases:
            # Alias table takes priority; a miss must not silently fall back.
            alias_target = anim.aliases[s]
            if alias_target in target.joints:
                method, tgt = "alias", alias_target
            else:
                unmatched.append({
                    "source": s,
                    "reason": "alias_miss",
                    "alias_target": alias_target,
                    "ancestor_candidates": [],
                })
                mapped_source[s] = None
                continue
        if method is None:
            norm = normalize_name(s)
            hits = target_by_norm.get(norm, [])
            if len(hits) == 1:
                method, tgt = "name", hits[0]
        if method is None:
            path = _hierarchy_path(s, anim.hierarchy)
            hits = target_by_path.get(path, [])
            if len(hits) == 1:
                method, tgt = "path", hits[0]
        if method is None:
            unmatched.append({
                "source": s,
                "reason": "no_name_or_path_match",
                "ancestor_candidates": [],
            })
            mapped_source[s] = None
        else:
            mappings.append({
                "source": s,
                "target": tgt,
                "method": method,
                "confidence": CONFIDENCE[method],
            })
            mapped_source[s] = tgt

    # Nearest mapped ancestor candidates for unmatched source joints.
    for entry in unmatched:
        candidates = []
        for anc in _ancestors(entry["source"], anim.hierarchy):
            if mapped_source.get(anc):
                candidates.append({"source_ancestor": anc,
                                   "target": mapped_source[anc]})
        entry["ancestor_candidates"] = candidates

    # Conflict: several source joints claiming the same target joint.
    by_target = {}
    for m in mappings:
        by_target.setdefault(m["target"], []).append(m["source"])
    conflicts = {t: srcs for t, srcs in by_target.items() if len(srcs) > 1}
    if conflicts:
        raise RetargetError(
            E_MAP_CONFLICT,
            "multiple source joints map to the same target joint",
            {"conflicts": [
                {"target": t, "sources": srcs}
                for t, srcs in sorted(conflicts.items())
            ]},
        )

    return {
        "mappings": mappings,
        "unmatched": unmatched,
        "stats": {
            "source_joints": len(anim.joints),
            "mapped": len(mappings),
            "unmatched": len(unmatched),
            "by_method": {
                method: sum(1 for m in mappings if m["method"] == method)
                for method in ("alias", "name", "path")
            },
        },
    }
