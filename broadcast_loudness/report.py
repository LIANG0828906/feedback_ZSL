"""Deterministic JSON report persistence with an atomic replace and hash chain."""

from __future__ import annotations

import hashlib
import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .analyzer import BlockResult
from .errors import ReportMismatchError
from .params import canonical_json
from .session import MeterSnapshot
from .wavio import AudioBuffer

REPORT_SCHEMA = "broadcast-loudness-report"
REPORT_VERSION = 1
CHAIN_GENESIS = hashlib.sha256(b"BROADCAST-LOUDNESS-REPORT-GENESIS-v1").hexdigest()


@dataclass(frozen=True)
class LoadedReport:
    document: dict[str, Any]
    blocks: tuple[BlockResult, ...]
    chain_head: str
    source_sha256: str


def _chain_hash(previous: str, block_document: dict[str, Any]) -> str:
    payload = (
        previous.encode("ascii")
        + b"\0"
        + canonical_json(block_document).encode("utf-8")
    )
    return hashlib.sha256(payload).hexdigest()


def build_report(audio: AudioBuffer, snapshot: MeterSnapshot) -> dict[str, Any]:
    previous = CHAIN_GENESIS
    records: list[dict[str, Any]] = []
    for block in snapshot.blocks:
        record = block.as_dict()
        record["chain_previous"] = previous
        chain = _chain_hash(previous, record)
        record["chain_hash"] = chain
        records.append(record)
        previous = chain

    true_peak_candidates = [
        block for block in snapshot.blocks if block.true_peak_dbtp is not None
    ]
    peak_block = (
        max(true_peak_candidates, key=lambda block: block.true_peak_dbtp or -999.0)
        if true_peak_candidates
        else None
    )
    return {
        "schema": REPORT_SCHEMA,
        "report_version": REPORT_VERSION,
        "source": {
            "filename": Path(audio.filename).name,
            "sha256": audio.source_sha256,
            "sample_rate": audio.sample_rate,
            "bit_depth": audio.bit_depth,
            "channels": audio.channels,
            "layout": list(audio.layout),
            "frames": audio.frame_count,
        },
        "observation": {
            "block_samples": snapshot.params.block_samples,
            "block_seconds": snapshot.params.block_samples / audio.sample_rate,
            "hop_samples": snapshot.params.hop_samples,
            "hop_seconds": snapshot.params.hop_samples / audio.sample_rate,
            "short_window_samples": snapshot.params.short_window_samples,
            "short_window_seconds": snapshot.params.short_window_samples
            / audio.sample_rate,
            "true_peak_oversample": snapshot.params.true_peak_oversample,
        },
        "parameters": snapshot.params.as_dict(),
        "measurement_rules": {
            "standard": "ITU-R BS.1770-4",
            "k_weighting": {
                "pre_filter_b": [1.53512485958697, -2.69169618940638, 1.19839281085285],
                "pre_filter_a": [1.0, -1.69065929318241, 0.73248077421585],
                "rlb_filter_b": [1.0, -2.0, 1.0],
                "rlb_filter_a": [1.0, -1.99004745483398, 0.99007225036621],
            },
            "loudness_offset": -0.691,
            "block_duration_seconds": 0.4,
            "hop_duration_seconds": 0.1,
            "short_term_window_seconds": 3.0,
            "integrated_gating": "absolute then relative over committed prefix",
            "true_peak": "4x band-limited interpolation",
            "downmix_matrix": [list(row) for row in snapshot.params.downmix],
            "loudness_channel_weights": list(snapshot.params.weights),
            "output_layout": list(snapshot.params.output_layout),
        },
        "snapshot": {
            "version": snapshot.version,
            "parameter_version": snapshot.parameter_version,
            "publish_reason": snapshot.publish_reason,
            "committed_blocks": snapshot.committed_count,
            "committed_until_sample": snapshot.committed_until_sample,
            "affected_blocks": list(snapshot.affected_blocks),
            "cache_stats": snapshot.cache_stats,
        },
        "summary": {
            "integrated_lufs": snapshot.integrated_lufs,
            "true_peak_dbtp": snapshot.true_peak_dbtp,
            "true_peak_position_sample": (
                peak_block.true_peak_position_sample if peak_block else None
            ),
            "true_peak_channel": peak_block.true_peak_channel if peak_block else None,
            "target_lufs": snapshot.params.target_lufs,
            "true_peak_limit_dbtp": snapshot.params.true_peak_limit_dbtp,
            "peak_exceeded_blocks": [
                block.index for block in snapshot.blocks if block.peak_exceeded
            ],
        },
        "parameter_history": [entry.as_dict() for entry in snapshot.parameter_history],
        "blocks": records,
        "hash_chain": {
            "algorithm": "sha256",
            "genesis": CHAIN_GENESIS,
            "head": previous,
        },
    }


def serialize_report(document: dict[str, Any]) -> bytes:
    text = json.dumps(
        document,
        sort_keys=True,
        indent=2,
        ensure_ascii=False,
        allow_nan=False,
    )
    return (text + "\n").encode("utf-8")


def save_report(
    path: str | os.PathLike[str],
    audio: AudioBuffer,
    snapshot: MeterSnapshot,
) -> str:
    """Atomically write a report and return its SHA-256."""
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    payload = serialize_report(build_report(audio, snapshot))
    temporary = destination.with_name(f".{destination.name}.{os.getpid()}.tmp")
    try:
        with temporary.open("wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
    finally:
        if temporary.exists():
            temporary.unlink()
    return hashlib.sha256(payload).hexdigest()


def save_report_document(
    path: str | os.PathLike[str],
    document: dict[str, Any],
) -> str:
    """Atomically write an already-built document (also used for offline JSON)."""
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    payload = serialize_report(document)
    temporary = destination.with_name(f".{destination.name}.{os.getpid()}.tmp")
    try:
        with temporary.open("wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary, destination)
    finally:
        if temporary.exists():
            temporary.unlink()
    return hashlib.sha256(payload).hexdigest()


def _fail(message: str, **details: Any) -> None:
    raise ReportMismatchError(message, **details)


def load_report(
    path: str | os.PathLike[str],
    *,
    audio: AudioBuffer | None = None,
) -> LoadedReport:
    """Validate a report without ever modifying it."""
    source_path = Path(path)
    try:
        raw = source_path.read_bytes()
        document = json.loads(raw.decode("utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ReportMismatchError(
            "report cannot be read", filename=source_path.name
        ) from exc
    if not isinstance(document, dict):
        _fail("report root is not an object")
    if document.get("schema") != REPORT_SCHEMA:
        _fail("report schema mismatch", schema=document.get("schema"))
    if document.get("report_version") != REPORT_VERSION:
        _fail(
            "report version mismatch",
            report_version=document.get("report_version"),
            expected=REPORT_VERSION,
        )
    source = document.get("source")
    chain = document.get("hash_chain")
    records = document.get("blocks")
    if not isinstance(source, dict) or not isinstance(chain, dict):
        _fail("report source or hash chain missing")
    if not isinstance(records, list):
        _fail("report blocks missing")
    if chain.get("algorithm") != "sha256" or chain.get("genesis") != CHAIN_GENESIS:
        _fail("unsupported report hash chain")
    if audio is not None and source.get("sha256") != audio.source_sha256:
        _fail(
            "report source hash does not match the supplied audio",
            report=source.get("sha256"),
            audio=audio.source_sha256,
        )

    previous = CHAIN_GENESIS
    blocks: list[BlockResult] = []
    for expected_index, record in enumerate(records):
        if not isinstance(record, dict):
            _fail("report block is not an object", index=expected_index)
        if record.get("chain_previous") != previous:
            _fail("report hash chain link mismatch", index=expected_index)
        stored_chain = record.get("chain_hash")
        if not isinstance(stored_chain, str):
            _fail("report block chain hash missing", index=expected_index)
        base = dict(record)
        base.pop("chain_hash", None)
        calculated = _chain_hash(previous, base)
        if calculated != stored_chain:
            _fail(
                "report block hash mismatch",
                index=expected_index,
                stored=stored_chain,
                calculated=calculated,
            )
        try:
            block = BlockResult(
                index=int(record["index"]),
                start_sample=int(record["window_start_sample"]),
                end_sample=int(record["window_end_sample"]),
                sync_marker=str(record["sync_marker"]),
                content_hash=str(record["content_hash"]),
                parameter_version=int(record["parameter_version"]),
                mean_square=float(record["mean_square"]),
                momentary_lufs=_optional_float(record["momentary_lufs"]),
                short_lufs=_optional_float(record["short_lufs"]),
                integrated_lufs=_optional_float(record["integrated_lufs"]),
                true_peak_dbtp=_optional_float(record["true_peak_dbtp"]),
                true_peak_position_sample=float(record["true_peak_position_sample"]),
                true_peak_channel=int(record["true_peak_channel"]),
                peak_exceeded=bool(record["peak_exceeded"]),
                measurement_hash=str(record["measurement_hash"]),
            )
        except (KeyError, TypeError, ValueError) as exc:
            _fail("report block fields are invalid", index=expected_index)
            raise AssertionError("unreachable") from exc
        if block.index != expected_index:
            _fail(
                "report block indexes are not contiguous",
                expected=expected_index,
                actual=block.index,
            )
        if audio is not None:
            if block.end_sample > audio.frame_count:
                _fail("report block is outside audio", index=expected_index)
            if (
                audio.content_hash(block.start_sample, block.end_sample)
                != block.content_hash
            ):
                _fail("report content hash mismatch", index=expected_index)
            if (
                audio.sync_marker(block.start_sample, block.end_sample)
                != block.sync_marker
            ):
                _fail("report sync marker mismatch", index=expected_index)
        blocks.append(block)
        previous = stored_chain

    if chain.get("head") != previous:
        _fail(
            "report chain head mismatch", stored=chain.get("head"), calculated=previous
        )
    return LoadedReport(
        document=document,
        blocks=tuple(blocks),
        chain_head=previous,
        source_sha256=str(source.get("sha256", "")),
    )


def _optional_float(value: Any) -> float | None:
    if value is None:
        return None
    return float(value)
