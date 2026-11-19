#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import shutil
from pathlib import Path
from typing import Any

from backup_chain.common import atomic_write_json, load_manifest, utc_now


def find_backup(manifest: Any, sequence: int) -> Any:
    if sequence == 0:
        return manifest.full
    for increment in manifest.increments:
        if increment.sequence == sequence:
            return increment
    raise SystemExit(f"increment sequence {sequence} is not in the manifest")


def physical_target(repo: Path, backup: Any, block_id: str | None) -> tuple[Any, Any]:
    blocks = sorted(backup.blocks, key=lambda item: (item.ordinal, item.block_id))
    if block_id:
        blocks = [block for block in blocks if block.block_id == block_id]
    for block in blocks:
        replicas = sorted(block.replicas, key=lambda item: (-item.priority, item.rel_path))
        for replica in replicas:
            path = repo / replica.rel_path
            if path.is_file() and not path.is_symlink():
                return block, replica
    raise SystemExit("no existing declared replica matches the injection target")


def append_log(repo: Path, event: dict[str, Any], chain_id: str) -> Path:
    path = repo / "injections.json"
    if path.exists():
        try:
            value = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            value = {"events": []}
    else:
        value = {"events": []}
    events = value.get("events")
    if not isinstance(events, list):
        events = []
    max_id = 0
    for item in events:
        if isinstance(item, dict):
            try:
                max_id = max(max_id, int(item.get("id", 0)))
            except (TypeError, ValueError):
                pass
    event["id"] = max_id + 1
    event["at"] = utc_now()
    events.append(event)
    value["events"] = events
    value.setdefault("schema_version", "backup-chain-injections/v1")
    value.setdefault("chain_id", chain_id)
    atomic_write_json(path, value)
    return path


def inject(args: argparse.Namespace) -> dict[str, Any]:
    repo: Path = args.repo.resolve()
    manifest_path = args.manifest or (repo / "manifest.json")
    manifest = load_manifest(manifest_path)
    backup = find_backup(manifest, args.sequence)

    if args.mode == "duplicate":
        source_partition = args.partition
        if not source_partition:
            first_block = sorted(
                backup.blocks, key=lambda item: (item.ordinal, item.block_id)
            )[0]
            source_partition = sorted(
                first_block.replicas, key=lambda item: (-item.priority, item.rel_path)
            )[0].partition
        source = repo / source_partition
        if not source.is_dir():
            raise SystemExit(f"partition does not exist: {source_partition}")
        destination_name = args.destination or (
            f"p-batch-copy-{len(list(repo.glob('p-batch-copy-*'))) + 1:02d}"
        )
        destination = repo / destination_name
        if destination.exists():
            raise SystemExit(f"destination already exists: {destination_name}")
        shutil.copytree(source, destination, symlinks=False)
        affected = sorted(
            (
                {
                    "backup_id": backup.backup_id,
                    "block_id": block.block_id,
                }
                for block in backup.blocks
            ),
            key=lambda item: item["block_id"],
        )
        event = {
            "affected": affected,
            "destination": destination_name,
            "note": "whole partition batch-copied; copies are undeclared in the manifest",
            "source": source_partition,
            "type": "directory_batch_copy",
        }
        log_path = append_log(repo, event, manifest.chain_id)
        return {
            "affected": affected,
            "destination": str(destination),
            "injection_log": str(log_path),
            "mode": args.mode,
            "sequence": args.sequence,
        }

    if args.mode == "priority":
        affected = []
        updated = 0
        data = json.loads(manifest_path.read_text(encoding="utf-8"))
        section = (
            data["full"]
            if args.sequence == 0
            else next(
                item
                for item in data["increments"]
                if int(item["sequence"]) == args.sequence
            )
        )
        for block in section["blocks"]:
            replicas = block.get("replicas") or []
            if len(replicas) < 2:
                continue
            priorities = [int(replica.get("priority", index + 1)) for index, replica in enumerate(replicas)]
            for replica, priority in zip(replicas, priorities):
                replica["priority"] = priorities[-1] + priorities[0] - priority
            updated += 1
            affected.append(
                {
                    "backup_id": backup.backup_id,
                    "block_id": block["block_id"],
                    "new_priorities": [
                        {
                            "path": replica["path"],
                            "priority": replica["priority"],
                        }
                        for replica in replicas
                    ],
                }
            )
        if not updated:
            raise SystemExit("no block in the selected backup has multiple replicas")
        atomic_write_json(manifest_path, data)
        event = {
            "affected": [
                {
                    "backup_id": item["backup_id"],
                    "block_id": item["block_id"],
                }
                for item in affected
            ],
            "note": "declared replica priorities were reordered",
            "type": "replica_priority_change",
        }
        log_path = append_log(repo, event, manifest.chain_id)
        return {
            "affected": affected,
            "injection_log": str(log_path),
            "mode": args.mode,
            "sequence": args.sequence,
            "updated_blocks": updated,
        }

    block, replica = physical_target(repo, backup, args.block_id)
    path = repo / replica.rel_path
    original_size = path.stat().st_size
    affected = [
        {"backup_id": backup.backup_id, "block_id": block.block_id}
    ]

    if args.mode == "replace":
        payload = (
            f"REPLACED|seed={args.marker}|block={block.block_id}".encode("utf-8")
        )
        with path.open("r+b") as handle:
            handle.seek(max(0, original_size // 2))
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        event_type = "content_replacement"
        detail = {"length_preserved": original_size}
    elif args.mode in {"truncate", "half-write"}:
        new_size = max(0, original_size - args.bytes)
        with path.open("r+b") as handle:
            handle.truncate(new_size)
            handle.flush()
            os.fsync(handle.fileno())
        event_type = "block_truncation"
        detail = {"original_length": original_size, "new_length": new_size}
    elif args.mode == "tail":
        insert_at = max(0, original_size - 32)
        with path.open("r+b") as handle:
            handle.seek(insert_at)
            handle.write(b"TAIL-CORRUPTION-" + b"X" * 16)
            handle.flush()
            os.fsync(handle.fileno())
        event_type = "tail_corruption"
        detail = {"length_preserved": original_size}
    elif args.mode == "missing":
        path.unlink()
        event_type = "block_removal"
        detail = {"removed_path": replica.rel_path}
    else:
        raise SystemExit(f"unsupported mode: {args.mode}")

    event = {
        "affected": affected,
        "detail": detail,
        "note": f"fault injection mode {args.mode}",
        "path": replica.rel_path,
        "type": event_type,
    }
    log_path = append_log(repo, event, manifest.chain_id)
    return {
        "affected": affected,
        "injection_log": str(log_path),
        "mode": args.mode,
        "path": str(path),
        "sequence": args.sequence,
    }


def main() -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Offline fault injector for the sample backup repository. "
            "This tool is intentionally separate from the read-only service."
        )
    )
    parser.add_argument("--repo", type=Path, required=True)
    parser.add_argument("--manifest", type=Path)
    parser.add_argument(
        "--mode",
        required=True,
        choices=(
            "replace",
            "truncate",
            "half-write",
            "tail",
            "missing",
            "priority",
            "duplicate",
        ),
    )
    parser.add_argument("--sequence", type=int, default=0)
    parser.add_argument("--block-id")
    parser.add_argument("--bytes", type=int, default=4096)
    parser.add_argument("--marker", default="fault-injection")
    parser.add_argument("--partition")
    parser.add_argument("--destination")
    args = parser.parse_args()
    print(
        json.dumps(
            inject(args), ensure_ascii=False, sort_keys=True, indent=2
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
