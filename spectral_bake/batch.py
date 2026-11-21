"""Atomic batch commit and crash recovery (requirement 7).

Each batch is written to a temporary file, fsynced, then atomically
renamed into place. Recovery only trusts on-disk manifests: the newest
valid manifest is loaded, its required fields and hash chain verified;
a broken chain or missing field aborts with E_RECOVERY_INVALID.
An optional per-sphere journal lets a re-run reuse spheres committed
before a forced kill, as long as the journal chain validates.
"""
from __future__ import annotations

import json
import os
import threading
from pathlib import Path

from .errors import RecoveryInvalid
from .pipeline import content_hash, entry_content_hash

REQUIRED_MANIFEST_FIELDS = (
    "batch_id",
    "param_version",
    "entries",
    "hash_chain",
    "quota",
    "degradation",
)

REQUIRED_ENTRY_FIELDS = (
    "material_id",
    "material_index",
    "material_param_hash",
    "codes",
    "content_hash",
)


def chain_step(prev: str, entry_hash: str) -> str:
    return content_hash(prev.encode(), b"|", entry_hash.encode())


def genesis_chain(batch_id: str) -> str:
    return content_hash(b"genesis|", batch_id.encode())


def build_hash_chain(batch_id: str, entries: list[dict]) -> list[str]:
    chain = [genesis_chain(batch_id)]
    for entry in entries:
        chain.append(chain_step(chain[-1], entry["content_hash"]))
    return chain


def validate_manifest(manifest: dict) -> None:
    for field in REQUIRED_MANIFEST_FIELDS:
        if field not in manifest:
            raise RecoveryInvalid("manifest missing field", field=field)
    entries = manifest["entries"]
    chain = manifest["hash_chain"]
    if len(chain) != len(entries) + 1:
        raise RecoveryInvalid("hash chain length mismatch", entries=len(entries), chain=len(chain))
    expected = build_hash_chain(manifest["batch_id"], entries)
    if expected != chain:
        raise RecoveryInvalid("hash chain broken", batch_id=manifest["batch_id"])
    for entry in entries:
        for field in REQUIRED_ENTRY_FIELDS:
            if field not in entry:
                raise RecoveryInvalid(
                    "entry missing field", field=field, material_id=entry.get("material_id")
                )
        expected_hash = entry_content_hash(
            entry["material_id"], entry["material_param_hash"], entry["codes"]
        )
        if expected_hash != entry["content_hash"]:
            raise RecoveryInvalid(
                "entry content hash mismatch", material_id=entry["material_id"]
            )


def commit_batch(snapshots_dir, manifest: dict) -> Path:
    """Write manifest via temp file + atomic rename."""
    snapshots = Path(snapshots_dir)
    snapshots.mkdir(parents=True, exist_ok=True)
    validate_manifest(manifest)
    final = snapshots / f"batch-{manifest['batch_id']}.json"
    tmp = snapshots / f"batch-{manifest['batch_id']}.json.tmp"
    payload = json.dumps(manifest, sort_keys=True, separators=(",", ":"))
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write(payload)
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, final)
    return final


def list_committed_batches(snapshots_dir) -> list[Path]:
    snapshots = Path(snapshots_dir)
    if not snapshots.is_dir():
        return []
    return sorted(snapshots.glob("batch-*.json"))


def load_latest_valid(snapshots_dir) -> dict | None:
    """Newest committed manifest that validates; tmp files are ignored."""
    for path in reversed(list_committed_batches(snapshots_dir)):
        try:
            manifest = json.loads(path.read_text(encoding="utf-8"))
            validate_manifest(manifest)
            return manifest
        except RecoveryInvalid:
            raise
        except (json.JSONDecodeError, OSError) as exc:
            raise RecoveryInvalid("unreadable manifest", path=str(path), error=str(exc))
    return None


def journal_path(snapshots_dir, batch_id: str) -> Path:
    return Path(snapshots_dir) / f"batch-{batch_id}.journal.jsonl"


_journal_lock = threading.Lock()


def append_journal(snapshots_dir, batch_id: str, entry: dict, param_version: str) -> None:
    with _journal_lock:
        path = journal_path(snapshots_dir, batch_id)
        path.parent.mkdir(parents=True, exist_ok=True)
        prev = genesis_chain(batch_id)
        if path.exists():
            with open(path, "r", encoding="utf-8") as fh:
                for line in fh:
                    if line.strip():
                        prev = json.loads(line)["chain"]
        record = {
            "batch_id": batch_id,
            "param_version": param_version,
            "entry": entry,
            "chain": chain_step(prev, entry["content_hash"]),
        }
        with open(path, "a", encoding="utf-8") as fh:
            fh.write(json.dumps(record, sort_keys=True) + "\n")
            fh.flush()
            os.fsync(fh.fileno())


def load_journal(snapshots_dir, batch_id: str) -> list[dict]:
    """Validate journal chain; broken chain or fields -> E_RECOVERY_INVALID."""
    path = journal_path(snapshots_dir, batch_id)
    if not path.exists():
        return []
    records = []
    prev = genesis_chain(batch_id)
    with open(path, "r", encoding="utf-8") as fh:
        for lineno, line in enumerate(fh, start=1):
            if not line.strip():
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError:
                raise RecoveryInvalid("journal line corrupt", line=lineno, batch_id=batch_id)
            for field in ("batch_id", "param_version", "entry", "chain"):
                if field not in record:
                    raise RecoveryInvalid("journal record missing field", field=field, line=lineno)
            expected = chain_step(prev, record["entry"]["content_hash"])
            if record["chain"] != expected:
                raise RecoveryInvalid("journal chain broken", line=lineno, batch_id=batch_id)
            prev = record["chain"]
            records.append(record)
    return records


def discard_journal(snapshots_dir, batch_id: str) -> None:
    path = journal_path(snapshots_dir, batch_id)
    if path.exists():
        path.unlink()
