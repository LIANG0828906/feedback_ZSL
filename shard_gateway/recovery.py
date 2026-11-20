"""Restart recovery for interrupted shard sessions."""

from __future__ import annotations

import os
from pathlib import Path
from typing import Any

from .errors import E_MANIFEST_INCOMPLETE, E_METADATA_CORRUPT, GatewayError
from .integrity import manifest_shard_issues
from .storage import UploadGateway
from .util import (
    file_size,
    load_json,
    ranges_from_numbers,
    sha256_file,
)


def _discard(path: Path) -> int:
    size = file_size(path)
    try:
        path.unlink()
    except FileNotFoundError:
        return 0
    return size


def recover(root: str | Path) -> dict[str, Any]:
    gateway = UploadGateway(root)
    sessions: list[dict[str, Any]] = []
    total_recovered = 0
    total_discarded = 0
    manifest_tmp_discarded = 0
    issues: list[dict[str, Any]] = []

    for tmp_path in sorted(gateway.manifests_dir.glob(".manifest-*.part")):
        if tmp_path.is_file():
            manifest_tmp_discarded += _discard(tmp_path)
    for tmp_path in sorted(gateway.manifests_dir.glob(".*.tmp-*")):
        if tmp_path.is_file():
            manifest_tmp_discarded += _discard(tmp_path)

    for directory in sorted(path for path in gateway.sessions_dir.iterdir() if path.is_dir()):
        session_id = directory.name
        if not (directory / "meta.json").is_file():
            continue
        discarded = 0
        recovered = 0
        changed = False
        try:
            with gateway.session_lock(session_id):
                meta = gateway._read_meta_unlocked(session_id)
                status = meta.get("status", "active")
                if (directory / "ABORTED").exists():
                    status = "aborted"
                    if meta.get("status") != "aborted":
                        meta["status"] = "aborted"
                        changed = True
                if status == "aborted" and meta.get("status") != "aborted":
                    meta["status"] = "aborted"
                    changed = True

                for path in sorted((directory / "tmp").glob("*")):
                    if path.is_file():
                        discarded += _discard(path)
                for path in sorted(directory.glob(".meta.json.tmp-*")):
                    if path.is_file():
                        discarded += _discard(path)

                shards = meta.get("shards", {})
                if not isinstance(shards, dict):
                    raise GatewayError(E_METADATA_CORRUPT, "session shard map is invalid")

                verified_files: set[str] = set()
                for number in sorted((int(key) for key in shards), reverse=True):
                    record = shards.get(str(number))
                    valid = False
                    relative = record.get("file") if isinstance(record, dict) else None
                    if isinstance(relative, str):
                        path = directory / relative
                        if path.is_file():
                            digest, length = sha256_file(path)
                            valid = (
                                digest == record.get("digest")
                                and length == record.get("length")
                            )
                            if valid:
                                verified_files.add(relative)
                            else:
                                discarded += _discard(path)
                    if valid:
                        recovered += 1
                    else:
                        del shards[str(number)]
                        changed = True

                for path in sorted((directory / "shards").glob("*")):
                    if not path.is_file():
                        continue
                    relative = f"shards/{path.name}"
                    if relative not in verified_files:
                        discarded += _discard(path)

                manifest_path = gateway._manifest_path(meta["object_key"])
                manifest: dict[str, Any] | None = None
                if manifest_path.is_file():
                    try:
                        manifest = load_json(manifest_path)
                    except (OSError, ValueError):
                        issues.append(
                            {
                                "code": E_MANIFEST_INCOMPLETE,
                                "session_id": session_id,
                                "message": "published manifest cannot be parsed",
                            }
                        )
                if (
                    status == "active"
                    and isinstance(manifest, dict)
                    and manifest.get("session_id") == session_id
                    and manifest.get("object_key") == meta["object_key"]
                ):
                    cross_issues = manifest_shard_issues(
                        manifest, meta, directory, check_files=True
                    )
                    if not cross_issues:
                        meta["status"] = "committed"
                        meta["manifest"] = manifest_path.name
                        meta["object_version"] = manifest.get("object_version")
                        meta["version"] = int(meta["version"]) + 1
                        meta["committed_manifest_sha256"] = sha256_file(manifest_path)[0]
                        status = "committed"
                        changed = True
                    else:
                        issues.extend(
                            {"session_id": session_id, **issue} for issue in cross_issues
                        )

                if status == "committed":
                    if not manifest_path.is_file():
                        issues.append(
                            {
                                "code": E_MANIFEST_INCOMPLETE,
                                "session_id": session_id,
                                "message": "committed session has no manifest",
                            }
                        )
                    elif isinstance(manifest, dict):
                        cross_issues = manifest_shard_issues(
                            manifest, meta, directory, check_files=True
                        )
                        issues.extend(
                            {"session_id": session_id, **issue} for issue in cross_issues
                        )

                if changed:
                    meta["version"] = int(meta["version"]) + 1
                    gateway._write_meta_unlocked(meta)
                status = meta.get("status", status)
                valid_numbers = sorted(int(number) for number in meta.get("shards", {}))
                continuable = (
                    []
                    if status in {"aborted", "committed"}
                    else ranges_from_numbers(valid_numbers, int(meta["total_shards"]))
                )
                completed_digests = {
                    str(number): meta["shards"][str(number)]["digest"]
                    for number in valid_numbers
                }
                sessions.append(
                    {
                        "session_id": session_id,
                        "object_key": meta["object_key"],
                        "status": status,
                        "version": meta["version"],
                        "recovered_shards": recovered,
                        "discarded_bytes": discarded,
                        "continuable_ranges": continuable,
                        "completed_digests": completed_digests,
                        "object_version": meta.get("object_version"),
                    }
                )
                total_recovered += recovered
                total_discarded += discarded
        except GatewayError as exc:
            issues.append({"session_id": session_id, **exc.as_dict()})

    return {
        "ok": not issues,
        "sessions": sessions,
        "totals": {
            "sessions": len(sessions),
            "recovered_shards": total_recovered,
            "discarded_bytes": total_discarded + manifest_tmp_discarded,
            "discarded_manifest_bytes": manifest_tmp_discarded,
        },
        "issues": issues,
    }
