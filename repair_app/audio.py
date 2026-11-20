from __future__ import annotations

import hashlib
import io
import os
import struct
import tempfile
import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .constants import (
    MAX_SAMPLE_RATE,
    MIN_SAMPLE_RATE,
    SUPPORTED_CHANNELS,
    WAV_HEADER_SIZE,
)
from .errors import AppError
from .models import validate_sample_rate


@dataclass(frozen=True)
class WavInfo:
    sample_rate: int
    channels: int
    frames: int
    sample_width: int = 2

    @property
    def data_size(self) -> int:
        return self.frames * self.channels * self.sample_width

    @property
    def file_size(self) -> int:
        return WAV_HEADER_SIZE + self.data_size


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: str | Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _validate_info(info: WavInfo, *, source: str) -> WavInfo:
    if info.channels not in SUPPORTED_CHANNELS:
        raise AppError(
            "E_INVALID_INPUT",
            f"{source}: 仅支持单声道或双声道",
            channels=info.channels,
        )
    if not MIN_SAMPLE_RATE <= info.sample_rate <= MAX_SAMPLE_RATE:
        raise AppError(
            "E_INVALID_INPUT",
            f"{source}: 采样率必须在 {MIN_SAMPLE_RATE}..{MAX_SAMPLE_RATE} Hz",
            sample_rate=info.sample_rate,
        )
    if info.sample_width != 2:
        raise AppError(
            "E_INVALID_INPUT",
            f"{source}: 仅支持 16-bit PCM",
            sample_width=info.sample_width,
        )
    if info.frames < 0:
        raise AppError("E_INVALID_INPUT", f"{source}: 帧数非法")
    return info


def inspect_wav(path: str | Path) -> WavInfo:
    source = os.fspath(path)
    try:
        with wave.open(source, "rb") as handle:
            if handle.getcomptype() != "NONE":
                raise AppError("E_INVALID_INPUT", "仅支持未压缩 PCM WAV")
            info = WavInfo(
                sample_rate=handle.getframerate(),
                channels=handle.getnchannels(),
                frames=handle.getnframes(),
                sample_width=handle.getsampwidth(),
            )
    except AppError:
        raise
    except (OSError, EOFError, wave.Error) as exc:
        raise AppError("E_INVALID_INPUT", f"无法读取 WAV: {source}", reason=str(exc)) from exc
    return _validate_info(info, source=source)


def read_wav(path: str | Path) -> tuple[np.ndarray, WavInfo]:
    source = os.fspath(path)
    try:
        with wave.open(source, "rb") as handle:
            comptype = handle.getcomptype()
            info = WavInfo(
                sample_rate=handle.getframerate(),
                channels=handle.getnchannels(),
                frames=handle.getnframes(),
                sample_width=handle.getsampwidth(),
            )
            _validate_info(info, source=source)
            if comptype != "NONE":
                raise AppError("E_INVALID_INPUT", "仅支持未压缩 PCM WAV")
            payload = handle.readframes(info.frames)
    except AppError:
        raise
    except (OSError, EOFError, wave.Error) as exc:
        raise AppError("E_INVALID_INPUT", f"无法读取 WAV: {source}", reason=str(exc)) from exc

    expected = info.frames * info.channels * 2
    if len(payload) != expected:
        raise AppError(
            "E_INVALID_INPUT",
            "WAV 数据被截断",
            expected_bytes=expected,
            actual_bytes=len(payload),
        )
    samples = np.frombuffer(payload, dtype="<i2").astype(np.int16, copy=True)
    samples = samples.reshape(info.frames, info.channels)
    return samples, info


def _fmt_chunk(sample_rate: int, channels: int) -> bytes:
    block_align = channels * 2
    byte_rate = sample_rate * block_align
    return struct.pack(
        "<4sIHHIIHH",
        b"fmt ",
        16,
        1,
        channels,
        sample_rate,
        byte_rate,
        block_align,
        16,
    )


def wav_header(frames: int, sample_rate: int, channels: int) -> bytes:
    validate_sample_rate(sample_rate)
    if channels not in SUPPORTED_CHANNELS:
        raise AppError("E_INVALID_ARGUMENT", "仅支持 1 或 2 声道")
    if frames < 0:
        raise AppError("E_INVALID_ARGUMENT", "帧数不能为负数")
    data_size = frames * channels * 2
    if data_size > 0xFFFFFFFF - 36:
        raise AppError("E_INVALID_ARGUMENT", "WAV 文件超过 RIFF 32-bit 大小上限")
    riff_size = 36 + data_size
    header = io.BytesIO()
    header.write(struct.pack("<4sI4s", b"RIFF", riff_size, b"WAVE"))
    header.write(_fmt_chunk(sample_rate, channels))
    header.write(struct.pack("<4sI", b"data", data_size))
    payload = header.getvalue()
    assert len(payload) == WAV_HEADER_SIZE
    return payload


def _atomic_write_bytes(path: str | Path, payload: bytes) -> None:
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary_name = tempfile.mkstemp(
        prefix=f".{destination.name}.", suffix=".tmp", dir=str(destination.parent)
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_name, destination)
        _fsync_directory(destination.parent)
    except BaseException:
        try:
            os.unlink(temporary_name)
        except FileNotFoundError:
            pass
        raise


def atomic_write_bytes(path: str | Path, payload: bytes) -> None:
    _atomic_write_bytes(path, payload)


def _fsync_directory(directory: Path) -> None:
    if os.name == "nt":
        return
    try:
        fd = os.open(directory, os.O_RDONLY)
    except OSError:
        return
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def write_wav_atomic(
    path: str | Path,
    samples: np.ndarray,
    sample_rate: int,
    *,
    expected_channels: int | None = None,
) -> None:
    array = np.asarray(samples)
    if array.dtype != np.int16 or array.ndim != 2:
        raise AppError("E_INVALID_ARGUMENT", "写入样本必须为二维 int16 数组")
    frames, channels = array.shape
    if expected_channels is not None and channels != expected_channels:
        raise AppError("E_INVALID_ARGUMENT", "声道数与预期不一致")
    payload = wav_header(frames, sample_rate, channels) + array.astype(
        "<i2", copy=False
    ).tobytes(order="C")
    _atomic_write_bytes(path, payload)


def create_wav_skeleton_atomic(
    path: str | Path, frames: int, sample_rate: int, channels: int
) -> None:
    header = wav_header(frames, sample_rate, channels)
    destination = Path(path)
    destination.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary_name = tempfile.mkstemp(
        prefix=f".{destination.name}.", suffix=".tmp", dir=str(destination.parent)
    )
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(header)
            handle.truncate(WAV_HEADER_SIZE + frames * channels * 2)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(temporary_name, destination)
        _fsync_directory(destination.parent)
    except BaseException:
        try:
            os.unlink(temporary_name)
        except FileNotFoundError:
            pass
        raise


def read_wav_header(path: str | Path) -> WavInfo:
    source = os.fspath(path)
    try:
        with open(source, "rb") as handle:
            header = handle.read(WAV_HEADER_SIZE)
    except OSError as exc:
        raise AppError("E_RECOVERY_INVALID", f"输出 WAV 不存在或不可读: {source}") from exc
    if len(header) != WAV_HEADER_SIZE:
        raise AppError("E_RECOVERY_INVALID", "输出 WAV 头部被截断", path=source)
    try:
        riff, riff_size, wave_name, fmt, fmt_size = struct.unpack("<4sI4s4sI", header[:20])
        (
            audio_format,
            channels,
            sample_rate,
            byte_rate,
            block_align,
            bits,
        ) = struct.unpack("<HHIIHH", header[20:36])
        data_tag, data_size = struct.unpack("<4sI", header[36:44])
    except struct.error as exc:
        raise AppError("E_RECOVERY_INVALID", "输出 WAV 头部格式非法", path=source) from exc
    expected_block_align = channels * 2
    if (
        riff != b"RIFF"
        or wave_name != b"WAVE"
        or fmt != b"fmt "
        or fmt_size != 16
        or audio_format != 1
        or data_tag != b"data"
        or bits != 16
        or channels not in SUPPORTED_CHANNELS
        or block_align != expected_block_align
        or byte_rate != sample_rate * expected_block_align
        or riff_size != 36 + data_size
        or data_size % expected_block_align != 0
    ):
        raise AppError("E_RECOVERY_INVALID", "输出 WAV 头部字段不一致", path=source)
    info = WavInfo(
        sample_rate=sample_rate,
        channels=channels,
        frames=data_size // expected_block_align,
        sample_width=2,
    )
    try:
        _validate_info(info, source=source)
    except AppError as exc:
        raise AppError(
            "E_RECOVERY_INVALID",
            "输出 WAV 参数非法",
            path=source,
            reason=exc.message,
        ) from exc
    return info


def validate_output_file(path: str | Path, expected: WavInfo) -> None:
    info = read_wav_header(path)
    if info != expected:
        raise AppError(
            "E_RECOVERY_INVALID",
            "输出 WAV 头部与会话不一致",
            expected=expected,
            actual=info,
        )
    try:
        actual_size = os.path.getsize(path)
    except OSError as exc:
        raise AppError("E_RECOVERY_INVALID", "无法读取输出 WAV 大小") from exc
    if actual_size != expected.file_size:
        raise AppError(
            "E_RECOVERY_INVALID",
            "输出 WAV 被截断或包含多余数据",
            expected_bytes=expected.file_size,
            actual_bytes=actual_size,
        )


def read_output_block(path: str | Path, info: WavInfo, block_index: int, block_size: int) -> bytes:
    start = block_index * block_size
    frames = min(block_size, info.frames - start)
    payload_bytes = frames * info.channels * 2
    offset = WAV_HEADER_SIZE + start * info.channels * 2
    try:
        with open(path, "rb") as handle:
            handle.seek(offset)
            payload = handle.read(payload_bytes)
    except OSError as exc:
        raise AppError("E_RECOVERY_INVALID", "无法读取已有输出块") from exc
    if len(payload) != payload_bytes:
        raise AppError("E_RECOVERY_INVALID", "已有输出块被截断", block_index=block_index)
    return payload



