"""Incremental rebake: affected-set computation (requirement 6)."""
from __future__ import annotations

from .pipeline import global_param_hash, material_param_hash


def compute_param_version(config, materials) -> dict:
    return {
        "global": global_param_hash(config),
        "materials": {m.material_id: material_param_hash(m) for m in materials},
    }


def compute_affected_set(old_version: dict | None, new_version: dict) -> tuple[list[str], dict]:
    """Return (affected_material_ids, reasons).

    Conservative rule: whenever we cannot prove a sphere is unaffected,
    it is rebaked and the reason is recorded.
    """
    reasons: dict[str, str] = {}
    if old_version is None:
        return list(new_version["materials"]), {"*": "no_previous_batch"}
    if "global" not in old_version or "materials" not in old_version:
        return list(new_version["materials"]), {"*": "provenance_missing"}
    if old_version["global"] != new_version["global"]:
        # illuminant / white point / observer / adaptation / targets changed
        return list(new_version["materials"]), {"*": "global_params_changed"}
    affected = []
    old_materials = old_version["materials"]
    for material_id, new_hash in new_version["materials"].items():
        old_hash = old_materials.get(material_id)
        if old_hash is None:
            affected.append(material_id)
            reasons[material_id] = "new_material"
        elif old_hash != new_hash:
            affected.append(material_id)
            reasons[material_id] = "material_params_changed"
    removed = set(old_materials) - set(new_version["materials"])
    for material_id in removed:
        reasons[material_id] = "removed_material"
    return affected, reasons
