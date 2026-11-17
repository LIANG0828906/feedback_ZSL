"""Crash-safe shard, ledger, checkpoint, and destination file handling."""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import struct
from pathlib import Path
from typing import Any

import numpy as np

from .errors import CheckpointCorrupt

SHARD_CHAIN_INITIAL = hashlib.sha256(b"biomemaster-shard-chain-v1").hexdigest()
CHECKPOINT_FILENAME = "checkpoint.json"
LEDGER_FILENAME = "loudness_ledger.bin"


def fsync_directory(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY)
        try:
            os.fsync(descriptor)
        finally:
            os.close(descriptor)
    except OSError:
        # Directory handles are not fsync-able on every supported Windows
        # filesystem; file contents are still fsynced before the rename.
        return


def atomic_write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    payload = (
        json.dumps(
            value,
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
            allow_nan=False,
        ).encode("utf-8")
        + b"\n"
    )
    with temporary.open("wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    fsync_directory(path.parent)


def atomic_write_bytes(path: Path, payload: bytes) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    os.replace(temporary, path)
    fsync_directory(path.parent)


def shard_name(index: int) -> str:
    return f"shard_{index:08d}.f32"


def write_shard(
    work_dir: Path,
    index: int,
    audio: np.ndarray,
) -> tuple[int, str]:
    payload = np.asarray(audio, dtype="<f4").tobytes(order="C")
    path = work_dir / shard_name(index)
    with path.open("wb") as handle:
        handle.write(payload)
        handle.flush()
        os.fsync(handle.fileno())
    return len(payload), hashlib.sha256(payload).hexdigest()


def read_shard_bytes(
    work_dir: Path,
    index: int,
    expected_samples: int,
    *,
    channels: int = 2,
) -> bytes:
    path = work_dir / shard_name(index)
    expected_size = expected_samples * channels * 4
    try:
        payload = path.read_bytes()
    except OSError as exc:
        raise CheckpointCorrupt(
            "committed shard is missing or unreadable",
            shard=path.name,
            error=str(exc),
        ) from exc
    if len(payload) != expected_size:
        raise CheckpointCorrupt(
            "committed shard size does not match checkpoint",
            shard=path.name,
            expected_bytes=expected_size,
            actual_bytes=len(payload),
        )
    return payload


def shard_bytes_to_audio(payload: bytes) -> np.ndarray:
    return (
        np.frombuffer(payload, dtype="<f4").reshape(-1, 2).astype(np.float64, copy=True)
    )


def update_shard_chain(
    chain_hex: str,
    index: int,
    samples: int,
    shard_sha256: str,
) -> str:
    digest = hashlib.sha256(bytes.fromhex(chain_hex))
    digest.update(struct.pack("<QQ", index, samples))
    digest.update(bytes.fromhex(shard_sha256))
    return digest.hexdigest()


def verify_committed_shards(
    work_dir: Path,
    completed_chunks: int,
    chunk_samples: int,
    total_samples: int,
    expected_chain_sha256: str,
    expected_latest: dict[str, Any] | None,
) -> bytes | None:
    chain = SHARD_CHAIN_INITIAL
    latest_payload: bytes | None = None
    for index in range(completed_chunks):
        remaining = total_samples - index * chunk_samples
        samples = min(chunk_samples, remaining)
        payload = read_shard_bytes(work_dir, index, samples)
        digest = hashlib.sha256(payload).hexdigest()
        chain = update_shard_chain(chain, index, samples, digest)
        if index == completed_chunks - 1:
            latest_payload = payload

    if chain != expected_chain_sha256:
        raise CheckpointCorrupt(
            "shard chain hash does not match checkpoint",
            expected=expected_chain_sha256,
            actual=chain,
        )
    if completed_chunks == 0:
        if expected_latest is not None:
            raise CheckpointCorrupt("unexpected latest shard metadata for empty render")
        return None
    if expected_latest is None:
        raise CheckpointCorrupt("checkpoint is missing latest shard metadata")
    try:
        latest_index = int(expected_latest["index"])
        latest_samples = int(expected_latest["samples"])
        latest_size = int(expected_latest["size_bytes"])
        latest_digest = str(expected_latest["sha256"])
    except (KeyError, TypeError, ValueError) as exc:
        raise CheckpointCorrupt("latest shard metadata is malformed") from exc
    if latest_index != completed_chunks - 1:
        raise CheckpointCorrupt("latest shard index does not match checkpoint")
    expected_samples = min(chunk_samples, total_samples - latest_index * chunk_samples)
    if latest_samples != expected_samples:
        raise CheckpointCorrupt("latest shard sample count does not match checkpoint")
    if latest_payload is None or len(latest_payload) != latest_size:
        raise CheckpointCorrupt("latest shard byte count does not match checkpoint")
    if hashlib.sha256(latest_payload).hexdigest() != latest_digest:
        raise CheckpointCorrupt("latest shard digest does not match checkpoint")
    return latest_payload


def remove_uncommitted_shards(work_dir: Path, completed_chunks: int) -> None:
    if not work_dir.exists():
        return
    for path in work_dir.glob("shard_*.f32"):
        try:
            index = int(path.stem.split("_", 1)[1])
        except (IndexError, ValueError):
            continue
        if index >= completed_chunks:
            path.unlink(missing_ok=True)


def truncate_ledger(path: Path, expected_bytes: int) -> None:
    if expected_bytes < 0:
        raise CheckpointCorrupt("checkpoint ledger size is invalid")
    try:
        current = path.stat().st_size
    except FileNotFoundError:
        if expected_bytes == 0:
            path.touch()
            return
        raise CheckpointCorrupt("checkpoint requires a missing loudness ledger")
    if current < expected_bytes:
        raise CheckpointCorrupt(
            "loudness ledger is shorter than the checkpoint",
            expected_bytes=expected_bytes,
            actual_bytes=current,
        )
    if current > expected_bytes:
        with path.open("r+b") as handle:
            handle.truncate(expected_bytes)
            handle.flush()
            os.fsync(handle.fileno())


def cleanup_work_directory(work_dir: Path, keep_directory: bool = False) -> None:
    if not work_dir.exists():
        return
    for path in work_dir.iterdir():
        if path.is_file() or path.is_symlink():
            path.unlink(missing_ok=True)
        elif path.is_dir():
            shutil.rmtree(path)
    if not keep_directory:
        work_dir.rmdir()


def read_json_file(path: Path) -> Any:
    with path.open("rb") as handle:
        return json.loads(handle.read().decode("utf-8"))
