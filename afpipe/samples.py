"""Deterministic local sample generator.

Renders synthetic game/media material with a fixed seed:

* long "master" clips built by concatenating reusable short phrases with
  audible tone markers at every phrase boundary (ground-truth timestamps);
* the same phrases rendered as standalone query clips;
* every clip emitted as WAV PCM (16-bit) plus an FFmpeg-transcoded lossy
  version (MP3, and OGG/Vorbis for one group);
* a manifest declaring sample rate, channels, duration, per-file SHA-256 and
  the tone-marker ground truth.

No network access is required beyond the local ffmpeg binary.
"""

from __future__ import annotations

import os
import subprocess
import wave
from dataclasses import dataclass, field

import numpy as np

from .ffmpeg import find_executables, probe
from .hashing import atomic_write_bytes, canonical_bytes, file_sha256
from .manifest import MANIFEST_SCHEMA_VERSION

DEFAULT_SEED = 20261123


@dataclass
class Segment:
    seg_id: str
    kind: str                 # "phrase" | "pad"
    samples: np.ndarray
    markers: list[dict] = field(default_factory=list)


def _tone(freq: float, n: int, sr: int, amp: float = 0.22) -> np.ndarray:
    t = np.arange(n) / sr
    attack = np.linspace(0, 1, max(8, n // 8))
    release = np.linspace(1, 0, max(8, n // 8))
    sustain = np.ones(max(0, n - attack.size - release.size))
    env = np.concatenate([attack, sustain, release])[:n]
    return (amp * env * np.sin(2 * np.pi * freq * t)).astype(np.float32)


def _phrase(seed_seq: np.random.SeedSequence, n: int, sr: int,
            marker_freq: float) -> tuple[np.ndarray, list[dict]]:
    rng = np.random.default_rng(seed_seq)
    t = np.arange(n) / sr
    base = np.zeros(n, dtype=np.float32)
    # Three detuned harmonics with slow vibrato -> stable spectral shape.
    for harmonic, amp in ((1, 0.28), (2, 0.14), (3, 0.07)):
        vibrato = 1.0 + 0.004 * np.sin(2 * np.pi * (3.0 + rng.uniform(0, 2)) * t)
        freq = rng.uniform(180, 340) * harmonic
        base += amp * np.sin(2 * np.pi * freq * t * vibrato)
    # Filtered noise texture, convolved with a fixed deterministic kernel.
    noise = rng.normal(0, 0.05, n).astype(np.float32)
    kernel = np.exp(-np.linspace(0, 4, 32)).astype(np.float32)
    kernel /= kernel.sum()
    base += np.convolve(noise, kernel, mode="same")
    # Amplitude swell, deterministic.
    base *= 0.6 + 0.4 * np.sin(np.pi * np.linspace(0, 1, n)) ** 2
    # Embedded tone marker with declared start/end (ground truth).
    m_start = int(sr * 0.05)
    m_len = int(sr * 0.09)
    marker = _tone(marker_freq, m_len, sr)
    base[m_start:m_start + m_len] += marker
    markers = [{
        "name": "phrase_head",
        "freq_hz": round(float(marker_freq), 2),
        "start_s": round(m_start / sr, 6),
        "end_s": round((m_start + m_len) / sr, 6),
    }]
    return base.astype(np.float32), markers


def _pad(seed_seq: np.random.SeedSequence, n: int, sr: int) -> np.ndarray:
    rng = np.random.default_rng(seed_seq)
    t = np.arange(n) / sr
    pad = 0.06 * np.sin(2 * np.pi * rng.uniform(70, 120) * t)
    pad += rng.normal(0, 0.015, n)
    return pad.astype(np.float32)


def _write_wav(path: str, audio: np.ndarray, sr: int, channels: int) -> None:
    if audio.ndim == 1:
        audio = np.stack([audio] * channels, axis=1)
    pcm16 = np.clip(audio, -1.0, 1.0)
    pcm16 = (pcm16 * 32767.0).round().astype("<i2")
    with wave.open(path, "wb") as wf:
        wf.setnchannels(channels)
        wf.setsampwidth(2)
        wf.setframerate(sr)
        wf.writeframes(pcm16.tobytes())


def _transcode(ffmpeg: str, src: str, dst: str, codec: str, *,
               sr: int, channels: int) -> None:
    if codec == "mp3":
        args = ["-c:a", "libmp3lame", "-b:a", "64k"]
    elif codec == "ogg":
        args = ["-c:a", "libvorbis", "-qscale:a", "4"]
    else:
        raise ValueError(codec)
    cmd = [ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
           "-y", "-i", src, *args, "-ar", str(sr), "-ac", str(channels), dst]
    subprocess.run(cmd, check=True, capture_output=True)


def _file_entry(path: str, container: str, codec: str, kind: str,
                sr: int, channels: int, nominal_s: float,
                markers: list[dict], file_id: str) -> dict:
    return {
        "file_id": file_id,
        "path": path.replace(os.sep, "/"),
        "container": container,
        "codec": codec,
        "format_kind": kind,
        "sample_rate": sr,
        "channels": channels,
        "duration_s": round(nominal_s, 6),
        "duration_probed_s": round(probe(path)["duration_s"] or nominal_s, 6),
        "sha256": file_sha256(path),
        "markers": markers,
    }


def generate(out_dir: str, *, seed: int = DEFAULT_SEED) -> dict:
    """Generate the full deterministic corpus and write the manifest."""
    ffmpeg, _ = find_executables()
    root = os.path.abspath(out_dir)
    audio_dir = os.path.join(root, "audio")
    os.makedirs(audio_dir, exist_ok=True)
    root_seq = np.random.SeedSequence(seed)

    # Group C exercises resampling + downmix (stereo, 22050 Hz).
    group_specs = [
        ("gA", 16000, 1, ("mp3", "ogg")),
        ("gB", 16000, 1, ("mp3",)),
        ("gC", 22050, 2, ("mp3",)),
    ]
    phrase_lens = (2.0, 1.6)
    marker_freqs = {"gA": 880.0, "gB": 660.0, "gC": 990.0}

    manifest: dict = {
        "schema_version": MANIFEST_SCHEMA_VERSION,
        "seed": seed,
        "sample_rate": 16000,          # pipeline target, per-file sr inside
        "channels": 1,
        "groups": {},
        "queries": {},
    }

    seqs = root_seq.spawn(len(group_specs))
    for (gid, sr, channels, lossy), gseq in zip(group_specs, seqs):
        sub = gseq.spawn(6)
        intro = _pad(sub[0], int(sr * 1.5), sr)
        bridge = _pad(sub[1], int(sr * 1.0), sr)
        outro = _pad(sub[2], int(sr * 1.2), sr)
        phrases: list[tuple[np.ndarray, list[dict]]] = []
        for i, plen in enumerate(phrase_lens):
            phrases.append(_phrase(
                sub[3 + i], int(sr * plen), sr,
                marker_freqs[gid] + i * 110,
            ))

        # Assemble the long master and record absolute ground-truth offsets.
        parts: list[np.ndarray] = [intro]
        master_markers: list[dict] = []
        cursor = intro.size / sr
        phrase_specs: list[dict] = []
        for i, (phrase, markers) in enumerate(phrases):
            seg_id = f"{gid}_p{i + 1}"
            for mk in markers:
                master_markers.append({
                    **mk,
                    "segment": seg_id,
                    "start_s": round(cursor + mk["start_s"], 6),
                    "end_s": round(cursor + mk["end_s"], 6),
                })
            phrase_specs.append({
                "segment_id": seg_id,
                "start_s": round(cursor, 6),
                "end_s": round(cursor + phrase.size / sr, 6),
            })
            parts.append(phrase)
            cursor += phrase.size / sr
            if i == 0:
                parts.append(bridge)
                cursor += bridge.size / sr
        parts.append(outro)
        master = np.concatenate(parts).astype(np.float32)
        nominal = master.size / sr

        base = os.path.join(audio_dir, f"{gid}_long")
        wav_rel = f"audio/{gid}_long.wav"
        _write_wav(base + ".wav", master, sr, channels)
        files = [_file_entry(
            os.path.join(audio_dir, f"{gid}_long.wav"),
            "wav", "pcm_s16le", "pcm", sr, channels, nominal,
            master_markers, f"{gid}_long_wav",
        )]
        for codec in lossy:
            ext = "mp3" if codec == "mp3" else "ogg"
            dst = f"{base}.{ext}"
            _transcode(ffmpeg, base + ".wav", dst, codec,
                       sr=sr, channels=channels)
            files.append(_file_entry(
                dst, ext, f"lib{codec == 'mp3' and 'mp3lame' or 'vorbis'}",
                "lossy", sr, channels, nominal, master_markers,
                f"{gid}_long_{ext}",
            ))
        manifest["groups"][gid] = {
            "group_id": gid,
            "physical_key": f"phys-{gid}-long",
            "sample_rate": sr,
            "channels": channels,
            "segments": phrase_specs,
            "files": files,
        }

        # Standalone query clips = the exact phrase signals.
        for i, (phrase, markers) in enumerate(phrases):
            qid = f"q_{gid}_p{i + 1}"
            qbase = os.path.join(audio_dir, qid)
            _write_wav(qbase + ".wav", phrase, sr, channels)
            qfiles = [_file_entry(
                qbase + ".wav", "wav", "pcm_s16le", "pcm",
                sr, channels, phrase.size / sr, markers, f"{qid}_wav",
            )]
            _transcode(ffmpeg, qbase + ".wav", qbase + ".mp3", "mp3",
                       sr=sr, channels=channels)
            qfiles.append(_file_entry(
                qbase + ".mp3", "mp3", "libmp3lame", "lossy",
                sr, channels, phrase.size / sr, markers, f"{qid}_mp3",
            ))
            manifest["queries"][qid] = {
                "query_id": qid,
                "group_id": gid,
                "physical_key": f"phys-{gid}-p{i + 1}",
                "target_segment": f"{gid}_p{i + 1}",
                "files": qfiles,
            }

    manifest_path = os.path.join(root, "manifest.json")
    atomic_write_bytes(manifest_path, canonical_bytes(manifest))
    return manifest
