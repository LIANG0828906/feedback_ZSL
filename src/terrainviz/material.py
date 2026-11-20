from __future__ import annotations

import hashlib
import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import E_HEIGHT_INVALID, TerrainError
from .io_utils import atomic_write_bytes


MATERIAL_MAGIC = b"TMAT1\r\n\x00"
MATERIAL_VERSION = 1
# magic, version, width, height, item size, SHA-256(payload), reserved.
MATERIAL_HEADER = struct.Struct("<8sIIII32s8s")


@dataclass(frozen=True)
class MaterialMap:
    indices: np.ndarray
    source_digest: str

    def __post_init__(self) -> None:
        indices = np.asarray(self.indices)
        if indices.ndim != 2 or min(indices.shape) < 1:
            raise TerrainError(
                E_HEIGHT_INVALID,
                "material index image must be a non-empty 2-D array",
                shape=list(indices.shape),
            )
        if indices.dtype != np.dtype("uint8"):
            raise TerrainError(
                E_HEIGHT_INVALID,
                "material index image must use uint8 indices",
                dtype=str(indices.dtype),
            )
        object.__setattr__(
            self, "indices", np.array(indices, dtype=np.uint8, copy=True, order="C")
        )

    @property
    def width(self) -> int:
        return int(self.indices.shape[1])

    @property
    def height(self) -> int:
        return int(self.indices.shape[0])

    @property
    def material_version(self) -> str:
        return hashlib.sha256(self.indices.tobytes(order="C")).hexdigest()

    def to_bytes(self) -> bytes:
        payload = np.ascontiguousarray(self.indices).tobytes(order="C")
        digest = hashlib.sha256(payload).digest()
        header = MATERIAL_HEADER.pack(
            MATERIAL_MAGIC,
            MATERIAL_VERSION,
            self.width,
            self.height,
            1,
            digest,
            b"\x00" * 8,
        )
        return header + payload

    def save(self, path: str | Path) -> None:
        atomic_write_bytes(path, self.to_bytes())

    @classmethod
    def from_bytes(cls, data: bytes) -> "MaterialMap":
        if len(data) < MATERIAL_HEADER.size:
            raise TerrainError(
                E_HEIGHT_INVALID,
                "material file is shorter than its header",
                expected_at_least=MATERIAL_HEADER.size,
                actual=len(data),
            )
        magic, version, width, height, item_size, digest, reserved = (
            MATERIAL_HEADER.unpack_from(data, 0)
        )
        if magic != MATERIAL_MAGIC:
            raise TerrainError(E_HEIGHT_INVALID, "invalid material-file magic")
        if version != MATERIAL_VERSION:
            raise TerrainError(
                E_HEIGHT_INVALID, "unsupported material-file version", version=version
            )
        if width < 1 or height < 1:
            raise TerrainError(
                E_HEIGHT_INVALID, "material image dimensions must be positive"
            )
        if item_size != 1:
            raise TerrainError(
                E_HEIGHT_INVALID, "material index items must be uint8", item_size=item_size
            )
        if reserved != b"\x00" * 8:
            raise TerrainError(E_HEIGHT_INVALID, "material-file reserved bytes are not zero")
        expected_size = MATERIAL_HEADER.size + width * height
        if len(data) != expected_size:
            raise TerrainError(
                E_HEIGHT_INVALID,
                "material payload size does not match header",
                expected=expected_size,
                actual=len(data),
            )
        payload = data[MATERIAL_HEADER.size :]
        if hashlib.sha256(payload).digest() != digest:
            raise TerrainError(E_HEIGHT_INVALID, "material payload checksum mismatch")
        indices = np.frombuffer(payload, dtype=np.uint8).reshape((height, width)).copy()
        return cls(indices=indices, source_digest=hashlib.sha256(data).hexdigest())

    @classmethod
    def load(cls, path: str | Path) -> "MaterialMap":
        try:
            data = Path(path).read_bytes()
        except OSError as exc:
            raise TerrainError(
                E_HEIGHT_INVALID, "cannot read material file", path=str(path), error=str(exc)
            ) from exc
        return cls.from_bytes(data)


PALETTE = np.array(
    [
        [74, 112, 61],
        [105, 137, 72],
        [189, 174, 118],
        [128, 119, 105],
        [110, 105, 101],
        [236, 238, 240],
        [66, 98, 122],
        [122, 87, 54],
        [86, 130, 96],
        [151, 132, 92],
        [197, 191, 181],
        [58, 73, 83],
        [145, 154, 107],
        [171, 128, 96],
        [92, 102, 117],
        [218, 216, 198],
    ],
    dtype=np.float64,
)


def sample_material_color(
    material: MaterialMap, heightfield_width: int, heightfield_height: int, row: np.ndarray, col: np.ndarray
) -> np.ndarray:
    """Bilinearly sample palette colors at heightfield sample coordinates."""
    mat_row = np.asarray(row, dtype=np.float64)
    mat_col = np.asarray(col, dtype=np.float64)
    mr = np.clip(mat_row * (material.height - 1) / max(heightfield_height - 1, 1), 0.0, material.height - 1)
    mc = np.clip(mat_col * (material.width - 1) / max(heightfield_width - 1, 1), 0.0, material.width - 1)
    r0 = np.minimum(np.floor(mr).astype(np.int64), material.height - 1)
    c0 = np.minimum(np.floor(mc).astype(np.int64), material.width - 1)
    r1 = np.minimum(r0 + 1, material.height - 1)
    c1 = np.minimum(c0 + 1, material.width - 1)
    fr = mr - r0
    fc = mc - c0
    idx00 = material.indices[r0, c0].astype(np.int64)
    idx01 = material.indices[r0, c1].astype(np.int64)
    idx10 = material.indices[r1, c0].astype(np.int64)
    idx11 = material.indices[r1, c1].astype(np.int64)
    w00 = (1.0 - fr) * (1.0 - fc)
    w01 = (1.0 - fr) * fc
    w10 = fr * (1.0 - fc)
    w11 = fr * fc
    return (
        PALETTE[idx00 % len(PALETTE)] * w00[..., None]
        + PALETTE[idx01 % len(PALETTE)] * w01[..., None]
        + PALETTE[idx10 % len(PALETTE)] * w10[..., None]
        + PALETTE[idx11 % len(PALETTE)] * w11[..., None]
    )

