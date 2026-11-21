"""Workspace measurement and geometry models."""

from __future__ import annotations

from dataclasses import dataclass, field
import math
from pathlib import Path
from typing import Any

from .config import AnalysisConfig, load_config
from .errors import E_CONFIG_INVALID, E_GEOMETRY_INVALID, E_MEASUREMENT_INVALID, VenueError
from .utils import read_json, sha256_json


@dataclass(frozen=True)
class Point3D:
    x: float
    y: float
    z: float

    @classmethod
    def from_value(cls, value: Any, *, owner: str) -> "Point3D":
        if not isinstance(value, (list, tuple)) or len(value) != 3:
            raise VenueError(
                E_GEOMETRY_INVALID,
                "position must be a three-element [x, y, z] array",
                {"entry": owner},
            )
        try:
            point = cls(float(value[0]), float(value[1]), float(value[2]))
        except (TypeError, ValueError) as exc:
            raise VenueError(
                E_GEOMETRY_INVALID,
                "position coordinates must be finite numbers",
                {"entry": owner},
            ) from exc
        if not all(math.isfinite(item) for item in (point.x, point.y, point.z)):
            raise VenueError(
                E_GEOMETRY_INVALID,
                "position coordinates must be finite",
                {"entry": owner},
            )
        return point

    def to_list(self) -> list[float]:
        return [self.x, self.y, self.z]


@dataclass(frozen=True)
class GeometryManifest:
    speakers: dict[str, Point3D]
    pickups: dict[str, Point3D]
    speed_of_sound_m_s: float = 343.0
    coordinate_system: str = "cartesian_m"

    @property
    def fingerprint(self) -> str:
        return sha256_json(self.to_dict())

    def to_dict(self) -> dict[str, Any]:
        return {
            "coordinate_system": self.coordinate_system,
            "speed_of_sound_m_s": self.speed_of_sound_m_s,
            "speakers": {
                key: {"position": point.to_list()}
                for key, point in sorted(self.speakers.items())
            },
            "pickups": {
                key: {"position": point.to_list()}
                for key, point in sorted(self.pickups.items())
            },
        }


@dataclass(frozen=True)
class PairSpec:
    pair_id: str
    source: str
    pickup: str
    wav_path: str
    layout: str
    source_channel_index: int | None
    pickup_channel_index: int
    reference_path: str | None = None
    reference_channel_index: int = 0
    distance_override_m: float | None = None
    primary_leakage: float | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.pair_id,
            "source": self.source,
            "pickup": self.pickup,
            "wav": self.wav_path,
            "layout": self.layout,
            "source_channel_index": self.source_channel_index,
            "pickup_channel_index": self.pickup_channel_index,
            "reference": self.reference_path,
            "reference_channel_index": self.reference_channel_index,
            "distance_m": self.distance_override_m,
            "primary_leakage": self.primary_leakage,
        }


@dataclass(frozen=True)
class Workspace:
    root: Path
    manifest: dict[str, Any]
    geometry: GeometryManifest
    config: AnalysisConfig
    pairs: tuple[PairSpec, ...]
    manifest_fingerprint: str = field(init=False)

    def __post_init__(self) -> None:
        object.__setattr__(self, "manifest_fingerprint", sha256_json(self.manifest))

    @property
    def pickups(self) -> list[str]:
        return sorted({pair.pickup for pair in self.pairs})

    @property
    def sources(self) -> list[str]:
        return sorted({pair.source for pair in self.pairs})

    def pair_map(self) -> dict[str, PairSpec]:
        return {pair.pair_id: pair for pair in self.pairs}


def _parse_geometry(data: dict[str, Any], source_path: str | Path) -> GeometryManifest:
    speed = float(data.get("speed_of_sound_m_s", 343.0))
    if speed <= 0:
        raise VenueError(
            E_GEOMETRY_INVALID,
            "speed_of_sound_m_s must be positive",
            {"path": str(source_path), "speed_of_sound_m_s": speed},
        )
    speakers_raw = data.get("speakers")
    pickups_raw = data.get("pickups")
    if not isinstance(speakers_raw, dict):
        raise VenueError(
            E_GEOMETRY_INVALID,
            "geometry manifest is missing speakers mapping",
            {"path": str(source_path), "missing_kind": "speakers"},
        )
    if not isinstance(pickups_raw, dict):
        raise VenueError(
            E_GEOMETRY_INVALID,
            "geometry manifest is missing pickups mapping",
            {"path": str(source_path), "missing_kind": "pickups"},
        )
    speakers: dict[str, Point3D] = {}
    pickups: dict[str, Point3D] = {}
    for key, value in sorted(speakers_raw.items()):
        if not isinstance(value, dict) or "position" not in value:
            raise VenueError(
                E_GEOMETRY_INVALID,
                "speaker entry is missing position",
                {"entry": key, "kind": "speaker"},
            )
        speakers[key] = Point3D.from_value(value["position"], owner=f"speaker:{key}")
    for key, value in sorted(pickups_raw.items()):
        if not isinstance(value, dict) or "position" not in value:
            raise VenueError(
                E_GEOMETRY_INVALID,
                "pickup entry is missing position",
                {"entry": key, "kind": "pickup"},
            )
        pickups[key] = Point3D.from_value(value["position"], owner=f"pickup:{key}")
    return GeometryManifest(
        speakers=speakers,
        pickups=pickups,
        speed_of_sound_m_s=speed,
        coordinate_system=str(data.get("coordinate_system", "cartesian_m")),
    )


def _parse_pair(raw: dict[str, Any], position: int) -> PairSpec:
    try:
        pair_id = str(raw["id"])
        source = str(raw["source"])
        pickup = str(raw["pickup"])
        wav_path = str(raw["wav"])
        layout = str(raw["layout"])
        pickup_channel_index = int(raw["pickup_channel_index"])
    except (KeyError, TypeError, ValueError) as exc:
        raise VenueError(
            E_MEASUREMENT_INVALID,
            "pair manifest entry is missing required fields",
            {"pair_index": position, "error": str(exc)},
        ) from exc
    source_channel_raw = raw.get("source_channel_index")
    source_channel_index = None if source_channel_raw is None else int(source_channel_raw)
    distance_override = raw.get("distance_m")
    if distance_override is not None:
        distance_override = float(distance_override)
        if distance_override < 0:
            raise VenueError(
                E_GEOMETRY_INVALID,
                "pair distance override must not be negative",
                {"pair": pair_id, "distance_m": distance_override},
            )
    primary_leakage = raw.get("primary_leakage")
    if primary_leakage is not None:
        primary_leakage = float(primary_leakage)
        if not (0.0 <= primary_leakage < 1.0):
            raise VenueError(
                E_CONFIG_INVALID,
                "pair primary_leakage must be in [0, 1)",
                {"pair": pair_id, "primary_leakage": primary_leakage},
            )
    return PairSpec(
        pair_id=pair_id,
        source=source,
        pickup=pickup,
        wav_path=wav_path,
        layout=layout,
        source_channel_index=source_channel_index,
        pickup_channel_index=pickup_channel_index,
        reference_path=None if raw.get("reference") is None else str(raw["reference"]),
        reference_channel_index=int(raw.get("reference_channel_index", 0)),
        distance_override_m=distance_override,
        primary_leakage=primary_leakage,
    )


def load_workspace(workspace: str | Path) -> Workspace:
    root = Path(workspace).resolve()
    manifest_path = root / "manifest.json"
    try:
        manifest = read_json(manifest_path)
    except (OSError, ValueError) as exc:
        raise VenueError(
            E_MEASUREMENT_INVALID,
            f"cannot read workspace manifest: {exc}",
            {"path": str(manifest_path)},
        ) from exc
    config_path = root / str(manifest.get("config_file", "crossover.json"))
    geometry_path = root / str(manifest.get("geometry_file", "geometry.json"))
    try:
        config = load_config(config_path)
    except VenueError:
        raise
    except Exception as exc:
        raise VenueError(E_CONFIG_INVALID, str(exc), {"path": str(config_path)}) from exc
    try:
        geometry_data = read_json(geometry_path)
    except (OSError, ValueError) as exc:
        raise VenueError(
            E_GEOMETRY_INVALID,
            f"cannot read geometry manifest: {exc}",
            {"path": str(geometry_path)},
        ) from exc
    geometry = _parse_geometry(geometry_data, geometry_path)
    declared_rate = int(manifest.get("sample_rate", config.sample_rate))
    if declared_rate != config.sample_rate:
        raise VenueError(
            E_CONFIG_INVALID,
            "manifest sample rate differs from crossover config",
            {
                "manifest_sample_rate": declared_rate,
                "config_sample_rate": config.sample_rate,
            },
        )
    raw_pairs = manifest.get("pairs")
    if not isinstance(raw_pairs, list) or not raw_pairs:
        raise VenueError(
            E_MEASUREMENT_INVALID,
            "workspace manifest must contain a non-empty pairs list",
            {"path": str(manifest_path)},
        )
    pairs = tuple(_parse_pair(raw, index) for index, raw in enumerate(raw_pairs))
    ids = [pair.pair_id for pair in pairs]
    if len(ids) != len(set(ids)):
        raise VenueError(E_MEASUREMENT_INVALID, "pair ids must be unique")
    return Workspace(
        root=root,
        manifest=manifest,
        geometry=geometry,
        config=config,
        pairs=pairs,
    )
