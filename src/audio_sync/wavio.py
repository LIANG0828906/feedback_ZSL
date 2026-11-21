"""RIFF/WAVE input with a read-only memory map, plus atomic PCM output."""

from __future__ import annotations

import hashlib
import os
import struct
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import BinaryIO

import numpy as np

from .errors import E_INVALID_WAV, SyncError

_PCM_GUID_TAIL = b"\x00\x00\x00\x00\x10\x00\x80\x00\x00\xaa\x00\x38\x9b\x71"


@dataclass(frozen=True)
class WavInfo:
    path: Path
    sample_rate: int
    channels: int
    frames: int
    data_offset: int
    data_bytes: int
    file_size: int
    sha256: str
    container: str

    @property
    def duration_seconds(self) -> float:
        return self.frames / float(self.sample_rate)

    def to_dict(self) -> dict:
        return {
            "path": str(self.path),
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "frames": self.frames,
            "duration_seconds": self.duration_seconds,
            "data_bytes": self.data_bytes,
            "file_size": self.file_size,
            "sha256": self.sha256,
            "container": self.container,
        }


def _read_exact(handle: BinaryIO, count: int) -> bytes:
    data = handle.read(count)
    if len(data) != count:
        raise SyncError(E_INVALID_WAV, "unexpected end of WAV file")
    return data


def _parse_fmt(payload: bytes) -> tuple[int, int, int, int]:
    if len(payload) < 16:
        raise SyncError(E_INVALID_WAV, "fmt chunk is too short")
    format_tag, channels, sample_rate, _byte_rate, block_align, bits = struct.unpack_from(
        "<HHIIHH", payload, 0
    )
    if format_tag == 0xFFFE:
        if len(payload) < 40:
            raise SyncError(E_INVALID_WAV, "WAVE_FORMAT_EXTENSIBLE fmt chunk is too short")
        valid_bits = struct.unpack_from("<H", payload, 18)[0]
        subformat = payload[24:40]
        if subformat[:2] != b"\x01\x00" or subformat[2:] != _PCM_GUID_TAIL:
            raise SyncError(E_INVALID_WAV, "only PCM WAVE_FORMAT_EXTENSIBLE is supported")
        if valid_bits not in (0, 16):
            raise SyncError(E_INVALID_WAV, f"only 16-bit PCM is supported, got {valid_bits}-bit")
    elif format_tag != 1:
        raise SyncError(E_INVALID_WAV, f"only PCM WAV is supported, got format tag {format_tag}")

    if channels <= 0 or sample_rate <= 0:
        raise SyncError(E_INVALID_WAV, "invalid channel count or sample rate")
    if bits != 16:
        raise SyncError(E_INVALID_WAV, f"only 16-bit PCM is supported, got {bits}-bit")
    if block_align != channels * 2:
        raise SyncError(E_INVALID_WAV, "invalid block alignment for 16-bit PCM")
    return channels, sample_rate, bits, block_align


def inspect_wav(path: str | os.PathLike[str]) -> WavInfo:
    wav_path = Path(path).expanduser().resolve()
    try:
        file_size = wav_path.stat().st_size
        with wav_path.open("rb") as handle:
            header = _read_exact(handle, 12)
            if header[:4] not in (b"RIFF", b"RF64", b"RIFX") or header[8:12] != b"WAVE":
                raise SyncError(E_INVALID_WAV, "file is not a RIFF/WAVE file")
            if header[:4] == b"RIFX":
                raise SyncError(E_INVALID_WAV, "big-endian RIFX is not supported")
            container = "RF64" if header[:4] == b"RF64" else "RIFF"

            channels = sample_rate = 0
            data_offset = data_bytes = 0
            ds64_data_bytes: int | None = None
            ds64_frame_count: int | None = None
            while handle.tell() + 8 <= file_size:
                chunk_header = _read_exact(handle, 8)
                chunk_id = chunk_header[:4]
                chunk_size = struct.unpack("<I", chunk_header[4:])[0]
                payload_start = handle.tell()
                if chunk_id == b"fmt ":
                    payload = _read_exact(handle, chunk_size)
                    channels, sample_rate, _, _ = _parse_fmt(payload)
                elif chunk_id == b"ds64":
                    if chunk_size < 28:
                        raise SyncError(E_INVALID_WAV, "RF64 ds64 chunk is too short")
                    payload = _read_exact(handle, chunk_size)
                    _riff_size, ds64_data_bytes, ds64_frame_count, _table_length = (
                        struct.unpack_from("<QQQI", payload, 0)
                    )
                elif chunk_id == b"data":
                    data_offset = payload_start
                    declared_size = (
                        ds64_data_bytes
                        if container == "RF64" and chunk_size == 0xFFFFFFFF
                        else chunk_size
                    )
                    if declared_size is None:
                        raise SyncError(E_INVALID_WAV, "RF64 data chunk has no ds64 size")
                    data_bytes = min(
                        declared_size,
                        max(0, file_size - payload_start),
                    )
                    break
                handle.seek(payload_start + chunk_size + (chunk_size & 1))
    except FileNotFoundError as exc:
        raise SyncError(E_INVALID_WAV, f"WAV file not found: {wav_path}") from exc
    except OSError as exc:
        raise SyncError(E_INVALID_WAV, f"cannot read WAV file: {exc}") from exc

    if channels <= 0 or sample_rate <= 0:
        raise SyncError(E_INVALID_WAV, "missing fmt chunk")
    if data_offset <= 0:
        raise SyncError(E_INVALID_WAV, "missing data chunk")
    block_align = channels * 2
    frames = data_bytes // block_align
    if ds64_frame_count is not None and ds64_frame_count < frames:
        frames = int(ds64_frame_count)
    if frames <= 0:
        raise SyncError(E_INVALID_WAV, "WAV contains no complete PCM frames")

    return WavInfo(
        path=wav_path,
        sample_rate=sample_rate,
        channels=channels,
        frames=frames,
        data_offset=data_offset,
        data_bytes=data_bytes,
        file_size=file_size,
        sha256=hash_file(wav_path),
        container=container,
    )


def hash_file(path: str | os.PathLike[str]) -> str:
    digest = hashlib.sha256()
    with Path(path).open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


class WavReader:
    """Read-only multichannel PCM access backed by ``numpy.memmap``."""

    def __init__(self, path: str | os.PathLike[str]):
        self.info = inspect_wav(path)
        self._handle = self.info.path.open("rb")
        self._data = np.memmap(
            self.info.path,
            dtype="<i2",
            mode="r",
            offset=self.info.data_offset,
            shape=(self.info.frames, self.info.channels),
        )

    @property
    def channels(self) -> int:
        return self.info.channels

    @property
    def sample_rate(self) -> int:
        return self.info.sample_rate

    @property
    def frames(self) -> int:
        return self.info.frames

    def read_i16(self, channel: int, start: int = 0, stop: int | None = None) -> np.ndarray:
        self._validate_channel(channel)
        stop = self.frames if stop is None else min(stop, self.frames)
        start = max(0, start)
        if stop <= start:
            return np.empty(0, dtype=np.int16)
        return np.array(self._data[start:stop, channel], dtype=np.int16, copy=True)

    def read_float(self, channel: int, start: int = 0, stop: int | None = None) -> np.ndarray:
        return self.read_i16(channel, start, stop).astype(np.float64) / 32768.0

    def _validate_channel(self, channel: int) -> None:
        if not 0 <= channel < self.channels:
            raise SyncError(
                E_INVALID_WAV,
                f"channel {channel} is out of range for {self.channels}-channel WAV",
            )

    def close(self) -> None:
        data = getattr(self, "_data", None)
        if data is not None:
            mmap_handle = getattr(data, "_mmap", None)
            if mmap_handle is not None:
                mmap_handle.close()
            del self._data
        handle = getattr(self, "_handle", None)
        if handle is not None:
            handle.close()
            del self._handle

    def __enter__(self) -> "WavReader":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()


def _atomic_replace_bytes(path: Path, writer) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(prefix=f".{path.name}.", suffix=".tmp", dir=path.parent)
    os.close(fd)
    temp_path = Path(temp_name)
    try:
        writer(temp_path)
        with temp_path.open("rb") as handle:
            os.fsync(handle.fileno())
        os.replace(temp_path, path)
        _fsync_dir(path.parent)
    except BaseException:
        temp_path.unlink(missing_ok=True)
        raise


def _fsync_dir(path: Path) -> None:
    try:
        fd = os.open(path, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_wav_i16(
    path: str | os.PathLike[str],
    data: np.ndarray,
    sample_rate: int,
) -> None:
    """Write a channel-first int16 array atomically using a temporary WAV."""

    array = np.asarray(data)
    if array.ndim != 2 or array.shape[0] <= 0 or array.shape[1] <= 0:
        raise SyncError(E_INVALID_WAV, "output data must have shape (channels, frames)")
    if array.dtype != np.int16:
        raise SyncError(E_INVALID_WAV, "output data must be int16")
    with AtomicWavSegmentWriter(
        path,
        channels=array.shape[0],
        sample_rate=sample_rate,
        total_frames=array.shape[1],
    ) as writer:
        writer.write_segment(array)
        writer.commit()


def write_wav_segments_i16(
    path: str | os.PathLike[str],
    segments: list[np.ndarray],
    sample_rate: int,
) -> None:
    """Write channel-first segment blocks without concatenating the whole file."""

    if not segments:
        raise SyncError(E_INVALID_WAV, "no compensated segments to write")
    channel_count = segments[0].shape[0]
    if any(segment.shape[0] != channel_count or segment.ndim != 2 for segment in segments):
        raise SyncError(E_INVALID_WAV, "compensated segments are not channel-aligned")
    total_frames = sum(int(segment.shape[1]) for segment in segments)
    with AtomicWavSegmentWriter(
        path,
        channels=channel_count,
        sample_rate=sample_rate,
        total_frames=total_frames,
    ) as writer:
        for segment in segments:
            writer.write_segment(np.asarray(segment, dtype=np.int16))
        writer.commit()


_RIFF_DATA_LIMIT = 0xFFFFFFFF - 128


def _pcm_wav_header(
    channels: int,
    sample_rate: int,
    frames: int,
    rf64: bool,
) -> bytes:
    data_bytes = frames * channels * 2
    fmt_payload = struct.pack(
        "<HHIIHH",
        1,
        channels,
        sample_rate,
        sample_rate * channels * 2,
        channels * 2,
        16,
    )
    fmt_chunk = b"fmt " + struct.pack("<I", len(fmt_payload)) + fmt_payload
    padding = b"\x00" if data_bytes & 1 else b""
    if rf64:
        ds64_payload_size = 28
        file_size = (
            12
            + 8
            + ds64_payload_size
            + len(fmt_chunk)
            + 8
            + data_bytes
            + len(padding)
        )
        riff_size = file_size - 8
        ds64_payload = struct.pack("<QQQI", riff_size, data_bytes, frames, 0)
        ds64_chunk = b"ds64" + struct.pack("<I", ds64_payload_size) + ds64_payload
        data_header = b"data" + struct.pack("<I", 0xFFFFFFFF)
        return b"RF64" + struct.pack("<I", 0xFFFFFFFF) + b"WAVE" + ds64_chunk + fmt_chunk + data_header

    file_size = 12 + len(fmt_chunk) + 8 + data_bytes + len(padding)
    riff_size = file_size - 8
    if riff_size > 0xFFFFFFFF:
        raise SyncError(E_INVALID_WAV, "RIFF output exceeds the 32-bit WAV size limit")
    return (
        b"RIFF"
        + struct.pack("<I", riff_size)
        + b"WAVE"
        + fmt_chunk
        + b"data"
        + struct.pack("<I", data_bytes)
    )


class AtomicWavSegmentWriter:
    """Stream channel-aligned segment blocks to a temporary WAV, then rename."""

    def __init__(
        self,
        path: str | os.PathLike[str],
        channels: int,
        sample_rate: int,
        total_frames: int,
    ):
        self.destination = Path(path).expanduser().resolve()
        self.channels = int(channels)
        self.sample_rate = int(sample_rate)
        self.total_frames = int(total_frames)
        if self.total_frames <= 0:
            raise SyncError(E_INVALID_WAV, "total_frames must be positive")
        self._temp_path: Path | None = None
        self._handle: BinaryIO | None = None
        self._frames_written = 0
        self._rf64 = self.total_frames * self.channels * 2 > _RIFF_DATA_LIMIT
        self._committed = False

    def __enter__(self) -> "AtomicWavSegmentWriter":
        self.destination.parent.mkdir(parents=True, exist_ok=True)
        fd, name = tempfile.mkstemp(
            prefix=f".{self.destination.name}.",
            suffix=".tmp",
            dir=self.destination.parent,
        )
        os.close(fd)
        self._temp_path = Path(name)
        self._handle = self._temp_path.open("wb")
        self._handle.write(
            _pcm_wav_header(
                self.channels,
                self.sample_rate,
                self.total_frames,
                self._rf64,
            )
        )
        return self

    def write_segment(self, segment: np.ndarray) -> None:
        if self._handle is None:
            raise RuntimeError("WAV writer is not open")
        array = np.asarray(segment)
        if array.ndim != 2 or array.shape[0] != self.channels or array.dtype != np.int16:
            raise SyncError(E_INVALID_WAV, "segment must be channel-first int16")
        if self._frames_written + array.shape[1] > self.total_frames:
            raise SyncError(E_INVALID_WAV, "segments exceed declared WAV frame count")
        self._handle.write(np.ascontiguousarray(array.T).tobytes(order="C"))
        self._frames_written += int(array.shape[1])

    def commit(self) -> None:
        if self._handle is None or self._temp_path is None:
            raise RuntimeError("WAV writer is not open")
        if self._frames_written != self.total_frames:
            raise SyncError(
                E_INVALID_WAV,
                f"WAV frame count mismatch: wrote {self._frames_written}, expected {self.total_frames}",
            )
        if self._frames_written * self.channels * 2 & 1:
            self._handle.write(b"\x00")
        self._handle.flush()
        os.fsync(self._handle.fileno())
        self._handle.close()
        self._handle = None
        with self._temp_path.open("rb") as handle:
            os.fsync(handle.fileno())
        os.replace(self._temp_path, self.destination)
        _fsync_dir(self.destination.parent)
        self._temp_path = None
        self._committed = True

    def __exit__(self, exc_type, exc, tb) -> None:
        if self._handle is not None:
            self._handle.close()
            self._handle = None
        if not self._committed and self._temp_path is not None:
            self._temp_path.unlink(missing_ok=True)
            self._temp_path = None


def write_npz_atomic(path: str | os.PathLike[str], **arrays: np.ndarray) -> None:
    destination = Path(path).expanduser().resolve()

    def write(temp_path: Path) -> None:
        with temp_path.open("wb") as handle:
            np.savez_compressed(handle, **arrays)
            handle.flush()
            os.fsync(handle.fileno())

    _atomic_replace_bytes(destination, write)


def write_json_atomic(path: str | os.PathLike[str], payload: dict) -> None:
    import json

    destination = Path(path).expanduser().resolve()

    def write(temp_path: Path) -> None:
        with temp_path.open("w", encoding="utf-8") as handle:
            json.dump(payload, handle, ensure_ascii=False, indent=2, sort_keys=True)
            handle.flush()
            os.fsync(handle.fileno())

    _atomic_replace_bytes(destination, write)
