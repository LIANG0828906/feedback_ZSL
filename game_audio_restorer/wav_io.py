"""Strict 16-bit PCM WAV parsing and atomic WAV emission."""

from __future__ import annotations

import hashlib
import os
import struct
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import numpy as np

from .errors import BusinessError, ErrorCode

_RIFF = b"RIFF"
_WAVE = b"WAVE"
_FMT = b"fmt "
_DATA = b"data"
_PCM = 1
_MIN_RATE = 8_000
_MAX_RATE = 48_000
_VALID_CHANNELS = (1, 2)
_BITS_PER_SAMPLE = 16


@dataclass(frozen=True)
class AudioData:
    """Internal PCM representation.

    ``samples`` is a float64 array shaped ``(frames, channels)`` and scaled to
    the nominal range [-1.0, 0.999969...].  The original byte layout is kept in
    metadata so exports and reports can be audited.
    """

    samples: np.ndarray
    sample_rate: int
    source_path: str
    sha256: str
    byte_length: int
    data_offset: int

    @property
    def channels(self) -> int:
        return int(self.samples.shape[1])

    @property
    def frames(self) -> int:
        return int(self.samples.shape[0])

    @property
    def duration_s(self) -> float:
        return self.frames / float(self.sample_rate)


def _wav_error(
    message: str,
    path: str | Path,
    byte_offset: int,
    field: str,
    **extra: Any,
) -> BusinessError:
    return BusinessError(
        ErrorCode.WAV_INVALID,
        message,
        {
            "location": {
                "file": os.fspath(path),
                "byte_offset": int(byte_offset),
                "field": field,
                **extra,
            }
        },
    )


def read_wav(path: str | os.PathLike[str]) -> AudioData:
    """Read and validate a mono/stereo 16-bit PCM WAV file.

    Only canonical PCM RIFF/WAVE is accepted.  Unknown chunks are skipped, but
    the RIFF size, chunk sizes, byte rate, block alignment and data length are
    all checked against the concrete file bytes.
    """

    source = Path(path)
    try:
        raw = source.read_bytes()
    except OSError as exc:
        raise BusinessError(
            ErrorCode.WAV_INVALID,
            f"无法读取 WAV 文件: {exc}",
            {"location": {"file": os.fspath(source), "byte_offset": 0, "field": "file"}},
        ) from exc

    if len(raw) < 12:
        raise _wav_error("WAV 文件短于 RIFF 头", source, 0, "riff_header", size=len(raw))
    if raw[0:4] != _RIFF:
        raise _wav_error("RIFF 魔数不匹配", source, 0, "riff_magic", actual=raw[0:4].hex())
    if raw[8:12] != _WAVE:
        raise _wav_error("WAVE 标识不匹配", source, 8, "wave_magic", actual=raw[8:12].hex())

    riff_size = struct.unpack_from("<I", raw, 4)[0]
    if riff_size + 8 != len(raw):
        raise _wav_error(
            "RIFF 声明长度与实际文件长度不一致",
            source,
            4,
            "riff_size",
            declared=riff_size + 8,
            actual=len(raw),
        )

    offset = 12
    fmt: tuple[int, int, int, int, int, int] | None = None
    data_offset: int | None = None
    data_size: int | None = None

    while offset < len(raw):
        if offset + 8 > len(raw):
            raise _wav_error(
                "WAV chunk 头被截断",
                source,
                offset,
                "chunk_header",
                remaining=len(raw) - offset,
            )
        chunk_id = raw[offset : offset + 4]
        chunk_size = struct.unpack_from("<I", raw, offset + 4)[0]
        body = offset + 8
        body_end = body + chunk_size
        if body_end > len(raw):
            raise _wav_error(
                "chunk 声明长度超过文件末尾",
                source,
                offset + 4,
                "chunk_size",
                chunk=chunk_id.decode("latin-1", errors="replace"),
                declared=chunk_size,
                available=len(raw) - body,
            )

        if chunk_id == _FMT:
            if fmt is not None:
                raise _wav_error("存在重复 fmt chunk", source, offset, "fmt_duplicate")
            if chunk_size < 16:
                raise _wav_error(
                    "fmt chunk 小于 PCM 最小长度",
                    source,
                    offset + 4,
                    "fmt_size",
                    declared=chunk_size,
                )
            (
                audio_format,
                channels,
                sample_rate,
                byte_rate,
                block_align,
                bits_per_sample,
            ) = struct.unpack_from("<HHIIHH", raw, body)
            fmt = (audio_format, channels, sample_rate, byte_rate, block_align, bits_per_sample)
        elif chunk_id == _DATA:
            if data_offset is not None:
                raise _wav_error("存在重复 data chunk", source, offset, "data_duplicate")
            data_offset = body
            data_size = chunk_size

        offset = body_end
        if chunk_size & 1:
            offset += 1
            if offset > len(raw):
                raise _wav_error(
                    "chunk 对齐填充字节缺失",
                    source,
                    body_end,
                    "chunk_padding",
                    chunk=chunk_id.decode("latin-1", errors="replace"),
                )

    if fmt is None:
        raise _wav_error("缺少 fmt chunk", source, 12, "fmt")
    if data_offset is None or data_size is None:
        raise _wav_error("缺少 data chunk", source, 12, "data")

    audio_format, channels, sample_rate, byte_rate, block_align, bits_per_sample = fmt
    if audio_format != _PCM:
        raise _wav_error(
            "仅支持 PCM WAV",
            source,
            20,
            "audio_format",
            declared=audio_format,
            expected=_PCM,
        )
    if channels not in _VALID_CHANNELS:
        raise _wav_error(
            "声道数非法，仅支持单声道或双声道",
            source,
            22,
            "channels",
            declared=channels,
            allowed=list(_VALID_CHANNELS),
        )
    if not (_MIN_RATE <= sample_rate <= _MAX_RATE):
        raise _wav_error(
            "采样率越界",
            source,
            24,
            "sample_rate",
            declared=sample_rate,
            minimum=_MIN_RATE,
            maximum=_MAX_RATE,
        )
    if bits_per_sample != _BITS_PER_SAMPLE:
        raise _wav_error(
            "仅支持 16-bit PCM",
            source,
            34,
            "bits_per_sample",
            declared=bits_per_sample,
            expected=_BITS_PER_SAMPLE,
        )

    expected_block_align = channels * (_BITS_PER_SAMPLE // 8)
    expected_byte_rate = sample_rate * expected_block_align
    if block_align != expected_block_align:
        raise _wav_error(
            "block_align 与声道/位深不一致",
            source,
            32,
            "block_align",
            declared=block_align,
            expected=expected_block_align,
        )
    if byte_rate != expected_byte_rate:
        raise _wav_error(
            "byte_rate 与采样率/block_align 不一致",
            source,
            28,
            "byte_rate",
            declared=byte_rate,
            expected=expected_byte_rate,
        )
    if data_size % expected_block_align != 0:
        raise _wav_error(
            "data 长度不是完整帧",
            source,
            data_offset - 4,
            "data_size",
            declared=data_size,
            block_align=expected_block_align,
        )

    pcm = np.frombuffer(raw, dtype="<i2", count=data_size // 2, offset=data_offset)
    frames = data_size // expected_block_align
    samples = pcm.reshape(frames, channels).astype(np.float64) / 32768.0
    return AudioData(
        samples=samples,
        sample_rate=sample_rate,
        source_path=os.fspath(source.resolve()),
        sha256=hashlib.sha256(raw).hexdigest(),
        byte_length=len(raw),
        data_offset=data_offset,
    )


def samples_to_pcm16(samples: np.ndarray) -> bytes:
    """Quantize internal float samples to little-endian PCM-16 deterministically."""

    arr = np.asarray(samples, dtype=np.float64)
    if arr.ndim == 1:
        arr = arr[:, None]
    if arr.ndim != 2:
        raise ValueError("samples must be a 2-D frames x channels array")
    quantized = np.rint(np.clip(arr, -1.0, 32767.0 / 32768.0) * 32768.0)
    quantized = np.clip(quantized, -32768, 32767).astype("<i2")
    return quantized.tobytes(order="C")


def encode_wav(samples: np.ndarray, sample_rate: int) -> bytes:
    arr = np.asarray(samples, dtype=np.float64)
    if arr.ndim != 2 or arr.shape[1] not in _VALID_CHANNELS:
        raise ValueError("WAV output must contain one or two channels")
    if not (_MIN_RATE <= sample_rate <= _MAX_RATE):
        raise ValueError("WAV output sample rate is outside the supported range")
    channels = int(arr.shape[1])
    pcm = samples_to_pcm16(arr)
    block_align = channels * 2
    byte_rate = sample_rate * block_align
    fmt = struct.pack(
        "<HHIIHH",
        _PCM,
        channels,
        sample_rate,
        byte_rate,
        block_align,
        _BITS_PER_SAMPLE,
    )
    chunks = b"fmt " + struct.pack("<I", len(fmt)) + fmt
    chunks += b"data" + struct.pack("<I", len(pcm)) + pcm
    return b"RIFF" + struct.pack("<I", 4 + len(chunks)) + b"WAVE" + chunks


def atomic_write_bytes(path: str | os.PathLike[str], data: bytes) -> None:
    """Write bytes beside the target, fsync, then atomically replace it."""

    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(
        prefix=f".{target.name}.",
        suffix=".tmp",
        dir=os.fspath(target.parent),
    )
    temp_path = Path(temp_name)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temp_path, target)
        _fsync_directory(target.parent)
    except BaseException:
        try:
            temp_path.unlink(missing_ok=True)
        finally:
            raise


def _fsync_directory(path: Path) -> None:
    # Directory fsync is meaningful on POSIX and harmless to skip on Windows.
    try:
        fd = os.open(os.fspath(path), os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    except OSError:
        pass
    finally:
        os.close(fd)
