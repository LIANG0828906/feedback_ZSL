"""Out-of-band anomaly injection at the input boundary."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Dict, List, Mapping, Optional, Sequence, Tuple

import numpy as np

from .codes import (
    AcousticError,
    E_CHANNEL_LAYOUT_MISMATCH,
    E_CONFIG_INVALID,
    E_DECAY_NEGATIVE,
    E_IR_INVALID,
    E_POSITION_OUT_OF_BOUNDS,
)
from .scene import clone_scene, listener_by_id


INJECTION_CODES: Tuple[str, ...] = (
    "ir_invalid",
    "decay_negative",
    "position_out_of_bounds",
    "channel_layout_missing",
)


@dataclass(frozen=True)
class Injection:
    kind: str
    target: Optional[str] = None

    @staticmethod
    def parse(value: str) -> "Injection":
        text = value.strip()
        if ":" in text:
            kind, target = text.split(":", 1)
            return Injection(kind=kind, target=target or None)
        return Injection(kind=text, target=None)

    def label(self) -> str:
        return self.kind if not self.target else f"{self.kind}:{self.target}"


@dataclass
class PreparedInjection:
    scene: Dict[str, Any]
    ir_invalid: bool
    records: List[Dict[str, Any]]
    target_listener: str


def prepare_scene_injection(
    scene: Mapping[str, Any],
    listener_id: str,
    injections: Sequence[Injection],
) -> PreparedInjection:
    working = clone_scene(scene)
    listener = listener_by_id(working, listener_id)
    ir_invalid = False
    records: List[Dict[str, Any]] = []
    for injection in injections:
        if injection.kind == "ir_invalid":
            ir_invalid = True
            records.append(
                {
                    "kind": injection.kind,
                    "injected_at": f"input:listeners[{listener_id}].ir.samples",
                }
            )
        elif injection.kind == "decay_negative":
            working.setdefault("decay_config", {})["decay_time_override"] = -0.25
            records.append(
                {
                    "kind": injection.kind,
                    "injected_at": "input:decay_config.decay_time_override",
                }
            )
        elif injection.kind == "position_out_of_bounds":
            listener["position"] = [1000000.0, 1000000.0, 1000000.0]
            records.append(
                {
                    "kind": injection.kind,
                    "injected_at": f"input:listeners[{listener_id}].position",
                }
            )
        elif injection.kind == "channel_layout_missing":
            working["channel_layout"] = []
            records.append(
                {
                    "kind": injection.kind,
                    "injected_at": "input:channel_layout",
                }
            )
        else:
            raise AcousticError(
                E_CONFIG_INVALID,
                f"Unknown injection kind: {injection.kind}",
                injection_point="request:inject",
            )
    return PreparedInjection(
        scene=working,
        ir_invalid=ir_invalid,
        records=records,
        target_listener=listener_id,
    )


def inject_ir_value(samples: np.ndarray, injection_records: Sequence[Mapping[str, Any]]) -> Dict[str, Any]:
    if not injection_records:
        return {}
    working = np.array(samples, copy=True)
    if working.size == 0:
        working = np.asarray([[np.nan]], dtype=np.float64)
    location = (0, min(1, working.shape[1] - 1))
    working[location] = np.nan
    metadata = {
        "kind": "ir_invalid",
        "injected_at": f"input:ir.samples[{location[0]},{location[1]}]",
    }
    return {"samples": working, "metadata": metadata}


def validate_ir_runtime(
    samples: np.ndarray,
    *,
    sample_rate: int,
    expected_rate: int,
    channel_count: int,
    layout: Sequence[str],
    injection_records: Sequence[Mapping[str, Any]],
) -> None:
    injected = next(
        (record for record in injection_records if record.get("kind") == "ir_invalid"),
        None,
    )
    if samples.ndim != 2 or samples.shape[1] != channel_count:
        raise AcousticError(
            E_CHANNEL_LAYOUT_MISMATCH,
            "WAV channel count does not match the declared 5.1 layout.",
            injection_point="wav:data:channels",
            details={"detected_at": "wav:channel_count", "wav_channels": int(samples.shape[1]) if samples.ndim == 2 else None, "layout": list(layout)},
        )
    if sample_rate != expected_rate:
        raise AcousticError(
            E_IR_INVALID,
            "WAV sample rate does not match the scene sample rate.",
            injection_point="wav:fmt:sample_rate",
            details={"detected_at": "wav:sample_rate", "sample_rate": sample_rate, "expected": expected_rate},
        )
    if samples.size == 0:
        raise AcousticError(
            E_IR_INVALID,
            "IR contains no samples.",
            injection_point="wav:data",
        )
    finite = np.isfinite(samples)
    if not bool(finite.all()):
        bad = np.argwhere(~finite)[0]
        location = f"wav:data[frame={int(bad[0])},channel={int(bad[1])}]"
        details: Dict[str, Any] = {
            "detected_at": "validation:ir_finite",
            "wav_sample_index": int(bad[0]),
            "wav_channel_index": int(bad[1]),
        }
        if injected is not None:
            details["injected_at"] = injected.get("injected_at")
            details["injection_kind"] = injected.get("kind")
            location = str(injected.get("injected_at"))
        raise AcousticError(
            E_IR_INVALID,
            "IR contains non-finite samples.",
            injection_point=location,
            details=details,
        )
    for channel_index, channel in enumerate(samples.T):
        if not np.any(channel):
            raise AcousticError(
                E_IR_INVALID,
                "IR channel is silent and has no detectable direct arrival.",
                injection_point=f"wav:data:channel={channel_index}",
                details={"detected_at": "validation:direct_arrival", "channel": layout[channel_index]},
            )

