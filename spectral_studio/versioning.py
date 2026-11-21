"""Material version evolution model.

Every parameter commit becomes a new version.  A version records the
parameter set, dependency factors, encoded values per target color space,
chain intermediates and a content hash; version numbers increase
monotonically per material.  Rollback appends a new version that copies a
historical parameter set -- history is never rewritten.

A *release* is an atomically published set of material versions plus the
library environment and UI state, linked by a hash chain.
"""

from __future__ import annotations

from .canon import canon, content_hash

STATUS_PUBLISHED = "published"
STATUS_INCOMPLETE = "incomplete"


class MaterialVersion:
    def __init__(self, material_id, version, params, dep_factors, encoded,
                 intermediates, prev_hash, status=STATUS_PUBLISHED,
                 reused_from=None, recompute_reason=None, rollback_from=None):
        self.material_id = material_id
        self.version = int(version)
        self.params = canon(params)
        self.dep_factors = canon(dict(dep_factors))
        self.encoded = canon(encoded)
        self.intermediates = canon(intermediates)
        self.prev_hash = prev_hash
        self.status = status
        self.reused_from = reused_from
        self.recompute_reason = recompute_reason
        self.rollback_from = rollback_from
        self.content_hash = self._compute_hash()

    def _payload(self):
        return {
            "material_id": self.material_id,
            "version": self.version,
            "params": self.params,
            "dep_factors": self.dep_factors,
            "encoded": self.encoded,
            "intermediates": self.intermediates,
            "prev_hash": self.prev_hash,
            "status": self.status,
            "reused_from": self.reused_from,
            "recompute_reason": self.recompute_reason,
            "rollback_from": self.rollback_from,
        }

    def _compute_hash(self):
        return content_hash(self._payload())

    def to_dict(self):
        payload = self._payload()
        payload["content_hash"] = self.content_hash
        return canon(payload)

    @classmethod
    def from_dict(cls, data):
        return cls(
            material_id=data["material_id"],
            version=data["version"],
            params=data["params"],
            dep_factors=data["dep_factors"],
            encoded=data["encoded"],
            intermediates=data["intermediates"],
            prev_hash=data["prev_hash"],
            status=data.get("status", STATUS_PUBLISHED),
            reused_from=data.get("reused_from"),
            recompute_reason=data.get("recompute_reason"),
            rollback_from=data.get("rollback_from"),
        )


def diff_versions(old: MaterialVersion, new: MaterialVersion) -> dict:
    """Field-level diff between two versions of one material."""
    diff = {"material_id": new.material_id,
            "from_version": old.version, "to_version": new.version}
    changed_params = {}
    for key in sorted(set(old.params) | set(new.params)):
        before, after = old.params.get(key), new.params.get(key)
        if canon(before) != canon(after):
            changed_params[key] = {"before": before, "after": after}
    if changed_params:
        diff["params"] = changed_params
    changed_factors = {}
    for key in sorted(set(old.dep_factors) | set(new.dep_factors)):
        before, after = old.dep_factors.get(key), new.dep_factors.get(key)
        if before != after:
            changed_factors[key] = {"before": before, "after": after}
    if changed_factors:
        diff["dep_factors"] = changed_factors
    changed_encoded = {}
    for key in sorted(set(old.encoded) | set(new.encoded)):
        before, after = old.encoded.get(key), new.encoded.get(key)
        if canon(before) != canon(after):
            changed_encoded[key] = {"before": before, "after": after}
    if changed_encoded:
        diff["encoded"] = changed_encoded
    if old.content_hash != new.content_hash:
        diff["content_hash"] = {"before": old.content_hash,
                                "after": new.content_hash}
    return diff


class Release:
    """An atomically published set of material versions."""

    def __init__(self, release_id, versions, env_config, ui_state,
                 prev_chain_hash, trace_note=None):
        self.release_id = int(release_id)
        self.versions = dict(versions)  # material_id -> MaterialVersion
        self.env_config = env_config
        self.ui_state = ui_state or {}
        self.prev_chain_hash = prev_chain_hash
        self.trace_note = trace_note
        self.chain_hash = self._compute_chain_hash()

    def _compute_chain_hash(self):
        return content_hash({
            "release_id": self.release_id,
            "prev_chain_hash": self.prev_chain_hash,
            "env": self.env_config,
            "ui_state": self.ui_state,
            "versions": {mid: v.content_hash for mid, v in
                         sorted(self.versions.items())},
        })

    def to_dict(self):
        return canon({
            "release_id": self.release_id,
            "prev_chain_hash": self.prev_chain_hash,
            "chain_hash": self.chain_hash,
            "env": self.env_config,
            "ui_state": self.ui_state,
            "versions": {mid: v.to_dict() for mid, v in
                         sorted(self.versions.items())},
        })

    @classmethod
    def from_dict(cls, data):
        versions = {mid: MaterialVersion.from_dict(vdata)
                    for mid, vdata in data["versions"].items()}
        return cls(
            release_id=data["release_id"],
            versions=versions,
            env_config=data["env"],
            ui_state=data.get("ui_state", {}),
            prev_chain_hash=data["prev_chain_hash"],
        )


GENESIS_HASH = content_hash({"genesis": "spectral-studio"})
