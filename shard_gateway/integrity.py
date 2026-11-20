"""Read-only manifest/session cross-checks shared by recovery and verifier."""

from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any

from .errors import (
    E_MANIFEST_INCOMPLETE,
    E_SHARD_DIGEST_MISMATCH,
)
from .util import file_size, sha256_file


def manifest_structure_issues(manifest: dict[str, Any]) -> list[dict[str, str]]:
    issues: list[dict[str, str]] = []
    total = manifest.get("total_shards")
    shards = manifest.get("shards")
    if not isinstance(total, int) or total < 1 or not isinstance(shards, list):
        return [
            {
                "code": E_MANIFEST_INCOMPLETE,
                "message": "manifest has invalid total_shards or shards",
            }
        ]
    numbers: list[int] = []
    for item in shards:
        if not isinstance(item, dict) or not isinstance(item.get("number"), int):
            issues.append(
                {
                    "code": E_MANIFEST_INCOMPLETE,
                    "message": "manifest contains a malformed shard record",
                }
            )
            continue
        numbers.append(item["number"])
    expected = list(range(1, total + 1))
    if sorted(numbers) != expected or len(numbers) != len(set(numbers)):
        issues.append(
            {
                "code": E_MANIFEST_INCOMPLETE,
                "message": "manifest shard numbers are not exactly 1..total_shards",
            }
        )
    return issues


def manifest_shard_issues(
    manifest: dict[str, Any],
    session: dict[str, Any] | None,
    session_dir: Path | None,
    *,
    check_files: bool = True,
) -> list[dict[str, str]]:
    """Cross-check a manifest against session metadata and, optionally, files."""

    issues = manifest_structure_issues(manifest)
    total = manifest.get("total_shards")
    if not isinstance(total, int) or total < 1:
        return issues
    if session is None:
        issues.append(
            {
                "code": E_MANIFEST_INCOMPLETE,
                "message": "manifest references a session that is not present",
            }
        )
        return issues

    session_shards = session.get("shards")
    if not isinstance(session_shards, dict):
        issues.append(
            {
                "code": E_MANIFEST_INCOMPLETE,
                "message": "session metadata has no shard map",
            }
        )
        return issues

    manifest_shards = manifest.get("shards")
    if not isinstance(manifest_shards, list):
        return issues

    for item in manifest_shards:
        if not isinstance(item, dict):
            continue
        number = item.get("number")
        if not isinstance(number, int):
            continue
        record = session_shards.get(str(number))
        if not isinstance(record, dict):
            issues.append(
                {
                    "code": E_MANIFEST_INCOMPLETE,
                    "message": f"manifest shard {number} is absent from session metadata",
                    "shard": str(number),
                }
            )
            continue
        expected_digest = item.get("sha256")
        expected_length = item.get("length")
        if (
            record.get("digest") != expected_digest
            or record.get("length") != expected_length
        ):
            issues.append(
                {
                    "code": E_SHARD_DIGEST_MISMATCH,
                    "message": f"manifest shard {number} differs from session metadata",
                    "shard": str(number),
                }
            )
            continue
        if not check_files or session_dir is None:
            continue
        relative = record.get("file")
        if not isinstance(relative, str):
            issues.append(
                {
                    "code": E_MANIFEST_INCOMPLETE,
                    "message": f"session shard {number} has no file reference",
                    "shard": str(number),
                }
            )
            continue
        shard_path = session_dir / relative
        if not shard_path.is_file():
            issues.append(
                {
                    "code": E_MANIFEST_INCOMPLETE,
                    "message": f"session shard {number} file is missing",
                    "shard": str(number),
                }
            )
            continue
        if file_size(shard_path) != expected_length:
            issues.append(
                {
                    "code": E_SHARD_DIGEST_MISMATCH,
                    "message": f"session shard {number} length mismatch",
                    "shard": str(number),
                }
            )
            continue
        actual_digest, _ = sha256_file(shard_path)
        if actual_digest != expected_digest:
            issues.append(
                {
                    "code": E_SHARD_DIGEST_MISMATCH,
                    "message": f"session shard {number} digest mismatch",
                    "shard": str(number),
                }
            )
    return issues


def object_digest_from_session(
    session: dict[str, Any], session_dir: Path
) -> tuple[str, int]:
    digest = hashlib.sha256()
    length = 0
    total = int(session["total_shards"])
    shards = session.get("shards", {})
    for number in range(1, total + 1):
        record = shards[str(number)]
        path = session_dir / record["file"]
        with path.open("rb") as stream:
            while True:
                block = stream.read(1024 * 1024)
                if not block:
                    break
                digest.update(block)
                length += len(block)
    return digest.hexdigest(), length
