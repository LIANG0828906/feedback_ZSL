"""Slice index persistence, hash-chain construction and recovery validation."""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
from typing import Any

from .errors import (
    E_IO,
    E_RECOVERY_INVALID,
    E_SLICE_GAP,
    MixError,
)
from .models import canonical_json
from .wavio import content_sha256, read_wav, sha256_file

INDEX_FILENAME = "index.json"
INDEX_SCHEMA_VERSION = 1


def _digest(value: Any) -> str:
    return hashlib.sha256(canonical_json(value).encode("utf-8")).hexdigest()


def atomic_write_json(path: str | os.PathLike[str], value: Any) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    temporary = target.with_name(target.name + f".tmp-{os.getpid()}")
    text = json.dumps(
        value,
        ensure_ascii=True,
        allow_nan=False,
        sort_keys=True,
        indent=2,
    ) + "\n"
    try:
        with temporary.open("w", encoding="utf-8", newline="\n") as handle:
            handle.write(text)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, target)
    except OSError as exc:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise MixError(E_IO, f"cannot atomically write JSON: {exc}", details={"path": str(target)}) from exc


def load_index(path: str | os.PathLike[str]) -> dict[str, Any]:
    try:
        with Path(path).open("r", encoding="utf-8") as handle:
            value = json.load(handle)
    except (OSError, json.JSONDecodeError) as exc:
        raise MixError(E_RECOVERY_INVALID, f"cannot load slice index: {exc}", details={"path": str(path)}) from exc
    if not isinstance(value, dict):
        raise MixError(E_RECOVERY_INVALID, "slice index root must be an object")
    return value


def genesis_chain(job_id: str, output: dict[str, Any]) -> str:
    # Track parameters intentionally do not participate: an incremental update
    # extends the same chain rather than starting a new recording identity.
    payload = {
        "schema_version": INDEX_SCHEMA_VERSION,
        "job_id": job_id,
        "output": output,
    }
    return _digest(payload)


def slice_chain_hash(record: dict[str, Any], *, genesis: str, prev_chain: str) -> str:
    payload = {
        "genesis": genesis,
        "prev_chain": prev_chain,
        "index": record["index"],
        "start_sample": record["start_sample"],
        "end_sample": record["end_sample"],
        "sample_count": record["sample_count"],
        "content_sha256": record["content_sha256"],
        "file_sha256": record["file_sha256"],
        "file": record["file"],
        "artifact_version": record["artifact_version"],
        "parameter_versions": record["parameter_versions"],
    }
    return _digest(payload)


def rebuild_chain(index: dict[str, Any]) -> dict[str, Any]:
    genesis = str(index["genesis_chain"])
    prev = genesis
    for expected_index, record in enumerate(index["slices"]):
        record["index"] = expected_index
        record["prev_chain"] = prev
        record["chain_sha256"] = slice_chain_hash(record, genesis=genesis, prev_chain=prev)
        prev = record["chain_sha256"]
    index["committed_slices"] = len(index["slices"])
    index["chain_head"] = prev
    index["total_slices"] = int(index.get("total_slices", len(index["slices"])))
    index["complete"] = len(index["slices"]) >= index["total_slices"]
    seal_index(index)
    return index


def seal_index(index: dict[str, Any]) -> dict[str, Any]:
    """Bind the index generation and publication head into one digest.

    The chain itself authenticates slice content.  The seal additionally makes
    a mutated ``index_version``, track-version map or top-level metadata
    detectable during recovery.
    """

    payload = {
        "schema_version": int(index.get("schema_version", 0)),
        "job_id": index.get("job_id"),
        "index_version": int(index.get("index_version", 0)),
        "chain_head": index.get("chain_head"),
        "genesis_chain": index.get("genesis_chain"),
        "params_fingerprint": index.get("params_fingerprint"),
        "committed_slices": int(index.get("committed_slices", 0)),
        "total_slices": int(index.get("total_slices", 0)),
        "track_versions": index.get("track_versions"),
    }
    index["index_seal_sha256"] = _digest(payload)
    return index


def validate_index(
    index: dict[str, Any],
    *,
    output_dir: str | os.PathLike[str] | None = None,
    verify_files: bool = True,
    recovery: bool = False,
) -> None:
    """Validate structure, boundaries, chain and committed artifacts."""

    def invalid(message: str, **details: Any) -> None:
        code = E_RECOVERY_INVALID if recovery else E_RECOVERY_INVALID
        raise MixError(code, message, details=details)

    if not isinstance(index, dict):
        invalid("index is not an object")
    required = {
        "schema_version",
        "job_id",
        "index_version",
        "genesis_chain",
        "chain_head",
        "slice_samples",
        "total_samples",
        "total_slices",
        "committed_slices",
        "slices",
        "output",
    }
    missing = sorted(required - set(index))
    if missing:
        invalid("index is missing required fields", missing=missing)
    if int(index["schema_version"]) != INDEX_SCHEMA_VERSION:
        invalid("unsupported index schema", schema_version=index["schema_version"])
    slices = index.get("slices")
    if not isinstance(slices, list):
        invalid("slices must be a list")
    if int(index["committed_slices"]) != len(slices):
        invalid(
            "committed_slices does not match the slice prefix",
            committed_slices=index["committed_slices"],
            actual=len(slices),
        )
    total_slices = int(index["total_slices"])
    if len(slices) > total_slices:
        invalid("slice prefix is longer than total_slices", total_slices=total_slices, actual=len(slices))
    if bool(index.get("complete", False)) != (len(slices) == total_slices):
        invalid("complete flag does not match the committed prefix")
    slice_samples = int(index["slice_samples"])
    total_samples = int(index["total_samples"])
    if slice_samples <= 0 or total_samples < 0:
        invalid("invalid output sample dimensions")
    if total_slices != (total_samples + slice_samples - 1) // slice_samples:
        invalid("total_slices is inconsistent with total_samples")

    genesis = str(index["genesis_chain"])
    prev = genesis
    expected_start = 0
    for position, record in enumerate(slices):
        if not isinstance(record, dict):
            invalid("slice record must be an object", slice_index=position)
        if int(record.get("index", -1)) != position:
            raise MixError(
                E_SLICE_GAP,
                "slice order is not contiguous",
                slice_index=position,
                details={"record_index": record.get("index")},
            )
        start = int(record.get("start_sample", -1))
        end = int(record.get("end_sample", -1))
        count = int(record.get("sample_count", -1))
        expected_end = min(expected_start + slice_samples, total_samples)
        if start != expected_start or end != expected_end or count != end - start:
            raise MixError(
                E_SLICE_GAP,
                "adjacent slices do not join continuously",
                slice_index=position,
                details={
                    "expected_start": expected_start,
                    "actual_start": start,
                    "expected_end": expected_end,
                    "actual_end": end,
                },
            )
        if str(record.get("prev_chain")) != prev:
            invalid("slice hash chain link is broken", slice_index=position)
        expected_chain = slice_chain_hash(record, genesis=genesis, prev_chain=prev)
        if str(record.get("chain_sha256")) != expected_chain:
            invalid("slice hash chain digest is invalid", slice_index=position)
        for field in (
            "content_sha256",
            "file_sha256",
            "file",
            "artifact_version",
            "parameter_versions",
        ):
            if field not in record:
                invalid("slice record is missing a required field", slice_index=position, field=field)
        prev = expected_chain
        expected_start = end

    if str(index["chain_head"]) != prev:
        invalid("index chain head does not match the committed prefix")
    if "index_seal_sha256" in index:
        seal = seal_index(dict(index))
        if seal.get("index_seal_sha256") != index.get("index_seal_sha256"):
            invalid("index version seal is invalid")
    if expected_start != (slices[-1]["end_sample"] if slices else 0):
        raise MixError(E_SLICE_GAP, "slice prefix end is inconsistent", slice_index=len(slices))

    if verify_files:
        if output_dir is None:
            invalid("output_dir is required to verify slice files")
        base = Path(output_dir)
        for position, record in enumerate(slices):
            path = base / str(record["file"])
            if not path.is_file():
                invalid("committed slice file is missing", slice_index=position, path=str(record["file"]))
            if sha256_file(path) != str(record["file_sha256"]):
                invalid("slice file hash does not match index", slice_index=position)
            try:
                wav = read_wav(path, track_id=f"slice_{position}")
            except MixError as exc:
                invalid("committed slice is unreadable", slice_index=position, error=exc.code)
            if content_sha256(wav.pcm) != str(record["content_sha256"]):
                invalid("slice content hash does not match index", slice_index=position)


def verify_index_file(
    index_path: str | os.PathLike[str],
    *,
    output_dir: str | os.PathLike[str] | None = None,
    verify_files: bool = True,
) -> dict[str, Any]:
    index = load_index(index_path)
    base = Path(output_dir) if output_dir is not None else Path(index_path).parent
    validate_index(index, output_dir=base, verify_files=verify_files, recovery=True)
    return index
