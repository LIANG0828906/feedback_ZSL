"""Image sequence binary format and frame/block indexing."""

from __future__ import annotations

import hashlib
import os
import struct
from dataclasses import dataclass
from enum import IntEnum
from pathlib import Path
from typing import Iterator, List, Optional, Sequence, Tuple

import numpy as np

from .errors import E_IMG_INVALID, WorkbenchError


MAGIC = b"SCIIMG01"
FORMAT_VERSION = 1
PIXEL_FORMAT_MONO16_LE = 1
PIXEL_FORMAT_MONO16_BE = 2
HEADER_STRUCT = struct.Struct("<8sHHIIIHHQQII")
HEADER_SIZE = 64


class PixelFormat(IntEnum):
    MONO16_LE = PIXEL_FORMAT_MONO16_LE
    MONO16_BE = PIXEL_FORMAT_MONO16_BE


@dataclass(frozen=True)
class ImageHeader:
    width: int
    height: int
    frame_count: int
    bit_depth: int
    pixel_format: PixelFormat
    data_offset: int
    frame_stride: int
    header_size: int = HEADER_SIZE
    format_version: int = FORMAT_VERSION
    flags: int = 0
    reserved: int = 0

    @property
    def bytes_per_pixel(self) -> int:
        return (self.bit_depth + 7) // 8

    @property
    def bytes_per_frame(self) -> int:
        return self.width * self.height * self.bytes_per_pixel

    def to_bytes(self) -> bytes:
        packed = HEADER_STRUCT.pack(
            MAGIC,
            self.format_version,
            self.header_size,
            self.width,
            self.height,
            self.frame_count,
            self.bit_depth,
            int(self.pixel_format),
            self.data_offset,
            self.frame_stride,
            self.flags,
            self.reserved,
        )
        if self.header_size < len(packed):
            raise ValueError("header_size is smaller than the fixed header")
        return packed + b"\x00" * (self.header_size - len(packed))

    def canonical_dict(self) -> dict:
        return {
            "width": int(self.width),
            "height": int(self.height),
            "frame_count": int(self.frame_count),
            "bit_depth": int(self.bit_depth),
            "pixel_format": self.pixel_format.name,
            "data_offset": int(self.data_offset),
            "frame_stride": int(self.frame_stride),
            "header_size": int(self.header_size),
            "format_version": int(self.format_version),
            "flags": int(self.flags),
            "reserved": int(self.reserved),
        }


@dataclass(frozen=True)
class BlockRef:
    ordinal: int
    frame_index: int
    block_index: int
    y0: int
    y1: int
    x0: int
    x1: int

    @property
    def shape(self) -> Tuple[int, int]:
        return (self.y1 - self.y0, self.x1 - self.x0)

    @property
    def pixel_count(self) -> int:
        return (self.y1 - self.y0) * (self.x1 - self.x0)

    def as_dict(self) -> dict:
        return {
            "ordinal": self.ordinal,
            "frame": self.frame_index,
            "block": self.block_index,
            "y0": self.y0,
            "y1": self.y1,
            "x0": self.x0,
            "x1": self.x1,
        }


@dataclass(frozen=True)
class FrameRef:
    frame_index: int
    data_offset: int
    block_indices: Tuple[int, ...]


@dataclass(frozen=True)
class SequenceStructure:
    width: int
    height: int
    frame_count: int
    block_size: int
    block_rows: int
    block_cols: int
    blocks_per_frame: int
    frames: Tuple[FrameRef, ...]
    blocks: Tuple[BlockRef, ...]

    @property
    def total_blocks(self) -> int:
        return len(self.blocks)

    def block(self, frame_index: int, block_index: int) -> BlockRef:
        if frame_index < 0 or frame_index >= self.frame_count:
            raise IndexError("frame index out of range")
        if block_index < 0 or block_index >= self.blocks_per_frame:
            raise IndexError("block index out of range")
        return self.blocks[frame_index * self.blocks_per_frame + block_index]

    def block_at_ordinal(self, ordinal: int) -> BlockRef:
        return self.blocks[ordinal]

    def iter_frame_blocks(self, frame_index: int) -> Iterator[BlockRef]:
        start = frame_index * self.blocks_per_frame
        end = start + self.blocks_per_frame
        return iter(self.blocks[start:end])

    def block_id_for_pixel(self, frame_index: int, y: int, x: int) -> int:
        block_row = y // self.block_size
        block_col = x // self.block_size
        return block_row * self.block_cols + block_col

    def grid_coord_for_block(self, block_index: int) -> Tuple[int, int]:
        return divmod(block_index, self.block_cols)

    def halo_block_ids(
        self, block_index: int, *, radius: int = 1
    ) -> Tuple[int, ...]:
        row, col = self.grid_coord_for_block(block_index)
        result: List[int] = []
        for rr in range(max(0, row - radius), min(self.block_rows, row + radius + 1)):
            for cc in range(
                max(0, col - radius), min(self.block_cols, col + radius + 1)
            ):
                result.append(rr * self.block_cols + cc)
        return tuple(result)

    def canonical_dict(self) -> dict:
        return {
            "width": self.width,
            "height": self.height,
            "frame_count": self.frame_count,
            "block_size": self.block_size,
            "block_rows": self.block_rows,
            "block_cols": self.block_cols,
            "blocks_per_frame": self.blocks_per_frame,
            "total_blocks": self.total_blocks,
            "frames": [
                {
                    "frame": frame.frame_index,
                    "data_offset": frame.data_offset,
                    "blocks": list(frame.block_indices),
                }
                for frame in self.frames
            ],
        }


def build_structure(
    width: int,
    height: int,
    frame_count: int,
    block_size: int,
    data_offset: int = HEADER_SIZE,
    frame_stride: Optional[int] = None,
) -> SequenceStructure:
    if block_size <= 0:
        raise ValueError("block_size must be positive")
    if frame_stride is None:
        frame_stride = width * height * 2
    block_rows = (height + block_size - 1) // block_size
    block_cols = (width + block_size - 1) // block_size
    blocks_per_frame = block_rows * block_cols
    frames: List[FrameRef] = []
    blocks: List[BlockRef] = []
    for frame_index in range(frame_count):
        frame_blocks: List[int] = []
        for block_row in range(block_rows):
            y0 = block_row * block_size
            y1 = min(height, y0 + block_size)
            for block_col in range(block_cols):
                x0 = block_col * block_size
                x1 = min(width, x0 + block_size)
                block_index = block_row * block_cols + block_col
                ordinal = len(blocks)
                blocks.append(
                    BlockRef(
                        ordinal=ordinal,
                        frame_index=frame_index,
                        block_index=block_index,
                        y0=y0,
                        y1=y1,
                        x0=x0,
                        x1=x1,
                    )
                )
                frame_blocks.append(block_index)
        frames.append(
            FrameRef(
                frame_index=frame_index,
                data_offset=data_offset + frame_index * frame_stride,
                block_indices=tuple(frame_blocks),
            )
        )
    return SequenceStructure(
        width=width,
        height=height,
        frame_count=frame_count,
        block_size=block_size,
        block_rows=block_rows,
        block_cols=block_cols,
        blocks_per_frame=blocks_per_frame,
        frames=tuple(frames),
        blocks=tuple(blocks),
    )


def _read_header(path: Path, file_size: int) -> ImageHeader:
    if file_size < HEADER_STRUCT.size:
        raise WorkbenchError(
            E_IMG_INVALID,
            "file is shorter than the fixed sequence header",
            frame=0,
            path=str(path),
        )
    with path.open("rb") as handle:
        raw = handle.read(HEADER_STRUCT.size)
    (
        magic,
        version,
        header_size,
        width,
        height,
        frame_count,
        bit_depth,
        pixel_format_value,
        data_offset,
        frame_stride,
        flags,
        reserved,
    ) = HEADER_STRUCT.unpack(raw)
    if magic != MAGIC:
        raise WorkbenchError(
            E_IMG_INVALID,
            "unknown image-sequence magic",
            frame=0,
            path=str(path),
        )
    if version != FORMAT_VERSION:
        raise WorkbenchError(
            E_IMG_INVALID,
            "unsupported format version {}".format(version),
            frame=0,
            path=str(path),
        )
    if header_size < HEADER_STRUCT.size or header_size > file_size:
        raise WorkbenchError(
            E_IMG_INVALID,
            "header size is inconsistent with file length",
            frame=0,
            path=str(path),
        )
    if width <= 0 or height <= 0 or frame_count <= 0:
        raise WorkbenchError(
            E_IMG_INVALID,
            "width, height and frame count must all be positive",
            frame=0,
            path=str(path),
        )
    if bit_depth != 16:
        raise WorkbenchError(
            E_IMG_INVALID,
            "only 16-bit monochrome sequences are supported",
            frame=0,
            path=str(path),
        )
    try:
        pixel_format = PixelFormat(pixel_format_value)
    except ValueError:
        raise WorkbenchError(
            E_IMG_INVALID,
            "unknown pixel format {}".format(pixel_format_value),
            frame=0,
            path=str(path),
        )
    expected_stride = width * height * 2
    if frame_stride != expected_stride:
        raise WorkbenchError(
            E_IMG_INVALID,
            "frame stride contradicts width, height and bit depth",
            frame=0,
            path=str(path),
            details={
                "frame_stride": int(frame_stride),
                "expected_frame_stride": int(expected_stride),
            },
        )
    if data_offset < header_size:
        raise WorkbenchError(
            E_IMG_INVALID,
            "frame data offset overlaps the header",
            frame=0,
            path=str(path),
        )
    expected_size = data_offset + frame_count * frame_stride
    if file_size < expected_size:
        available = max(0, file_size - data_offset)
        first_bad_frame = min(frame_count - 1, available // frame_stride)
        raise WorkbenchError(
            E_IMG_INVALID,
            "frame data is truncated",
            frame=first_bad_frame,
            path=str(path),
            details={
                "expected_file_size": int(expected_size),
                "actual_file_size": int(file_size),
            },
        )
    if file_size > expected_size:
        raise WorkbenchError(
            E_IMG_INVALID,
            "file contains bytes beyond the declared frame data",
            frame=frame_count - 1,
            path=str(path),
            details={
                "expected_file_size": int(expected_size),
                "actual_file_size": int(file_size),
            },
        )
    return ImageHeader(
        width=width,
        height=height,
        frame_count=frame_count,
        bit_depth=bit_depth,
        pixel_format=pixel_format,
        data_offset=data_offset,
        frame_stride=frame_stride,
        header_size=header_size,
        format_version=version,
        flags=flags,
        reserved=reserved,
    )


class ImageSequence:
    """An immutable in-memory sequence plus its frame/block index."""

    def __init__(
        self,
        path: Path,
        header: ImageHeader,
        pixels: np.ndarray,
        structure: SequenceStructure,
        sequence_hash: str,
    ) -> None:
        self.path = Path(path)
        self.header = header
        self.pixels = pixels
        self.structure = structure
        self.sequence_hash = sequence_hash
        pixels.setflags(write=False)

    @property
    def shape(self) -> Tuple[int, int, int]:
        return (
            self.header.frame_count,
            self.header.height,
            self.header.width,
        )

    def frame(self, frame_index: int) -> np.ndarray:
        return self.pixels[frame_index]

    def frame_bytes(self, frame_index: int) -> bytes:
        dtype = "<u2" if self.header.pixel_format == PixelFormat.MONO16_LE else ">u2"
        return np.asarray(self.pixels[frame_index], dtype=dtype).tobytes(order="C")

    def frame_digest(self, frame_index: int) -> str:
        return hashlib.sha256(self.frame_bytes(frame_index)).hexdigest()


def load_sequence(path: os.PathLike, *, block_size: int = 64) -> ImageSequence:
    seq_path = Path(path)
    try:
        file_size = seq_path.stat().st_size
    except OSError as exc:
        raise WorkbenchError(
            E_IMG_INVALID,
            "cannot stat image sequence: {}".format(exc),
            frame=0,
            path=str(seq_path),
        ) from exc
    header = _read_header(seq_path, file_size)
    count = header.width * header.height * header.frame_count
    dtype = np.dtype("<u2")
    try:
        flat = np.fromfile(seq_path, dtype=dtype, count=count, offset=header.data_offset)
    except OSError as exc:
        raise WorkbenchError(
            E_IMG_INVALID,
            "cannot read frame data: {}".format(exc),
            frame=0,
            path=str(seq_path),
        ) from exc
    if flat.size != count:
        first_bad_frame = min(header.frame_count - 1, flat.size // (header.width * header.height))
        raise WorkbenchError(
            E_IMG_INVALID,
            "frame data was truncated while reading",
            frame=first_bad_frame,
            path=str(seq_path),
        )
    pixels = flat.reshape(header.frame_count, header.height, header.width)
    if header.pixel_format == PixelFormat.MONO16_BE:
        pixels = pixels.astype(">u2").byteswap().astype(np.uint16)
    else:
        pixels = pixels.astype(np.uint16, copy=False)
    structure = build_structure(
        header.width,
        header.height,
        header.frame_count,
        block_size,
        data_offset=header.data_offset,
        frame_stride=header.frame_stride,
    )
    digest = hashlib.sha256()
    digest.update(header.to_bytes())
    digest.update(np.ascontiguousarray(pixels, dtype="<u2").tobytes(order="C"))
    return ImageSequence(
        path=seq_path,
        header=header,
        pixels=pixels,
        structure=structure,
        sequence_hash=digest.hexdigest(),
    )


def write_sequence(
    path: os.PathLike,
    frames: np.ndarray,
    *,
    pixel_format: PixelFormat = PixelFormat.MONO16_LE,
    atomic: bool = True,
) -> ImageSequence:
    arr = np.asarray(frames)
    if arr.ndim != 3:
        raise ValueError("frames must have shape (frame, height, width)")
    if arr.dtype != np.uint16:
        raise ValueError("frames must use uint16 pixel values")
    frame_count, height, width = arr.shape
    data_offset = HEADER_SIZE
    header = ImageHeader(
        width=width,
        height=height,
        frame_count=frame_count,
        bit_depth=16,
        pixel_format=pixel_format,
        data_offset=data_offset,
        frame_stride=width * height * 2,
    )
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    temp_path = destination.with_name(
        ".{}.tmp-{}-{}".format(destination.name, os.getpid(), id(arr))
    )
    dtype = "<u2" if pixel_format == PixelFormat.MONO16_LE else ">u2"
    try:
        with temp_path.open("wb") as handle:
            handle.write(header.to_bytes())
            handle.write(np.ascontiguousarray(arr, dtype=dtype).tobytes(order="C"))
            handle.flush()
            os.fsync(handle.fileno())
        if atomic:
            os.replace(temp_path, destination)
        else:
            os.unlink(destination) if destination.exists() else None
            temp_path.replace(destination)
    finally:
        if temp_path.exists():
            temp_path.unlink()
    return load_sequence(destination)


def structure_from_sequence(sequence: ImageSequence, block_size: int) -> SequenceStructure:
    return build_structure(
        sequence.header.width,
        sequence.header.height,
        sequence.header.frame_count,
        block_size,
        data_offset=sequence.header.data_offset,
        frame_stride=sequence.header.frame_stride,
    )


parse_sequence = load_sequence
read_sequence = load_sequence
