"""v2 fingerprint format: segmented hashes + timestamp quantization + bands.

Layout:
{
  "format_version": 2,
  "payload": {
    ... v1 core fields ...,
    "peak_pairs": [[anchor_frame, freq1, freq2, delta_frames], ...],
    "segment_hashes": [{"hash": int, "t_quant": int, "segment": int}, ...],
    "quant_bits": int,
    "time_quant_ms": number,
    "bands_hz": [number, ...]
  },
  "checksum": sha256(payload)
}
"""

from __future__ import annotations

from ..errors import E_FMT_INVALID, FpError
from ..model import Fingerprint, PeakPair
from .common import require, require_type, verify_checksum, wrap_payload

VERSION = 2


def parse(doc: dict, file: str) -> Fingerprint:
    version = require(doc, "format_version", "format_version", file)
    if version != VERSION:
        raise FpError(E_FMT_INVALID,
                      f"v2 parser got format_version={version!r}",
                      file=file, field="format_version")
    payload = verify_checksum(doc, file)

    sample_id = require_type(payload, "sample_id", (str,), "payload.sample_id", file)
    sample_rate = require_type(payload, "sample_rate", (int,), "payload.sample_rate", file)
    channels = require_type(payload, "channels", (int,), "payload.channels", file)
    duration_ms = require_type(payload, "duration_ms", (int, float), "payload.duration_ms", file)
    frame_length = require_type(payload, "frame_length", (int,), "payload.frame_length", file)
    hop_length = require_type(payload, "hop_length", (int,), "payload.hop_length", file)
    quant_bits = require_type(payload, "quant_bits", (int,), "payload.quant_bits", file)
    time_quant_ms = require_type(payload, "time_quant_ms", (int, float),
                                 "payload.time_quant_ms", file)
    bands_hz = require_type(payload, "bands_hz", (list,), "payload.bands_hz", file)
    raw_pairs = require_type(payload, "peak_pairs", (list,), "payload.peak_pairs", file)
    raw_hashes = require_type(payload, "segment_hashes", (list,),
                              "payload.segment_hashes", file)

    peak_pairs = []
    for i, row in enumerate(raw_pairs):
        field = f"payload.peak_pairs[{i}]"
        if not (isinstance(row, list) and len(row) == 4
                and all(isinstance(v, int) and not isinstance(v, bool) for v in row)):
            raise FpError(E_FMT_INVALID,
                          "peak pair must be [anchor_frame, freq1, freq2, delta_frames] of ints",
                          file=file, field=field)
        peak_pairs.append(PeakPair(*row))

    hashes = []
    for i, entry in enumerate(raw_hashes):
        field = f"payload.segment_hashes[{i}]"
        if not isinstance(entry, dict):
            raise FpError(E_FMT_INVALID, "segment hash entry must be an object",
                          file=file, field=field)
        for key in ("hash", "t_quant", "segment"):
            if key not in entry:
                raise FpError(E_FMT_INVALID, f"missing key '{key}'",
                              file=file, field=f"{field}.{key}")
            if not isinstance(entry[key], int) or isinstance(entry[key], bool):
                raise FpError(E_FMT_INVALID, f"key '{key}' must be an int",
                              file=file, field=f"{field}.{key}")
        hashes.append((entry["hash"], entry["t_quant"] * float(time_quant_ms)))
    if not hashes:
        raise FpError(E_FMT_INVALID, "fingerprint contains no segment hashes",
                      file=file, field="payload.segment_hashes")

    fp = Fingerprint(
        sample_id=sample_id, sample_rate=sample_rate, channels=channels,
        duration_ms=float(duration_ms), frame_length=frame_length,
        hop_length=hop_length, time_quant_ms=float(time_quant_ms),
        peak_pairs=peak_pairs, hashes=hashes, bands_hz=tuple(bands_hz),
        quant_bits=quant_bits, source_version=2,
    )
    fp.provenance = {"source": "v2-file", "extractor": None}
    return fp


def serialize(fp: Fingerprint) -> dict:
    q = fp.time_quant_ms
    payload = {
        "sample_id": fp.sample_id,
        "sample_rate": fp.sample_rate,
        "channels": fp.channels,
        "duration_ms": fp.duration_ms,
        "frame_length": fp.frame_length,
        "hop_length": fp.hop_length,
        "quant_bits": fp.quant_bits if fp.quant_bits is not None else 20,
        "time_quant_ms": q,
        "bands_hz": list(fp.bands_hz),
        "peak_pairs": [[p.anchor_frame, p.freq1, p.freq2, p.delta_frames]
                       for p in fp.peak_pairs],
        "segment_hashes": [
            {"hash": h, "t_quant": int(round(t / q)),
             "segment": int(t // 1000.0)}
            for h, t in fp.hashes
        ],
    }
    doc = {"format_version": VERSION}
    doc.update(wrap_payload(payload))
    return doc
