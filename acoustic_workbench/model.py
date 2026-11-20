from __future__ import annotations

import copy
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from .errors import E_CONFIG_INVALID, E_FACTOR_INVALID, fail


DEFAULT_CHANNEL_LAYOUT = ["FL", "FR", "FC", "LFE", "BL", "BR"]
DEFAULT_BAND_CENTERS = [125.0, 250.0, 500.0, 1000.0, 2000.0, 4000.0]


@dataclass
class Point:
    id: str
    x: float
    y: float
    z: float = 1.2

    def to_dict(self) -> dict[str, Any]:
        return {"id": self.id, "x": self.x, "y": self.y, "z": self.z}


@dataclass
class Wall:
    id: str
    x1: float
    y1: float
    x2: float
    y2: float
    material: str

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "x1": self.x1,
            "y1": self.y1,
            "x2": self.x2,
            "y2": self.y2,
            "material": self.material,
        }


@dataclass
class Scene:
    scene_id: str
    sample_rate: int
    channel_layout: list[str]
    reference_channel: str
    source: Point
    listeners: list[Point]
    walls: list[Wall]
    ir_files: dict[str, str]
    test_signal: str
    alignment: dict[str, Any] = field(default_factory=dict)
    early: dict[str, Any] = field(default_factory=dict)
    bands: dict[str, Any] = field(default_factory=dict)
    late: dict[str, Any] = field(default_factory=dict)
    render: dict[str, Any] = field(default_factory=dict)
    materials_file: str = "materials.json"

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> "Scene":
        try:
            source_payload = payload["source"]
            source = Point(
                id="source",
                x=float(source_payload["x"]),
                y=float(source_payload["y"]),
                z=float(source_payload.get("z", 1.5)),
            )
            listeners = [
                Point(
                    id=str(item["id"]),
                    x=float(item["x"]),
                    y=float(item["y"]),
                    z=float(item.get("z", 1.2)),
                )
                for item in payload["listeners"]
            ]
            walls = [
                Wall(
                    id=str(item["id"]),
                    x1=float(item["x1"]),
                    y1=float(item["y1"]),
                    x2=float(item["x2"]),
                    y2=float(item["y2"]),
                    material=str(item["material"]),
                )
                for item in payload["walls"]
            ]
            scene = cls(
                scene_id=str(payload["scene_id"]),
                sample_rate=int(payload.get("sample_rate", 48000)),
                channel_layout=list(
                    payload.get("channel_layout", DEFAULT_CHANNEL_LAYOUT)
                ),
                reference_channel=str(payload.get("reference_channel", "FC")),
                source=source,
                listeners=listeners,
                walls=walls,
                ir_files={str(k): str(v) for k, v in payload["ir_files"].items()},
                test_signal=str(payload.get("test_signal", "test_signal.wav")),
                alignment=dict(payload.get("alignment", {})),
                early=dict(payload.get("early", {})),
                bands=dict(payload.get("bands", {})),
                late=dict(payload.get("late", {})),
                render=dict(payload.get("render", {})),
                materials_file=str(payload.get("materials_file", "materials.json")),
            )
        except (KeyError, TypeError, ValueError) as exc:
            fail(E_CONFIG_INVALID, "invalid scene.json", error=str(exc))
        scene.validate()
        return scene

    @classmethod
    def load(cls, path: str | Path) -> "Scene":
        source = Path(path)
        try:
            payload = json.loads(source.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            fail(E_CONFIG_INVALID, f"cannot load scene: {source}", error=str(exc))
        return cls.from_dict(payload)

    def validate(self) -> None:
        if self.sample_rate != 48000:
            fail(
                E_CONFIG_INVALID,
                "sample rate must be 48000 Hz",
                sample_rate=self.sample_rate,
            )
        if len(self.channel_layout) != 6:
            fail(
                E_CONFIG_INVALID,
                "scene must declare exactly six 5.1 channels",
                channels=self.channel_layout,
            )
        if self.reference_channel not in self.channel_layout:
            fail(
                E_CONFIG_INVALID,
                "reference channel is not in channel layout",
                reference=self.reference_channel,
            )
        ids = [point.id for point in self.listeners]
        if not ids or len(ids) != len(set(ids)):
            fail(E_CONFIG_INVALID, "listener ids must be non-empty and unique")
        if set(ids) != set(self.ir_files):
            fail(
                E_CONFIG_INVALID,
                "ir_files must contain exactly one entry for every listener",
                listeners=ids,
                ir_files=sorted(self.ir_files),
            )
        wall_ids = [wall.id for wall in self.walls]
        if not wall_ids or len(wall_ids) != len(set(wall_ids)):
            fail(E_CONFIG_INVALID, "wall ids must be non-empty and unique")
        self.alignment = {
            "threshold": 0.02,
            "max_shift_samples": 8192,
            **self.alignment,
        }
        self.early = {
            "start_ms": 1.0,
            "end_ms": 80.0,
            "threshold_ratio": 0.03,
            "min_gap_ms": 1.5,
            "fft_size": 1024,
            **self.early,
        }
        self.bands = {
            "mode": "octave",
            "fraction": 1,
            "center_frequencies": DEFAULT_BAND_CENTERS,
            **self.bands,
        }
        self.late = {
            "tail_ms": 800.0,
            "seed": 20261120,
            "min_t60_s": 0.08,
            "max_t60_s": 8.0,
            **self.late,
        }
        self.render = {
            "max_seconds": 3.0,
            "normalize_peak": 0.89,
            **self.render,
        }

    def listener(self, listener_id: str) -> Point:
        for point in self.listeners:
            if point.id == listener_id:
                return point
        fail(E_CONFIG_INVALID, f"unknown listener: {listener_id}")

    def wall(self, wall_id: str) -> Wall:
        for wall in self.walls:
            if wall.id == wall_id:
                return wall
        fail(E_CONFIG_INVALID, f"unknown wall: {wall_id}")

    def resolved_ir_path(self, base_dir: str | Path, listener_id: str) -> Path:
        return Path(base_dir) / self.ir_files[listener_id]

    def resolved_test_signal(self, base_dir: str | Path) -> Path:
        return Path(base_dir) / self.test_signal

    def to_dict(self) -> dict[str, Any]:
        return {
            "scene_id": self.scene_id,
            "sample_rate": self.sample_rate,
            "channel_layout": list(self.channel_layout),
            "reference_channel": self.reference_channel,
            "source": {
                "x": self.source.x,
                "y": self.source.y,
                "z": self.source.z,
            },
            "listeners": [point.to_dict() for point in self.listeners],
            "walls": [wall.to_dict() for wall in self.walls],
            "ir_files": dict(sorted(self.ir_files.items())),
            "test_signal": self.test_signal,
            "alignment": copy.deepcopy(self.alignment),
            "early": copy.deepcopy(self.early),
            "bands": copy.deepcopy(self.bands),
            "late": copy.deepcopy(self.late),
            "render": copy.deepcopy(self.render),
            "materials_file": self.materials_file,
        }

    def clone(self) -> "Scene":
        return Scene.from_dict(copy.deepcopy(self.to_dict()))


def load_materials(path: str | Path) -> dict[str, Any]:
    source = Path(path)
    try:
        payload = json.loads(source.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(E_CONFIG_INVALID, f"cannot load materials: {source}", error=str(exc))
    if not isinstance(payload, dict) or not payload:
        fail(E_CONFIG_INVALID, "materials.json must be a non-empty object")
    for name, material in payload.items():
        absorption = (
            material.get("absorption")
            if isinstance(material, dict)
            else material
        )
        if not isinstance(absorption, list) or not absorption:
            fail(
                E_CONFIG_INVALID,
                f"material {name} must provide a non-empty absorption array",
            )
    return payload


def geometry_digest(scene: Scene, materials: dict[str, Any]) -> str:
    from .hashing import sha256_json

    used_materials = {
        wall.material: materials[wall.material]
        for wall in scene.walls
        if wall.material in materials
    }
    return sha256_json(
        {
            "source": scene.to_dict()["source"],
            "listeners": [point.to_dict() for point in scene.listeners],
            "walls": [wall.to_dict() for wall in scene.walls],
            "materials": used_materials,
        }
    )


def band_centers(scene: Scene) -> list[float]:
    centers = scene.bands.get("center_frequencies", DEFAULT_BAND_CENTERS)
    if isinstance(centers, dict):
        centers = list(centers.keys())
    try:
        return [float(value) for value in centers]
    except (TypeError, ValueError):
        fail(E_CONFIG_INVALID, "band center frequencies must be numeric")


def _position(change: dict[str, Any]) -> tuple[float, float, float]:
    if "position" in change:
        value = change["position"]
        if not isinstance(value, (list, tuple)) or len(value) not in (2, 3):
            fail(E_FACTOR_INVALID, "position must contain x, y and optional z")
        x, y = float(value[0]), float(value[1])
        z = float(value[2]) if len(value) == 3 else 1.2
        return x, y, z
    if "x" not in change or "y" not in change:
        fail(E_FACTOR_INVALID, "position change requires position or x/y")
    return float(change["x"]), float(change["y"]), float(change.get("z", 1.2))


def normalize_factor_name(value: str) -> str:
    aliases = {
        "material": "material",
        "wall_material": "material",
        "source": "source_position",
        "source_pos": "source_position",
        "source_location": "source_position",
        "source_position": "source_position",
        "listener": "listener_position",
        "listener_pos": "listener_position",
        "receiver_position": "listener_position",
        "listener_position": "listener_position",
        "bands": "band_config",
        "band": "band_config",
        "band_config": "band_config",
        "band_configuration": "band_config",
        "window": "window",
        "window_params": "window",
        "early_params": "window",
        "early_window": "window",
        "alignment": "alignment",
        "late": "late_config",
        "late_config": "late_config",
    }
    normalized = aliases.get(str(value).strip().lower())
    if normalized is None:
        fail(E_FACTOR_INVALID, f"unknown influence factor: {value}")
    return normalized


def apply_factor(
    scene: Scene,
    materials: dict[str, Any],
    change: dict[str, Any],
) -> tuple[Scene, str, dict[str, Any]]:
    """Apply one modification to a cloned scene and return scope metadata."""

    updated = scene.clone()
    factor = normalize_factor_name(
        change.get("factor", change.get("type", change.get("kind", "")))
    )
    scope: dict[str, Any] = {"factor": factor}

    if factor == "material":
        wall_id = str(change.get("wall_id", change.get("wall", "")))
        material = str(change.get("material", change.get("name", "")))
        if not wall_id or not material:
            fail(E_FACTOR_INVALID, "material change requires wall_id and material")
        if material not in materials:
            fail(E_FACTOR_INVALID, f"unknown material: {material}")
        updated.wall(wall_id).material = material
        scope.update({"wall_id": wall_id, "material": material})

    elif factor == "source_position":
        x, y, z = _position(change)
        updated.source.x, updated.source.y, updated.source.z = x, y, z
        scope["position"] = [x, y, z]

    elif factor == "listener_position":
        listener_id = str(
            change.get("listener_id", change.get("listener", change.get("id", "")))
        )
        if not listener_id:
            fail(E_FACTOR_INVALID, "listener_position requires listener_id")
        x, y, z = _position(change)
        point = updated.listener(listener_id)
        point.x, point.y, point.z = x, y, z
        scope.update({"listener_id": listener_id, "position": [x, y, z]})

    elif factor == "band_config":
        payload = change.get("bands", change.get("config", change))
        if not isinstance(payload, dict):
            fail(E_FACTOR_INVALID, "band_config must be an object")
        if payload is change:
            payload = {
                key: value
                for key, value in payload.items()
                if key not in ("factor", "type", "kind")
            }
        if "center_frequencies" not in payload and "frequencies" in payload:
            payload = dict(payload)
            payload["center_frequencies"] = payload.pop("frequencies")
        updated.bands.update(copy.deepcopy(payload))
        scope["bands"] = copy.deepcopy(payload)

    elif factor == "window":
        payload = change.get(
            "window", change.get("early", change.get("params", change.get("config")))
        )
        if not isinstance(payload, dict):
            fail(E_FACTOR_INVALID, "window change must be an object")
        updated.early.update(copy.deepcopy(payload))
        scope["window"] = copy.deepcopy(payload)

    elif factor == "alignment":
        payload = change.get("alignment", change.get("config"))
        if not isinstance(payload, dict):
            fail(E_FACTOR_INVALID, "alignment change must be an object")
        updated.alignment.update(copy.deepcopy(payload))
        scope["alignment"] = copy.deepcopy(payload)

    elif factor == "late_config":
        payload = change.get("late", change.get("config"))
        if not isinstance(payload, dict):
            fail(E_FACTOR_INVALID, "late_config change must be an object")
        updated.late.update(copy.deepcopy(payload))
        scope["late"] = copy.deepcopy(payload)

    updated.validate()
    return updated, factor, scope

