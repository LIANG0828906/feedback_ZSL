from __future__ import annotations

import json
import shutil
import tempfile
from pathlib import Path

from gamepad_studio.models import (
    AxisSpec,
    BindingRule,
    BindingTable,
    DeviceSpec,
    InputEvent,
    SampleBundle,
    TruthManifest,
)

ROOT = Path(__file__).resolve().parents[1]
SAMPLES = ROOT / "samples"


def sample_path(device_id: str = "dual_stick") -> Path:
    path = SAMPLES / device_id
    if not path.exists():
        from gamepad_studio.samples import generate_samples

        generate_samples(SAMPLES)
    return path


def load_bundle(device_id: str = "dual_stick") -> SampleBundle:
    from gamepad_studio.validation import load_sample

    return load_sample(sample_path(device_id))


def copy_sample(device_id: str = "dual_stick") -> tuple[tempfile.TemporaryDirectory, Path]:
    temporary = tempfile.TemporaryDirectory(prefix="padweave-test-")
    target = Path(temporary.name) / device_id
    shutil.copytree(sample_path(device_id), target)
    return temporary, target


def mutate_json(path: Path, mutator) -> None:
    payload = json.loads(path.read_text(encoding="utf-8"))
    mutator(payload)
    path.write_text(json.dumps(payload, indent=2), encoding="utf-8")


def synthetic_bundle(
    *,
    events: list[InputEvent],
    rules: list[BindingRule],
    axes: list[AxisSpec] | None = None,
    buttons: tuple[str, ...] = ("a", "b"),
) -> SampleBundle:
    device = DeviceSpec(
        schema_version=1,
        id="synthetic",
        name="Synthetic Device",
        axes=tuple(axes or []),
        buttons=buttons,
    )
    bindings = BindingTable(schema_version=1, rules=tuple(rules))
    truth = TruthManifest(
        schema_version=1,
        device_id=device.id,
        algorithm_version="1.0.0",
        binding_version=bindings.content_version,
        axis_tolerance=0.001,
        time_tolerance_ms=0,
        axis_samples=(),
        action_samples=(),
        trigger_samples=(),
    )
    return SampleBundle(device=device, bindings=bindings, events=tuple(events), truth=truth)


def button_event(seq: int, t_ms: int, button: str, pressed: bool) -> InputEvent:
    return InputEvent(seq=seq, t_ms=t_ms, type="button", button=button, pressed=pressed)

