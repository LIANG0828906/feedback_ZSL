from __future__ import annotations

import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Optional, Tuple

from .errors import E_PAIR_MISSING, BusinessError
from .util import file_hash, finite_number, load_json, require_keys


@dataclass(frozen=True)
class PairGeometry:
    source: str
    receiver: str
    distance_m: float
    position_distance_m: Optional[float]
    distance_delta_m: Optional[float]
    speed_of_sound_m_s: float
    basis: str


@dataclass(frozen=True)
class GeometryManifest:
    path: Path
    speed_of_sound_m_s: float
    distance_basis: str
    pairs: Dict[Tuple[str, str], PairGeometry]
    digest: str

    def pair(self, source: str, receiver: str) -> PairGeometry:
        key = (source, receiver)
        pair = self.pairs.get(key)
        if pair is None:
            raise BusinessError(
                E_PAIR_MISSING,
                "geometry pair is missing",
                source=source,
                receiver=receiver,
            )
        return pair


def _positions(payload: Dict[str, Any], key: str) -> Dict[str, tuple[float, ...]]:
    raw = payload.get(key, {})
    if not isinstance(raw, dict):
        return {}
    result: Dict[str, tuple[float, ...]] = {}
    for item_id, coordinates in raw.items():
        if not isinstance(coordinates, (list, tuple)) or len(coordinates) not in (2, 3):
            continue
        result[str(item_id)] = tuple(float(value) for value in coordinates)
    return result


def _distance(left: tuple[float, ...], right: tuple[float, ...]) -> float:
    width = max(len(left), len(right))
    padded_left = tuple(left) + (0.0,) * (width - len(left))
    padded_right = tuple(right) + (0.0,) * (width - len(right))
    return math.sqrt(sum((a - b) ** 2 for a, b in zip(padded_left, padded_right)))


def load_geometry(path: Path) -> GeometryManifest:
    payload = load_json(path)
    require_keys(
        payload,
        ("schema_version", "speed_of_sound_m_s", "distance_basis", "pairs"),
        "geometry",
    )
    speed = finite_number(payload["speed_of_sound_m_s"], "speed_of_sound_m_s")
    if speed <= 0.0:
        raise BusinessError(
            E_PAIR_MISSING,
            "speed_of_sound_m_s must be positive",
            details={"speed_of_sound_m_s": speed},
        )
    basis = str(payload["distance_basis"])
    speaker_positions = _positions(payload, "speaker_positions_m")
    receiver_positions = _positions(payload, "receiver_positions_m")
    raw_pairs = payload["pairs"]
    if not isinstance(raw_pairs, list):
        raise BusinessError(
            E_PAIR_MISSING,
            "geometry.pairs must be a list",
            details={"field": "pairs"},
        )
    pairs: Dict[Tuple[str, str], PairGeometry] = {}
    for index, raw in enumerate(raw_pairs):
        if not isinstance(raw, dict):
            raise BusinessError(
                E_PAIR_MISSING, f"geometry.pairs[{index}] must be an object"
            )
        source = str(raw.get("source", ""))
        receiver = str(raw.get("receiver", ""))
        if "distance_m" not in raw:
            raise BusinessError(
                E_PAIR_MISSING,
                "geometry pair is missing distance_m",
                source=source or None,
                receiver=receiver or None,
                details={"index": index},
            )
        distance = finite_number(raw["distance_m"], f"pairs[{index}].distance_m")
        if distance <= 0.0:
            raise BusinessError(
                E_PAIR_MISSING,
                "geometry distance_m must be positive",
                source=source,
                receiver=receiver,
                details={"distance_m": distance},
            )
        pos_distance: Optional[float] = None
        delta: Optional[float] = None
        if source in speaker_positions and receiver in receiver_positions:
            pos_distance = _distance(
                speaker_positions[source], receiver_positions[receiver]
            )
            delta = distance - pos_distance
            basis_detail = (
                f"{basis}; explicit distance_m={distance:.9f}, "
                f"euclidean={pos_distance:.9f}, delta={delta:.6f} m"
            )
        else:
            basis_detail = f"{basis}; explicit distance_m={distance:.9f}"
        pairs[(source, receiver)] = PairGeometry(
            source=source,
            receiver=receiver,
            distance_m=distance,
            position_distance_m=pos_distance,
            distance_delta_m=delta,
            speed_of_sound_m_s=speed,
            basis=basis_detail,
        )
    return GeometryManifest(
        path=path,
        speed_of_sound_m_s=speed,
        distance_basis=basis,
        pairs=pairs,
        digest=file_hash(path),
    )
