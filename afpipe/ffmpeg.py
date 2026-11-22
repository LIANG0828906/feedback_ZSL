"""Local FFmpeg discovery, probing and deterministic decode/normalisation.

Decoding always goes through the locally installed ``ffmpeg`` binary
(PATH, ``FFMPEG``/``FFPROBE`` env vars, or an ``imageio-ffmpeg`` style
provider).  Samples are requested as little-endian signed 16-bit PCM and the
integer-to-float normalisation (``/32768``) is performed explicitly so the
behaviour is identical across containers and codecs.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
from dataclasses import dataclass

import numpy as np

from .errors import ChannelError, DecodeError, E_DECODE


@dataclass
class AudioData:
    samples: np.ndarray          # float32, shape (channels, samples)
    sample_rate: int
    channels: int
    duration_s: float
    source_path: str


def find_executables() -> tuple[str, str | None]:
    """Return (ffmpeg, ffprobe); ffprobe is optional."""
    ffmpeg = os.environ.get("FFMPEG") or shutil.which("ffmpeg")
    ffprobe = os.environ.get("FFPROBE") or shutil.which("ffprobe")
    if not ffmpeg:
        try:  # pip-distributable fallback, e.g. imageio-ffmpeg
            import imageio_ffmpeg  # type: ignore

            ffmpeg = imageio_ffmpeg.get_ffmpeg_exe()
        except Exception as exc:  # pragma: no cover - environment specific
            raise DecodeError(
                "no ffmpeg executable found (PATH, FFMPEG env or imageio-ffmpeg)",
                details={"reason": str(exc)},
            ) from exc
    return ffmpeg, ffprobe


def probe(path: str) -> dict:
    """Return declared stream metadata: sample_rate, channels, duration_s."""
    ffmpeg, ffprobe = find_executables()
    if ffprobe:
        try:
            proc = subprocess.run(
                [
                    ffprobe, "-v", "error", "-show_streams",
                    "-select_streams", "a:0", "-of", "json", path,
                ],
                capture_output=True, text=True, check=True,
            )
            info = json.loads(proc.stdout)["streams"][0]
            return {
                "sample_rate": int(info["sample_rate"]),
                "channels": int(info["channels"]),
                "duration_s": float(info.get("duration", 0.0)) or None,
                "codec": info.get("codec_name", "unknown"),
            }
        except (subprocess.CalledProcessError, KeyError, ValueError, json.JSONDecodeError):
            pass  # fall through to ffmpeg stderr parsing
    try:
        proc = subprocess.run(
            [ffmpeg, "-hide_banner", "-i", path],
            capture_output=True, text=True, check=False,
        )
        line = next(
            ln for ln in proc.stderr.splitlines() if "Audio:" in ln
        )
        parts = line.split(",")
        rate = int(next(p.strip().split()[0] for p in parts if "Hz" in p))
        chan_tok = next(p.strip() for p in parts if "channel" in p)
        channels = 1 if chan_tok.startswith("mono") else (
            2 if "stereo" in chan_tok else int(chan_tok.split()[0])
        )
        duration = None
        for token in line.split():
            if token.count(":") == 2:
                hh, mm, ss = token.split(":")
                duration = int(hh) * 3600 + int(mm) * 60 + float(ss)
                break
        return {
            "sample_rate": rate,
            "channels": channels,
            "duration_s": duration,
            "codec": "unknown",
        }
    except (StopIteration, ValueError, IndexError) as exc:
        raise DecodeError(
            f"unable to probe audio metadata: {os.path.basename(path)}",
            details={"path": path, "reason": str(exc)},
        ) from exc


def decode(
    path: str,
    *,
    target_rate: int,
    channel_mode: str,
    max_channels: int,
    decode_buf_frames: int,
) -> AudioData:
    """Decode any ffmpeg-readable file to normalised float32 audio.

    Resampling and downmixing are delegated to ffmpeg; the integer PCM it
    emits is converted to float in-process.  Reads are streamed in bounded
    chunks so per-thread buffer memory never exceeds ``decode_buf_frames``.
    """
    if not os.path.isfile(path):
        raise DecodeError("source file missing", details={"path": path})

    meta = probe(path)
    if meta["channels"] > max_channels:
        raise ChannelError(
            "channel count exceeds declared cap",
            details={
                "path": path,
                "actual_channels": meta["channels"],
                "max_channels": max_channels,
            },
        )

    ffmpeg, _ = find_executables()
    out_channels = 1 if channel_mode == "mono" else meta["channels"]
    cmd = [
        ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
        "-i", path,
        "-vn",
        "-ar", str(target_rate),
        "-f", "s16le",
        "-acodec", "pcm_s16le",
        "-ac", str(out_channels),
        "pipe:1",
    ]
    proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    assert proc.stdout is not None

    chunk_bytes = decode_buf_frames * out_channels * 2
    chunks: list[np.ndarray] = []
    total = 0
    try:
        while True:
            block = proc.stdout.read(chunk_bytes)
            if not block:
                break
            arr = np.frombuffer(block, dtype="<i2").astype(np.float32) / 32768.0
            chunks.append(arr)
            total += arr.size
            if total > decode_buf_frames * out_channels * 256:
                # Hard ceiling on accumulated samples for a single file.
                proc.kill()
                raise DecodeError(
                    "decoded stream exceeded per-file sample ceiling",
                    details={"path": path, "samples": total},
                )
    finally:
        stderr = proc.stderr.read() if proc.stderr else b""
        if proc.stdout is not None:
            proc.stdout.close()
        if proc.stderr is not None:
            proc.stderr.close()
        proc.wait()

    if proc.returncode != 0:
        raise DecodeError(
            f"ffmpeg decode failed (rc={proc.returncode})",
            details={"path": path, "stderr": stderr.decode("utf-8", "replace")[:500],
                     "code": E_DECODE},
        )
    if not chunks:
        raise DecodeError("decoder produced no samples", details={"path": path})

    flat = np.concatenate(chunks)
    usable = (flat.size // out_channels) * out_channels
    flat = flat[:usable]
    samples = flat.reshape(-1, out_channels).T.copy()
    duration_s = samples.shape[1] / target_rate
    return AudioData(
        samples=samples,
        sample_rate=target_rate,
        channels=out_channels,
        duration_s=duration_s,
        source_path=os.path.abspath(path),
    )
