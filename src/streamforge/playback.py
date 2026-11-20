from __future__ import annotations

import json
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

from .errors import ErrorCode, fail
from .models import BudgetConfig, SliceArtifact, TierSpec


@dataclass(frozen=True)
class BandwidthSample:
    time_ms: int
    kbps: int


@dataclass
class PlaybackConfig:
    initial_tier: str
    initial_buffer_bytes: int
    bandwidth_safety_ratio: float = 0.8

    @classmethod
    def from_dict(cls, value: dict[str, Any]) -> "PlaybackConfig":
        try:
            config = cls(
                initial_tier=str(value["initial_tier"]),
                initial_buffer_bytes=int(value["initial_buffer_bytes"]),
                bandwidth_safety_ratio=float(
                    value.get("bandwidth_safety_ratio", 0.8)
                ),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "invalid playback configuration",
                error=str(exc),
            ) from exc
        if not 0.1 <= config.bandwidth_safety_ratio <= 1.0:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "bandwidth_safety_ratio out of range",
            )
        return config


@dataclass
class PlaybackResult:
    events: list[dict[str, Any]]
    errors: list[dict[str, Any]]
    final_tier: str
    min_buffer_bytes: int
    max_buffer_bytes: int
    switch_count: int


def load_bandwidth(path: Path) -> list[BandwidthSample]:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise fail(
            ErrorCode.INVALID_CONFIG,
            "cannot read bandwidth series",
            path=str(path),
            error=str(exc),
        ) from exc
    raw_samples = document.get("samples") if isinstance(document, dict) else None
    if not isinstance(raw_samples, list) or not raw_samples:
        raise fail(
            ErrorCode.INVALID_CONFIG,
            "bandwidth document requires a non-empty samples array",
        )
    samples: list[BandwidthSample] = []
    for index, raw in enumerate(raw_samples):
        if not isinstance(raw, dict):
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "bandwidth sample must be an object",
                index=index,
            )
        try:
            sample = BandwidthSample(
                time_ms=int(raw["time_ms"]), kbps=int(raw["kbps"])
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "invalid bandwidth sample",
                index=index,
                error=str(exc),
            ) from exc
        if sample.time_ms < 0 or sample.kbps <= 0:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "bandwidth values must be positive",
                index=index,
            )
        if samples and sample.time_ms <= samples[-1].time_ms:
            raise fail(
                ErrorCode.INVALID_CONFIG,
                "bandwidth timestamps must strictly increase",
                index=index,
            )
        samples.append(sample)
    if samples[0].time_ms != 0:
        raise fail(
            ErrorCode.INVALID_CONFIG,
            "bandwidth series must start at zero",
        )
    return samples


def _bandwidth_at(samples: list[BandwidthSample], time_ms: int) -> int:
    selected = samples[0].kbps
    for sample in samples:
        if sample.time_ms > time_ms:
            break
        selected = sample.kbps
    return selected


def _validate_switch(
    *,
    current: SliceArtifact,
    candidate: SliceArtifact,
    decode_limit: int,
    buffer_bytes: int,
) -> BusinessError | None:
    if (
        candidate.index != current.index
        or candidate.start_ns != current.start_ns
        or candidate.end_ns != current.end_ns
        or candidate.audio_start_ns != current.start_ns
        or candidate.video_start_ns != current.start_ns
        or candidate.audio_end_ns != current.end_ns
        or candidate.video_end_ns != current.end_ns
        or not candidate.frames
    ):
        return fail(
            ErrorCode.SWITCH_MISALIGN,
            "candidate slice timestamps do not align with the boundary",
            boundary_index=current.index,
            current_audio_start_ns=current.audio_start_ns,
            current_video_start_ns=current.video_start_ns,
            candidate_audio_start_ns=candidate.audio_start_ns,
            candidate_video_start_ns=candidate.video_start_ns,
            boundary_start_ns=current.start_ns,
        )
    projected = buffer_bytes + candidate.memory.estimated_bytes
    if projected > decode_limit:
        return fail(
            ErrorCode.SWITCH_MISALIGN,
            "candidate would overflow the decoder buffer",
            boundary_index=current.index,
            buffer_bytes=buffer_bytes,
            candidate_bytes=candidate.memory.estimated_bytes,
            decode_buffer_bytes=decode_limit,
            projected_bytes=projected,
        )
    return None


def simulate_playback(
    *,
    tiers: Iterable[TierSpec],
    slices: dict[str, list[SliceArtifact]],
    bandwidth: list[BandwidthSample],
    playback: PlaybackConfig,
    budget: BudgetConfig,
) -> PlaybackResult:
    tier_list = sorted(tiers, key=lambda item: item.target_bitrate_kbps)
    tier_map = {tier.tier_id: tier for tier in tier_list}
    if playback.initial_tier not in tier_map:
        raise fail(
            ErrorCode.INVALID_CONFIG,
            "initial playback tier is not configured",
            tier=playback.initial_tier,
        )
    for tier in tier_list:
        artifacts = slices.get(tier.tier_id, [])
        for expected_index, artifact in enumerate(artifacts):
            if artifact.index != expected_index:
                raise fail(
                    ErrorCode.SWITCH_MISALIGN,
                    "playback slice sequence is not contiguous",
                    tier=tier.tier_id,
                    expected_index=expected_index,
                    actual_index=artifact.index,
                )
    current_tier = playback.initial_tier
    buffer_bytes = min(
        playback.initial_buffer_bytes, budget.decode_buffer_bytes
    )
    events: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    switch_count = 0
    min_buffer = buffer_bytes
    max_buffer = buffer_bytes
    event_seq = 0
    slice_count = max((len(items) for items in slices.values()), default=0)

    for index in range(slice_count):
        current_artifacts = slices.get(current_tier, [])
        if index >= len(current_artifacts):
            error = fail(
                ErrorCode.SWITCH_MISALIGN,
                "current tier has no slice at playback boundary",
                boundary_index=index,
                tier=current_tier,
            ).to_dict()
            errors.append(error)
            events.append(
                {
                    "event": "playback_stopped",
                    "event_seq": event_seq,
                    "error": error,
                    "index": index,
                    "tier": current_tier,
                }
            )
            break
        current = current_artifacts[index]
        time_ms = current.start_ns // 1_000_000
        bandwidth_kbps = _bandwidth_at(bandwidth, time_ms)
        affordable = [
            tier
            for tier in tier_list
            if tier.target_bitrate_kbps
            <= bandwidth_kbps * playback.bandwidth_safety_ratio
        ]
        if buffer_bytes < budget.buffer_low_bytes:
            lowered = [
                tier
                for tier in tier_list
                if tier.target_bitrate_kbps < tier_map[current_tier].target_bitrate_kbps
            ]
            desired = lowered[-1] if lowered else tier_list[0]
            reason = "buffer_low_downgrade"
        elif affordable:
            desired = affordable[-1]
            if desired.target_bitrate_kbps > tier_map[current_tier].target_bitrate_kbps:
                reason = "bandwidth_upgrade"
            elif desired.target_bitrate_kbps < tier_map[current_tier].target_bitrate_kbps:
                reason = "bandwidth_downgrade"
            else:
                reason = "bandwidth_hold"
        else:
            desired = tier_list[0]
            reason = "bandwidth_below_all_tiers"

        if desired.tier_id != current_tier:
            candidate_artifacts = slices.get(desired.tier_id, [])
            candidate = (
                candidate_artifacts[index] if index < len(candidate_artifacts) else None
            )
            if candidate is None:
                validation_error = fail(
                    ErrorCode.SWITCH_MISALIGN,
                    "candidate tier is missing the boundary slice",
                    boundary_index=index,
                    tier=desired.tier_id,
                )
            else:
                validation_error = _validate_switch(
                    current=current,
                    candidate=candidate,
                    decode_limit=budget.decode_buffer_bytes,
                    buffer_bytes=buffer_bytes,
                )
            if validation_error is not None:
                error_document = validation_error.to_dict()
                errors.append(error_document)
                events.append(
                    {
                        "bandwidth_kbps": bandwidth_kbps,
                        "buffer_bytes": buffer_bytes,
                        "event": "switch_rejected",
                        "event_seq": event_seq,
                        "from_tier": current_tier,
                        "index": index,
                        "reason": reason,
                        "to_tier": desired.tier_id,
                        "error": error_document,
                    }
                )
                event_seq += 1
            else:
                from_tier = current_tier
                current = candidate
                current_tier = desired.tier_id
                switch_count += 1
                events.append(
                    {
                        "bandwidth_kbps": bandwidth_kbps,
                        "buffer_bytes": buffer_bytes,
                        "event": "switch",
                        "event_seq": event_seq,
                        "from_tier": from_tier,
                        "index": index,
                        "reason": reason,
                        "to_tier": current_tier,
                    }
                )
                event_seq += 1

        tier = tier_map[current_tier]
        duration_s = (current.end_ns - current.start_ns) / 1_000_000_000
        media_bytes = int(tier.target_bitrate_kbps * 1000 * duration_s / 8)
        downloaded_bytes = int(bandwidth_kbps * 1000 * duration_s / 8)
        previous_buffer = buffer_bytes
        buffer_bytes = max(0, buffer_bytes + downloaded_bytes - media_bytes)
        capped = False
        if buffer_bytes > budget.decode_buffer_bytes:
            buffer_bytes = budget.decode_buffer_bytes
            capped = True
        if previous_buffer + downloaded_bytes - media_bytes < 0:
            events.append(
                {
                    "bandwidth_kbps": bandwidth_kbps,
                    "buffer_bytes": 0,
                    "event": "playback_underflow",
                    "event_seq": event_seq,
                    "index": index,
                    "tier": current_tier,
                }
            )
            event_seq += 1
        if capped:
            events.append(
                {
                    "buffer_bytes": buffer_bytes,
                    "event": "decode_buffer_capped",
                    "event_seq": event_seq,
                    "index": index,
                    "tier": current_tier,
                }
            )
            event_seq += 1
        min_buffer = min(min_buffer, buffer_bytes)
        max_buffer = max(max_buffer, buffer_bytes)

    return PlaybackResult(
        events=events,
        errors=errors,
        final_tier=current_tier,
        min_buffer_bytes=min_buffer,
        max_buffer_bytes=max_buffer,
        switch_count=switch_count,
    )
