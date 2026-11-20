"""Immutable public reports and deterministic hash-chain helpers."""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, replace
from typing import Any, Iterable, Mapping

from .errors import ErrorCode, error


SCHEMA_VERSION = 1
LOUDNESS_FLOOR = -70.0
DBTP_FLOOR = -120.0


def canonical_json(value: Any) -> str:
    return json.dumps(
        value,
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=True,
        allow_nan=False,
    )


def sha256_value(value: Any) -> str:
    return hashlib.sha256(canonical_json(value).encode("utf-8")).hexdigest()


def _round(value: float, digits: int = 12) -> float:
    # Hashing and JSON output must not depend on the platform's last bit of a
    # binary float.  The tolerance is much smaller than a hundredth of an LU.
    return float(round(float(value), digits))


@dataclass(frozen=True)
class ResliceDiff:
    old_block_samples: int
    new_block_samples: int
    old_boundaries: tuple[int, ...]
    new_boundaries: tuple[int, ...]
    added_boundaries: tuple[int, ...]
    removed_boundaries: tuple[int, ...]
    changed_blocks: tuple[int, ...]
    max_boundary_shift_samples: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "old_block_samples": self.old_block_samples,
            "new_block_samples": self.new_block_samples,
            "old_boundaries": list(self.old_boundaries),
            "new_boundaries": list(self.new_boundaries),
            "added_boundaries": list(self.added_boundaries),
            "removed_boundaries": list(self.removed_boundaries),
            "changed_blocks": list(self.changed_blocks),
            "old_block_count": max(0, len(self.old_boundaries) - 1),
            "new_block_count": max(0, len(self.new_boundaries) - 1),
            "max_boundary_shift_samples": self.max_boundary_shift_samples,
        }


@dataclass(frozen=True)
class BlockResult:
    asset_id: str
    block_index: int
    window_start: int
    window_end: int
    sync_marker: str
    content_hash: str
    short_term_lufs: float
    integrated_lufs: float
    true_peak_dbtp: float
    sample_peak_dbtp: float
    target_lufs: float
    true_peak_limit_dbtp: float
    gain_to_target_db: float
    true_peak_ok: bool
    gate_status: str
    absolute_gate_lufs: float
    relative_gate_lu: float
    degrade_stage: str
    memory_bytes: int
    parameter_version: int
    partial: bool
    block_energy: float
    short_term_energy: float
    block_hash: str = ""
    chain_hash: str = ""

    def hash_payload(self) -> dict[str, Any]:
        return {
            "asset_id": self.asset_id,
            "block_index": self.block_index,
            "window_start": self.window_start,
            "window_end": self.window_end,
            "sync_marker": self.sync_marker,
            "content_hash": self.content_hash,
            "short_term_lufs": _round(self.short_term_lufs),
            "integrated_lufs": _round(self.integrated_lufs),
            "true_peak_dbtp": _round(self.true_peak_dbtp),
            "sample_peak_dbtp": _round(self.sample_peak_dbtp),
            "target_lufs": _round(self.target_lufs),
            "true_peak_limit_dbtp": _round(self.true_peak_limit_dbtp),
            "gain_to_target_db": _round(self.gain_to_target_db),
            "true_peak_ok": self.true_peak_ok,
            "gate_status": self.gate_status,
            "absolute_gate_lufs": _round(self.absolute_gate_lufs),
            "relative_gate_lu": _round(self.relative_gate_lu),
            "degrade_stage": self.degrade_stage,
            "memory_bytes": int(self.memory_bytes),
            "parameter_version": self.parameter_version,
            "partial": self.partial,
            "block_energy": _round(self.block_energy, 15),
            "short_term_energy": _round(self.short_term_energy, 15),
        }

    def to_dict(self) -> dict[str, Any]:
        result = self.hash_payload()
        result["block_hash"] = self.block_hash
        result["chain_hash"] = self.chain_hash
        return result

    @classmethod
    def from_dict(cls, raw: Mapping[str, Any]) -> "BlockResult":
        return cls(
            asset_id=str(raw["asset_id"]),
            block_index=int(raw["block_index"]),
            window_start=int(raw["window_start"]),
            window_end=int(raw["window_end"]),
            sync_marker=str(raw["sync_marker"]),
            content_hash=str(raw["content_hash"]),
            short_term_lufs=float(raw["short_term_lufs"]),
            integrated_lufs=float(raw["integrated_lufs"]),
            true_peak_dbtp=float(raw["true_peak_dbtp"]),
            sample_peak_dbtp=float(raw["sample_peak_dbtp"]),
            target_lufs=float(raw["target_lufs"]),
            true_peak_limit_dbtp=float(raw["true_peak_limit_dbtp"]),
            gain_to_target_db=float(raw["gain_to_target_db"]),
            true_peak_ok=bool(raw["true_peak_ok"]),
            gate_status=str(raw["gate_status"]),
            absolute_gate_lufs=float(raw["absolute_gate_lufs"]),
            relative_gate_lu=float(raw["relative_gate_lu"]),
            degrade_stage=str(raw["degrade_stage"]),
            memory_bytes=int(raw["memory_bytes"]),
            parameter_version=int(raw["parameter_version"]),
            partial=bool(raw["partial"]),
            block_energy=float(raw["block_energy"]),
            short_term_energy=float(raw["short_term_energy"]),
            block_hash=str(raw["block_hash"]),
            chain_hash=str(raw["chain_hash"]),
        )


@dataclass(frozen=True)
class AssetReport:
    asset_id: str
    sample_rate: int
    channels: int
    bits_per_sample: int
    layout: str
    channel_mask: int
    block_samples: int
    total_samples: int
    parameter_version: int
    config_hash: str
    complete: bool
    blocks: tuple[BlockResult, ...]
    head_hash: str
    report_hash: str

    @property
    def block_count(self) -> int:
        return len(self.blocks)

    @property
    def final_integrated_lufs(self) -> float:
        return self.blocks[-1].integrated_lufs if self.blocks else LOUDNESS_FLOOR

    def metadata_dict(self) -> dict[str, Any]:
        return {
            "schema_version": SCHEMA_VERSION,
            "asset_id": self.asset_id,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "bits_per_sample": self.bits_per_sample,
            "layout": self.layout,
            "channel_mask": self.channel_mask,
            "block_samples": self.block_samples,
            "total_samples": self.total_samples,
            "parameter_version": self.parameter_version,
            "config_hash": self.config_hash,
            "complete": self.complete,
            "block_count": len(self.blocks),
            "head_hash": self.head_hash,
        }

    def report_payload(self) -> dict[str, Any]:
        metadata = self.metadata_dict()
        metadata.pop("head_hash", None)
        metadata["blocks"] = [block.to_dict() for block in self.blocks]
        metadata["head_hash"] = self.head_hash
        return metadata

    def to_dict(self) -> dict[str, Any]:
        result = self.report_payload()
        result["report_hash"] = self.report_hash
        return result

    @classmethod
    def from_dict(cls, raw: Mapping[str, Any]) -> "AssetReport":
        return cls(
            asset_id=str(raw["asset_id"]),
            sample_rate=int(raw["sample_rate"]),
            channels=int(raw["channels"]),
            bits_per_sample=int(raw["bits_per_sample"]),
            layout=str(raw["layout"]),
            channel_mask=int(raw["channel_mask"]),
            block_samples=int(raw["block_samples"]),
            total_samples=int(raw["total_samples"]),
            parameter_version=int(raw["parameter_version"]),
            config_hash=str(raw["config_hash"]),
            complete=bool(raw["complete"]),
            blocks=tuple(BlockResult.from_dict(item) for item in raw["blocks"]),
            head_hash=str(raw["head_hash"]),
            report_hash=str(raw.get("report_hash", "")),
        )


def build_report(
    *,
    asset_id: str,
    sample_rate: int,
    channels: int,
    bits_per_sample: int,
    layout: str,
    channel_mask: int,
    block_samples: int,
    total_samples: int,
    parameter_version: int,
    config_hash: str,
    complete: bool,
    blocks: Iterable[BlockResult],
) -> AssetReport:
    """Attach deterministic block hashes and a chain to a report."""
    chained: list[BlockResult] = []
    previous = "0" * 64
    for block in blocks:
        payload = block.hash_payload()
        block_hash = sha256_value(payload)
        chain_hash = hashlib.sha256(
            bytes.fromhex(previous + block_hash)
        ).hexdigest()
        chained.append(replace(block, block_hash=block_hash, chain_hash=chain_hash))
        previous = chain_hash
    report_without_hash = {
        "schema_version": SCHEMA_VERSION,
        "asset_id": asset_id,
        "sample_rate": sample_rate,
        "channels": channels,
        "bits_per_sample": bits_per_sample,
        "layout": layout,
        "channel_mask": channel_mask,
        "block_samples": block_samples,
        "total_samples": total_samples,
        "parameter_version": parameter_version,
        "config_hash": config_hash,
        "complete": complete,
        "block_count": len(chained),
        "head_hash": previous,
        "blocks": [block.to_dict() for block in chained],
    }
    report_hash = sha256_value(report_without_hash)
    return AssetReport(
        asset_id=asset_id,
        sample_rate=sample_rate,
        channels=channels,
        bits_per_sample=bits_per_sample,
        layout=layout,
        channel_mask=channel_mask,
        block_samples=block_samples,
        total_samples=total_samples,
        parameter_version=parameter_version,
        config_hash=config_hash,
        complete=complete,
        blocks=tuple(chained),
        head_hash=previous,
        report_hash=report_hash,
    )


def verify_report_chain(report: AssetReport) -> None:
    previous = "0" * 64
    for block in report.blocks:
        expected_block_hash = sha256_value(block.hash_payload())
        if block.block_hash != expected_block_hash:
            raise error(
                ErrorCode.E_REPORT_MISMATCH,
                "block hash mismatch",
                {"sync_marker": block.sync_marker, "block_index": block.block_index},
            )
        expected_chain = hashlib.sha256(
            bytes.fromhex(previous + expected_block_hash)
        ).hexdigest()
        if block.chain_hash != expected_chain:
            raise error(
                ErrorCode.E_REPORT_MISMATCH,
                "hash chain mismatch",
                {"sync_marker": block.sync_marker, "block_index": block.block_index},
            )
        previous = expected_chain
    if report.head_hash != previous:
        raise error(
            ErrorCode.E_REPORT_MISMATCH,
            "report head hash mismatch",
            {"expected": previous, "actual": report.head_hash},
        )
    rebuilt = build_report(
        asset_id=report.asset_id,
        sample_rate=report.sample_rate,
        channels=report.channels,
        bits_per_sample=report.bits_per_sample,
        layout=report.layout,
        channel_mask=report.channel_mask,
        block_samples=report.block_samples,
        total_samples=report.total_samples,
        parameter_version=report.parameter_version,
        config_hash=report.config_hash,
        complete=report.complete,
        blocks=report.blocks,
    )
    if rebuilt.report_hash != report.report_hash:
        raise error(
            ErrorCode.E_REPORT_MISMATCH,
            "report hash mismatch",
            {"expected": rebuilt.report_hash, "actual": report.report_hash},
        )
