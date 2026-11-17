"""Deterministic WAV IO.

libsndfile attaches a PEAK chunk to float WAV files whose peak-position field
is not stable across separate process runs. We strip that chunk after every
write and recompute the RIFF size, yielding byte-identical files for identical
sample data.
"""

from __future__ import annotations

import os
import struct

import soundfile as sf


def strip_peak_chunk(path):
    with open(path, "rb") as handle:
        data = handle.read()
    if b"PEAK" not in data or data[:4] != b"RIFF":
        return path
    riff_size = struct.unpack("<I", data[4:8])[0]
    # RIFF size counts the WAVE id plus every chunk; chunks start at offset 12.
    wave_id = data[8:12]
    assert wave_id == b"WAVE"
    chunk_region = data[12:8 + riff_size]
    chunks = []
    offset = 0
    while offset + 8 <= len(chunk_region):
        chunk_id = chunk_region[offset:offset + 4]
        size = struct.unpack("<I", chunk_region[offset + 4:offset + 8])[0]
        total = 8 + size + (size & 1)
        if chunk_id != b"PEAK":
            chunks.append(chunk_region[offset:offset + total])
        offset += total
    new_chunks = b"".join(chunks)
    rebuilt = b"RIFF" + struct.pack("<I", 4 + len(new_chunks)) + wave_id + new_chunks
    tmp = path + ".tmp"
    with open(tmp, "wb") as handle:
        handle.write(rebuilt)
    os.replace(tmp, path)
    return path


def write_wav(path, data, sample_rate, subtype="FLOAT"):
    os.makedirs(os.path.dirname(path) or ".", exist_ok=True)
    sf.write(path, data, sample_rate, subtype=subtype)
    if subtype == "FLOAT":
        strip_peak_chunk(path)
    return path


def open_rw(path):
    """Open an existing WAV for seek/write; PEAK chunk is absent by design."""
    return sf.SoundFile(path, "r+")
