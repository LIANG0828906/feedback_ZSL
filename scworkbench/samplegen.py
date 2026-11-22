from __future__ import annotations

import json
import math
import random
from pathlib import Path
from typing import Any

from .anomalies import write_presets
from .negotiation import negotiate
from .profiles import CODEC_PROFILES, PROTOCOL_VERSION, SCHEMA_VERSION
from .rendering import image_digest, render_scene

GENERATOR_VERSION = "deterministic-fixture-v1"
FRAME_COUNT = 40
FPS = 12
FRAME_SEED = 424242
DECLARED_WIDTH = 640
DECLARED_HEIGHT = 360


def _color(r: int, g: int, b: int) -> str:
    return f"#{max(0, min(255, r)):02x}{max(0, min(255, g)):02x}{max(0, min(255, b)):02x}"


def _frame_scene(index: int, rng: random.Random) -> dict[str, Any]:
    tick = index - 1
    phase = tick / FPS
    base_windows = [
        {
            "id": 1,
            "title": "Render Pipeline / Game View",
            "x": 36 + int(18 * math.sin(phase * 1.4)),
            "y": 42 + int(7 * math.cos(phase * 1.1)),
            "w": 316,
            "h": 194,
            "color": "#35c9e8",
        },
        {
            "id": 2,
            "title": "Combat Telemetry",
            "x": 382 + int(10 * math.cos(phase * 0.8)),
            "y": 30 + int(11 * math.sin(phase * 0.9)),
            "w": 220,
            "h": 126,
            "color": "#ffbd4a",
        },
        {
            "id": 3,
            "title": "Director Notes",
            "x": 366 + int(14 * math.sin(phase * 0.65 + 0.7)),
            "y": 184 + int(8 * math.cos(phase * 1.3)),
            "w": 234,
            "h": 124,
            "color": "#c36bff",
        },
    ]
    particles = []
    for particle_index in range(14):
        angle = phase * (0.35 + particle_index * 0.013) + particle_index
        radius = 90 + particle_index * 17
        particles.append(
            {
                "x": int(DECLARED_WIDTH / 2 + math.cos(angle) * radius),
                "y": int(DECLARED_HEIGHT / 2 + math.sin(angle * 1.2) * radius * 0.5),
                "r": 2 + particle_index % 4,
                "color": _color(
                    60 + particle_index * 11,
                    150 + particle_index * 5,
                    210 + particle_index * 2,
                ),
            }
        )
    cursor_x = 80 + int(470 * ((tick * 13) % 100) / 100)
    cursor_y = 80 + int(210 * ((tick * 29) % 100) / 100)
    return {
        "tick": tick,
        "background": [
            _color(7 + tick % 5, 16 + tick % 7, 31 + tick % 9),
            _color(19 + tick % 6, 31 + tick % 8, 57 + tick % 11),
        ],
        "windows": base_windows,
        "particles": particles,
        "cursor": [cursor_x, cursor_y],
        "scene_tag": f"{rng.getrandbits(32):08x}",
    }


def _input_specs() -> list[dict[str, Any]]:
    return [
        {"timestamp_ms": 47, "type": "click", "x": 160, "y": 132},
        {"timestamp_ms": 131, "type": "key", "x": 160, "y": 132, "key": "W"},
        {"timestamp_ms": 214, "type": "drag", "x": 115, "y": 102, "to_x": 178, "to_y": 126},
        {"timestamp_ms": 300, "type": "scroll", "x": 464, "y": 94, "delta": -3},
        {"timestamp_ms": 382, "type": "click", "x": 485, "y": 92},
        {"timestamp_ms": 468, "type": "key", "x": 485, "y": 92, "key": "A"},
        {"timestamp_ms": 551, "type": "drag", "x": 470, "y": 92, "to_x": 449, "to_y": 118},
        {"timestamp_ms": 640, "type": "scroll", "x": 478, "y": 236, "delta": 5},
        {"timestamp_ms": 720, "type": "click", "x": 490, "y": 228},
        {"timestamp_ms": 800, "type": "key", "x": 490, "y": 228, "key": "Space"},
        {"timestamp_ms": 880, "type": "drag", "x": 446, "y": 224, "to_x": 602, "to_y": 298},
        {"timestamp_ms": 963, "type": "scroll", "x": 250, "y": 170, "delta": -2},
        {"timestamp_ms": 1035, "type": "click", "x": 220, "y": 150},
        {"timestamp_ms": 1100, "type": "key", "x": 250, "y": 170, "key": "D"},
        {"timestamp_ms": 1185, "type": "drag", "x": 184, "y": 126, "to_x": 246, "to_y": 154},
        {"timestamp_ms": 1270, "type": "scroll", "x": 462, "y": 100, "delta": 4},
        {"timestamp_ms": 1350, "type": "click", "x": 470, "y": 100},
        {"timestamp_ms": 1650, "type": "key", "x": 470, "y": 100, "key": "E"},
        {"timestamp_ms": 1820, "type": "drag", "x": 510, "y": 104, "to_x": 580, "to_y": 120},
        {"timestamp_ms": 1980, "type": "scroll", "x": 320, "y": 190, "delta": -6},
        {"timestamp_ms": 2300, "type": "click", "x": 300, "y": 180},
        {"timestamp_ms": 2420, "type": "drag", "x": 600, "y": 330, "to_x": 720, "to_y": 410},
        {"timestamp_ms": 2550, "type": "key", "x": 300, "y": 180, "key": "Q"},
        {"timestamp_ms": 2700, "type": "scroll", "x": 500, "y": 96, "delta": 2},
        {"timestamp_ms": 2870, "type": "click", "x": 470, "y": 96},
        {"timestamp_ms": 3070, "type": "drag", "x": 500, "y": 250, "to_x": 540, "to_y": 286},
        {"timestamp_ms": 3200, "type": "key", "x": 300, "y": 180, "key": "R"},
        {"timestamp_ms": 3350, "type": "scroll", "x": 220, "y": 140, "delta": -1},
    ]


def build_sample() -> dict[str, Any]:
    rng = random.Random(FRAME_SEED)
    frames = [
        {
            "seq": index + 1,
            "timestamp_ms": round(index * 1000 / FPS),
            "scene": _frame_scene(index + 1, rng),
        }
        for index in range(FRAME_COUNT)
    ]
    events: list[dict[str, Any]] = []
    for seq, spec in enumerate(_input_specs(), start=1):
        event = {"seq": seq, **spec}
        events.append(event)

    sender_caps = {
        "codecs": ["h264-baseline", "h264-high", "hevc-main"],
        "max_width": 640,
        "max_height": 360,
        "max_fps": 60,
        "supports_input": True,
    }
    receiver_caps = {
        "codecs": ["h264-baseline", "h264-high", "hevc-main"],
        "max_width": 1920,
        "max_height": 1080,
        "max_fps": 60,
        "supports_input": True,
    }
    return {
        "schema_version": SCHEMA_VERSION,
        "protocol_version": PROTOCOL_VERSION,
        "generator": GENERATOR_VERSION,
        "session_id": "cast-demo-uid-001",
        "session_name": "Neon Frontier · 多屏导演台",
        "sender_device_id": "game-pc-01",
        "receiver_device_id": "workbench-01",
        "devices": [
            {
                "id": "game-pc-01",
                "label": "Game Publisher / RTX Rig",
                "role": "sender",
                "capabilities": sender_caps,
            },
            {
                "id": "workbench-01",
                "label": "Director Workbench",
                "role": "receiver",
                "capabilities": receiver_caps,
            },
        ],
        "declared": {
            "width": DECLARED_WIDTH,
            "height": DECLARED_HEIGHT,
            "fps": FPS,
            "pixel_format": "RGBA8888",
            "input_coordinate_space": "source",
        },
        "requires_input_backchannel": True,
        "establish_time": "2026-11-22T10:00:00.000000Z",
        "disconnect_time": "2026-11-22T10:00:03.500000Z",
        "frame_seed": FRAME_SEED,
        "frames": frames,
        "inputs": events,
        "phase_script": [
            {"timestamp_ms": 1020, "action": "pause"},
            {"timestamp_ms": 1060, "action": "resume"},
            {"timestamp_ms": 2020, "action": "reconnect"},
            {"timestamp_ms": 2050, "action": "reconnect_complete"},
            {"timestamp_ms": 3500, "action": "disconnect"},
        ],
    }


def write_sample_set(directory: str | Path) -> dict[str, str]:
    from .loader import parse_session

    target = Path(directory)
    target.mkdir(parents=True, exist_ok=True)
    sample_data = build_sample()
    sample_path = target / "sample_session.json"
    sample_path.write_text(
        json.dumps(
            sample_data,
            ensure_ascii=False,
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )
    sample = parse_session(sample_data)
    negotiation = negotiate(sample)
    baseline_frames = []
    for frame in sample.frames:
        image = render_scene(
            frame.scene,
            negotiation.width,
            negotiation.height,
            sample.declared_width,
            sample.declared_height,
            {"frame_seq": frame.seq},
        )
        baseline_frames.append(
            {
                "seq": frame.seq,
                "timestamp_ms": frame.timestamp_ms,
                "source_image_sha256": image_digest(image),
            }
        )
    truth = {
        "schema_version": SCHEMA_VERSION,
        "protocol_version": PROTOCOL_VERSION,
        "generator": GENERATOR_VERSION,
        "session_id": sample.session_id,
        "content_hash": sample.content_hash,
        "timestamp_truth": {
            "establish_iso": sample.establish_time,
            "disconnect_iso": sample.disconnect_time,
            "establish_ms_relative": 0,
            "disconnect_ms_relative": 3500,
        },
        "counts": {
            "frames": len(sample.frames),
            "inputs": len(sample.inputs),
            "phase_actions": len(sample.phase_script),
            "frame_first_seq": sample.frames[0].seq,
            "frame_last_seq": sample.frames[-1].seq,
            "input_first_seq": sample.inputs[0].seq,
            "input_last_seq": sample.inputs[-1].seq,
        },
        "negotiation_expected": negotiation.to_dict(),
        "frame_sequence": [frame.seq for frame in sample.frames],
        "input_sequence": [event.seq for event in sample.inputs],
        "phase_script": [action.to_dict() for action in sample.phase_script],
        "source_frame_digests": baseline_frames,
        "clean_expected": {
            "frames_committed": len(sample.frames),
            "frames_missing": 0,
            "inputs_replayed": 27,
            "inputs_rejected": 1,
            "expected_error_codes": ["E_STATE"],
            "final_phase": "disconnected",
        },
    }
    truth_path = target / "truth.json"
    truth_path.write_text(
        json.dumps(truth, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    capabilities_path = target / "capabilities.json"
    capabilities_path.write_text(
        json.dumps(
            {
                "profiles": {
                    name: {
                        "rank": profile.rank,
                        "fragment_bytes": profile.fragment_bytes,
                        "compression_level": profile.compression_level,
                    }
                    for name, profile in CODEC_PROFILES.items()
                },
                "devices": {
                    device.id: device.capabilities.to_dict()
                    for device in sample.devices
                },
            },
            ensure_ascii=False,
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )
    write_presets(target / "anomalies.json")
    return {
        "sample": str(sample_path),
        "truth": str(truth_path),
        "capabilities": str(capabilities_path),
        "anomalies": str(target / "anomalies.json"),
    }


if __name__ == "__main__":
    print(json.dumps(write_sample_set(Path(__file__).parent.parent / "samples"), indent=2))
