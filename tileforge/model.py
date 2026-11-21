"""Data models for the custom tiled sequence container."""

from __future__ import annotations

from dataclasses import dataclass, field, replace
from enum import IntEnum
from typing import Iterable


class TransferCode(IntEnum):
    SDR_GAMMA = 0
    PQ = 1
    HLG = 2


class Primaries(IntEnum):
    REC709 = 0
    DISPLAY_P3 = 1
    REC2020 = 2


TRANSFER_NAMES = {
    TransferCode.SDR_GAMMA: "sdr-gamma",
    TransferCode.PQ: "pq",
    TransferCode.HLG: "hlg",
}

TRANSFER_BY_NAME = {name: code for code, name in TRANSFER_NAMES.items()}
TRANSFER_BY_NAME.update({"sdr": TransferCode.SDR_GAMMA, "gamma": TransferCode.SDR_GAMMA})

PRIMARIES_NAMES = {
    Primaries.REC709: "rec709",
    Primaries.DISPLAY_P3: "display-p3",
    Primaries.REC2020: "rec2020",
}

PRIMARIES_BY_NAME = {name: value for value, name in PRIMARIES_NAMES.items()}
PRIMARIES_BY_NAME.update({"p3": Primaries.DISPLAY_P3, "bt709": Primaries.REC709, "bt2020": Primaries.REC2020})


@dataclass(frozen=True)
class SequenceHeader:
    width: int
    height: int
    tiles_x: int
    tiles_y: int
    bit_depth: int
    channels: int
    source_transfer: TransferCode
    target_transfer: TransferCode
    conversion_id: int
    block_count: int
    data_length: int
    flags: int = 0

    @property
    def tile_width(self) -> int:
        return self.width // self.tiles_x

    @property
    def tile_height(self) -> int:
        return self.height // self.tiles_y

    @property
    def bytes_per_sample(self) -> int:
        return 1 if self.bit_depth == 8 else 2

    @property
    def max_code(self) -> int:
        return (1 << self.bit_depth) - 1


@dataclass(frozen=True)
class TileBlock:
    tile_id: int
    x: int
    y: int
    width: int
    height: int
    exposure: float
    sample_count: int
    data: bytes
    content_hash: bytes

    @property
    def payload_size(self) -> int:
        return len(self.data)


@dataclass
class TileSequence:
    header: SequenceHeader
    blocks: list[TileBlock]
    source_path: str | None = None
    file_sha256: str | None = None

    def ordered_blocks(self) -> list[TileBlock]:
        return sorted(self.blocks, key=lambda block: block.tile_id)

    def block_map(self) -> dict[int, TileBlock]:
        return {block.tile_id: block for block in self.blocks}

    def with_blocks(self, blocks: Iterable[TileBlock]) -> "TileSequence":
        ordered = sorted(blocks, key=lambda block: block.tile_id)
        data_length = sum(block.payload_size for block in ordered)
        header = replace(
            self.header,
            block_count=len(ordered),
            data_length=data_length,
        )
        return TileSequence(header=header, blocks=ordered)

    def tile_ids(self) -> list[int]:
        return sorted(block.tile_id for block in self.blocks)

    def sample_values(self, block: TileBlock) -> list[int]:
        bps = self.header.bytes_per_sample
        if bps == 1:
            return list(block.data)
        return list(int.from_bytes(block.data[pos : pos + 2], "little") for pos in range(0, len(block.data), 2))
