#!/usr/bin/env python3
from __future__ import annotations

import argparse
import hashlib
import json
import os
import random
import shutil
from pathlib import Path
from typing import Any

from backup_chain.common import (
    TAIL_BYTES,
    atomic_write_json,
    disk_space_ok,
    remove_tree,
    stable_state_digest,
)


BLOCK_COUNTS = [5, 5, 5, 5, 5, 5, 5, 5, 5, 5, 4, 4, 4, 4]
SIZE_CYCLE_MB = [1.1, 1.4, 1.7, 2.0, 2.3, 3.9, 1.3, 1.9, 2.2, 2.6, 1.6, 3.2]


def size_for(index: int) -> int:
    return int(SIZE_CYCLE_MB[index % len(SIZE_CYCLE_MB)] * 1024 * 1024)


def write_chunk(
    path: Path,
    *,
    seed: int,
    backup_id: str,
    block_id: str,
    version: str,
    size: int,
) -> dict[str, Any]:
    path.parent.mkdir(parents=True, exist_ok=True)
    header = (
        f"BC1|seed={seed}|backup={backup_id}|block={block_id}|"
        f"version={version}\n"
    ).encode("utf-8")
    if len(header) > size:
        raise RuntimeError("generated chunk is smaller than its header")
    salt = hashlib.sha256(
        f"{seed}|{backup_id}|{block_id}|{version}|{size}".encode("utf-8")
    ).digest()
    pattern = hashlib.sha256(salt).digest()
    pattern = (pattern * ((TAIL_BYTES // len(pattern)) + 1))[:TAIL_BYTES]
    full = hashlib.sha256()
    tail = bytearray()
    written = 0
    with path.open("wb") as handle:
        def emit(data: bytes) -> None:
            nonlocal written, tail
            handle.write(data)
            full.update(data)
            written += len(data)
            tail.extend(data)
            if len(tail) > TAIL_BYTES:
                del tail[: len(tail) - TAIL_BYTES]

        emit(header)
        remaining = size - len(header)
        while remaining:
            count = min(len(pattern), remaining)
            emit(pattern[:count])
            remaining -= count
        if remaining or written != size:
            raise RuntimeError(
                f"short write for {path}: {written} != {size}"
            )
        handle.flush()
        os.fsync(handle.fileno())
    return {
        "length": written,
        "sha256": full.hexdigest(),
        "tail_sha256": hashlib.sha256(bytes(tail)).hexdigest(),
    }


def copy_chunk(source: Path, destination: Path) -> None:
    destination.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, destination)


def build_manifest(
    *,
    seed: int,
    chain_id: str,
    full_blocks: list[dict[str, Any]],
    increments: list[dict[str, Any]],
) -> dict[str, Any]:
    full_state = [
        {
            "block_id": block["block_id"],
            "length": block["length"],
            "ordinal": block["ordinal"],
            "sha256": block["sha256"],
        }
        for block in full_blocks
    ]
    full_digest = stable_state_digest(full_state)
    manifest = {
        "block_file_suffix": ".chunk",
        "chain_id": chain_id,
        "full": {
            "backup_id": "full-000",
            "blocks": full_blocks,
            "expected_state_digest": full_digest,
            "sequence": 0,
        },
        "generator": "generate_sample.py",
        "increments": increments,
        "schema_version": "backup-chain/v1",
        "seed": seed,
    }
    return manifest


def generate(args: argparse.Namespace) -> dict[str, Any]:
    root: Path = args.root.resolve()
    root.parent.mkdir(parents=True, exist_ok=True)
    if root.exists() and not args.force:
        raise SystemExit(
            f"refusing to overwrite {root}; pass --force to regenerate"
        )
    required = args.min_free_mb * 1024 * 1024
    _ok, free = disk_space_ok(root.parent, required)
    reclaimable = 0
    if args.force and root.exists():
        if root.is_dir():
            reclaimable = sum(
                path.stat().st_size
                for path in root.rglob("*")
                if path.is_file()
            )
        else:
            reclaimable = root.stat().st_size
    if free + reclaimable < required:
        raise SystemExit(
            f"insufficient temporary space: {free} bytes free, "
            f"{reclaimable} bytes reclaimable, {required} bytes required"
        )
    if root.exists():
        if root.is_dir():
            remove_tree(root)
        else:
            root.unlink()
    root.mkdir(parents=True)

    rng = random.Random(args.seed)
    chain_id = args.chain_id
    block_ids = [f"blk-{index:04d}" for index in range(1, 28)]
    full_blocks: list[dict[str, Any]] = []
    full_primary: dict[str, dict[str, Any]] = {}
    file_count = 0
    total_bytes = 0

    primary_partition = "p-full-primary"
    legacy_partition = "p-full-legacy"
    conflicting_full: list[str] = []

    for index, block_id in enumerate(block_ids):
        size = size_for(index)
        ordinal = index + 1
        primary_rel = f"{primary_partition}/full-000/{block_id}.chunk"
        legacy_rel = f"{legacy_partition}/full-000/{block_id}.chunk"
        primary_path = root / primary_rel
        generated = write_chunk(
            primary_path,
            seed=args.seed,
            backup_id="full-000",
            block_id=block_id,
            version="full-current",
            size=size,
        )
        file_count += 1
        total_bytes += generated["length"]
        full_primary[block_id] = {
            **generated,
            "block_id": block_id,
            "ordinal": ordinal,
        }
        full_blocks.append(
            {
                "block_id": block_id,
                "length": generated["length"],
                "ordinal": ordinal,
                "replicas": [
                    {
                        "partition": legacy_partition,
                        "path": legacy_rel,
                        "priority": 1,
                    },
                    {
                        "partition": primary_partition,
                        "path": primary_rel,
                        "priority": 2,
                    },
                ],
                "sha256": generated["sha256"],
                "tail_sha256": generated["tail_sha256"],
            }
        )

    # The migration copied the full partition twice.  Eight legacy copies hold
    # an older logical version with the same lengths but different digests.
    legacy_changed = set(rng.sample(block_ids, 8))
    for index, block_id in enumerate(block_ids):
        legacy_rel = f"{legacy_partition}/full-000/{block_id}.chunk"
        if block_id in legacy_changed:
            copy_chunk(
                root / f"{primary_partition}/full-000/{block_id}.chunk",
                root / legacy_rel,
            )
            with (root / legacy_rel).open("r+b") as handle:
                handle.seek(max(64, size_for(index) // 3))
                handle.write(
                    f"LEGACY-OLDER|{args.seed}|{block_id}|partial-change".encode(
                        "utf-8"
                    )
                )
                handle.flush()
                os.fsync(handle.fileno())
            conflicting_full.append(block_id)
        else:
            copy_chunk(
                root / f"{primary_partition}/full-000/{block_id}.chunk",
                root / legacy_rel,
            )
        file_count += 1
        total_bytes += size_for(index)

    increments: list[dict[str, Any]] = []
    current_state = {
        block_id: {
            "block_id": block_id,
            "length": value["length"],
            "ordinal": value["ordinal"],
            "sha256": value["sha256"],
        }
        for block_id, value in full_primary.items()
    }
    previous_digest = stable_state_digest(current_state.values())
    all_generated: dict[str, list[str]] = {}
    for sequence, count in enumerate(BLOCK_COUNTS, start=1):
        backup_id = f"inc-{sequence:03d}"
        partition = f"p-daily-{sequence:02d}"
        changed_ids = rng.sample(block_ids, count)
        blocks: list[dict[str, Any]] = []
        generated_ids: list[str] = []
        for order, block_id in enumerate(changed_ids):
            original = full_primary[block_id]
            size = original["length"]
            rel_path = f"{partition}/{backup_id}/{block_id}.chunk"
            generated = write_chunk(
                root / rel_path,
                seed=args.seed,
                backup_id=backup_id,
                block_id=block_id,
                version=f"daily-{sequence:02d}-{order + 1}",
                size=size,
            )
            file_count += 1
            total_bytes += generated["length"]
            blocks.append(
                {
                    "block_id": block_id,
                    "length": generated["length"],
                    "ordinal": original["ordinal"],
                    "replicas": [
                        {
                            "partition": partition,
                            "path": rel_path,
                            "priority": 1,
                        }
                    ],
                    "sha256": generated["sha256"],
                    "tail_sha256": generated["tail_sha256"],
                }
            )
            current_state[block_id] = {
                "block_id": block_id,
                "length": generated["length"],
                "ordinal": original["ordinal"],
                "sha256": generated["sha256"],
            }
            generated_ids.append(block_id)
        result_digest = stable_state_digest(current_state.values())
        increments.append(
            {
                "backup_id": backup_id,
                "baseline_digest": previous_digest,
                "blocks": blocks,
                "sequence": sequence,
                "state_digest": result_digest,
            }
        )
        all_generated[backup_id] = generated_ids
        previous_digest = result_digest

    injection_events: list[dict[str, Any]] = [
        {
            "affected": [
                {"backup_id": "full-000", "block_id": block_id}
                for block_id in sorted(conflicting_full)
            ],
            "at": f"deterministic-seed-{args.seed}",
            "id": 1,
            "note": (
                "the full partition was copied twice during migration; the "
                "legacy copy is priority 1 but does not match the manifest digest"
            ),
            "type": "baseline_partition_copy",
        }
    ]
    incident_affected: list[dict[str, str]] = []
    if args.profile == "incident":
        seq12_id = all_generated["inc-012"][0]
        seq13_id = all_generated["inc-013"][0]
        seq14_id = all_generated["inc-014"][0]
        missing_path = (
            root
            / f"p-daily-12/inc-012/{seq12_id}.chunk"
        )
        truncated_path = (
            root
            / f"p-daily-13/inc-013/{seq13_id}.chunk"
        )
        tail_path = root / f"p-daily-14/inc-014/{seq14_id}.chunk"
        missing_path.unlink()
        with truncated_path.open("r+b") as handle:
            handle.truncate(max(0, truncated_path.stat().st_size - 4096))
            handle.flush()
            os.fsync(handle.fileno())
        with tail_path.open("r+b") as handle:
            handle.seek(max(0, tail_path.stat().st_size - 16))
            handle.write(b"INTERRUPTED-TAIL")
            handle.flush()
            os.fsync(handle.fileno())
        incident_affected = [
            {"backup_id": "inc-012", "block_id": seq12_id},
            {"backup_id": "inc-013", "block_id": seq13_id},
            {"backup_id": "inc-014", "block_id": seq14_id},
        ]
        injection_events.append(
            {
                "affected": incident_affected,
                "at": f"deterministic-seed-{args.seed}",
                "id": 2,
                "note": (
                    "migration was interrupted: one missing block, one "
                    "truncated block and one tail-corrupted half-write"
                ),
                "type": "migration_interruption",
            }
        )

    manifest = build_manifest(
        seed=args.seed,
        chain_id=chain_id,
        full_blocks=full_blocks,
        increments=increments,
    )
    atomic_write_json(root / "manifest.json", manifest)
    atomic_write_json(
        root / "injections.json",
        {
            "chain_id": chain_id,
            "events": injection_events,
            "schema_version": "backup-chain-injections/v1",
        },
    )
    summary = {
        "actual_file_count": file_count,
        "actual_total_bytes": total_bytes,
        "baseline_conflict_blocks": len(conflicting_full),
        "chain_id": chain_id,
        "increment_count": len(increments),
        "manifest": str(root / "manifest.json"),
        "profile": args.profile,
        "seed": args.seed,
        "unique_block_count": len(block_ids) + sum(
            len(items) for items in all_generated.values()
        ),
    }
    atomic_write_json(root / "sample-summary.json", summary)
    return summary


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Generate a deterministic backup-chain sample repository."
    )
    parser.add_argument(
        "--root", type=Path, default=Path("sample-backup"), help="output repository root"
    )
    parser.add_argument("--seed", type=int, default=314)
    parser.add_argument("--chain-id", default="payments-weekly-chain")
    parser.add_argument(
        "--profile",
        choices=("healthy", "incident"),
        default="healthy",
        help="healthy: every increment is complete; incident: inject missing/truncated/half-write faults",
    )
    parser.add_argument("--force", action="store_true")
    parser.add_argument("--min-free-mb", type=int, default=500)
    args = parser.parse_args(argv)
    summary = generate(args)
    print(json.dumps(summary, ensure_ascii=False, sort_keys=True, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
