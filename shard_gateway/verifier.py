"""Independent session/manifest consistency verifier."""

from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any

from .errors import (
    E_MANIFEST_INCOMPLETE,
    E_METADATA_CORRUPT,
    E_SHARD_DIGEST_MISMATCH,
)
from .integrity import (
    manifest_shard_issues,
    object_digest_from_session,
)
from .storage import UploadGateway
from .util import load_json


def verify(root: str | Path) -> dict[str, Any]:
    gateway = UploadGateway(root)
    session_records: dict[str, tuple[dict[str, Any], Path]] = {}
    issues: list[dict[str, Any]] = []

    for directory in sorted(path for path in gateway.sessions_dir.iterdir() if path.is_dir()):
        meta_path = directory / "meta.json"
        if not meta_path.is_file():
            continue
        try:
            meta = gateway._read_meta_unlocked(directory.name)
        except Exception as exc:
            issues.append(
                {
                    "code": E_METADATA_CORRUPT,
                    "session_id": directory.name,
                    "message": str(exc),
                }
            )
            continue
        session_records[directory.name] = (meta, directory)

    manifests: list[tuple[Path, dict[str, Any]]] = []
    for path in sorted(gateway.manifests_dir.glob("*.json")):
        if not path.is_file():
            continue
        try:
            manifest = load_json(path)
        except (OSError, ValueError) as exc:
            issues.append(
                {
                    "code": E_MANIFEST_INCOMPLETE,
                    "manifest": path.name,
                    "message": f"manifest cannot be parsed: {exc}",
                }
            )
            continue
        if not isinstance(manifest, dict):
            issues.append(
                {
                    "code": E_MANIFEST_INCOMPLETE,
                    "manifest": path.name,
                    "message": "manifest root is not an object",
                }
            )
            continue
        manifests.append((path, manifest))

    manifest_session_ids = {
        manifest.get("session_id")
        for _, manifest in manifests
        if isinstance(manifest.get("session_id"), str)
    }
    objects: list[dict[str, Any]] = []
    for path, manifest in sorted(
        manifests, key=lambda item: (str(item[1].get("object_key")), str(item[0]))
    ):
        session_id = manifest.get("session_id")
        session_entry = session_records.get(session_id) if isinstance(session_id, str) else None
        session = session_entry[0] if session_entry else None
        session_dir = session_entry[1] if session_entry else None
        object_issues = manifest_shard_issues(
            manifest, session, session_dir, check_files=True
        )
        issues.extend(
            {"manifest": path.name, "session_id": session_id, **issue}
            for issue in object_issues
        )

        object_digest = None
        actual_size = None
        if session_dir is not None and session is not None:
            try:
                object_digest, actual_size = object_digest_from_session(session, session_dir)
            except (KeyError, OSError) as exc:
                issues.append(
                    {
                        "code": E_MANIFEST_INCOMPLETE,
                        "manifest": path.name,
                        "session_id": session_id,
                        "message": f"cannot assemble object: {exc}",
                    }
                )
            if object_digest is not None:
                if object_digest != manifest.get("object_sha256"):
                    issues.append(
                        {
                            "code": E_SHARD_DIGEST_MISMATCH,
                            "manifest": path.name,
                            "session_id": session_id,
                            "message": "assembled object digest differs from manifest",
                        }
                    )
                if actual_size != manifest.get("total_size"):
                    issues.append(
                        {
                            "code": E_SHARD_DIGEST_MISMATCH,
                            "manifest": path.name,
                            "session_id": session_id,
                            "message": "assembled object length differs from manifest",
                        }
                    )
        objects.append(
            {
                "object_key": manifest.get("object_key"),
                "object_version": manifest.get("object_version"),
                "session_id": session_id,
                "manifest_sha256": None,
                "object_sha256": object_digest,
                "total_size": actual_size,
                "ok": not object_issues,
            }
        )
        objects[-1]["manifest_sha256"] = hashlib.sha256(path.read_bytes()).hexdigest()

    for session_id, (meta, _) in sorted(session_records.items()):
        if (
            meta.get("status") == "committed"
            and session_id not in manifest_session_ids
        ):
            issues.append(
                {
                    "code": E_MANIFEST_INCOMPLETE,
                    "session_id": session_id,
                    "message": "committed session has no matching manifest",
                }
            )

    codes = sorted({issue["code"] for issue in issues})
    return {
        "ok": not issues,
        "objects": objects,
        "sessions": [
            {
                "session_id": session_id,
                "object_key": meta.get("object_key"),
                "status": meta.get("status"),
                "version": meta.get("version"),
                "completed_shards": len(meta.get("shards", {})),
            }
            for session_id, (meta, _) in sorted(session_records.items())
        ],
        "issues": issues,
        "error_codes": codes,
    }
