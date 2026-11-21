from __future__ import annotations

import array
import bisect
import struct
import sys
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import BinaryIO

from .errors import BusinessError, timebase_error
from .models import LayoutParams, SubtitleAsset


SAMPLE_RATE = 48_000
TICKS_PER_SECOND = 6_000_000
TICKS_PER_SAMPLE = TICKS_PER_SECOND // SAMPLE_RATE  # 125
TICKS_PER_MICROSECOND = TICKS_PER_SECOND // 1_000_000  # 6
FRAME_MAGIC = b"FTBL0001"
FRAME_HEADER_SIZE = 28
FRAME_RECORD = struct.Struct("<Q")


@dataclass(frozen=True)
class WavInfo:
    path: Path
    channels: int
    sample_rate: int
    bits_per_sample: int
    block_align: int
    data_offset: int
    data_size: int
    frame_count: int
    channel_mask: int = 0


@dataclass(frozen=True)
class FrameTable:
    path: Path
    version: int
    frame_rate: int
    timebase_num: int
    flags: int
    positions: tuple[int, ...]

    @property
    def frame_count(self) -> int:
        return len(self.positions)

    def frame_for_sample(self, sample: int) -> int:
        if sample < 0:
            return 0
        index = bisect.bisect_right(self.positions, sample) - 1
        return max(0, min(index, len(self.positions) - 1))

    def frame_interval(self, start_sample: int, end_sample: int) -> tuple[int, int]:
        return self.frame_for_sample(start_sample), self.frame_for_sample(end_sample)


def _read_exact(handle: BinaryIO, size: int) -> bytes:
    data = handle.read(size)
    if len(data) != size:
        raise BusinessError("E_INVALID_INPUT", "truncated WAV file", {"expected": size, "actual": len(data)})
    return data


def read_wav_info(path: Path | str) -> WavInfo:
    """Read PCM WAV metadata without loading the sample payload.

    Both plain PCM and WAVE_FORMAT_EXTENSIBLE with a PCM sub-format are
    accepted.  The latter is common for six-channel 5.1 material.
    """

    path = Path(path)
    try:
        with path.open("rb") as handle:
            header = _read_exact(handle, 12)
            if header[:4] != b"RIFF" or header[8:12] != b"WAVE":
                raise timebase_error("not a RIFF/WAVE file", field="wav.format", value=str(path))
            fmt: tuple[int, int, int, int, int] | None = None
            data_offset = -1
            data_size = 0
            channel_mask = 0
            while True:
                chunk_header = handle.read(8)
                if not chunk_header:
                    break
                if len(chunk_header) != 8:
                    raise timebase_error("truncated WAV chunk", field="wav.chunk")
                chunk_id, chunk_size = struct.unpack("<4sI", chunk_header)
                payload_offset = handle.tell()
                if chunk_id == b"fmt ":
                    payload = _read_exact(handle, chunk_size)
                    if len(payload) < 16:
                        raise timebase_error("truncated fmt chunk", field="wav.fmt")
                    audio_format, channels, rate, _byte_rate, block_align, bits = struct.unpack_from(
                        "<HHIIHH", payload, 0
                    )
                    if audio_format == 0xFFFE:
                        if len(payload) < 40:
                            raise timebase_error("truncated extensible fmt chunk", field="wav.fmt")
                        channel_mask = struct.unpack_from("<I", payload, 20)[0]
                        valid_bits = struct.unpack_from("<H", payload, 18)[0]
                        subformat = struct.unpack_from("<H", payload, 24)[0]
                        bits = valid_bits or bits
                        audio_format = subformat
                    fmt = (audio_format, channels, rate, block_align, bits)
                elif chunk_id == b"data":
                    data_offset = payload_offset
                    data_size = chunk_size
                    handle.seek(chunk_size, 1)
                else:
                    handle.seek(chunk_size, 1)
                if chunk_size % 2:
                    handle.seek(1, 1)
            if fmt is None:
                raise timebase_error("missing fmt chunk", field="wav.fmt")
            if data_offset < 0:
                raise timebase_error("missing data chunk", field="wav.data")
    except FileNotFoundError:
        raise timebase_error("WAV file does not exist", field="wav.path", value=str(path)) from None

    audio_format, channels, rate, block_align, bits = fmt
    if audio_format != 1:
        raise timebase_error("WAV must contain integer PCM", field="wav.audio_format", value=audio_format)
    if channels not in (1, 6):
        raise timebase_error("WAV must be mono or 5.1", field="wav.channels", value=channels)
    if rate != SAMPLE_RATE:
        raise timebase_error(
            "WAV sample rate must be 48000 Hz",
            field="wav.sample_rate",
            value=rate,
        )
    if bits != 16:
        raise timebase_error("WAV must be 16-bit PCM", field="wav.bits_per_sample", value=bits)
    expected_align = channels * 2
    if block_align != expected_align:
        raise timebase_error(
            "WAV block alignment is inconsistent with sample rate/channels",
            field="wav.block_align",
            value=block_align,
        )
    if data_size <= 0 or data_size % block_align:
        raise timebase_error("WAV data size is invalid", field="wav.data_size", value=data_size)
    return WavInfo(
        path=path,
        channels=channels,
        sample_rate=rate,
        bits_per_sample=bits,
        block_align=block_align,
        data_offset=data_offset,
        data_size=data_size,
        frame_count=data_size // block_align,
        channel_mask=channel_mask,
    )


def read_frame_table(path: Path | str) -> FrameTable:
    path = Path(path)
    try:
        data = path.read_bytes()
    except FileNotFoundError:
        raise timebase_error("frame table does not exist", field="frames.path", value=str(path)) from None
    if len(data) < FRAME_HEADER_SIZE or data[:8] != FRAME_MAGIC:
        raise timebase_error("invalid frame table header", field="frames.magic")
    version, frame_rate, timebase_num, frame_count, flags = struct.unpack_from("<IIIII", data, 8)
    if version != 1:
        raise timebase_error("unsupported frame table version", field="frames.version", value=version)
    if frame_rate <= 0 or frame_rate > 1000:
        raise timebase_error("invalid frame rate", field="frames.frame_rate", value=frame_rate)
    if timebase_num <= 0:
        raise timebase_error("invalid frame timebase", field="frames.timebase_num", value=timebase_num)
    expected = FRAME_HEADER_SIZE + frame_count * FRAME_RECORD.size
    if expected != len(data):
        raise timebase_error(
            "frame table length does not match frame count",
            field="frames.frame_count",
            value=frame_count,
            expected_size=expected,
            actual_size=len(data),
        )
    positions = tuple(
        FRAME_RECORD.unpack_from(data, FRAME_HEADER_SIZE + index * FRAME_RECORD.size)[0]
        for index in range(frame_count)
    )
    if not positions:
        raise timebase_error("empty frame table", field="frames.frame_count", value=0)
    if positions[0] != 0:
        raise timebase_error("first frame must start at sample zero", field="frames.positions", value=positions[0])
    last = positions[0]
    for index, position in enumerate(positions[1:], 1):
        if position <= last:
            raise timebase_error(
                "frame positions must be strictly increasing",
                field="frames.positions",
                value={"index": index, "sample": position},
            )
        last = position
    return FrameTable(
        path=path,
        version=version,
        frame_rate=frame_rate,
        timebase_num=timebase_num,
        flags=flags,
        positions=positions,
    )


def read_channel_samples(info: WavInfo, channel: int, start_sample: int = 0,
                         count: int | None = None) -> array.array:
    if channel < 0 or channel >= info.channels:
        raise timebase_error(
            "channel index is outside the WAV channel layout",
            field="parameters.channel",
            value=channel,
        )
    if start_sample < 0 or start_sample > info.frame_count:
        raise timebase_error(
            "sample start is outside the WAV",
            field="wav.start_sample",
            value=start_sample,
        )
    if count is None:
        count = info.frame_count - start_sample
    count = max(0, min(count, info.frame_count - start_sample))
    byte_offset = info.data_offset + start_sample * info.block_align + channel * 2
    with info.path.open("rb") as handle:
        handle.seek(byte_offset)
        raw = handle.read(count * 2)
    values = array.array("h")
    values.frombytes(raw)
    if sys.byteorder != "little":
        values.byteswap()
    return values


@dataclass(frozen=True)
class SentenceTiming:
    sentence_id: str
    start_ticks: int
    end_ticks: int
    orig_start_sample: int
    orig_end_sample: int
    frame_start: int
    frame_end: int
    start_timecode: str
    end_timecode: str


def parse_timecode(value: str, *, sentence_id: str | None = None, field: str = "timecode") -> int:
    if not isinstance(value, str):
        raise timebase_error("timecode must be a string", sentence_id=sentence_id, field=field, value=value)
    text = value.strip().replace(",", ".")
    pieces = text.split(":")
    if len(pieces) != 3:
        raise timebase_error("timecode must be HH:MM:SS.ffffff", sentence_id=sentence_id, field=field, value=value)
    try:
        hours, minutes, seconds_text = int(pieces[0]), int(pieces[1]), pieces[2]
        if hours < 0 or minutes < 0 or minutes >= 60 or not seconds_text:
            raise ValueError
        if "." in seconds_text:
            whole_text, fraction_text = seconds_text.split(".", 1)
        else:
            whole_text, fraction_text = seconds_text, ""
        if not whole_text.isdigit() or (fraction_text and not fraction_text.isdigit()):
            raise ValueError
        seconds = int(whole_text)
        if seconds >= 60:
            raise ValueError
        if len(fraction_text) > 6:
            # Six decimal places are the declared local timecode precision.
            raise ValueError
        microseconds = int(fraction_text.ljust(6, "0")) if fraction_text else 0
    except ValueError:
        raise timebase_error("invalid timecode", sentence_id=sentence_id, field=field, value=value) from None
    total_microseconds = ((hours * 60 + minutes) * 60 + seconds) * 1_000_000 + microseconds
    return total_microseconds * TICKS_PER_MICROSECOND


def format_timecode(ticks: int) -> str:
    if ticks < 0:
        sign = "-"
        ticks = -ticks
    else:
        sign = ""
    total_microseconds = ticks // TICKS_PER_MICROSECOND
    microseconds = total_microseconds % 1_000_000
    total_seconds = total_microseconds // 1_000_000
    seconds = total_seconds % 60
    total_minutes = total_seconds // 60
    minutes = total_minutes % 60
    hours = total_minutes // 60
    return f"{sign}{hours:02d}:{minutes:02d}:{seconds:02d}.{microseconds:06d}"


def ticks_to_samples(ticks: int) -> int:
    if ticks < 0:
        return -((-ticks + TICKS_PER_SAMPLE // 2) // TICKS_PER_SAMPLE)
    return (ticks + TICKS_PER_SAMPLE // 2) // TICKS_PER_SAMPLE


def samples_to_ticks(samples: int) -> int:
    return samples * TICKS_PER_SAMPLE


def ms_to_samples(milliseconds: int) -> int:
    if milliseconds < 0:
        raise timebase_error("duration cannot be negative", field="duration_ms", value=milliseconds)
    return milliseconds * SAMPLE_RATE // 1000


def validate_media_and_asset(
    asset: SubtitleAsset,
    wav: WavInfo,
    frames: FrameTable,
    params: LayoutParams | None = None,
) -> list[SentenceTiming]:
    params = params or asset.parameters
    if params.min_duration_ms < 0:
        raise timebase_error(
            "minimum subtitle duration cannot be negative",
            field="parameters.min_duration_ms",
            value=params.min_duration_ms,
        )
    if params.min_gap_ms < 0:
        raise timebase_error(
            "minimum subtitle gap cannot be negative",
            field="parameters.min_gap_ms",
            value=params.min_gap_ms,
        )
    try:
        default_speed = Fraction(params.default_cps)
    except (ValueError, ZeroDivisionError):
        raise timebase_error(
            "default reading speed is invalid",
            field="parameters.default_cps",
            value=params.default_cps,
        ) from None
    if default_speed <= 0:
        raise timebase_error(
            "default reading speed must be positive",
            field="parameters.default_cps",
            value=params.default_cps,
        )
    if frames.positions[-1] > wav.frame_count:
        raise timebase_error(
            "frame table extends beyond the audio",
            field="frames.positions",
            value=frames.positions[-1],
            audio_samples=wav.frame_count,
        )
    if wav.frame_count <= 0:
        raise timebase_error("audio has no samples", field="wav.frame_count", value=wav.frame_count)
    if not asset.sentences:
        raise timebase_error("asset has no sentences", field="sentences", value=0)
    ids: set[str] = set()
    timings: list[SentenceTiming] = []
    for sentence in asset.sentences:
        if sentence.id in ids:
            raise timebase_error(
                "duplicate sentence id",
                sentence_id=sentence.id,
                field="id",
                value=sentence.id,
            )
        ids.add(sentence.id)
        if sentence.cps is not None:
            try:
                sentence_speed = Fraction(sentence.cps)
            except (ValueError, ZeroDivisionError):
                raise timebase_error(
                    "sentence reading speed is invalid",
                    sentence_id=sentence.id,
                    field="cps",
                    value=sentence.cps,
                ) from None
            if sentence_speed <= 0:
                raise timebase_error(
                    "sentence reading speed must be positive",
                    sentence_id=sentence.id,
                    field="cps",
                    value=sentence.cps,
                )
        start_ticks = parse_timecode(sentence.start, sentence_id=sentence.id, field="start")
        end_ticks = parse_timecode(sentence.end, sentence_id=sentence.id, field="end")
        if end_ticks <= start_ticks:
            raise timebase_error(
                "sentence end must be after start",
                sentence_id=sentence.id,
                field="end",
                value=sentence.end,
            )
        start_sample = ticks_to_samples(start_ticks)
        end_sample = ticks_to_samples(end_ticks)
        if start_sample > wav.frame_count:
            raise timebase_error(
                "sentence start is outside the audio",
                sentence_id=sentence.id,
                field="start",
                value=sentence.start,
                audio_samples=wav.frame_count,
            )
        if end_sample > wav.frame_count:
            raise timebase_error(
                "sentence end is outside the audio",
                sentence_id=sentence.id,
                field="end",
                value=sentence.end,
                audio_samples=wav.frame_count,
            )
        if end_sample <= start_sample:
            raise timebase_error(
                "sentence is shorter than one audio sample",
                sentence_id=sentence.id,
                field="end",
                value=sentence.end,
            )
        frame_start, frame_end = frames.frame_interval(start_sample, end_sample)
        timings.append(
            SentenceTiming(
                sentence_id=sentence.id,
                start_ticks=start_ticks,
                end_ticks=end_ticks,
                orig_start_sample=start_sample,
                orig_end_sample=end_sample,
                frame_start=frame_start,
                frame_end=frame_end,
                start_timecode=format_timecode(start_ticks),
                end_timecode=format_timecode(end_ticks),
            )
        )
    description_ids: set[str] = set()
    for description in asset.descriptions:
        if description.id in description_ids:
            raise BusinessError(
                "E_INVALID_INPUT",
                "duplicate description id",
                {"description_id": description.id, "field": "id"},
            )
        description_ids.add(description.id)
        try:
            description_speed = Fraction(description.max_cps)
        except (ValueError, ZeroDivisionError):
            raise timebase_error(
                "description reading speed is invalid",
                field="max_cps",
                value=description.max_cps,
                description_id=description.id,
            ) from None
        if description_speed <= 0:
            raise timebase_error(
                "description reading speed must be positive",
                field="max_cps",
                value=description.max_cps,
                description_id=description.id,
            )
        if description.after_sentence_id not in ids:
            raise BusinessError(
                "E_INVALID_INPUT",
                "description anchor sentence does not exist",
                {"description_id": description.id, "sentence_id": description.after_sentence_id},
            )
    _validate_channel_map(params, wav)
    return timings


def _validate_channel_map(params: LayoutParams, wav: WavInfo) -> None:
    channels = [params.default_channel, *params.channel_map.values()]
    for channel in channels:
        if channel < 0 or channel >= wav.channels:
            raise timebase_error(

                "channel mapping is outside the WAV channel layout",
                field="parameters.channel_map",
                value=channel,
                wav_channels=wav.channels,
            )


def load_media(asset: SubtitleAsset, wav_path: Path | str, frames_path: Path | str) -> tuple[WavInfo, FrameTable, list[SentenceTiming]]:
    wav = read_wav_info(wav_path)
    frames = read_frame_table(frames_path)
    timings = validate_media_and_asset(asset, wav, frames)
    return wav, frames, timings

