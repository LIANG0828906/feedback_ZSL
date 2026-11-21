"""Strict reader and atomic writer for ``.tcseq`` tiled sequences."""

from __future__ import annotations

import hashlib
import math
import os
import struct
import tempfile
import zlib
from pathlib import Path
from typing import Iterable

from .errors import (
    E_BIT_DEPTH_INVALID,
    E_BLOCK_HASH,
    E_DATA_LENGTH,
    E_GRID_INVALID,
    E_HEADER_INVALID,
    E_IO,
    E_TRANSFER_CODE,
    error,
)
from .model import SequenceHeader, TileBlock, TileSequence, TransferCode


MAGIC = b"TCSEQ1\x00\x00"
FORMAT_VERSION = 1
HEADER_SIZE = 56
BLOCK_HEADER = struct.Struct("<IIIHHdII")
HEADER_STRUCT = struct.Struct("<8sHHIIHHBBBBIIQIII")
BLOCK_HASH_SIZE = 32
VALID_BIT_DEPTHS = (8, 10, 12, 16)

assert HEADER_STRUCT.size == HEADER_SIZE


def _bytes_per_sample(bit_depth: int) -> int:
    return 1 if bit_depth == 8 else 2


def _header_payload(header: SequenceHeader, header_crc: int = 0) -> bytes:
    return HEADER_STRUCT.pack(
        MAGIC,
        FORMAT_VERSION,
        HEADER_SIZE,
        header.width,
        header.height,
        header.tiles_x,
        header.tiles_y,
        header.bit_depth,
        header.channels,
        int(header.source_transfer),
        int(header.target_transfer),
        header.conversion_id,
        header.block_count,
        header.data_length,
        header.flags,
        0,
        header_crc,
    )


def _block_meta(block: TileBlock) -> bytes:
    return BLOCK_HEADER.pack(
        block.tile_id,
        block.x,
        block.y,
        block.width,
        block.height,
        block.exposure,
        block.sample_count,
        len(block.data),
    )


def compute_block_hash(
    tile_id: int,
    x: int,
    y: int,
    width: int,
    height: int,
    exposure: float,
    sample_count: int,
    data: bytes,
) -> bytes:
    meta = BLOCK_HEADER.pack(tile_id, x, y, width, height, exposure, sample_count, len(data))
    return hashlib.sha256(meta + data).digest()


def make_block(
    tile_id: int,
    x: int,
    y: int,
    width: int,
    height: int,
    exposure: float,
    channels: int,
    data: bytes,
) -> TileBlock:
    sample_count = width * height * channels
    return TileBlock(
        tile_id=tile_id,
        x=x,
        y=y,
        width=width,
        height=height,
        exposure=float(exposure),
        sample_count=sample_count,
        data=data,
        content_hash=compute_block_hash(tile_id, x, y, width, height, exposure, sample_count, data),
    )


def pack_samples(values: Iterable[int], bit_depth: int) -> bytes:
    if bit_depth == 8:
        return bytes(values)
    bytearray_value = bytearray()
    for value in values:
        bytearray_value.extend(int(value).to_bytes(2, "little"))
    return bytes(bytearray_value)


def unpack_samples(data: bytes, bit_depth: int) -> list[int]:
    if bit_depth == 8:
        return list(data)
    return [int.from_bytes(data[pos : pos + 2], "little") for pos in range(0, len(data), 2)]


def _validate_header_fields(header: SequenceHeader) -> None:
    if header.width <= 0 or header.height <= 0:
        raise error(E_HEADER_INVALID, "width and height must be positive", field="width/height", width=header.width, height=header.height)
    if header.tiles_x <= 0 or header.tiles_y <= 0:
        raise error(E_HEADER_INVALID, "tile grid must be positive", field="tiles_x/tiles_y", tiles_x=header.tiles_x, tiles_y=header.tiles_y)
    if header.width % header.tiles_x != 0 or header.height % header.tiles_y != 0:
        raise error(
            E_GRID_INVALID,
            "tile grid does not divide image dimensions",
            field="tiles_x/tiles_y",
            collision_fields=["width", "tiles_x"] if header.width % header.tiles_x else ["height", "tiles_y"],
            width=header.width,
            height=header.height,
            tiles_x=header.tiles_x,
            tiles_y=header.tiles_y,
        )
    if header.bit_depth not in VALID_BIT_DEPTHS:
        raise error(
            E_BIT_DEPTH_INVALID,
            "unsupported bit depth",
            field="bit_depth",
            actual=header.bit_depth,
            allowed=list(VALID_BIT_DEPTHS),
        )
    if header.channels < 1 or header.channels > 4:
        raise error(E_HEADER_INVALID, "channels must be in 1..4", field="channels", actual=header.channels)
    for field_name, transfer in (
        ("source_transfer", header.source_transfer),
        ("target_transfer", header.target_transfer),
    ):
        try:
            TransferCode(int(transfer))
        except ValueError as exc:
            raise error(E_TRANSFER_CODE, "unknown transfer function code", field=field_name, actual=int(transfer)) from exc
    if header.block_count != header.tiles_x * header.tiles_y:
        raise error(
            E_HEADER_INVALID,
            "block_count does not match tile grid",
            field="block_count",
            actual=header.block_count,
            expected=header.tiles_x * header.tiles_y,
        )


def serialize_sequence(sequence: TileSequence) -> bytes:
    header = sequence.header
    _validate_header_fields(header)
    ordered = sequence.ordered_blocks()
    if [block.tile_id for block in ordered] != list(range(header.block_count)):
        raise error(
            E_GRID_INVALID,
            "tile ids must cover the declared grid exactly once",
            field="tile_id",
            block_count=header.block_count,
        )

    expected_data_length = 0
    body = bytearray()
    for block in ordered:
        expected_x = (block.tile_id % header.tiles_x) * header.tile_width
        expected_y = (block.tile_id // header.tiles_x) * header.tile_height
        if (block.x, block.y, block.width, block.height) != (
            expected_x,
            expected_y,
            header.tile_width,
            header.tile_height,
        ):
            raise error(
                E_GRID_INVALID,
                "tile geometry does not match declared grid",
                field="x/y/width/height",
                tile_id=block.tile_id,
                expected=[expected_x, expected_y, header.tile_width, header.tile_height],
                actual=[block.x, block.y, block.width, block.height],
            )
        expected_sample_count = block.width * block.height * header.channels
        expected_bytes = expected_sample_count * _bytes_per_sample(header.bit_depth)
        if block.sample_count != expected_sample_count:
            raise error(
                E_DATA_LENGTH,
                "block sample_count does not match geometry",
                field="sample_count",
                tile_id=block.tile_id,
                actual=block.sample_count,
                expected=expected_sample_count,
            )
        if len(block.data) != expected_bytes:
            raise error(
                E_DATA_LENGTH,
                "block payload length does not match sample_count",
                field="payload_length",
                tile_id=block.tile_id,
                actual=len(block.data),
                expected=expected_bytes,
            )
        if not math.isfinite(block.exposure) or abs(block.exposure) > 32:
            raise error(E_HEADER_INVALID, "invalid block exposure", field="exposure", tile_id=block.tile_id, actual=block.exposure)
        max_code = (1 << header.bit_depth) - 1
        if header.bit_depth > 8:
            for value in unpack_samples(block.data, header.bit_depth):
                if value > max_code:
                    raise error(
                        E_DATA_LENGTH,
                        "sample exceeds declared bit depth",
                        field="sample_value",
                        tile_id=block.tile_id,
                        actual=value,
                        max_code=max_code,
                    )
        expected_hash = compute_block_hash(
            block.tile_id,
            block.x,
            block.y,
            block.width,
            block.height,
            block.exposure,
            block.sample_count,
            block.data,
        )
        if block.content_hash and block.content_hash != expected_hash:
            raise error(E_BLOCK_HASH, "tile content hash mismatch", field="content_hash", tile_id=block.tile_id)
        meta = _block_meta(block)
        body.extend(meta)
        body.extend(block.data)
        body.extend(expected_hash)
        expected_data_length += len(block.data)

    if header.data_length != expected_data_length:
        raise error(
            E_DATA_LENGTH,
            "declared data length differs from payload total",
            field="data_length",
            actual=header.data_length,
            expected=expected_data_length,
        )
    header_no_crc = _header_payload(header, 0)
    crc = zlib.crc32(header_no_crc[:-4]) & 0xFFFFFFFF
    packed_header = _header_payload(header, crc)
    return packed_header + bytes(body)


def parse_sequence_bytes(data: bytes, source_path: str | None = None) -> TileSequence:
    if len(data) < HEADER_SIZE:
        raise error(E_HEADER_INVALID, "file is shorter than fixed header", field="header", actual=len(data), expected=HEADER_SIZE)
    fields = HEADER_STRUCT.unpack_from(data, 0)
    (
        magic,
        version,
        header_size,
        width,
        height,
        tiles_x,
        tiles_y,
        bit_depth,
        channels,
        source_transfer_raw,
        target_transfer_raw,
        conversion_id,
        block_count,
        data_length,
        flags,
        reserved,
        header_crc,
    ) = fields

    if magic != MAGIC:
        raise error(E_HEADER_INVALID, "bad magic", field="magic", actual=magic.hex())
    if version != FORMAT_VERSION:
        raise error(E_HEADER_INVALID, "unsupported container version", field="version", actual=version)
    if header_size != HEADER_SIZE:
        raise error(E_HEADER_INVALID, "bad header size", field="header_size", actual=header_size, expected=HEADER_SIZE)
    crc_actual = zlib.crc32(data[: HEADER_SIZE - 4]) & 0xFFFFFFFF
    if crc_actual != header_crc:
        raise error(
            E_HEADER_INVALID,
            "header CRC mismatch",
            field="header_crc",
            actual=header_crc,
            expected=crc_actual,
        )
    try:
        source_transfer = TransferCode(source_transfer_raw)
    except ValueError as exc:
        raise error(E_TRANSFER_CODE, "unknown source transfer code", field="source_transfer", actual=source_transfer_raw) from exc
    try:
        target_transfer = TransferCode(target_transfer_raw)
    except ValueError as exc:
        raise error(E_TRANSFER_CODE, "unknown target transfer code", field="target_transfer", actual=target_transfer_raw) from exc

    header = SequenceHeader(
        width=width,
        height=height,
        tiles_x=tiles_x,
        tiles_y=tiles_y,
        bit_depth=bit_depth,
        channels=channels,
        source_transfer=source_transfer,
        target_transfer=target_transfer,
        conversion_id=conversion_id,
        block_count=block_count,
        data_length=data_length,
        flags=flags,
    )
    _validate_header_fields(header)
    if reserved != 0:
        raise error(E_HEADER_INVALID, "reserved header field must be zero", field="reserved", actual=reserved)

    blocks: list[TileBlock] = []
    pos = HEADER_SIZE
    payload_total = 0
    seen_ids: set[int] = set()
    bps = _bytes_per_sample(bit_depth)
    max_code = (1 << bit_depth) - 1
    for _ in range(block_count):
        if pos + BLOCK_HEADER.size > len(data):
            raise error(E_DATA_LENGTH, "truncated block header", field="data", offset=pos)
        tile_id, x, y, tw, th, exposure, sample_count, payload_len = BLOCK_HEADER.unpack_from(data, pos)
        pos += BLOCK_HEADER.size
        expected_sample_count = tw * th * channels
        expected_payload_len = expected_sample_count * bps
        if sample_count != expected_sample_count:
            raise error(
                E_DATA_LENGTH,
                "block sample_count does not match dimensions",
                field="sample_count",
                tile_id=tile_id,
                actual=sample_count,
                expected=expected_sample_count,
            )
        if payload_len != expected_payload_len:
            raise error(
                E_DATA_LENGTH,
                "block payload length does not match sample_count",
                field="payload_length",
                tile_id=tile_id,
                actual=payload_len,
                expected=expected_payload_len,
            )
        if pos + payload_len + BLOCK_HASH_SIZE > len(data):
            raise error(E_DATA_LENGTH, "truncated block payload", field="data", tile_id=tile_id, offset=pos)
        payload = data[pos : pos + payload_len]
        pos += payload_len
        stored_hash = data[pos : pos + BLOCK_HASH_SIZE]
        pos += BLOCK_HASH_SIZE
        if tile_id >= block_count or tile_id in seen_ids:
            raise error(E_GRID_INVALID, "tile id is out of range or duplicated", field="tile_id", tile_id=tile_id)
        seen_ids.add(tile_id)
        expected_x = (tile_id % tiles_x) * (width // tiles_x)
        expected_y = (tile_id // tiles_x) * (height // tiles_y)
        if (x, y, tw, th) != (expected_x, expected_y, width // tiles_x, height // tiles_y):
            raise error(
                E_GRID_INVALID,
                "tile geometry does not match declared grid",
                field="x/y/width/height",
                tile_id=tile_id,
                expected=[expected_x, expected_y, width // tiles_x, height // tiles_y],
                actual=[x, y, tw, th],
            )
        if not math.isfinite(exposure) or abs(exposure) > 32:
            raise error(E_HEADER_INVALID, "invalid block exposure", field="exposure", tile_id=tile_id, actual=exposure)
        actual_hash = compute_block_hash(tile_id, x, y, tw, th, exposure, sample_count, payload)
        if actual_hash != stored_hash:
            raise error(
                E_BLOCK_HASH,
                "tile content hash mismatch",
                field="content_hash",
                tile_id=tile_id,
                actual=stored_hash.hex(),
                expected=actual_hash.hex(),
            )
        for value in unpack_samples(payload, bit_depth):
            if value > max_code:
                raise error(
                    E_DATA_LENGTH,
                    "sample exceeds declared bit depth",
                    field="sample_value",
                    tile_id=tile_id,
                    actual=value,
                    max_code=max_code,
                )
        payload_total += payload_len
        blocks.append(
            TileBlock(
                tile_id=tile_id,
                x=x,
                y=y,
                width=tw,
                height=th,
                exposure=exposure,
                sample_count=sample_count,
                data=payload,
                content_hash=stored_hash,
            )
        )

    if pos != len(data):
        raise error(
            E_DATA_LENGTH,
            "trailing bytes after declared blocks",
            field="data",
            actual=len(data),
            expected=pos,
        )
    if seen_ids != set(range(block_count)):
        raise error(E_GRID_INVALID, "tile id coverage is incomplete", field="tile_id")
    if data_length != payload_total:
        raise error(
            E_DATA_LENGTH,
            "declared data length differs from payload total",
            field="data_length",
            actual=data_length,
            expected=payload_total,
        )
    return TileSequence(
        header=header,
        blocks=sorted(blocks, key=lambda block: block.tile_id),
        source_path=source_path,
        file_sha256=hashlib.sha256(data).hexdigest(),
    )


def parse_sequence(path: str | os.PathLike[str]) -> TileSequence:
    source = Path(path)
    try:
        data = source.read_bytes()
    except OSError as exc:
        raise error(E_IO, "cannot read sequence", field="path", path=str(source), reason=str(exc)) from exc
    return parse_sequence_bytes(data, source_path=str(source))


def atomic_write_bytes(path: str | os.PathLike[str], data: bytes) -> None:
    """Write bytes through a same-directory temp file and atomic rename."""

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd = -1
    temp_name: str | None = None
    try:
        fd, temp_name = tempfile.mkstemp(
            prefix=f".{target.name}.",
            suffix=".tmp",
            dir=target.parent,
        )
        with os.fdopen(fd, "wb", closefd=True) as handle:
            fd = -1
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_name, target)
        temp_name = None
        dir_fd = os.open(target.parent, os.O_RDONLY)
        try:
            os.fsync(dir_fd)
        finally:
            os.close(dir_fd)
    except OSError as exc:
        raise error(E_IO, "atomic write failed", field="path", path=str(target), reason=str(exc)) from exc
    finally:
        if fd >= 0:
            os.close(fd)
        if temp_name is not None:
            try:
                os.unlink(temp_name)
            except FileNotFoundError:
                pass


def write_sequence_atomic(path: str | os.PathLike[str], sequence: TileSequence) -> str:
    data = serialize_sequence(sequence)
    atomic_write_bytes(path, data)
    return hashlib.sha256(data).hexdigest()
