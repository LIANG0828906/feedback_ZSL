"""Snapshot persistence and forced-kill recovery.

Snapshots are JSON documents written to a temporary file and atomically
renamed into place.  A snapshot records the published release (material
params, version numbers, dependency factors, encoded values), the full
version history, the per-material hash chains, the release hash chain and
the UI state.

Recovery rebuilds the version sequence and UI state from the newest valid
snapshot.  A broken hash chain or a missing version field aborts with
E_RECOVERY_INVALID and locates the first broken version.
"""

from __future__ import annotations

import json
import os

from .canon import canonical_json
from .errors import ErrorCode, SpectralStudioError
from .versioning import GENESIS_HASH, MaterialVersion, Release

SNAPSHOT_FORMAT = "spectral-studio-snapshot"
SNAPSHOT_FORMAT_VERSION = 1

REQUIRED_VERSION_FIELDS = (
    "material_id", "version", "params", "dep_factors", "encoded",
    "intermediates", "prev_hash", "status", "content_hash",
)


def save_snapshot(library, path, release=None, history=None):
    """Write a snapshot atomically (temp file + rename)."""
    release = release or library.current_release
    if release is None:
        return
    history = history if history is not None else library._history
    document = {
        "format": SNAPSHOT_FORMAT,
        "format_version": SNAPSHOT_FORMAT_VERSION,
        "env": release.env_config,
        "ui_state": release.ui_state,
        "release": release.to_dict(),
        "history": {mid: [v.to_dict() for v in versions]
                    for mid, versions in sorted(history.items())},
        "material_params": {mid: library._material_params[mid]
                            for mid in sorted(library._material_params)},
    }
    tmp_path = f"{path}.tmp-{os.getpid()}"
    with open(tmp_path, "w", encoding="utf-8") as handle:
        handle.write(canonical_json(document))
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(tmp_path, path)


def _fail(message, detail=None):
    raise SpectralStudioError(ErrorCode.E_RECOVERY_INVALID, message, detail)


def _validate_version_dict(mid, vdata):
    for field in REQUIRED_VERSION_FIELDS:
        if field not in vdata:
            _fail("version record missing required field",
                  {"material_id": mid, "version": vdata.get("version"),
                   "missing_field": field})
    version = MaterialVersion.from_dict(vdata)
    if version.content_hash != vdata["content_hash"]:
        _fail("version content hash mismatch",
              {"material_id": mid, "version": vdata["version"],
               "expected": version.content_hash,
               "found": vdata["content_hash"]})
    return version


def load_snapshot(path):
    """Load and fully validate a snapshot; returns the parsed document."""
    try:
        with open(path, "r", encoding="utf-8") as handle:
            data = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        _fail("snapshot unreadable", {"path": str(path), "error": str(exc)})
    if not isinstance(data, dict) or data.get("format") != SNAPSHOT_FORMAT:
        _fail("not a spectral-studio snapshot", {"path": str(path)})
    for field in ("env", "release", "history", "material_params"):
        if field not in data:
            _fail("snapshot missing section", {"section": field})

    # Validate every historical version and each material hash chain.
    for mid in sorted(data["history"]):
        chain = data["history"][mid]
        if not chain:
            _fail("empty version chain", {"material_id": mid})
        prev_hash = GENESIS_HASH
        prev_version = 0
        for vdata in chain:
            version = _validate_version_dict(mid, vdata)
            if version.version <= prev_version:
                _fail("version numbers not monotonically increasing",
                      {"material_id": mid, "version": version.version})
            if version.prev_hash != prev_hash:
                _fail("hash chain broken",
                      {"material_id": mid, "version": version.version,
                       "expected_prev_hash": prev_hash,
                       "found_prev_hash": version.prev_hash})
            prev_hash = version.content_hash
            prev_version = version.version

    # Validate the release hash chain link.
    release_data = data["release"]
    for field in ("release_id", "prev_chain_hash", "chain_hash", "env",
                  "versions"):
        if field not in release_data:
            _fail("release record missing field", {"missing_field": field})
    release = Release.from_dict(release_data)
    if release.chain_hash != release_data["chain_hash"]:
        _fail("release chain hash mismatch",
              {"release_id": release_data["release_id"],
               "expected": release.chain_hash,
               "found": release_data["chain_hash"]})
    # Release heads must match the validated history heads.
    for mid, vdata in release_data["versions"].items():
        history = data["history"].get(mid)
        if not history or history[-1]["content_hash"] != vdata["content_hash"]:
            _fail("release head does not match version history",
                  {"material_id": mid, "version": vdata.get("version")})
    return data


def restore_into(library, data):
    """Rebuild version sequences, UI state and specs inside *library*."""
    release = Release.from_dict(data["release"])
    library._release = release
    library.ui_state = dict(data.get("ui_state", {}))
    library._history = {
        mid: [MaterialVersion.from_dict(v) for v in chain]
        for mid, chain in data["history"].items()
    }
    library._material_params = dict(data["material_params"])
    library._specs = {}
    for mid, params in library._material_params.items():
        library._specs[mid] = library._import_spec(params)
    return library
