from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable

from .common import (
    BC_BLOCK_CONFLICT,
    BC_BLOCK_DIGEST_MISMATCH,
    BC_BLOCK_LENGTH_MISMATCH,
    BC_BLOCK_MISSING,
    BC_BLOCK_ORDINAL_MISMATCH,
    BC_BLOCK_TAIL_MISMATCH,
    BC_BLOCK_UNCONFIRMED,
    BC_CHAIN_BASELINE_MISMATCH,
    BC_CHAIN_SEQUENCE_GAP,
    BC_CHAIN_STATE_DIGEST_MISMATCH,
    BC_MANIFEST_DIRECTORY_MISMATCH,
    BC_MANIFEST_UNDECLARED_COPY,
    BC_UNINDEXED_FILE,
    BusinessError,
    BlockSpec,
    FileObservation,
    Manifest,
    hash_file,
    load_injection_log,
    load_manifest,
    normalize_digest,
    stable_state_digest,
)


@dataclass
class AuditOutcome:
    catalog: dict[str, Any]
    blocks: list[dict[str, Any]]
    restore_points: list[dict[str, Any]]
    restore_point_blocks: dict[int, list[dict[str, Any]]]
    breakpoint: dict[str, Any] | None
    errors: list[dict[str, Any]]
    injection_impact: dict[str, Any] | None
    unusable_blocks: list[dict[str, Any]] = field(default_factory=list)

    def summary(self) -> dict[str, Any]:
        return {
            "block_count": len(self.blocks),
            "breakpoint": self.breakpoint,
            "chain_id": self.catalog["chain_id"],
            "conflict_blocks": self.catalog["conflict_blocks"],
            "half_written_blocks": self.catalog["half_written_blocks"],
            "increment_count": self.catalog["increment_count"],
            "last_usable_sequence": self.catalog["last_usable_sequence"],
            "manifest_sha256": self.catalog["manifest_sha256"],
            "missing_blocks": self.catalog["missing_blocks"],
            "status": self.catalog["status"],
            "undeclared_copies": self.catalog["undeclared_copies"],
            "unconfirmed_blocks": self.catalog["unconfirmed_blocks"],
            "usable_restore_points": self.catalog["usable_restore_points"],
        }


def _error(
    *,
    code: str,
    message: str,
    manifest: Manifest,
    sequence: int | None,
    backup_id: str | None,
    block_id: str | None,
    path: str | None = None,
    detail: Any = None,
    severity: str = "error",
) -> dict[str, Any]:
    return {
        "backup_id": backup_id,
        "block_id": block_id,
        "chain_id": manifest.chain_id,
        "code": code,
        "detail": detail,
        "message": message,
        "path": path,
        "sequence": sequence,
        "severity": severity,
    }


def _sort_errors(errors: Iterable[dict[str, Any]]) -> list[dict[str, Any]]:
    return sorted(
        errors,
        key=lambda item: (
            item.get("sequence") is None,
            item.get("sequence") or 0,
            item.get("block_id") or "",
            item.get("code") or "",
            item.get("path") or "",
            item.get("message") or "",
        ),
    )


def _candidate_order(candidate: dict[str, Any]) -> tuple[int, str, str]:
    return (
        int(candidate.get("priority", 1_000_000_000)),
        str(candidate.get("path") or ""),
        str(candidate.get("sha256") or ""),
    )


def _candidate_classification(
    spec: BlockSpec, observation: FileObservation
) -> tuple[str, dict[str, bool]]:
    checks = {
        "length": observation.size == spec.length,
        "tail": (
            True
            if spec.tail_sha256 is None
            else observation.tail_sha256 == spec.tail_sha256
        ),
        "sha256": observation.sha256 == spec.sha256,
    }
    if not checks["length"]:
        return "length_mismatch", checks
    if not checks["tail"]:
        return "tail_mismatch", checks
    if not checks["sha256"]:
        return "digest_mismatch", checks
    return "valid", checks


def _scan_repository(
    repo: Path, manifest: Manifest, declared_paths: dict[str, tuple[str, str]]
) -> tuple[dict[tuple[str, str], list[str]], list[dict[str, Any]]]:
    """Return undeclared physical paths grouped by (backup_id, block_id).

    A file is associated with a logical backup by an exact declared path or by
    a directory component equal to a backup_id.  Ambiguous files are reported
    as unindexed instead of being guessed into any block.
    """

    block_owners: dict[str, set[str]] = {}
    for backup in (manifest.full, *manifest.increments):
        for block in backup.blocks:
            block_owners.setdefault(block.block_id, set()).add(backup.backup_id)

    actual: dict[tuple[str, str], list[str]] = {}
    errors: list[dict[str, Any]] = []
    declared_rel = set(declared_paths)
    for root, dirs, files in os.walk(repo, followlinks=False):
        dirs.sort()
        for name in sorted(files):
            full_path = Path(root) / name
            try:
                if full_path.is_symlink() or not full_path.is_file():
                    continue
            except OSError:
                continue
            try:
                rel = full_path.relative_to(repo).as_posix()
            except ValueError:
                continue
            if rel in declared_rel:
                backup_id, block_id = declared_paths[rel]
                actual.setdefault((backup_id, block_id), []).append(rel)
                continue
            if not name.endswith(manifest.block_file_suffix):
                continue
            block_id = name[: -len(manifest.block_file_suffix)]
            owners = block_owners.get(block_id)
            if not owners:
                errors.append(
                    _error(
                        code=BC_UNINDEXED_FILE,
                        message="data file does not match any manifest block_id",
                        manifest=manifest,
                        sequence=None,
                        backup_id=None,
                        block_id=block_id,
                        path=rel,
                    )
                )
                continue
            components = set(Path(rel).parts[:-1])
            matching_owners = sorted(owners.intersection(components))
            if len(matching_owners) != 1:
                errors.append(
                    _error(
                        code=BC_UNINDEXED_FILE,
                        message=(
                            "data file cannot be associated with exactly one "
                            "manifest backup_id"
                        ),
                        manifest=manifest,
                        sequence=None,
                        backup_id=None,
                        block_id=block_id,
                        path=rel,
                        detail={"candidate_backup_ids": matching_owners},
                    )
                )
                continue
            actual.setdefault((matching_owners[0], block_id), []).append(rel)
    for paths in actual.values():
        paths.sort()
    return actual, errors


def _evaluate_blocks(
    repo: Path,
    manifest: Manifest,
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    backups = (manifest.full, *manifest.increments)
    declared_paths: dict[str, tuple[str, str]] = {}
    specs: list[BlockSpec] = []
    for backup in backups:
        for block in backup.blocks:
            specs.append(block)
            for replica in block.replicas:
                existing = declared_paths.get(replica.rel_path)
                if existing and existing != (backup.backup_id, block.block_id):
                    raise BusinessError(
                        BC_MANIFEST_DIRECTORY_MISMATCH,
                        "one physical path is declared for different logical blocks",
                        chain_id=manifest.chain_id,
                        backup_id=backup.backup_id,
                        block_id=block.block_id,
                        path=replica.rel_path,
                        other=existing[0],
                    )
                declared_paths[replica.rel_path] = (
                    backup.backup_id,
                    block.block_id,
                )

    undeclared_map, scan_errors = _scan_repository(
        repo, manifest, declared_paths
    )
    observation_cache: dict[str, FileObservation | tuple[str, str]] = {}
    errors: list[dict[str, Any]] = list(scan_errors)
    results: list[dict[str, Any]] = []

    def observe(rel_path: str) -> FileObservation | None:
        cached = observation_cache.get(rel_path)
        if isinstance(cached, FileObservation):
            return cached
        if isinstance(cached, tuple):
            return None
        full_path = repo / rel_path
        if not full_path.is_file() or full_path.is_symlink():
            observation_cache[rel_path] = ("missing", "")
            return None
        try:
            observed = hash_file(full_path)
        except (OSError, BusinessError) as exc:
            observation_cache[rel_path] = ("read_error", str(exc))
            return None
        observation_cache[rel_path] = FileObservation(
            rel_path=rel_path,
            partition=rel_path.split("/", 1)[0],
            size=observed.size,
            sha256=observed.sha256,
            tail_sha256=observed.tail_sha256,
            mtime_ns=observed.mtime_ns,
        )
        return observation_cache[rel_path]  # type: ignore[return-value]

    for spec in specs:
        candidates: list[dict[str, Any]] = []
        block_errors: list[dict[str, Any]] = []
        declared_rel_paths = {replica.rel_path for replica in spec.replicas}
        declared_existing = 0
        half_written = False

        for replica in sorted(
            spec.replicas, key=lambda item: (item.priority, item.rel_path)
        ):
            observation = observe(replica.rel_path)
            base_candidate = {
                "declared": True,
                "exists": observation is not None,
                "length": observation.size if observation else None,
                "mtime_ns": observation.mtime_ns if observation else None,
                "partition": replica.partition,
                "path": replica.rel_path,
                "priority": replica.priority,
                "sha256": observation.sha256 if observation else None,
                "tail_sha256": observation.tail_sha256 if observation else None,
            }
            if observation is None:
                cached = observation_cache.get(replica.rel_path)
                reason = cached[1] if isinstance(cached, tuple) else "not found"
                candidate = {
                    **base_candidate,
                    "checks": {
                        "length": False,
                        "tail": False,
                        "sha256": False,
                    },
                    "classification": "missing",
                }
                candidates.append(candidate)
                block_errors.append(
                    _error(
                        code=BC_BLOCK_MISSING,
                        message="a declared replica is missing or unreadable",
                        manifest=manifest,
                        sequence=spec.sequence,
                        backup_id=spec.backup_id,
                        block_id=spec.block_id,
                        path=replica.rel_path,
                        detail={"reason": reason},
                    )
                )
                continue

            declared_existing += 1
            classification, checks = _candidate_classification(spec, observation)
            candidate = {
                **base_candidate,
                "checks": checks,
                "classification": classification,
            }
            candidates.append(candidate)
            if classification == "length_mismatch":
                half_written = True
                block_errors.append(
                    _error(
                        code=BC_BLOCK_LENGTH_MISMATCH,
                        message="declared replica length does not match manifest",
                        manifest=manifest,
                        sequence=spec.sequence,
                        backup_id=spec.backup_id,
                        block_id=spec.block_id,
                        path=replica.rel_path,
                        detail={
                            "expected_length": spec.length,
                            "actual_length": observation.size,
                        },
                    )
                )
            elif classification == "tail_mismatch":
                half_written = True
                block_errors.append(
                    _error(
                        code=BC_BLOCK_TAIL_MISMATCH,
                        message="declared replica tail digest does not match manifest",
                        manifest=manifest,
                        sequence=spec.sequence,
                        backup_id=spec.backup_id,
                        block_id=spec.block_id,
                        path=replica.rel_path,
                        detail={
                            "expected_tail_sha256": spec.tail_sha256,
                            "actual_tail_sha256": observation.tail_sha256,
                        },
                    )
                )
            elif classification == "digest_mismatch":
                block_errors.append(
                    _error(
                        code=BC_BLOCK_DIGEST_MISMATCH,
                        message="declared replica content digest does not match manifest",
                        manifest=manifest,
                        sequence=spec.sequence,
                        backup_id=spec.backup_id,
                        block_id=spec.block_id,
                        path=replica.rel_path,
                        detail={
                            "expected_sha256": spec.sha256,
                            "actual_sha256": observation.sha256,
                        },
                    )
                )

        undeclared_paths = sorted(
            path
            for path in undeclared_map.get((spec.backup_id, spec.block_id), [])
            if path not in declared_rel_paths
        )
        for index, rel_path in enumerate(undeclared_paths):
            observation = observe(rel_path)
            if observation is None:
                continue
            classification, checks = _candidate_classification(spec, observation)
            candidates.append(
                {
                    "checks": checks,
                    "classification": classification,
                    "declared": False,
                    "exists": True,
                    "length": observation.size,
                    "mtime_ns": observation.mtime_ns,
                    "partition": (
                        rel_path.split("/", 1)[0]
                        if "/" in rel_path
                        else "."
                    ),
                    "path": rel_path,
                    "priority": 1_000_000 + index,
                    "sha256": observation.sha256,
                    "tail_sha256": observation.tail_sha256,
                }
            )
            if classification == "length_mismatch":
                half_written = True
                block_errors.append(
                    _error(
                        code=BC_BLOCK_LENGTH_MISMATCH,
                        message=(
                            "an undeclared physical copy has a length mismatch"
                        ),
                        manifest=manifest,
                        sequence=spec.sequence,
                        backup_id=spec.backup_id,
                        block_id=spec.block_id,
                        path=rel_path,
                        detail={
                            "expected_length": spec.length,
                            "actual_length": observation.size,
                            "declared": False,
                        },
                    )
                )
            elif classification == "tail_mismatch":
                half_written = True
                block_errors.append(
                    _error(
                        code=BC_BLOCK_TAIL_MISMATCH,
                        message=(
                            "an undeclared physical copy has a tail-digest mismatch"
                        ),
                        manifest=manifest,
                        sequence=spec.sequence,
                        backup_id=spec.backup_id,
                        block_id=spec.block_id,
                        path=rel_path,
                        detail={
                            "expected_tail_sha256": spec.tail_sha256,
                            "actual_tail_sha256": observation.tail_sha256,
                            "declared": False,
                        },
                    )
                )
            block_errors.append(
                _error(
                    code=BC_MANIFEST_UNDECLARED_COPY,
                    message=(
                        "a physical copy exists but is not declared in the "
                        "manifest; it is observable but not selectable"
                    ),
                    manifest=manifest,
                    sequence=spec.sequence,
                    backup_id=spec.backup_id,
                    block_id=spec.block_id,
                    path=rel_path,
                    severity="warning",
                )
            )

        candidates.sort(key=_candidate_order)
        actual_digests = {
            candidate["sha256"]
            for candidate in candidates
            if candidate.get("exists") and candidate.get("sha256")
        }
        conflict = len(actual_digests) > 1
        if conflict:
            block_errors.append(
                _error(
                    code=BC_BLOCK_CONFLICT,
                    message=(
                        "multiple physical copies have different content digests"
                    ),
                    manifest=manifest,
                    sequence=spec.sequence,
                    backup_id=spec.backup_id,
                    block_id=spec.block_id,
                    detail={"digests": sorted(actual_digests)},
                )
            )

        valid_declared = [
            candidate
            for candidate in candidates
            if candidate["declared"]
            and candidate.get("exists")
            and candidate["classification"] == "valid"
        ]
        selected = valid_declared[0] if valid_declared else None
        missing = declared_existing == 0
        if selected:
            status = "valid"
            primary_code = BC_BLOCK_CONFLICT if conflict else None
        elif missing:
            status = "missing"
            primary_code = BC_BLOCK_MISSING
        else:
            status = "unconfirmed"
            primary_code = None

        if not selected:
            classes = {
                candidate["classification"]
                for candidate in candidates
                if candidate.get("exists")
            }
            if status == "missing":
                code = BC_BLOCK_MISSING
                message = "no declared replica exists for the logical block"
            elif "length_mismatch" in classes:
                code = BC_BLOCK_LENGTH_MISMATCH
                message = "the logical block has no uniquely confirmable version"
            elif "tail_mismatch" in classes:
                code = BC_BLOCK_TAIL_MISMATCH
                message = "the logical block has no tail-digest-confirmable version"
            elif "digest_mismatch" in classes:
                code = BC_BLOCK_DIGEST_MISMATCH
                message = "the logical block has no manifest-digest-confirmable version"
            else:
                code = BC_BLOCK_UNCONFIRMED
                message = "the logical block has no uniquely confirmable version"
            block_errors.append(
                _error(
                    code=code,
                    message=message,
                    manifest=manifest,
                    sequence=spec.sequence,
                    backup_id=spec.backup_id,
                    block_id=spec.block_id,
                    detail={"candidate_classifications": sorted(classes)},
                )
            )
            primary_code = code

        selected_public = None
        if selected:
            selected_public = {
                "length": selected["length"],
                "partition": selected["partition"],
                "path": selected["path"],
                "priority": selected["priority"],
                "sha256": selected["sha256"],
                "tail_sha256": selected["tail_sha256"],
            }
        result = {
            "backup_id": spec.backup_id,
            "block_id": spec.block_id,
            "candidates": candidates,
            "codes": sorted({item["code"] for item in block_errors}),
            "conflict": conflict,
            "expected": {
                "length": spec.length,
                "sha256": spec.sha256,
                "tail_sha256": spec.tail_sha256,
            },
            "half_written": half_written,
            "kind": spec.kind,
            "missing": missing,
            "ordinal": spec.ordinal,
            "primary_code": primary_code,
            "selection_rule": (
                "declared-replica priority, then path; selected replica must "
                "match length, tail digest and manifest content digest"
            ),
            "selected": selected_public,
            "sequence": spec.sequence,
            "status": status,
        }
        results.append(result)
        errors.extend(block_errors)

    results.sort(key=lambda item: (item["sequence"], item["ordinal"], item["block_id"]))
    return results, _sort_errors(errors)


def _public_state_block(
    block: dict[str, Any],
    *,
    block_id: str,
    ordinal: int,
    origin_sequence: int,
    origin_backup_id: str,
) -> dict[str, Any]:
    return {
        "block_id": block_id,
        "length": int(block["length"]),
        "ordinal": int(ordinal),
        "origin_backup_id": origin_backup_id,
        "origin_sequence": origin_sequence,
        "partition": block["partition"],
        "path": block["path"],
        "priority": int(block["priority"]),
        "sha256": block["sha256"],
        "tail_sha256": block["tail_sha256"],
    }


def _point_block(block: dict[str, Any]) -> dict[str, Any]:
    return {
        "block_id": block["block_id"],
        "length": int(block["length"]),
        "ordinal": int(block["ordinal"]),
        "origin_backup_id": block["origin_backup_id"],
        "origin_sequence": int(block["origin_sequence"]),
        "partition": block["partition"],
        "path": block["path"],
        "priority": int(block["priority"]),
        "sha256": block["sha256"],
        "tail_sha256": block["tail_sha256"],
    }


def _breakpoint(
    *,
    kind: str,
    sequence: int,
    backup_id: str | None,
    block_id: str | None,
    code: str,
    message: str,
    after_usable_sequence: int | None,
) -> dict[str, Any]:
    return {
        "after_usable_sequence": after_usable_sequence,
        "backup_id": backup_id,
        "block_id": block_id,
        "code": code,
        "kind": kind,
        "message": message,
        "sequence": sequence,
    }


def _evaluate_injection_log(
    repo: Path,
    manifest: Manifest,
    block_map: dict[tuple[str, str], dict[str, Any]],
) -> dict[str, Any] | None:
    log = load_injection_log(repo, manifest)
    if log is None:
        return None
    if "error" in log:
        return {"error": log["error"], "event": None}
    events = log.get("events")
    if not isinstance(events, list) or not events:
        return None
    parsed: list[tuple[int, dict[str, Any]]] = []
    for index, event in enumerate(events):
        if not isinstance(event, dict):
            continue
        try:
            event_id = int(event.get("id", index + 1))
        except (TypeError, ValueError):
            event_id = index + 1
        parsed.append((event_id, event))
    parsed.sort(key=lambda item: item[0])
    event_id, event = parsed[-1]
    affected = event.get("affected")
    if not isinstance(affected, list):
        affected = []
    affected = sorted(
        (item for item in affected if isinstance(item, dict)),
        key=lambda item: (
            str(item.get("backup_id") or ""),
            str(item.get("block_id") or ""),
        ),
    )
    observed = []
    for target in affected:
        if not isinstance(target, dict):
            continue
        backup_id = str(target.get("backup_id") or "")
        block_id = str(target.get("block_id") or "")
        block = block_map.get((backup_id, block_id))
        if block is None:
            observed.append(
                {
                    "backup_id": backup_id,
                    "block_id": block_id,
                    "codes": [],
                    "conflict": False,
                    "half_written": False,
                    "missing": True,
                    "status": "not_in_manifest",
                }
            )
            continue
        observed.append(
            {
                "backup_id": backup_id,
                "block_id": block_id,
                "codes": block["codes"],
                "conflict": block["conflict"],
                "half_written": block["half_written"],
                "missing": block["missing"],
                "status": block["status"],
            }
        )
    observed.sort(
        key=lambda item: (item["backup_id"], item["block_id"])
    )
    counts = {
        "affected": len(observed),
        "conflicts": sum(1 for item in observed if item["conflict"]),
        "half_written": sum(1 for item in observed if item["half_written"]),
        "missing": sum(1 for item in observed if item["missing"]),
        "unconfirmed": sum(
            1 for item in observed if item["status"] == "unconfirmed"
        ),
    }
    return {
        "event": {
            "affected": affected,
            "at": event.get("at"),
            "id": event_id,
            "note": event.get("note"),
            "type": event.get("type"),
        },
        "observed": {
            "blocks": observed,
            "counts": counts,
            "effects_observed": any(
                item["conflict"]
                or item["half_written"]
                or item["missing"]
                or item["status"] != "valid"
                for item in observed
            ),
        },
    }


def audit_repository(repo: Path, manifest_path: Path) -> AuditOutcome:
    manifest = load_manifest(manifest_path)
    blocks, errors = _evaluate_blocks(repo, manifest)
    block_map = {
        (block["backup_id"], block["block_id"]): block for block in blocks
    }

    restore_points: list[dict[str, Any]] = []
    point_blocks: dict[int, list[dict[str, Any]]] = {}
    breakpoint: dict[str, Any] | None = None
    state: dict[str, dict[str, Any]] = {}
    after_usable = -1

    full_results = [
        block_map[(manifest.full.backup_id, block.block_id)]
        for block in manifest.full.blocks
    ]
    full_invalid = next(
        (block for block in full_results if block["status"] != "valid"), None
    )
    if full_invalid:
        breakpoint = _breakpoint(
            kind="baseline",
            sequence=0,
            backup_id=manifest.full.backup_id,
            block_id=full_invalid["block_id"],
            code=full_invalid["primary_code"] or BC_BLOCK_UNCONFIRMED,
            message="the full baseline has no uniquely confirmable block version",
            after_usable_sequence=None,
        )
    else:
        for block in manifest.full.blocks:
            selected = block_map[
                (manifest.full.backup_id, block.block_id)
            ]["selected"]
            state[block.block_id] = _public_state_block(
                selected,
                block_id=block.block_id,
                ordinal=block.ordinal,
                origin_sequence=0,
                origin_backup_id=manifest.full.backup_id,
            )
        state_list = sorted(
            state.values(), key=lambda item: (item["ordinal"], item["block_id"])
        )
        digest = stable_state_digest(state_list)
        if (
            manifest.full.expected_state_digest
            and digest != manifest.full.expected_state_digest
        ):
            breakpoint = _breakpoint(
                kind="baseline",
                sequence=0,
                backup_id=manifest.full.backup_id,
                block_id=state_list[0]["block_id"] if state_list else None,
                code=BC_CHAIN_STATE_DIGEST_MISMATCH,
                message="reconstructed full state digest does not match manifest",
                after_usable_sequence=None,
            )
        else:
            restore_points.append(
                {
                    "backup_id": manifest.full.backup_id,
                    "block_count": len(state_list),
                    "kind": "full",
                    "sequence": 0,
                    "state_digest": digest,
                    "usable": True,
                }
            )
            point_blocks[0] = [_point_block(item) for item in state_list]
            after_usable = 0

    if breakpoint is None:
        previous_state_digest = stable_state_digest(
            sorted(state.values(), key=lambda item: (item["ordinal"], item["block_id"]))
        )
        expected_sequence = 1
        for increment in manifest.increments:
            if increment.sequence != expected_sequence:
                breakpoint = _breakpoint(
                    kind="sequence_gap",
                    sequence=expected_sequence,
                    backup_id=increment.backup_id,
                    block_id=increment.blocks[0].block_id,
                    code=BC_CHAIN_SEQUENCE_GAP,
                    message=(
                        "increment sequence is not contiguous with the chain"
                    ),
                    after_usable_sequence=after_usable,
                )
                break
            expected_sequence += 1
            if (
                increment.baseline_digest is None
                or increment.baseline_digest != previous_state_digest
            ):
                first_block = sorted(
                    increment.blocks, key=lambda item: (item.ordinal, item.block_id)
                )[0]
                breakpoint = _breakpoint(
                    kind="increment",
                    sequence=increment.sequence,
                    backup_id=increment.backup_id,
                    block_id=first_block.block_id,
                    code=BC_CHAIN_BASELINE_MISMATCH,
                    message=(
                        "increment baseline digest does not match the previously "
                        "selected logical state"
                    ),
                    after_usable_sequence=after_usable,
                )
                break

            increment_results = [
                block_map[(increment.backup_id, block.block_id)]
                for block in sorted(
                    increment.blocks, key=lambda item: (item.ordinal, item.block_id)
                )
            ]
            invalid = next(
                (
                    block
                    for block in increment_results
                    if block["status"] != "valid"
                ),
                None,
            )
            if invalid:
                breakpoint = _breakpoint(
                    kind="increment",
                    sequence=increment.sequence,
                    backup_id=increment.backup_id,
                    block_id=invalid["block_id"],
                    code=invalid["primary_code"] or BC_BLOCK_UNCONFIRMED,
                    message=(
                        "increment has a missing, half-written or "
                        "digest-unconfirmed block"
                    ),
                    after_usable_sequence=after_usable,
                )
                break

            candidate_state = dict(state)
            ordinal_error: dict[str, Any] | None = None
            for block in increment.blocks:
                selected = block_map[
                    (increment.backup_id, block.block_id)
                ]["selected"]
                replacement = _public_state_block(
                    selected,
                    block_id=block.block_id,
                    ordinal=block.ordinal,
                    origin_sequence=increment.sequence,
                    origin_backup_id=increment.backup_id,
                )
                existing = candidate_state.get(block.block_id)
                if existing and existing["ordinal"] != replacement["ordinal"]:
                    ordinal_error = {
                        "block_id": block.block_id,
                        "expected_ordinal": existing["ordinal"],
                        "actual_ordinal": replacement["ordinal"],
                    }
                    break
                candidate_state[block.block_id] = replacement
            if ordinal_error:
                breakpoint = _breakpoint(
                    kind="increment",
                    sequence=increment.sequence,
                    backup_id=increment.backup_id,
                    block_id=ordinal_error["block_id"],
                    code=BC_BLOCK_ORDINAL_MISMATCH,
                    message="an increment changed a logical block's ordinal",
                    after_usable_sequence=after_usable,
                )
                break
            ordinal_values = [
                item["ordinal"] for item in candidate_state.values()
            ]
            if len(ordinal_values) != len(set(ordinal_values)):
                breakpoint = _breakpoint(
                    kind="increment",
                    sequence=increment.sequence,
                    backup_id=increment.backup_id,
                    block_id=increment.blocks[0].block_id,
                    code=BC_BLOCK_ORDINAL_MISMATCH,
                    message="multiple logical blocks claim the same ordinal",
                    after_usable_sequence=after_usable,
                )
                break
            candidate_list = sorted(
                candidate_state.values(),
                key=lambda item: (item["ordinal"], item["block_id"]),
            )
            candidate_digest = stable_state_digest(candidate_list)
            if (
                increment.expected_state_digest
                and increment.expected_state_digest != candidate_digest
            ):
                breakpoint = _breakpoint(
                    kind="increment",
                    sequence=increment.sequence,
                    backup_id=increment.backup_id,
                    block_id=increment.blocks[0].block_id,
                    code=BC_CHAIN_STATE_DIGEST_MISMATCH,
                    message="increment result state digest does not match manifest",
                    after_usable_sequence=after_usable,
                )
                break
            state = candidate_state
            previous_state_digest = candidate_digest
            after_usable = increment.sequence
            restore_points.append(
                {
                    "backup_id": increment.backup_id,
                    "block_count": len(candidate_list),
                    "kind": "increment",
                    "sequence": increment.sequence,
                    "state_digest": candidate_digest,
                    "usable": True,
                }
            )
            point_blocks[increment.sequence] = [
                _point_block(item) for item in candidate_list
            ]

    if breakpoint is None:
        catalog_status = (
            "usable_with_faults"
            if any(
                block["status"] != "valid"
                or block["conflict"]
                or block["half_written"]
                or block["missing"]
                or any(
                    not candidate["declared"]
                    for candidate in block["candidates"]
                )
                for block in blocks
            )
            else "usable"
        )
    else:
        catalog_status = "degraded"

    block_map_for_injection = {
        (block["backup_id"], block["block_id"]): block for block in blocks
    }
    injection_impact = _evaluate_injection_log(
        repo,
        manifest,
        block_map_for_injection,
    )
    unusable = [
        {
            "backup_id": block["backup_id"],
            "block_id": block["block_id"],
            "codes": block["codes"],
            "conflict": block["conflict"],
            "half_written": block["half_written"],
            "missing": block["missing"],
            "ordinal": block["ordinal"],
            "sequence": block["sequence"],
            "status": block["status"],
        }
        for block in blocks
        if block["status"] != "valid"
        or block["conflict"]
        or block["half_written"]
        or block["missing"]
    ]
    catalog = {
        "block_count": len(blocks),
        "breakpoint": breakpoint,
        "chain_id": manifest.chain_id,
        "conflict_blocks": sum(1 for block in blocks if block["conflict"]),
        "half_written_blocks": sum(
            1 for block in blocks if block["half_written"]
        ),
        "increment_count": len(manifest.increments),
        "last_usable_sequence": (
            restore_points[-1]["sequence"] if restore_points else None
        ),
        "manifest_path": str(manifest_path),
        "manifest_sha256": manifest.raw_sha256,
        "missing_blocks": sum(1 for block in blocks if block["missing"]),
        "status": catalog_status,
        "undeclared_copies": sum(
            1
            for block in blocks
            for candidate in block["candidates"]
            if not candidate["declared"]
        ),
        "unconfirmed_blocks": sum(
            1 for block in blocks if block["status"] == "unconfirmed"
        ),
        "usable_restore_points": [
            point["sequence"] for point in restore_points
        ],
    }
    return AuditOutcome(
        catalog=catalog,
        blocks=blocks,
        restore_points=restore_points,
        restore_point_blocks=point_blocks,
        breakpoint=breakpoint,
        errors=errors,
        injection_impact=injection_impact,
        unusable_blocks=unusable,
    )
