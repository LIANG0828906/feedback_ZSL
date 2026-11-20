from __future__ import annotations

import hashlib
import math
import struct
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import E_HEIGHT_INVALID, TerrainError
from .io_utils import atomic_write_bytes


HEIGHT_MAGIC = b"THF1\r\n\x1a\n"
HEIGHT_VERSION = 1
# magic, version, width, height, type, spacing, quantization, min, max,
# SHA-256(payload), reserved. The on-disk header is exactly 96 bytes.
HEIGHT_HEADER = struct.Struct("<8sIIII4d32s8s")
HEIGHT_DTYPE_BY_CODE = {1: np.dtype("<f4"), 2: np.dtype("<f8")}
HEIGHT_CODE_BY_DTYPE = {np.dtype("float32"): 1, np.dtype("float64"): 2}


@dataclass(frozen=True)
class HeightField:
    heights: np.ndarray
    spacing: float
    quantization_step: float
    valid_min: float
    valid_max: float
    source_digest: str

    def __post_init__(self) -> None:
        heights = np.asarray(self.heights)
        if heights.ndim != 2 or min(heights.shape) < 2:
            raise TerrainError(
                E_HEIGHT_INVALID,
                "height grid must be two-dimensional and at least 2x2",
                shape=list(heights.shape),
            )
        if heights.dtype not in (np.dtype("float32"), np.dtype("float64")):
            raise TerrainError(
                E_HEIGHT_INVALID,
                "height samples must be float32 or float64",
                dtype=str(heights.dtype),
            )
        object.__setattr__(self, "heights", np.array(heights, copy=True, order="C"))
        self._validate_values()

    def _validate_values(self) -> None:
        if not math.isfinite(self.spacing) or self.spacing <= 0.0:
            raise TerrainError(
                E_HEIGHT_INVALID, "grid spacing must be finite and positive", spacing=self.spacing
            )
        if not (
            math.isfinite(self.quantization_step) and self.quantization_step > 0.0
        ):
            raise TerrainError(
                E_HEIGHT_INVALID,
                "quantization step must be finite and positive",
                quantization_step=self.quantization_step,
            )
        if not (
            math.isfinite(self.valid_min)
            and math.isfinite(self.valid_max)
            and self.valid_min < self.valid_max
        ):
            raise TerrainError(
                E_HEIGHT_INVALID,
                "valid height range must be finite with min < max",
                valid_min=self.valid_min,
                valid_max=self.valid_max,
            )
        if not np.isfinite(self.heights).all():
            raise TerrainError(E_HEIGHT_INVALID, "height grid contains NaN or infinity")
        minimum = float(np.min(self.heights))
        maximum = float(np.max(self.heights))
        dtype_epsilon = float(np.finfo(self.heights.dtype).eps)
        magnitude = max(1.0, abs(self.valid_min), abs(self.valid_max))
        tolerance = max(1e-12, dtype_epsilon * magnitude * 4.0)
        if minimum < self.valid_min - tolerance or maximum > self.valid_max + tolerance:
            raise TerrainError(
                E_HEIGHT_INVALID,
                "height sample is outside the declared valid range",
                sample_min=minimum,
                sample_max=maximum,
                valid_min=self.valid_min,
                valid_max=self.valid_max,
            )

    @property
    def width(self) -> int:
        return int(self.heights.shape[1])

    @property
    def height(self) -> int:
        return int(self.heights.shape[0])

    @property
    def dtype_code(self) -> int:
        return HEIGHT_CODE_BY_DTYPE[self.heights.dtype]

    @property
    def height_digest(self) -> str:
        return hashlib.sha256(self.heights.tobytes(order="C")).hexdigest()

    @property
    def world_width(self) -> float:
        return (self.width - 1) * self.spacing

    @property
    def world_height(self) -> float:
        return (self.height - 1) * self.spacing

    @property
    def elevation_range(self) -> float:
        return self.valid_max - self.valid_min

    def normalized(self) -> np.ndarray:
        span = self.elevation_range
        result = (self.heights.astype(np.float64) - self.valid_min) / span
        return np.clip(result, 0.0, 1.0)

    def world_position(self, row: float, col: float) -> tuple[float, float, float]:
        return (
            float(col) * self.spacing,
            self.height_at(row, col),
            float(row) * self.spacing,
        )

    def height_at(self, row: float, col: float) -> float:
        row = min(max(float(row), 0.0), float(self.height - 1))
        col = min(max(float(col), 0.0), float(self.width - 1))
        r0 = min(int(math.floor(row)), self.height - 1)
        c0 = min(int(math.floor(col)), self.width - 1)
        r1 = min(r0 + 1, self.height - 1)
        c1 = min(c0 + 1, self.width - 1)
        fr = row - r0
        fc = col - c0
        top = self.heights[r0, c0] * (1.0 - fc) + self.heights[r0, c1] * fc
        bottom = self.heights[r1, c0] * (1.0 - fc) + self.heights[r1, c1] * fc
        return float(top * (1.0 - fr) + bottom * fr)

    def to_bytes(self) -> bytes:
        payload = np.ascontiguousarray(self.heights).tobytes(order="C")
        digest = hashlib.sha256(payload).digest()
        header = HEIGHT_HEADER.pack(
            HEIGHT_MAGIC,
            HEIGHT_VERSION,
            self.width,
            self.height,
            self.dtype_code,
            float(self.spacing),
            float(self.quantization_step),
            float(self.valid_min),
            float(self.valid_max),
            digest,
            b"\x00" * 8,
        )
        return header + payload

    def save(self, path: str | Path) -> None:
        atomic_write_bytes(path, self.to_bytes())

    @classmethod
    def from_bytes(cls, data: bytes) -> "HeightField":
        if len(data) < HEIGHT_HEADER.size:
            raise TerrainError(
                E_HEIGHT_INVALID,
                "height file is shorter than its header",
                expected_at_least=HEIGHT_HEADER.size,
                actual=len(data),
            )
        (
            magic,
            version,
            width,
            height,
            dtype_code,
            spacing,
            quantization_step,
            valid_min,
            valid_max,
            payload_digest,
            reserved,
        ) = HEIGHT_HEADER.unpack_from(data, 0)
        if magic != HEIGHT_MAGIC:
            raise TerrainError(E_HEIGHT_INVALID, "invalid height-file magic")
        if version != HEIGHT_VERSION:
            raise TerrainError(
                E_HEIGHT_INVALID, "unsupported height-file version", version=version
            )
        if width < 2 or height < 2:
            raise TerrainError(
                E_HEIGHT_INVALID,
                "height dimensions must each be at least 2",
                width=width,
                height=height,
            )
        if dtype_code not in HEIGHT_DTYPE_BY_CODE:
            raise TerrainError(
                E_HEIGHT_INVALID, "unsupported elevation type", elevation_type=dtype_code
            )
        if reserved != b"\x00" * 8:
            raise TerrainError(E_HEIGHT_INVALID, "height-file reserved bytes are not zero")
        dtype = HEIGHT_DTYPE_BY_CODE[dtype_code]
        expected_size = HEIGHT_HEADER.size + width * height * dtype.itemsize
        if len(data) != expected_size:
            raise TerrainError(
                E_HEIGHT_INVALID,
                "height payload size does not match header",
                expected=expected_size,
                actual=len(data),
            )
        payload = data[HEIGHT_HEADER.size :]
        actual_digest = hashlib.sha256(payload).digest()
        if actual_digest != payload_digest:
            raise TerrainError(E_HEIGHT_INVALID, "height payload checksum mismatch")
        heights = np.frombuffer(payload, dtype=dtype).reshape((height, width)).copy()
        heights = heights.astype(dtype.newbyteorder("="), copy=False)
        return cls(
            heights=heights,
            spacing=float(spacing),
            quantization_step=float(quantization_step),
            valid_min=float(valid_min),
            valid_max=float(valid_max),
            source_digest=hashlib.sha256(data).hexdigest(),
        )

    @classmethod
    def load(cls, path: str | Path) -> "HeightField":
        try:
            data = Path(path).read_bytes()
        except OSError as exc:
            raise TerrainError(
                E_HEIGHT_INVALID, "cannot read height file", path=str(path), error=str(exc)
            ) from exc
        return cls.from_bytes(data)

