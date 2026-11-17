"""Streaming ITU-R BS.1770-4 integrated loudness measurement."""

from __future__ import annotations

import copy
import hashlib
import math
import struct
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np
from scipy.signal import lfilter

from .constants import (
    ABSOLUTE_GATE_LUFS,
    LOUDNESS_HOP_SECONDS,
    LOUDNESS_OFFSET,
    RELATIVE_GATE_LU,
)
from .errors import CheckpointCorrupt

LEDGER_RECORD_BYTES = 16
LEDGER_INITIAL_HASH = hashlib.sha256(b"biomemaster-loudness-ledger-v1").hexdigest()


def k_weighting_coefficients(
    sample_rate: int,
) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
    """Return BS.1770 K-weighting shelf and RLB high-pass coefficients."""

    shelf_f0 = 1681.974450955533
    shelf_gain_db = 3.999843853973347
    shelf_q = 0.7071752369554196
    k = math.tan(math.pi * shelf_f0 / sample_rate)
    vh = 10.0 ** (shelf_gain_db / 20.0)
    vb = vh**0.4996667741545416
    a0 = 1.0 + k / shelf_q + k * k
    shelf_b = np.asarray(
        [
            (vh + vb * k / shelf_q + k * k) / a0,
            2.0 * (k * k - vh) / a0,
            (vh - vb * k / shelf_q + k * k) / a0,
        ],
        dtype=np.float64,
    )
    shelf_a = np.asarray(
        [
            1.0,
            2.0 * (k * k - 1.0) / a0,
            (1.0 - k / shelf_q + k * k) / a0,
        ],
        dtype=np.float64,
    )

    highpass_f0 = 38.13547087602444
    highpass_q = 0.5003270373238773
    k = math.tan(math.pi * highpass_f0 / sample_rate)
    a0 = 1.0 + k / highpass_q + k * k
    highpass_b = np.asarray([1.0, -2.0, 1.0], dtype=np.float64)
    highpass_a = np.asarray(
        [
            1.0,
            2.0 * (k * k - 1.0) / a0,
            (1.0 - k / highpass_q + k * k) / a0,
        ],
        dtype=np.float64,
    )
    return shelf_b, shelf_a, highpass_b, highpass_a


@dataclass
class LoudnessState:
    sample_rate: int
    channels: int
    hop_samples: int
    shelf_zi: list[list[float]]
    highpass_zi: list[list[float]]
    partial_count: int
    partial_energy: list[float]
    hop_count: int
    recent_hops: list[list[float]]
    block_count: int
    absolute_energy_sum: float
    absolute_block_count: int
    ledger_head_sha256: str
    processed_samples: int
    intermediate_lufs: float | None

    def to_dict(self) -> dict[str, Any]:
        return {
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "hop_samples": self.hop_samples,
            "shelf_zi": self.shelf_zi,
            "highpass_zi": self.highpass_zi,
            "partial_count": self.partial_count,
            "partial_energy": self.partial_energy,
            "hop_count": self.hop_count,
            "recent_hops": self.recent_hops,
            "block_count": self.block_count,
            "absolute_energy_sum": self.absolute_energy_sum,
            "absolute_block_count": self.absolute_block_count,
            "ledger_head_sha256": self.ledger_head_sha256,
            "processed_samples": self.processed_samples,
            "intermediate_lufs": self.intermediate_lufs,
        }

    @classmethod
    def from_dict(cls, raw: dict[str, Any]) -> LoudnessState:
        try:
            state = cls(
                sample_rate=int(raw["sample_rate"]),
                channels=int(raw["channels"]),
                hop_samples=int(raw["hop_samples"]),
                shelf_zi=[[float(value) for value in row] for row in raw["shelf_zi"]],
                highpass_zi=[
                    [float(value) for value in row] for row in raw["highpass_zi"]
                ],
                partial_count=int(raw["partial_count"]),
                partial_energy=[float(value) for value in raw["partial_energy"]],
                hop_count=int(raw["hop_count"]),
                recent_hops=[
                    [float(value) for value in row] for row in raw["recent_hops"]
                ],
                block_count=int(raw["block_count"]),
                absolute_energy_sum=float(raw["absolute_energy_sum"]),
                absolute_block_count=int(raw["absolute_block_count"]),
                ledger_head_sha256=str(raw["ledger_head_sha256"]),
                processed_samples=int(raw["processed_samples"]),
                intermediate_lufs=(
                    None
                    if raw["intermediate_lufs"] is None
                    else float(raw["intermediate_lufs"])
                ),
            )
        except (KeyError, TypeError, ValueError) as exc:
            raise CheckpointCorrupt(
                "checkpoint loudness state is malformed", error=str(exc)
            ) from exc
        state.validate()
        return state

    def validate(self) -> None:
        if self.sample_rate <= 0 or self.channels != 2:
            raise CheckpointCorrupt("checkpoint loudness state has invalid format")
        if self.hop_samples != int(round(LOUDNESS_HOP_SECONDS * self.sample_rate)):
            raise CheckpointCorrupt(
                "checkpoint loudness hop size does not match sample rate"
            )
        for name, matrix in (
            ("shelf_zi", self.shelf_zi),
            ("highpass_zi", self.highpass_zi),
        ):
            if (
                len(matrix) != 2
                or any(len(row) != self.channels for row in matrix)
                or not all(math.isfinite(value) for row in matrix for value in row)
            ):
                raise CheckpointCorrupt(f"checkpoint {name} has invalid shape")
        if len(self.partial_energy) != self.channels:
            raise CheckpointCorrupt("checkpoint partial energy has invalid shape")
        if len(self.recent_hops) > 4:
            raise CheckpointCorrupt("checkpoint has too many recent hop records")
        if not 0 <= self.partial_count < self.hop_samples:
            raise CheckpointCorrupt("checkpoint partial hop count is invalid")
        if self.processed_samples < 0 or self.hop_count < 0 or self.block_count < 0:
            raise CheckpointCorrupt("checkpoint loudness counters are invalid")
        if self.processed_samples != (
            self.hop_count * self.hop_samples + self.partial_count
        ):
            raise CheckpointCorrupt(
                "checkpoint loudness sample counters are inconsistent"
            )


def initial_loudness_state(sample_rate: int, channels: int = 2) -> LoudnessState:
    hop_samples = int(round(LOUDNESS_HOP_SECONDS * sample_rate))
    return LoudnessState(
        sample_rate=sample_rate,
        channels=channels,
        hop_samples=hop_samples,
        shelf_zi=[[0.0] * channels for _ in range(2)],
        highpass_zi=[[0.0] * channels for _ in range(2)],
        partial_count=0,
        partial_energy=[0.0] * channels,
        hop_count=0,
        recent_hops=[],
        block_count=0,
        absolute_energy_sum=0.0,
        absolute_block_count=0,
        ledger_head_sha256=LEDGER_INITIAL_HASH,
        processed_samples=0,
        intermediate_lufs=None,
    )


def clone_loudness_state(state: LoudnessState) -> LoudnessState:
    return copy.deepcopy(state)


def _update_ledger_head(head_hex: str, energy: np.ndarray) -> str:
    digest = hashlib.sha256(bytes.fromhex(head_hex))
    digest.update(struct.pack("<dd", float(energy[0]), float(energy[1])))
    return digest.hexdigest()


def process_loudness_chunk(
    state: LoudnessState,
    audio: np.ndarray,
) -> tuple[LoudnessState, list[tuple[float, float]]]:
    """Advance a loudness state by one block and return completed hop records."""

    data = np.asarray(audio, dtype=np.float64)
    if data.ndim != 2 or data.shape[1] != state.channels:
        raise ValueError("loudness block has an unexpected channel count")
    if data.shape[0] == 0:
        return clone_loudness_state(state), []

    next_state = clone_loudness_state(state)
    shelf_b, shelf_a, highpass_b, highpass_a = k_weighting_coefficients(
        state.sample_rate
    )
    shelf_zi = np.asarray(next_state.shelf_zi, dtype=np.float64)
    highpass_zi = np.asarray(next_state.highpass_zi, dtype=np.float64)
    shelved, shelf_zi = lfilter(shelf_b, shelf_a, data, axis=0, zi=shelf_zi)
    filtered, highpass_zi = lfilter(
        highpass_b, highpass_a, shelved, axis=0, zi=highpass_zi
    )
    next_state.shelf_zi = shelf_zi.tolist()
    next_state.highpass_zi = highpass_zi.tolist()

    squared = filtered * filtered
    partial = np.asarray(next_state.partial_energy, dtype=np.float64)
    partial_count = next_state.partial_count
    offset = 0
    records: list[tuple[float, float]] = []
    while offset < squared.shape[0]:
        take = min(state.hop_samples - partial_count, squared.shape[0] - offset)
        partial += np.sum(squared[offset : offset + take], axis=0)
        partial_count += take
        offset += take
        if partial_count != state.hop_samples:
            continue

        hop_energy = partial / float(state.hop_samples)
        record = (float(hop_energy[0]), float(hop_energy[1]))
        records.append(record)
        next_state.ledger_head_sha256 = _update_ledger_head(
            next_state.ledger_head_sha256, hop_energy
        )
        next_state.hop_count += 1
        next_state.recent_hops.append([record[0], record[1]])
        if len(next_state.recent_hops) > 4:
            next_state.recent_hops.pop(0)
        if len(next_state.recent_hops) == 4:
            block_energy = float(
                np.sum(np.asarray(next_state.recent_hops, dtype=np.float64)) / 4.0
            )
            next_state.block_count += 1
            if (
                LOUDNESS_OFFSET + 10.0 * math.log10(max(block_energy, 1e-30))
                >= ABSOLUTE_GATE_LUFS
            ):
                next_state.absolute_energy_sum += block_energy
                next_state.absolute_block_count += 1
        partial = np.zeros(state.channels, dtype=np.float64)
        partial_count = 0

    next_state.partial_count = partial_count
    next_state.partial_energy = partial.tolist()
    next_state.processed_samples += data.shape[0]
    if next_state.absolute_block_count:
        mean_energy = next_state.absolute_energy_sum / next_state.absolute_block_count
        next_state.intermediate_lufs = LOUDNESS_OFFSET + 10.0 * math.log10(
            max(mean_energy, 1e-30)
        )
    else:
        next_state.intermediate_lufs = None
    next_state.validate()
    return next_state, records


def append_ledger_records(
    path: Path,
    records: list[tuple[float, float]],
) -> None:
    if not records:
        return
    payload = b"".join(struct.pack("<dd", left, right) for left, right in records)
    with path.open("ab") as handle:
        handle.write(payload)
        handle.flush()
        import os

        os.fsync(handle.fileno())


def read_ledger(path: Path) -> np.ndarray:
    try:
        payload = path.read_bytes()
    except OSError as exc:
        raise CheckpointCorrupt("loudness ledger is missing", error=str(exc)) from exc
    if len(payload) % LEDGER_RECORD_BYTES:
        raise CheckpointCorrupt("loudness ledger has a partial record")
    if not payload:
        return np.zeros((0, 2), dtype=np.float64)
    return (
        np.frombuffer(payload, dtype="<f8").reshape(-1, 2).astype(np.float64, copy=True)
    )


def verify_ledger_head(records: np.ndarray) -> str:
    head = LEDGER_INITIAL_HASH
    for left, right in records:
        head = _update_ledger_head(head, np.asarray([left, right], dtype=np.float64))
    return head


def integrated_loudness_from_ledger(
    records: np.ndarray,
    state: LoudnessState,
) -> float:
    """Apply absolute and relative gating to the persisted hop energies."""

    if records.shape[0] != state.hop_count:
        raise CheckpointCorrupt(
            "loudness ledger record count does not match checkpoint state",
            ledger_records=int(records.shape[0]),
            checkpoint_records=int(state.hop_count),
        )

    if records.shape[0] >= 4:
        block_energy = (
            records[0:-3] + records[1:-2] + records[2:-1] + records[3:]
        ).sum(axis=1) / 4.0
        block_loudness = LOUDNESS_OFFSET + 10.0 * np.log10(
            np.maximum(block_energy, 1e-30)
        )
        absolute = block_loudness >= ABSOLUTE_GATE_LUFS
        if not np.any(absolute):
            return float("-inf")
        ungated_mean = float(np.mean(block_energy[absolute]))
        relative_gate = (
            LOUDNESS_OFFSET + 10.0 * math.log10(max(ungated_mean, 1e-30))
        ) + RELATIVE_GATE_LU
        relative = block_loudness >= relative_gate
        if not np.any(relative):
            return float("-inf")
        gated_mean = float(np.mean(block_energy[relative]))
        return LOUDNESS_OFFSET + 10.0 * math.log10(max(gated_mean, 1e-30))

    # Very short projects cannot form a full 400 ms block. Use all measured
    # energy as a single block so target normalization remains well-defined.
    if state.processed_samples == 0:
        return float("-inf")
    total_energy = records.sum(axis=0) * state.hop_samples
    total_energy += np.asarray(state.partial_energy, dtype=np.float64)
    mean_energy = float(np.sum(total_energy) / state.processed_samples)
    if mean_energy <= 0.0:
        return float("-inf")
    loudness = LOUDNESS_OFFSET + 10.0 * math.log10(mean_energy)
    return loudness if loudness >= ABSOLUTE_GATE_LUFS else float("-inf")


def integrated_loudness_from_audio(
    audio: np.ndarray,
    sample_rate: int,
) -> float:
    """Convenience exact measurement for a complete in-memory stereo signal."""

    data = np.asarray(audio, dtype=np.float64)
    if data.ndim == 1:
        data = data[:, None]
    state = initial_loudness_state(sample_rate, data.shape[1])
    energies: list[tuple[float, float]] = []
    step = max(1, state.hop_samples)
    for start in range(0, data.shape[0], step):
        state, new_records = process_loudness_chunk(state, data[start : start + step])
        energies.extend(new_records)
    records = (
        np.asarray(energies, dtype=np.float64)
        if energies
        else np.zeros((0, 2), dtype=np.float64)
    )
    return integrated_loudness_from_ledger(records, state)
