"""Observation slicing, K-weighted block metrics and diagnostic reports."""

from __future__ import annotations

import copy
from collections.abc import Callable, Iterable
from dataclasses import dataclass
from typing import Any

import numpy as np

from .dsp import (
    energy_to_loudness,
    k_weight,
    oversampled_peak_envelope,
)
from .errors import E_CONFIG_INVALID, DiagnosticError
from .gate import GateParameters, apply_gate, reconstruct_gate_decisions
from .jsonutil import canonical_bytes, sha256_bytes
from .scheduler import (
    BlockScheduler,
    SchedulerConfig,
    scheduler_config_from_mapping,
)
from .wav import CHANNEL_WEIGHTS, WavData, block_content_hash

ENGINE_VERSION = "loudness-diag/1.0.0"


@dataclass(frozen=True)
class BlockSpec:
    block_index: int
    block_id: str
    window_start: int
    window_end: int
    sync_marker: str

    @property
    def sample_count(self) -> int:
        return self.window_end - self.window_start

    def as_dict(self) -> dict[str, Any]:
        return {
            "block_index": self.block_index,
            "block_id": self.block_id,
            "window_start": self.window_start,
            "window_end": self.window_end,
            "sync_marker": self.sync_marker,
            "sample_count": self.sample_count,
        }


@dataclass
class PreparedAsset:
    wav: WavData
    oversample: int
    filtered_channels: tuple[np.ndarray, ...]
    peak_envelopes: tuple[np.ndarray, ...]


def slice_blocks(
    wav: WavData,
    block_samples: int,
    *,
    asset_id: str | None = None,
) -> list[BlockSpec]:
    if not isinstance(block_samples, int) or block_samples < 1:
        raise DiagnosticError(E_CONFIG_INVALID, "block_samples must be a positive integer")
    identifier = asset_id if asset_id is not None else wav.info.source_id
    result: list[BlockSpec] = []
    for index, start in enumerate(range(0, wav.frame_count, block_samples)):
        end = min(start + block_samples, wav.frame_count)
        block_id = f"{index:08d}"
        sync_marker = f"sync:{identifier}:{block_id}:{start}-{end}"
        result.append(
            BlockSpec(
                block_index=index,
                block_id=block_id,
                window_start=start,
                window_end=end,
                sync_marker=sync_marker,
            )
        )
    return result


def _validate_oversample(value: int) -> int:
    if not isinstance(value, int) or value < 1 or value > 16:
        raise DiagnosticError(
            E_CONFIG_INVALID, "oversample must be an integer between 1 and 16"
        )
    return value


def prepare_asset(wav: WavData, *, oversample: int = 4) -> PreparedAsset:
    factor = _validate_oversample(oversample)
    filtered: list[np.ndarray] = []
    envelopes: list[np.ndarray] = []
    for channel in range(wav.info.channels):
        source = wav.samples[:, channel]
        filtered.append(k_weight(source))
        envelopes.append(oversampled_peak_envelope(source, factor=factor))
    return PreparedAsset(
        wav=wav,
        oversample=factor,
        filtered_channels=tuple(filtered),
        peak_envelopes=tuple(envelopes),
    )


def _evaluate_one(
    prepared: PreparedAsset,
    spec: BlockSpec,
    weights: np.ndarray,
    *,
    nominal_block_samples: int,
) -> dict[str, Any]:
    start = spec.window_start
    end = spec.window_end
    energy = 0.0
    for channel, filtered in enumerate(prepared.filtered_channels):
        segment = filtered[start:end]
        mean_square = float(np.mean(segment * segment)) if segment.size else 0.0
        energy += float(weights[channel]) * mean_square
    short_term_loudness = energy_to_loudness(energy)

    true_peak = 0.0
    factor = prepared.oversample
    for envelope in prepared.peak_envelopes:
        segment = envelope[start * factor : end * factor]
        if segment.size:
            true_peak = max(true_peak, float(np.max(np.abs(segment))))

    channel_bytes = prepared.wav.channel_bytes_for_window(start, end)
    content_hash = block_content_hash(channel_bytes)
    result: dict[str, Any] = {
        "block_index": spec.block_index,
        "block_id": spec.block_id,
        "window_start": start,
        "window_end": end,
        "sample_count": spec.sample_count,
        "complete": spec.sample_count == nominal_block_samples,
        "sync_marker": spec.sync_marker,
        "content_hash": content_hash,
        "ungated_energy": energy,
        "short_term_loudness": short_term_loudness,
        "true_peak": true_peak,
    }
    result["evaluation_hash"] = _evaluation_hash(result, prepared.oversample)
    return result


def _evaluation_hash(row: dict[str, Any], oversample: int) -> str:
    payload = {
        "engine_version": ENGINE_VERSION,
        "oversample": oversample,
        "block_id": row["block_id"],
        "window_start": row["window_start"],
        "window_end": row["window_end"],
        "content_hash": row["content_hash"],
        "ungated_energy": row["ungated_energy"],
        "short_term_loudness": row["short_term_loudness"],
        "true_peak": row["true_peak"],
    }
    return sha256_bytes(canonical_bytes(payload))


def evaluate_block_specs(
    wav: WavData,
    specs: Iterable[BlockSpec],
    *,
    oversample: int = 4,
    asset_id: str | None = None,
    scheduler_config: SchedulerConfig | dict[str, Any] | None = None,
    block_samples: int | None = None,
) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    identifier = asset_id if asset_id is not None else wav.info.source_id
    prepared = prepare_asset(wav, oversample=oversample)
    specs_list = list(specs)
    by_index = {spec.block_index: spec for spec in specs_list}
    if len(by_index) != len(specs_list):
        raise DiagnosticError(E_CONFIG_INVALID, "duplicate block index in specs")
    nominal = (
        block_samples
        if block_samples is not None
        else (specs_list[0].sample_count if specs_list else 1)
    )
    if nominal < 1:
        raise DiagnosticError(E_CONFIG_INVALID, "block_samples must be positive")
    weights = np.asarray(
        [CHANNEL_WEIGHTS[name] for name in wav.info.channel_names], dtype=np.float64
    )

    def task(block_index: int) -> dict[str, Any]:
        return _evaluate_one(
            prepared,
            by_index[block_index],
            weights,
            nominal_block_samples=nominal,
        )

    if scheduler_config is None:
        config = SchedulerConfig(
            parallelism=1,
            memory_budget_bytes=max(
                1,
                max((spec.sample_count for spec in specs_list), default=1)
                * wav.info.channels
                * 16,
            ),
            block_memory_bytes=max(
                1,
                max((spec.sample_count for spec in specs_list), default=1)
                * wav.info.channels
                * 16,
            ),
        )
    elif isinstance(scheduler_config, dict):
        config = scheduler_config_from_mapping(scheduler_config)
    else:
        config = scheduler_config

    with BlockScheduler(config) as scheduler:
        rows, trace = scheduler.run(
            identifier,
            [spec.block_index for spec in specs_list],
            task,
        )
    return rows, trace


def make_block_evaluator(
    wav: WavData,
    specs: Iterable[BlockSpec],
    *,
    oversample: int = 4,
    block_samples: int | None = None,
) -> Callable[[int], dict[str, Any]]:
    """Prepare immutable channel state and return an index-based block task."""

    specs_list = list(specs)
    by_index = {spec.block_index: spec for spec in specs_list}
    if len(by_index) != len(specs_list):
        raise DiagnosticError(E_CONFIG_INVALID, "duplicate block index in specs")
    prepared = prepare_asset(wav, oversample=oversample)
    nominal = (
        block_samples
        if block_samples is not None
        else (specs_list[0].sample_count if specs_list else 1)
    )
    weights = np.asarray(
        [CHANNEL_WEIGHTS[name] for name in wav.info.channel_names], dtype=np.float64
    )

    def evaluate(block_index: int) -> dict[str, Any]:
        return _evaluate_one(
            prepared,
            by_index[block_index],
            weights,
            nominal_block_samples=nominal,
        )

    return evaluate


def build_report(
    wav: WavData,
    specs: list[BlockSpec],
    blocks: list[dict[str, Any]],
    gate_parameters: GateParameters,
    scheduler_config: SchedulerConfig,
    *,
    block_samples: int,
    oversample: int,
    asset_id: str | None = None,
    scheduler_trace: list[dict[str, Any]] | None = None,
) -> dict[str, Any]:
    identifier = asset_id if asset_id is not None else wav.info.source_id
    gate = apply_gate(blocks, gate_parameters)
    report: dict[str, Any] = {
        "schema_version": 1,
        "engine_version": ENGINE_VERSION,
        "asset_id": identifier,
        "asset": wav.evidence(),
        "analysis": {
            "block_samples": block_samples,
            "block_count": len(specs),
            "oversample": oversample,
            "synchronization": "sample_position",
            "scheduler": scheduler_config.as_dict(),
        },
        "gate": {
            key: value for key, value in gate.items() if key != "blocks"
        },
        "blocks": gate["blocks"],
        "scheduler_trace": scheduler_trace or [],
        "decision_index": {
            "absolute_threshold_energy": gate["absolute_threshold_energy"],
            "relative_threshold_energy": gate["relative_threshold_energy"],
            "block_energy_field": "ungated_energy",
            "decision_fields": [
                "absolute_pass",
                "relative_pass",
                "final_pass",
                "removed_reason",
            ],
        },
        "decision_by_block_id": {
            row["block_id"]: {
                "ungated_energy": row["ungated_energy"],
                "absolute_pass": row["absolute_pass"],
                "relative_pass": row["relative_pass"],
                "final_pass": row["final_pass"],
                "removed_reason": row["removed_reason"],
            }
            for row in gate["blocks"]
        },
    }
    return report


def finalize_report(
    report: dict[str, Any],
    *,
    oversample: int,
) -> dict[str, Any]:
    report = copy.deepcopy(report)
    report["analysis"]["oversample"] = oversample
    unsigned = copy.deepcopy(report)
    unsigned.pop("report_hash", None)
    report["report_hash"] = sha256_bytes(canonical_bytes(unsigned))
    return report


def analyze_asset(
    wav: WavData,
    *,
    block_samples: int = 19200,
    oversample: int = 4,
    gate_parameters: GateParameters | None = None,
    asset_id: str | None = None,
    scheduler_config: SchedulerConfig | dict[str, Any] | None = None,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    params = gate_parameters or GateParameters()
    factor = _validate_oversample(oversample)
    identifier = asset_id if asset_id is not None else wav.info.source_id
    specs = slice_blocks(wav, block_samples, asset_id=identifier)
    rows, trace = evaluate_block_specs(
        wav,
        specs,
        oversample=factor,
        asset_id=identifier,
        scheduler_config=scheduler_config,
        block_samples=block_samples,
    )
    if scheduler_config is None:
        max_block_bytes = max(
            (spec.sample_count for spec in specs), default=1
        ) * wav.info.channels * 16
        config = SchedulerConfig(
            parallelism=1,
            memory_budget_bytes=max(1, max_block_bytes),
            block_memory_bytes=max(1, max_block_bytes),
        )
    elif isinstance(scheduler_config, dict):
        config = scheduler_config_from_mapping(scheduler_config)
    else:
        config = scheduler_config
    report = build_report(
        wav,
        specs,
        rows,
        params,
        config,
        block_samples=block_samples,
        oversample=factor,
        asset_id=identifier,
        scheduler_trace=trace,
    )
    report = finalize_report(report, oversample=factor)
    verify = reconstruct_gate_decisions(
        report["blocks"],
        absolute_threshold_energy=report["decision_index"][
            "absolute_threshold_energy"
        ],
        relative_threshold_energy=report["decision_index"][
            "relative_threshold_energy"
        ],
    )
    for row in report["blocks"]:
        expected = verify[row["block_id"]]
        if (
            expected["absolute_pass"] != row["absolute_pass"]
            or expected["relative_pass"] != row["relative_pass"]
            or expected["final_pass"] != row["final_pass"]
        ):
            raise AssertionError("decision index cannot reconstruct gate state")
    return report, trace


def compare_reports(
    old_report: dict[str, Any],
    new_report: dict[str, Any],
) -> dict[str, Any]:
    old_blocks = old_report["blocks"]
    new_blocks = new_report["blocks"]
    mapping: dict[str, list[dict[str, Any]]] = {}
    for old in old_blocks:
        overlaps: list[dict[str, Any]] = []
        for new in new_blocks:
            overlap = max(
                0,
                min(old["window_end"], new["window_end"])
                - max(old["window_start"], new["window_start"]),
            )
            if overlap:
                overlaps.append(
                    {
                        "new_block_id": new["block_id"],
                        "overlap_samples": overlap,
                        "start_delta": new["window_start"] - old["window_start"],
                        "end_delta": new["window_end"] - old["window_end"],
                        "content_hash_equal": old["content_hash"]
                        == new["content_hash"],
                        "evaluation_hash_equal": old["evaluation_hash"]
                        == new["evaluation_hash"],
                    }
                )
        mapping[old["block_id"]] = overlaps

    unmatched_new = [
        new["block_id"]
        for new in new_blocks
        if not any(
            new["window_start"] < old["window_end"]
            and new["window_end"] > old["window_start"]
            for old in old_blocks
        )
    ]
    return {
        "schema_version": 1,
        "old": {
            "block_samples": old_report["analysis"]["block_samples"],
            "oversample": old_report["analysis"]["oversample"],
            "block_count": len(old_blocks),
            "report_hash": old_report.get("report_hash"),
        },
        "new": {
            "block_samples": new_report["analysis"]["block_samples"],
            "oversample": new_report["analysis"]["oversample"],
            "block_count": len(new_blocks),
            "report_hash": new_report.get("report_hash"),
        },
        "block_count_delta": len(new_blocks) - len(old_blocks),
        "boundary_deviation_samples": abs(
            (new_report["analysis"]["block_samples"] or 0)
            - (old_report["analysis"]["block_samples"] or 0)
        ),
        "old_to_new_block_mapping": mapping,
        "unmatched_new_block_ids": unmatched_new,
    }


def compare_slicing(
    wav: WavData,
    *,
    old_block_samples: int,
    new_block_samples: int,
    old_oversample: int = 4,
    new_oversample: int = 4,
    asset_id: str | None = None,
    gate_parameters: GateParameters | None = None,
    scheduler_config: SchedulerConfig | dict[str, Any] | None = None,
) -> dict[str, Any]:
    old_report, _ = analyze_asset(
        wav,
        block_samples=old_block_samples,
        oversample=old_oversample,
        gate_parameters=gate_parameters,
        asset_id=asset_id,
        scheduler_config=scheduler_config,
    )
    new_report, _ = analyze_asset(
        wav,
        block_samples=new_block_samples,
        oversample=new_oversample,
        gate_parameters=gate_parameters,
        asset_id=asset_id,
        scheduler_config=scheduler_config,
    )
    return compare_reports(old_report, new_report)

