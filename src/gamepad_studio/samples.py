"""Deterministic local sample generator with hand-authored truth facts."""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from .errors import ALGORITHM_VERSION
from .models import (
    ActionSample,
    AxisSample,
    AxisSpec,
    BindingTable,
    DeviceSpec,
    InputEvent,
    SampleBundle,
    TriggerSample,
    TruthManifest,
)
from .validation import write_sample_json


@dataclass
class _EventBuilder:
    events: list[InputEvent]
    seq: int = 0
    last_t: int = 0

    def _next(self, t_ms: int) -> int:
        if t_ms < self.last_t:
            raise ValueError(f"sample generator attempted to move time backwards: {self.last_t}->{t_ms}")
        self.last_t = t_ms
        seq = self.seq
        self.seq += 1
        return seq

    def axis(self, t_ms: int, axis: str, value: float) -> int:
        seq = self._next(t_ms)
        self.events.append(
            InputEvent(seq=seq, t_ms=t_ms, type="axis", axis=axis, value=float(value))
        )
        return seq

    def button(
        self,
        t_ms: int,
        button: str,
        pressed: bool,
        *,
        repeat: bool = False,
    ) -> int:
        seq = self._next(t_ms)
        self.events.append(
            InputEvent(
                seq=seq,
                t_ms=t_ms,
                type="button",
                button=button,
                pressed=pressed,
                repeat=repeat,
            )
        )
        return seq


def _oracle_axis(axis: AxisSpec, raw: float) -> float:
    """Independent closed-form oracle for truth ranges (no replay import)."""

    if axis.minimum < 0.0 < axis.maximum:
        value = raw / max(abs(axis.minimum), abs(axis.maximum))
        value = min(max(value, -1.0), 1.0)
        magnitude = abs(value)
        if magnitude <= axis.deadzone:
            value = 0.0
        elif axis.deadzone > 0.0:
            value = math.copysign(
                (magnitude - axis.deadzone) / (1.0 - axis.deadzone),
                value,
            )
        if axis.inverted:
            value = -value
        if axis.curve.kind == "power":
            value = math.copysign(abs(value) ** axis.curve.exponent, value)
        return value
    value = (raw - axis.minimum) / (axis.maximum - axis.minimum)
    if axis.kind == "trigger" and axis.trigger_direction == "decreasing":
        value = 1.0 - value
    if value <= axis.deadzone:
        value = 0.0
    elif axis.deadzone > 0.0:
        value = (value - axis.deadzone) / (1.0 - axis.deadzone)
    if axis.inverted:
        value = 1.0 - value
    if axis.curve.kind == "power":
        value = value**axis.curve.exponent
    return value


def _truth(
    device: DeviceSpec,
    bindings: BindingTable,
    axis_facts: list[tuple[int, str, float]],
    action_samples: list[ActionSample],
    trigger_samples: list[TriggerSample],
    *,
    tolerance: float = 0.004,
) -> TruthManifest:
    axis_map = device.axis_map
    axis_samples = []
    for t_ms, axis_id, raw in axis_facts:
        expected = _oracle_axis(axis_map[axis_id], raw)
        axis_samples.append(
            AxisSample(
                t_ms=t_ms,
                axis=axis_id,
                minimum=expected - tolerance,
                maximum=expected + tolerance,
            )
        )
    return TruthManifest(
        schema_version=1,
        device_id=device.id,
        algorithm_version=ALGORITHM_VERSION,
        binding_version=bindings.content_version,
        axis_tolerance=tolerance,
        time_tolerance_ms=0,
        axis_samples=tuple(axis_samples),
        action_samples=tuple(action_samples),
        trigger_samples=tuple(trigger_samples),
    )


def _make_dual_stick() -> SampleBundle:
    device = DeviceSpec.from_dict(
        {
            "schema_version": 1,
            "id": "dual_stick",
            "name": "Atlas Dual Stick",
            "axes": [
                {
                    "id": "left_x",
                    "kind": "stick",
                    "minimum": -1.0,
                    "maximum": 1.0,
                    "deadzone": 0.12,
                    "inverted": False,
                    "curve": {"kind": "power", "exponent": 1.5},
                },
                {
                    "id": "left_y",
                    "kind": "stick",
                    "minimum": -1.0,
                    "maximum": 1.0,
                    "deadzone": 0.12,
                    "inverted": True,
                    "curve": {"kind": "linear"},
                },
                {
                    "id": "right_x",
                    "kind": "stick",
                    "minimum": -1.0,
                    "maximum": 1.0,
                    "deadzone": 0.08,
                    "inverted": False,
                    "curve": {"kind": "linear"},
                },
                {
                    "id": "right_y",
                    "kind": "stick",
                    "minimum": -1.0,
                    "maximum": 1.0,
                    "deadzone": 0.08,
                    "inverted": False,
                    "curve": {"kind": "linear"},
                },
                {
                    "id": "trigger_l",
                    "kind": "trigger",
                    "minimum": 0.0,
                    "maximum": 1.0,
                    "deadzone": 0.05,
                    "inverted": False,
                    "trigger_direction": "decreasing",
                    "curve": {"kind": "linear"},
                },
                {
                    "id": "trigger_r",
                    "kind": "trigger",
                    "minimum": 0.0,
                    "maximum": 1.0,
                    "deadzone": 0.05,
                    "inverted": False,
                    "trigger_direction": "increasing",
                    "curve": {"kind": "linear"},
                },
                {
                    "id": "gyro_x",
                    "kind": "gyro",
                    "minimum": -2.0,
                    "maximum": 2.0,
                    "deadzone": 0.02,
                    "inverted": False,
                    "curve": {"kind": "linear"},
                },
            ],
            "buttons": ["a", "b", "x", "y", "lb", "rb", "start", "back"],
        }
    )
    bindings = BindingTable.from_dict(
        {
            "schema_version": 1,
            "version": "dual-rules-v1",
            "rules": [
                {
                    "id": "move_x",
                    "action": "move_x",
                    "kind": "axis",
                    "source_axis": "left_x",
                },
                {
                    "id": "look_y",
                    "action": "look_y",
                    "kind": "axis",
                    "source_axis": "right_y",
                    "curve": {"kind": "power", "exponent": 1.3},
                },
                {
                    "id": "fire",
                    "action": "fire",
                    "kind": "axis",
                    "source_axis": "trigger_r",
                },
                {"id": "jump", "action": "jump", "kind": "button", "buttons": ["a"]},
                {
                    "id": "sprint",
                    "action": "sprint",
                    "kind": "combo",
                    "buttons": ["lb", "rb"],
                    "window_ms": 80,
                },
                {
                    "id": "charge",
                    "action": "charge",
                    "kind": "long_press",
                    "buttons": ["x"],
                    "hold_ms": 300,
                },
                {
                    "id": "dash",
                    "action": "dash",
                    "kind": "double_press",
                    "buttons": ["b"],
                    "window_ms": 250,
                },
                {
                    "id": "menu",
                    "action": "menu",
                    "kind": "button",
                    "buttons": ["start"],
                },
            ],
        }
    )
    build = _EventBuilder(events=[])
    build.axis(0, "left_x", 0.0)
    build.axis(0, "left_y", 0.0)
    build.axis(0, "right_x", 0.0)
    build.axis(0, "right_y", 0.0)
    build.axis(0, "trigger_l", 1.0)
    build.axis(0, "trigger_r", 0.0)
    build.axis(0, "gyro_x", 0.0)
    build.axis(20, "left_x", 0.05)
    build.axis(40, "left_x", 0.30)
    build.axis(60, "left_y", 0.50)
    build.axis(80, "right_x", -0.50)
    build.axis(100, "trigger_l", 0.0)
    build.axis(120, "trigger_r", 0.70)
    build.axis(140, "gyro_x", 1.50)
    jump_press = build.button(160, "a", True)
    jump_repeat_1 = build.button(200, "a", True, repeat=True)
    jump_repeat_2 = build.button(240, "a", True, repeat=True)
    build.button(280, "a", False)
    build.button(500, "lb", True)
    combo_trigger = build.button(540, "rb", True)
    build.button(580, "lb", False)
    build.button(600, "rb", False)
    build.button(700, "lb", True)
    build.button(790, "lb", False)
    build.button(820, "rb", True)
    build.button(860, "rb", False)
    long_press = build.button(1000, "x", True)
    build.button(1350, "x", False)
    build.button(1500, "x", True)
    build.button(1700, "x", False)
    build.button(2000, "b", True)
    build.button(2050, "b", False)
    build.button(2120, "b", True)
    double_trigger_1 = build.button(2180, "b", False)
    build.button(2700, "b", True)
    build.button(2750, "b", False)
    build.button(2950, "b", True)
    build.button(3000, "b", False)
    build.button(3200, "b", True)
    build.button(3250, "b", False)
    build.button(3350, "b", True)
    double_trigger_2 = build.button(3450, "b", False)
    build.button(3600, "lb", True)
    combo_trigger_2 = build.button(3680, "rb", True)
    build.button(3720, "lb", False)
    build.button(3760, "rb", False)
    build.axis(3900, "left_x", 0.75)

    truth = _truth(
        device,
        bindings,
        [
            (20, "left_x", 0.05),
            (40, "left_x", 0.30),
            (60, "left_y", 0.50),
            (80, "right_x", -0.50),
            (100, "trigger_l", 0.0),
            (120, "trigger_r", 0.70),
            (140, "gyro_x", 1.50),
            (3900, "left_x", 0.75),
        ],
        [
            ActionSample(160, "jump", pressed=True, trigger_count=1, repeat_count=0),
            ActionSample(
                240,
                "jump",
                pressed=True,
                trigger_count=3,
                repeat_count=2,
                reason="button_auto_repeat",
            ),
            ActionSample(280, "jump", pressed=False, trigger_count=3, repeat_count=2),
            ActionSample(
                540,
                "sprint",
                pressed=True,
                trigger_count=1,
                reason="combo_triggered",
            ),
            ActionSample(
                780,
                "sprint",
                pressed=False,
                trigger_count=1,
                reason="combo_window_expired",
            ),
            ActionSample(
                1300,
                "charge",
                pressed=True,
                trigger_count=1,
                reason="long_triggered",
            ),
            ActionSample(1350, "charge", pressed=False, trigger_count=1),
            ActionSample(
                2180,
                "dash",
                trigger_count=1,
                reason="double_triggered",
            ),
            ActionSample(
                2950,
                "dash",
                trigger_count=1,
                reason="double_window_expired",
            ),
            ActionSample(
                3450,
                "dash",
                trigger_count=2,
                reason="double_triggered",
            ),
            ActionSample(
                3680,
                "sprint",
                pressed=True,
                trigger_count=2,
                reason="combo_triggered",
            ),
        ],
        [
            TriggerSample("jump", 160, jump_press, 1),
            TriggerSample("jump", 200, jump_repeat_1, 2, repeat=True),
            TriggerSample("jump", 240, jump_repeat_2, 3, repeat=True),
            TriggerSample("sprint", 540, combo_trigger, 1),
            TriggerSample("charge", 1300, long_press, 1),
            TriggerSample("dash", 2180, double_trigger_1, 1),
            TriggerSample("dash", 3450, double_trigger_2, 2),
            TriggerSample("sprint", 3680, combo_trigger_2, 2),
        ],
    )
    return SampleBundle(
        device=device,
        bindings=bindings,
        events=tuple(build.events),
        truth=truth,
    )


def _make_single_stick() -> SampleBundle:
    device = DeviceSpec.from_dict(
        {
            "schema_version": 1,
            "id": "single_stick",
            "name": "Nano Single Stick",
            "axes": [
                {
                    "id": "stick_x",
                    "kind": "stick",
                    "minimum": -1.0,
                    "maximum": 1.0,
                    "deadzone": 0.18,
                    "inverted": False,
                    "curve": {"kind": "power", "exponent": 1.25},
                },
                {
                    "id": "stick_y",
                    "kind": "stick",
                    "minimum": -1.0,
                    "maximum": 1.0,
                    "deadzone": 0.18,
                    "inverted": False,
                    "curve": {"kind": "linear"},
                },
                {
                    "id": "trigger",
                    "kind": "trigger",
                    "minimum": 0.0,
                    "maximum": 1.0,
                    "deadzone": 0.08,
                    "inverted": False,
                    "trigger_direction": "increasing",
                    "curve": {"kind": "linear"},
                },
            ],
            "buttons": ["a", "b", "start"],
        }
    )
    bindings = BindingTable.from_dict(
        {
            "schema_version": 1,
            "version": "single-rules-v1",
            "rules": [
                {
                    "id": "move_x",
                    "action": "move_x",
                    "kind": "axis",
                    "source_axis": "stick_x",
                },
                {
                    "id": "move_y",
                    "action": "move_y",
                    "kind": "axis",
                    "source_axis": "stick_y",
                },
                {
                    "id": "fire",
                    "action": "fire",
                    "kind": "axis",
                    "source_axis": "trigger",
                },
                {"id": "jump", "action": "jump", "kind": "button", "buttons": ["a"]},
                {
                    "id": "sprint",
                    "action": "sprint",
                    "kind": "combo",
                    "buttons": ["a", "b"],
                    "window_ms": 90,
                },
                {
                    "id": "dash",
                    "action": "dash",
                    "kind": "double_press",
                    "buttons": ["b"],
                    "window_ms": 240,
                },
            ],
        }
    )
    build = _EventBuilder(events=[])
    build.axis(0, "stick_x", 0)
    build.axis(0, "stick_y", 0)
    build.axis(0, "trigger", 0)
    build.axis(30, "stick_x", 0.12)
    build.axis(60, "stick_x", 0.62)
    build.axis(90, "stick_y", -0.48)
    build.axis(120, "trigger", 0.72)
    build.button(220, "a", True)
    combo_trigger = build.button(260, "b", True)
    build.button(300, "b", False)
    build.button(340, "a", False)
    build.button(520, "b", True)
    build.button(560, "b", False)
    build.button(680, "b", True)
    double_trigger = build.button(720, "b", False)
    build.axis(900, "stick_x", -0.7)

    truth = _truth(
        device,
        bindings,
        [
            (30, "stick_x", 0.12),
            (60, "stick_x", 0.62),
            (90, "stick_y", -0.48),
            (120, "trigger", 0.72),
            (900, "stick_x", -0.7),
        ],
        [
            ActionSample(
                260,
                "sprint",
                pressed=True,
                trigger_count=1,
                reason="combo_triggered",
            ),
            ActionSample(340, "sprint", pressed=False, trigger_count=1),
            ActionSample(
                720,
                "dash",
                trigger_count=1,
                reason="double_triggered",
            ),
        ],
        [
            TriggerSample("sprint", 260, combo_trigger, 1),
            TriggerSample("dash", 720, double_trigger, 1),
        ],
    )
    return SampleBundle(
        device=device,
        bindings=bindings,
        events=tuple(build.events),
        truth=truth,
    )


def _make_hybrid() -> SampleBundle:
    device = DeviceSpec.from_dict(
        {
            "schema_version": 1,
            "id": "hybrid_trigger_gyro",
            "name": "Vector Trigger + Gyro",
            "axes": [
                {
                    "id": "stick_x",
                    "kind": "stick",
                    "minimum": -1.0,
                    "maximum": 1.0,
                    "deadzone": 0.10,
                    "inverted": False,
                    "curve": {"kind": "power", "exponent": 1.4},
                },
                {
                    "id": "stick_y",
                    "kind": "stick",
                    "minimum": -1.0,
                    "maximum": 1.0,
                    "deadzone": 0.10,
                    "inverted": True,
                    "curve": {"kind": "linear"},
                },
                {
                    "id": "trigger_l",
                    "kind": "trigger",
                    "minimum": 0.0,
                    "maximum": 1.0,
                    "deadzone": 0.04,
                    "inverted": False,
                    "trigger_direction": "decreasing",
                    "curve": {"kind": "linear"},
                },
                {
                    "id": "trigger_r",
                    "kind": "trigger",
                    "minimum": 0.0,
                    "maximum": 1.0,
                    "deadzone": 0.04,
                    "inverted": False,
                    "trigger_direction": "increasing",
                    "curve": {"kind": "linear"},
                },
                {
                    "id": "gyro_x",
                    "kind": "gyro",
                    "minimum": -4.0,
                    "maximum": 4.0,
                    "deadzone": 0.03,
                    "inverted": False,
                    "curve": {"kind": "linear"},
                },
                {
                    "id": "gyro_y",
                    "kind": "gyro",
                    "minimum": -4.0,
                    "maximum": 4.0,
                    "deadzone": 0.03,
                    "inverted": False,
                    "curve": {"kind": "linear"},
                },
            ],
            "buttons": ["a", "b", "lb", "rb", "menu"],
        }
    )
    bindings = BindingTable.from_dict(
        {
            "schema_version": 1,
            "version": "hybrid-rules-v1",
            "rules": [
                {
                    "id": "move_x",
                    "action": "move_x",
                    "kind": "axis",
                    "source_axis": "stick_x",
                },
                {
                    "id": "aim_x",
                    "action": "aim_x",
                    "kind": "axis",
                    "source_axis": "gyro_x",
                },
                {
                    "id": "fire",
                    "action": "fire",
                    "kind": "axis",
                    "source_axis": "trigger_l",
                },
                {
                    "id": "brake",
                    "action": "brake",
                    "kind": "axis",
                    "source_axis": "trigger_r",
                },
                {"id": "jump", "action": "jump", "kind": "button", "buttons": ["b"]},
                {
                    "id": "sprint",
                    "action": "sprint",
                    "kind": "combo",
                    "buttons": ["lb", "rb"],
                    "window_ms": 70,
                },
                {
                    "id": "menu_hold",
                    "action": "menu_hold",
                    "kind": "long_press",
                    "buttons": ["menu"],
                    "hold_ms": 500,
                },
                {
                    "id": "dash",
                    "action": "dash",
                    "kind": "double_press",
                    "buttons": ["a"],
                    "window_ms": 220,
                },
            ],
        }
    )
    build = _EventBuilder(events=[])
    build.axis(0, "stick_x", 0)
    build.axis(0, "stick_y", 0)
    build.axis(0, "trigger_l", 1)
    build.axis(0, "trigger_r", 0)
    build.axis(0, "gyro_x", 0)
    build.axis(0, "gyro_y", 0)
    build.axis(25, "stick_x", 0.08)
    build.axis(50, "stick_x", 0.55)
    build.axis(75, "stick_y", 0.60)
    build.axis(100, "trigger_l", 0.0)
    build.axis(125, "trigger_r", 0.80)
    build.axis(150, "gyro_x", 2.0)
    build.axis(175, "gyro_y", -3.0)
    jump_press = build.button(220, "b", True)
    jump_repeat = build.button(250, "b", True, repeat=True)
    build.button(280, "b", False)
    build.button(420, "lb", True)
    combo_trigger = build.button(470, "rb", True)
    build.button(500, "lb", False)
    build.button(530, "rb", False)
    menu_press = build.button(700, "menu", True)
    build.button(1300, "menu", False)
    build.button(1600, "a", True)
    build.button(1650, "a", False)
    build.button(1750, "a", True)
    double_trigger = build.button(1800, "a", False)
    build.axis(2100, "gyro_x", -1.5)
    build.axis(2200, "stick_x", -0.65)

    truth = _truth(
        device,
        bindings,
        [
            (25, "stick_x", 0.08),
            (50, "stick_x", 0.55),
            (75, "stick_y", 0.60),
            (100, "trigger_l", 0.0),
            (125, "trigger_r", 0.80),
            (150, "gyro_x", 2.0),
            (175, "gyro_y", -3.0),
            (2100, "gyro_x", -1.5),
            (2200, "stick_x", -0.65),
        ],
        [
            ActionSample(250, "jump", pressed=True, trigger_count=2, repeat_count=1),
            ActionSample(
                470,
                "sprint",
                pressed=True,
                trigger_count=1,
                reason="combo_triggered",
            ),
            ActionSample(
                1200,
                "menu_hold",
                pressed=True,
                trigger_count=1,
                reason="long_triggered",
            ),
            ActionSample(1300, "menu_hold", pressed=False, trigger_count=1),
            ActionSample(
                1800,
                "dash",
                trigger_count=1,
                reason="double_triggered",
            ),
        ],
        [
            TriggerSample("jump", 220, jump_press, 1),
            TriggerSample("jump", 250, jump_repeat, 2, repeat=True),
            TriggerSample("sprint", 470, combo_trigger, 1),
            TriggerSample("menu_hold", 1200, menu_press, 1),
            TriggerSample("dash", 1800, double_trigger, 1),
        ],
    )
    return SampleBundle(
        device=device,
        bindings=bindings,
        events=tuple(build.events),
        truth=truth,
    )


def generate_samples(output_directory: str | Path) -> dict[str, Any]:
    output = Path(output_directory)
    output.mkdir(parents=True, exist_ok=True)
    bundles = [
        _make_dual_stick(),
        _make_single_stick(),
        _make_hybrid(),
    ]
    entries = []
    for bundle in bundles:
        sample_path = output / bundle.device.id
        sample_path.mkdir(parents=True, exist_ok=True)
        write_sample_json(sample_path / "device.json", bundle.device.to_dict())
        write_sample_json(sample_path / "bindings.json", bundle.bindings.to_dict())
        write_sample_json(
            sample_path / "events.json",
            {
                "schema_version": 1,
                "device_id": bundle.device.id,
                "events": [event.to_dict() for event in bundle.events],
            },
        )
        write_sample_json(sample_path / "truth.json", bundle.truth.to_dict())
        entries.append(
            {
                "device_id": bundle.device.id,
                "name": bundle.device.name,
                "path": bundle.device.id,
                "events": len(bundle.events),
                "axes": len(bundle.device.axes),
                "buttons": len(bundle.device.buttons),
                "rules": len(bundle.bindings.rules),
                "input_hash": bundle.input_hash,
                "binding_version": bundle.binding_version,
            }
        )
    index = {
        "schema_version": 1,
        "algorithm_version": ALGORITHM_VERSION,
        "samples": entries,
    }
    write_sample_json(output / "index.json", index)
    return index


def load_generated_sample(output_directory: str | Path, device_id: str) -> SampleBundle:
    from .validation import load_sample

    return load_sample(Path(output_directory) / device_id)


__all__ = ["generate_samples", "load_generated_sample"]
